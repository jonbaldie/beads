package dolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/jonbaldie/beads/internal/storage/kvkeys"
	"github.com/jonbaldie/beads/internal/storage/schema"
)

func (s *DoltStore) commitAuthorString() string {
	return fmt.Sprintf("%s <%s>", s.committerName, s.committerEmail)
}

// Commit creates a Dolt commit with the given message.
//
// GH#2455: Stages all dirty tables EXCEPT config, then commits with '-m'.
// The old '-Am' approach staged ALL dirty tables including config, which
// swept up stale issue_prefix changes from concurrent operations. By
// excluding config from automatic staging, we prevent the corruption.
//
// Callers that intentionally modify config (e.g., CommitPending after
// 'bd config set') must call CommitWithConfig instead.
func (s *DoltStore) Commit(ctx context.Context, message string) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.commitWorkingSet(ctx, message, configExclude)
	})
}

// commitBeforePull commits the working set ahead of a pull's merge, INCLUDING
// config. The pre-pull auto-commit (GH#2474) must include config because user
// KV data lives there as kv.* rows (persistent memories are the kv.memory.*
// subset) and Commit() deliberately skips config (GH#2455): without this those
// rows sit permanently uncommitted, so the "clean the working set before
// merging" step leaves config dirty and DOLT_MERGE refuses to start ("cannot
// merge with uncommitted changes").
//
// It includes ONLY this clone's own user kv.* rows: if any other config key is
// dirty (an internal key such as issue_prefix above all) it refuses rather than
// auto-committing it, so the stale-config corruption GH#2455 guards against is
// never re-opened by a pull. Auto-*resolution* of a config conflict stays
// narrower still — only convergent kv.memory.* keys (see
// configConflictsAreMemoryConvergent) — so widening the commit screen to the
// whole kv. namespace cannot auto-resolve a genuine kv.* conflict; it only stops
// generic `bd kv set` writes from wedging the pull. Config is staged explicitly
// (via DOLT_ADD in commitWorkingSet) rather than through CommitWithConfig's
// DOLT_COMMIT('-Am'), which was observed not to stage config reliably under the
// server-mode stored-procedure path. Committing this clone's own kv.* rows as
// the merge basis is the same explicit, user-initiated action CommitPending ('bd dolt
// commit') already performs, so it does not widen the concurrent-writer race
// GH#2455 guards against.
func (s *DoltStore) commitBeforePull(ctx context.Context, message string) error {
	return s.commitWorkingSet(ctx, message, configIncludeUserKVOnly)
}

// CommitMergeResolution concludes a merge whose conflicts were resolved by an
// explicit operator strategy (bd federation sync --strategy / bd vc merge
// --strategy ours|theirs), committing the resolved working set INCLUDING config.
// Plain Commit excludes config (GH#2455), so a config-only resolution — exactly
// the case this change makes routine by syncing kv.* through config — would be
// silently dropped, leaving the merge unconcluded and re-wedging the next
// pull/sync. Unlike commitBeforePull it does not screen config keys: the operator
// chose this resolution, so whichever config rows it touched (issue_prefix
// included) are committed as-is. It satisfies storage.VersionControl so cmd/bd
// concludes bd vc merge --strategy through the same config-inclusive commit
// instead of the config-excluding Commit that would drop the resolution.
func (s *DoltStore) CommitMergeResolution(ctx context.Context, message string) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.commitWorkingSet(ctx, message, configIncludeAll)
	})
}

// commitWorkingSet stages the dirty tables reported by dolt_status and commits
// them with '-m'. The config table is staged according to mode: configExclude
// skips it (GH#2455) so a concurrent writer's half-applied issue_prefix change
// is never swept into an unrelated commit; configIncludeUserKVOnly stages it for
// the pre-pull path but refuses when any non-kv. (internal) config key is dirty;
// configIncludeAll stages every dirty config row to conclude an explicit merge
// resolution.
func (s *DoltStore) commitWorkingSet(ctx context.Context, message string, mode configCommitMode) (retErr error) {
	ctx, span := doltTracer.Start(ctx, "dolt.commit",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(s.doltSpanAttrs()...),
	)
	defer func() { endSpan(span, retErr) }()

	// Pin a single connection so all operations run on the same Dolt session.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire connection: %w", err)
	}
	defer conn.Close()

	tables, configDirty, err := loadWorkingSetTables(ctx, conn, mode)
	if err != nil {
		return err
	}

	// GH#2455 + GH#2474: the pre-pull auto-commit includes config so user kv.*
	// writes sync, but it must NOT auto-commit any internal (non-kv.) config key.
	// Refuse before staging anything so the merge is never concluded over an
	// unsafe config row; the operator commits those explicitly.
	if configDirty && mode == configIncludeUserKVOnly {
		if err := s.assertDirtyConfigUserKVOnly(ctx, conn); err != nil {
			return err
		}
	}

	if len(tables) == 0 {
		// A merge resolution with a clean working set is NOT a no-op: it is
		// the `--ours` case, where our values already stood and resolving the
		// conflict dirtied nothing. Returning here left is_merging true while
		// the caller reported "Merge committed", and the next pull re-wedged
		// on the unconcluded merge (wy-36ilm, caught by the F9 integration
		// test). Only the merge-conclusion mode takes this path: for the
		// other modes an empty working set really is nothing to commit.
		if mode == configIncludeAll {
			return s.concludeOpenMerge(ctx, conn, message)
		}
		return nil
	}

	if err := stageWorkingSetTables(ctx, conn, tables); err != nil {
		return err
	}

	return commitWorkingSetTables(ctx, conn, message, s.commitAuthorString(), s.wrapDoltPublicationFailure)
}

// commitWorkingSetAfterSQLCommit preserves the no-replay boundary for a Dolt
// publication that follows an already-visible SQL mutation. commitWorkingSet
// classifies DOLT_COMMIT response loss itself; this wrapper adds the same
// sentinel to earlier publication failures such as a lost DOLT_ADD response.
func (s *DoltStore) commitWorkingSetAfterSQLCommit(ctx context.Context, message string, mode configCommitMode) error {
	err := s.commitWorkingSet(ctx, message, mode)
	if err == nil || errors.Is(err, ErrCommitIndeterminate) || !isIndeterminateCommitResponse(err) {
		return err
	}
	return s.recordDoltPublicationFailure(ctx,
		fmt.Errorf("publish working set after SQL commit: %w: %w", err, ErrCommitIndeterminate))
}

// concludeOpenMerge commits an open merge whose resolution left the working
// set clean, so the merge is actually concluded rather than left open with
// nothing to show for it. It is a no-op when no merge is in progress, and it
// runs on the CALLER'S pinned connection because dolt's merge state is
// session state. isDoltNothingToCommit still absorbs the race where the merge
// closed between the status read and the commit.
func (s *DoltStore) concludeOpenMerge(ctx context.Context, conn *sql.Conn, message string) error {
	var merging bool
	if err := conn.QueryRowContext(ctx, "SELECT is_merging FROM dolt_merge_status").Scan(&merging); err != nil {
		// No merge status to read is no evidence of a merge — keep the old
		// "nothing to commit" behavior rather than failing a resolution.
		return nil //nolint:nilerr // diagnosis only; never a gate
	}
	if !merging {
		return nil
	}
	if err := schema.DrainCall(ctx, conn, "CALL DOLT_COMMIT('-m', ?, '--author', ?)", message, s.commitAuthorString()); err != nil {
		if isDoltNothingToCommit(err) {
			return nil
		}
		return s.wrapDoltPublicationFailure(ctx, "failed to conclude merge", err)
	}
	return nil
}

// assertDirtyConfigUserKVOnly returns an error unless every config row dirty in
// the working set is this clone's own user KV data (the kv.* namespace, which
// includes kv.memory.* memories). The pre-pull auto-commit opts config into the
// staged set so user KV writes sync and stop wedging DOLT_MERGE (GH#2474), but
// auto-committing an unrelated dirty internal config key such as issue_prefix
// would re-open the GH#2455 stale-config corruption — that is the operator's
// explicit `bd dolt commit` to make, not the pull's. Screening on the whole kv.
// namespace (not just kv.memory.*) un-wedges generic `bd kv set` writes too: a
// kv.* row is this clone's own data, exactly as safe to auto-commit as a memory,
// and a genuine kv.* merge conflict is still left for the operator because
// auto-resolution stays kv.memory.*-only (configConflictsAreMemoryConvergent).
// config's primary key is `key`, so dolt_diff exposes to_key/from_key; an add or
// delete leaves one side NULL, so COALESCE picks whichever key the change carries.
func (s *DoltStore) assertDirtyConfigUserKVOnly(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx,
		"SELECT COALESCE(to_key, from_key) FROM dolt_diff('HEAD', 'WORKING', 'config')")
	if err != nil {
		return fmt.Errorf("inspect dirty config before pull: %w", err)
	}
	defer rows.Close()

	var unsafe []string
	for rows.Next() {
		var key sql.NullString
		if err := rows.Scan(&key); err != nil {
			return fmt.Errorf("scan dirty config key: %w", err)
		}
		if key.Valid && !strings.HasPrefix(key.String, kvkeys.Prefix) {
			unsafe = append(unsafe, key.String)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate dirty config diff: %w", err)
	}
	if len(unsafe) > 0 {
		return fmt.Errorf("refusing to auto-commit %d dirty internal config key(s) before pull: %s; "+
			"only user %s* keys auto-commit before a pull (GH#2455) — commit or revert "+
			"these explicitly with `bd dolt commit` first", len(unsafe), strings.Join(unsafe, ", "), kvkeys.Prefix)
	}
	return nil
}

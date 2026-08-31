package issueops

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// BatchContext holds per-batch state read once and reused for every issue.
type BatchContext struct {
	CustomStatuses  []string
	CustomTypes     []string
	ConfigPrefix    string
	AllowedPrefixes string
	Opts            storage.BatchCreateOptions
	// SkipChildCounterReconcile tells CreateIssueInTxWithResult to skip its
	// per-issue ReconcileChildCounters call. CreateIssuesInTxWithResult sets
	// this because it already runs one slice-wide ReconcileChildCounters over
	// the whole accepted batch after the per-issue loop, which covers every
	// issue the per-issue call would have handled; running it again per issue
	// during a batch import was 3-4 redundant round trips per hierarchical
	// issue for a result the caller discards. Singular creates leave this
	// false so they keep reconciling immediately, per-issue.
	SkipChildCounterReconcile bool
}

// NewBatchContext reads config from the database and returns a BatchContext.
func NewBatchContext(ctx context.Context, tx DBTX, opts storage.BatchCreateOptions) (*BatchContext, error) {
	customStatuses, err := GetCustomStatusesTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("failed to get custom statuses: %w", err)
	}
	customTypes, err := ResolveCustomTypesInTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("failed to get custom types: %w", err)
	}
	configPrefix, err := ReadConfigPrefix(ctx, tx)
	if err != nil {
		return nil, err
	}
	var allowedPrefixes string
	_ = tx.QueryRowContext(ctx, "SELECT value FROM config WHERE `key` = ?", "allowed_prefixes").Scan(&allowedPrefixes)

	return &BatchContext{
		CustomStatuses:  customStatuses,
		CustomTypes:     customTypes,
		ConfigPrefix:    configPrefix,
		AllowedPrefixes: allowedPrefixes,
		Opts:            opts,
	}, nil
}

func CreateIssueInTx(ctx context.Context, tx DBTX, bc *BatchContext, issue *types.Issue, actor string) error {
	_, err := CreateIssueInTxWithResult(ctx, tx, bc, issue, actor)
	return err
}

func (r *CreateIssueResult) markChanged(table string) {
	if table == "" {
		return
	}
	if r.ChangedTables == nil {
		r.ChangedTables = map[string]bool{}
	}
	r.ChangedTables[table] = true
}

func mergeChangedTables(dst map[string]bool, src map[string]bool) map[string]bool {
	for table := range src {
		if dst == nil {
			dst = map[string]bool{}
		}
		dst[table] = true
	}
	return dst
}

func CreateIssueInTxWithResult(ctx context.Context, tx DBTX, bc *BatchContext, issue *types.Issue, actor string) (CreateIssueResult, error) {
	issueTable, eventTable, skipped, err := prepareCreateIssue(ctx, tx, bc, issue, actor)
	if err != nil {
		return CreateIssueResult{}, err
	}
	if skipped {
		return CreateIssueResult{}, nil
	}
	result, isNew, err := insertPreparedIssue(ctx, tx, bc, issueTable, eventTable, issue, actor)
	if err != nil {
		return result, err
	}
	if result.StaleRejected {
		if bc.Opts.OnStaleRejected != nil {
			bc.Opts.OnStaleRejected(issue.ID)
		}
		return result, nil
	}

	auxResult, err := persistCreateAuxData(ctx, tx, bc, eventTable, issue, actor, isNew)
	if err != nil {
		return result, err
	}
	result.ChangedTables = mergeChangedTables(result.ChangedTables, auxResult.ChangedTables)
	result.persistedComments = append(result.persistedComments, auxResult.persistedComments...)

	return journalCreatedIssue(ctx, tx, issue.ID, result)
}

func prepareCreateIssue(ctx context.Context, tx DBTX, bc *BatchContext, issue *types.Issue, actor string) (string, string, bool, error) {
	if err := PrepareIssueForInsert(issue, bc.CustomStatuses, bc.CustomTypes); err != nil {
		return "", "", false, err
	}
	issueTable, eventTable := TableRouting(issue)
	if err := assignCreateIssueIDInTx(ctx, tx, bc, issue, actor); err != nil {
		return "", "", false, err
	}
	if bc.Opts.CreateOnly {
		if err := EnsureIssueIDAvailableInTx(ctx, tx, issue.ID); err != nil {
			return "", "", false, err
		}
	}
	if skip, err := checkCrossTableIDCollision(ctx, tx, issue.ID, issueTable, bc.Opts); err != nil {
		return "", "", false, err
	} else if skip {
		return "", "", true, nil
	}
	return issueTable, eventTable, false, nil
}

func insertPreparedIssue(ctx context.Context, tx DBTX, bc *BatchContext, issueTable, eventTable string, issue *types.Issue, actor string) (CreateIssueResult, bool, error) {
	var result CreateIssueResult
	isNew, staleRejected, err := InsertIssueIfNew(ctx, tx, issueTable, issue, bc.Opts)
	if err != nil {
		return result, false, err
	}
	if staleRejected {
		// The stored row is strictly newer than this snapshot: nothing was
		// written, and the snapshot's labels/comments belong to the older
		// version, so they must not merge in either (bd-578h9.8).
		result.StaleRejected = true
		return result, false, nil
	}
	result.markChanged(issueTable)

	// Reconcile the ephemeral lease row with the accepted issue state
	// (restore an imported lease / drop an orphaned one — see
	// RestoreLeaseOnImportInTx). Wisps are never leased. The leases table is
	// dolt_ignored, so this is deliberately not marked as a changed table.
	if issueTable == "issues" {
		if err := RestoreLeaseOnImportInTx(ctx, tx, issue, isNew); err != nil {
			return result, false, err
		}
	}

	if isNew {
		if err := RecordEventInTable(ctx, tx, eventTable, issue.ID, types.EventCreated, actor, ""); err != nil {
			return result, false, fmt.Errorf("failed to record event for %s: %w", issue.ID, err)
		}
		result.markChanged(eventTable)
	}
	return result, isNew, nil
}

func persistCreateAuxData(ctx context.Context, tx DBTX, bc *BatchContext, eventTable string, issue *types.Issue, actor string, isNew bool) (CreateIssueResult, error) {
	var result CreateIssueResult
	labelResult, err := PersistLabels(ctx, tx, issue, actor, eventTable)
	if err != nil {
		return result, err
	}
	result.ChangedTables = mergeChangedTables(result.ChangedTables, labelResult.ChangedTables)
	commentResult, err := PersistComments(ctx, tx, issue)
	if err != nil {
		return result, err
	}
	result.ChangedTables = mergeChangedTables(result.ChangedTables, commentResult.ChangedTables)
	result.persistedComments = append(result.persistedComments, commentResult.persistedComments...)

	// Advance child_counters when a singular create materializes a hierarchical
	// ID (e.g. bd create --id P.8). The batch path already calls
	// ReconcileChildCounters after CreateIssuesInTx; without this, explicit --id
	// creates leave last_child behind the live suffix high-water mark and the
	// next bd create --parent can recycle lower suffixes (GH#4750).
	if isNew && !bc.SkipChildCounterReconcile {
		if _, childNum, ok := ParseHierarchicalID(issue.ID); ok && childNum > 0 {
			changedCounters, err := ReconcileChildCounters(ctx, tx, []*types.Issue{issue})
			if err != nil {
				return result, err
			}
			result.ChangedTables = mergeChangedTables(result.ChangedTables, changedCounters)
		}
	}
	return result, nil
}

func journalCreatedIssue(ctx context.Context, tx DBTX, issueID string, result CreateIssueResult) (CreateIssueResult, error) {
	// Journal the create once, after labels and comments are in the row's
	// transaction, so the snapshot is the complete bead. The early returns above
	// (collision skip, stale reject) wrote nothing and journal nothing.
	if err := RecordEventInTx(ctx, tx, EventCreate, issueID); err != nil {
		return result, err
	}
	// Creation-time comments (import/interchange carries them inline) are
	// replayable content the create snapshot does NOT contain — issue hydration
	// joins labels but not comments — so each inserted comment gets its own op,
	// emitted after the create so a consumer is never told about a comment on a
	// bead it has not seen created. Dedup hits above inserted nothing and emit
	// nothing.
	for i := range result.persistedComments {
		if err := RecordCommentEventInTx(ctx, tx, issueID, &result.persistedComments[i]); err != nil {
			return result, err
		}
	}
	return result, nil
}

func assignCreateIssueIDInTx(ctx context.Context, tx DBTX, bc *BatchContext, issue *types.Issue, actor string) error {
	if issue.ID == "" {
		issueTable, _ := TableRouting(issue)
		prefix := bc.ConfigPrefix
		if issue.PrefixOverride != "" {
			prefix = issue.PrefixOverride
		} else if issue.IDPrefix != "" {
			prefix = bc.ConfigPrefix + "-" + issue.IDPrefix
		} else if IsWisp(issue) {
			prefix = bc.ConfigPrefix + "-wisp"
		}
		var err error
		issue.ID, err = GenerateIssueIDInTable(ctx, tx, issueTable, prefix, issue, actor)
		if err != nil {
			return fmt.Errorf("failed to generate issue ID: %w", err)
		}
		return nil
	}
	if !bc.Opts.SkipPrefixValidation {
		if err := ValidateIssueIDPrefix(issue.ID, bc.ConfigPrefix, bc.AllowedPrefixes); err != nil {
			return fmt.Errorf("prefix validation failed for %s: %w", issue.ID, err)
		}
	}
	return nil
}

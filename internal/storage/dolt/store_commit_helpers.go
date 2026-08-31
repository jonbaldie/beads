package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/schema"
)

// loadWorkingSetTables finds dirty, stageable tables and records whether config
// was dirty before the selected config-commit mode filtered it out.
func loadWorkingSetTables(ctx context.Context, conn *sql.Conn, mode configCommitMode) ([]string, bool, error) {
	// GH#2455: stage each dirty table individually, skipping config unless the
	// mode opts it in, to avoid sweeping up stale issue_prefix changes from
	// concurrent operations. Exclude dolt_ignore'd tables (wisps, wisp_%, leases)
	// with the same anti-join HasCommittablePending uses: they surface in
	// dolt_status but are never stageable, and the fail-hard DOLT_ADD loop below
	// must see only tables it can actually stage. A dirty wisp or lease row is the
	// normal steady state; staging it depends on Dolt's version-specific
	// ignored-table DOLT_ADD behavior (a silent no-op on 2.2.0), so filtering here
	// keeps ordinary commits from failing whenever an ignored table is dirty.
	rows, err := conn.QueryContext(ctx, `
		SELECT s.table_name FROM dolt_status s
		WHERE NOT EXISTS (
			SELECT 1 FROM dolt_ignore di
			WHERE di.ignored = 1
			AND s.table_name LIKE di.pattern
		)`)
	if err != nil {
		return nil, false, fmt.Errorf("failed to query dolt_status: %w", err)
	}
	defer rows.Close()

	var tables []string
	configDirty := false
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, false, fmt.Errorf("failed to scan dolt_status: %w", err)
		}
		if table == "config" {
			configDirty = true
			if mode == configExclude {
				continue
			}
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("failed to iterate dolt_status: %w", err)
	}
	return tables, configDirty, nil
}

func stageWorkingSetTables(ctx context.Context, conn *sql.Conn, tables []string) error {
	for _, table := range tables {
		if err := schema.DrainCall(ctx, conn, "CALL DOLT_ADD(?)", table); err != nil {
			return fmt.Errorf("failed to stage %s before commit: %w", table, err)
		}
	}
	return nil
}

func commitWorkingSetTables(
	ctx context.Context,
	conn *sql.Conn,
	message, author string,
	wrapPublicationFailure func(context.Context, string, error) error,
) error {
	// NOTE: In SQL procedure mode, Dolt defaults author to the authenticated SQL user
	// (e.g. root@localhost). Always pass an explicit author for deterministic history.
	if err := schema.DrainCall(ctx, conn, "CALL DOLT_COMMIT('-m', ?, '--author', ?)", message, author); err != nil {
		if isDoltNothingToCommit(err) {
			return nil
		}
		return wrapPublicationFailure(ctx, "failed to commit", err)
	}
	return nil
}

func batchCommitIssueCounts(ctx context.Context, db *sql.DB) (added, modified, removed int) {
	// Count issue-level changes by diff type. This is best effort because the
	// commit message must remain usable when Dolt's diff metadata is unavailable.
	rows, err := db.QueryContext(ctx, `
		SELECT diff_type, COUNT(*) as cnt
		FROM dolt_diff('HEAD', 'WORKING', 'issues')
		GROUP BY diff_type
	`)
	if err != nil {
		return 0, 0, 0
	}
	defer rows.Close()
	for rows.Next() {
		var diffType string
		var count int
		if err := rows.Scan(&diffType, &count); err != nil {
			continue
		}
		switch diffType {
		case "added":
			added = count
		case "modified":
			modified = count
		case "removed":
			removed = count
		}
	}
	return added, modified, removed
}

func batchCommitOtherTables(ctx context.Context, db *sql.DB) []string {
	// Check which other tables have uncommitted changes beyond issues. This
	// surfaces label, comment, event, and dependency changes that would otherwise
	// produce a generic fallback message.
	rows, err := db.QueryContext(ctx, `
		SELECT table_name FROM dolt_status s
		WHERE table_name != 'issues'
		AND NOT EXISTS (
			SELECT 1 FROM dolt_ignore di
			WHERE di.ignored = 1
			AND s.table_name LIKE di.pattern
		)`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err == nil {
			tables = append(tables, table)
		}
	}
	return tables
}

func formatBatchCommitMessage(actor string, added, modified, removed int, otherTables []string) string {
	var parts []string
	if added > 0 {
		parts = append(parts, fmt.Sprintf("%d created", added))
	}
	if modified > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", modified))
	}
	if removed > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted", removed))
	}
	if len(parts) == 0 && len(otherTables) == 0 {
		return fmt.Sprintf("bd: batch commit by %s", actor)
	}

	msg := fmt.Sprintf("bd: batch commit by %s", actor)
	if len(parts) > 0 {
		msg += " — " + strings.Join(parts, ", ")
	}
	if len(otherTables) > 0 {
		msg += fmt.Sprintf(" (+ %s)", strings.Join(otherTables, ", "))
	}
	return msg
}

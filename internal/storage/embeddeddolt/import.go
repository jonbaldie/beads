//go:build cgo

package embeddeddolt

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/utils"
)

// ImportJSONLData atomically checks if the database is empty and, if so,
// imports parsed issues and config key/value pairs in a single transaction.
// Returns the count of issues imported, or 0 if the database was not empty.
// Does NOT issue DOLT_COMMIT — the caller is responsible for committing
// (e.g. via the PersistentPostRun auto-commit hook).
func (s *EmbeddedDoltStore) ImportJSONLData(
	ctx context.Context,
	issues []*types.Issue,
	configEntries map[string]string,
	actor string,
) (int, error) {
	var imported int
	err := s.withConn(ctx, true, func(tx *sql.Tx) error {
		return importJSONLDataInTx(ctx, tx, issues, configEntries, actor, &imported)
	})
	return imported, err
}

func importJSONLDataInTx(
	ctx context.Context,
	tx *sql.Tx,
	issues []*types.Issue,
	configEntries map[string]string,
	actor string,
	imported *int,
) error {
	empty, err := importDatabaseIsEmpty(ctx, tx)
	if err != nil {
		return err
	}
	if !empty {
		return nil
	}
	if err := importConfigEntries(ctx, tx, configEntries); err != nil {
		return err
	}
	if len(issues) == 0 {
		return nil
	}
	if err := ensureImportIssuePrefix(ctx, tx, issues, configEntries); err != nil {
		return err
	}
	if err := createImportedIssues(ctx, tx, issues, actor); err != nil {
		return err
	}
	*imported = len(issues)
	return nil
}

func importDatabaseIsEmpty(ctx context.Context, tx *sql.Tx) (bool, error) {
	stats := &types.Statistics{}
	if err := issueops.ScanIssueCountsInTx(ctx, tx, stats); err != nil {
		return false, fmt.Errorf("checking issue count: %w", err)
	}
	return stats.TotalIssues == 0, nil
}

func importConfigEntries(ctx context.Context, tx *sql.Tx, configEntries map[string]string) error {
	for key, value := range configEntries {
		if err := issueops.SetConfigInTx(ctx, tx, key, value); err != nil {
			return fmt.Errorf("importing config %q: %w", key, err)
		}
	}
	return nil
}

func ensureImportIssuePrefix(ctx context.Context, tx *sql.Tx, issues []*types.Issue, configEntries map[string]string) error {
	if _, hasPrefix := configEntries["issue_prefix"]; hasPrefix {
		return nil
	}
	firstPrefix := utils.ExtractIssuePrefix(issues[0].ID)
	if firstPrefix == "" {
		return nil
	}
	if err := issueops.SetConfigInTx(ctx, tx, "issue_prefix", firstPrefix); err != nil {
		return fmt.Errorf("setting issue_prefix: %w", err)
	}
	return nil
}

func createImportedIssues(ctx context.Context, tx *sql.Tx, issues []*types.Issue, actor string) error {
	return issueops.CreateIssuesInTx(ctx, tx, issues, actor, storage.BatchCreateOptions{
		SkipPrefixValidation: true,
		// Defense-in-depth (GH#3955): the embedded fast-path is the primary
		// auto-import route for 1.0+ users and is gated by the in-transaction
		// emptiness check above. Make it insert-if-new too so a regression in
		// that check cannot clobber live rows — matching the server-mode
		// fallback's conflict-skip behavior.
		ConflictSkip: true,
	})
}

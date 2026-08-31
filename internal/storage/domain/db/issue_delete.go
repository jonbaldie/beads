package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
)

func (r *issueDeletionRepository) DeleteByIDs(ctx context.Context, ids []string, opts domain.IssueTableOpts) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	table := pickIssueTable(opts.UseWispsTable)
	actualIDs, err := r.resolveDeleteIDs(ctx, table, ids)
	if err != nil {
		return 0, err
	}
	if err := r.journalDeleteDependencies(ctx, actualIDs); err != nil {
		return 0, err
	}
	total, err := r.deleteIDBatches(ctx, table, ids, opts.UseWispsTable)
	if err != nil {
		return total, err
	}
	if err := r.recordDeletedIDs(ctx, actualIDs); err != nil {
		return total, err
	}
	return total, nil
}

func (r *issueDeletionRepository) resolveDeleteIDs(ctx context.Context, table string, ids []string) ([]string, error) {
	// Resolve WHICH ids this delete actually removes before the batched DELETE
	// runs: afterwards the rows are gone, and RowsAffected reports a count, not
	// a set. A journal record for an id that was already absent would tell a
	// consumer to drop a bead this transaction never touched.
	actualIDs, err := issueops.ExistingIssueIDsInTableInTx(ctx, r.runner, table, ids)
	if err != nil {
		return nil, fmt.Errorf("db: IssueSQLRepository.DeleteByIDs resolve existing ids: %w", err)
	}
	return actualIDs, nil
}

func (r *issueDeletionRepository) journalDeleteDependencies(ctx context.Context, ids []string) error {
	// Edges are journaled before the rows go, while their source snapshots can
	// still be read.
	if err := issueops.RecordDependencyRemovalsForIssuesInTx(ctx, r.runner, ids); err != nil {
		return fmt.Errorf("db: IssueSQLRepository.DeleteByIDs journal dependency removals: %w", err)
	}
	return nil
}

func (r *issueDeletionRepository) deleteIDBatches(ctx context.Context, table string, ids []string, useWispsTable bool) (int, error) {
	total := 0
	idsLen := len(ids)
	for start := 0; start < idsLen; start += deleteBatchSize {
		end := min(start+deleteBatchSize, idsLen)
		n, err := r.deleteIDBatch(ctx, table, ids[start:end], useWispsTable)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (r *issueDeletionRepository) deleteIDBatch(ctx context.Context, table string, ids []string, useWispsTable bool) (int, error) {
	placeholders, args := buildDeleteArgs(ids)
	//nolint:gosec // G201: table is a hardcoded constant; placeholders are ?.
	res, err := r.runner.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE id IN (%s)", table, strings.Join(placeholders, ",")),
		args...)
	if err != nil {
		return 0, fmt.Errorf("db: IssueSQLRepository.DeleteByIDs from %s: %w", table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: IssueSQLRepository.DeleteByIDs rows affected: %w", err)
	}
	if err := r.deleteBatchLeases(ctx, placeholders, args, useWispsTable); err != nil {
		return int(n), err
	}
	return int(n), nil
}

func buildDeleteArgs(ids []string) ([]string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return placeholders, args
}

func (r *issueDeletionRepository) deleteBatchLeases(ctx context.Context, placeholders []string, args []any, useWispsTable bool) error {
	if useWispsTable {
		return nil
	}
	// Deleted issues hold no leases.
	//nolint:gosec // G201: placeholders are ?.
	if _, err := r.runner.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM leases WHERE issue_id IN (%s)", strings.Join(placeholders, ",")),
		args...); err != nil {
		return fmt.Errorf("db: IssueSQLRepository.DeleteByIDs leases: %w", err)
	}
	return nil
}

func (r *issueDeletionRepository) recordDeletedIDs(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if err := issueops.RecordDeleteInTx(ctx, r.runner, id); err != nil {
			return err
		}
	}
	return nil
}

package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
)

func NewEventsSQLRepository(runner Runner) domain.EventsSQLRepository {
	return &eventsSQLRepositoryImpl{runner: runner}
}

type eventsSQLRepositoryImpl struct {
	runner Runner
}

var _ domain.EventsSQLRepository = (*eventsSQLRepositoryImpl)(nil)

func (r *eventsSQLRepositoryImpl) Record(ctx context.Context, evt domain.Event, opts domain.RecordEventOpts) error {
	table := "events"
	if opts.UseWispsTable {
		table = "wisp_events"
	}
	if err := issueops.RecordFullEventInTable(ctx, r.runner, table, evt.IssueID, evt.Type, evt.Actor, evt.OldValue, evt.NewValue); err != nil {
		return fmt.Errorf("db: record event in %s: %w", table, err)
	}
	return nil
}

func (r *eventsSQLRepositoryImpl) DeleteAllForIDs(ctx context.Context, ids []string, opts domain.RecordEventOpts) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	table := "events"
	if opts.UseWispsTable {
		table = "wisp_events"
	}
	total := 0
	idsLen := len(ids)
	for start := 0; start < idsLen; start += deleteBatchSize {
		end := start + deleteBatchSize
		if end > idsLen {
			end = idsLen
		}
		batch := ids[start:end]
		n, err := deleteEventBatch(ctx, r.runner, table, batch)
		if err != nil {
			if opts.UseWispsTable && dberrors.IsTableNotExist(err) {
				return total, nil
			}
			return total, err
		}
		total += int(n)
	}
	return total, nil
}

func deleteEventBatch(ctx context.Context, runner Runner, table string, batch []string) (int64, error) {
	placeholders := make([]string, len(batch))
	args := make([]any, len(batch))
	for i, id := range batch {
		placeholders[i] = "?"
		args[i] = id
	}
	//nolint:gosec // G201: table is one of two hardcoded constants; ? placeholders only.
	res, err := runner.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE issue_id IN (%s)", table, strings.Join(placeholders, ",")),
		args...)
	if err != nil {
		return 0, fmt.Errorf("db: EventsSQLRepository.DeleteAllForIDs from %s: %w", table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: EventsSQLRepository.DeleteAllForIDs rows affected: %w", err)
	}
	return n, nil
}

func (r *eventsSQLRepositoryImpl) CountAllForIDs(ctx context.Context, ids []string, opts domain.RecordEventOpts) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	table := "events"
	if opts.UseWispsTable {
		table = "wisp_events"
	}
	count, err := issueops.CountRowsForIssueIDsInTx(ctx, r.runner, table, ids)
	if err != nil {
		if opts.UseWispsTable && dberrors.IsTableNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("db: EventsSQLRepository.CountAllForIDs: %w", err)
	}
	return count, nil
}

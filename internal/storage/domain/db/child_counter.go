package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/domain"
)

func NewChildCounterSQLRepository(runner Runner) domain.ChildCounterSQLRepository {
	return &childCounterSQLRepositoryImpl{runner: runner}
}

type childCounterSQLRepositoryImpl struct {
	runner Runner
}

var _ domain.ChildCounterSQLRepository = (*childCounterSQLRepositoryImpl)(nil)

func (r *childCounterSQLRepositoryImpl) NextChildID(ctx context.Context, parentID string, _ domain.ChildCounterOpts) (string, error) {
	if parentID == "" {
		return "", errors.New("db: ChildCounterSQLRepository.NextChildID: parentID must not be empty")
	}

	counterTable, issueTable, err := r.childCounterTables(ctx, parentID)
	if err != nil {
		return "", err
	}
	lastChild, err := r.readChildCounter(ctx, counterTable, parentID)
	if err != nil {
		return "", err
	}
	lastChild, err = r.highestExistingChild(ctx, issueTable, parentID, lastChild)
	if err != nil {
		return "", err
	}

	next := lastChild + 1
	if err := r.upsertChildCounter(ctx, counterTable, parentID, next); err != nil {
		return "", err
	}

	return fmt.Sprintf("%s.%d", parentID, next), nil
}

func (r *childCounterSQLRepositoryImpl) childCounterTables(ctx context.Context, parentID string) (string, string, error) {
	counterTable, issueTable := "child_counters", "issues"
	parentIsWisp, err := r.parentIsActiveWisp(ctx, parentID)
	if err != nil {
		return "", "", fmt.Errorf("db: ChildCounterSQLRepository.NextChildID: probe parent table for %s: %w", parentID, err)
	}
	if parentIsWisp {
		counterTable, issueTable = "wisp_child_counters", "wisps"
	}
	return counterTable, issueTable, nil
}

func (r *childCounterSQLRepositoryImpl) readChildCounter(ctx context.Context, counterTable, parentID string) (int, error) {
	var lastChild int
	err := r.runner.QueryRowContext(ctx,
		//nolint:gosec // G201: counterTable is one of two hardcoded constants
		fmt.Sprintf("SELECT last_child FROM %s WHERE parent_id = ?", counterTable),
		parentID,
	).Scan(&lastChild)
	switch {
	case err == nil:
		return lastChild, nil
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	default:
		return 0, fmt.Errorf("db: ChildCounterSQLRepository.NextChildID: read counter for %s: %w", parentID, err)
	}
}

func (r *childCounterSQLRepositoryImpl) highestExistingChild(ctx context.Context, issueTable, parentID string, lastChild int) (int, error) {
	rows, err := r.runner.QueryContext(ctx, fmt.Sprintf(`
		SELECT id FROM %s
		WHERE id LIKE CONCAT(?, '.%%')
		  AND id NOT LIKE CONCAT(?, '.%%.%%')
	`, issueTable), parentID, parentID) //nolint:gosec // G201: issueTable is one of two hardcoded constants
	if err != nil {
		return 0, fmt.Errorf("db: ChildCounterSQLRepository.NextChildID: scan existing children of %s: %w", parentID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("db: ChildCounterSQLRepository.NextChildID: scan: %w", err)
		}
		if n, ok := parseChildSuffix(id); ok && n > lastChild {
			lastChild = n
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("db: ChildCounterSQLRepository.NextChildID: rows: %w", err)
	}
	return lastChild, nil
}

func (r *childCounterSQLRepositoryImpl) upsertChildCounter(ctx context.Context, counterTable, parentID string, next int) error {
	//nolint:gosec // G201: counterTable is one of two hardcoded constants
	if _, err := r.runner.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (parent_id, last_child) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE last_child = ?
	`, counterTable), parentID, next, next); err != nil {
		return fmt.Errorf("db: ChildCounterSQLRepository.NextChildID: upsert counter for %s: %w", parentID, err)
	}
	return nil
}

func (r *childCounterSQLRepositoryImpl) parentIsActiveWisp(ctx context.Context, parentID string) (bool, error) {
	var probe int
	err := r.runner.QueryRowContext(ctx, "SELECT 1 FROM wisps WHERE id = ? LIMIT 1", parentID).Scan(&probe)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case dberrors.IsTableNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

func parseChildSuffix(id string) (int, bool) {
	dot := strings.LastIndex(id, ".")
	if dot < 0 || dot == len(id)-1 {
		return 0, false
	}
	n, err := strconv.Atoi(id[dot+1:])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

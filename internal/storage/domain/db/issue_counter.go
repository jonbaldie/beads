package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func (r *issueLookupRepository) NextCounterID(ctx context.Context, prefix string) (int, error) {
	if prefix == "" {
		return 0, errors.New("db: NextCounterID: prefix must not be empty")
	}
	if err := r.ensureCounter(ctx, prefix); err != nil {
		return 0, fmt.Errorf("db: NextCounterID: ensure %q: %w", prefix, err)
	}
	return r.readCounter(ctx, prefix)
}

func (r *issueLookupRepository) ensureCounter(ctx context.Context, prefix string) error {
	rows, err := r.incrementCounter(ctx, prefix, "increment", "rows affected")
	if err != nil {
		return err
	}
	if rows > 0 {
		return nil
	}

	if err := r.seedCounterFromExisting(ctx, prefix); err != nil {
		return fmt.Errorf("seed %q: %w", prefix, err)
	}
	rows, err = r.incrementCounter(ctx, prefix, "increment after seed", "rows affected after seed")
	if err != nil {
		return err
	}
	if rows > 0 {
		return nil
	}
	if _, err := r.runner.ExecContext(ctx, "INSERT INTO issue_counter (prefix, last_id) VALUES (?, 1)", prefix); err != nil {
		return fmt.Errorf("insert initial %q: %w", prefix, err)
	}
	return nil
}

func (r *issueLookupRepository) incrementCounter(ctx context.Context, prefix, operation, rowsOperation string) (int64, error) {
	res, err := r.runner.ExecContext(ctx, "UPDATE issue_counter SET last_id = last_id + 1 WHERE prefix = ?", prefix)
	if err != nil {
		return 0, fmt.Errorf("db: NextCounterID: %s %q: %w", operation, prefix, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("db: NextCounterID: %s %q: %w", rowsOperation, prefix, err)
	}
	return rows, nil
}

func (r *issueLookupRepository) readCounter(ctx context.Context, prefix string) (int, error) {
	var nextID int
	if err := r.runner.QueryRowContext(ctx, "SELECT last_id FROM issue_counter WHERE prefix = ?", prefix).Scan(&nextID); err != nil {
		return 0, fmt.Errorf("db: NextCounterID: read last_id %q: %w", prefix, err)
	}
	return nextID, nil
}

func (r *issueLookupRepository) seedCounterFromExisting(ctx context.Context, prefix string) error {
	existing, err := r.readExistingCounter(ctx, prefix)
	if err == nil || existing {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read existing counter %q: %w", prefix, err)
	}

	maxNum, err := r.maxExistingCounter(ctx, prefix)
	if err != nil {
		return err
	}
	if maxNum == 0 {
		return nil
	}
	if _, err := r.runner.ExecContext(ctx, "INSERT INTO issue_counter (prefix, last_id) VALUES (?, ?)", prefix, maxNum); err != nil {
		return fmt.Errorf("seed counter %q at %d: %w", prefix, maxNum, err)
	}
	return nil
}

func (r *issueLookupRepository) readExistingCounter(ctx context.Context, prefix string) (bool, error) {
	var existing int
	err := r.runner.QueryRowContext(ctx, "SELECT last_id FROM issue_counter WHERE prefix = ?", prefix).Scan(&existing)
	if err == nil {
		return true, nil
	}
	return false, err
}

func (r *issueLookupRepository) maxExistingCounter(ctx context.Context, prefix string) (int, error) {
	rows, err := r.runner.QueryContext(ctx, "SELECT id FROM issues WHERE id LIKE CONCAT(?, '-%')", prefix)
	if err != nil {
		return 0, fmt.Errorf("scan issues for %q: %w", prefix, err)
	}
	defer rows.Close()

	maxNum := 0
	pfxDash := prefix + "-"
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		if n, ok := counterSuffix(id, pfxDash); ok && n > maxNum {
			maxNum = n
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate issues for %q: %w", prefix, err)
	}
	return maxNum, nil
}

func counterSuffix(id, prefix string) (int, bool) {
	suffix := strings.TrimPrefix(id, prefix)
	if strings.Contains(suffix, ".") {
		return 0, false
	}
	n, err := strconv.Atoi(suffix)
	return n, err == nil
}

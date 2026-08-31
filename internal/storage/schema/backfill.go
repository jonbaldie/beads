package schema

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

func ensureBackfilledCustomStatusesCustomTypes(ctx context.Context, db DBConn) (bool, error) {
	typesWrote, err := backfillCustomTypes(ctx, db)
	if err != nil {
		return typesWrote, fmt.Errorf("backfill custom_types: %w", err)
	}
	statusesWrote, err := backfillCustomStatuses(ctx, db)
	if err != nil {
		return typesWrote || statusesWrote, fmt.Errorf("backfill custom_statuses: %w", err)
	}
	return typesWrote || statusesWrote, nil
}

func needsBackfilledCustomStatusesCustomTypes(ctx context.Context, db DBConn) (bool, error) {
	typesNeed, err := needsCustomTypesBackfill(ctx, db)
	if err != nil {
		return false, fmt.Errorf("custom_types: %w", err)
	}
	statusesNeed, err := needsCustomStatusesBackfill(ctx, db)
	if err != nil {
		return false, fmt.Errorf("custom_statuses: %w", err)
	}
	return typesNeed || statusesNeed, nil
}

func needsCustomTypesBackfill(ctx context.Context, db DBConn) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM custom_types").Scan(&count); err != nil {
		return false, err
	}
	if count > 0 {
		return false, nil
	}

	var value string
	err := db.QueryRowContext(ctx, "SELECT `value` FROM config WHERE `key` = 'types.custom'").Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(issueops.ParseTypesConfigValue(value)) > 0, nil
}

func needsCustomStatusesBackfill(ctx context.Context, db DBConn) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM custom_statuses").Scan(&count); err != nil {
		return false, err
	}
	if count > 0 {
		return false, nil
	}

	var value string
	err := db.QueryRowContext(ctx, "SELECT `value` FROM config WHERE `key` = 'status.custom'").Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(value) == "" {
		return false, nil
	}

	parsed, parseErr := types.ParseCustomStatusConfig(value)
	if parseErr != nil {
		return false, nil
	}
	return len(parsed) > 0, nil
}

func backfillCustomTypes(ctx context.Context, db DBConn) (bool, error) {
	if populated, err := backfillTablePopulated(ctx, db, "custom_types"); err != nil {
		return false, err
	} else if populated {
		return false, nil
	}
	value, found, err := backfillConfigValue(ctx, db, "types.custom")
	if err != nil {
		return false, err
	}
	if !found || value == "" {
		return false, nil
	}
	return insertCustomTypes(ctx, db, value)
}

func backfillCustomStatuses(ctx context.Context, db DBConn) (bool, error) {
	if populated, err := backfillTablePopulated(ctx, db, "custom_statuses"); err != nil {
		return false, err
	} else if populated {
		return false, nil
	}
	value, found, err := backfillConfigValue(ctx, db, "status.custom")
	if err != nil {
		return false, err
	}
	if !found || value == "" {
		return false, nil
	}

	parsed, parseErr := types.ParseCustomStatusConfig(value)
	if parseErr != nil {
		log.Printf("schema: skipping invalid status.custom entries: %v", parseErr)
		return false, nil
	}
	return insertCustomStatuses(ctx, db, parsed)
}

func backfillTablePopulated(ctx context.Context, db DBConn, table string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func backfillConfigValue(ctx context.Context, db DBConn, key string) (string, bool, error) {
	var value string
	err := db.QueryRowContext(ctx, "SELECT `value` FROM config WHERE `key` = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func insertCustomTypes(ctx context.Context, db DBConn, value string) (bool, error) {
	wrote := false
	// ParseTypesConfigValue already trims elements and drops empties.
	for _, name := range issueops.ParseTypesConfigValue(value) {
		res, err := db.ExecContext(ctx, "INSERT IGNORE INTO custom_types (name) VALUES (?)", name)
		if err != nil {
			return wrote, fmt.Errorf("inserting type %q: %w", name, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			wrote = true
		}
	}
	return wrote, nil
}

func insertCustomStatuses(ctx context.Context, db DBConn, statuses []types.CustomStatus) (bool, error) {
	wrote := false
	for _, status := range statuses {
		res, err := db.ExecContext(ctx, "INSERT IGNORE INTO custom_statuses (name, category) VALUES (?, ?)", status.Name, string(status.Category))
		if err != nil {
			return wrote, fmt.Errorf("inserting status %q: %w", status.Name, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			wrote = true
		}
	}
	return wrote, nil
}

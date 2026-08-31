package issueops

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jonbaldie/beads/internal/types"
)

type childCounterBucket struct {
	maxChild int
	isWisp   bool
	known    bool
}

func ReconcileChildCounters(ctx context.Context, tx DBTX, issues []*types.Issue) (map[string]bool, error) {
	parents := collectChildCounterBuckets(issues)
	unknownParentIDs := unknownChildCounterParents(parents)
	wispParents, err := WispIDSetInTx(ctx, tx, unknownParentIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to route child counter parents: %w", err)
	}
	for _, parentID := range unknownParentIDs {
		_, parents[parentID].isWisp = wispParents[parentID]
	}

	var changed map[string]bool
	for parentID, bucket := range parents {
		table, didChange, err := reconcileChildCounter(ctx, tx, parentID, bucket)
		if err != nil {
			return nil, err
		}
		if didChange {
			if changed == nil {
				changed = map[string]bool{}
			}
			changed[table] = true
		}
	}
	return changed, nil
}

func collectChildCounterBuckets(issues []*types.Issue) map[string]*childCounterBucket {
	parents := make(map[string]*childCounterBucket)
	markWispChildCounterParents(parents, issues)
	addChildCounterChildren(parents, issues)
	return parents
}

func markWispChildCounterParents(parents map[string]*childCounterBucket, issues []*types.Issue) {
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		if IsWisp(issue) {
			if b, ok := parents[issue.ID]; ok {
				b.isWisp, b.known = true, true
			} else {
				parents[issue.ID] = &childCounterBucket{isWisp: true, known: true}
			}
		}
	}
}

func addChildCounterChildren(parents map[string]*childCounterBucket, issues []*types.Issue) {
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		parentID, childNum, ok := ParseHierarchicalID(issue.ID)
		if !ok {
			continue
		}
		b, exists := parents[parentID]
		if !exists {
			b = &childCounterBucket{}
			parents[parentID] = b
		}
		if childNum > b.maxChild {
			b.maxChild = childNum
		}
	}
}

func unknownChildCounterParents(parents map[string]*childCounterBucket) []string {
	unknownParentIDs := make([]string, 0, len(parents))
	for parentID, b := range parents {
		if b.maxChild > 0 && !b.known {
			unknownParentIDs = append(unknownParentIDs, parentID)
		}
	}
	return unknownParentIDs
}

func reconcileChildCounter(ctx context.Context, tx DBTX, parentID string, bucket *childCounterBucket) (string, bool, error) {
	if bucket.maxChild == 0 {
		return "", false, nil
	}
	table, parentTable := childCounterTables(bucket.isWisp)
	if exists, err := childCounterParentExists(ctx, tx, parentTable, parentID); err != nil {
		return "", false, err
	} else if !exists {
		return "", false, nil
	}
	current, exists, err := currentChildCounter(ctx, tx, table, parentID)
	if err != nil {
		return "", false, err
	}
	if exists && current >= bucket.maxChild {
		return "", false, nil
	}
	// Qualify the existing-row column so canonical MySQL and SQLite's translated
	// ON CONFLICT forms both refer to the target row, not the incoming value.
	//nolint:gosec // G201: table is one of two hardcoded constants.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %[1]s (parent_id, last_child) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE last_child = GREATEST(%[1]s.last_child, ?)
	`, table), parentID, bucket.maxChild, bucket.maxChild); err != nil {
		return "", false, fmt.Errorf("failed to reconcile child counter for %s: %w", parentID, err)
	}
	return table, true, nil
}

func childCounterTables(isWisp bool) (string, string) {
	if isWisp {
		return "wisp_child_counters", "wisps"
	}
	return "child_counters", "issues"
}

//nolint:gosec // G201: parentTable is one of two hardcoded constants.
func childCounterParentExists(ctx context.Context, tx DBTX, parentTable, parentID string) (bool, error) {
	var parentExists int
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT 1 FROM %s WHERE id = ?
	`, parentTable), parentID).Scan(&parentExists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to check child counter parent %s: %w", parentID, err)
	}
	return true, nil
}

//nolint:gosec // G201: table is one of two hardcoded constants.
func currentChildCounter(ctx context.Context, tx DBTX, table, parentID string) (int, bool, error) {
	var current int
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT last_child FROM %s WHERE parent_id = ?
	`, table), parentID).Scan(&current)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("failed to read child counter for %s: %w", parentID, err)
	}
	return current, true, nil
}

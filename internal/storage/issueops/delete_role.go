package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/workapi"
	publicops "github.com/jonbaldie/beads/issueops"
)

// DeleteInTx is the store-backed body behind issueops.Deleter: the whole of
// `bd delete` from the existence probe to the reference rewrite, inside ONE
// transaction.
//
// It lives here rather than in an importable internal/workapi/store<role>
// package for the reason SweepInTx does: the work is several reads and several
// writes that must see one snapshot.
//
// It assumes a request already refused by workapi.ValidateDeleteRequest and
// already normalized by workapi.NormalizeDeleteIDs. The accessors do both
// BEFORE opening a transaction, so a malformed request costs no database work.
//
// THE REWRITE IS INSIDE THE TRANSACTION. A route that deleted the rows in one
// transaction and rewrote the neighbors' text afterwards left, on a failure
// between the two, a workspace whose rows were gone and whose descriptions
// still cited them.
func DeleteInTx(ctx context.Context, tx *sql.Tx, req publicops.DeleteRequest) (publicops.DeleteResult, error) {
	ids := req.IDs
	result := publicops.DeleteResult{DryRun: req.DryRun}

	// The existence probe comes FIRST, so `bd delete typo real` reports the
	// typo rather than whatever the graph says about the id that resolved.
	if err := validateDeletePresenceInTx(ctx, tx, ids); err != nil {
		return publicops.DeleteResult{}, err
	}

	if err := validateDeleteVersionInTx(ctx, tx, ids, req.ExpectedVersion); err != nil {
		return publicops.DeleteResult{}, err
	}

	idSet := deleteIDSet(ids)

	if !req.Cascade {
		orphaned, err := deleteExternalDependentsInTx(ctx, tx, ids, idSet, req.Force)
		if err != nil {
			return publicops.DeleteResult{}, fmt.Errorf("delete: check dependents: %w", err)
		}
		result.Orphaned = orphaned
	}

	set, err := ResolveDeletionSetInTx(ctx, tx, ids, req.Cascade)
	if err != nil {
		return publicops.DeleteResult{}, fmt.Errorf("delete: %w", err)
	}

	neighbors, err := deleteNeighborsInTx(ctx, tx, set.All)
	if err != nil {
		return publicops.DeleteResult{}, err
	}

	result, err = executeDeleteInTx(ctx, tx, result, set, neighbors, req)
	if err != nil {
		return publicops.DeleteResult{}, err
	}
	return result, nil
}

func validateDeleteVersionInTx(ctx context.Context, tx DBTX, ids []string, expected *int64) error {
	if expected == nil {
		return nil
	}
	return CheckVersionInTx(ctx, tx, ids[0], *expected)
}

func deleteIDSet(ids []string) map[string]bool {
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	return idSet
}

func executeDeleteInTx(ctx context.Context, tx *sql.Tx, result publicops.DeleteResult, set DeletionSet, neighbors []*types.Issue, req publicops.DeleteRequest) (publicops.DeleteResult, error) {
	deleted, err := DeleteResolvedSetInTx(ctx, tx, set, req.DryRun)
	if err != nil {
		return publicops.DeleteResult{}, err
	}
	result.Deleted = deleted.DeletedCount
	result.Dependencies = deleted.DependenciesCount
	result.Labels = deleted.LabelsCount
	result.Events = deleted.EventsCount
	if req.DryRun {
		return result, nil
	}
	rewritten, err := RewriteDeletedReferencesInTx(ctx, tx, set.All, neighbors, req.Actor)
	if err != nil {
		return publicops.DeleteResult{}, err
	}
	result.ReferencesUpdated = rewritten
	return result, nil
}

func validateDeletePresenceInTx(ctx context.Context, tx DBTX, ids []string) error {
	wispSet, err := WispIDSetInTx(ctx, tx, ids)
	if err != nil {
		return fmt.Errorf("delete: classify planes: %w", err)
	}
	found, err := GetIssuesByIDsInTx(ctx, tx, ids, wispSet)
	if err != nil {
		return fmt.Errorf("delete: resolve ids: %w", err)
	}
	present := make(map[string]bool, len(found))
	for _, issue := range found {
		if issue != nil {
			present[issue.ID] = true
		}
	}
	missing := missingDeleteIDs(ids, present)
	if len(missing) > 0 {
		return &publicops.NotFoundError{IDs: missing}
	}
	return nil
}

func missingDeleteIDs(ids []string, present map[string]bool) []string {
	var missing []string
	for _, id := range ids {
		if !present[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

func deleteExternalDependentsInTx(ctx context.Context, tx DBTX, ids []string, idSet map[string]bool, force bool) ([]string, error) {
	external, err := ExternalDependentsBySourceInTx(ctx, tx, ids, idSet)
	if err != nil {
		return nil, err
	}
	if !force {
		for _, id := range ids {
			if deps := external[id]; len(deps) > 0 {
				return nil, &publicops.DependentsOutsideRequestError{IssueID: id, Dependents: deps}
			}
		}
		return nil, nil
	}
	orphaned := make(map[string]bool)
	for _, deps := range external {
		for _, id := range deps {
			orphaned[id] = true
		}
	}
	return workapi.SortedDeleteIDs(orphaned), nil
}

// ExternalDependentsBySourceInTx reports, for each of ids, the DIRECT
// dependents that idSet does not contain — the rows a forced delete orphans
// and an unforced one refuses over.
//
// The per-source shape is what lets the unforced refusal name ONE blocked id
// instead of a flat union that answers "something is blocked".
//
//nolint:gosec // G201: inClause contains only ? placeholders
func ExternalDependentsBySourceInTx(ctx context.Context, tx DBTX, ids []string, idSet map[string]bool) (map[string][]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	bySource := make(map[string]map[string]bool)
	total := len(ids)
	for i := 0; i < total; i += deleteBatchSize {
		end := i + deleteBatchSize
		if end > total {
			end = total
		}
		inClause, args := buildSQLInClause(ids[i:end])

		for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
			if err := readExternalDependentsFromTable(ctx, tx, depTable, inClause, args, idSet, bySource); err != nil {
				return nil, err
			}
		}
	}

	out := make(map[string][]string, len(bySource))
	for target, dependents := range bySource {
		out[target] = workapi.SortedDeleteIDs(dependents)
	}
	return out, nil
}

//nolint:gosec // G201: inClause contains only ? placeholders
func readExternalDependentsFromTable(ctx context.Context, tx DBTX, depTable, inClause string, args []interface{}, idSet map[string]bool, bySource map[string]map[string]bool) error {
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s AS depends_on_id, issue_id FROM %s WHERE %s`,
			DepTargetExpr, depTable, depTargetIn("", inClause)),
		args...)
	if err != nil {
		if optionalBlockedTable(depTable) && isTableNotExistError(err) {
			return nil
		}
		return fmt.Errorf("query dependents from %s: %w", depTable, err)
	}
	for rows.Next() {
		var target, dependent string
		if err := rows.Scan(&target, &dependent); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan dependent: %w", err)
		}
		if idSet[dependent] {
			continue
		}
		if bySource[target] == nil {
			bySource[target] = make(map[string]bool)
		}
		bySource[target][dependent] = true
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate dependents from %s: %w", depTable, err)
	}
	return nil
}

// deleteNeighborsInTx hydrates the SURVIVING rows joined to the deletion set
// by a dependency edge in either direction — the rows whose text the deletion
// rewrites.
//
// One query per plane over the whole set, so a `--from-file` batch costs two
// queries rather than two per deleted id.
//
//nolint:gosec // G201: inClause contains only ? placeholders
func deleteNeighborsInTx(ctx context.Context, tx DBTX, ids []string) ([]*types.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	deleting := make(map[string]bool, len(ids))
	for _, id := range ids {
		deleting[id] = true
	}

	neighborIDs, err := loadDeleteNeighborIDs(ctx, tx, ids, deleting)
	if err != nil {
		return nil, err
	}
	if len(neighborIDs) == 0 {
		return nil, nil
	}

	// Sorted so the rewrite touches rows in a stable order, which is what
	// makes a partially-applied failure reproducible.
	hydrate := workapi.SortedDeleteIDs(neighborIDs)
	// An `external:` target and a target belonging to another repository name
	// no row here; GetIssuesByIDsInTx simply does not return them.
	issues, err := GetIssuesByIDsInTx(ctx, tx, hydrate, nil)
	if err != nil {
		return nil, fmt.Errorf("hydrate neighbors: %w", err)
	}
	return issues, nil
}

//nolint:gosec // G201: inClause contains only ? placeholders
func loadDeleteNeighborIDs(ctx context.Context, tx DBTX, ids []string, deleting map[string]bool) (map[string]bool, error) {
	neighborIDs := make(map[string]bool)
	total := len(ids)
	for i := 0; i < total; i += deleteBatchSize {
		end := i + deleteBatchSize
		if end > total {
			end = total
		}
		inClause, args := buildSQLInClause(ids[i:end])
		doubled := append(append([]interface{}{}, args...), args...)

		for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
			if err := readDeleteNeighborsFromTable(ctx, tx, depTable, inClause, doubled, deleting, neighborIDs); err != nil {
				return nil, err
			}
		}
	}
	return neighborIDs, nil
}

//nolint:gosec // G201: inClause contains only ? placeholders
func readDeleteNeighborsFromTable(ctx context.Context, tx DBTX, depTable, inClause string, args []interface{}, deleting, neighborIDs map[string]bool) error {
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`SELECT issue_id, %s AS depends_on_id FROM %s WHERE issue_id IN (%s) OR %s`,
			DepTargetExpr, depTable, inClause, depTargetIn("", inClause)),
		args...)
	if err != nil {
		if optionalBlockedTable(depTable) && isTableNotExistError(err) {
			return nil
		}
		return fmt.Errorf("query neighbors from %s: %w", depTable, err)
	}
	for rows.Next() {
		var source, target string
		if err := rows.Scan(&source, &target); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan neighbor: %w", err)
		}
		appendDeleteNeighborCandidates(source, target, deleting, neighborIDs)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate neighbors from %s: %w", depTable, err)
	}
	return nil
}

func appendDeleteNeighborCandidates(source, target string, deleting, neighborIDs map[string]bool) {
	for _, candidate := range [2]string{source, target} {
		if candidate == "" || deleting[candidate] {
			continue
		}
		neighborIDs[candidate] = true
	}
}

// RewriteDeletedReferencesInTx replaces every word-boundary occurrence of a
// deleted id with `[deleted:<id>]` in each neighbor's description, notes,
// design and acceptance criteria, and reports how many ROWS it changed.
//
// Exported because the unit-of-work body needs the same rule and neither
// implementation may own it: a route that spelled the pattern differently
// would rewrite a different set of citations for the same deletion.
func RewriteDeletedReferencesInTx(ctx context.Context, tx DBTX, deletedIDs []string, neighbors []*types.Issue, actor string) (int, error) {
	if len(neighbors) == 0 {
		return 0, nil
	}
	touched := make(map[string]bool)
	for _, id := range deletedIDs {
		if err := rewriteDeletedReferenceForID(ctx, tx, id, neighbors, actor, touched); err != nil {
			return 0, err
		}
	}
	return len(touched), nil
}

func rewriteDeletedReferenceForID(ctx context.Context, tx DBTX, deletedID string, neighbors []*types.Issue, actor string, touched map[string]bool) error {
	re := DeletedReferencePattern(deletedID)
	replacement := `$1[deleted:` + deletedID + `]$3`
	for _, neighbor := range neighbors {
		if neighbor == nil {
			continue
		}
		updates := rewriteNeighborFields(neighbor, re, replacement)
		if len(updates) == 0 {
			continue
		}
		if _, err := UpdateIssueInTx(ctx, tx, neighbor.ID, updates, actor); err != nil {
			return fmt.Errorf("rewrite references in %s: %w", neighbor.ID, err)
		}
		touched[neighbor.ID] = true
	}
	return nil
}

func rewriteNeighborFields(neighbor *types.Issue, re *regexp.Regexp, replacement string) map[string]interface{} {
	updates := make(map[string]interface{})
	for _, field := range []struct {
		column string
		value  *string
	}{
		{"description", &neighbor.Description},
		{"notes", &neighbor.Notes},
		{"design", &neighbor.Design},
		{"acceptance_criteria", &neighbor.AcceptanceCriteria},
	} {
		if *field.value == "" || !re.MatchString(*field.value) {
			continue
		}
		rewritten := re.ReplaceAllString(*field.value, replacement)
		updates[field.column] = rewritten
		// Write the rewrite back onto the in-memory row so a second deleted id in
		// the same field sees the first one's result rather than re-reading the
		// original.
		*field.value = rewritten
	}
	return updates
}

// DeletedReferencePattern is the citation rule, in one place: a literal id at
// ASCII word boundaries, where a word character includes the hyphen an id is
// full of. It matches `be-1` in "see (be-1)." and not inside `xbe-1` or
// `be-12`.
func DeletedReferencePattern(id string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^A-Za-z0-9_-])(` + regexp.QuoteMeta(id) + `)($|[^A-Za-z0-9_-])`)
}

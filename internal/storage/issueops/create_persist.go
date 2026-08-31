package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/depid"
	"github.com/jonbaldie/beads/internal/types"
)

// CreateIssueResult reports the tables actually written by CreateIssueInTx.
type CreateIssueResult struct {
	ChangedTables map[string]bool
	// StaleRejected reports that the RejectStaleUpserts guard kept the stored
	// row: nothing was written, and the issue's aux data must not be
	// persisted by later batch stages either (bd-578h9.8).
	StaleRejected         bool
	persistedDependencies []persistedDependency
	// persistedComments are the comments this create actually inserted, carried
	// up to the entry point so their journal rows land AFTER the create's. A
	// consumer must never see a comment for a bead it has not been told about.
	persistedComments []EventComment
}

type persistedDependency struct {
	source     string
	target     string
	depType    types.DependencyType
	sourceWisp bool
}

type pendingDependency struct {
	dep      *types.Dependency
	depTable string
}

func PersistLabels(ctx context.Context, tx DBTX, issue *types.Issue, actor, eventTable string) (CreateIssueResult, error) {
	var result CreateIssueResult
	if len(issue.Labels) == 0 {
		return result, nil
	}
	labelTable := "labels"
	if IsWisp(issue) {
		labelTable = "wisp_labels"
	}
	seen := make(map[string]struct{}, len(issue.Labels))
	for _, label := range issue.Labels {
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		labelResult, err := persistLabel(ctx, tx, labelTable, eventTable, issue.ID, label, actor)
		if err != nil {
			return result, err
		}
		if labelResult.ChangedTables == nil {
			continue
		}
		result.ChangedTables = mergeChangedTables(result.ChangedTables, labelResult.ChangedTables)
	}
	return result, nil
}

//nolint:gosec // G201: labelTable is determined by the issue's storage bucket.
func persistLabel(ctx context.Context, tx DBTX, labelTable, eventTable, issueID, label, actor string) (CreateIssueResult, error) {
	var result CreateIssueResult
	// Reject an over-length label before the INSERT IGNORE, which would otherwise
	// silently truncate it to VARCHAR(255). This is the create and import
	// chokepoint (AddLabelInTx guards the bd label-add path).
	if err := types.CheckFieldLen("label", label); err != nil {
		return result, err
	}
	sqlResult, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT IGNORE INTO %s (issue_id, label)
		VALUES (?, ?)
	`, labelTable), issueID, label)
	if err != nil {
		return result, fmt.Errorf("failed to insert label %q for %s: %w", label, issueID, err)
	}
	rowsAffected, err := sqlResult.RowsAffected()
	if err != nil {
		return result, fmt.Errorf("failed to check label insert result for %q on %s: %w", label, issueID, err)
	}
	if rowsAffected == 0 {
		return result, nil
	}
	result.markChanged(labelTable)
	if err := InsertDerivedEvent(ctx, tx, eventTable, AuxEvent{
		IssueID:   issueID,
		EventType: types.EventLabelAdded,
		Actor:     actor,
		Comment:   str("Added label: " + label),
	}); err != nil {
		return result, fmt.Errorf("failed to record label event %q for %s: %w", label, issueID, err)
	}
	result.markChanged(eventTable)
	return result, nil
}

func PersistComments(ctx context.Context, tx DBTX, issue *types.Issue) (CreateIssueResult, error) {
	var result CreateIssueResult
	if len(issue.Comments) == 0 {
		return result, nil
	}
	commentTable := "comments"
	if IsWisp(issue) {
		commentTable = "wisp_comments"
	}
	for _, comment := range issue.Comments {
		commentResult, err := persistComment(ctx, tx, commentTable, issue.ID, comment)
		if err != nil {
			return result, err
		}
		result.ChangedTables = mergeChangedTables(result.ChangedTables, commentResult.ChangedTables)
		result.persistedComments = append(result.persistedComments, commentResult.persistedComments...)
	}
	return result, nil
}

func persistComment(ctx context.Context, tx DBTX, commentTable, issueID string, comment *types.Comment) (CreateIssueResult, error) {
	var result CreateIssueResult
	createdAt := comment.CreatedAt
	if createdAt.IsZero() {
		// No supplied timestamp: this is a live comment, so stamp it the same way
		// AddIssueComment does — one second past the issue's newest comment when
		// the clock second would collide.
		stamped, err := NextLiveCommentTime(ctx, tx, commentTable, issueID, time.Now())
		if err != nil {
			return result, fmt.Errorf("failed to insert comment for %s: %w", issueID, err)
		}
		createdAt = stamped
	}
	createdAtText := FormatAuxTime(createdAt)
	if comment.ID == "" {
		return persistDerivedComment(ctx, tx, commentTable, issueID, comment, createdAt, createdAtText)
	}
	return persistImportedComment(ctx, tx, commentTable, issueID, comment, createdAt, createdAtText)
}

//nolint:gosec // G201: commentTable is determined by the issue's storage bucket.
func persistDerivedComment(ctx context.Context, tx DBTX, commentTable, issueID string, comment *types.Comment, createdAt time.Time, createdAtText string) (CreateIssueResult, error) {
	var result CreateIssueResult
	// No incoming id (fresh comment): content-derived id, collapsing onto an
	// identical existing row exactly like the import dedup.
	id, existed, err := InsertDerivedComment(ctx, tx, commentTable, issueID, comment.Author, comment.Text, createdAtText)
	if err != nil {
		return result, fmt.Errorf("failed to insert comment for %s: %w", issueID, err)
	}
	comment.ID = id
	if existed {
		return result, nil
	}
	result.markChanged(commentTable)
	result.persistedComments = append(result.persistedComments, EventComment{
		ID: id, Author: comment.Author, Text: comment.Text, CreatedAt: createdAt, Source: CommentSourceStructured,
	})
	return result, nil
}

//nolint:gosec // G201: commentTable is determined by the issue's storage bucket.
func persistImportedComment(ctx context.Context, tx DBTX, commentTable, issueID string, comment *types.Comment, createdAt time.Time, createdAtText string) (CreateIssueResult, error) {
	var result CreateIssueResult
	// Incoming id (import/interchange): preserve it, with the historical
	// existence check preventing duplicates on re-import.
	var exists int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`
			SELECT COUNT(*) FROM %s
			WHERE issue_id = ? AND author = ? AND created_at = ? AND text = ?
		`, commentTable), issueID, comment.Author, createdAtText, comment.Text).Scan(&exists); err != nil {
		return result, fmt.Errorf("failed to check comment existence for %s: %w", issueID, err)
	}
	if exists > 0 {
		return result, nil
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, issue_id, author, text, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, commentTable), comment.ID, issueID, comment.Author, comment.Text, createdAtText)
	if err != nil {
		return result, fmt.Errorf("failed to insert comment for %s: %w", issueID, err)
	}
	result.markChanged(commentTable)
	result.persistedComments = append(result.persistedComments, EventComment{
		ID: comment.ID, Author: comment.Author, Text: comment.Text, CreatedAt: createdAt, Source: CommentSourceStructured,
	})
	return result, nil
}

func PersistDependencies(ctx context.Context, tx DBTX, issues []*types.Issue, actor string) error {
	_, err := PersistDependenciesWithResult(ctx, tx, issues, actor)
	return err
}

func PersistDependenciesWithResult(ctx context.Context, tx DBTX, issues []*types.Issue, actor string) (CreateIssueResult, error) {
	return PersistDependenciesWithOptionsResult(ctx, tx, issues, actor, storage.BatchCreateOptions{})
}

func PersistDependenciesWithOptionsResult(ctx context.Context, tx DBTX, issues []*types.Issue, actor string, opts storage.BatchCreateOptions) (CreateIssueResult, error) {
	var result CreateIssueResult
	pending := collectPendingDependencies(ctx, tx, issues)
	for phase := 0; phase < 2; phase++ {
		phaseResult, err := persistDependencyPhase(ctx, tx, pending, actor, opts, phase == 0)
		result = mergeCreateIssueResults(result, phaseResult)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func collectPendingDependencies(ctx context.Context, tx DBTX, issues []*types.Issue) []pendingDependency {
	var pending []pendingDependency
	for _, issue := range issues {
		if len(issue.Dependencies) == 0 {
			continue
		}
		for _, dep := range issue.Dependencies {
			// Default IssueID to the owning issue when not pre-set (e.g.,
			// markdown bulk create where the ID is auto-generated).
			if dep.IssueID == "" {
				dep.IssueID = issue.ID
			}
			depTable := "dependencies"
			if IsActiveWispInTx(ctx, tx, dep.IssueID) {
				depTable = "wisp_dependencies"
			}
			pending = append(pending, pendingDependency{dep: dep, depTable: depTable})
		}
	}
	return pending
}

func mergeCreateIssueResults(dst, src CreateIssueResult) CreateIssueResult {
	dst.ChangedTables = mergeChangedTables(dst.ChangedTables, src.ChangedTables)
	dst.persistedDependencies = append(dst.persistedDependencies, src.persistedDependencies...)
	return dst
}

func persistDependencyPhase(ctx context.Context, tx DBTX, pending []pendingDependency, actor string, opts storage.BatchCreateOptions, parentPhase bool) (CreateIssueResult, error) {
	var result CreateIssueResult
	for _, item := range pending {
		if (item.dep.Type == types.DepParentChild) != parentPhase {
			continue
		}
		itemResult, err := persistPendingDependency(ctx, tx, item, actor, opts)
		if err != nil {
			return result, err
		}
		result = mergeCreateIssueResults(result, itemResult)
	}
	return result, nil
}

func persistPendingDependency(ctx context.Context, tx DBTX, item pendingDependency, actor string, opts storage.BatchCreateOptions) (CreateIssueResult, error) {
	dep := item.dep
	kind, keep, err := validatePendingDependency(ctx, tx, dep, opts)
	if err != nil {
		return CreateIssueResult{}, err
	}
	if !keep {
		return CreateIssueResult{}, nil
	}
	createdAt := dep.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	metadata := dep.Metadata
	if metadata == "" {
		metadata = "{}"
	}
	return insertPendingDependency(ctx, tx, item, kind, actor, createdAt, metadata)
}

func validatePendingDependency(ctx context.Context, tx DBTX, dep *types.Dependency, opts storage.BatchCreateOptions) (DepTargetKind, bool, error) {
	isCrossPrefix := types.ExtractPrefix(dep.IssueID) != types.ExtractPrefix(dep.DependsOnID)
	kind := ClassifyDepTarget(ctx, tx, dep, isCrossPrefix)
	if kind != DepTargetExternal {
		exists, err := dependencyTargetExists(ctx, tx, kind, dep)
		if err != nil {
			return kind, false, err
		}
		if !exists {
			recordSkippedDependency(opts, dep, "target not found")
			return kind, false, nil
		}
	}
	if err := validatePendingDependencyRelations(ctx, tx, dep, kind); err != nil {
		if opts.SkipDependencyValidationErrors {
			recordSkippedDependency(opts, dep, err.Error())
			return kind, false, nil
		}
		return kind, false, fmt.Errorf("invalid dependency %s -> %s: %w", dep.IssueID, dep.DependsOnID, err)
	}
	return kind, true, nil
}

//nolint:gosec // G201: lookupTable is one of two hardcoded constants.
func dependencyTargetExists(ctx context.Context, tx DBTX, kind DepTargetKind, dep *types.Dependency) (bool, error) {
	lookupTable := "issues"
	if kind == DepTargetWisp {
		lookupTable = "wisps"
	}
	var exists int
	if err := tx.QueryRowContext(ctx,
		fmt.Sprintf("SELECT 1 FROM %s WHERE id = ?", lookupTable),
		dep.DependsOnID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("failed to check dependency target %s for %s: %w", dep.DependsOnID, dep.IssueID, err)
	}
	return true, nil
}

func validatePendingDependencyRelations(ctx context.Context, tx DBTX, dep *types.Dependency, kind DepTargetKind) error {
	if kind != DepTargetExternal && types.ExtractPrefix(dep.IssueID) == types.ExtractPrefix(dep.DependsOnID) {
		if err := CheckBlockingHierarchyInTx(ctx, tx, dep, nil); err != nil {
			return err
		}
	}
	return CheckDependencyCycleInTx(ctx, tx, dep, nil)
}

//nolint:gosec // G201: item.depTable is hardcoded; the target column is selected by kind.
func insertPendingDependency(ctx context.Context, tx DBTX, item pendingDependency, kind DepTargetKind, actor string, createdAt time.Time, metadata string) (CreateIssueResult, error) {
	dep := item.dep
	// Deterministic id from (issue_id, target) keeps bulk-imported edges merge-safe
	// across clones — two clones importing the same JSONL get one primary key.
	createdBy := dependencyCreatedBy(dep, actor)
	sqlResult, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO %s (id, issue_id, %s, type, created_by, created_at, metadata, thread_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE type = type
		`, item.depTable, kind.Column()), depid.New(dep.IssueID, dep.DependsOnID), dep.IssueID, dep.DependsOnID, dep.Type, createdBy, createdAt, metadata, dep.ThreadID)
	if err != nil {
		return CreateIssueResult{}, fmt.Errorf("failed to insert dependency %s -> %s: %w", dep.IssueID, dep.DependsOnID, err)
	}
	rowsAffected, err := sqlResult.RowsAffected()
	if err != nil {
		return CreateIssueResult{}, fmt.Errorf("failed to check dependency insert result for %s -> %s: %w", dep.IssueID, dep.DependsOnID, err)
	}
	if rowsAffected == 0 {
		return CreateIssueResult{}, nil
	}
	var result CreateIssueResult
	result.markChanged(item.depTable)
	result.persistedDependencies = append(result.persistedDependencies, persistedDependency{
		source:     dep.IssueID,
		target:     dep.DependsOnID,
		depType:    dep.Type,
		sourceWisp: item.depTable == "wisp_dependencies",
	})
	if dep.Type == types.DepParentChild {
		if err := TouchDependencyCoordinationTableInTx(ctx, tx, dep.DependsOnID, item.depTable); err != nil {
			return result, err
		}
	}
	// Creation-time edges are independently replayable operations; do not rely
	// on the issue create payload's inline dependencies.
	if err := RecordDepEventInTx(ctx, tx, EventDepAdd, dep.IssueID, string(dep.Type), dep.DependsOnID, metadata); err != nil {
		return result, err
	}
	return result, nil
}

// dependencyCreatedBy returns the author stamped on a dependency edge.
// Import/restore paths populate dep.CreatedBy from JSONL; interactive
// creation leaves it empty and falls back to the current actor.
func dependencyCreatedBy(dep *types.Dependency, actor string) string {
	if dep != nil && dep.CreatedBy != "" {
		return dep.CreatedBy
	}
	return actor
}

func recordSkippedDependency(opts storage.BatchCreateOptions, dep *types.Dependency, reason string) {
	if dep == nil {
		return
	}
	recordSkippedDependencyEdge(opts, dep.IssueID, dep.DependsOnID, reason)
}

func recordSkippedDependencyEdge(opts storage.BatchCreateOptions, issueID, dependsOnID, reason string) {
	if opts.OnSkippedDependency == nil {
		return
	}
	opts.OnSkippedDependency(issueID, dependsOnID, reason)
}

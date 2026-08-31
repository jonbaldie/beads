package query

import (
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

// QueryResult contains the result of evaluating a query.
// For simple queries, Filter will be populated and Predicate will be nil.
// For complex queries with OR, Predicate will be set and Filter will contain
// base filters that can pre-filter issues.
type QueryResult struct {
	// Filter contains filters that can be passed to SearchIssues.
	// This is always populated with at least base filters.
	Filter types.IssueFilter

	// Predicate is a function that evaluates whether an issue matches the query.
	// If nil, the Filter alone is sufficient.
	// If non-nil, issues matching Filter should be further filtered by Predicate.
	Predicate func(*types.Issue) bool

	// RequiresPredicate indicates if in-memory filtering is needed.
	// True when the query contains OR or complex NOT expressions.
	RequiresPredicate bool
}

// NewEvaluator creates a new Evaluator with the given reference time.
func NewEvaluator(now time.Time) *Evaluator {
	return &Evaluator{now: now}
}

// Evaluate evaluates the query AST and returns a QueryResult.
func (e *Evaluator) Evaluate(node Node) (*QueryResult, error) {
	result := &QueryResult{
		Filter: types.IssueFilter{},
	}

	// Check if we can use Filter-only mode (simple AND chains)
	if e.canUseFilterOnly(node) {
		if err := e.buildFilter(node, &result.Filter); err != nil {
			return nil, err
		}
		return result, nil
	}

	// Complex query: build predicate and extract base filters
	pred, err := e.buildPredicate(node)
	if err != nil {
		return nil, err
	}
	result.Predicate = pred
	result.RequiresPredicate = true

	// Extract base filters for pre-filtering (optional optimization)
	e.extractBaseFilters(node, &result.Filter)

	return result, nil
}

// canUseFilterOnly returns true if the query can be expressed as IssueFilter only.
// This is true for:
// - Simple comparisons
// - AND chains of simple comparisons
// - NOT with certain fields
func (e *Evaluator) canUseFilterOnly(node Node) bool {
	switch n := node.(type) {
	case *ComparisonNode:
		return true
	case *AndNode:
		return e.canUseFilterOnly(n.Left) && e.canUseFilterOnly(n.Right)
	case *NotNode:
		// NOT is only filter-compatible for certain fields
		if comp, ok := n.Operand.(*ComparisonNode); ok {
			switch comp.Field {
			case "status":
				return comp.Op == OpEquals
			case "type":
				return comp.Op == OpEquals
			default:
				return false
			}
		}
		return false
	case *OrNode:
		// OR can be filter-compatible for labels
		return e.canUseLabelsAnyOptimization(n)
	default:
		return false
	}
}

// canUseLabelsAnyOptimization checks if an OR node can use LabelsAny.
func (e *Evaluator) canUseLabelsAnyOptimization(node *OrNode) bool {
	labels := e.collectOrLabels(node)
	return len(labels) > 0
}

// collectOrLabels collects label values from an OR chain of label=X comparisons.
// Returns nil if the OR chain contains non-label comparisons.
func (e *Evaluator) collectOrLabels(node Node) []string {
	switch n := node.(type) {
	case *ComparisonNode:
		if (n.Field == "label" || n.Field == "labels") && n.Op == OpEquals {
			return []string{n.Value}
		}
		return nil
	case *OrNode:
		left := e.collectOrLabels(n.Left)
		right := e.collectOrLabels(n.Right)
		if left == nil || right == nil {
			return nil
		}
		return append(left, right...)
	default:
		return nil
	}
}

// buildFilter populates the IssueFilter from a filter-compatible AST.
func (e *Evaluator) buildFilter(node Node, filter *types.IssueFilter) error {
	switch n := node.(type) {
	case *ComparisonNode:
		return e.applyComparison(n, filter)
	case *AndNode:
		if err := e.buildFilter(n.Left, filter); err != nil {
			return err
		}
		return e.buildFilter(n.Right, filter)
	case *NotNode:
		return e.applyNot(n, filter)
	case *OrNode:
		// Only reached for LabelsAny optimization
		labels := e.collectOrLabels(n)
		if labels != nil {
			filter.LabelsAny = append(filter.LabelsAny, labels...)
			return nil
		}
		return fmt.Errorf("OR not supported for this field combination")
	default:
		return fmt.Errorf("unexpected node type: %T", node)
	}
}

type comparisonFilterBuilder func(*Evaluator, *ComparisonNode, *types.IssueFilter) error

var comparisonFilterBuilders = map[string]comparisonFilterBuilder{
	"status":           (*Evaluator).applyStatusFilter,
	"priority":         (*Evaluator).applyPriorityFilter,
	"type":             (*Evaluator).applyTypeFilter,
	"assignee":         (*Evaluator).applyAssigneeFilter,
	"owner":            (*Evaluator).applyOwnerFilter,
	"label":            (*Evaluator).applyLabelFilter,
	"labels":           (*Evaluator).applyLabelFilter,
	"title":            (*Evaluator).applyTitleFilter,
	"description":      (*Evaluator).applyDescriptionFilter,
	"desc":             (*Evaluator).applyDescriptionFilter,
	"notes":            (*Evaluator).applyNotesFilter,
	"created":          (*Evaluator).applyCreatedFilter,
	"created_at":       (*Evaluator).applyCreatedFilter,
	"updated":          (*Evaluator).applyUpdatedFilter,
	"updated_at":       (*Evaluator).applyUpdatedFilter,
	"closed":           (*Evaluator).applyClosedFilter,
	"closed_at":        (*Evaluator).applyClosedFilter,
	"started":          (*Evaluator).applyStartedFilter,
	"started_at":       (*Evaluator).applyStartedFilter,
	"id":               (*Evaluator).applyIDFilter,
	"spec":             (*Evaluator).applySpecFilter,
	"spec_id":          (*Evaluator).applySpecFilter,
	"parent":           (*Evaluator).applyParentFilter,
	"mol_type":         (*Evaluator).applyMolTypeFilter,
	"has_metadata_key": (*Evaluator).applyHasMetadataKeyFilter,
}

var booleanFilterFields = map[string]string{
	"pinned":    "pinned",
	"ephemeral": "ephemeral",
	"template":  "template",
}

package query

import "time"

// Evaluator converts a query AST to an IssueFilter and/or predicate function.
type Evaluator struct {
	now time.Time
}

// Keep the field visible to file-local unused-code analysis; evaluator behavior
// is implemented in evaluator.go.
var _ = Evaluator{now: time.Time{}}

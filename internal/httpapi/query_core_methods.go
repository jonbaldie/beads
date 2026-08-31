package httpapi

import (
	"fmt"
	"strconv"
)

// refuse records a refusal unless one is already recorded.
func (q *query) refuse(res Result) {
	if q.res == nil {
		q.res = &res
	}
}

func (q *query) invalid(param, detail string) {
	q.refuse(InvalidArgument(param, ReasonInvalidValue, detail))
}

// has reports whether the parameter was supplied, and marks it known.
func (q *query) has(name string) bool {
	q.read[name] = true
	return len(q.values[name]) > 0
}

// str reads a single-valued string parameter. A repeated parameter is refused
// rather than silently resolved to one of its values.
func (q *query) str(name string) string {
	if !q.has(name) {
		return ""
	}
	vals := q.values[name]
	if len(vals) > 1 {
		q.invalid(name, "this parameter takes a single value")
		return ""
	}
	return vals[0]
}

// list reads a repeatable parameter, returning the values as supplied. Comma
// splitting, where the document allows it, happens in the filter builder that
// owns the vocabulary — not here.
func (q *query) list(name string) []string {
	q.read[name] = true
	return q.values[name]
}

// integer reads an optional integer, returning nil when absent so a caller can
// tell "unset" from an explicit value.
func (q *query) integer(name string) *int {
	raw := q.str(name)
	if raw == "" {
		return nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		q.invalid(name, fmt.Sprintf("%q is not an integer", raw))
		return nil
	}
	return &v
}

// offender names the value a refusal turned down, for the request line.
func (r Result) offender() string {
	if r.Problem.Param != nil {
		return *r.Problem.Param
	}
	return ""
}

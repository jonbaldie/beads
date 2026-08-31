package db

import (
	"fmt"

	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

// searchWindow is the row window one search runs under: the clause the engine
// carries, and what this package must still do to the rows that come back. It
// is one value rather than two decisions at each call site because the halves
// only add up when they are decided together.
//
// WHO CARRIES THE OFFSET is the whole of it, and A CAP DECIDES IT. MaxRows
// counts the rows the query TOUCHED — offset + limit — because a row the engine
// skipped is still a row it matched, so an offset walks a caller toward the
// breaker and must never walk one past it. When a cap is set the skip therefore
// stays HERE: the bound covers the rows this package is about to drop, and
// EnforceMaxRowsCap counts them. That is the same window the store-backed seam
// runs under, which reaches it by widening the filter before it sizes its probe
// row (internal/workapi.WithRowsBeforeThePage, then WithFetchOneExtra), so one
// request fires on both.
//
// With no cap the engine carries it — LIMIT n OFFSET k — except when there is
// no LIMIT to hang one on. MySQL has no OFFSET without a LIMIT, and the
// sentinel this used to render for that shape
// ("LIMIT 18446744073709551615 OFFSET k") makes the Dolt engine size a result
// buffer from limit+offset and answer with a recovered "makeslice: cap out of
// range" out of topRowsIter instead of rows. An unbounded query reads every
// matching row anyway, so skipping here is the same answer at the same cost.
//
// THE CALLERS THAT COMBINE AN OFFSET WITH A CAP are the ones that consume the
// FILTER as a value and run their own query: proxied `bd list --watch`, with
// and without --ready, and the proxied hierarchical --parent walk. They used to
// get the engine's OFFSET and a cap counted on the survivors, which answered a
// page for the very request `bd list --offset --max-rows` refuses one row of
// result set earlier. The role that pages — issueops.Reader — hands this seam
// no offset at all; it applies its own in the shared page epilogue for the
// reason internal/workapi.FinishPageAt gives. The unit-of-work Querier does set
// one here, and a query request carries no cap, so that is the shape still
// pushing the skip down.
type searchWindow struct {
	// sql is the LIMIT/OFFSET clause, empty for an unbounded scan.
	sql string
	// skip is the offset the clause did not carry.
	skip int
	// limit is the page the caller receives; 0 is unlimited.
	limit int
	// rowCap and capSource are the defensive cap and its attribution. rowCap is
	// SearchProbeLimit's cap, not the filter's: at touched == maxRows the probe
	// row moves it by one.
	rowCap    int
	capSource string
	// legLimit is the deepest row index the outer query can reach, and so the
	// bound each UNION leg may carry. 0 means the legs stay unbounded, either
	// because the search is unbounded or because its order is not one SQL can
	// express — see legWindowSQL.
	legLimit int
}

// searchWindowFor sizes one search's window. goSideSort reports a sort key SQL
// renders no ORDER BY for (sqlbuild.IsGoSideSort — "id", which needs the
// natural-numeric comparison that puts bd-9 before bd-10).
//
// A PAGE BOUND IS ONLY EVER PUSHED UNDER AN ORDER THE QUERY CAN EXPRESS, and
// that is the whole of what goSideSort changes here. A LIMIT with no ORDER BY
// does not return the first n rows; it returns n rows. Measured on a two-plane
// store with ids interleaved across the planes, `--sort id --limit 5` answered
// with five DURABLE rows and no wisp at all — a page whose order was right, and
// whose membership silently excluded an entire plane.
//
// So a Go-side sort takes the same window internal/workapi.SQLLimit gives the
// reader role for the same reason: none. The query returns the complete
// matching set, the comparator that can express the order runs over it, and
// FinishPageAt's cut is the only thing bounding the page — "this is where the
// requested order first exists", as its doc says. The CAP is not waived with
// it: maxRows still sizes a bound, so a Go-side sort over a set too large to
// order in memory reports ErrTooManyRows instead of scanning the workspace.
//
// The cost is stated rather than absorbed: a caller combining `--sort id` with
// a small limit and NO cap now reads every matching row where it used to read
// limit+1. It read the wrong ones, and the reader role has always paid this
// price on the same query.
func searchWindowFor(limit, offset, maxRows int, maxRowsSource string, goSideSort bool) searchWindow {
	// The window the cap is sized against is the one the query TOUCHES, so a
	// skip this package keeps has to be inside the bound. An unbounded limit
	// already carries every matching row and needs no widening — the same two
	// lines WithRowsBeforeThePage runs on the filter for the other seam.
	pageLimit := limit
	if goSideSort {
		pageLimit = 0
	}
	skipHere := maxRows > 0 && offset > 0
	touched := pageLimit
	if skipHere && pageLimit > 0 {
		touched += offset
	}
	bound, rowCap := issueops.SearchProbeLimit(touched, maxRows)
	w := searchWindow{limit: limit, rowCap: rowCap, capSource: maxRowsSource}
	switch {
	case bound <= 0:
		w.skip = offset
	case skipHere:
		w.skip, w.sql = offset, fmt.Sprintf("LIMIT %d", bound)
		w.legLimit = bound
	case offset > 0:
		w.sql = fmt.Sprintf("LIMIT %d OFFSET %d", bound, offset)
		w.legLimit = bound + offset
	default:
		w.sql = fmt.Sprintf("LIMIT %d", bound)
		w.legLimit = bound
	}
	if goSideSort {
		// The bound that survives above is the CAP's, not the page's, and a cap
		// is a defensive ceiling on an unordered scan rather than a top-n. It
		// stays on the outer query, where trimming it is the caller's error;
		// it must not become a leg's idea of which rows matter.
		w.legLimit = 0
	}
	return w
}

func searchWindowForFilter(filter types.IssueFilter) searchWindow {
	return searchWindowFor(filter.Limit, filter.Offset, filter.MaxRows, filter.MaxRowsSource, sqlbuild.IsGoSideSort(filter.SortBy))
}

// readyWindowForFilter sizes the ready query's window. types.WorkFilter carries
// no sort key — ready has one policy order, which SQL renders — so no Go-side
// sort is reachable here.
func readyWindowForFilter(filter types.WorkFilter) searchWindow {
	return searchWindowFor(filter.Limit, filter.Offset, filter.MaxRows, filter.MaxRowsSource, false)
}

// finishWindow is the Go half of a window: the defensive cap against what the
// query matched, then the offset the engine was not given, then the page trim
// that produces the has-more verdict. The cap runs first because it counts rows
// the query MATCHED, skipped ones included — the same count the store-backed
// seam checks, which skips nothing because its body does.
func finishWindow[T any](rows []T, w searchWindow) ([]T, bool, error) {
	if err := issueops.EnforceMaxRowsCap(len(rows), w.rowCap, w.capSource); err != nil {
		return nil, false, err
	}
	items, hasMore := applyN1Overflow(dropFirst(rows, w.skip), w.limit)
	return items, hasMore, nil
}

func dropFirst[T any](rows []T, n int) []T {
	if n <= 0 {
		return rows
	}
	if n >= len(rows) {
		return rows[:0]
	}
	return rows[n:]
}

func applyN1Overflow[T any](items []T, limit int) ([]T, bool) {
	if limit <= 0 || len(items) <= limit {
		return items, false
	}
	return items[:limit], true
}

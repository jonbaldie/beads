package ado

import (
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

const (
	// bootstrapTimeWindow is the maximum time difference for heuristic matching.
	bootstrapTimeWindow = 24 * time.Hour
)

// MatchResult describes the outcome of a bootstrap match attempt.
type MatchResult struct {
	Matched    bool   // Whether a match was found
	BeadsID    string // The matched beads issue ID (if matched)
	MatchType  string // How it matched: "external_ref", "source_system", "heuristic", ""
	Candidates int    // Number of heuristic candidates found (for warnings)
}

// BootstrapMatcher handles deduplication during first sync.
type BootstrapMatcher struct {
	Mapper         tracker.FieldMapper
	HeuristicMatch bool // Whether heuristic matching is enabled (--bootstrap-match)
}

// NewBootstrapMatcher creates a new matcher. Set heuristicMatch=true to enable
// title+type+time matching (opt-in via --bootstrap-match flag).
func NewBootstrapMatcher(mapper tracker.FieldMapper, heuristicMatch bool) *BootstrapMatcher {
	return &BootstrapMatcher{
		Mapper:         mapper,
		HeuristicMatch: heuristicMatch,
	}
}

// FindMatch searches local beads issues for one that matches the given ADO work item.
// It checks matching policies in priority order: external_ref → source_system → heuristic.
// localIssues should be the full set of beads issues to search against.
// adoItem is the incoming ADO TrackerIssue to find a match for.
func (m *BootstrapMatcher) FindMatch(adoItem *tracker.TrackerIssue, localIssues []*types.Issue) MatchResult {
	if issue, ok := findByExternalRef(localIssues, adoItem.URL); ok {
		return matchedResult(issue, "external_ref")
	}
	if issue, ok := findBySourceSystem(localIssues, adoItem.ID); ok {
		return matchedResult(issue, "source_system")
	}
	if m.HeuristicMatch {
		return heuristicMatch(m.Mapper, adoItem, localIssues, adoItem.Title)
	}
	return MatchResult{}
}

// BootstrapIndex holds pre-built lookup maps for O(1) matching.
type BootstrapIndex struct {
	// ExternalRefMap maps external_ref URL → issue for O(1) external ref lookups.
	ExternalRefMap map[string]*types.Issue
	// SourceSystemMap maps extracted source system ID → issue for O(1) source_system lookups.
	SourceSystemMap map[string]*types.Issue
	// TitleMap maps lowercase title → issues for O(1) heuristic candidate lookup.
	TitleMap map[string][]*types.Issue
}

// BuildBootstrapIndex pre-indexes a slice of issues for fast matching.
func BuildBootstrapIndex(issues []*types.Issue) *BootstrapIndex {
	idx := &BootstrapIndex{
		ExternalRefMap:  make(map[string]*types.Issue, len(issues)),
		SourceSystemMap: make(map[string]*types.Issue, len(issues)),
		TitleMap:        make(map[string][]*types.Issue, len(issues)),
	}
	for _, issue := range issues {
		if issue.ExternalRef != nil && *issue.ExternalRef != "" {
			idx.ExternalRefMap[*issue.ExternalRef] = issue
		}
		if id := extractIDFromSourceSystem(issue.SourceSystem); id != "" {
			idx.SourceSystemMap[id] = issue
		}
		key := strings.ToLower(issue.Title)
		idx.TitleMap[key] = append(idx.TitleMap[key], issue)
	}
	return idx
}

// FindMatchIndexed searches pre-indexed local issues for a match against the given ADO work item.
// Uses O(1) hash lookups instead of O(N) iteration.
func (m *BootstrapMatcher) FindMatchIndexed(adoItem *tracker.TrackerIssue, idx *BootstrapIndex) MatchResult {
	if idx == nil {
		return MatchResult{}
	}

	if issue, ok := idx.ExternalRefMap[adoItem.URL]; ok {
		return matchedResult(issue, "external_ref")
	}

	if issue, ok := idx.SourceSystemMap[adoItem.ID]; ok {
		return matchedResult(issue, "source_system")
	}

	if m.HeuristicMatch {
		key := strings.ToLower(adoItem.Title)
		return heuristicMatch(m.Mapper, adoItem, idx.TitleMap[key], "")
	}
	return MatchResult{}
}

func findByExternalRef(issues []*types.Issue, url string) (*types.Issue, bool) {
	for _, issue := range issues {
		if issue.ExternalRef != nil && *issue.ExternalRef == url {
			return issue, true
		}
	}
	return nil, false
}

func findBySourceSystem(issues []*types.Issue, adoID string) (*types.Issue, bool) {
	for _, issue := range issues {
		if id := extractIDFromSourceSystem(issue.SourceSystem); id != "" && id == adoID {
			return issue, true
		}
	}
	return nil, false
}

func matchedResult(issue *types.Issue, matchType string) MatchResult {
	return MatchResult{Matched: true, BeadsID: issue.ID, MatchType: matchType}
}

func heuristicMatch(mapper tracker.FieldMapper, adoItem *tracker.TrackerIssue, issues []*types.Issue, title string) MatchResult {
	adoBeadsType := mapper.TypeToBeads(adoItem.Type)
	candidates := make([]*types.Issue, 0)
	for _, issue := range issues {
		if title != "" && issue.Title != title {
			continue
		}
		if issue.IssueType != adoBeadsType || !withinBootstrapWindow(issue, adoItem) {
			continue
		}
		candidates = append(candidates, issue)
	}
	return heuristicResult(candidates)
}

func withinBootstrapWindow(issue *types.Issue, adoItem *tracker.TrackerIssue) bool {
	diff := issue.CreatedAt.Sub(adoItem.CreatedAt)
	if diff < 0 {
		diff = -diff
	}
	return diff <= bootstrapTimeWindow
}

func heuristicResult(candidates []*types.Issue) MatchResult {
	if len(candidates) == 1 {
		return MatchResult{Matched: true, BeadsID: candidates[0].ID, MatchType: "heuristic", Candidates: 1}
	}
	if len(candidates) > 1 {
		return MatchResult{Candidates: len(candidates)}
	}
	return MatchResult{}
}

// extractIDFromSourceSystem extracts the work item ID from a source_system string
// like "ado:https://dev.azure.com/org/proj/_workitems/edit/42" or "ado:42".
func extractIDFromSourceSystem(sourceSystem string) string {
	if !strings.HasPrefix(sourceSystem, "ado:") {
		return ""
	}
	value := sourceSystem[len("ado:"):]
	if value == "" {
		return ""
	}
	// If it's a URL, extract the last path segment.
	if strings.Contains(value, "/") {
		idx := strings.LastIndex(value, "/")
		value = value[idx+1:]
	}
	return value
}

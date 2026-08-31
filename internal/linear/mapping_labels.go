package linear

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

// LabelCache maps normalized Linear label names (lowercase) to Linear label IDs
// for a single team.
type LabelCache struct {
	IDByLowerName map[string]string
}

// BuildLabelCache fetches team labels and indexes them by lowercase trimmed name.
func BuildLabelCache(ctx context.Context, client *Client) (*LabelCache, error) {
	if client == nil {
		return nil, fmt.Errorf("no linear client")
	}
	labels, err := client.GetTeamLabels(ctx)
	if err != nil {
		return nil, err
	}
	c := &LabelCache{
		IDByLowerName: make(map[string]string, len(labels)),
	}
	for _, lb := range labels {
		k := strings.ToLower(strings.TrimSpace(lb.Name))
		if k != "" && lb.ID != "" {
			c.IDByLowerName[k] = lb.ID
		}
	}
	return c, nil
}

// IssueTypeToLinearLabelLookupKey returns the label_type_map key (lowercase) for
// the given beads issue type, inverting linear.label_type_map.<label>=<type>.
// When multiple labels map to the same type, the smallest key lexicographically wins.
func IssueTypeToLinearLabelLookupKey(issueType types.IssueType, config *MappingConfig) string {
	if config == nil || len(config.LabelTypeMap) == 0 {
		return ""
	}
	want := strings.ToLower(strings.TrimSpace(string(issueType)))
	if want == "" {
		return ""
	}
	var keys []string
	for labelKey, typeStr := range config.LabelTypeMap {
		if strings.ToLower(strings.TrimSpace(typeStr)) == want {
			keys = append(keys, labelKey)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return keys[0]
}

// ResolveLabelIDs maps beads issue_type (via inverted label_type_map) and
// issue.Labels to Linear label UUIDs. Names not present on the team are returned
// in missing (deduplicated by display string order of discovery).
func ResolveLabelIDs(issue *types.Issue, cache *LabelCache, config *MappingConfig) (ids []string, missing []string) {
	if issue == nil || cache == nil || len(cache.IDByLowerName) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{})

	tryAdd := func(lowerName, display string) {
		if lowerName == "" {
			return
		}
		id, ok := cache.IDByLowerName[lowerName]
		if !ok {
			missing = append(missing, display)
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	if lk := IssueTypeToLinearLabelLookupKey(issue.IssueType, config); lk != "" {
		tryAdd(lk, lk)
	}
	for _, raw := range issue.Labels {
		ln := strings.ToLower(strings.TrimSpace(raw))
		tryAdd(ln, raw)
	}
	return ids, missing
}

func linearIssueLabelIDs(remote *Issue) []string {
	if remote == nil || remote.Labels == nil {
		return nil
	}
	out := make([]string, 0, len(remote.Labels.Nodes))
	for _, n := range remote.Labels.Nodes {
		if n.ID != "" {
			out = append(out, n.ID)
		}
	}
	return out
}

// PushFieldsEqual compares only the fields that a Linear push can actually
// mutate. This avoids repeated updates caused by local-only fields such as
// issue type and metadata. When labelCache is non-nil, resolved Linear label ID
// sets are compared so label drift is detected; when nil, labels are ignored
// (callers that can build a LabelCache should pass it for accurate skip logic).
func PushFieldsEqual(local *types.Issue, remote *Issue, config *MappingConfig, labelCache *LabelCache) bool {
	if local == nil || remote == nil {
		return false
	}
	if local.Title != remote.Title {
		return false
	}
	if BuildLinearDescription(local) != remote.Description {
		return false
	}
	if PriorityToLinear(local.Priority, config) != remote.Priority {
		return false
	}
	if StateToBeadsStatus(remote.State, config) != local.Status {
		return false
	}
	if labelCache != nil {
		wantIDs, _ := ResolveLabelIDs(local, labelCache, config)
		remoteIDs := linearIssueLabelIDs(remote)
		slices.Sort(wantIDs)
		slices.Sort(remoteIDs)
		if !slices.Equal(wantIDs, remoteIDs) {
			return false
		}
	}
	return true
}

// PushFieldsEqualToBeads is a fallback comparator for cases where Linear's raw
// payload is unavailable and only the normalized beads form remains.
func PushFieldsEqualToBeads(local, remote *types.Issue) bool {
	if local == nil || remote == nil {
		return false
	}
	if local.Title != remote.Title {
		return false
	}
	if BuildLinearDescription(local) != remote.Description {
		return false
	}
	if local.Priority != remote.Priority {
		return false
	}
	return local.Status == remote.Status
}

// LabelToIssueType infers issue type from label names.
// Uses configurable mapping from linear.label_type_map.* config.
func LabelToIssueType(labels *Labels, config *MappingConfig) types.IssueType {
	if labels == nil {
		return types.TypeTask
	}

	for _, label := range labels.Nodes {
		labelName := strings.ToLower(label.Name)

		// Check exact match first
		if issueType, ok := config.LabelTypeMap[labelName]; ok {
			return ParseIssueType(issueType)
		}

		// Check if label contains any broad legacy keyword. New issue types are
		// exact-only to avoid labels like "history" being inferred as "story".
		for keyword, issueType := range config.LabelTypeMap {
			if !allowsSubstringLabelType(keyword, issueType) {
				continue
			}
			if strings.Contains(labelName, keyword) {
				return ParseIssueType(issueType)
			}
		}
	}

	return types.TypeTask // Default
}

func allowsSubstringLabelType(keyword, issueType string) bool {
	switch keyword {
	case "decision", "spike", "story", "milestone":
		return false
	}

	switch ParseIssueType(issueType) {
	case types.TypeDecision, types.TypeSpike, types.TypeStory, types.TypeMilestone:
		return false
	default:
		return true
	}
}

// ParseIssueType converts an issue type string to types.IssueType.
func ParseIssueType(s string) types.IssueType {
	normalized := strings.ToLower(s)
	for _, mapping := range []struct {
		name  string
		value types.IssueType
	}{
		{"bug", types.TypeBug},
		{"feature", types.TypeFeature},
		{"task", types.TypeTask},
		{"epic", types.TypeEpic},
		{"chore", types.TypeChore},
		{"decision", types.TypeDecision},
		{"spike", types.TypeSpike},
		{"story", types.TypeStory},
		{"milestone", types.TypeMilestone},
	} {
		if normalized == mapping.name {
			return mapping.value
		}
	}
	return types.TypeTask
}

// RelationToBeadsDep converts a Linear relation to a Beads dependency type.
// Uses configurable mapping from linear.relation_map.* config.
func RelationToBeadsDep(relationType string, config *MappingConfig) string {
	if depType, ok := config.RelationMap[relationType]; ok {
		return depType
	}
	return "related" // Default fallback
}

// IssueToBeads converts a Linear issue to a Beads issue.
func IssueToBeads(li *Issue, config *MappingConfig) *IssueConversion {
	issue := buildBeadsIssue(li, config)
	return &IssueConversion{
		Issue:        issue,
		Dependencies: linearIssueDependencies(li, config),
	}
}

func buildBeadsIssue(li *Issue, config *MappingConfig) *types.Issue {
	issue := &types.Issue{
		IssueContent: types.IssueContent{
			Title:       li.Title,
			Description: li.Description,
		},
		IssueWorkflow: types.IssueWorkflow{
			Priority:  PriorityToBeads(li.Priority, config),
			IssueType: LabelToIssueType(li.Labels, config),
			Status:    StateToBeadsStatus(li.State, config),
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: parseLinearTimestamp(li.CreatedAt),
			UpdatedAt: parseLinearTimestamp(li.UpdatedAt),
		},
		IssueMeta: types.IssueMeta{
			ExternalRef: canonicalLinearExternalRef(li.URL),
		},
		IssueGraph: types.IssueGraph{
			Labels: linearIssueLabels(li.Labels),
		},
	}
	if completedAt := parseOptionalLinearTimestamp(li.CompletedAt); completedAt != nil {
		issue.ClosedAt = completedAt
	}
	if li.Assignee != nil {
		issue.Assignee = linearAssigneeName(li.Assignee)
	}
	return issue
}

func parseLinearTimestamp(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Now()
	}
	return parsed
}

func parseOptionalLinearTimestamp(value string) *time.Time {
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil
	}
	return &parsed
}

func linearAssigneeName(assignee *User) string {
	if assignee.Email != "" {
		return assignee.Email
	}
	return assignee.Name
}

func linearIssueLabels(labels *Labels) []string {
	if labels == nil || len(labels.Nodes) == 0 {
		return nil
	}
	result := make([]string, 0, len(labels.Nodes))
	for _, label := range labels.Nodes {
		result = append(result, label.Name)
	}
	return result
}

func canonicalLinearExternalRef(externalRef string) *string {
	if canonical, ok := CanonicalizeLinearExternalRef(externalRef); ok {
		externalRef = canonical
	}
	return &externalRef
}

func linearIssueDependencies(li *Issue, config *MappingConfig) []DependencyInfo {
	var deps []DependencyInfo
	if li.Parent != nil {
		deps = append(deps, DependencyInfo{
			FromLinearID: li.Identifier,
			ToLinearID:   li.Parent.Identifier,
			Type:         "parent-child",
			Source:       DependencySourceParent,
		})
	}
	if li.Relations != nil {
		for _, rel := range li.Relations.Nodes {
			deps = append(deps, linearRelationDependency(li, rel, config))
		}
	}
	return deps
}

func linearRelationDependency(li *Issue, rel Relation, config *MappingConfig) DependencyInfo {
	dep := DependencyInfo{
		Type:   RelationToBeadsDep(rel.Type, config),
		Source: DependencySourceRelation,
	}
	switch rel.Type {
	case "blockedBy":
		dep.FromLinearID = li.Identifier
		dep.ToLinearID = rel.RelatedIssue.Identifier
	case "blocks":
		dep.FromLinearID = rel.RelatedIssue.Identifier
		dep.ToLinearID = li.Identifier
	default:
		dep.FromLinearID = li.Identifier
		dep.ToLinearID = rel.RelatedIssue.Identifier
	}
	return dep
}

// BuildLinearToLocalUpdates creates an updates map from a Linear issue
// to apply to a local Beads issue. This is used when Linear wins a conflict.
func BuildLinearToLocalUpdates(li *Issue, config *MappingConfig) map[string]interface{} {
	updates := make(map[string]interface{})

	// Update title
	updates["title"] = li.Title

	// Update description
	updates["description"] = li.Description

	// Update priority using configured mapping
	updates["priority"] = PriorityToBeads(li.Priority, config)

	// Update status using configured mapping
	updates["status"] = string(StateToBeadsStatus(li.State, config))

	// Update assignee if present
	if li.Assignee != nil {
		if li.Assignee.Email != "" {
			updates["assignee"] = li.Assignee.Email
		} else {
			updates["assignee"] = li.Assignee.Name
		}
	} else {
		updates["assignee"] = ""
	}

	// Update labels from Linear
	if li.Labels != nil {
		var labels []string
		for _, label := range li.Labels.Nodes {
			labels = append(labels, label.Name)
		}
		updates["labels"] = labels
	}

	// Update timestamps
	if li.UpdatedAt != "" {
		if updatedAt, err := time.Parse(time.RFC3339, li.UpdatedAt); err == nil {
			updates["updated_at"] = updatedAt
		}
	}

	// Handle closed state
	if li.CompletedAt != "" {
		if closedAt, err := time.Parse(time.RFC3339, li.CompletedAt); err == nil {
			updates["closed_at"] = closedAt
		}
	}

	return updates
}

// ProjectToEpic converts a Linear Project to a Beads Epic issue.
func ProjectToEpic(lp *Project) *types.Issue {
	issue := &types.Issue{
		IssueContent: types.IssueContent{
			Title:       lp.Name,
			Description: lp.Description,
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    projectStateToBeadsStatus(lp.State),
			IssueType: types.TypeEpic,
			Priority:  2,
		},
		IssueTimes: types.IssueTimes{
			// Default medium priority
			CreatedAt: parseLinearTimestamp(lp.CreatedAt),
			UpdatedAt: parseLinearTimestamp(lp.UpdatedAt),
		},
		IssueMeta: types.IssueMeta{
			ExternalRef: canonicalLinearExternalRef(lp.URL),
		},
	}

	if completedAt := parseOptionalLinearTimestamp(lp.CompletedAt); completedAt != nil {
		issue.ClosedAt = completedAt
	}

	if lp.State == "canceled" {
		issue.CloseReason = "canceled"
	}

	return issue
}

func projectStateToBeadsStatus(state string) types.Status {
	switch state {
	case "completed", "canceled":
		return types.StatusClosed
	case "started", "paused":
		return types.StatusInProgress
	default:
		return types.StatusOpen // planned, or unknown
	}
}

// MapEpicToProjectState maps a Beads status to Linear project state.
func MapEpicToProjectState(status types.Status) string {
	switch status {
	case types.StatusClosed:
		return "completed"
	case types.StatusInProgress:
		return "started"
	default:
		return "planned"
	}
}

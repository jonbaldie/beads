package ado

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
)

// IssueToBeads converts an ADO WorkItem (via TrackerIssue) to a beads Issue.
// Returns nil if the TrackerIssue's Raw field is not a *WorkItem.
func (m *adoFieldMapper) IssueToBeads(ti *tracker.TrackerIssue) *tracker.IssueConversion {
	if ti == nil {
		return nil
	}
	wi, ok := ti.Raw.(*WorkItem)
	if !ok || wi == nil {
		return nil
	}

	issue := m.issueFromWorkItem(wi)
	restoreADOPriority(issue, ti.Metadata)

	// Build external ref URL.
	ref := buildExternalRef(wi)
	if ref != "" {
		issue.ExternalRef = &ref
	}

	applyADOMetadataToIssue(issue, wi, ti.Metadata)

	return &tracker.IssueConversion{Issue: issue, Dependencies: ExtractLinkDeps(wi)}
}

func (m *adoFieldMapper) issueFromWorkItem(wi *WorkItem) *types.Issue {
	desc, _ := HTMLToMarkdown(wi.GetStringField(FieldDescription))
	issue := &types.Issue{
		IssueContent: types.IssueContent{Title: wi.GetStringField(FieldTitle), Description: desc},
		IssueWorkflow: types.IssueWorkflow{
			Priority: m.PriorityToBeads(wi.GetField(FieldPriority)), Status: m.StatusToBeads(wi.GetField(FieldState)),
			IssueType: m.TypeToBeads(wi.GetField(FieldWorkItemType)), Owner: extractAssignedTo(wi.GetField(FieldAssignedTo)),
		},
		IssueGraph: types.IssueGraph{Labels: filterBeadsTags(parseTags(wi.GetStringField(FieldTags)))},
	}
	if issue.Status == types.StatusInProgress && hasBeadsTag(wi.GetStringField(FieldTags), "beads:blocked") {
		issue.Status = types.StatusBlocked
	}
	return issue
}

func restoreADOPriority(issue *types.Issue, metadata map[string]interface{}) {
	if metadata == nil {
		return
	}
	value, ok := metadata["beads_priority"]
	if !ok {
		return
	}
	p, ok := adoPriorityValue(value)
	if ok && p >= 0 && p <= 4 {
		issue.Priority = p
	}
}

func adoPriorityValue(value interface{}) (int, bool) {
	switch v := value.(type) {
	case string:
		n, err := strconv.Atoi(v)
		return n, err == nil
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}

func applyADOMetadataToIssue(issue *types.Issue, wi *WorkItem, trackerMetadata map[string]interface{}) {
	meta := buildMetadata(wi)
	if trackerMetadata != nil {
		if bp, ok := trackerMetadata["beads_priority"]; ok {
			meta["beads_priority"] = bp
		}
	}
	if len(meta) == 0 {
		return
	}
	raw, err := json.Marshal(meta)
	if err == nil {
		issue.Metadata = json.RawMessage(raw)
	}
}

// IssueToTracker converts a beads Issue to a map of ADO work item field values.
func (m *adoFieldMapper) IssueToTracker(issue *types.Issue) map[string]interface{} {
	fields := map[string]interface{}{
		FieldTitle:    issue.Title,
		FieldState:    m.StatusToTracker(issue.Status),
		FieldPriority: m.PriorityToTracker(issue.Priority),
	}

	addADODescription(fields, issue.Description)
	addADOTags(fields, issue)
	storeADOPriority(issue)
	addADOSeverity(fields, m, issue)
	restoreMetadata(issue, fields)
	return fields
}

func addADODescription(fields map[string]interface{}, description string) {
	if description == "" {
		return
	}
	htmlDesc, err := MarkdownToHTML(description)
	if err == nil && htmlDesc != "" {
		fields[FieldDescription] = htmlDesc
	}
}

func addADOTags(fields map[string]interface{}, issue *types.Issue) {
	tags := append([]string{}, issue.Labels...)
	if issue.Status == types.StatusBlocked {
		tags = append(tags, "beads:blocked")
	}
	if len(tags) > 0 {
		fields[FieldTags] = buildTagString(tags)
	}
}

func storeADOPriority(issue *types.Issue) {
	if issue.Priority != 3 && issue.Priority != 4 {
		return
	}
	var meta map[string]interface{}
	if len(issue.Metadata) > 0 {
		_ = json.Unmarshal(issue.Metadata, &meta)
	}
	if meta == nil {
		meta = make(map[string]interface{})
	}
	meta["beads_priority"] = strconv.Itoa(issue.Priority)
	if raw, err := json.Marshal(meta); err == nil {
		issue.Metadata = json.RawMessage(raw)
	}
}

func addADOSeverity(fields map[string]interface{}, m *adoFieldMapper, issue *types.Issue) {
	typeName, _ := m.TypeToTracker(issue.IssueType).(string)
	if strings.EqualFold(typeName, "Bug") {
		fields[FieldSeverity] = m.SeverityForBug(issue.Priority)
	}
}

// extractAssignedTo extracts the display name from an ADO AssignedTo field.
// The field may be a simple string or an identity object with a displayName key.
func extractAssignedTo(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if m, ok := v.(map[string]interface{}); ok {
		if name, ok := m["displayName"].(string); ok {
			return name
		}
	}
	return ""
}

// buildExternalRef constructs the ADO web URL for a work item.
// Falls back to the API URL if org/project cannot be determined.
func buildExternalRef(wi *WorkItem) string {
	if wi.URL == "" {
		return ""
	}
	// ADO API URL format: https://dev.azure.com/{org}/{project}/_apis/wit/workItems/{id}
	// Web URL format:     https://dev.azure.com/{org}/{project}/_workitems/edit/{id}
	if idx := strings.Index(wi.URL, "/_apis/"); idx > 0 {
		return fmt.Sprintf("%s/_workitems/edit/%d", wi.URL[:idx], wi.ID)
	}
	return wi.URL
}

// buildMetadata extracts ADO-specific fields into a metadata map.
func buildMetadata(wi *WorkItem) map[string]interface{} {
	meta := make(map[string]interface{})

	if v := wi.GetStringField(FieldAreaPath); v != "" {
		meta["ado.area_path"] = v
	}
	if v := wi.GetStringField(FieldIterationPath); v != "" {
		meta["ado.iteration_path"] = v
	}
	if v := wi.GetField(FieldStoryPoints); v != nil {
		meta["ado.story_points"] = v
	}
	if v := wi.GetField(FieldRemainingWork); v != nil {
		meta["ado.remaining_work"] = v
	}
	if v := wi.GetStringField(FieldSeverity); v != "" {
		meta["ado.severity"] = v
	}
	if wi.Rev > 0 {
		meta["ado.rev"] = wi.Rev
	}

	return meta
}

// restoreMetadata copies ADO-specific fields from issue metadata back into the field map.
func restoreMetadata(issue *types.Issue, fields map[string]interface{}) {
	if len(issue.Metadata) == 0 {
		return
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(issue.Metadata, &meta); err != nil {
		return
	}
	if v, ok := meta["ado.area_path"]; ok {
		fields[FieldAreaPath] = v
	}
	if v, ok := meta["ado.iteration_path"]; ok {
		fields[FieldIterationPath] = v
	}
	if v, ok := meta["ado.story_points"]; ok {
		fields[FieldStoryPoints] = v
	}
	if v, ok := meta["ado.severity"]; ok {
		fields[FieldSeverity] = v
	}
}

// parseTags splits an ADO semicolon-separated tag string into a trimmed slice.
// Returns nil for empty input.
func parseTags(tagStr string) []string {
	if strings.TrimSpace(tagStr) == "" {
		return nil
	}
	parts := strings.Split(tagStr, ";")
	var tags []string
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// buildTagString joins tags with "; " separator (ADO convention).
func buildTagString(tags []string) string {
	return strings.Join(tags, "; ")
}

// filterBeadsTags removes internal beads:* tags from a tag slice.
func filterBeadsTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		if !strings.HasPrefix(t, "beads:") {
			out = append(out, t)
		}
	}
	return out
}

// hasBeadsTag checks if a specific beads:* tag is present in an ADO tag string.
func hasBeadsTag(tagStr, tag string) bool {
	for _, t := range parseTags(tagStr) {
		if t == tag {
			return true
		}
	}
	return false
}

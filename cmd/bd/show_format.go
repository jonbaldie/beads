package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/uimd"
)

// formatShortIssue returns a compact one-line representation of an issue
// Format: STATUS_ICON ID PRIORITY [Type] Title
func formatShortIssue(issue *types.Issue) string {
	statusIcon := ui.RenderStatusIcon(string(issue.Status))
	priorityTag := ui.RenderPriority(issue.Priority)

	// Type badge only for notable types
	typeBadge := ""
	switch issue.IssueType {
	case "epic":
		typeBadge = ui.TypeEpicStyle().Render("[epic]") + " "
	case "bug":
		typeBadge = ui.TypeBugStyle().Render("[bug]") + " "
	}

	// Closed issues: entire line is muted
	if issue.Status == types.StatusClosed {
		return fmt.Sprintf("%s %s %s %s%s",
			statusIcon,
			ui.RenderMuted(issue.ID),
			ui.RenderMuted(fmt.Sprintf("P%d", issue.Priority)),
			ui.RenderMuted(string(issue.IssueType)),
			ui.RenderMuted(" "+issue.Title))
	}

	return fmt.Sprintf("%s %s %s %s%s", statusIcon, issue.ID, priorityTag, typeBadge, issue.Title)
}

// formatIssueHeader returns the Tufte-aligned header line
// Format: ID · Title   [Priority · STATUS]
// All elements in bd show get semantic colors since focus is on one issue
func formatIssueHeader(issue *types.Issue) string {
	// Get status icon and style
	statusIcon := ui.RenderStatusIcon(string(issue.Status))
	statusStyle := ui.GetStatusStyle(string(issue.Status))
	statusStr := statusStyle.Render(strings.ToUpper(string(issue.Status)))

	// Priority with semantic color (P-label only)
	priorityTag := ui.RenderPriority(issue.Priority)

	// Type badge for notable types
	typeBadge := ""
	switch issue.IssueType {
	case "epic":
		typeBadge = " " + ui.TypeEpicStyle().Render("[EPIC]")
	case "bug":
		typeBadge = " " + ui.TypeBugStyle().Render("[BUG]")
	}

	// Compaction indicator
	tierEmoji := ""
	switch issue.CompactionLevel {
	case 1:
		tierEmoji = " 🗜️"
	case 2:
		tierEmoji = " 📦"
	}

	// Build header: STATUS_ICON ID · Title   [Priority · STATUS]
	idStyled := ui.RenderAccent(issue.ID)
	return fmt.Sprintf("%s %s%s · %s%s   [%s · %s]",
		statusIcon, idStyled, typeBadge, issue.Title, tierEmoji, priorityTag, statusStr)
}

// formatIssueMetadata returns the metadata line(s) with grouped info
// Format: Owner: user · Type: task
//
//	Created: 2026-01-06 · Updated: 2026-01-08
func formatIssueMetadata(issue *types.Issue) string {
	lines := issueMetadataIdentityLines(issue)
	lines = append(lines, issueMetadataTimeLines(issue)...)
	lines = append(lines, issueMetadataLeaseLines(issue)...)
	lines, closeReasonSection := appendIssueCloseReason(lines, issue)
	lines = append(lines, issueMetadataRefLines(issue)...)
	return strings.Join(lines, "\n") + closeReasonSection
}

func issueMetadataIdentityLines(issue *types.Issue) []string {
	metaParts := []string{}
	if issue.CreatedBy != "" {
		metaParts = append(metaParts, fmt.Sprintf("Owner: %s", issue.CreatedBy))
	}
	if issue.Assignee != "" {
		metaParts = append(metaParts, fmt.Sprintf("Assignee: %s", issue.Assignee))
	}
	typeStr := string(issue.IssueType)
	switch issue.IssueType {
	case "epic":
		typeStr = ui.TypeEpicStyle().Render("epic")
	case "bug":
		typeStr = ui.TypeBugStyle().Render("bug")
	}
	metaParts = append(metaParts, fmt.Sprintf("Type: %s", typeStr))
	if len(metaParts) == 0 {
		return nil
	}
	return []string{strings.Join(metaParts, " · ")}
}

func issueMetadataTimeLines(issue *types.Issue) []string {
	timeParts := []string{
		fmt.Sprintf("Created: %s", issue.CreatedAt.Format("2006-01-02")),
	}
	if issue.StartedAt != nil {
		timeParts = append(timeParts, fmt.Sprintf("Started: %s", issue.StartedAt.Format("2006-01-02")))
	}
	timeParts = append(timeParts, fmt.Sprintf("Updated: %s", issue.UpdatedAt.Format("2006-01-02")))
	if issue.DueAt != nil {
		timeParts = append(timeParts, fmt.Sprintf("Due: %s", issue.DueAt.Local().Format("2006-01-02")))
	}
	if issue.DeferUntil != nil {
		timeParts = append(timeParts, fmt.Sprintf("Deferred: %s", issue.DeferUntil.Local().Format("2006-01-02")))
	}
	return []string{strings.Join(timeParts, " · ")}
}

func issueMetadataLeaseLines(issue *types.Issue) []string {
	// Lease line: only when an active lease is held (in_progress + non-null
	// lease_expires_at). row_lock is internal and never surfaced.
	if issue.Status != types.StatusInProgress || issue.LeaseExpiresAt == nil {
		return nil
	}
	leaseLine := fmt.Sprintf("Lease: expires %s", formatTimeUntil(*issue.LeaseExpiresAt))
	if issue.HeartbeatAt != nil {
		leaseLine += fmt.Sprintf(" (heartbeat %s)", formatTimeAgo(*issue.HeartbeatAt))
	}
	leaseLine += issueLeaseReplicaSuffix(issue)
	return []string{ui.RenderMuted(leaseLine)}
}

func issueLeaseReplicaSuffix(issue *types.Issue) string {
	// Granting replica: only worth a reader's attention when it is NOT
	// this node, since that is the case where the lease is unenforceable
	// here and bd reclaim will decline it. Unknown provenance ("") stays
	// silent — it is the pre-wy-jpd3.7 shape, not a fact about the lease.
	// The local node being unnamed silences it too: the guard is disarmed
	// there, so reclaim will NOT decline the lease and claiming otherwise
	// would be a promise the reaper does not keep.
	local := config.NodeID()
	if local == "" {
		return ""
	}
	node := issue.LeaseGrantedNode
	if node == "" || node == local {
		return ""
	}
	return fmt.Sprintf(" — granted by replica %s", node)
}

func appendIssueCloseReason(lines []string, issue *types.Issue) ([]string, string) {
	// Line 3: Close reason (if closed). A reason too long or too structured to
	// sit on a metadata line is body text, not metadata, so it gets the same
	// markdown section treatment as DESCRIPTION — `bd close --reason-file`
	// exists to write exactly that. Emitted after the remaining metadata lines
	// so a multi-line reason cannot split the block it is part of.
	//
	// Trimmed first: --reason-file and heredoc content virtually always ends
	// in a newline, and without this a genuine one-liner would be promoted to
	// a section on that byte alone.
	if issue.Status != types.StatusClosed {
		return lines, ""
	}
	reason := strings.TrimSpace(issue.CloseReason)
	if reason == "" {
		return lines, ""
	}
	line := "Close reason: " + reason
	if fitsMetadataLine(line) {
		return append(lines, ui.RenderMuted(line)), ""
	}
	section := fmt.Sprintf("\n\n%s\n%s", ui.RenderBold("CLOSE REASON"),
		strings.TrimRight(uimd.RenderMarkdown(reason), "\n"))
	return lines, section
}

func issueMetadataRefLines(issue *types.Issue) []string {
	var lines []string
	if issue.ExternalRef != nil && *issue.ExternalRef != "" {
		lines = append(lines, fmt.Sprintf("External: %s", *issue.ExternalRef))
	}
	if issue.SpecID != "" {
		lines = append(lines, fmt.Sprintf("Spec: %s", issue.SpecID))
	}
	if issue.Ephemeral && issue.WispType != "" {
		lines = append(lines, fmt.Sprintf("Wisp type: %s", ui.RenderMuted(string(issue.WispType))))
	}
	// Compaction savings. A metadata line rather than a callout the
	// callers print after the block, so a promoted close reason cannot strand
	// it below the body text, and so all five show paths report it alike.
	if line := compactionSavingsLine(issue); line != "" {
		lines = append(lines, line)
	}
	return lines
}

// compactionSavingsLine reports how much a compacted issue's stored body shrank.
// Returns "" when the issue was never compacted, or when compaction recorded no
// saving worth reporting.
func compactionSavingsLine(issue *types.Issue) string {
	if issue.CompactionLevel == 0 || issue.OriginalSize <= 0 {
		return ""
	}
	currentSize := len(issue.Description) + len(issue.Design) + len(issue.Notes) + len(issue.AcceptanceCriteria)
	saved := issue.OriginalSize - currentSize
	if saved <= 0 {
		return ""
	}
	reduction := float64(saved) / float64(issue.OriginalSize) * 100
	return fmt.Sprintf("📊 %d → %d bytes (%.0f%% reduction)", issue.OriginalSize, currentSize, reduction)
}

// fitsMetadataLine reports whether a value can occupy one metadata line without
// the terminal breaking it for us. Anything that cannot belongs in a rendered
// section, where it gets the wrapping and indentation body text gets.
func fitsMetadataLine(s string) bool {
	if strings.ContainsAny(s, "\n\r") {
		return false
	}
	width := uimd.WrapWidth()
	return width == 0 || ansi.StringWidth(s) <= width
}

// formatDependencyLine formats a single dependency with semantic colors
// Closed items get entire row muted - the work is done, no need for attention
func formatDependencyLine(prefix string, dep *types.IssueWithDependencyMetadata) string {
	// Status icon (always rendered with semantic color)
	statusIcon := ui.GetStatusIcon(string(dep.Status))

	// Closed items: mute entire row since the work is complete
	if dep.Status == types.StatusClosed {
		return fmt.Sprintf("  %s %s %s: %s %s",
			prefix, statusIcon,
			ui.RenderMuted(dep.ID),
			ui.RenderMuted(dep.Title),
			ui.RenderMuted(fmt.Sprintf("P%d", dep.Priority)))
	}

	// Active items: ID with status color, priority with semantic color
	style := ui.GetStatusStyle(string(dep.Status))
	idStr := style.Render(dep.ID)
	priorityTag := ui.RenderPriority(dep.Priority)

	// Type indicator for epics/bugs
	typeStr := ""
	if dep.IssueType == "epic" {
		typeStr = ui.TypeEpicStyle().Render("(EPIC)") + " "
	} else if dep.IssueType == "bug" {
		typeStr = ui.TypeBugStyle().Render("(BUG)") + " "
	}

	return fmt.Sprintf("  %s %s %s: %s%s %s", prefix, statusIcon, idStr, typeStr, dep.Title, priorityTag)
}

// printDepSection prints one dependency section: bold heading, then a line per
// edge marked with the section's direction glyph.
func printDepSection(sec depSection) {
	fmt.Printf("\n%s\n", ui.RenderBold(sec.Heading))
	for _, dep := range sec.Deps {
		fmt.Println(formatDependencyLine(sec.Glyph, dep))
	}
}

// printRelatedSection prints the deduplicated RELATED section that both
// directions of the symmetric related/relates-to edges collapse into.
func printRelatedSection(relatedSeen map[string]*types.IssueWithDependencyMetadata) {
	if len(relatedSeen) == 0 {
		return
	}
	fmt.Printf("\n%s\n", ui.RenderBold("RELATED"))
	ids := make([]string, 0, len(relatedSeen))
	for id := range relatedSeen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Println(formatDependencyLine("↔", relatedSeen[id]))
	}
}

// printEpicChildProgress summarizes how much of an epic its children have
// finished, printed under the CHILDREN section.
func printEpicChildProgress(children []*types.IssueWithDependencyMetadata) {
	if len(children) == 0 {
		return
	}
	closed := 0
	for _, dep := range children {
		if dep.Status == types.StatusClosed {
			closed++
		}
	}
	pct := closed * 100 / len(children)
	if closed == len(children) {
		fmt.Printf("  %s %d/%d complete (%d%%) — eligible for close\n", ui.RenderPass("✓"), closed, len(children), pct)
	} else {
		fmt.Printf("  %s %d/%d complete (%d%%)\n", ui.RenderMuted("◐"), closed, len(children), pct)
	}
}

// formatSimpleDependencyLine formats a dependency without metadata (fallback)
// Closed items get entire row muted - the work is done, no need for attention
func formatSimpleDependencyLine(prefix string, dep *types.Issue) string {
	statusIcon := ui.GetStatusIcon(string(dep.Status))

	// Closed items: mute entire row since the work is complete
	if dep.Status == types.StatusClosed {
		return fmt.Sprintf("  %s %s %s: %s %s",
			prefix, statusIcon,
			ui.RenderMuted(dep.ID),
			ui.RenderMuted(dep.Title),
			ui.RenderMuted(fmt.Sprintf("P%d", dep.Priority)))
	}

	// Active items: use semantic colors
	style := ui.GetStatusStyle(string(dep.Status))
	idStr := style.Render(dep.ID)
	priorityTag := ui.RenderPriority(dep.Priority)

	return fmt.Sprintf("  %s %s %s: %s %s", prefix, statusIcon, idStr, dep.Title, priorityTag)
}

// formatIssueCustomMetadata renders the issue's custom JSON metadata field
// for bd show output. Returns empty string if no metadata is set.
// Top-level keys are displayed sorted alphabetically, one per line.
// Scalar values are shown inline; objects/arrays are shown as compact JSON.
func formatIssueCustomMetadata(issue *types.Issue) string {
	if len(issue.Metadata) == 0 {
		return ""
	}
	// Treat empty object as "no metadata"
	trimmed := strings.TrimSpace(string(issue.Metadata))
	if trimmed == "{}" || trimmed == "null" {
		return ""
	}

	var data map[string]any
	if err := json.Unmarshal(issue.Metadata, &data); err != nil {
		// Not a JSON object — show raw value
		return fmt.Sprintf("%s\n  %s", ui.RenderBold("METADATA"), trimmed)
	}
	if len(data) == 0 {
		return ""
	}

	// Sort keys for stable output
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var lines []string
	for _, k := range keys {
		v := data[k]
		lines = append(lines, fmt.Sprintf("  %s: %s", k, formatMetadataValue(v)))
	}

	return fmt.Sprintf("%s\n%s", ui.RenderBold("METADATA"), strings.Join(lines, "\n"))
}

// formatIssueLongExtras returns additional detail sections for --long mode.
// Only sections with data are included. Fields already shown in default mode are skipped.
func formatIssueLongExtras(issue *types.Issue, formatTime func(time.Time) string) string {
	sections := collectIssueLongExtraSections(issue, formatTime)
	if len(sections) == 0 {
		return ""
	}
	return "\n" + strings.Join(sections, "\n\n") + "\n"
}

func collectIssueLongExtraSections(issue *types.Issue, formatTime func(time.Time) string) []string {
	var sections []string
	sections = appendLabeledSection(sections, "EXTENDED DETAILS", issueExtendedDetailLines(issue, formatTime))
	sections = appendLabeledSection(sections, "COMPACTION", issueCompactionLines(issue, formatTime))
	sections = appendLabeledSection(sections, "GATE", issueGateLines(issue))
	sections = appendLabeledSection(sections, "SOURCE TRACING", issueSourceTracingLines(issue))
	sections = appendLabeledSection(sections, "BONDED FROM", issueBondedFromLines(issue))
	return appendLabeledSection(sections, "EVENT", issueEventLines(issue))
}

func appendLabeledSection(sections []string, title string, lines []string) []string {
	if len(lines) == 0 {
		return sections
	}
	return append(sections, fmt.Sprintf("%s\n%s", ui.RenderBold(title), strings.Join(lines, "\n")))
}

func issueExtendedDetailLines(issue *types.Issue, formatTime func(time.Time) string) []string {
	lines := issueClosureDetailLines(issue, formatTime)
	return append(lines, issueFlagDetailLines(issue)...)
}

func issueClosureDetailLines(issue *types.Issue, formatTime func(time.Time) string) []string {
	var lines []string
	if issue.ClosedAt != nil {
		lines = append(lines, fmt.Sprintf("  Closed at: %s", formatTime(*issue.ClosedAt)))
	}
	if issue.ClosedBySession != "" {
		lines = append(lines, fmt.Sprintf("  Closed by session: %s", issue.ClosedBySession))
	}
	if issue.EstimatedMinutes != nil {
		lines = append(lines, fmt.Sprintf("  Estimated: %d minutes", *issue.EstimatedMinutes))
	}
	if issue.SourceSystem != "" {
		lines = append(lines, fmt.Sprintf("  Source system: %s", issue.SourceSystem))
	}
	if issue.Sender != "" {
		lines = append(lines, fmt.Sprintf("  Sender: %s", issue.Sender))
	}
	return lines
}

func issueFlagDetailLines(issue *types.Issue) []string {
	var lines []string
	if issue.Ephemeral {
		lines = append(lines, "  Ephemeral: yes")
	}
	if issue.Pinned {
		lines = append(lines, "  Pinned: yes")
	}
	if issue.IsTemplate {
		lines = append(lines, "  Template: yes")
	}
	if issue.MolType != "" {
		lines = append(lines, fmt.Sprintf("  Mol type: %s", issue.MolType))
	}
	if issue.WorkType != "" {
		lines = append(lines, fmt.Sprintf("  Work type: %s", issue.WorkType))
	}
	return lines
}

func issueCompactionLines(issue *types.Issue, formatTime func(time.Time) string) []string {
	if issue.CompactionLevel <= 0 {
		return nil
	}
	lines := []string{fmt.Sprintf("  Level: %d", issue.CompactionLevel)}
	if issue.CompactedAt != nil {
		lines = append(lines, fmt.Sprintf("  Compacted at: %s", formatTime(*issue.CompactedAt)))
	}
	if issue.CompactedAtCommit != nil {
		lines = append(lines, fmt.Sprintf("  Compacted at commit: %s", *issue.CompactedAtCommit))
	}
	if issue.OriginalSize > 0 {
		lines = append(lines, fmt.Sprintf("  Original size: %d bytes", issue.OriginalSize))
	}
	return lines
}

func issueGateLines(issue *types.Issue) []string {
	var lines []string
	if issue.AwaitType != "" {
		lines = append(lines, fmt.Sprintf("  Await type: %s", issue.AwaitType))
	}
	if issue.AwaitID != "" {
		lines = append(lines, fmt.Sprintf("  Await ID: %s", issue.AwaitID))
	}
	if issue.Timeout > 0 {
		lines = append(lines, fmt.Sprintf("  Timeout: %s", issue.Timeout))
	}
	if len(issue.Waiters) > 0 {
		lines = append(lines, fmt.Sprintf("  Waiters: %s", strings.Join(issue.Waiters, ", ")))
	}
	return lines
}

func issueSourceTracingLines(issue *types.Issue) []string {
	var lines []string
	if issue.SourceFormula != "" {
		lines = append(lines, fmt.Sprintf("  Formula: %s", issue.SourceFormula))
	}
	if issue.SourceLocation != "" {
		lines = append(lines, fmt.Sprintf("  Location: %s", issue.SourceLocation))
	}
	return lines
}

func issueBondedFromLines(issue *types.Issue) []string {
	if len(issue.BondedFrom) == 0 {
		return nil
	}
	refs := make([]string, 0, len(issue.BondedFrom))
	for _, b := range issue.BondedFrom {
		refs = append(refs, fmt.Sprintf("  %s (%s)", b.SourceID, b.BondType))
	}
	return refs
}

func issueEventLines(issue *types.Issue) []string {
	var lines []string
	if issue.EventKind != "" {
		lines = append(lines, fmt.Sprintf("  Kind: %s", issue.EventKind))
	}
	if issue.Actor != "" {
		lines = append(lines, fmt.Sprintf("  Actor: %s", issue.Actor))
	}
	if issue.Target != "" {
		lines = append(lines, fmt.Sprintf("  Target: %s", issue.Target))
	}
	if issue.Payload != "" {
		lines = append(lines, fmt.Sprintf("  Payload: %s", issue.Payload))
	}
	return lines
}

// formatMetadataValue formats a single metadata value for display.
// Strings are shown unquoted, numbers/bools as-is, objects/arrays as compact JSON.
func formatMetadataValue(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case float64:
		// JSON numbers unmarshal as float64; show integers without decimal
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%g", val)
	case bool:
		return fmt.Sprintf("%t", val)
	case nil:
		return "null"
	default:
		// Arrays and nested objects — compact JSON
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}
}

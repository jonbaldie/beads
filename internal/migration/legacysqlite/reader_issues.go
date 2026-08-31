//go:build cgo

// Package legacysqlite reads the small, authenticated SQLite history that
// predates the current Dolt store. It intentionally has no general migration
// registry: each accepted layout is an exact, audited contract.
package legacysqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
	_ "github.com/mattn/go-sqlite3"
)

func loadIssues(ctx context.Context, db *sql.Tx) ([]*types.Issue, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+loadIssuesProjection+" FROM issues ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []*types.Issue
	for rows.Next() {
		var x legacyExtras
		issue, err := issueops.ScanIssueFrom(rows, x.scanDests()...)
		if err != nil {
			return nil, err
		}
		if err := x.validate(issue); err != nil {
			return nil, err
		}
		result = append(result, issue)
	}
	return result, rows.Err()
}

// scanDests returns the destinations for the legacy trailing columns that
// follow the canonical issueops.IssueSelectColumns prefix in
// loadIssuesProjection, in projection order. ScanIssueFrom appends these after
// the canonical scan destinations.
func (x *legacyExtras) scanDests() []any {
	return []any{
		&x.removed.closedBy, &x.removed.deletedAt, &x.removed.deletedBy, &x.removed.deleteReason, &x.removed.originalType,
		&x.removed.crystallizes, &x.removed.quality, &x.removed.hookBead, &x.removed.roleBead, &x.removed.agentState,
		&x.removed.lastActivity, &x.removed.roleType, &x.removed.rig, &x.current.metadata, &x.current.waiters,
		&x.current.ephemeral, &x.current.pinned, &x.current.template, &x.current.estimatedMinutes, &x.current.compactionLevel,
		&x.current.originalSize, &x.current.closedAt, &x.current.compactedAt, &x.current.dueAt, &x.current.deferUntil,
	}
}

type legacyRemovedFields struct {
	closedBy, deletedAt, deletedBy, deleteReason, originalType sql.NullString
	hookBead, roleBead, agentState, lastActivity               sql.NullString
	roleType, rig                                              sql.NullString
	crystallizes                                               sql.NullInt64
	quality                                                    sql.NullFloat64
}

type legacyCurrentFields struct {
	metadata, waiters                               sql.NullString
	closedAt, compactedAt, dueAt, deferUntil        sql.NullString
	ephemeral, pinned, template                     sql.NullInt64
	estimatedMinutes, compactionLevel, originalSize sql.NullInt64
}

type legacyExtras struct {
	removed legacyRemovedFields
	current legacyCurrentFields
}

func (x legacyExtras) validate(issue *types.Issue) error {
	if err := x.validateRawFields(issue); err != nil {
		return err
	}
	if err := x.validateIssueFields(issue); err != nil {
		return err
	}
	if err := x.validateCanonicalFields(issue); err != nil {
		return err
	}
	if err := issue.Validate(); err != nil {
		return fmt.Errorf("legacy SQLite issue %s: %w", issue.ID, err)
	}
	return nil
}

func (x legacyExtras) validateRawFields(issue *types.Issue) error {
	if err := checkUTF8(
		currentString{"issue metadata", nullString(x.current.metadata)},
		currentString{"issue waiters", nullString(x.current.waiters)},
	); err != nil {
		return err
	}
	if err := x.applyOptionalTimestamps(issue); err != nil {
		return err
	}
	return x.applyMetadataAndWaiters(issue)
}

func (x legacyExtras) validateIssueFields(issue *types.Issue) error {
	if err := validateIssueStrings(issue); err != nil {
		return err
	}
	if err := validateCurrentTextBytes(issue); err != nil {
		return err
	}
	return validateIssueVarcharsAndInts(x, issue)
}

func validateIssueVarcharsAndInts(x legacyExtras, issue *types.Issue) error {
	if err := validateIssueVarchars(issue); err != nil {
		return err
	}
	return checkCurrentInts(
		currentInt{"issue estimated_minutes", x.current.estimatedMinutes},
		currentInt{"issue compaction_level", x.current.compactionLevel},
		currentInt{"issue original_size", x.current.originalSize},
	)
}

func (x legacyExtras) validateCanonicalFields(issue *types.Issue) error {
	if err := x.applyCanonicalTimestamps(issue); err != nil {
		return err
	}
	if err := checkRequiredScalars(issue); err != nil {
		return err
	}
	return x.checkRemovedFields(issue)
}

// applyOptionalTimestamps parses the legacy issue's optional timestamp columns
// (closed_at, compacted_at, due_at, defer_until) and assigns them to issue.
func (x legacyExtras) applyOptionalTimestamps(issue *types.Issue) error {
	var err error
	if issue.ClosedAt, err = parseOptionalTime("closed_at", x.current.closedAt); err != nil {
		return fmt.Errorf("legacy SQLite issue %s: %w", issue.ID, err)
	}
	if issue.CompactedAt, err = parseOptionalTime("compacted_at", x.current.compactedAt); err != nil {
		return fmt.Errorf("legacy SQLite issue %s: %w", issue.ID, err)
	}
	if issue.DueAt, err = parseOptionalTime("due_at", x.current.dueAt); err != nil {
		return fmt.Errorf("legacy SQLite issue %s: %w", issue.ID, err)
	}
	if issue.DeferUntil, err = parseOptionalTime("defer_until", x.current.deferUntil); err != nil {
		return fmt.Errorf("legacy SQLite issue %s: %w", issue.ID, err)
	}
	return nil
}

// applyMetadataAndWaiters validates the legacy metadata and waiters JSON blobs
// (well-formed and free of unpaired surrogates) and assigns them to issue.
// metadata is stored verbatim unless it is the empty object; waiters is decoded.
func (x legacyExtras) applyMetadataAndWaiters(issue *types.Issue) error {
	if err := x.validateMetadata(issue.ID); err != nil {
		return err
	}
	if hasValue(x.current.metadata) && x.current.metadata.String != "{}" {
		issue.Metadata = []byte(x.current.metadata.String)
	}
	return x.applyWaiters(issue)
}

func (x legacyExtras) validateMetadata(issueID string) error {
	if !hasValue(x.current.metadata) {
		return nil
	}
	if !json.Valid([]byte(x.current.metadata.String)) {
		return fmt.Errorf("legacy SQLite issue %s has invalid metadata JSON", issueID)
	}
	if err := checkJSONSurrogates(x.current.metadata.String); err != nil {
		return fmt.Errorf("legacy SQLite issue %s metadata: %w", issueID, err)
	}
	return nil
}

func (x legacyExtras) applyWaiters(issue *types.Issue) error {
	if !hasValue(x.current.waiters) {
		return nil
	}
	if !json.Valid([]byte(x.current.waiters.String)) {
		return fmt.Errorf("issue %s waiters: invalid JSON", issue.ID)
	}
	if err := checkJSONSurrogates(x.current.waiters.String); err != nil {
		return fmt.Errorf("issue %s waiters: %w", issue.ID, err)
	}
	waiters, err := decodeWaiters(x.current.waiters.String)
	if err != nil {
		return fmt.Errorf("issue %s waiters: %w", issue.ID, err)
	}
	issue.Waiters = waiters
	return nil
}

func hasValue(value sql.NullString) bool {
	return value.Valid && value.String != ""
}

// applyCanonicalTimestamps normalizes the required created_at/updated_at values
// to the canonical current-schema representation.
func (x legacyExtras) applyCanonicalTimestamps(issue *types.Issue) error {
	var err error
	if issue.CreatedAt, err = canonicalCurrentDatetime(issue.CreatedAt); err != nil {
		return fmt.Errorf("legacy SQLite issue %s created_at: %w", issue.ID, err)
	}
	if issue.UpdatedAt, err = canonicalCurrentDatetime(issue.UpdatedAt); err != nil {
		return fmt.Errorf("legacy SQLite issue %s updated_at: %w", issue.ID, err)
	}
	return nil
}

// checkRequiredScalars enforces the non-empty ID, non-tombstone status, and
// present created_at/updated_at invariants every legacy issue must satisfy.
func checkRequiredScalars(issue *types.Issue) error {
	if issue.ID == "" {
		return fmt.Errorf("legacy SQLite issue has empty ID")
	}
	if issue.Status == "tombstone" {
		return fmt.Errorf("legacy SQLite issue %s is a tombstone", issue.ID)
	}
	if issue.CreatedAt.IsZero() || issue.UpdatedAt.IsZero() {
		return fmt.Errorf("legacy SQLite issue %s has invalid created_at or updated_at", issue.ID)
	}
	return nil
}

// checkRemovedFields rejects legacy rows that populate columns the current
// schema no longer supports, then validates the tri-state boolean columns.
func (x legacyExtras) checkRemovedFields(issue *types.Issue) error {
	if x.removed.hasUnsupportedValues(issue) {
		return fmt.Errorf("legacy SQLite issue %s uses unsupported removed fields", issue.ID)
	}
	return x.current.validateBooleans(issue.ID)
}

func (r legacyRemovedFields) hasUnsupportedValues(issue *types.Issue) bool {
	return nonempty(r.closedBy, r.deletedBy, r.deleteReason, r.originalType, r.hookBead, r.roleBead, r.agentState, r.lastActivity, r.roleType, r.rig) || r.deletedAt.Valid || r.crystallizes.Int64 != 0 || r.quality.Valid || (issue.SourceRepo != "" && issue.SourceRepo != ".")
}

func (c legacyCurrentFields) validateBooleans(issueID string) error {
	for _, b := range []struct {
		name string
		v    sql.NullInt64
	}{{"ephemeral", c.ephemeral}, {"pinned", c.pinned}, {"is_template", c.template}} {
		if b.v.Valid && b.v.Int64 != 0 && b.v.Int64 != 1 {
			return fmt.Errorf("issue %s has invalid %s boolean", issueID, b.name)
		}
	}
	return nil
}

type currentVarchar struct {
	name, value string
	maxRunes    int
}

type currentString struct {
	name, value string
}

func validateIssueStrings(issue *types.Issue) error {
	fields := []currentString{
		{"issue id", issue.ID},
		{"issue title", issue.Title},
		{"issue description", issue.Description},
		{"issue design", issue.Design},
		{"issue acceptance_criteria", issue.AcceptanceCriteria},
		{"issue notes", issue.Notes},
		{"issue status", string(issue.Status)},
		{"issue type", string(issue.IssueType)},
		{"issue assignee", issue.Assignee},
		{"issue created_by", issue.CreatedBy},
		{"issue owner", issue.Owner},
		{"issue spec_id", issue.SpecID},
		{"issue close_reason", issue.CloseReason},
		{"issue sender", issue.Sender},
		{"issue wisp_type", string(issue.WispType)},
		{"issue await_type", issue.AwaitType},
		{"issue await_id", issue.AwaitID},
		{"issue mol_type", string(issue.MolType)},
		{"issue work_type", string(issue.WorkType)},
		{"issue source_system", issue.SourceSystem},
		{"issue event_kind", issue.EventKind},
		{"issue actor", issue.Actor},
		{"issue target", issue.Target},
		{"issue payload", issue.Payload},
	}
	if issue.ExternalRef != nil {
		fields = append(fields, currentString{"issue external_ref", *issue.ExternalRef})
	}
	if issue.CompactedAtCommit != nil {
		fields = append(fields, currentString{"issue compacted_at_commit", *issue.CompactedAtCommit})
	}
	for i, waiter := range issue.Waiters {
		fields = append(fields, currentString{fmt.Sprintf("issue waiters[%d]", i), waiter})
	}
	return checkUTF8(fields...)
}

func checkUTF8(fields ...currentString) error {
	for _, field := range fields {
		if !utf8.ValidString(field.value) {
			return fmt.Errorf("legacy SQLite %s contains invalid UTF-8", field.name)
		}
	}
	return nil
}

func checkJSONSurrogates(raw string) error {
	rawLen := len(raw)
	for i := 0; i < rawLen; i++ {
		if raw[i] != '\\' {
			continue
		}
		next, err := checkJSONEscape(raw, i)
		if err != nil {
			return err
		}
		i = next
	}
	return nil
}

func checkJSONEscape(raw string, slash int) (int, error) {
	if slash+1 >= len(raw) {
		return slash, fmt.Errorf("truncated JSON escape")
	}
	if raw[slash+1] != 'u' {
		return slash + 1, nil
	}
	if slash+6 > len(raw) {
		return slash, fmt.Errorf("truncated JSON Unicode escape")
	}
	code, err := strconv.ParseUint(raw[slash+2:slash+6], 16, 16)
	if err != nil {
		return slash, fmt.Errorf("invalid JSON Unicode escape")
	}
	switch {
	case code >= 0xd800 && code <= 0xdbff:
		return checkJSONHighSurrogate(raw, slash+6)
	case code >= 0xdc00 && code <= 0xdfff:
		return slash, fmt.Errorf("lone low UTF-16 surrogate escape")
	default:
		return slash + 5, nil
	}
}

func checkJSONHighSurrogate(raw string, next int) (int, error) {
	if next+6 > len(raw) || raw[next] != '\\' || raw[next+1] != 'u' {
		return next, fmt.Errorf("lone high UTF-16 surrogate escape")
	}
	low, err := strconv.ParseUint(raw[next+2:next+6], 16, 16)
	if err != nil || low < 0xdc00 || low > 0xdfff {
		return next, fmt.Errorf("lone high UTF-16 surrogate escape")
	}
	return next + 5, nil
}

func validateCurrentTextBytes(issue *types.Issue) error {
	if len(issue.Payload) > currentTextBytes {
		return fmt.Errorf("legacy SQLite issue payload is %d bytes (current TEXT maximum %d)", len(issue.Payload), currentTextBytes)
	}
	waiters := issueops.FormatJSONStringArray(issue.Waiters)
	if len(waiters) > currentTextBytes {
		return fmt.Errorf("legacy SQLite issue waiters serialize to %d bytes (current TEXT maximum %d)", len(waiters), currentTextBytes)
	}
	return nil
}

func decodeWaiters(raw string) ([]string, error) {
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, err
	}
	values, ok := decoded.([]any)
	if !ok {
		return nil, fmt.Errorf("must be an array of strings")
	}
	waiters := make([]string, len(values))
	for i, value := range values {
		waiter, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("element %d is not a string", i)
		}
		if err := checkUTF8(currentString{fmt.Sprintf("waiters[%d]", i), waiter}); err != nil {
			return nil, err
		}
		waiters[i] = waiter
	}
	return waiters, nil
}

func validateIssueVarchars(issue *types.Issue) error {
	fields := []currentVarchar{
		{"issue id", issue.ID, types.MaxFieldLen},
		{"issue title", issue.Title, currentTitleVarcharRunes},
		{"issue status", string(issue.Status), currentShortVarcharRunes},
		{"issue type", string(issue.IssueType), currentShortVarcharRunes},
		{"issue assignee", issue.Assignee, types.MaxFieldLen},
		{"issue created_by", issue.CreatedBy, types.MaxFieldLen},
		{"issue owner", issue.Owner, types.MaxFieldLen},
		{"issue spec_id", issue.SpecID, currentSpecIDVarcharRunes},
		{"issue sender", issue.Sender, types.MaxFieldLen},
		{"issue wisp_type", string(issue.WispType), currentShortVarcharRunes},
		{"issue await_type", issue.AwaitType, currentShortVarcharRunes},
		{"issue await_id", issue.AwaitID, types.MaxFieldLen},
		{"issue mol_type", string(issue.MolType), currentShortVarcharRunes},
		{"issue event_kind", issue.EventKind, currentShortVarcharRunes},
		{"issue actor", issue.Actor, types.MaxFieldLen},
		{"issue target", issue.Target, types.MaxFieldLen},
		{"issue work_type", string(issue.WorkType), currentShortVarcharRunes},
		{"issue source_system", issue.SourceSystem, types.MaxFieldLen},
	}
	if issue.ExternalRef != nil {
		fields = append(fields, currentVarchar{"issue external_ref", *issue.ExternalRef, types.MaxFieldLen})
	}
	if issue.CompactedAtCommit != nil {
		fields = append(fields, currentVarchar{"issue compacted_at_commit", *issue.CompactedAtCommit, currentCommitVarcharRunes})
	}
	return checkCurrentVarchars(fields...)
}

func checkCurrentVarchars(fields ...currentVarchar) error {
	for _, field := range fields {
		if n := utf8.RuneCountInString(field.value); n > field.maxRunes {
			return fmt.Errorf("legacy SQLite %s is %d characters (current VARCHAR(%d) maximum)", field.name, n, field.maxRunes)
		}
	}
	return nil
}

type currentInt struct {
	name  string
	value sql.NullInt64
}

func checkCurrentInts(fields ...currentInt) error {
	for _, field := range fields {
		if field.value.Valid && (field.value.Int64 < math.MinInt32 || field.value.Int64 > math.MaxInt32) {
			return fmt.Errorf("legacy SQLite %s is %d (current INT range %d..%d)", field.name, field.value.Int64, math.MinInt32, math.MaxInt32)
		}
	}
	return nil
}

func nonempty(values ...sql.NullString) bool {
	for _, v := range values {
		if v.Valid && v.String != "" {
			return true
		}
	}
	return false
}
func nullString(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}
func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999"} {
		if t, e := time.Parse(layout, s); e == nil {
			canonical, err := canonicalCurrentDatetime(t)
			if err != nil {
				return time.Time{}, fmt.Errorf("timestamp %q: %w", s, err)
			}
			return canonical, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q", s)
}

func canonicalCurrentDatetime(t time.Time) (time.Time, error) {
	canonical := t.UTC().Round(time.Second)
	if year := canonical.Year(); year < 0 || year > 9999 {
		return time.Time{}, fmt.Errorf("timestamp rounds outside current DATETIME range")
	}
	return canonical, nil
}

func parseOptionalTime(name string, raw sql.NullString) (*time.Time, error) {
	if !raw.Valid {
		return nil, nil
	}
	parsed, err := parseTime(raw.String)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &parsed, nil
}

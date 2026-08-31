package issueops

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

// IssueSelectColumns is the canonical column list for full issue hydration.
// Every query that reads a complete types.Issue from the issues table should
// use this constant to avoid column-list drift between scan sites. The list
// itself lives in internal/storage/sqlbuild, shared with the domain/db stack;
// ScanIssueFrom below scans it positionally and must stay in agreement.
const IssueSelectColumns = sqlbuild.IssueSelectColumns

// IssueSelectColumnsLite is the column list for lite issue hydration. It mirrors
// IssueSelectColumns in order, minus the heavy TEXT columns enumerated in
// HeavyDropList. Used when a caller opts in via types.IssueFilter.Lite=true to
// skip materializing large text bodies on listing paths.
//
// metadata is intentionally retained — it is small and read by routing.
//
// row_lock and the leases.* overlay (lease_expires_at, heartbeat_at,
// granted_node; lease_expires_at/heartbeat_at added by migration 0054 after
// this list was first written, see #4150; granted_node added by migration
// 0016/wy-jpd3.7 for replica-aware leases) are also retained: all three are
// small, non-TEXT columns that routing/claim code reads (optimistic
// concurrency token, active-lease state, granting replica), not the
// multi-KB bodies this split exists to skip. Any query selecting
// IssueSelectColumnsLite must include sqlbuild.LeaseJoin(table) in its FROM
// clause, exactly as full hydration does (see issueLiteProjection in
// search.go, joinLeases: true).
// The list itself lives in internal/storage/sqlbuild beside IssueSelectColumns,
// shared with the domain/db stack and with the counts mega-query, which renders
// a qualified variant of it.
const IssueSelectColumnsLite = sqlbuild.IssueSelectColumnsLite

// HeavyDropList enumerates the columns omitted from IssueSelectColumnsLite.
// Test-only: the schema-parity test asserts
//
//	cols(IssueSelectColumnsLite) ∪ HeavyDropList == cols(IssueSelectColumns)
//
// so every future column added to IssueSelectColumns must be classified
// explicitly — either into IssueSelectColumnsLite (small, routing/listing
// reads it) or into HeavyDropList (large body, fetch via GetIssue when
// needed). Production code paths reference IssueSelectColumns or
// IssueSelectColumnsLite directly; do not consume this list at runtime.
var HeavyDropList = []string{
	"description",
	"design",
	"acceptance_criteria",
	"notes",
	"waiters",
	"payload",
}

// IssueScanner is the common interface between *sql.Row and *sql.Rows,
// allowing a single scan function to work with both single-row and
// multi-row query results.
type IssueScanner interface {
	Scan(dest ...any) error
}

type issueScanFields struct {
	times    issueScanTimes
	numbers  issueScanNumbers
	identity issueScanIdentity
	event    issueScanEvent
	routing  issueScanRouting
}

type issueScanTimes struct {
	createdAtStr, updatedAtStr                          sql.NullString
	startedAt, closedAt, compactedAt, dueAt, deferUntil sql.NullTime
	leaseExpiresAt, heartbeatAt                         sql.NullTime
}

type issueScanNumbers struct {
	estimatedMinutes, originalSize, timeoutNs sql.NullInt64
	ephemeral, noHistory, pinned, isTemplate  sql.NullInt64
	rowLock                                   sql.NullInt64
}

type issueScanIdentity struct {
	createdBy                                               sql.NullString
	assignee, externalRef, specID, compactedAtCommit, owner sql.NullString
	contentHash, sourceRepo, closeReason, closedBySession   sql.NullString
}

type issueScanEvent struct {
	sender, wispType, molType, eventKind, actor, target, payload sql.NullString
	awaitType, awaitID, waiters                                  sql.NullString
}

type issueScanRouting struct {
	workType, sourceSystem sql.NullString
	metadata, storageClass sql.NullString
	leaseGrantedNode       sql.NullString
}

func (f *issueScanFields) destinations(issue *types.Issue, lite bool) []any {
	if lite {
		return f.liteDestinations(issue)
	}
	return f.fullDestinations(issue)
}

func (f *issueScanFields) fullDestinations(issue *types.Issue) []any {
	return []any{
		&issue.ID, &f.identity.contentHash, &issue.Title, &issue.Description, &issue.Design,
		&issue.AcceptanceCriteria, &issue.Notes, &issue.Status,
		&issue.Priority, &issue.IssueType, &f.identity.assignee, &f.numbers.estimatedMinutes,
		&f.times.createdAtStr, &f.identity.createdBy, &f.identity.owner, &f.times.updatedAtStr, &f.times.startedAt, &f.times.closedAt, &f.identity.externalRef, &f.identity.specID,
		&issue.CompactionLevel, &f.times.compactedAt, &f.identity.compactedAtCommit, &f.numbers.originalSize, &f.identity.sourceRepo, &f.identity.closeReason, &f.identity.closedBySession,
		&f.event.sender, &f.numbers.ephemeral, &f.numbers.noHistory, &f.event.wispType, &f.numbers.pinned, &f.numbers.isTemplate,
		&f.event.awaitType, &f.event.awaitID, &f.numbers.timeoutNs, &f.event.waiters,
		&f.event.molType,
		&f.event.eventKind, &f.event.actor, &f.event.target, &f.event.payload,
		&f.times.dueAt, &f.times.deferUntil,
		&f.routing.workType, &f.routing.sourceSystem, &f.routing.metadata, &f.numbers.rowLock, &f.routing.storageClass,
		&f.times.leaseExpiresAt, &f.times.heartbeatAt, &f.routing.leaseGrantedNode,
	}
}

func (f *issueScanFields) liteDestinations(issue *types.Issue) []any {
	return []any{
		&issue.ID, &f.identity.contentHash, &issue.Title,
		&issue.Status,
		&issue.Priority, &issue.IssueType, &f.identity.assignee, &f.numbers.estimatedMinutes,
		&f.times.createdAtStr, &f.identity.createdBy, &f.identity.owner, &f.times.updatedAtStr, &f.times.startedAt, &f.times.closedAt, &f.identity.externalRef, &f.identity.specID,
		&issue.CompactionLevel, &f.times.compactedAt, &f.identity.compactedAtCommit, &f.numbers.originalSize, &f.identity.sourceRepo, &f.identity.closeReason, &f.identity.closedBySession,
		&f.event.sender, &f.numbers.ephemeral, &f.numbers.noHistory, &f.event.wispType, &f.numbers.pinned, &f.numbers.isTemplate,
		&f.event.awaitType, &f.event.awaitID, &f.numbers.timeoutNs,
		&f.event.molType,
		&f.event.eventKind, &f.event.actor, &f.event.target,
		&f.times.dueAt, &f.times.deferUntil,
		&f.routing.workType, &f.routing.sourceSystem, &f.routing.metadata, &f.numbers.rowLock, &f.routing.storageClass,
		&f.times.leaseExpiresAt, &f.times.heartbeatAt, &f.routing.leaseGrantedNode,
	}
}

func applyIssueScanFields(issue *types.Issue, f *issueScanFields) {
	issue.CreatedAt = parseIssueTime(f.times.createdAtStr)
	issue.UpdatedAt = parseIssueTime(f.times.updatedAtStr)
	issue.ContentHash = f.identity.contentHash.String
	issue.StartedAt = issueTimePointer(f.times.startedAt)
	issue.ClosedAt = issueTimePointer(f.times.closedAt)
	issue.EstimatedMinutes = issueIntPointer(f.numbers.estimatedMinutes)
	issue.Assignee = f.identity.assignee.String
	issue.CreatedBy = f.identity.createdBy.String
	issue.Owner = f.identity.owner.String
	issue.ExternalRef = issueStringPointer(f.identity.externalRef)
	issue.SpecID = f.identity.specID.String
	issue.CompactedAt = issueTimePointer(f.times.compactedAt)
	issue.CompactedAtCommit = issueStringPointer(f.identity.compactedAtCommit)
	issue.OriginalSize = int(f.numbers.originalSize.Int64)
	issue.SourceRepo = f.identity.sourceRepo.String
	issue.CloseReason = f.identity.closeReason.String
	issue.ClosedBySession = f.identity.closedBySession.String
	issue.Sender = f.event.sender.String
	issue.Ephemeral = issueFlag(f.numbers.ephemeral)
	issue.NoHistory = issueFlag(f.numbers.noHistory)
	issue.WispType = types.WispType(f.event.wispType.String)
	issue.Pinned = issueFlag(f.numbers.pinned)
	issue.IsTemplate = issueFlag(f.numbers.isTemplate)
	issue.AwaitType = f.event.awaitType.String
	issue.AwaitID = f.event.awaitID.String
	issue.Timeout = time.Duration(f.numbers.timeoutNs.Int64)
	issue.Waiters = issueStringArray(f.event.waiters)
	issue.MolType = types.MolType(f.event.molType.String)
	issue.EventKind = f.event.eventKind.String
	issue.Actor = f.event.actor.String
	issue.Target = f.event.target.String
	issue.Payload = f.event.payload.String
	issue.DueAt = issueTimePointer(f.times.dueAt)
	issue.DeferUntil = issueTimePointer(f.times.deferUntil)
	issue.WorkType = types.WorkType(f.routing.workType.String)
	issue.SourceSystem = f.routing.sourceSystem.String
	issue.Metadata = issueMetadataBytes(f.routing.metadata)
	issue.RowVersion = f.numbers.rowLock.Int64
	issue.StorageClass = types.StorageClass(f.routing.storageClass.String)
	issue.LeaseExpiresAt = issueTimePointer(f.times.leaseExpiresAt)
	issue.HeartbeatAt = issueTimePointer(f.times.heartbeatAt)
	issue.LeaseGrantedNode = f.routing.leaseGrantedNode.String
}

func parseIssueTime(value sql.NullString) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return ParseTimeString(value.String)
}

func issueTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func issueStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func issueIntPointer(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	result := int(value.Int64)
	return &result
}

func issueFlag(value sql.NullInt64) bool {
	return value.Valid && value.Int64 != 0
}

func issueStringArray(value sql.NullString) []string {
	if !value.Valid {
		return nil
	}
	return ParseJSONStringArray(value.String)
}

func issueMetadataBytes(value sql.NullString) []byte {
	if !value.Valid || value.String == "" || value.String == "{}" {
		return nil
	}
	return []byte(value.String)
}

// ScanIssueFrom scans a full issue from any source implementing IssueScanner.
// The caller must ensure the query selected exactly IssueSelectColumns in
// order; any extra dests are appended for trailing columns beyond that list.
func ScanIssueFrom(s IssueScanner, extra ...any) (*types.Issue, error) {
	var issue types.Issue
	fields := issueScanFields{}
	dests := fields.destinations(&issue, false)
	dests = append(dests, extra...)
	if err := s.Scan(dests...); err != nil {
		return nil, err
	}
	applyIssueScanFields(&issue, &fields)
	return &issue, nil
}

// ScanIssueLiteFrom scans a lite issue from any source implementing IssueScanner.
// The caller must ensure the query selected exactly IssueSelectColumnsLite in
// order. Heavy text fields (Description, Design, AcceptanceCriteria, Notes,
// Payload, Waiters) are NOT read from the row and remain zero-valued on the
// returned issue. The returned issue has IsLitePartial=true so downstream code
// can detect the partial hydration.
func ScanIssueLiteFrom(s IssueScanner, extra ...any) (*types.Issue, error) {
	var issue types.Issue
	fields := issueScanFields{}
	dests := fields.destinations(&issue, true)
	dests = append(dests, extra...)
	if err := s.Scan(dests...); err != nil {
		return nil, err
	}
	applyIssueScanFields(&issue, &fields)
	issue.IsLitePartial = true
	return &issue, nil
}

// ParseTimeString parses a time string from database TEXT columns (non-nullable).
// Supports RFC3339Nano, RFC3339, and MySQL DATETIME format.
func ParseTimeString(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Try RFC3339Nano first (more precise), then RFC3339, then DATETIME format
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{} // Unparseable - shouldn't happen with valid data
}

// ParseJSONStringArray unmarshals a JSON string array. Returns nil on error or empty input.
func ParseJSONStringArray(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		return nil
	}
	return result
}

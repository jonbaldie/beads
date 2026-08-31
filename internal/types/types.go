// Package types defines core data structures for the bd issue tracker.
package types

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"time"
	"unicode/utf8"
)

// Issue represents a trackable work item.
// Fields are organized into logical groups for maintainability.
// Nested anonymous groups keep the field count under messgo TooManyFields
// while preserving promoted access (issue.Title) and flattened JSON order.
type Issue struct {
	IssueID
	IssueContent
	IssueWorkflow
	IssueTimes
	IssueLease
	IssueMeta
	IssueGraph
	IssueWisp
	IssueCoord
	IssueEvent
}

// ComputeContentHash creates a deterministic hash of the issue's content.
// Uses all substantive fields (excluding ID, timestamps, and compaction metadata)
// to ensure that identical content produces identical hashes across all clones.
func (i *Issue) ComputeContentHash() string {
	h := sha256.New()
	w := hashFieldWriter{h}

	// Core fields in stable order
	w.str(i.Title)
	w.str(i.Description)
	w.str(i.Design)
	w.str(i.AcceptanceCriteria)
	w.str(i.Notes)
	w.str(i.SpecID)
	w.str(string(i.Status))
	w.int(i.Priority)
	w.str(string(i.IssueType))
	w.str(i.Assignee)
	w.str(i.Owner)
	w.str(i.CreatedBy)

	// Optional fields
	w.strPtr(i.ExternalRef)
	w.str(i.SourceSystem)
	w.flag(i.Pinned, "pinned")
	w.str(string(i.Metadata)) // Include metadata in content hash
	w.flag(i.IsTemplate, "template")

	// Bonded molecules
	for _, br := range i.BondedFrom {
		w.str(br.SourceID)
		w.str(br.BondType)
		w.str(br.BondPoint)
	}

	// Gate fields for async coordination
	w.str(i.AwaitType)
	w.str(i.AwaitID)
	w.duration(i.Timeout)
	for _, waiter := range i.Waiters {
		w.str(waiter)
	}

	// Molecule type
	w.str(string(i.MolType))

	// Work type
	w.str(string(i.WorkType))

	// Event fields
	w.str(i.EventKind)
	w.str(i.Actor)
	w.str(i.Target)
	w.str(i.Payload)

	return fmt.Sprintf("%x", h.Sum(nil))
}

// hashFieldWriter provides helper methods for writing fields to a hash.
// Each method writes the value followed by a null separator for consistency.
type hashFieldWriter struct {
	h hash.Hash
}

func (w hashFieldWriter) str(s string) {
	w.h.Write([]byte(s))
	w.h.Write([]byte{0})
}

func (w hashFieldWriter) int(n int) {
	w.h.Write([]byte(fmt.Sprintf("%d", n)))
	w.h.Write([]byte{0})
}

func (w hashFieldWriter) strPtr(p *string) {
	if p != nil {
		w.h.Write([]byte(*p))
	}
	w.h.Write([]byte{0})
}

func (w hashFieldWriter) duration(d time.Duration) {
	w.h.Write([]byte(fmt.Sprintf("%d", d)))
	w.h.Write([]byte{0})
}

func (w hashFieldWriter) flag(b bool, label string) {
	if b {
		w.h.Write([]byte(label))
	}
	w.h.Write([]byte{0})
}

// MaxFieldLen is the maximum length (in characters) of common bounded text
// fields, matching their VARCHAR(255) columns.
const MaxFieldLen = 255

// ErrFieldTooLong is returned when a bounded text field exceeds MaxFieldLen
// characters. Callers can errors.Is it instead of matching a raw backend "data
// too long" string.
var ErrFieldTooLong = errors.New("field exceeds maximum length")

// CheckFieldLen returns ErrFieldTooLong (wrapped with context) when val exceeds
// MaxFieldLen characters. name is the field label used in the message. Length is
// counted in runes, not bytes, so a multibyte value up to MaxFieldLen characters
// fits the VARCHAR(255) column and passes.
func CheckFieldLen(name, val string) error {
	if n := utf8.RuneCountInString(val); n > MaxFieldLen {
		return fmt.Errorf("%w: %s is %d characters (max %d)", ErrFieldTooLong, name, n, MaxFieldLen)
	}
	return nil
}

// MaxTextBytes is the maximum size, in BYTES, of a `TEXT` column — the storage
// ceiling for the values this schema keeps in one rather than in a LONGTEXT.
//
// BYTES, NOT CHARACTERS, which is the one place this differs from MaxFieldLen
// beside it and the reason CheckTextLen does not simply call CheckFieldLen with
// a bigger number: MySQL and Dolt bound a TEXT column by its encoded length, so
// a value of 40000 multi-byte characters overflows it while a value of 65000
// ASCII characters does not.
//
// The large-content columns are deliberately NOT bounded by this — issue
// descriptions and comment bodies are LONGTEXT precisely so an embedded image or
// a captured transcript fits (migrations 0049 and 0065). This is for the columns
// that hold a VALUE rather than a document, `config.value` being the one a front
// door can reach with an arbitrary payload.
const MaxTextBytes = 65535

// CheckTextLen returns ErrFieldTooLong (wrapped with context) when val exceeds
// MaxTextBytes bytes. name is the field label used in the message.
//
// It exists so a front door can refuse an oversized value with a 400 that names
// the member, instead of letting the column refuse it — which arrives as a
// driver error, is classified as a generic 500, and tells the caller nothing it
// could act on.
func CheckTextLen(name, val string) error {
	if n := len(val); n > MaxTextBytes {
		return fmt.Errorf("%w: %s is %d bytes (max %d)", ErrFieldTooLong, name, n, MaxTextBytes)
	}
	return nil
}

// ValidateIssueTitle checks the canonical issue-title requirements. The title
// must be nonempty, valid UTF-8, and at most 500 bytes.
func ValidateIssueTitle(title string) error {
	if len(title) == 0 {
		return fmt.Errorf("title is required")
	}
	if !utf8.ValidString(title) {
		return fmt.Errorf("title must be valid UTF-8")
	}
	if len(title) > 500 {
		return fmt.Errorf("title must be 500 bytes or less (got %d)", len(title))
	}
	return nil
}

// ValidateIssuePriority checks that priority is in the canonical P0-P4 range.
func ValidateIssuePriority(priority int) error {
	if priority < 0 || priority > 4 {
		return fmt.Errorf("priority must be between 0 and 4 (got %d)", priority)
	}
	return nil
}

// ValidateIssueEstimatedMinutes checks that a supplied estimate is
// nonnegative. A nil estimate is valid.
func ValidateIssueEstimatedMinutes(estimatedMinutes *int) error {
	if estimatedMinutes != nil && *estimatedMinutes < 0 {
		return fmt.Errorf("estimated_minutes cannot be negative")
	}
	return nil
}

// Validate checks if the issue has valid field values (built-in statuses only)
func (i *Issue) Validate() error {
	return i.ValidateWithCustomStatuses(nil)
}

// ValidateWithCustomStatuses checks if the issue has valid field values,
// allowing custom statuses in addition to built-in ones.
func (i *Issue) ValidateWithCustomStatuses(customStatuses []string) error {
	return i.ValidateWithCustom(customStatuses, nil)
}

// ValidateWithCustom checks if the issue has valid field values,
// allowing custom statuses and types in addition to built-in ones.
func (i *Issue) ValidateWithCustom(customStatuses, customTypes []string) error {
	if err := validateIssueBasics(i, customStatuses); err != nil {
		return err
	}
	if !i.IssueType.IsValidWithCustom(customTypes) {
		return fmt.Errorf("invalid issue type: %s", i.IssueType)
	}
	if err := ValidateIssueEstimatedMinutes(i.EstimatedMinutes); err != nil {
		return err
	}
	if err := validateIssueClosedAt(i); err != nil {
		return err
	}
	if err := validateIssueMetadata(i.Metadata); err != nil {
		return err
	}
	if err := validateIssueStorage(i); err != nil {
		return err
	}
	return validateIssueAssignment(i)
}

// ValidateForImport validates the issue for multi-repo import (federation trust model).
// Built-in types are validated (to catch typos). Non-built-in types are trusted
// since the source repo already validated them when the issue was created.
// This implements "trust the chain below you" from the HOP federation model.
func (i *Issue) ValidateForImport(customStatuses []string) error {
	if err := validateIssueBasics(i, customStatuses); err != nil {
		return err
	}
	// Issue type validation follows the federation trust model: source repos
	// have already validated custom types, so import accepts them unchanged.
	if err := ValidateIssueEstimatedMinutes(i.EstimatedMinutes); err != nil {
		return err
	}
	if err := validateIssueClosedAt(i); err != nil {
		return err
	}
	if err := validateIssueMetadata(i.Metadata); err != nil {
		return err
	}
	return validateIssueAssignment(i)
}

func validateIssueBasics(i *Issue, customStatuses []string) error {
	if err := ValidateIssueTitle(i.Title); err != nil {
		return err
	}
	if err := ValidateIssuePriority(i.Priority); err != nil {
		return err
	}
	if !i.Status.IsValidWithCustom(customStatuses) {
		return fmt.Errorf("invalid status: %s", i.Status)
	}
	return nil
}

func validateIssueClosedAt(i *Issue) error {
	if i.Status == StatusClosed && i.ClosedAt == nil {
		return fmt.Errorf("closed issues must have closed_at timestamp")
	}
	if i.Status != StatusClosed && i.ClosedAt != nil {
		return fmt.Errorf("non-closed issues cannot have closed_at timestamp")
	}
	return nil
}

func validateIssueMetadata(metadata json.RawMessage) error {
	if len(metadata) > 0 && !json.Valid(metadata) {
		return fmt.Errorf("metadata must be valid JSON")
	}
	return nil
}

func validateIssueStorage(i *Issue) error {
	if i.Ephemeral && i.NoHistory {
		return fmt.Errorf("ephemeral and no_history are mutually exclusive")
	}
	return validateIssueStorageClass(i)
}

func validateIssueStorageClass(i *Issue) error {
	if !i.StorageClass.IsValid() {
		return fmt.Errorf("invalid storage class %q (must be %s)", i.StorageClass, ValidStorageClassNames())
	}
	if i.StorageClass == "" {
		return nil
	}

	wispPlane := i.Ephemeral || i.NoHistory
	if wispPlane && i.StorageClass != StorageClassEphemeral {
		return fmt.Errorf("storage class %q conflicts with ephemeral/no_history (wisp-plane records are storage class ephemeral)", i.StorageClass)
	}
	if !wispPlane && i.StorageClass == StorageClassEphemeral {
		return fmt.Errorf("storage class ephemeral requires the ephemeral flag (create with --ephemeral)")
	}
	return nil
}

func validateIssueAssignment(i *Issue) error {
	if err := CheckFieldLen("assignee", i.Assignee); err != nil {
		return err
	}
	if err := CheckFieldLen("owner", i.Owner); err != nil {
		return err
	}
	return nil
}

// SetDefaults applies default values for fields that may be omitted during deserialization.
// Call this after json.Unmarshal to ensure missing fields have proper defaults:
//   - Status: defaults to StatusOpen if empty
//   - Priority: defaults to 2 if zero (note: P0 issues must explicitly set priority=0)
//   - IssueType: defaults to TypeTask if empty
func (i *Issue) SetDefaults() {
	if i.Status == "" {
		i.Status = StatusOpen
	}
	// Note: priority 0 (P0) is a valid value, so we can't distinguish between
	// "explicitly set to 0" and "omitted". We treat priority 0 as P0,
	// not as "use default". P0 issues are explicitly marked.
	// Priority default of 2 only applies to new issues via Create, not import.
	if i.IssueType == "" {
		i.IssueType = TypeTask
	}
}

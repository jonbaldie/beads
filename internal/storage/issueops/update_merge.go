package issueops

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// HasMergeOps reports whether the update map carries any read-merge-write
// operation key. Updates with merge ops must resolve those ops against the row
// read inside the same mutation transaction (see ResolveMergeOps); on Dolt the
// store's whole-attempt retry then re-runs that resolution against the winning
// writer's committed row.
func HasMergeOps(updates map[string]interface{}) bool {
	for _, op := range []string{OpMergeMetadata, OpSetMetadata, OpUnsetMetadata, OpAppendNotes} {
		if _, ok := updates[op]; ok {
			return true
		}
	}
	return false
}

// ResolveMergeOps rewrites merge-operation keys into concrete column values
// using oldIssue, which the caller read in the same mutation transaction.
// Returns the input map unchanged when no operation keys are present;
// otherwise returns a copy so the caller's map is not mutated.
func ResolveMergeOps(oldIssue *types.Issue, updates map[string]interface{}) (map[string]interface{}, error) {
	if !HasMergeOps(updates) {
		return updates, nil
	}

	resolved := make(map[string]interface{}, len(updates))
	for k, v := range updates {
		if !isMergeOpKey(k) {
			resolved[k] = v
		}
	}

	if err := resolveMetadataMergeOps(oldIssue, updates, resolved); err != nil {
		return nil, err
	}
	if err := resolveNotesAppendOp(oldIssue, updates, resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

// DiscardNoopIssueUpdates removes concrete updates whose value already matches
// the row read by the caller. This keeps idempotent updates from advancing the
// row version or recording an event.
func DiscardNoopIssueUpdates(oldIssue *types.Issue, updates map[string]interface{}) (map[string]interface{}, error) {
	filtered := make(map[string]interface{}, len(updates))
	for key, value := range updates {
		unchanged, err := issueFieldMatches(oldIssue, key, value)
		if err != nil {
			return nil, err
		}
		if !unchanged {
			filtered[key] = value
		}
	}
	return filtered, nil
}

func issueFieldMatches(issue *types.Issue, key string, value interface{}) (bool, error) {
	if matched, handled := matchIssueStringField(issue, key, value); handled {
		return matched, nil
	}
	if matched, handled := matchIssueStatusField(issue, key, value); handled {
		return matched, nil
	}
	if matched, handled := matchIssueBooleanField(issue, key, value); handled {
		return matched, nil
	}
	if matched, handled := matchIssuePointerField(issue, key, value); handled {
		return matched, nil
	}
	return matchIssueSpecialField(issue, key, value)
}

func matchIssueSpecialField(issue *types.Issue, key string, value interface{}) (bool, error) {
	switch key {
	case "waiters":
		waiters, ok := value.([]string)
		return ok && slices.Equal(issue.Waiters, waiters), nil
	case "metadata":
		current, err := normalizedMetadata(issue.Metadata)
		if err != nil {
			return false, err
		}
		candidate, err := storage.NormalizeMetadataValue(value)
		if err != nil {
			return false, err
		}
		return current == candidate, nil
	default:
		return false, nil
	}
}

func matchIssueStringField(issue *types.Issue, key string, value interface{}) (bool, bool) {
	fields := map[string]string{
		"title":               issue.Title,
		"description":         issue.Description,
		"design":              issue.Design,
		"acceptance_criteria": issue.AcceptanceCriteria,
		"notes":               issue.Notes,
		"spec_id":             issue.SpecID,
		"await_id":            issue.AwaitID,
		"assignee":            issue.Assignee,
		"owner":               issue.Owner,
		"close_reason":        issue.CloseReason,
		"closed_by_session":   issue.ClosedBySession,
		"source_repo":         issue.SourceRepo,
		"sender":              issue.Sender,
		"event_category":      issue.EventKind,
		"event_kind":          issue.EventKind,
		"event_actor":         issue.Actor,
		"actor":               issue.Actor,
		"event_target":        issue.Target,
		"target":              issue.Target,
		"event_payload":       issue.Payload,
		"payload":             issue.Payload,
	}
	current, ok := fields[key]
	if !ok {
		return false, false
	}
	return matchesString(current, value), true
}

func matchIssueStatusField(issue *types.Issue, key string, value interface{}) (bool, bool) {
	switch key {
	case "status":
		return matchesStatus(issue.Status, value), true
	case "priority":
		return matchesInt(issue.Priority, value), true
	case "issue_type":
		return matchesIssueType(issue.IssueType, value), true
	default:
		return false, false
	}
}

func matchIssueBooleanField(issue *types.Issue, key string, value interface{}) (bool, bool) {
	switch key {
	case "wisp":
		return matchesBool(issue.Ephemeral, value), true
	case "no_history":
		return matchesBool(issue.NoHistory, value), true
	case "pinned":
		return matchesBool(issue.Pinned, value), true
	case "wisp_type":
		return matchesWispType(issue.WispType, value), true
	case "mol_type":
		return matchesMolType(issue.MolType, value), true
	default:
		return false, false
	}
}

func matchIssuePointerField(issue *types.Issue, key string, value interface{}) (bool, bool) {
	switch key {
	case "estimated_minutes":
		return matchesIntPointer(issue.EstimatedMinutes, value), true
	case "external_ref":
		return matchesStringPointer(issue.ExternalRef, value), true
	case "started_at":
		return matchesTimePointer(issue.StartedAt, value), true
	case "closed_at":
		return matchesTimePointer(issue.ClosedAt, value), true
	case "due_at":
		return matchesTimePointer(issue.DueAt, value), true
	case "defer_until":
		return matchesTimePointer(issue.DeferUntil, value), true
	default:
		return false, false
	}
}

func matchesString(current string, candidate interface{}) bool {
	switch value := candidate.(type) {
	case nil:
		return current == ""
	case string:
		return current == value
	default:
		return false
	}
}

func matchesInt(current int, candidate interface{}) bool {
	value, ok := candidate.(int)
	return ok && current == value
}

func matchesBool(current bool, candidate interface{}) bool {
	value, ok := candidate.(bool)
	return ok && current == value
}

func matchesStatus(current types.Status, candidate interface{}) bool {
	switch value := candidate.(type) {
	case string:
		return current == types.Status(value)
	case types.Status:
		return current == value
	default:
		return false
	}
}

func matchesIssueType(current types.IssueType, candidate interface{}) bool {
	switch value := candidate.(type) {
	case string:
		return current == types.IssueType(value)
	case types.IssueType:
		return current == value
	default:
		return false
	}
}

func matchesWispType(current types.WispType, candidate interface{}) bool {
	switch value := candidate.(type) {
	case string:
		return current == types.WispType(value)
	case types.WispType:
		return current == value
	default:
		return false
	}
}

func matchesMolType(current types.MolType, candidate interface{}) bool {
	switch value := candidate.(type) {
	case string:
		return current == types.MolType(value)
	case types.MolType:
		return current == value
	default:
		return false
	}
}

func matchesIntPointer(current *int, candidate interface{}) bool {
	switch value := candidate.(type) {
	case nil:
		return current == nil
	case *int:
		return (current == nil && value == nil) || (current != nil && value != nil && *current == *value)
	case int:
		return current != nil && *current == value
	default:
		return false
	}
}

func matchesStringPointer(current *string, candidate interface{}) bool {
	switch value := candidate.(type) {
	case nil:
		return current == nil
	case *string:
		return (current == nil && value == nil) || (current != nil && value != nil && *current == *value)
	case string:
		return current != nil && *current == value
	default:
		return false
	}
}

func matchesTimePointer(current *time.Time, candidate interface{}) bool {
	switch value := candidate.(type) {
	case nil:
		return current == nil
	case *time.Time:
		return (current == nil && value == nil) || (current != nil && value != nil && current.Equal(*value))
	case time.Time:
		return current != nil && current.Equal(value)
	default:
		return false
	}
}

func normalizedMetadata(value json.RawMessage) (string, error) {
	if len(value) == 0 {
		return "{}", nil
	}
	return storage.NormalizeMetadataValue(value)
}

// isMergeOpKey reports whether k is a read-merge-write operation key consumed by
// ResolveMergeOps rather than a concrete column value to pass through unchanged.
func isMergeOpKey(k string) bool {
	switch k {
	case OpMergeMetadata, OpSetMetadata, OpUnsetMetadata, OpAppendNotes:
		return true
	default:
		return false
	}
}

// resolveMetadataMergeOps folds OpMergeMetadata/OpSetMetadata/OpUnsetMetadata
// into a concrete "metadata" value on resolved, using oldIssue.Metadata (read in
// the same mutation transaction) as the base. It is a no-op when no metadata
// operation keys are present.
func resolveMetadataMergeOps(oldIssue *types.Issue, updates, resolved map[string]interface{}) error {
	_, hasMerge := updates[OpMergeMetadata]
	_, hasSet := updates[OpSetMetadata]
	_, hasUnset := updates[OpUnsetMetadata]
	if !hasMerge && !hasSet && !hasUnset {
		return nil
	}
	if _, direct := resolved["metadata"]; direct {
		return fmt.Errorf("cannot combine a metadata replacement with incremental metadata edits")
	}

	current, err := resolveMetadataValue(oldIssue.Metadata, updates, hasMerge, hasSet, hasUnset)
	if err != nil {
		return err
	}
	// Validate the merged result, matching the schema check stores apply to
	// direct metadata replacements (GH#1416 Phase 2).
	if err := ValidateMetadataIfConfigured(current); err != nil {
		return err
	}
	resolved["metadata"] = current
	return nil
}

func resolveMetadataValue(current json.RawMessage, updates map[string]interface{}, hasMerge, hasSet, hasUnset bool) (json.RawMessage, error) {
	if hasMerge {
		merged, err := applyMetadataMergeOp(current, updates[OpMergeMetadata])
		if err != nil {
			return nil, err
		}
		current = merged
	}
	if hasSet || hasUnset {
		merged, err := applyMetadataEditOps(current, updates, hasSet, hasUnset)
		if err != nil {
			return nil, fmt.Errorf("metadata edit failed: %w", err)
		}
		current = merged
	}
	return current, nil
}

func applyMetadataMergeOp(current json.RawMessage, value interface{}) (json.RawMessage, error) {
	normalized, err := storage.NormalizeMetadataValue(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", OpMergeMetadata, err)
	}
	merged, err := storage.MergeMetadataJSON(current, json.RawMessage(normalized))
	if err != nil {
		return nil, fmt.Errorf("metadata merge failed: %w", err)
	}
	return merged, nil
}

func applyMetadataEditOps(current json.RawMessage, updates map[string]interface{}, hasSet, hasUnset bool) (json.RawMessage, error) {
	unset, err := mergeOpStrings(OpUnsetMetadata, updates[OpUnsetMetadata], hasUnset)
	if err != nil {
		return nil, err
	}
	if set, typed := updates[OpSetMetadata].(map[string]json.RawMessage); typed {
		return applyTypedMetadataEdits(current, set, unset)
	}
	set, err := mergeOpStrings(OpSetMetadata, updates[OpSetMetadata], hasSet)
	if err != nil {
		return nil, err
	}
	return storage.ApplyMetadataEdits(current, set, unset)
}

func applyTypedMetadataEdits(existing json.RawMessage, set map[string]json.RawMessage, unset []string) (json.RawMessage, error) {
	data, err := decodeExistingMetadata(existing)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if err := applyTypedMetadataSet(data, set, keys); err != nil {
		return nil, err
	}
	if err := applyTypedMetadataUnset(data, unset); err != nil {
		return nil, err
	}
	result, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal metadata: %w", err)
	}
	return json.RawMessage(result), nil
}

func decodeExistingMetadata(existing json.RawMessage) (map[string]json.RawMessage, error) {
	data := make(map[string]json.RawMessage)
	if len(existing) == 0 {
		return data, nil
	}
	trimmed := strings.TrimSpace(string(existing))
	if trimmed == "" || trimmed == "null" {
		return data, nil
	}
	if err := json.Unmarshal(existing, &data); err != nil {
		return nil, fmt.Errorf("existing metadata is not a JSON object: %w", err)
	}
	return data, nil
}

func applyTypedMetadataSet(data map[string]json.RawMessage, set map[string]json.RawMessage, keys []string) error {
	for _, key := range keys {
		if err := storage.ValidateMetadataKey(key); err != nil {
			return err
		}
		if !json.Valid(set[key]) {
			return fmt.Errorf("metadata value for key %q is not valid JSON", key)
		}
		data[key] = set[key]
	}
	return nil
}

func applyTypedMetadataUnset(data map[string]json.RawMessage, unset []string) error {
	for _, key := range unset {
		if err := storage.ValidateMetadataKey(key); err != nil {
			return err
		}
		delete(data, key)
	}
	return nil
}

// resolveNotesAppendOp folds OpAppendNotes into a concrete "notes" value on
// resolved, appending to oldIssue.Notes (read in the same mutation transaction).
// It is a no-op when the append op is absent.
func resolveNotesAppendOp(oldIssue *types.Issue, updates, resolved map[string]interface{}) error {
	raw, ok := updates[OpAppendNotes]
	if !ok {
		return nil
	}
	if _, direct := resolved["notes"]; direct {
		return fmt.Errorf("%w: cannot combine a notes replacement with %s", storage.ErrValidation, OpAppendNotes)
	}
	text, ok := raw.(string)
	if !ok {
		return fmt.Errorf("%s must be a string, got %T", OpAppendNotes, raw)
	}
	combined := oldIssue.Notes
	if combined != "" {
		combined += "\n"
	}
	combined += text
	resolved["notes"] = combined
	return nil
}

// mergeOpStrings coerces a merge-operation value to []string. Accepts
// []interface{} of strings as well, so operation maps survive a JSON
// round-trip (e.g. daemon transports).
func mergeOpStrings(op string, value interface{}, present bool) ([]string, error) {
	if !present {
		return nil, nil
	}
	switch v := value.(type) {
	case []string:
		return v, nil
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a list of strings, got element %T", op, item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be a list of strings, got %T", op, value)
	}
}

// readIssueAndResolveMergeOps reads the pre-update row in-transaction and folds
// any merge-operation keys (metadata edits, note appends) into concrete column
// values against that row, returning the row and the rewritten update map. It
// keeps the read-merge-write plumbing off updateIssueInTx's already-large body.
func readIssueAndResolveMergeOps(ctx context.Context, tx DBTX, id string, updates map[string]interface{}) (*types.Issue, map[string]interface{}, error) {
	oldIssue, err := GetIssueInTx(ctx, tx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get issue for update: %w", err)
	}
	resolved, err := ResolveMergeOps(oldIssue, updates)
	if err != nil {
		return nil, nil, err
	}
	return oldIssue, resolved, nil
}

// RecordFullEventInTable records an event with both old and new values.
func RecordFullEventInTable(ctx context.Context, tx DBTX, table, issueID string, eventType types.EventType, actor, oldValue, newValue string) error {
	return InsertDerivedEvent(ctx, tx, table, AuxEvent{
		IssueID:   issueID,
		EventType: eventType,
		Actor:     actor,
		OldValue:  str(oldValue),
		NewValue:  str(newValue),
	})
}

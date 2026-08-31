package issueops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// UpdateIssueInTx performs the full update SQL logic within a transaction.
// It routes to the correct table (issues/wisps) automatically.
// The caller is responsible for Dolt versioning (DOLT_ADD/COMMIT) if needed.
//
//nolint:gosec // G201: table names come from WispTableRouting (hardcoded constants)
func UpdateIssueInTx(ctx context.Context, tx DBTX, id string, updates map[string]interface{}, actor string) (*UpdateResult, error) {
	return updateIssueInTx(ctx, tx, id, updates, actor, true)
}

// UpdateIssueWithoutEventInTx applies normal update semantics without recording
// an intermediate event. Demotion uses this to preserve the historical event
// stream: create/update history is copied, then a single demotion event is added.
func UpdateIssueWithoutEventInTx(ctx context.Context, tx DBTX, id string, updates map[string]interface{}, actor string) (*UpdateResult, error) {
	return updateIssueInTx(ctx, tx, id, updates, actor, false)
}

func updateIssueInTx(ctx context.Context, tx DBTX, id string, updates map[string]interface{}, actor string, recordEvent bool) (*UpdateResult, error) {
	updates = cloneUpdateFields(updates)
	forceClosePolicy := PopForceClosePolicy(updates)
	isWisp := IsActiveWispInTx(ctx, tx, id)
	issueTable, _, eventTable, _ := WispTableRouting(isWisp)
	oldIssue, updates, noOp, err := prepareUpdateInput(ctx, tx, id, updates)
	if err != nil {
		return nil, err
	}
	if noOp {
		return &UpdateResult{OldIssue: oldIssue, IsWisp: isWisp, Changed: false}, nil
	}
	statement, err := prepareUpdateStatement(ctx, tx, id, issueTable, oldIssue, updates, forceClosePolicy)
	if err != nil {
		return nil, err
	}
	if err := executeUpdateStatement(ctx, tx, id, statement); err != nil {
		return nil, err
	}
	if err := recordUpdateEvent(ctx, tx, eventTable, id, actor, oldIssue, updates, recordEvent); err != nil {
		return nil, err
	}

	updateResult := &UpdateResult{OldIssue: oldIssue, IsWisp: isWisp, Changed: true, IssueRowsChanged: !isWisp, WispRowsChanged: isWisp}
	if err := recomputeUpdateStatus(ctx, tx, id, isWisp, oldIssue, updates, updateResult); err != nil {
		return nil, err
	}
	if err := RecordEventInTx(ctx, tx, EventUpdate, id); err != nil {
		return nil, err
	}
	return updateResult, nil
}

func prepareUpdateInput(ctx context.Context, tx DBTX, id string, updates map[string]interface{}) (*types.Issue, map[string]interface{}, bool, error) {
	oldIssue, resolved, err := readIssueAndResolveMergeOps(ctx, tx, id, updates)
	if err != nil {
		return nil, nil, false, err
	}
	if err := ValidateClosedAtCoherence(oldIssue, resolved); err != nil {
		return nil, nil, false, err
	}
	filtered, err := DiscardNoopIssueUpdates(oldIssue, resolved)
	if err != nil {
		return nil, nil, false, err
	}
	return oldIssue, filtered, len(filtered) == 0, nil
}

type preparedUpdateStatement struct {
	query      string
	args       []interface{}
	clearLease bool
}

func prepareUpdateStatement(ctx context.Context, tx DBTX, id, issueTable string, oldIssue *types.Issue, updates map[string]interface{}, forceClosePolicy bool) (*preparedUpdateStatement, error) {
	crossing, err := CrossesIntoDoneCategoryInTx(ctx, tx, oldIssue.Status, updates)
	if err != nil {
		return nil, err
	}
	if crossing {
		if _, err := EnforceClosePolicyInTx(ctx, tx, id, forceClosePolicy); err != nil {
			return nil, err
		}
	}
	if err := ValidateScalarUpdates(ctx, tx, updates); err != nil {
		return nil, err
	}
	setClauses, args, err := concreteUpdateClauses(updates)
	if err != nil {
		return nil, err
	}
	setClauses, args = appendPinnedClear(oldIssue, updates, setClauses, args)
	setClauses, args = ManageClosedAt(oldIssue, updates, setClauses, args)
	setClauses, args = ManageStartedAt(oldIssue, updates, setClauses, args)
	clearLease := ManageLeaseOnUpdate(oldIssue, updates)
	setClauses = append(setClauses, "row_lock = ?")
	args = append(args, freshRowLock(), id)
	//nolint:gosec // G201: issueTable comes from WispTableRouting (hardcoded constants)
	return &preparedUpdateStatement{
		query:      fmt.Sprintf("UPDATE %s SET %s WHERE id = ?", issueTable, strings.Join(setClauses, ", ")),
		args:       args,
		clearLease: clearLease,
	}, nil
}

func concreteUpdateClauses(updates map[string]interface{}) ([]string, []interface{}, error) {
	setClauses := []string{"updated_at = ?"}
	args := []interface{}{time.Now().UTC()}
	for key, value := range updates {
		if !IsAllowedUpdateField(key) {
			return nil, nil, fmt.Errorf("invalid field for update: %s", key)
		}
		columnName := key
		if key == "wisp" {
			columnName = "ephemeral"
		}
		setClauses = append(setClauses, fmt.Sprintf("`%s` = ?", columnName))
		serialized, err := serializeUpdateValue(key, value)
		if err != nil {
			return nil, nil, err
		}
		args = append(args, serialized)
	}
	return setClauses, args, nil
}

func serializeUpdateValue(key string, value interface{}) (interface{}, error) {
	switch key {
	case "waiters":
		waitersJSON, _ := json.Marshal(value)
		return string(waitersJSON), nil
	case "metadata":
		metadataStr, err := storage.NormalizeMetadataValue(value)
		if err != nil {
			return nil, fmt.Errorf("invalid metadata: %w", err)
		}
		return metadataStr, nil
	default:
		return value, nil
	}
}

func appendPinnedClear(oldIssue *types.Issue, updates map[string]interface{}, setClauses []string, args []interface{}) ([]string, []interface{}) {
	status, ok := updateStatusString(updates)
	if !ok || !oldIssue.Pinned || status == string(types.StatusPinned) {
		return setClauses, args
	}
	if _, alreadySet := updates["pinned"]; alreadySet {
		return setClauses, args
	}
	return append(setClauses, "`pinned` = ?"), append(args, false)
}

func updateStatusString(updates map[string]interface{}) (string, bool) {
	rawStatus, ok := updates["status"]
	if !ok {
		return "", false
	}
	switch value := rawStatus.(type) {
	case string:
		return value, true
	case types.Status:
		return string(value), true
	default:
		return "", true
	}
}

func executeUpdateStatement(ctx context.Context, tx DBTX, id string, statement *preparedUpdateStatement) error {
	if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
		return fmt.Errorf("failed to update issue: %w", err)
	}
	if statement.clearLease {
		if err := DeleteLeaseInTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func recordUpdateEvent(ctx context.Context, tx DBTX, eventTable, id, actor string, oldIssue *types.Issue, updates map[string]interface{}, recordEvent bool) error {
	if !recordEvent {
		return nil
	}
	oldData, _ := json.Marshal(oldIssue)
	newData, _ := json.Marshal(updates)
	eventType := DetermineEventType(oldIssue, updates)
	if err := RecordFullEventInTable(ctx, tx, eventTable, id, eventType, actor, string(oldData), string(newData)); err != nil {
		return fmt.Errorf("failed to record event: %w", err)
	}
	return nil
}

func recomputeUpdateStatus(ctx context.Context, tx DBTX, id string, isWisp bool, oldIssue *types.Issue, updates map[string]interface{}, result *UpdateResult) error {
	newStatus, hasStatus := updateStatusString(updates)
	if !hasStatus {
		return nil
	}
	oldActive := oldIssue.Status != types.StatusClosed && oldIssue.Status != types.StatusPinned
	newActive := newStatus != string(types.StatusClosed) && newStatus != string(types.StatusPinned)
	if oldActive == newActive {
		return nil
	}
	affectedIssues, affectedWisps, err := affectedByUpdateStatus(ctx, tx, id, isWisp)
	if err != nil {
		return fmt.Errorf("affected by status change for %s: %w", id, err)
	}
	recompute, err := RecomputeIsBlockedInTxWithResult(ctx, tx, affectedIssues, affectedWisps)
	if err != nil {
		return fmt.Errorf("recompute is_blocked after status change for %s: %w", id, err)
	}
	result.IssueRowsChanged = !isWisp || recompute.IssueRowsChanged
	result.WispRowsChanged = isWisp || recompute.WispRowsChanged
	return nil
}

func affectedByUpdateStatus(ctx context.Context, tx DBTX, id string, isWisp bool) ([]string, []string, error) {
	if isWisp {
		return AffectedByStatusChangeForWispInTx(ctx, tx, id)
	}
	return AffectedByStatusChangeInTx(ctx, tx, id)
}

func cloneUpdateFields(updates map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(updates))
	for key, value := range updates {
		cloned[key] = value
	}
	return cloned
}

// Merge-operation update keys. Unlike plain column updates, these are resolved
// against the current row INSIDE the mutation transaction: re-read, merge,
// write, commit. Callers pass the operation (keys to set/unset, text to
// append) instead of a pre-merged value, so concurrent writers touching
// different keys of the same issue cannot erase each other.
const (
	// OpMergeMetadata merges a JSON object's top-level keys into the issue's
	// metadata (bd update --metadata). Value: string, []byte, or json.RawMessage.
	OpMergeMetadata = "_merge_metadata"
	// OpSetMetadata sets individual metadata entries. CLI callers use []string
	// key=value values; public issue operations use map[string]json.RawMessage.
	OpSetMetadata = "_set_metadata"
	// OpUnsetMetadata removes metadata keys (bd update --unset-metadata).
	// Value: []string.
	OpUnsetMetadata = "_unset_metadata"
	// OpAppendNotes appends a line to the issue's notes
	// (bd update --append-notes). Value: string.
	OpAppendNotes = "append_notes"
)

// OpForceClosePolicy carries a close-policy override into a generic update
// (bd update --force). Value: bool. It is not a merge operation and not a
// column — the write funnels pop it before validating fields, so it never
// reaches SQL.
//
// It rides the update map, like the merge operations, so adding the override
// breaks no interface. That choice has a deliberate failure mode: the field
// allowlists do not name this key, so an occurrence that reaches field
// validation unpopped is refused by name. A caller that misspells the override,
// or a write path that forgets to pop it, fails loudly instead of quietly
// running the update with force read as off.
const OpForceClosePolicy = "_force_close_policy"

// PopForceClosePolicy removes the close-policy override from updates and
// reports whether it asked for force. A value that is not a bool is left in
// place on purpose: it cannot be read as an intent, and leaving it lets the
// field allowlist refuse it by name.
func PopForceClosePolicy(updates map[string]interface{}) bool {
	raw, present := updates[OpForceClosePolicy]
	if !present {
		return false
	}
	force, isBool := raw.(bool)
	if !isBool {
		return false
	}
	delete(updates, OpForceClosePolicy)
	return force
}

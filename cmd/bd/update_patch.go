package main

import (
	"encoding/json"
	"fmt"

	storageissueops "github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/issueops"
)

type updatePersistFlags struct {
	wisp      *bool
	noHistory *bool
}

// buildUpdatePatch reshapes the flag-derived update map into the facade's typed
// patch. Every key it handles is a key the flag parsing above wrote, so an
// unrecognized one is a bug in this file rather than user input.
func buildUpdatePatch(updates map[string]interface{}) (issueops.IssuePatch, error) {
	var patch issueops.IssuePatch
	var persist updatePersistFlags
	for key, value := range updates {
		if err := applyUpdatePatchField(key, value, &patch, &persist); err != nil {
			return issueops.IssuePatch{}, err
		}
	}
	applyUpdatePersistence(&patch, persist)
	return patch, nil
}

func applyUpdatePatchField(key string, value any, patch *issueops.IssuePatch, persist *updatePersistFlags) error {
	if handled, ok := applyUpdateContentField(key, value, patch); handled {
		return updateFieldTypeError(ok, key, value)
	}
	if handled, ok := applyUpdateLinkField(key, value, patch); handled {
		return updateFieldTypeError(ok, key, value)
	}
	if handled, ok := applyUpdateTypedField(key, value, patch); handled {
		return updateFieldTypeError(ok, key, value)
	}
	if handled, ok := applyUpdateScheduleField(key, value, patch); handled {
		return updateFieldTypeError(ok, key, value)
	}
	if handled, err := applyUpdateMetaField(key, value, patch, persist); handled {
		return err
	}
	return fmt.Errorf("unsupported update field %q", key)
}

func updateFieldTypeError(ok bool, key string, value any) error {
	if ok {
		return nil
	}
	return fmt.Errorf("unsupported value %T for update field %q", value, key)
}

func applyUpdateContentField(key string, value any, patch *issueops.IssuePatch) (handled, ok bool) {
	switch key {
	case "title":
		patch.Title, ok = stringField(value)
	case "description":
		patch.Description, ok = stringField(value)
	case "design":
		patch.Design, ok = stringField(value)
	case "acceptance_criteria":
		patch.AcceptanceCriteria, ok = stringField(value)
	case "notes":
		patch.Notes, ok = stringField(value)
	case storageissueops.OpAppendNotes:
		patch.AppendNotes, ok = stringField(value)
	default:
		return false, false
	}
	return true, ok
}

func applyUpdateLinkField(key string, value any, patch *issueops.IssuePatch) (handled, ok bool) {
	switch key {
	case "spec_id":
		patch.SpecID, ok = stringField(value)
	case "await_id":
		patch.AwaitID, ok = stringField(value)
	case "closed_by_session":
		patch.ClosedBySession, ok = stringField(value)
	case "assignee":
		patch.Assignee, ok = stringField(value)
	case "parent":
		patch.ParentID, ok = stringField(value)
	default:
		return false, false
	}
	return true, ok
}

func applyUpdateTypedField(key string, value any, patch *issueops.IssuePatch) (handled, ok bool) {
	switch key {
	case "status":
		return true, applyUpdateStatusField(value, patch)
	case "issue_type":
		return true, applyUpdateIssueTypeField(value, patch)
	case "priority":
		return true, applyUpdatePriorityField(value, patch)
	case "estimated_minutes":
		return true, applyUpdateEstimateField(value, patch)
	case "external_ref":
		return true, applyUpdateExternalRefField(value, patch)
	}
	return false, false
}

func applyUpdateStatusField(value any, patch *issueops.IssuePatch) bool {
	status, ok := stringField(value)
	if !ok {
		return false
	}
	patch.Status = setField(issueops.Status(status.Value))
	return true
}

func applyUpdateIssueTypeField(value any, patch *issueops.IssuePatch) bool {
	issueType, ok := stringField(value)
	if !ok {
		return false
	}
	patch.IssueType = setField(issueops.IssueType(issueType.Value))
	return true
}

func applyUpdatePriorityField(value any, patch *issueops.IssuePatch) bool {
	priority, ok := value.(int)
	if !ok {
		return false
	}
	patch.Priority = setField(priority)
	return true
}

func applyUpdateEstimateField(value any, patch *issueops.IssuePatch) bool {
	estimate, ok := value.(int)
	if !ok {
		return false
	}
	patch.EstimatedMinutes = setField(&estimate)
	return true
}

func applyUpdateExternalRefField(value any, patch *issueops.IssuePatch) bool {
	if value == nil {
		patch.ExternalRef = setField[*string](nil)
		return true
	}
	ref, ok := value.(string)
	if !ok {
		return false
	}
	patch.ExternalRef = setField(&ref)
	return true
}

func applyUpdateScheduleField(key string, value any, patch *issueops.IssuePatch) (handled, ok bool) {
	switch key {
	case "due_at":
		patch.DueAt, ok = optionalTimeField(value)
	case "defer_until":
		patch.DeferUntil, ok = optionalTimeField(value)
	case "add_labels":
		patch.Labels.Add, ok = value.([]string)
	case "remove_labels":
		patch.Labels.Remove, ok = value.([]string)
	case "set_labels":
		return true, applyUpdateSetLabelsField(value, patch)
	default:
		return false, false
	}
	return true, ok
}

func applyUpdateSetLabelsField(value any, patch *issueops.IssuePatch) bool {
	labels, ok := value.([]string)
	if !ok {
		return false
	}
	patch.Labels.Replace = setField(labels)
	return true
}

func applyUpdateMetaField(key string, value any, patch *issueops.IssuePatch, persist *updatePersistFlags) (bool, error) {
	switch key {
	case storageissueops.OpMergeMetadata:
		return true, applyUpdateMergeMetadata(value, patch)
	case storageissueops.OpSetMetadata:
		return true, applyUpdateSetMetadata(value, patch)
	case storageissueops.OpUnsetMetadata:
		ok := false
		patch.Metadata.Unset, ok = value.([]string)
		return true, updateFieldTypeError(ok, key, value)
	case "wisp":
		return true, applyUpdateBoolFlag(value, &persist.wisp, key)
	case "no_history":
		return true, applyUpdateBoolFlag(value, &persist.noHistory, key)
	}
	return false, nil
}

func applyUpdateMergeMetadata(value any, patch *issueops.IssuePatch) error {
	merge, ok := value.(json.RawMessage)
	if !ok {
		return fmt.Errorf("unsupported value %T for update field %q", value, storageissueops.OpMergeMetadata)
	}
	patch.Metadata.Merge = setField(merge)
	return nil
}

func applyUpdateSetMetadata(value any, patch *issueops.IssuePatch) error {
	flags, ok := value.([]string)
	if !ok {
		return fmt.Errorf("unsupported value %T for update field %q", value, storageissueops.OpSetMetadata)
	}
	set, err := parseSetMetadataFlags(flags)
	if err != nil {
		return err
	}
	patch.Metadata.Set = set
	return nil
}

func applyUpdateBoolFlag(value any, dest **bool, key string) error {
	flag, ok := value.(bool)
	if !ok {
		return fmt.Errorf("unsupported value %T for update field %q", value, key)
	}
	*dest = &flag
	return nil
}

func applyUpdatePersistence(patch *issueops.IssuePatch, persist updatePersistFlags) {
	// --ephemeral/--persistent/--no-history/--history select one complete
	// persistence state. The flag parsing already rejects the contradictory
	// pairs; the remaining combinations resolve most-specific-first, which
	// reproduces the column pairs the old two-boolean write produced.
	switch {
	case persist.wisp != nil && *persist.wisp:
		patch.Persistence = setField(issueops.PersistenceModeEphemeral)
	case persist.noHistory != nil && *persist.noHistory:
		patch.Persistence = setField(issueops.PersistenceModeNoHistory)
	case persist.wisp != nil || persist.noHistory != nil:
		patch.Persistence = setField(issueops.PersistenceModePersistent)
	}
}

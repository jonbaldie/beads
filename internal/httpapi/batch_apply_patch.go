package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/httpapi/apigen"
	"github.com/jonbaldie/beads/issueops"
)

// applyUpdateItem projects an update payload onto the role's update item.
func applyUpdateItem(prefix string, encoded json.RawMessage, raw map[string]json.RawMessage) (*issueops.UpdateItem, *Result) {
	if offender, unknown := unknownMember(raw, applyUpdateItemMembers); unknown {
		return nil, applyUnknownMember(prefix, offender, applyUpdateItemMembers)
	}
	var wire apigen.ApplyUpdateItem
	if err := json.Unmarshal(encoded, &wire); err != nil {
		res := InvalidArgument(applyParam(prefix, ""), ReasonInvalidValue, "an `update` member carries the wrong JSON type")
		return nil, &res
	}
	target, res := applyRequiredRef(prefix, raw, "target")
	if res != nil {
		return nil, res
	}
	for _, bounded := range []struct {
		member string
		value  *string
	}{{"expected_status", wire.ExpectedStatus}, {"expected_assignee", wire.ExpectedAssignee}} {
		if res := applyBoundedText(prefix, bounded.member, bounded.value); res != nil {
			return nil, res
		}
	}
	patchRaw, res := applyObjectMember(raw, prefix, "patch")
	if res != nil {
		return nil, res
	}
	patch, res := applyPatch(prefix+"patch.", raw["patch"], patchRaw)
	if res != nil {
		return nil, res
	}

	return buildApplyUpdateItem(wire, target, patch), nil
}

func buildApplyUpdateItem(wire apigen.ApplyUpdateItem, target issueops.Ref, patch issueops.IssuePatch) *issueops.UpdateItem {
	item := &issueops.UpdateItem{Target: target, Patch: patch, ExpectedVersion: wire.ExpectedVersion}
	if wire.ExpectedStatus != nil {
		status := issueops.Status(*wire.ExpectedStatus)
		item.ExpectedStatus = &status
	}
	item.ExpectedAssignee = wire.ExpectedAssignee
	if wire.ForceClosePolicy != nil {
		item.ForceClosePolicy = *wire.ForceClosePolicy
	}
	if wire.ForceAssigneeTransfer != nil {
		item.ForceAssigneeTransfer = *wire.ForceAssigneeTransfer
	}
	return item
}

// applyPatch projects the decoded `patch` member onto the role's IssuePatch.
//
// MEMBER PRESENCE IS THE SIGNAL, which is why this level is read as raw members
// too: a member present sets the role's Field, a member absent leaves the field
// untouched, and the generated struct cannot tell those apart because it models
// both as a nil pointer. Explicit `null` is a third state read straight off the
// raw bytes — a clear on the four nullable members, and a 400 naming the member
// everywhere else.
func applyPatch(prefix string, encoded json.RawMessage, raw map[string]json.RawMessage) (issueops.IssuePatch, *Result) {
	if res := validateApplyPatchInput(prefix, raw); res != nil {
		return issueops.IssuePatch{}, res
	}
	wire, res := decodeApplyPatchBody(prefix, encoded)
	if res != nil {
		return issueops.IssuePatch{}, res
	}
	if res := validateApplyPatchBoundedText(prefix, wire); res != nil {
		return issueops.IssuePatch{}, res
	}

	patch := issueops.IssuePatch{}
	if res := applyPatchTitleAndCore(prefix, raw, wire, &patch); res != nil {
		return issueops.IssuePatch{}, res
	}
	if res := applyPatchNotes(prefix, raw, wire, &patch); res != nil {
		return issueops.IssuePatch{}, res
	}
	if res := applyPatchScalarFields(prefix, raw, wire, &patch); res != nil {
		return issueops.IssuePatch{}, res
	}
	applyPatchNullableFields(raw, wire, &patch)
	if res := applyPatchStructuredFields(prefix, raw, &patch); res != nil {
		return issueops.IssuePatch{}, res
	}
	return patch, nil
}

func validateApplyPatchInput(prefix string, raw map[string]json.RawMessage) *Result {
	refuse := func(member, detail string) *Result {
		res := InvalidArgument(applyParam(prefix, member), ReasonInvalidValue, detail)
		return &res
	}
	if len(raw) == 0 {
		// A write that writes nothing is a client bug, not a no-op to answer.
		return refuse("", "`patch` must carry at least one field; an update that updates nothing is refused rather than answered")
	}
	if offender, unknown := unknownMember(raw, applyPatchMembers); unknown {
		return applyUnknownMember(prefix, offender, applyPatchMembers)
	}
	// Explicit null, before any typed decode: unmarshaling null into *T yields
	// nil, which is indistinguishable from the member being absent, so a null on
	// a non-nullable member would otherwise slide through as "untouched" — a
	// write the client asked for and the server silently dropped.
	for name, value := range raw {
		if isJSONNull(value) && !applyNullablePatchMembers[name] {
			return refuse(name, "`"+name+"` is not nullable; omit it to leave the field unchanged")
		}
	}
	return nil
}

func decodeApplyPatchBody(prefix string, encoded json.RawMessage) (apigen.ApplyPatchBody, *Result) {
	var wire apigen.ApplyPatchBody
	if err := json.Unmarshal(encoded, &wire); err != nil {
		res := InvalidArgument(applyParam(prefix, ""), ReasonInvalidValue, "a `patch` member carries the wrong JSON type")
		return apigen.ApplyPatchBody{}, &res
	}
	return wire, nil
}

func validateApplyPatchBoundedText(prefix string, wire apigen.ApplyPatchBody) *Result {
	for _, bounded := range []struct {
		member string
		value  *string
	}{
		{"issue_type", wire.IssueType}, {"status", wire.Status},
		{"assignee", wire.Assignee}, {"owner", wire.Owner},
		{"external_ref", wire.ExternalRef},
	} {
		if res := applyBoundedText(prefix, bounded.member, bounded.value); res != nil {
			return res
		}
	}
	return nil
}

func applyPatchTitleAndCore(prefix string, raw map[string]json.RawMessage, wire apigen.ApplyPatchBody, patch *issueops.IssuePatch) *Result {
	if _, present := raw["title"]; present {
		title := *wire.Title
		if strings.TrimSpace(title) == "" {
			res := InvalidArgument(applyParam(prefix, "title"), ReasonInvalidValue, "`title` must not be blank")
			return &res
		}
		if res := applyBoundedText(prefix, "title", &title); res != nil {
			return res
		}
		patch.Title = issueops.Field[string]{Set: true, Value: title}
	}
	if _, present := raw["description"]; present {
		patch.Description = issueops.Field[string]{Set: true, Value: *wire.Description}
	}
	if _, present := raw["design"]; present {
		patch.Design = issueops.Field[string]{Set: true, Value: *wire.Design}
	}
	if _, present := raw["acceptance_criteria"]; present {
		patch.AcceptanceCriteria = issueops.Field[string]{Set: true, Value: *wire.AcceptanceCriteria}
	}
	return nil
}

func applyPatchNotes(prefix string, raw map[string]json.RawMessage, wire apigen.ApplyPatchBody, patch *issueops.IssuePatch) *Result {
	_, hasNotes := raw["notes"]
	_, hasAppendNotes := raw["append_notes"]
	// The role refuses both together too; refusing here keeps the 400 a
	// statement about the request rather than a translated storage error.
	if hasNotes && hasAppendNotes {
		res := InvalidArgument(applyParam(prefix, "append_notes"), ReasonInvalidValue, "`notes` replaces the notes and `append_notes` adds to them; send one")
		return &res
	}
	if hasNotes {
		patch.Notes = issueops.Field[string]{Set: true, Value: *wire.Notes}
	}
	if hasAppendNotes {
		patch.AppendNotes = issueops.Field[string]{Set: true, Value: *wire.AppendNotes}
	}
	return nil
}

func applyPatchScalarFields(prefix string, raw map[string]json.RawMessage, wire apigen.ApplyPatchBody, patch *issueops.IssuePatch) *Result {
	if _, present := raw["priority"]; present {
		priority := *wire.Priority
		if priority < 0 || priority > 4 {
			res := InvalidArgument(applyParam(prefix, "priority"), ReasonInvalidValue, fmt.Sprintf("`priority` is %d; the range is 0 to 4", priority))
			return &res
		}
		patch.Priority = issueops.Field[int]{Set: true, Value: priority}
	}
	if _, present := raw["issue_type"]; present {
		patch.IssueType = issueops.Field[issueops.IssueType]{Set: true, Value: issueops.IssueType(*wire.IssueType)}
	}
	if _, present := raw["status"]; present {
		patch.Status = issueops.Field[issueops.Status]{Set: true, Value: issueops.Status(*wire.Status)}
	}
	if _, present := raw["assignee"]; present {
		patch.Assignee = issueops.Field[string]{Set: true, Value: *wire.Assignee}
	}
	if _, present := raw["owner"]; present {
		patch.Owner = issueops.Field[string]{Set: true, Value: *wire.Owner}
	}
	return nil
}

func applyPatchNullableFields(raw map[string]json.RawMessage, wire apigen.ApplyPatchBody, patch *issueops.IssuePatch) {
	// The four nullable members. Set is true whenever the member is present; the
	// VALUE is a nil pointer for an explicit null, which is how a clear reaches
	// the role.
	if _, present := raw["estimated_minutes"]; present {
		patch.EstimatedMinutes = issueops.Field[*int]{Set: true, Value: wire.EstimatedMinutes}
	}
	if _, present := raw["external_ref"]; present {
		patch.ExternalRef = issueops.Field[*string]{Set: true, Value: wire.ExternalRef}
	}
	if _, present := raw["due_at"]; present {
		patch.DueAt = issueops.Field[*time.Time]{Set: true, Value: wire.DueAt}
	}
	if _, present := raw["defer_until"]; present {
		patch.DeferUntil = issueops.Field[*time.Time]{Set: true, Value: wire.DeferUntil}
	}
}

func applyPatchStructuredFields(prefix string, raw map[string]json.RawMessage, patch *issueops.IssuePatch) *Result {
	if _, present := raw["labels"]; present {
		labels, res := applyLabelPatch(prefix+"labels.", raw["labels"])
		if res != nil {
			return res
		}
		patch.Labels = labels
	}
	if _, present := raw["metadata"]; present {
		metadata, res := applyMetadataPatch(prefix+"metadata.", raw["metadata"])
		if res != nil {
			return res
		}
		patch.Metadata = metadata
	}
	return nil
}

// applyLabelPatch projects the label edit, which is the FULL patch here rather
// than PATCH /v0/beads/issues/{id}'s complete replacement: a plan edits a label
// set it did not compose, and replacement would mean reading it back first.
func applyLabelPatch(prefix string, encoded json.RawMessage) (issueops.LabelPatch, *Result) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil || raw == nil {
		res := InvalidArgument(applyParam(prefix, ""), ReasonInvalidValue, "`labels` must be a JSON object")
		return issueops.LabelPatch{}, &res
	}
	if offender, unknown := unknownMember(raw, applyLabelPatchMembers); unknown {
		return issueops.LabelPatch{}, applyUnknownMember(prefix, offender, applyLabelPatchMembers)
	}
	var wire apigen.ApplyLabelPatch
	if err := json.Unmarshal(encoded, &wire); err != nil {
		res := InvalidArgument(applyParam(prefix, ""), ReasonInvalidValue, "a `labels` member carries the wrong JSON type")
		return issueops.LabelPatch{}, &res
	}

	patch := issueops.LabelPatch{}
	for _, edit := range []struct {
		member string
		value  *[]string
		assign func([]string)
	}{
		{"replace", wire.Replace, func(v []string) {
			// Presence is the signal, and an EMPTY array clears every label —
			// which is why this is a Field rather than a plain slice.
			patch.Replace = issueops.Field[[]string]{Set: true, Value: v}
		}},
		{"add", wire.Add, func(v []string) { patch.Add = v }},
		{"remove", wire.Remove, func(v []string) { patch.Remove = v }},
	} {
		if _, present := raw[edit.member]; !present {
			continue
		}
		if edit.value == nil {
			res := InvalidArgument(prefix+edit.member, ReasonInvalidValue, "`"+edit.member+"` must be an array of strings")
			return issueops.LabelPatch{}, &res
		}
		if res := applyBoundedLabels(prefix, edit.member, *edit.value); res != nil {
			return issueops.LabelPatch{}, res
		}
		edit.assign(*edit.value)
	}
	return patch, nil
}

// applyMetadataPatch projects the metadata edit.
//
// `set` IS READ RAW AND NEVER THROUGH THE GENERATED MAP, and that is a
// correctness fix rather than a shortcut: the generator models a map value as
// *MetadataValue, and encoding/json answers a JSON null against a pointer by
// setting the pointer to nil before any UnmarshalJSON runs — so a caller
// writing a key to JSON null would have it collapse into an absent key, which
// on this plane is the opposite request. The raw bytes carry the literal.
func applyMetadataPatch(prefix string, encoded json.RawMessage) (issueops.MetadataPatch, *Result) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil || raw == nil {
		res := InvalidArgument(applyParam(prefix, ""), ReasonInvalidValue, "`metadata` must be a JSON object")
		return issueops.MetadataPatch{}, &res
	}
	if offender, unknown := unknownMember(raw, applyMetadataPatchMembers); unknown {
		return issueops.MetadataPatch{}, applyUnknownMember(prefix, offender, applyMetadataPatchMembers)
	}

	patch := issueops.MetadataPatch{}
	if res := applyMetadataReplace(prefix, raw, &patch); res != nil {
		return issueops.MetadataPatch{}, res
	}
	applyMetadataMerge(raw, &patch)
	if res := applyMetadataSet(prefix, raw, &patch); res != nil {
		return issueops.MetadataPatch{}, res
	}
	if res := applyMetadataUnset(prefix, raw, &patch); res != nil {
		return issueops.MetadataPatch{}, res
	}
	return patch, nil
}

func applyMetadataReplace(prefix string, raw map[string]json.RawMessage, patch *issueops.MetadataPatch) *Result {
	value, present := raw["replace"]
	if !present {
		return nil
	}
	if len(raw) > 1 {
		res := InvalidArgument(prefix+"replace", ReasonInvalidValue,
			"`replace` writes the whole document and cannot be combined with `merge`, `set` or `unset`")
		return &res
	}
	patch.Replace = issueops.Field[json.RawMessage]{Set: true, Value: applyRawCopy(value)}
	return nil
}

func applyMetadataMerge(raw map[string]json.RawMessage, patch *issueops.MetadataPatch) {
	value, present := raw["merge"]
	if present {
		patch.Merge = issueops.Field[json.RawMessage]{Set: true, Value: applyRawCopy(value)}
	}
}

func applyMetadataSet(prefix string, raw map[string]json.RawMessage, patch *issueops.MetadataPatch) *Result {
	value, present := raw["set"]
	if !present {
		return nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(value, &entries); err != nil || entries == nil {
		res := InvalidArgument(prefix+"set", ReasonInvalidValue, "`set` must be an object of metadata values")
		return &res
	}
	patch.Set = make(map[string]json.RawMessage, len(entries))
	for key, keyValue := range entries {
		patch.Set[key] = applyRawCopy(keyValue)
	}
	return nil
}

func applyMetadataUnset(prefix string, raw map[string]json.RawMessage, patch *issueops.MetadataPatch) *Result {
	value, present := raw["unset"]
	if !present {
		return nil
	}
	var keys []string
	if err := json.Unmarshal(value, &keys); err != nil || keys == nil {
		res := InvalidArgument(prefix+"unset", ReasonInvalidValue, "`unset` must be an array of strings")
		return &res
	}
	patch.Unset = keys
	return nil
}

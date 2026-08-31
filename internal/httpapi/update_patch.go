package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonbaldie/beads/internal/httpapi/apigen"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/issueops"
)

// issuePatch projects the decoded `patch` member onto the role's IssuePatch.
//
// MEMBER PRESENCE IS THE SIGNAL, which is the whole reason the body is decoded
// as raw members: a member present sets the role's Field, a member absent
// leaves the field untouched, and the generated struct cannot tell those apart
// because it models both as a nil pointer. Explicit `null` is a third state
// this reads directly off the raw bytes — a clear on the four nullable members,
// and a 400 naming the member everywhere else.
func (s *Server) issuePatch(w http.ResponseWriter, r *http.Request, id string, members map[string]json.RawMessage) (issueops.IssuePatch, bool) {
	input, offender, res := readIssuePatch(members)
	if res != nil {
		s.fail(w, r, *res)
		return issueops.IssuePatch{}, false
	}
	if offender != "" {
		s.failUnknownMember(w, r, patchParam(offender), issuePatchMembers)
		return issueops.IssuePatch{}, false
	}
	patch, res := buildIssuePatch(input, id)
	if res != nil {
		s.fail(w, r, *res)
		return issueops.IssuePatch{}, false
	}
	return patch, true
}

type issuePatchInput struct {
	fields map[string]json.RawMessage
	wire   apigen.IssuePatchBody
}

func readIssuePatch(members map[string]json.RawMessage) (issuePatchInput, string, *Result) {
	raw, ok := members[updatePatchMember]
	if !ok {
		return issuePatchInput{}, "", issuePatchInvalid("", "`"+updatePatchMember+"` is required")
	}
	fields, offender, res := readIssuePatchFields(raw)
	if res != nil || offender != "" {
		return issuePatchInput{}, offender, res
	}

	var wire apigen.IssuePatchBody
	if err := json.Unmarshal(raw, &wire); err != nil {
		return issuePatchInput{}, "", issuePatchInvalid("", "a `"+updatePatchMember+"` member carries the wrong JSON type")
	}
	return issuePatchInput{fields: fields, wire: wire}, "", nil
}

func readIssuePatchFields(raw json.RawMessage) (map[string]json.RawMessage, string, *Result) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, "", issuePatchInvalid("", "`"+updatePatchMember+"` must be a JSON object")
	}
	if len(fields) == 0 {
		return nil, "", issuePatchInvalid("", "`"+updatePatchMember+"` must carry at least one field; an update that updates nothing is refused rather than answered")
	}
	if offender, unknown := unknownMember(fields, issuePatchMembers); unknown {
		return nil, offender, nil
	}
	for name, value := range fields {
		if isJSONNull(value) && !nullablePatchMembers[name] {
			return nil, "", issuePatchInvalid(name, "`"+name+"` is not nullable; omit it to leave the field unchanged")
		}
	}
	return fields, "", nil
}

func issuePatchInvalid(member, detail string) *Result {
	res := InvalidArgument(patchParam(member), ReasonInvalidValue, detail)
	return &res
}

func buildIssuePatch(input issuePatchInput, id string) (issueops.IssuePatch, *Result) {
	patch := issueops.IssuePatch{}
	if res := applyIssuePatchBasicFields(input.fields, input.wire, &patch); res != nil {
		return issueops.IssuePatch{}, res
	}
	if res := applyIssuePatchNotes(input.fields, input.wire, &patch); res != nil {
		return issueops.IssuePatch{}, res
	}
	if res := applyIssuePatchPriority(input.fields, input.wire, &patch); res != nil {
		return issueops.IssuePatch{}, res
	}
	if res := applyIssuePatchBoundedFields(input.fields, input.wire, id, &patch); res != nil {
		return issueops.IssuePatch{}, res
	}
	if res := applyIssuePatchMetadata(input.fields, &patch); res != nil {
		return issueops.IssuePatch{}, res
	}
	if res := applyIssuePatchLabels(input.fields, input.wire, &patch); res != nil {
		return issueops.IssuePatch{}, res
	}
	if res := applyIssuePatchNullable(input.fields, input.wire, &patch); res != nil {
		return issueops.IssuePatch{}, res
	}
	return patch, nil
}

func hasPatchMember(fields map[string]json.RawMessage, name string) bool {
	_, present := fields[name]
	return present
}

func applyIssuePatchBasicFields(fields map[string]json.RawMessage, wire apigen.IssuePatchBody, patch *issueops.IssuePatch) *Result {
	if hasPatchMember(fields, "title") {
		title := *wire.Title
		if strings.TrimSpace(title) == "" {
			return issuePatchInvalid("title", "`title` must not be blank")
		}
		if types.CheckFieldLen("title", title) != nil {
			return issuePatchInvalid("title", fmt.Sprintf("`title` is %d characters; storage holds at most %d",
				utf8.RuneCountInString(title), types.MaxFieldLen))
		}
		patch.Title = issueops.Field[string]{Set: true, Value: title}
	}
	if hasPatchMember(fields, "description") {
		patch.Description = issueops.Field[string]{Set: true, Value: *wire.Description}
	}
	if hasPatchMember(fields, "design") {
		patch.Design = issueops.Field[string]{Set: true, Value: *wire.Design}
	}
	if hasPatchMember(fields, "acceptance_criteria") {
		patch.AcceptanceCriteria = issueops.Field[string]{Set: true, Value: *wire.AcceptanceCriteria}
	}
	return nil
}

func applyIssuePatchNotes(fields map[string]json.RawMessage, wire apigen.IssuePatchBody, patch *issueops.IssuePatch) *Result {
	if hasPatchMember(fields, "notes") && hasPatchMember(fields, "append_notes") {
		return issuePatchInvalid("append_notes", "`notes` replaces the notes and `append_notes` adds to them; send one")
	}
	if hasPatchMember(fields, "notes") {
		patch.Notes = issueops.Field[string]{Set: true, Value: *wire.Notes}
	}
	if hasPatchMember(fields, "append_notes") {
		patch.AppendNotes = issueops.Field[string]{Set: true, Value: *wire.AppendNotes}
	}
	return nil
}

func applyIssuePatchPriority(fields map[string]json.RawMessage, wire apigen.IssuePatchBody, patch *issueops.IssuePatch) *Result {
	if !hasPatchMember(fields, "priority") {
		return nil
	}
	priority := *wire.Priority
	if priority < 0 || priority > 4 {
		return issuePatchInvalid("priority", fmt.Sprintf("`priority` is %d; the range is 0 to 4", priority))
	}
	patch.Priority = issueops.Field[int]{Set: true, Value: priority}
	return nil
}

func applyIssuePatchBoundedFields(fields map[string]json.RawMessage, wire apigen.IssuePatchBody, id string, patch *issueops.IssuePatch) *Result {
	for _, bounded := range []struct {
		member string
		value  *string
	}{
		{"issue_type", wire.IssueType}, {"status", wire.Status},
		{"assignee", wire.Assignee}, {"parent_id", wire.ParentId},
	} {
		if res := applyBoundedText(updatePatchMember+".", bounded.member, bounded.value); res != nil {
			return res
		}
	}
	if hasPatchMember(fields, "issue_type") {
		patch.IssueType = issueops.Field[issueops.IssueType]{Set: true, Value: issueops.IssueType(*wire.IssueType)}
	}
	if hasPatchMember(fields, "status") {
		patch.Status = issueops.Field[issueops.Status]{Set: true, Value: issueops.Status(*wire.Status)}
	}
	if hasPatchMember(fields, "assignee") {
		patch.Assignee = issueops.Field[string]{Set: true, Value: *wire.Assignee}
	}
	if hasPatchMember(fields, "parent_id") {
		if *wire.ParentId == id {
			return issuePatchInvalid("parent_id", "`parent_id` names this issue; an issue cannot be its own parent")
		}
		patch.ParentID = issueops.Field[string]{Set: true, Value: *wire.ParentId}
	}
	return nil
}

func applyIssuePatchMetadata(fields map[string]json.RawMessage, patch *issueops.IssuePatch) *Result {
	if !hasPatchMember(fields, "metadata") {
		return nil
	}
	metadata, res := applyMetadataPatch(patchParam("metadata")+".", fields["metadata"])
	if res != nil {
		return res
	}
	patch.Metadata = metadata
	return nil
}

func applyIssuePatchLabels(fields map[string]json.RawMessage, wire apigen.IssuePatchBody, patch *issueops.IssuePatch) *Result {
	labelEdit := issueops.LabelPatch{}
	for _, member := range []struct {
		name   string
		values *[]string
		apply  func([]string)
	}{
		{"labels", wire.Labels, func(v []string) {
			labelEdit.Replace = issueops.Field[[]string]{Set: true, Value: v}
		}},
		{"add_labels", wire.AddLabels, func(v []string) { labelEdit.Add = v }},
		{"remove_labels", wire.RemoveLabels, func(v []string) { labelEdit.Remove = v }},
	} {
		if !hasPatchMember(fields, member.name) {
			continue
		}
		values := *member.values
		for i, label := range values {
			if types.CheckFieldLen("label", label) != nil {
				return issuePatchInvalid(member.name, fmt.Sprintf("`%s[%d]` is %d characters; storage holds at most %d",
					member.name, i, utf8.RuneCountInString(label), types.MaxFieldLen))
			}
		}
		member.apply(values)
	}
	patch.Labels = labelEdit
	return nil
}

func applyIssuePatchNullable(fields map[string]json.RawMessage, wire apigen.IssuePatchBody, patch *issueops.IssuePatch) *Result {
	if hasPatchMember(fields, "estimated_minutes") {
		patch.EstimatedMinutes = issueops.Field[*int]{Set: true, Value: wire.EstimatedMinutes}
	}
	if hasPatchMember(fields, "external_ref") {
		if ref := wire.ExternalRef; ref != nil && types.CheckFieldLen("external_ref", *ref) != nil {
			return issuePatchInvalid("external_ref", fmt.Sprintf("`external_ref` is %d characters; storage holds at most %d",
				utf8.RuneCountInString(*ref), types.MaxFieldLen))
		}
		patch.ExternalRef = issueops.Field[*string]{Set: true, Value: wire.ExternalRef}
	}
	if hasPatchMember(fields, "due_at") {
		patch.DueAt = issueops.Field[*time.Time]{Set: true, Value: wire.DueAt}
	}
	if hasPatchMember(fields, "defer_until") {
		patch.DeferUntil = issueops.Field[*time.Time]{Set: true, Value: wire.DeferUntil}
	}
	return nil
}

// isJSONNull reports whether a raw member is the literal `null`.
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// patchParam names a patch member the way a client reads it back off `param`:
// qualified by the member that carries it, so `patch.title` is unambiguous
// against a request-level member of the same name.
func patchParam(member string) string {
	if member == "" {
		return updatePatchMember
	}
	return updatePatchMember + "." + member
}

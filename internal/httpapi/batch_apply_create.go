package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/httpapi/apigen"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/issueops"
)

// applyCreateItem projects a create payload onto the role's create item.
//
// It decodes into the GENERATED struct after the raw member check, which is
// what makes a member's type the DOCUMENT's type: `priority: "high"` is refused
// here rather than reaching a role that would have to guess what the caller
// meant.
func applyCreateItem(prefix string, encoded json.RawMessage, raw map[string]json.RawMessage) (*issueops.CreateItem, *Result) {
	wire, res := decodeApplyCreateItem(prefix, encoded, raw)
	if res != nil {
		return nil, res
	}
	if res := validateApplyCreateItem(prefix, wire); res != nil {
		return nil, res
	}
	issue, res := buildApplyCreateIssue(prefix, wire)
	if res != nil {
		return nil, res
	}
	refs, res := applyMetadataRefs(prefix, raw)
	if res != nil {
		return nil, res
	}
	return &issueops.CreateItem{Key: derefString(wire.Key), Issue: issue, MetadataRefs: refs}, nil
}

func decodeApplyCreateItem(prefix string, encoded json.RawMessage, raw map[string]json.RawMessage) (apigen.ApplyCreateItem, *Result) {
	if offender, unknown := unknownMember(raw, applyCreateItemMembers); unknown {
		return apigen.ApplyCreateItem{}, applyUnknownMember(prefix, offender, applyCreateItemMembers)
	}
	var wire apigen.ApplyCreateItem
	if err := json.Unmarshal(encoded, &wire); err != nil {
		return apigen.ApplyCreateItem{}, invalidApplyCreateItem(prefix, "", "a `create` member carries the wrong JSON type")
	}
	if strings.TrimSpace(wire.Title) == "" {
		return apigen.ApplyCreateItem{}, invalidApplyCreateItem(prefix, "title", "`title` is required and must not be blank")
	}
	return wire, nil
}

func invalidApplyCreateItem(prefix, member, detail string) *Result {
	res := InvalidArgument(applyParam(prefix, member), ReasonInvalidValue, detail)
	return &res
}

func validateApplyCreateItem(prefix string, wire apigen.ApplyCreateItem) *Result {
	// The role validates the type, the status and the id prefix against the
	// workspace's own configuration, which this server cannot read without a
	// transaction; what is checked here is only what the schema declares. A
	// SLICE and not a map, so an item breaking two rules always names the same
	// offender: `param` is what a client dispatches on and it must not depend on
	// map order.
	for _, bounded := range []struct {
		member string
		value  *string
	}{
		{"title", &wire.Title}, {"id", wire.Id}, {"key", wire.Key},
		{"issue_type", wire.IssueType}, {"status", wire.Status},
		{"assignee", wire.Assignee}, {"owner", wire.Owner},
		{"external_ref", wire.ExternalRef}, {"sender", wire.Sender},
	} {
		if res := applyBoundedText(prefix, bounded.member, bounded.value); res != nil {
			return res
		}
	}
	if wire.Ephemeral != nil && *wire.Ephemeral && wire.NoHistory != nil && *wire.NoHistory {
		return invalidApplyCreateItem(prefix, "no_history", "`ephemeral` and `no_history` select different retention modes; send one")
	}
	return nil
}

func buildApplyCreateIssue(prefix string, wire apigen.ApplyCreateItem) (*types.Issue, *Result) {
	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: derefString(wire.Id),
		},
		IssueContent: types.IssueContent{
			Title:              wire.Title,
			Description:        derefString(wire.Description),
			Design:             derefString(wire.Design),
			AcceptanceCriteria: derefString(wire.AcceptanceCriteria),
			Notes:              derefString(wire.Notes),
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:           types.Status(derefString(wire.Status)),
			IssueType:        types.IssueType(derefString(wire.IssueType)),
			Assignee:         derefString(wire.Assignee),
			Owner:            derefString(wire.Owner),
			EstimatedMinutes: wire.EstimatedMinutes,
		},
		IssueLease: types.IssueLease{
			DueAt:      wire.DueAt,
			DeferUntil: wire.DeferUntil,
		},
		IssueMeta: types.IssueMeta{
			ExternalRef: wire.ExternalRef,
			Metadata:    wire.Metadata,
		},
		IssueWisp: types.IssueWisp{
			Sender: derefString(wire.Sender),
		},
	}
	if wire.Priority != nil {
		if *wire.Priority < 0 || *wire.Priority > 4 {
			return nil, invalidApplyCreateItem(prefix, "priority", fmt.Sprintf("`priority` is %d; the range is 0 to 4", *wire.Priority))
		}
		issue.Priority = *wire.Priority
	}
	if wire.Labels != nil {
		if res := applyBoundedLabels(prefix, "labels", *wire.Labels); res != nil {
			return nil, res
		}
		issue.Labels = *wire.Labels
	}
	if wire.Ephemeral != nil {
		issue.Ephemeral = *wire.Ephemeral
	}
	if wire.NoHistory != nil {
		issue.NoHistory = *wire.NoHistory
	}
	return issue, nil
}

// applyMetadataRefs decodes the one member whose keys may reach FORWARD. The
// refs are read raw rather than off the generated map so an unknown member
// inside one is refused by name like every other one.
func applyMetadataRefs(prefix string, raw map[string]json.RawMessage) (map[string]issueops.Ref, *Result) {
	member, ok := raw["metadata_refs"]
	if !ok {
		return nil, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(member, &entries); err != nil || entries == nil {
		res := InvalidArgument(prefix+"metadata_refs", ReasonInvalidValue,
			"`metadata_refs` must be an object whose values are refs")
		return nil, &res
	}
	refs := make(map[string]issueops.Ref, len(entries))
	for key, encoded := range entries {
		ref, res := applyRef(prefix+"metadata_refs."+key, encoded)
		if res != nil {
			return nil, res
		}
		refs[key] = ref
	}
	return refs, nil
}

package httpapi

import (
	"encoding/json"
	"fmt"

	"github.com/jonbaldie/beads/internal/httpapi/apigen"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/issueops"
)

// applyCloseItem projects a close payload onto the role's close item.
func applyCloseItem(prefix string, raw map[string]json.RawMessage) (*issueops.CloseItem, *Result) {
	if offender, unknown := unknownMember(raw, applyCloseItemMembers); unknown {
		return nil, applyUnknownMember(prefix, offender, applyCloseItemMembers)
	}
	target, res := applyRequiredRef(prefix, raw, "target")
	if res != nil {
		return nil, res
	}
	reason, res := applyTextMember(raw, prefix, "reason")
	if res != nil {
		return nil, res
	}
	session, res := applyTextMember(raw, prefix, "session")
	if res != nil {
		return nil, res
	}
	force, res := applyBoolMember(raw, prefix, "force")
	if res != nil {
		return nil, res
	}
	expectedVersion, res := applyVersionGuardMember(raw, prefix)
	if res != nil {
		return nil, res
	}
	// There is deliberately no `expected_status` here: a close is idempotent, so
	// a guard spelled to refuse an already-closed row asks for a refusal where
	// this verb answers with a no-op. See the schema.
	return &issueops.CloseItem{
		Target:          target,
		Reason:          reason,
		Session:         session,
		Force:           force,
		ExpectedVersion: expectedVersion,
	}, nil
}

// applyDepAddItem projects a dep_add payload onto the role's edge item.
//
// The gate normalization a waits-for edge gets, and the refusal a bad gate
// earns, belong to the role: this checks only that the type IS a value the
// column can hold, never that it is a member of a known-types list, because the
// edge vocabulary is OPEN and a workspace's own type must pass.
func applyDepAddItem(prefix string, raw map[string]json.RawMessage) (*issueops.DepAddItem, *Result) {
	if offender, unknown := unknownMember(raw, applyDepAddItemMembers); unknown {
		return nil, applyUnknownMember(prefix, offender, applyDepAddItemMembers)
	}
	source, res := applyRequiredRef(prefix, raw, "source")
	if res != nil {
		return nil, res
	}
	target, res := applyRequiredRef(prefix, raw, "target")
	if res != nil {
		return nil, res
	}
	edgeType, res := applyRequiredText(raw, prefix, "type")
	if res != nil {
		return nil, res
	}
	if !types.DependencyType(edgeType).IsValid() {
		res := InvalidArgument(prefix+"type", ReasonInvalidValue,
			fmt.Sprintf("`type` must be 1 to %d characters", types.MaxDependencyTypeLen))
		return nil, &res
	}
	item := &issueops.DepAddItem{
		Source: source,
		Target: target,
		Type:   types.DependencyType(edgeType),
	}
	// The blob travels as the bytes the caller sent. The role is the single
	// definition of what it will accept, and a re-encode here would be a second
	// one — the metadata compare-and-set's rule, applied to an edge.
	if metadata, present := raw["metadata"]; present {
		item.Metadata = string(metadata)
	}
	return item, nil
}

// applyRequiredRef reads one ref member that must be present.
func applyRequiredRef(prefix string, raw map[string]json.RawMessage, member string) (issueops.Ref, *Result) {
	encoded, ok := raw[member]
	if !ok {
		res := InvalidArgument(prefix+member, ReasonInvalidValue, "`"+member+"` is required")
		return issueops.Ref{}, &res
	}
	return applyRef(prefix+member, encoded)
}

// applyRef decodes one ref and applies the exactly-one rule the schema cannot
// state: `oneOf` is unavailable in this document, so a client can construct a
// ref with both members or neither and only the server can say no.
//
// Whether a key RESOLVES — and whether it reaches backward far enough — is the
// role's question, because only the role can see the whole request's key index.
func applyRef(param string, encoded json.RawMessage) (issueops.Ref, *Result) {
	wire, res := decodeApplyRef(param, encoded)
	if res != nil {
		return issueops.Ref{}, res
	}
	if res := validateApplyRef(param, wire); res != nil {
		return issueops.Ref{}, res
	}
	return projectApplyRef(param, wire)
}

func decodeApplyRef(param string, encoded json.RawMessage) (apigen.Ref, *Result) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil || raw == nil {
		return apigen.Ref{}, invalidApplyRef(param, "a ref must be a JSON object naming a `key` or an `id`")
	}
	if offender, unknown := unknownMember(raw, applyRefMembers); unknown {
		return apigen.Ref{}, applyUnknownMember(param+".", offender, applyRefMembers)
	}
	var wire apigen.Ref
	if err := json.Unmarshal(encoded, &wire); err != nil {
		return apigen.Ref{}, invalidApplyRef(param, "a ref's `key` and `id` must be strings")
	}
	return wire, nil
}

func invalidApplyRef(param, detail string) *Result {
	res := InvalidArgument(param, ReasonInvalidValue, detail)
	return &res
}

func validateApplyRef(param string, wire apigen.Ref) *Result {
	for _, bounded := range []struct {
		member string
		value  *string
	}{{"key", wire.Key}, {"id", wire.Id}} {
		if res := applyBoundedText(param+".", bounded.member, bounded.value); res != nil {
			return res
		}
	}
	return nil
}

func projectApplyRef(param string, wire apigen.Ref) (issueops.Ref, *Result) {
	key, id := derefString(wire.Key), derefString(wire.Id)
	switch {
	case key == "" && id == "":
		return issueops.Ref{}, invalidApplyRef(param, "a ref must name a `key` or an `id`")
	case key != "" && id != "":
		return issueops.Ref{}, invalidApplyRef(param, "a ref names a `key` or an `id`, not both")
	}
	return issueops.Ref{Key: key, ID: id}, nil
}

// applyBatchResponse projects the role's result onto the LEAN wire result.
//
// ItemResult.Issue is deliberately dropped. The Go contract carries a post-item
// snapshot because the completion hooks over this role hand a script the row it
// is being told about — and hooks never fire on this surface, so the snapshot
// has no consumer here and a hundred hydrated issues would be a response an
// order of magnitude larger than the request that produced it. See
// issueops.ItemResult.Issue.
func applyBatchResponse(result issueops.ApplyBatchResult) apigen.ApplyBatchResponse {
	// Allocated rather than passed through: `keys` is a required member and a
	// request whose creates named nothing must answer with an empty object, not
	// the `null` a nil map marshals to.
	keys := make(map[string]string, len(result.Keys))
	for key, id := range result.Keys {
		keys[key] = id
	}
	items := make([]apigen.ApplyItemResult, 0, len(result.Items))
	for _, item := range result.Items {
		wire := apigen.ApplyItemResult{
			Kind:     apigen.ApplyItemResultKind(item.Kind),
			IssueId:  item.IssueID,
			Changed:  item.Changed,
			Revision: item.RowVersion,
		}
		if item.DependsOnID != "" {
			dependsOn := item.DependsOnID
			wire.DependsOnId = &dependsOn
		}
		items = append(items, wire)
	}
	return apigen.ApplyBatchResponse{Keys: keys, Items: items}
}

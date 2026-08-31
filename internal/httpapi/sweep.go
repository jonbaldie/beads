package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/httpapi/apigen"
	"github.com/jonbaldie/beads/issueops"
)

// The request body's member vocabulary. The schema is
// additionalProperties: false, so anything else is refused BY NAME — the same
// posture the unknown-query-parameter rule takes, and on this operation for a
// sharper reason: a narrowing term the server silently ignored would widen
// what is erased.
const (
	sweepTierMember              = "tier"
	sweepActorMember             = "actor"
	sweepClosedBeforeMember      = "closed_before"
	sweepPatternMember           = "pattern"
	sweepProtectReferencedMember = "protect_referenced"
	sweepDryRunMember            = "dry_run"
)

// sweepMembers is the whole vocabulary, in one place, so the unknown-member
// refusal and the decoding below cannot come to disagree about what this
// operation accepts.
var sweepMembers = []string{
	sweepTierMember,
	sweepActorMember,
	sweepClosedBeforeMember,
	sweepPatternMember,
	sweepProtectReferencedMember,
	sweepDryRunMember,
}

// handleSweep answers POST /v0/beads/issues:sweep — one of the two DESTRUCTIVE
// operations on this surface, the other being issues:delete.
//
// WHAT THIS HANDLER DOES NOT DO. It does not decide which beads are closed,
// does not match the glob, does not recheck closed_at, does not protect pinned
// beads, and — the one that matters most — does not implement the
// require-a-filter safety gate. All of that is issueops.Sweeper, the same
// library surface `bd prune` calls, so this endpoint could not erase every
// closed bead in a workspace by omission even if a future edit here forgot the
// rule existed. With the gate in the CLI handler instead, a second front door
// would be one handler away from an unguarded mass delete.
//
// Everything above the role here is argument validation: the media type, the
// body shape, and the six members the document publishes.
//
// NO ACTOR IS INFERRED, for the reason the claim gives: the server's own
// identity is meaningless to a remote caller. Unlike the claim, the actor is
// OPTIONAL here — a deleted bead leaves no row to attribute the deletion on —
// and it is validated by the same rules when present, because it reaches the
// same commit-message interpolation.
func (s *Server) handleSweep(w http.ResponseWriter, r *http.Request) {
	if !s.requireNoQuery(w, r) {
		return
	}
	if !s.requireJSONContent(w, r) {
		return
	}
	request, ok := s.sweepRequest(w, r)
	if !ok {
		return
	}

	sweeper, err := s.sweeper(r)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	result, err := sweeper.Sweep(r.Context(), request)
	if err != nil {
		s.failSweepErr(w, r, err)
		return
	}
	writeJSON(w, sweepResponse(result))
}

// sweepRequest decodes the body into the role's request, member by member.
//
// Member by member rather than straight into apigen.SweepRequest so that every
// refusal can NAME the member it is about: unmarshaling the generated struct
// reports a type mismatch only inside an error string, and this endpoint
// exists so clients can stop parsing prose.
func (s *Server) sweepRequest(w http.ResponseWriter, r *http.Request) (issueops.SweepRequest, bool) {
	members, res := decodeJSONObjectBody(w, r)
	if res != nil {
		s.fail(w, r, *res)
		return issueops.SweepRequest{}, false
	}

	if offender, unknown := unknownMember(members, sweepMembers); unknown {
		// One offender, chosen deterministically so a client dispatching on
		// `param` never sees it depend on map order.
		requestInfo(r.Context()).refuse(offender)
		s.fail(w, r, InvalidArgument(offender, ReasonUnknownParameter,
			"this operation's request body carries "+sweepMemberList()+" and nothing else"))
		return issueops.SweepRequest{}, false
	}

	request, res := parseSweepRequest(members)
	if res != nil {
		s.fail(w, r, *res)
		return issueops.SweepRequest{}, false
	}
	return request, true
}

func parseSweepRequest(members map[string]json.RawMessage) (issueops.SweepRequest, *Result) {
	tier, res := decodeSweepTier(members)
	if res != nil {
		return issueops.SweepRequest{}, res
	}
	actor, res := optionalActorMember(members)
	if res != nil {
		return issueops.SweepRequest{}, res
	}
	closedBefore, res := decodeSweepClosedBefore(members)
	if res != nil {
		return issueops.SweepRequest{}, res
	}
	pattern, res := decodeSweepPattern(members)
	if res != nil {
		return issueops.SweepRequest{}, res
	}
	flags, res := decodeSweepFlags(members)
	if res != nil {
		return issueops.SweepRequest{}, res
	}
	return issueops.SweepRequest{
		Actor:             actor,
		Tier:              tier,
		ClosedBefore:      closedBefore,
		IDPattern:         pattern,
		ProtectReferenced: flags.protectReferenced,
		DryRun:            flags.dryRun,
	}, nil
}

func decodeSweepTier(members map[string]json.RawMessage) (issueops.SweepTier, *Result) {
	raw, ok := members[sweepTierMember]
	if !ok {
		return "", sweepRefusal(sweepTierMember, "`"+sweepTierMember+"` is required and has no default")
	}
	var tier *string
	if err := json.Unmarshal(raw, &tier); err != nil || tier == nil {
		return "", sweepRefusal(sweepTierMember, "`"+sweepTierMember+"` must be a string")
	}
	// The enum check uses the GENERATED validator, which is derived from the
	// document rather than a second hand-written copy of the vocabulary. The
	// role refuses an unrecognized tier too; this is here so the refusal can
	// name the member.
	if !apigen.SweepRequestTier(*tier).Valid() {
		return "", sweepRefusal(sweepTierMember,
			"`"+sweepTierMember+"` must be \"ephemeral\" or \"durable\"")
	}
	return issueops.SweepTier(*tier), nil
}

func decodeSweepClosedBefore(members map[string]json.RawMessage) (*time.Time, *Result) {
	raw, ok := members[sweepClosedBeforeMember]
	if !ok {
		return nil, nil
	}
	var value *time.Time
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, sweepRefusal(sweepClosedBeforeMember,
			"`"+sweepClosedBeforeMember+"` must be an RFC 3339 timestamp")
	}
	return value, nil
}

func decodeSweepPattern(members map[string]json.RawMessage) (string, *Result) {
	raw, ok := members[sweepPatternMember]
	if !ok {
		return "", nil
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return "", sweepRefusal(sweepPatternMember, "`"+sweepPatternMember+"` must be a string")
	}
	// A malformed glob is NOT refused here. The role refuses it, and routing it
	// through the role is what keeps one definition of what a pattern is:
	// filepath.Match's, matched in Go on both front doors.
	return *value, nil
}

type sweepFlags struct {
	protectReferenced bool
	dryRun            bool
}

func decodeSweepFlags(members map[string]json.RawMessage) (sweepFlags, *Result) {
	protectReferenced, res := applyBoolMember(members, "", sweepProtectReferencedMember)
	if res != nil {
		return sweepFlags{}, res
	}
	dryRun, res := applyBoolMember(members, "", sweepDryRunMember)
	if res != nil {
		return sweepFlags{}, res
	}
	// protect_referenced DEFAULTS ON over HTTP, unlike the zero value used by
	// the role. An omitted member must not weaken the operator's safe default.
	if _, present := members[sweepProtectReferencedMember]; !present {
		protectReferenced = true
	}
	return sweepFlags{protectReferenced: protectReferenced, dryRun: dryRun}, nil
}

func sweepRefusal(param, detail string) *Result {
	res := InvalidArgument(param, ReasonInvalidValue, detail)
	return &res
}

func sweepMemberList() string {
	quoted := make([]string, len(sweepMembers))
	for i, name := range sweepMembers {
		quoted[i] = "`" + name + "`"
	}
	return strings.Join(quoted, ", ")
}

// failSweepErr answers a failed sweep.
//
// issueops.ErrValidation is mapped to a 400 HERE rather than in ClassifyError,
// because this operation's ROLE performs request validation the handler does
// not duplicate — the require-a-filter gate, the tier vocabulary and the glob
// are all refused below the wire. Delete, tree, edges, blocking and batch-create
// each draw the same line in their own handler, deliberately in the same shape.
// Widening ClassifyError instead would change what every other operation
// returns for an error it has never produced.
func (s *Server) failSweepErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, issueops.ErrValidation) {
		// No `param`: the refusal is about the REQUEST rather than one member
		// of it — an unfiltered durable sweep is two absent members at once —
		// and the document's `param` is documented absent on exactly that
		// case. The detail carries the role's own sentence, which names what
		// to send instead.
		s.fail(w, r, InvalidArgument("", ReasonInvalidValue, err.Error()))
		return
	}
	s.failErr(w, r, err)
}

// sweepResponse projects the role's result onto the wire type. It is a field
// list rather than an alias because SweepResult is deliberately not
// x-go-type-pinned: there is no canonical Go struct whose JSON encoding is
// this body (see the schema's own description), so the projection is where the
// two shapes are held together and TestSweepResponseCarriesEveryRoleField is
// what keeps a new result field from being dropped here in silence.
func sweepResponse(result issueops.SweepResult) apigen.SweepResult {
	body := apigen.SweepResult{
		DryRun:       result.DryRun,
		Swept:        result.Swept,
		Dependencies: result.Dependencies,
		Labels:       result.Labels,
		Events:       result.Events,
		Skipped: apigen.SweepSkips{
			Pinned:                result.Skipped.Pinned,
			Referenced:            result.Skipped.Referenced,
			NotClosed:             result.Skipped.NotClosed,
			UnknownClosedAt:       result.Skipped.UnknownClosedAt,
			ClosedAtOrAfterCutoff: result.Skipped.ClosedAtOrAfterCutoff,
			Unreadable:            result.Skipped.Unreadable,
		},
	}
	if len(result.ReferencedIDs) > 0 {
		ids := append([]string(nil), result.ReferencedIDs...)
		body.ReferencedIds = &ids
	}
	return body
}

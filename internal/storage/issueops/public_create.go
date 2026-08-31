package issueops

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/types"
	publicops "github.com/jonbaldie/beads/issueops"
)

// PublicCreateContext holds the configuration required to prepare a public
// create request for the domain create use case.
type PublicCreateContext struct {
	IssuePrefix     string
	AllowedPrefixes string
	CustomStatuses  []string
	CustomTypes     []string
}

type publicCreateEndpoint struct {
	newIssue bool
	id       string
}

type publicCreateEdge struct {
	source publicCreateEndpoint
	target publicCreateEndpoint
	typ    types.DependencyType
}

type publicCreateEdgeKey struct {
	source publicCreateEndpoint
	target publicCreateEndpoint
}

// ValidatePublicCreateRequest checks public-create invariants independent of
// database configuration.
func ValidatePublicCreateRequest(request publicops.CreateRequest) error {
	if request.Actor == "" || request.Issue == nil {
		return publicCreateValidationError(fmt.Errorf("create: actor and issue are required"))
	}
	if err := validatePublicCreateIssueFields(request); err != nil {
		return err
	}
	return validatePublicCreateDependencies(request)
}

func validatePublicCreateIssueFields(request publicops.CreateRequest) error {
	if err := types.CheckFieldLen("actor", request.Actor); err != nil {
		return publicCreateValidationError(fmt.Errorf("create: %w", err))
	}
	if len(request.Issue.Comments) > 0 || len(request.Issue.Dependencies) > 0 {
		return publicCreateValidationError(fmt.Errorf("create: issue comments and dependencies must be supplied through request fields"))
	}
	for _, field := range []struct{ name, value string }{{"assignee", request.Issue.Assignee}, {"owner", request.Issue.Owner}} {
		if err := types.CheckFieldLen(field.name, field.value); err != nil {
			return publicCreateValidationError(err)
		}
	}
	for _, label := range request.Issue.Labels {
		if err := types.CheckFieldLen("label", label); err != nil {
			return publicCreateValidationError(err)
		}
	}
	if err := types.CheckFieldLen("parent ID", request.ParentID); err != nil {
		return publicCreateValidationError(fmt.Errorf("create: %w", err))
	}
	return nil
}

// PreparePublicCreateRequest snapshots, normalizes, and validates a public
// create request using the supplied configuration.
func PreparePublicCreateRequest(request publicops.CreateRequest, context PublicCreateContext) (publicops.CreateRequest, error) {
	request = CloneCreateRequest(request)
	if err := ValidatePublicCreateRequest(request); err != nil {
		return publicops.CreateRequest{}, err
	}
	issue := publicCreateIssue(request.Issue)
	if issue.Status == "" {
		issue.Status = types.StatusOpen
	}
	if err := validatePublicCreateIDPrefix(request, context, issue); err != nil {
		return publicops.CreateRequest{}, err
	}
	if err := PrepareIssueForInsert(issue, context.CustomStatuses, context.CustomTypes); err != nil {
		return publicops.CreateRequest{}, publicCreateValidationError(err)
	}
	prepared := request
	prepared.Issue = issue
	defaultPublicCreateWaitGate(&prepared)
	if err := ValidatePublicCreateRequest(prepared); err != nil {
		return publicops.CreateRequest{}, err
	}
	return prepared, nil
}

func validatePublicCreateIDPrefix(request publicops.CreateRequest, context PublicCreateContext, issue *types.Issue) error {
	if issue.ID == "" || request.ForceIDPrefix {
		return nil
	}
	// The caller's prefix wins when it supplied one; see CreateRequest.IDPrefix
	// for why a front door may know better than the substrate does.
	prefix := context.IssuePrefix
	if request.IDPrefix != "" {
		prefix = request.IDPrefix
	}
	if err := ValidateIssueIDPrefix(issue.ID, strings.TrimSuffix(prefix, "-"), context.AllowedPrefixes); err != nil {
		return publicCreateValidationError(err)
	}
	return nil
}

func defaultPublicCreateWaitGate(request *publicops.CreateRequest) {
	if request.WaitsFor != nil && request.WaitsFor.Gate == "" {
		request.WaitsFor.Gate = string(types.WaitsForAllChildren)
	}
}

// ClassifyPublicCreateError adds ErrValidation only to known deterministic
// public-create failures and leaves infrastructure and commit errors intact.
func ClassifyPublicCreateError(err error) error {
	if err == nil || errors.Is(err, storage.ErrValidation) || errors.Is(err, storage.ErrAlreadyExists) {
		return err
	}
	if isPublicCreateUniqueConflict(err) {
		return fmt.Errorf("%w: %w", storage.ErrAlreadyExists, err)
	}
	if isPublicCreateValidationFailure(err) {
		return publicCreateValidationError(err)
	}
	return classifyMissingPublicCreateEndpoint(err)
}

func isPublicCreateUniqueConflict(err error) bool {
	var stateErr interface{ SQLState() string }
	return errors.As(err, &stateErr) && stateErr.SQLState() == "23505"
}

func isPublicCreateValidationFailure(err error) bool {
	var conflict *domain.DependencyTypeConflictError
	var hierarchyConflict *domain.DependencyHierarchyConflictError
	return errors.Is(err, storage.ErrPrefixMismatch) || errors.Is(err, domain.ErrSelfDependency) || errors.Is(err, types.ErrFieldTooLong) || errors.Is(err, domain.ErrDependencyCycle) || errors.As(err, &conflict) || errors.As(err, &hierarchyConflict)
}

func classifyMissingPublicCreateEndpoint(err error) error {
	// A create whose requested relationship names a row that does not exist is
	// refused by the dependency write: as the typed endpoint refusal where the
	// write could name the absent endpoint, and as the target foreign key where
	// it could not. The caller asked for an edge to something absent, so this
	// is a deterministic refusal rather than an infrastructure error: classify
	// it the same way ExecuteCreate refuses a skipped dependency, so every
	// backend reports a missing dependency, parent, or waits-for target as
	// ErrValidation wrapping ErrNotFound.
	var missingEndpoint *domain.DependencyEndpointNotFoundError
	if errors.As(err, &missingEndpoint) || dberrors.IsMissingForeignKeyTarget(err) {
		return publicCreateValidationError(fmt.Errorf("create: dependency target does not exist: %w: %w", err, storage.ErrNotFound))
	}
	return err
}

func publicCreateValidationError(err error) error {
	return fmt.Errorf("%w: %w", storage.ErrValidation, err)
}

func publicCreateIssue(source *types.Issue) *types.Issue {
	return &types.Issue{
		IssueID: types.IssueID{
			ID: source.ID,
		},
		IssueContent: types.IssueContent{
			Title:              source.Title,
			Description:        source.Description,
			Design:             source.Design,
			AcceptanceCriteria: source.AcceptanceCriteria,
			Notes:              source.Notes,
			SpecID:             source.SpecID,
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:           source.Status,
			Priority:         source.Priority,
			IssueType:        source.IssueType,
			Assignee:         source.Assignee,
			Owner:            source.Owner,
			EstimatedMinutes: cloneInt(source.EstimatedMinutes),
		},
		IssueTimes: types.IssueTimes{
			CreatedAt:       source.CreatedAt,
			CreatedBy:       source.CreatedBy,
			UpdatedAt:       source.UpdatedAt,
			StartedAt:       cloneTime(source.StartedAt),
			ClosedAt:        cloneTime(source.ClosedAt),
			CloseReason:     source.CloseReason,
			ClosedBySession: source.ClosedBySession,
		},
		IssueLease: types.IssueLease{
			DueAt:      cloneTime(source.DueAt),
			DeferUntil: cloneTime(source.DeferUntil),
		},
		IssueMeta: types.IssueMeta{
			ExternalRef:  cloneString(source.ExternalRef),
			SourceSystem: source.SourceSystem,
			Metadata:     cloneRawMessage(source.Metadata),
		},
		IssueGraph: types.IssueGraph{
			SourceRepo: source.SourceRepo,
			Labels:     append([]string(nil), source.Labels...),
		},
		IssueWisp: types.IssueWisp{
			Sender:       source.Sender,
			Ephemeral:    source.Ephemeral,
			NoHistory:    source.NoHistory,
			WispType:     source.WispType,
			StorageClass: source.StorageClass,
			Pinned:       source.Pinned,
			IsTemplate:   source.IsTemplate,
		},
		IssueCoord: types.IssueCoord{
			BondedFrom:     append([]types.BondRef(nil), source.BondedFrom...),
			AwaitType:      source.AwaitType,
			AwaitID:        source.AwaitID,
			Timeout:        source.Timeout,
			Waiters:        append([]string(nil), source.Waiters...),
			SourceFormula:  source.SourceFormula,
			SourceLocation: source.SourceLocation,
			MolType:        source.MolType,
			WorkType:       source.WorkType,
		},
		IssueEvent: types.IssueEvent{
			EventKind: source.EventKind,
			Actor:     source.Actor,
			Target:    source.Target,
			Payload:   source.Payload,
		},
	}
}

func validatePublicCreateDependencies(request publicops.CreateRequest) error {
	newIssue := publicCreateEndpoint{newIssue: true}
	edges := make([]publicCreateEdge, 0, 1+len(request.Dependencies)+1)
	if request.ParentID != "" {
		edges = append(edges, publicCreateEdge{newIssue, publicCreateEndpointFor(request, request.ParentID), types.DepParentChild})
	}
	dependencyEdges, err := publicCreateDependencyEdges(request, newIssue)
	if err != nil {
		return err
	}
	edges = append(edges, dependencyEdges...)
	waitsEdge, hasWaits, err := publicCreateWaitsForEdge(request, newIssue)
	if err != nil {
		return err
	}
	if hasWaits {
		edges = append(edges, waitsEdge)
	}
	return validatePublicCreateEdges(edges)
}

func publicCreateEndpointFor(request publicops.CreateRequest, id string) publicCreateEndpoint {
	if request.Issue.ID != "" && id == request.Issue.ID {
		return publicCreateEndpoint{newIssue: true}
	}
	return publicCreateEndpoint{id: id}
}

func publicCreateDependencyEdges(request publicops.CreateRequest, newIssue publicCreateEndpoint) ([]publicCreateEdge, error) {
	edges := make([]publicCreateEdge, 0, len(request.Dependencies))
	for index, dependency := range request.Dependencies {
		if err := types.CheckFieldLen("dependency target ID", dependency.TargetID); err != nil {
			return nil, publicCreateValidationError(fmt.Errorf("create: dependency %d: %w", index, err))
		}
		if dependency.TargetID == "" || !dependency.Type.IsValid() {
			return nil, publicCreateValidationError(fmt.Errorf("create: dependency target and type are required"))
		}
		if dependency.Metadata != "" && !json.Valid([]byte(dependency.Metadata)) {
			return nil, publicCreateValidationError(fmt.Errorf("create: dependency %d metadata must be valid JSON", index))
		}
		if err := types.CheckFieldLen("dependency thread_id", dependency.ThreadID); err != nil {
			return nil, publicCreateValidationError(fmt.Errorf("create: dependency %d: %w", index, err))
		}
		from, to := newIssue, publicCreateEndpointFor(request, dependency.TargetID)
		if dependency.Reverse {
			from, to = to, from
		}
		edges = append(edges, publicCreateEdge{from, to, dependency.Type})
	}
	return edges, nil
}

func publicCreateWaitsForEdge(request publicops.CreateRequest, newIssue publicCreateEndpoint) (publicCreateEdge, bool, error) {
	if request.WaitsFor == nil {
		return publicCreateEdge{}, false, nil
	}
	if err := types.CheckFieldLen("waits-for spawner ID", request.WaitsFor.SpawnerID); err != nil {
		return publicCreateEdge{}, false, publicCreateValidationError(fmt.Errorf("create: %w", err))
	}
	if request.WaitsFor.SpawnerID == "" || (request.WaitsFor.Gate != "" && request.WaitsFor.Gate != string(types.WaitsForAllChildren) && request.WaitsFor.Gate != string(types.WaitsForAnyChildren)) {
		return publicCreateEdge{}, false, publicCreateValidationError(fmt.Errorf("create: waits-for spawner and gate are invalid"))
	}
	return publicCreateEdge{newIssue, publicCreateEndpointFor(request, request.WaitsFor.SpawnerID), types.DepWaitsFor}, true, nil
}

func validatePublicCreateEdges(edges []publicCreateEdge) error {
	seen := map[publicCreateEdgeKey]publicCreateEdge{}
	for _, edge := range edges {
		if edge.source == edge.target {
			return publicCreateValidationError(fmt.Errorf("%w: %s", domain.ErrSelfDependency, edge.source.id))
		}
		key := publicCreateEdgeKey{source: edge.source, target: edge.target}
		if previous, ok := seen[key]; ok {
			if previous.typ == edge.typ {
				return publicCreateValidationError(fmt.Errorf("create: duplicate dependency %s -> %s", edge.source.id, edge.target.id))
			}
			return publicCreateValidationError(&domain.DependencyTypeConflictError{IssueID: edge.source.id, DependsOnID: edge.target.id, ExistingType: string(previous.typ), RequestedType: string(edge.typ)})
		}
		seen[key] = edge
	}
	return nil
}

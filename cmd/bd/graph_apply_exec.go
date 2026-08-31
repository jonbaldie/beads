package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/types"
)

type graphApplyIssueData struct {
	issueType     types.IssueType
	metadata      json.RawMessage
	priority      int
	ephemeral     bool
	noHistory     bool
	storageClass  types.StorageClass
	owner         string
	estimatedMins *int
}

func graphApplyNodeIssue(node GraphApplyNode, opts GraphApplyOptions, createdBy, owner string) (*types.Issue, error) {
	data, err := prepareGraphApplyIssue(node, opts, owner)
	if err != nil {
		return nil, err
	}
	issue := buildCreateIssue(createIssueParams{
		ident: createIssueIdentity{
			ID:          node.ID,
			Title:       node.Title,
			SpecID:      node.graphApplyNodeIssueFields.SpecID,
			ExternalRef: node.graphApplyNodeIssueFields.ExternalRef,
			CreatedBy:   createdBy,
			Owner:       data.owner,
		},
		body: createIssueBody{
			Description:        node.graphApplyNodeIssueFields.Description,
			Design:             node.graphApplyNodeIssueFields.Design,
			AcceptanceCriteria: node.graphApplyNodeIssueFields.AcceptanceCriteria,
			Notes:              node.graphApplyNodeIssueFields.Notes,
			Labels:             node.graphApplyNodeExtendedFields.Labels,
			Metadata:           data.metadata,
		},
		class: createIssueClass{
			Priority:      data.priority,
			IssueType:     data.issueType.Normalize(),
			Ephemeral:     data.ephemeral,
			NoHistory:     data.noHistory,
			StorageClass:  data.storageClass,
			MolType:       types.MolType(node.graphApplyNodeExtendedFields.MolType),
			WispType:      types.WispType(node.graphApplyNodeExtendedFields.WispType),
			InitialStatus: node.Status,
		},
		schedule: createIssueSchedule{
			EstimatedMinutes: data.estimatedMins,
			DueAt:            node.graphApplyNodeExtendedFields.DueAt,
			DeferUntil:       node.graphApplyNodeExtendedFields.DeferUntil,
		},
		event: createIssueEvent{
			EventKind: node.graphApplyNodeExtendedFields.EventKind,
			Actor:     node.graphApplyNodeExtendedFields.Actor,
			Target:    node.Target,
			Payload:   node.graphApplyNodeExtendedFields.Payload,
		},
	})
	backfillGraphApplyIssue(issue, node.graphApplyNodeExtendedFields.Pinned)
	return issue, nil
}

func prepareGraphApplyIssue(node GraphApplyNode, opts GraphApplyOptions, owner string) (graphApplyIssueData, error) {
	metadata, err := graphApplyMetadata(node)
	if err != nil {
		return graphApplyIssueData{}, err
	}
	ephemeral, noHistory, storageClass, err := graphApplyNodeStorageClass(node, opts)
	if err != nil {
		return graphApplyIssueData{}, err
	}
	estimatedMins, err := graphApplyEstimate(node)
	if err != nil {
		return graphApplyIssueData{}, err
	}
	if node.graphApplyNodeIssueFields.Owner != "" {
		owner = node.graphApplyNodeIssueFields.Owner
	}
	return graphApplyIssueData{
		issueType:     graphApplyIssueType(node.Type),
		metadata:      metadata,
		priority:      graphApplyIssuePriority(node.Priority),
		ephemeral:     ephemeral,
		noHistory:     noHistory,
		storageClass:  storageClass,
		owner:         owner,
		estimatedMins: estimatedMins,
	}, nil
}

func graphApplyMetadata(node GraphApplyNode) (json.RawMessage, error) {
	if len(node.graphApplyNodeExtendedFields.Metadata) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(node.graphApplyNodeExtendedFields.Metadata)
	if err != nil {
		return nil, fmt.Errorf("node %q: marshaling metadata: %w", node.Key, err)
	}
	return raw, nil
}

func graphApplyIssueType(rawType string) types.IssueType {
	issueType := types.IssueType(rawType)
	if issueType == "" {
		return types.TypeTask
	}
	return issueType
}

func graphApplyIssuePriority(priority *int) int {
	if priority == nil {
		return 2
	}
	return *priority
}

// Resolve the estimate alias here so every materialization path (embedded,
// proxied, dry-run, validation) sees one canonical field. The canonical
// estimated_minutes wins when both are set, matching the parent alias —
// but a negative alias value is still rejected rather than silently
// discarded by the precedence (GH#4064).
func graphApplyEstimate(node GraphApplyNode) (*int, error) {
	if node.graphApplyNodeIssueFields.Estimate != nil && *node.graphApplyNodeIssueFields.Estimate < 0 {
		return nil, fmt.Errorf("node %q: estimate cannot be negative", node.Key)
	}
	if node.graphApplyNodeExtendedFields.EstimatedMinutes != nil {
		return node.graphApplyNodeExtendedFields.EstimatedMinutes, nil
	}
	return node.graphApplyNodeIssueFields.Estimate, nil
}

// Backfill status-coupled timestamps: the proxied domain insert does no
// backfill, and neither path stamps started_at for issues born in_progress.
func backfillGraphApplyIssue(issue *types.Issue, pinned bool) {
	now := time.Now().UTC()
	if issue.Status == types.StatusClosed && issue.ClosedAt == nil {
		issue.ClosedAt = &now
	}
	if issue.Status == types.StatusInProgress && issue.StartedAt == nil {
		issue.StartedAt = &now
	}
	issue.Pinned = pinned
}

// graphApplyNodeStorageClass resolves a node's effective storage class from
// its per-node overrides, the plan-wide CLI flags, and the per-type config
// default, with single-issue create's precedence (Protocol v0.1 C1.3): the
// node's explicit storage_class wins, then storage-class.<type> config, else
// unset. "ephemeral" is the spelled-out spelling of ephemeral: true (C1.4) —
// it routes the node to the wisp plane and leaves the marker cell empty
// (wisp-plane rows derive their class, C1.2).
func graphApplyNodeStorageClass(node GraphApplyNode, opts GraphApplyOptions) (ephemeral, noHistory bool, class types.StorageClass, err error) {
	ephemeral, noHistory, err = graphApplyStorageFlags(node, opts)
	if err != nil {
		return false, false, "", err
	}
	issueType := types.IssueType(node.Type)
	if issueType == "" {
		issueType = types.TypeTask
	}
	class, err = resolveStorageClass(node.graphApplyNodeExtendedFields.StorageClass, issueType.Normalize())
	if err != nil {
		return false, false, "", fmt.Errorf("node %q: %w", node.Key, err)
	}
	return applyGraphApplyStorageClass(node, ephemeral, noHistory, class)
}

func graphApplyStorageFlags(node GraphApplyNode, opts GraphApplyOptions) (bool, bool, error) {
	ephemeral := opts.Ephemeral
	if node.graphApplyNodeExtendedFields.Ephemeral != nil {
		ephemeral = *node.graphApplyNodeExtendedFields.Ephemeral
	}
	noHistory := opts.NoHistory
	if node.NoHistory != nil {
		noHistory = *node.NoHistory
	}
	if ephemeral && noHistory {
		return false, false, fmt.Errorf("node %q: ephemeral and no_history are mutually exclusive", node.Key)
	}
	return ephemeral, noHistory, nil
}

func applyGraphApplyStorageClass(node GraphApplyNode, ephemeral, noHistory bool, class types.StorageClass) (bool, bool, types.StorageClass, error) {
	if class == types.StorageClassEphemeral {
		if noHistory {
			return false, false, "", fmt.Errorf("node %q: storage_class ephemeral and no_history are mutually exclusive", node.Key)
		}
		if node.graphApplyNodeExtendedFields.Ephemeral != nil && !*node.graphApplyNodeExtendedFields.Ephemeral {
			return false, false, "", fmt.Errorf("node %q: storage_class ephemeral conflicts with ephemeral: false", node.Key)
		}
		ephemeral = true
		class = ""
	}
	return ephemeral, noHistory, class, nil
}

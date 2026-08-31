package main

import (
	"fmt"

	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/validation"
	"github.com/jonbaldie/beads/internal/workapi"
)

// graphPlanConfig bundles the config inputs full-plan validation needs; the
// embedded and proxied paths populate it from their respective config sources.
type graphPlanConfig struct {
	customTypes     []string
	customStatuses  []string
	dbPrefix        string
	allowedPrefixes string
	// issueExists probes whether an ID is already taken by a stored issue (or
	// wisp, where the underlying lookup spans both tables). nil skips the
	// explicit-ID collision preflight (no store access in the calling context).
	issueExists func(id string) (bool, error)
}

// validateFullGraphPlan runs every plan-level check in one place so the
// embedded and proxied paths cannot skip a step independently: shared plan
// checks, storage-class resolution (uniform when requireUniform), and
// explicit-ID prefix checks. The returned useWisp is node 0's storage class —
// under requireUniform, the routing decision for the whole plan.
func validateFullGraphPlan(plan *GraphApplyPlan, cfg graphPlanConfig, opts GraphApplyOptions, requireUniform bool) (useWisp bool, err error) {
	if err := validateGraphApplyPlan(plan, cfg.customTypes, cfg.customStatuses, opts); err != nil {
		return false, err
	}
	useWisp, err = validateGraphApplyStorageClasses(plan, opts, requireUniform)
	if err != nil {
		return false, err
	}
	if err := validateGraphApplyExplicitIDPrefixes(plan, cfg.dbPrefix, cfg.allowedPrefixes, opts.Force); err != nil {
		return false, err
	}
	return useWisp, validateGraphApplyExplicitIDCollisions(plan, cfg.issueExists)
}

// validateGraphApplyExplicitIDCollisions rejects plan nodes whose explicit id
// already belongs to a stored issue or wisp. The insert path upserts on a
// duplicate ID (matching `bd create --id`), which for an atomic graph create
// would silently rewrite the existing issue while reporting creation — a plan
// must fail fast instead. Deliberately not overridable by --force: that flag
// vouches for a foreign prefix, not for destroying existing data. Best-effort
// by transport: exists is nil where the calling context has no store access.
func validateGraphApplyExplicitIDCollisions(plan *GraphApplyPlan, exists func(id string) (bool, error)) error {
	if exists == nil {
		return nil
	}
	for _, node := range plan.Nodes {
		if node.ID == "" {
			continue
		}
		taken, err := exists(node.ID)
		if err != nil {
			return fmt.Errorf("checking explicit id %q (node %q): %w", node.ID, node.Key, err)
		}
		if taken {
			return fmt.Errorf("explicit id %q (node %q) already exists; graph create will not overwrite an existing issue — drop the id to mint a new one, or update the existing issue instead", node.ID, node.Key)
		}
	}
	return nil
}

func validateGraphApplyPlan(plan *GraphApplyPlan, customTypes, customStatuses []string, opts GraphApplyOptions) error {
	if len(plan.Nodes) == 0 {
		return fmt.Errorf("plan has no nodes")
	}

	seenKeys, err := validateGraphApplyNodes(plan, customTypes, customStatuses, opts)
	if err != nil {
		return err
	}
	if err := validateGraphApplyEdges(plan, seenKeys); err != nil {
		return err
	}
	return validateGraphApplyLocalCycles(plan, seenKeys)
}

func validateGraphApplyNodes(plan *GraphApplyPlan, customTypes, customStatuses []string, opts GraphApplyOptions) (map[string]bool, error) {
	seenKeys := make(map[string]bool, len(plan.Nodes))
	seenIDs := make(map[string]bool)
	for i, node := range plan.Nodes {
		if err := validateGraphApplyNode(plan, node, i, seenKeys, seenIDs, customTypes, customStatuses, opts); err != nil {
			return nil, err
		}
	}
	return seenKeys, nil
}

func validateGraphApplyNode(plan *GraphApplyPlan, node GraphApplyNode, index int, seenKeys, seenIDs map[string]bool, customTypes, customStatuses []string, opts GraphApplyOptions) error {
	if err := validateGraphApplyNodeIdentity(node, index, seenKeys); err != nil {
		return err
	}
	if err := validateGraphApplyNodeFields(node, customTypes, customStatuses, opts); err != nil {
		return err
	}
	if err := validateGraphApplyNodeExplicitID(node, seenIDs); err != nil {
		return err
	}
	if err := validateGraphApplyNodeReferences(plan, node, seenKeys); err != nil {
		return err
	}
	return validateGraphApplyNodeDeps(node)
}

func validateGraphApplyNodeIdentity(node GraphApplyNode, index int, seenKeys map[string]bool) error {
	if node.Key == "" {
		return fmt.Errorf("node %d has empty key", index)
	}
	if seenKeys[node.Key] {
		return fmt.Errorf("duplicate node key %q", node.Key)
	}
	seenKeys[node.Key] = true
	if node.Title == "" {
		return fmt.Errorf("node %q has empty title", node.Key)
	}
	return nil
}

func validateGraphApplyNodeExplicitID(node GraphApplyNode, seenIDs map[string]bool) error {
	if node.ID == "" {
		return nil
	}
	if seenIDs[node.ID] {
		return fmt.Errorf("duplicate explicit id %q (node %q)", node.ID, node.Key)
	}
	seenIDs[node.ID] = true
	return nil
}

func validateGraphApplyNodeReferences(plan *GraphApplyPlan, node GraphApplyNode, seenKeys map[string]bool) error {
	for metaKey, refKey := range node.graphApplyNodeExtendedFields.MetadataRefs {
		if !graphApplyPlanHasKey(plan, seenKeys, refKey) {
			return fmt.Errorf("node %q: metadata ref %q references unknown key %q", node.Key, metaKey, refKey)
		}
	}
	parentKey := node.effectiveParentKey()
	if parentKey != "" && !graphApplyPlanHasKey(plan, seenKeys, parentKey) {
		return fmt.Errorf("node %q: parent key %q not found in plan", node.Key, parentKey)
	}
	return nil
}

func graphApplyPlanHasKey(plan *GraphApplyPlan, seenKeys map[string]bool, key string) bool {
	if seenKeys[key] {
		return true
	}
	for _, node := range plan.Nodes {
		if node.Key == key {
			return true
		}
	}
	return false
}

func validateGraphApplyNodeDeps(node GraphApplyNode) error {
	for i, dep := range node.Deps {
		if dep.Target == "" {
			return fmt.Errorf("node %q: dep %d has empty target", node.Key, i)
		}
		if dep.Type != "" && !types.DependencyType(dep.Type).IsValid() {
			return fmt.Errorf("node %q: dep %d: invalid dependency type %q", node.Key, i, dep.Type)
		}
	}
	return nil
}

func validateGraphApplyEdges(plan *GraphApplyPlan, seenKeys map[string]bool) error {
	for i, edge := range plan.Edges {
		if err := validateGraphApplyEdge(edge, i, seenKeys); err != nil {
			return err
		}
	}
	return nil
}

func validateGraphApplyEdge(edge GraphApplyEdge, index int, seenKeys map[string]bool) error {
	if err := validateGraphApplyEdgeEndpoints(edge, index, seenKeys); err != nil {
		return err
	}
	if edge.Type != "" && !types.DependencyType(edge.Type).IsValid() {
		return fmt.Errorf("edge %d: invalid dependency type %q", index, edge.Type)
	}
	return validateGraphApplyEdgeGate(edge, index, seenKeys)
}

func validateGraphApplyEdgeEndpoints(edge GraphApplyEdge, index int, seenKeys map[string]bool) error {
	if edge.FromKey != "" && !seenKeys[edge.FromKey] {
		return fmt.Errorf("edge %d: from key %q not found in plan", index, edge.FromKey)
	}
	if edge.ToKey != "" && !seenKeys[edge.ToKey] {
		return fmt.Errorf("edge %d: to key %q not found in plan", index, edge.ToKey)
	}
	if edge.FromKey == "" && edge.FromID == "" {
		return fmt.Errorf("edge %d: must specify from_key or from_id", index)
	}
	if edge.ToKey == "" && edge.ToID == "" {
		return fmt.Errorf("edge %d: must specify to_key or to_id", index)
	}
	return nil
}

func validateGraphApplyEdgeGate(edge GraphApplyEdge, index int, seenKeys map[string]bool) error {
	if edge.Gate == "" && edge.SpawnerKey == "" && edge.SpawnerID == "" {
		return nil
	}
	if err := validateGraphApplyEdgeGateType(edge, index); err != nil {
		return err
	}
	if err := validateGraphApplyEdgeGateValue(edge, index); err != nil {
		return err
	}
	if err := validateGraphApplyEdgeSpawner(edge, index, seenKeys); err != nil {
		return err
	}
	return validateGraphApplySpawnerEndpoint(edge, index)
}

func validateGraphApplyEdgeGateType(edge GraphApplyEdge, index int) error {
	if graphApplyDependencyType(edge.Type) != types.DepWaitsFor {
		return fmt.Errorf("edge %d: gate/spawner fields require type %q", index, types.DepWaitsFor)
	}
	return nil
}

func validateGraphApplyEdgeGateValue(edge GraphApplyEdge, index int) error {
	if edge.Gate != "" && !types.IsValidWaitsForGate(edge.Gate) {
		return fmt.Errorf("edge %d: invalid gate %q (valid: %s, %s)", index, edge.Gate, types.WaitsForAllChildren, types.WaitsForAnyChildren)
	}
	return nil
}

func validateGraphApplyEdgeSpawner(edge GraphApplyEdge, index int, seenKeys map[string]bool) error {
	if edge.SpawnerKey != "" && edge.SpawnerID != "" {
		return fmt.Errorf("edge %d: cannot specify both spawner_key and spawner_id", index)
	}
	if edge.SpawnerKey != "" && !seenKeys[edge.SpawnerKey] {
		return fmt.Errorf("edge %d: spawner key %q not found in plan", index, edge.SpawnerKey)
	}
	return nil
}

func validateGraphApplySpawnerEndpoint(edge GraphApplyEdge, index int) error {
	// Gate evaluation reads the spawner from the dependency target
	// (depends_on_id), not metadata, so the spawner must equal the to
	// endpoint. Since to_id overrides to_key at apply time (resolveEdgeRef),
	// a key-named spawner can't be combined with to_id.
	if edge.SpawnerKey != "" && edge.ToID != "" {
		return fmt.Errorf("edge %d: spawner_key %q cannot be combined with to_id %q (to_id overrides to_key as the waits-for target; use spawner_id)", index, edge.SpawnerKey, edge.ToID)
	}
	if edge.SpawnerKey != "" && edge.SpawnerKey != edge.ToKey {
		return fmt.Errorf("edge %d: spawner_key %q must match to_key %q (the waits-for target is the spawner)", index, edge.SpawnerKey, edge.ToKey)
	}
	if edge.SpawnerID != "" && edge.SpawnerID != edge.ToID {
		return fmt.Errorf("edge %d: spawner_id %q must match to_id %q (the waits-for target is the spawner)", index, edge.SpawnerID, edge.ToID)
	}
	return nil
}

// validateGraphApplyNodeFields checks the single-node fields added for
// bd-create parity, mirroring the flag-shape checks `bd create` applies
// (config-gated template linting is not run on graph plans).
func validateGraphApplyNodeFields(node GraphApplyNode, customTypes, customStatuses []string, opts GraphApplyOptions) error {
	if err := validateGraphApplyNodeShape(node, customStatuses); err != nil {
		return err
	}
	return validateGraphApplyNodeIssue(node, customTypes, customStatuses, opts)
}

func validateGraphApplyNodeShape(node GraphApplyNode, customStatuses []string) error {
	if err := validateGraphApplyNodeID(node); err != nil {
		return err
	}
	if err := validateGraphApplyNodeSpecialFields(node, customStatuses); err != nil {
		return err
	}
	return validateGraphApplyNodeEventFields(node)
}

func validateGraphApplyNodeID(node GraphApplyNode) error {
	if node.ID != "" {
		if _, err := validation.ValidateIDFormat(node.ID); err != nil {
			return fmt.Errorf("node %q: %w", node.Key, err)
		}
	}
	return nil
}

func validateGraphApplyNodeSpecialFields(node GraphApplyNode, customStatuses []string) error {
	// Friendlier status message than the issue-model validator's below.
	if node.Status != "" && !types.Status(node.Status).IsValidWithCustom(customStatuses) {
		return fmt.Errorf("node %q: invalid status %q (valid: %s; configure custom statuses via 'bd config set status.custom')", node.Key, node.Status, workapi.ValidStatusList(customStatuses))
	}
	if node.graphApplyNodeExtendedFields.WispType != "" && !types.WispType(node.graphApplyNodeExtendedFields.WispType).IsValid() {
		return fmt.Errorf("node %q: invalid wisp_type %q (must be %s)", node.Key, node.graphApplyNodeExtendedFields.WispType, types.ValidWispTypeNames())
	}
	if node.graphApplyNodeExtendedFields.MolType != "" && !types.MolType(node.graphApplyNodeExtendedFields.MolType).IsValid() {
		return fmt.Errorf("node %q: invalid mol_type %q (must be %s)", node.Key, node.graphApplyNodeExtendedFields.MolType, types.ValidMolTypeNames())
	}
	return nil
}

func validateGraphApplyNodeEventFields(node GraphApplyNode) error {
	if (node.graphApplyNodeExtendedFields.EventKind != "" || node.graphApplyNodeExtendedFields.Actor != "" || node.Target != "" || node.graphApplyNodeExtendedFields.Payload != "") && node.Type != string(types.TypeEvent) {
		return fmt.Errorf("node %q: event_kind, actor, target, and payload require type %q", node.Key, types.TypeEvent)
	}
	return nil
}

func validateGraphApplyNodeIssue(node GraphApplyNode, customTypes, customStatuses []string, opts GraphApplyOptions) error {
	// Issue-model rules (type validity, priority range, estimate sign,
	// metadata JSON, ...) run against the same materialized issue the apply
	// path stores, so plan-time and insert-time validation can't drift.
	issue, err := graphApplyNodeIssue(node, opts, "", "")
	if err != nil {
		return err
	}
	if err := issue.ValidateWithCustom(customStatuses, customTypes); err != nil {
		return fmt.Errorf("node %q: %w", node.Key, err)
	}
	return nil
}

// validateGraphApplyStorageClasses resolves each node's effective storage class
// (per-node overrides combined with the CLI flags) so conflicts surface at
// validation time instead of mid-apply. When requireUniform is set (proxied
// mode, which routes the whole plan to one table), mixed durable/wisp plans are
// also rejected. The returned useWisp is node 0's class — under requireUniform,
// the routing decision for the whole plan.
func validateGraphApplyStorageClasses(plan *GraphApplyPlan, opts GraphApplyOptions, requireUniform bool) (useWisp bool, err error) {
	for i, node := range plan.Nodes {
		ephemeral, noHistory, _, err := graphApplyNodeStorageClass(node, opts)
		if err != nil {
			return false, err
		}
		nodeWisp := ephemeral || noHistory
		if i == 0 {
			useWisp = nodeWisp
		} else if requireUniform && nodeWisp != useWisp {
			return false, fmt.Errorf("plan mixes durable and wisp storage classes (node %q vs node %q): effective ephemeral/no_history must be uniform across the plan in proxied-server mode", plan.Nodes[0].Key, node.Key)
		}
	}
	return useWisp, nil
}

// validateGraphApplyExplicitIDPrefixes mirrors the `bd create --id` prefix
// check for every plan node that pins an explicit ID: the ID must start with
// the database prefix (or one of allowed_prefixes) unless force is set.
func validateGraphApplyExplicitIDPrefixes(plan *GraphApplyPlan, dbPrefix, allowedPrefixes string, force bool) error {
	for _, node := range plan.Nodes {
		if node.ID == "" {
			continue
		}
		if err := validation.ValidateIDPrefixAllowed(node.ID, dbPrefix, allowedPrefixes, force); err != nil {
			return fmt.Errorf("node %q: %w", node.Key, err)
		}
	}
	return nil
}

// loadEmbeddedIDPrefixes returns the database prefix and allowed_prefixes for
// explicit-ID validation. YAML config takes precedence over DB (via
// overlayYAMLPrefix) — in shared-server mode the DB may belong to a different
// project (GH#2469) — except under --global, where the shared database's own
// stored prefix wins (GH#4957).
func loadEmbeddedIDPrefixes() (dbPrefix, allowedPrefixes string) {
	var storePrefix string
	if getStore() != nil {
		storePrefix, _ = getStore().GetConfig(getRootContext(), "issue_prefix")         // Best effort: empty prefix is a valid fallback
		allowedPrefixes, _ = getStore().GetConfig(getRootContext(), "allowed_prefixes") // Best effort: empty means no prefix restriction
	}
	// Under --global the shared database's stored prefix wins over the
	// project YAML overlay (GH#4957); selectCreateIDPrefix owns that rule.
	dbPrefix = selectCreateIDPrefix(isGlobalFlag(), overlayYAMLPrefix(""), storePrefix)
	return dbPrefix, allowedPrefixes
}

func validateGraphApplyLocalCycles(plan *GraphApplyPlan, knownKeys map[string]bool) error {
	adj := graphApplyLocalCycleAdjacency(plan, knownKeys)
	visiting := make(map[string]bool, len(knownKeys))
	visited := make(map[string]bool, len(knownKeys))
	for _, key := range graphApplySortedKeys(knownKeys) {
		if cycleKey, ok := graphApplyFindCycle(key, adj, visiting, visited); ok {
			return fmt.Errorf("graph contains a blocking dependency cycle involving node %q", cycleKey)
		}
	}
	return nil
}

func graphApplyLocalCycleAdjacency(plan *GraphApplyPlan, knownKeys map[string]bool) map[string][]string {
	adj := graphApplyParentCycleAdjacency(plan, knownKeys)
	for _, edge := range plan.Edges {
		depType := graphApplyDependencyType(edge.Type)
		if !graphApplyEdgeIsLocalCycleRelevant(edge, depType) {
			continue
		}
		if !knownKeys[edge.FromKey] || !knownKeys[edge.ToKey] {
			continue
		}
		adj[edge.FromKey] = append(adj[edge.FromKey], edge.ToKey)
	}
	return adj
}

func graphApplyParentCycleAdjacency(plan *GraphApplyPlan, knownKeys map[string]bool) map[string][]string {
	adj := make(map[string][]string)
	for _, node := range plan.Nodes {
		parentKey := node.effectiveParentKey()
		if parentKey != "" && knownKeys[node.Key] && knownKeys[parentKey] {
			// The parent key is guaranteed local by validateGraphApplyPlan, so it
			// is safe to model the implicit parent-child dependency by key here.
			adj[node.Key] = append(adj[node.Key], parentKey)
		}
	}
	return adj
}

func graphApplyFindCycle(key string, adj map[string][]string, visiting, visited map[string]bool) (string, bool) {
	if visiting[key] {
		return key, true
	}
	if visited[key] {
		return "", false
	}
	visiting[key] = true
	for _, next := range adj[key] {
		if cycleKey, ok := graphApplyFindCycle(next, adj, visiting, visited); ok {
			return cycleKey, true
		}
	}
	visiting[key] = false
	visited[key] = true
	return "", false
}

// graphApplyNodeIssue materializes a plan node into an issue via the same
// createIssueParams path used by single-issue `bd create`, so every field the
// CLI can set stays addressable from graph plans. Assignee handling is left
// to the caller (embedded and proxied paths defer assignment differently).

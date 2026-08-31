package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// GraphApplyPlan describes a symbolic bead graph to create atomically.
type GraphApplyPlan struct {
	CommitMessage string           `json:"commit_message,omitempty"`
	Nodes         []GraphApplyNode `json:"nodes"`
	Edges         []GraphApplyEdge `json:"edges,omitempty"`
}

// graphApplyNodeIssueFields groups optional issue content while retaining the
// flattened JSON shape of GraphApplyNode through anonymous embedding.
type graphApplyNodeIssueFields struct {
	Description        string `json:"description,omitempty"`
	Design             string `json:"design,omitempty"`
	AcceptanceCriteria string `json:"acceptance_criteria,omitempty"`
	Notes              string `json:"notes,omitempty"`
	SpecID             string `json:"spec_id,omitempty"`
	ExternalRef        string `json:"external_ref,omitempty"`
	Assignee           string `json:"assignee,omitempty"`
	AssignAfterCreate  bool   `json:"assign_after_create,omitempty"`
	Owner              string `json:"owner,omitempty"`
	Estimate           *int   `json:"estimate,omitempty"` // minutes (alias for estimated_minutes)
}

// graphApplyNodeExtendedFields groups less common storage, scheduling, and
// event fields without changing their promoted Go or flattened JSON names.
type graphApplyNodeExtendedFields struct {
	EstimatedMinutes *int                       `json:"estimated_minutes,omitempty"` // minutes
	DueAt            *time.Time                 `json:"due_at,omitempty"`            // RFC3339
	DeferUntil       *time.Time                 `json:"defer_until,omitempty"`       // RFC3339
	Labels           []string                   `json:"labels,omitempty"`
	Metadata         map[string]json.RawMessage `json:"metadata,omitempty"`
	MetadataRefs     map[string]string          `json:"metadata_refs,omitempty"`
	Ephemeral        *bool                      `json:"ephemeral,omitempty"`     // overrides --ephemeral for this node
	StorageClass     string                     `json:"storage_class,omitempty"` // explicit class (C1.3: wins over storage-class.<type> config); "ephemeral" routes to the wisp plane
	WispType         string                     `json:"wisp_type,omitempty"`
	MolType          string                     `json:"mol_type,omitempty"`
	Pinned           bool                       `json:"pinned,omitempty"`
	EventKind        string                     `json:"event_kind,omitempty"` // type=event only
	Actor            string                     `json:"actor,omitempty"`      // type=event only
	Payload          string                     `json:"payload,omitempty"`    // type=event only
}

// GraphApplyNode describes a single bead to create. Field names follow the
// types.Issue JSON tags (the names `bd show --json` emits), covering every
// field single-issue `bd create` can set plus initial status and pinned.
// Anonymous field groups keep the public wire schema flat while keeping this
// input aggregate small enough to remain easy to reason about.
type GraphApplyNode struct {
	Key       string              `json:"key"`
	ID        string              `json:"id,omitempty"` // explicit issue ID (default: generated)
	Title     string              `json:"title"`
	Type      string              `json:"type,omitempty"`
	Status    string              `json:"status,omitempty"`   // initial status (default: open, or deferred when defer_until is future)
	Priority  *int                `json:"priority,omitempty"` // nil defaults to P2
	Parent    string              `json:"parent,omitempty"`   // alias for parent_key
	ParentKey string              `json:"parent_key,omitempty"`
	ParentID  string              `json:"parent_id,omitempty"`
	Deps      []GraphApplyNodeDep `json:"deps,omitempty"`
	NoHistory *bool               `json:"no_history,omitempty"` // overrides --no-history for this node
	Target    string              `json:"target,omitempty"`     // type=event only

	graphApplyNodeIssueFields
	graphApplyNodeExtendedFields
}

// effectiveParentKey resolves the parent alias: parent_key wins, then parent.
// Every consumer of a node's plan-local parent (validation, dry-run, embedded
// apply, proxied apply) must go through this so the alias can't be dropped on
// one path.
func (n GraphApplyNode) effectiveParentKey() string {
	if n.ParentKey != "" {
		return n.ParentKey
	}
	return n.Parent
}

// GraphApplyEdge describes a dependency edge.
type GraphApplyEdge struct {
	FromKey string `json:"from_key,omitempty"`
	FromID  string `json:"from_id,omitempty"`
	ToKey   string `json:"to_key,omitempty"`
	ToID    string `json:"to_id,omitempty"`
	Type    string `json:"type,omitempty"`
	// Gate and spawner apply to waits-for edges only (fanout gates).
	Gate       string `json:"gate,omitempty"`        // all-children | any-children
	SpawnerKey string `json:"spawner_key,omitempty"` // plan-local key of the spawning node
	SpawnerID  string `json:"spawner_id,omitempty"`  // existing issue ID of the spawner
	ThreadID   string `json:"thread_id,omitempty"`   // conversation threading (replies-to)
}

// GraphApplyNodeDep describes an inline dependency on a single graph node.
// Target is resolved as a plan key first, then treated as a literal issue ID.
type GraphApplyNodeDep struct {
	Type   string `json:"type,omitempty"`
	Target string `json:"target"`
}

// GraphApplyResult returns the concrete bead IDs assigned to each symbolic key.
type GraphApplyResult struct {
	IDs map[string]string `json:"ids"`
}

// GraphApplyOptions carries CLI-level storage options that apply to every node
// in the graph.
type GraphApplyOptions struct {
	Ephemeral bool
	NoHistory bool
	Force     bool // --force: allow explicit IDs with foreign prefixes
}

func (opts GraphApplyOptions) Validate() error {
	if opts.Ephemeral && opts.NoHistory {
		return fmt.Errorf("ephemeral and no_history are mutually exclusive")
	}
	return nil
}

// GraphApplyDryRun describes the actions that would be taken by a graph plan,
// without performing any writes. Emitted by `bd create --graph --dry-run`.
type GraphApplyDryRun struct {
	DryRun          bool                  `json:"dry_run"`
	NodeCount       int                   `json:"node_count"`
	EdgeCount       int                   `json:"edge_count"`
	ParentDeps      int                   `json:"parent_deps"`
	ValidationNotes []string              `json:"validation_notes,omitempty"`
	Nodes           []GraphApplyDryRunRow `json:"nodes"`
}

// GraphApplyDryRunRow describes a single planned node in the dry-run preview.
type GraphApplyDryRunRow struct {
	Key       string `json:"key"`
	ID        string `json:"id,omitempty"` // explicit ID, when the plan sets one
	Title     string `json:"title"`
	Type      string `json:"type"`
	Status    string `json:"status,omitempty"` // effective initial status (explicit, or deferred when defer_until is future)
	Priority  int    `json:"priority"`
	ParentKey string `json:"parent_key,omitempty"`
	ParentID  string `json:"parent_id,omitempty"`
}

const graphApplyDryRunTransactionValidationNote = "dry-run validates the graph structure only; live create may still reject parent-child blocking paths after resolving stored dependencies"

// Known-field sets list the JSON keys recognized on each plan struct; unknown
// keys warn about schema typos. Derived from json tags so they can't drift. (GH#3367)
var (
	knownGraphPlanFields = jsonTagSet(reflect.TypeOf(GraphApplyPlan{}))
	knownGraphNodeFields = jsonTagSet(reflect.TypeOf(GraphApplyNode{
		graphApplyNodeIssueFields:    graphApplyNodeIssueFields{},
		graphApplyNodeExtendedFields: graphApplyNodeExtendedFields{},
	}))
	knownGraphEdgeFields = jsonTagSet(reflect.TypeOf(GraphApplyEdge{}))
)

// jsonTagSet returns the set of JSON field names declared on t's struct tags.
func jsonTagSet(t reflect.Type) map[string]struct{} {
	out := make(map[string]struct{}, t.NumField())
	addJSONTagFields(out, t)
	return out
}

func addJSONTagFields(out map[string]struct{}, t reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if field.Anonymous && tag == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				addJSONTagFields(out, embedded)
				continue
			}
		}
		if tag == "" || tag == "-" {
			continue
		}
		if comma := strings.IndexByte(tag, ','); comma >= 0 {
			tag = tag[:comma]
		}
		out[tag] = struct{}{}
	}
}

// graphFieldHints maps unknown-field names to a corrective hint pointing at
// the recognized schema field. Used by warnUnknownGraphFields to suggest the
// intended schema when a plan uses a common-but-wrong name (e.g. nodes carry
// a "parent" string instead of "parent_key", or "blocks" arrays instead of
// the top-level edges array). (GH#3367)
var graphFieldHints = map[string]string{
	"blocks":         "use the top-level 'edges' array or per-node 'deps', e.g. {\"deps\": [{\"target\": \"key\", \"type\": \"blocks\"}]}",
	"depends":        "use the top-level 'edges' array or per-node 'deps' with type 'blocks'",
	"children":       "set 'parent_key' or 'parent' on each child instead of listing children on the parent",
	"acceptance":     "use 'acceptance_criteria' (matching the issue model's JSON field)",
	"body":           "use 'description' (matching the issue model's JSON field)",
	"due":            "use 'due_at' with an RFC3339 timestamp",
	"defer":          "use 'defer_until' with an RFC3339 timestamp",
	"event_category": "use 'event_kind' (matching the issue model's JSON field)",
	"event_actor":    "use 'actor' (matching the issue model's JSON field)",
	"event_target":   "use 'target' (matching the issue model's JSON field)",
	"event_payload":  "use 'payload' (matching the issue model's JSON field)",
}

// detectUnknownGraphFields scans the raw plan JSON and returns unknown field
// names grouped by their location in the plan. The returned map keys describe
// the location ("plan", "node[<key-or-index>]", "edge[<index>]") and values
// are sorted lists of unknown field names at that location. Returns an empty
// map when the plan is structurally invalid (callers should still attempt the
// strict parse so the operator gets a normal parse error rather than only the
// schema warning). (GH#3367)
func detectUnknownGraphFields(rawData []byte) map[string][]string {
	out := make(map[string][]string)

	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawData, &top); err != nil {
		return out
	}
	addUnknownGraphFields(out, "plan", top, knownGraphPlanFields)
	addUnknownGraphCollections(out, top)
	return out
}

func addUnknownGraphCollections(out map[string][]string, top map[string]json.RawMessage) {
	if nodesRaw, ok := top["nodes"]; ok {
		addUnknownGraphNodeFields(out, nodesRaw)
	}
	if edgesRaw, ok := top["edges"]; ok {
		addUnknownGraphEdgeFields(out, edgesRaw)
	}
}

func addUnknownGraphNodeFields(out map[string][]string, raw json.RawMessage) {
	var rawNodes []json.RawMessage
	if err := json.Unmarshal(raw, &rawNodes); err != nil {
		return
	}
	for i, nodeRaw := range rawNodes {
		var nodeMap map[string]json.RawMessage
		if err := json.Unmarshal(nodeRaw, &nodeMap); err != nil {
			continue
		}
		label := fmt.Sprintf("node[%d]", i)
		if keyRaw, ok := nodeMap["key"]; ok {
			var keyStr string
			if err := json.Unmarshal(keyRaw, &keyStr); err == nil && keyStr != "" {
				label = fmt.Sprintf("node[%q]", keyStr)
			}
		}
		addUnknownGraphFields(out, label, nodeMap, knownGraphNodeFields)
	}
}

func addUnknownGraphEdgeFields(out map[string][]string, raw json.RawMessage) {
	var rawEdges []json.RawMessage
	if err := json.Unmarshal(raw, &rawEdges); err != nil {
		return
	}
	for i, edgeRaw := range rawEdges {
		var edgeMap map[string]json.RawMessage
		if err := json.Unmarshal(edgeRaw, &edgeMap); err != nil {
			continue
		}
		addUnknownGraphFields(out, fmt.Sprintf("edge[%d]", i), edgeMap, knownGraphEdgeFields)
	}
}

func addUnknownGraphFields(out map[string][]string, location string, fields map[string]json.RawMessage, known map[string]struct{}) {
	if unknown := unknownKeys(fields, known); len(unknown) > 0 {
		out[location] = unknown
	}
}

// unknownKeys returns the keys present in have that are not in known, sorted
// alphabetically for deterministic output. Matching is case-insensitive because
// encoding/json binds case-variant keys (e.g. "Pinned") to the lowercase field.
func unknownKeys(have map[string]json.RawMessage, known map[string]struct{}) []string {
	var unknown []string
	for k := range have {
		if _, ok := known[strings.ToLower(k)]; !ok {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// warnUnknownGraphFields prints a single warning line per location in the
// plan with one or more unknown fields, plus a per-field hint when one is
// available. Output goes to w (typically os.Stderr). Returns the sorted
// list of distinct unknown field names for test assertion; production
// callers may safely ignore the result. (GH#3367)
//
//nolint:unparam // return value used by tests for assertion; production callers ignore
func warnUnknownGraphFields(w io.Writer, unknown map[string][]string) []string {
	if len(unknown) == 0 {
		return nil
	}

	locations := make([]string, 0, len(unknown))
	for loc := range unknown {
		locations = append(locations, loc)
	}
	sort.Strings(locations)

	distinct := make(map[string]struct{})
	for _, loc := range locations {
		fields := append([]string(nil), unknown[loc]...)
		sort.Strings(fields)
		fmt.Fprintf(w, "warning: graph plan %s has unknown field(s): %v (silently dropped — see 'bd create --graph' schema)\n", loc, fields)
		for _, f := range fields {
			distinct[f] = struct{}{}
		}
	}

	hintFields := make([]string, 0, len(distinct))
	for f := range distinct {
		hintFields = append(hintFields, f)
	}
	sort.Strings(hintFields)
	for _, f := range hintFields {
		// Lowercase to match unknownKeys, which reports the original-case
		// spelling against the all-lowercase hint map.
		if hint, ok := graphFieldHints[strings.ToLower(f)]; ok {
			fmt.Fprintf(w, "  hint: %q is not part of the schema; %s\n", f, hint)
		}
	}

	return hintFields
}

func loadEmbeddedCustomTypes() []string {
	if getStore() != nil {
		if ct, err := getStore().GetCustomTypes(getRootContext()); err == nil && len(ct) > 0 {
			return ct
		}
	}
	return config.GetCustomTypesFromYAML()
}

// loadEmbeddedCustomStatuses reads custom statuses from the store only — no
// YAML fallback, matching single-issue create and loadListFilterConfig
// (custom types fall back to YAML; custom statuses deliberately do not).
func loadEmbeddedCustomStatuses() []string {
	if getStore() == nil {
		return nil
	}
	cs, err := getStore().GetCustomStatuses(getRootContext())
	if err != nil {
		return nil
	}
	return cs
}

// createIssuesFromGraph handles `bd create --graph <plan-file>`.
// When dryRun is true, the plan is parsed and validated but no writes occur;
// a preview is emitted to stdout (JSON when jsonOutput is set, otherwise
// human-readable). Unknown plan/node/edge fields are reported to stderr in
// both modes so schema gaps are visible before any writes happen. (GH#3367)
func createIssuesFromGraph(planFile string, dryRun bool, opts GraphApplyOptions) error {
	plan, cfg, err := loadGraphApplyPlan(planFile)
	if err != nil {
		return err
	}
	if _, err := validateFullGraphPlan(plan, cfg, opts, false); err != nil {
		return HandleErrorRespectJSON("invalid graph plan: %v", err)
	}

	if dryRun {
		return emitGraphApplyDryRun(plan, opts)
	}

	result, err := executeGraphApply(getRootContext(), plan, opts)
	if err != nil {
		return HandleErrorRespectJSON("graph create: %v", err)
	}
	return renderGraphApplyResult(result)
}

func loadGraphApplyPlan(planFile string) (*GraphApplyPlan, graphPlanConfig, error) {
	data, err := os.ReadFile(planFile) // #nosec G304 -- user-provided path is intentional
	if err != nil {
		return nil, graphPlanConfig{}, HandleErrorRespectJSON("reading graph plan: %v", err)
	}
	if unknown := detectUnknownGraphFields(data); len(unknown) > 0 {
		warnUnknownGraphFields(os.Stderr, unknown)
	}
	var plan GraphApplyPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, graphPlanConfig{}, HandleErrorRespectJSON("parsing graph plan: %v", err)
	}
	dbPrefix, allowedPrefixes := loadEmbeddedIDPrefixes()
	cfg := graphPlanConfig{
		customTypes:     loadEmbeddedCustomTypes(),
		customStatuses:  loadEmbeddedCustomStatuses(),
		dbPrefix:        dbPrefix,
		allowedPrefixes: allowedPrefixes,
	}
	if getStore() != nil {
		cfg.issueExists = graphApplyIssueExists
	}
	return &plan, cfg, nil
}

func graphApplyIssueExists(id string) (bool, error) {
	if _, err := getStore().GetIssue(getRootContext(), id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func renderGraphApplyResult(result *GraphApplyResult) error {
	if isJSONOutput() {
		return outputJSON(result)
	}
	fmt.Printf("Created %d issues\n", len(result.IDs))
	keys := make([]string, 0, len(result.IDs))
	for key := range result.IDs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Printf("  %s -> %s\n", key, result.IDs[key])
	}
	return nil
}

// emitGraphApplyDryRun prints what `bd create --graph` would do without
// performing any writes. Mirrors the JSON-vs-human split of the live path.
// (GH#3367)
func emitGraphApplyDryRun(plan *GraphApplyPlan, opts GraphApplyOptions) error {
	rows, parentDeps, err := buildGraphApplyDryRunRows(plan, opts)
	if err != nil {
		return err
	}
	preview := GraphApplyDryRun{
		DryRun:          true,
		NodeCount:       len(plan.Nodes),
		EdgeCount:       len(plan.Edges),
		ParentDeps:      parentDeps,
		ValidationNotes: []string{graphApplyDryRunTransactionValidationNote},
		Nodes:           rows,
	}
	return renderGraphApplyDryRun(preview)
}

func buildGraphApplyDryRunRows(plan *GraphApplyPlan, opts GraphApplyOptions) ([]GraphApplyDryRunRow, int, error) {
	parentDeps := 0
	rows := make([]GraphApplyDryRunRow, 0, len(plan.Nodes))
	for _, node := range plan.Nodes {
		// Preview via the same materialization the apply path uses so
		// type/priority/status defaults can't drift. (A defer_until passing
		// between dry-run and apply still shifts the real status to open.)
		row, err := graphApplyDryRunRow(node, opts)
		if err != nil {
			return nil, 0, err
		}
		if row.ParentKey != "" || row.ParentID != "" {
			parentDeps++
		}
		rows = append(rows, row)
	}
	return rows, parentDeps, nil
}

func graphApplyDryRunRow(node GraphApplyNode, opts GraphApplyOptions) (GraphApplyDryRunRow, error) {
	// Preview via the same materialization the apply path uses so
	// type/priority/status defaults can't drift. (A defer_until passing
	// between dry-run and apply still shifts the real status to open.)
	issue, err := graphApplyNodeIssue(node, opts, "", "")
	if err != nil {
		return GraphApplyDryRunRow{}, HandleErrorRespectJSON("invalid graph plan: %v", err)
	}
	status := string(issue.Status)
	if node.Status == "" && issue.Status == types.StatusOpen {
		status = "" // default status: keep the preview row terse
	}
	return GraphApplyDryRunRow{
		Key:       node.Key,
		ID:        node.ID,
		Title:     node.Title,
		Type:      string(issue.IssueType),
		Status:    status,
		Priority:  issue.Priority,
		ParentKey: node.effectiveParentKey(),
		ParentID:  node.ParentID,
	}, nil
}

func renderGraphApplyDryRun(preview GraphApplyDryRun) error {
	if isJSONOutput() {
		return outputJSON(preview)
	}

	fmt.Printf("Dry run: would create %d issue(s) and %d edge(s) (%d parent-child link(s))\n",
		preview.NodeCount, preview.EdgeCount, preview.ParentDeps)
	fmt.Printf("Note: %s.\n", graphApplyDryRunTransactionValidationNote)
	for _, row := range preview.Nodes {
		renderGraphApplyDryRunRow(row)
	}
	return nil
}

func renderGraphApplyDryRunRow(row GraphApplyDryRunRow) {
	extras := ""
	if row.ID != "" {
		extras += fmt.Sprintf(" id=%s", row.ID)
	}
	if row.Status != "" {
		extras += fmt.Sprintf(" status=%s", row.Status)
	}
	switch {
	case row.ParentKey != "":
		extras += fmt.Sprintf(" parent_key=%s", row.ParentKey)
	case row.ParentID != "":
		extras += fmt.Sprintf(" parent_id=%s", row.ParentID)
	}
	fmt.Printf("  %s [%s] P%d %q%s\n", row.Key, row.Type, row.Priority, row.Title, extras)
}

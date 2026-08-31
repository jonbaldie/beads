// Package main implements the bd CLI dependency management commands.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

type bulkDepInput struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Type        string `json:"type"`
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
}

type bulkDepEdge struct {
	Line        int
	IssueID     string
	DependsOnID string
	Type        types.DependencyType
	Store       storage.DoltStorage
	StoreKey    string
	Cleanups    []func()
}

func addBulkDependencies(cmd *cobra.Command, file string, defaultType string) error {
	edges, err := readBulkDepEdges(file, defaultType)
	if err != nil {
		return err
	}

	resolved, err := validateBulkDepEdges(getRootContext(), edges)
	if err != nil {
		return err
	}
	defer cleanupBulkDepEdges(resolved)
	targetStore, err := bulkDependencyTargetStore(resolved)
	if err != nil {
		return err
	}

	noCycleCheck, _ := cmd.Flags().GetBool("no-cycle-check")
	depEdges := dependencyEdgesForBulk(resolved)

	// One request, one transaction, one history entry. The role's
	// all-or-nothing contract is what the hand-rolled bulk transaction used to
	// spell out here, down to the parent-child-first ordering and the
	// whole-graph gate that runs even when the per-edge probe is off — so the
	// request replaces it rather than wrapping it. The version commit comes
	// with the role, which is why there is no transact() and no
	// commitPendingIfEmbedded around this call.
	opsCtx, err := issueOpsContext(getRootContext())
	if err != nil {
		return err
	}
	if err := addDependencyEdgesDirect(opsCtx, targetStore, depEdges, noCycleCheck); err != nil {
		return err
	}

	if !noCycleCheck {
		warnIfCyclesExist(targetStore)
	}
	return renderBulkDependencyAdd(resolved)
}

func cleanupBulkDepEdges(edges []bulkDepEdge) {
	for _, edge := range edges {
		for _, cleanup := range edge.Cleanups {
			cleanup()
		}
	}
}

func bulkDependencyTargetStore(edges []bulkDepEdge) (storage.DoltStorage, error) {
	if len(edges) == 0 {
		return nil, fmt.Errorf("no dependency edges found")
	}
	targetStore := edges[0].Store
	targetStoreKey := edges[0].StoreKey
	for _, edge := range edges[1:] {
		if edge.StoreKey != targetStoreKey {
			return nil, fmt.Errorf("bulk dep add requires all source issues to resolve to the same store")
		}
	}
	return targetStore, nil
}

func dependencyEdgesForBulk(edges []bulkDepEdge) []issueops.DependencyEdge {
	depEdges := make([]issueops.DependencyEdge, 0, len(edges))
	for _, edge := range edges {
		depEdges = append(depEdges, issueops.DependencyEdge{
			IssueID:     edge.IssueID,
			DependsOnID: edge.DependsOnID,
			Type:        edge.Type,
		})
	}
	return depEdges
}

func renderBulkDependencyAdd(edges []bulkDepEdge) error {
	if isJSONOutput() {
		out := make([]map[string]interface{}, 0, len(edges))
		for _, edge := range edges {
			out = append(out, map[string]interface{}{
				"issue_id":      edge.IssueID,
				"depends_on_id": edge.DependsOnID,
				"type":          string(edge.Type),
			})
		}
		return outputJSON(map[string]interface{}{
			"status":       "added",
			"count":        len(edges),
			"dependencies": out,
		})
	}
	fmt.Printf("%s Added %d dependencies\n", ui.RenderPass("✓"), len(edges))
	return nil
}

func readBulkDepEdges(file string, defaultType string) ([]bulkDepEdge, error) {
	r, f, err := openBulkDepReader(file)
	if err != nil {
		return nil, err
	}
	if f != nil {
		defer f.Close()
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var edges []bulkDepEdge
	var errs []string
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		edge, lineErrs, ok := parseBulkDepLine(scanner.Text(), lineNo, defaultType)
		errs = append(errs, lineErrs...)
		if !ok {
			continue
		}
		edges = append(edges, edge)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read dependency file: %w", err)
	}
	if len(errs) > 0 {
		return nil, bulkDepValidationError(errs)
	}
	return edges, nil
}

func openBulkDepReader(file string) (io.Reader, *os.File, error) {
	if file == "-" {
		return os.Stdin, nil, nil
	}
	f, err := os.Open(file) // #nosec G304 -- user-supplied bulk dependency file
	if err != nil {
		return nil, nil, fmt.Errorf("open dependency file: %w", err)
	}
	return f, f, nil
}

func parseBulkDepLine(rawLine string, lineNo int, defaultType string) (bulkDepEdge, []string, bool) {
	line := strings.TrimSpace(rawLine)
	if line == "" {
		return bulkDepEdge{}, nil, false
	}
	var in bulkDepInput
	if err := json.Unmarshal([]byte(line), &in); err != nil {
		return bulkDepEdge{}, []string{fmt.Sprintf("line %d: invalid JSON: %v", lineNo, err)}, false
	}

	from, to, depType := bulkDepLineValues(in, defaultType)
	dt := canonicalDependencyType(types.DependencyType(depType))
	errs := validateBulkDepLine(lineNo, from, to, dt)
	if len(errs) > 0 {
		return bulkDepEdge{}, errs, false
	}
	return bulkDepEdge{Line: lineNo, IssueID: from, DependsOnID: to, Type: dt}, nil, true
}

func bulkDepLineValues(in bulkDepInput, defaultType string) (string, string, string) {
	from := strings.TrimSpace(in.From)
	if from == "" {
		from = strings.TrimSpace(in.IssueID)
	}
	to := strings.TrimSpace(in.To)
	if to == "" {
		to = strings.TrimSpace(in.DependsOnID)
	}
	depType := strings.TrimSpace(in.Type)
	if depType == "" {
		depType = defaultType
	}
	return from, to, depType
}

func validateBulkDepLine(lineNo int, from string, to string, dt types.DependencyType) []string {
	var errs []string
	if from == "" {
		errs = append(errs, fmt.Sprintf("line %d: missing from", lineNo))
	}
	if to == "" {
		errs = append(errs, fmt.Sprintf("line %d: missing to", lineNo))
	}
	if err := validateDependencyType(dt); err != nil {
		errs = append(errs, fmt.Sprintf("line %d: %v", lineNo, err))
	}
	return errs
}

func validateBulkDepEdges(ctx context.Context, edges []bulkDepEdge) ([]bulkDepEdge, error) {
	resolved := make([]bulkDepEdge, 0, len(edges))
	var errs []string

	for _, edge := range edges {
		current, edgeErr, include := resolveBulkDepEdge(ctx, edge)
		if edgeErr != "" {
			errs = append(errs, edgeErr)
		}
		if include {
			resolved = append(resolved, current)
		}
	}

	if len(errs) > 0 {
		for _, edge := range resolved {
			for _, cleanup := range edge.Cleanups {
				cleanup()
			}
		}
		return nil, bulkDepValidationError(errs)
	}
	return resolved, nil
}

func resolveBulkDepEdge(ctx context.Context, edge bulkDepEdge) (bulkDepEdge, string, bool) {
	current, errMsg, ok := resolveBulkDepSource(ctx, edge)
	if !ok {
		return current, errMsg, false
	}
	current, errMsg = resolveBulkDepTarget(ctx, edge, current)
	if errMsg != "" {
		return current, errMsg, true
	}
	if isDisallowedHierarchicalDependency(current.IssueID, current.DependsOnID, current.Type) {
		return current, fmt.Sprintf("line %d: cannot add dependency: %s is already a child of %s", edge.Line, current.IssueID, current.DependsOnID), true
	}
	return current, "", true
}

func resolveBulkDepSource(ctx context.Context, edge bulkDepEdge) (bulkDepEdge, string, bool) {
	current := edge
	// Write-intent: addBulkDependencies writes through current.Store (the
	// source issue's store), so a routed source must open writable (#4141);
	// the depends-on target below stays read-only (bd-6dnrw.32, GH#3231).
	fromID, fromStore, fromCleanup, err := resolveIDForMutation(ctx, getStore(), edge.IssueID)
	if err != nil {
		return current, fmt.Sprintf("line %d: resolving issue ID %s: %v", edge.Line, edge.IssueID, err), false
	}
	current.Cleanups = append(current.Cleanups, fromCleanup)
	current.IssueID = fromID
	current.Store = fromStore
	current.StoreKey = dependencyStoreKey(fromStore)
	return current, "", true
}

func resolveBulkDepTarget(ctx context.Context, edge, current bulkDepEdge) (bulkDepEdge, string) {
	if strings.HasPrefix(edge.DependsOnID, "external:") {
		if err := validateExternalRef(edge.DependsOnID); err != nil {
			return current, fmt.Sprintf("line %d: %v", edge.Line, err)
		}
		current.DependsOnID = edge.DependsOnID
		return current, ""
	}
	toID, _, toCleanup, err := resolveIDWithRouting(ctx, getStore(), edge.DependsOnID)
	if err != nil {
		srcPrefix := types.ExtractPrefix(current.IssueID)
		tgtPrefix := types.ExtractPrefix(edge.DependsOnID)
		if srcPrefix != "" && tgtPrefix != "" && srcPrefix != tgtPrefix {
			current.DependsOnID = edge.DependsOnID
			return current, ""
		}
		return current, fmt.Sprintf("line %d: resolving dependency ID %s: %v", edge.Line, edge.DependsOnID, err)
	}
	current.Cleanups = append(current.Cleanups, toCleanup)
	current.DependsOnID = toID
	return current, ""
}

func bulkDepValidationError(errs []string) error {
	return fmt.Errorf("bulk dependency validation failed:\n  %s", strings.Join(errs, "\n  "))
}

func dependencyStoreKey(s storage.DoltStorage) string {
	if locator, ok := storage.UnwrapStore(s).(storage.StoreLocator); ok {
		if cliDir := strings.TrimSpace(locator.CLIDir()); cliDir != "" {
			return "cli:" + filepath.Clean(cliDir)
		}
		if path := strings.TrimSpace(locator.Path()); path != "" {
			return "path:" + filepath.Clean(path)
		}
	}
	return fmt.Sprintf("instance:%p", s)
}

// depListAnchor is one resolved `bd dep list` argument: the canonical id, the
// store that actually holds it, and the routing handle that has to be closed.

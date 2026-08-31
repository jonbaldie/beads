// Package main implements the bd CLI dependency management commands.
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

// addDependencyEdgesDirect asserts edges through the DependencyEditor role on
// st — the store that owns the source issue.
//
// It is addDependencyEdgesProxied's twin, and reaches the role the same way:
// through the ACCESSOR, never a constructor, because the accessor is where each
// storage decorator adds its layer. Built per write site rather than once per
// command, for the reason writeOps gives — a routed source hands back an editor
// carrying its own stack.
//
// skipPerEdgeCycleCheck is a separate argument from the --no-cycle-check flag
// for the reason the proxied twin states: only the bulk path trades the
// per-edge probe away.
func addDependencyEdgesDirect(ctx context.Context, st storage.DoltStorage, edges []issueops.DependencyEdge, skipPerEdgeCycleCheck bool) error {
	editor, err := st.DependencyEditor()
	if err != nil {
		return err
	}
	_, err = editor.AddDependencies(ctx, issueops.AddDependenciesRequest{
		Actor:                 getActor(),
		Edges:                 edges,
		SkipPerEdgeCycleCheck: skipPerEdgeCycleCheck,
	})
	return err
}

// exactDependencyTarget returns the depends_on_id of a raw dependency edge on
// issueID that equals rawTarget exactly. Used by `bd dep remove` so a bare
// slug that was stored verbatim (pre-GH#5005 create --deps bug) is removed
// instead of being resolved to a different, fully-qualified good edge.
func exactDependencyTarget(ctx context.Context, st storage.DependencyQueryStore, issueID, rawTarget string) (string, bool) {
	if st == nil || rawTarget == "" {
		return "", false
	}
	records, err := st.GetDependencyRecords(ctx, issueID)
	if err != nil {
		return "", false
	}
	for _, r := range records {
		if r != nil && r.DependsOnID == rawTarget {
			return rawTarget, true
		}
	}
	return "", false
}

// resolveIDWithRouting resolves a partial issue ID using prefix-based routing.
// It returns the resolved full ID and the store that contains the issue.
// If the issue routes to a different database, a routed store is returned
// and must be closed by the caller via the returned cleanup function.
// If the issue is in the local store, cleanup is a no-op.
//
// The routed store is opened read-only; callers that mutate the returned store
// (e.g. dep add/remove/link writing through the source issue's store) must use
// resolveIDForMutation instead (GH#3231, #4141).
func resolveIDWithRouting(ctx context.Context, localStore storage.DoltStorage, id string) (resolvedID string, targetStore storage.DoltStorage, cleanup func(), err error) {
	result, err := resolveAndGetIssueWithRouting(ctx, localStore, id)
	if err != nil {
		return "", nil, func() {}, fmt.Errorf("resolving issue ID %s: %w", id, err)
	}
	if result == nil || result.Issue == nil {
		return "", nil, func() {}, fmt.Errorf("no issue found matching %q", id)
	}
	s := result.Store
	if s == nil {
		s = localStore
	}
	return result.ResolvedID, s, func() { result.Close() }, nil
}

// resolveIDForMutation mirrors resolveIDWithRouting but opens prefix-routed
// target stores writable (resolveAndGetIssueForMutation) so mutation commands
// can commit to the routed repository. Its result validation, local-store
// fallback, and cleanup tail must stay aligned with resolveIDWithRouting.
func resolveIDForMutation(ctx context.Context, localStore storage.DoltStorage, id string) (resolvedID string, targetStore storage.DoltStorage, cleanup func(), err error) {
	result, err := resolveAndGetIssueForMutation(ctx, localStore, id)
	if err != nil {
		return "", nil, func() {}, fmt.Errorf("resolving issue ID %s: %w", id, err)
	}
	if result == nil || result.Issue == nil {
		return "", nil, func() {}, fmt.Errorf("no issue found matching %q", id)
	}
	s := result.Store
	if s == nil {
		s = localStore
	}
	return result.ResolvedID, s, func() { result.Close() }, nil
}

// isChildOf returns true if childID is a hierarchical child of parentID.
// For example, "bd-abc.1" is a child of "bd-abc", and "bd-abc.1.2" is a child of "bd-abc.1".
func isChildOf(childID, parentID string) bool {
	_, isAncestor := hierarchicalParentRelation(childID, parentID)
	return isAncestor
}

func hierarchicalParentRelation(childID, targetID string) (immediateParent string, isAncestor bool) {
	// A child ID has the format "parentID.N" or "parentID.N.M" etc.
	// Use ParseHierarchicalID to get the actual parent
	_, actualParentID, depth := types.ParseHierarchicalID(childID)
	if depth == 0 {
		return "", false // Not a hierarchical ID
	}
	// Check if the immediate parent matches
	if actualParentID == targetID {
		return actualParentID, true
	}
	// Also check if targetID is an ancestor (e.g., "bd-abc" is an ancestor of "bd-abc.1.2")
	return actualParentID, strings.HasPrefix(childID, targetID+".")
}

// isDisallowedHierarchicalDependency reports whether an explicit dependency
// conflicts with hierarchy encoded in a dotted issue ID. The one allowed match
// is a parent-child edge to the immediate dotted-ID parent; blocking and other
// edge types to any parent/ancestor, plus parent-child edges to higher ancestors,
// remain rejected.
func isDisallowedHierarchicalDependency(fromID, toID string, depType types.DependencyType) bool {
	immediateParent, isAncestor := hierarchicalParentRelation(fromID, toID)
	if !isAncestor {
		return false
	}
	return depType != types.DepParentChild || toID != immediateParent
}

var depCmd = &cobra.Command{
	Use:     "dep [issue-id]",
	GroupID: "deps",
	Short:   "Manage dependencies",
	Long: `Manage dependencies between issues.

When called with an issue ID and --blocks flag, creates a blocking dependency:
  bd dep <blocker-id> --blocks <blocked-id>

This is equivalent to:
  bd dep add <blocked-id> <blocker-id>

Examples:
  bd dep bd-xyz --blocks bd-abc    # bd-xyz blocks bd-abc
  bd dep add bd-abc bd-xyz         # Same as above (bd-abc depends on bd-xyz)`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("dep")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		blocksID, _ := cmd.Flags().GetString("blocks")

		if len(args) == 0 && blocksID == "" {
			_ = cmd.Help()
			return nil
		}

		if blocksID != "" {
			if len(args) != 1 {
				return HandleErrorRespectJSON("--blocks requires exactly one issue ID argument")
			}
			blockerID := args[0]

			if err := CheckReadonly("dep --blocks"); err != nil {
				return err
			}

			ctx := getRootContext()
			if usesProxiedServer() {
				return runDepBlocksProxiedServer(cmd, ctx, blockerID, blocksID)
			}
			depType := "blocks"

			// Resolve partial IDs with routing support. The source issue's store
			// is mutated below, so resolve it write-intent (#4141); the blocker
			// target is only resolved by ID and stays read-only, so a routed read
			// never opens a foreign project writable or runs open-time migrations
			// against its history (bd-6dnrw.32, GH#3231).
			fromID, fromStore, fromCleanup, err := resolveIDForMutation(ctx, getStore(), blocksID)
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			defer fromCleanup()

			toID, _, toCleanup, err := resolveIDWithRouting(ctx, getStore(), blockerID)
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			defer toCleanup()

			if isDisallowedHierarchicalDependency(fromID, toID, types.DepBlocks) {
				return HandleErrorRespectJSON("cannot add dependency: %s is already a child of %s. Children inherit dependency on parent completion via hierarchy. Adding an explicit dependency would create a deadlock", fromID, toID)
			}

			opsCtx, err := issueOpsContext(ctx)
			if err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			edge := issueops.DependencyEdge{IssueID: fromID, DependsOnID: toID, Type: types.DependencyType(depType)}
			if err := addDependencyEdgesDirect(opsCtx, fromStore, []issueops.DependencyEdge{edge}, false); err != nil {
				return HandleErrorRespectJSON("%v", err)
			}

			noCycleCheck, _ := cmd.Flags().GetBool("no-cycle-check")
			if !noCycleCheck {
				warnIfCyclesExist(fromStore)
			}

			if err := commitPendingIfEmbedded(ctx, fromStore, getActor(), doltAutoCommitParams{
				Command:  "dep add",
				IssueIDs: []string{fromID, toID},
			}); err != nil {
				return HandleErrorRespectJSON("failed to commit: %v", err)
			}

			if isJSONOutput() {
				return outputJSON(map[string]interface{}{
					"status":     "added",
					"blocker_id": toID,
					"blocked_id": fromID,
					"type":       depType,
				})
			}

			fmt.Printf("%s Added dependency: %s blocks %s\n",
				ui.RenderPass("✓"), formatFeedbackIDParen(toID, lookupTitle(toID)), formatFeedbackIDParen(fromID, lookupTitle(fromID)))
			return nil
		}

		_ = cmd.Help()
		return nil
	},
}

var depAddCmd = &cobra.Command{
	Use:   "add [issue-id] [depends-on-id]",
	Short: "Add a dependency",
	Long: `Add a dependency between two issues.

The depends-on-id can be provided as:
  - A positional argument: bd dep add issue-123 issue-456
  - A flag: bd dep add issue-123 --blocked-by issue-456
  - A flag: bd dep add issue-123 --depends-on issue-456

The --blocked-by and --depends-on flags are aliases and both mean "issue-123
depends on (is blocked by) the specified issue."

The depends-on-id can be:
  - A local issue ID (e.g., bd-xyz)
  - An external reference: external:<project>:<capability>

For bulk wiring, pass newline-delimited JSON with --file. Each line must be an
object with "from" and "to" fields, and may include "type". The aliases
"issue_id" and "depends_on_id" are also accepted. Use --file - to read stdin.

External references are stored as-is and resolved at query time using
the external_projects config. They block the issue until the capability
is "shipped" in the target project.

Examples:
  bd dep add bd-42 bd-41                              # Positional args
  bd dep add bd-42 --blocked-by bd-41                 # Flag syntax (same effect)
  bd dep add bd-42 --depends-on bd-41                 # Alias (same effect)
  bd dep add gt-xyz external:beads:mol-run-assignee   # Cross-project dependency
  bd dep add bd-42 bd-41 --no-cycle-check             # Skip cycle check (bulk wiring)
  bd dep add --file deps.jsonl                        # Bulk JSONL: {"from":"bd-42","to":"bd-41"}`,
	Args: func(cmd *cobra.Command, args []string) error {
		file, _ := cmd.Flags().GetString("file")
		blockedBy, _ := cmd.Flags().GetString("blocked-by")
		dependsOn, _ := cmd.Flags().GetString("depends-on")
		hasFlag := blockedBy != "" || dependsOn != ""

		if file != "" {
			if len(args) != 0 {
				return fmt.Errorf("--file cannot be used with positional issue IDs")
			}
			if hasFlag {
				return fmt.Errorf("--file cannot be used with --blocked-by or --depends-on")
			}
			return nil
		}

		if hasFlag {
			// If a flag is provided, we only need 1 positional arg (the dependent issue)
			if len(args) < 1 {
				return fmt.Errorf("requires at least 1 arg(s), only received %d", len(args))
			}
			if len(args) > 1 {
				return fmt.Errorf("cannot use both positional depends-on-id and --blocked-by/--depends-on flag")
			}
			return nil
		}
		// No flag provided, need exactly 2 positional args
		if len(args) != 2 {
			return fmt.Errorf("requires 2 arg(s), only received %d (or use --blocked-by/--depends-on flag)", len(args))
		}
		return nil
	},
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := CheckReadonly("dep add"); err != nil {
			return err
		}

		evt := metrics.NewCommandEvent("dep-add")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if usesProxiedServer() {
			return runDepAddProxiedServer(cmd, getRootContext(), args)
		}

		depType, _ := cmd.Flags().GetString("type")
		file, _ := cmd.Flags().GetString("file")

		if file != "" {
			if err := addBulkDependencies(cmd, file, depType); err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
			return nil
		}

		blockedBy, _ := cmd.Flags().GetString("blocked-by")
		dependsOn, _ := cmd.Flags().GetString("depends-on")

		var dependsOnArg string
		if blockedBy != "" {
			dependsOnArg = blockedBy
		} else if dependsOn != "" {
			dependsOnArg = dependsOn
		} else {
			dependsOnArg = args[1]
		}

		ctx := getRootContext()

		var fromID, toID string

		isExternalRef := strings.HasPrefix(dependsOnArg, "external:")

		// Write-intent: the source issue's store is mutated by AddDependency
		// below, so the routed source must open writable (#4141). The depends-on
		// target is only resolved by ID and stays read-only, so resolving it can
		// never open a foreign project writable (bd-6dnrw.32, GH#3231).
		fromID, fromStore, fromCleanup, err := resolveIDForMutation(ctx, getStore(), args[0])
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		defer fromCleanup()

		if isExternalRef {
			toID = dependsOnArg
			if err := validateExternalRef(toID); err != nil {
				return HandleErrorRespectJSON("%v", err)
			}
		} else {
			var toCleanup func()
			toID, _, toCleanup, err = resolveIDWithRouting(ctx, getStore(), dependsOnArg)
			if err != nil {
				srcPrefix := types.ExtractPrefix(fromID)
				tgtPrefix := types.ExtractPrefix(dependsOnArg)
				if srcPrefix != "" && tgtPrefix != "" && srcPrefix != tgtPrefix {
					toID = dependsOnArg
				} else {
					return HandleErrorRespectJSON("resolving dependency ID %s: %v", dependsOnArg, err)
				}
			} else {
				defer toCleanup()
			}
		}

		dt := canonicalDependencyType(types.DependencyType(depType))
		if isDisallowedHierarchicalDependency(fromID, toID, dt) {
			return HandleErrorRespectJSON("cannot add dependency: %s is already a child of %s. Children inherit dependency on parent completion via hierarchy. Adding an explicit dependency would create a deadlock", fromID, toID)
		}

		if err := validateDependencyType(dt); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		opsCtx, err := issueOpsContext(ctx)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		edge := issueops.DependencyEdge{IssueID: fromID, DependsOnID: toID, Type: dt}
		if err := addDependencyEdgesDirect(opsCtx, fromStore, []issueops.DependencyEdge{edge}, false); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}

		noCycleCheck, _ := cmd.Flags().GetBool("no-cycle-check")
		if !noCycleCheck {
			warnIfCyclesExist(fromStore)
		}

		if err := commitPendingIfEmbedded(ctx, fromStore, getActor(), doltAutoCommitParams{
			Command:  "dep add",
			IssueIDs: []string{fromID, toID},
		}); err != nil {
			return HandleErrorRespectJSON("failed to commit: %v", err)
		}

		if isJSONOutput() {
			return outputJSON(map[string]interface{}{
				"status":        "added",
				"issue_id":      fromID,
				"depends_on_id": toID,
				"type":          string(dt),
			})
		}

		fmt.Printf("%s Added dependency: %s %s %s (%s)\n",
			ui.RenderPass("✓"), formatFeedbackIDParen(fromID, lookupTitle(fromID)), depRelationFor(dt).phrase, formatFeedbackIDParen(toID, lookupTitle(toID)), dt)
		return nil
	},
}

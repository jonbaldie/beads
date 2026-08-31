package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

func newMolSquashCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "squash <molecule-id>",
		Short: "Compress molecule execution into a digest",
		Long: `Squash a molecule's ephemeral children into a single digest issue.

This command collects all ephemeral child issues of a molecule (Ephemeral=true),
generates a summary digest, and promotes the wisps to persistent by
clearing their Wisp flag (or optionally deletes them).

The squash operation:
  1. Loads the molecule and all its children
  2. Filters to only wisps (ephemeral issues with Ephemeral=true)
  3. Generates a digest (summary of work done)
  4. Creates a permanent digest issue (Ephemeral=false)
  5. Clears Wisp flag on children (promotes to persistent)
     OR keeps them with --keep-children (default: delete)

AGENT INTEGRATION:
Use --summary to provide an AI-generated summary. This keeps bd as a pure
tool - the calling agent (orchestrator worker, Claude Code, etc.) is responsible
for generating intelligent summaries. Without --summary, a basic concatenation
of child issue content is used.

This is part of the wisp workflow: spawn creates wisps,
execution happens, squash compresses the trace into an outcome (digest).

Example:
  bd mol squash bd-abc123                    # Squash and promote children
  bd mol squash bd-abc123 --dry-run          # Preview what would be squashed
  bd mol squash bd-abc123 --keep-children    # Keep wisps after digest
  bd mol squash bd-abc123 --summary "Agent-generated summary of work done"`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runMolSquash,
	}
	cmd.Flags().Bool("dry-run", false, "Preview what would be squashed")
	cmd.Flags().Bool("keep-children", false, "Don't delete ephemeral children after squash")
	cmd.Flags().String("summary", "", "Agent-provided summary (bypasses auto-generation)")
	return cmd
}

// SquashResult holds the result of a squash operation
type SquashResult struct {
	MoleculeID    string   `json:"molecule_id"`
	DigestID      string   `json:"digest_id"`
	SquashedIDs   []string `json:"squashed_ids"`
	SquashedCount int      `json:"squashed_count"`
	DeletedCount  int      `json:"deleted_count"`
	KeptChildren  bool     `json:"kept_children"`
	WispSquash    bool     `json:"wisp_squash,omitempty"` // True if this was a wisp→digest squash
}

type molSquashInput struct {
	moleculeArg  string
	dryRun       bool
	keepChildren bool
	summary      string
}

func gatherMolSquashInput(cmd *cobra.Command, args []string) molSquashInput {
	in := molSquashInput{moleculeArg: args[0]}
	in.dryRun, _ = cmd.Flags().GetBool("dry-run")
	in.keepChildren, _ = cmd.Flags().GetBool("keep-children")
	in.summary, _ = cmd.Flags().GetString("summary")
	return in
}

func wispChildrenOf(subgraph *TemplateSubgraph) []*types.Issue {
	var wispChildren []*types.Issue
	for _, issue := range subgraph.Issues {
		if issue.ID == subgraph.Root.ID {
			continue
		}
		if issue.Ephemeral {
			wispChildren = append(wispChildren, issue)
		}
	}
	return wispChildren
}

func renderSquashDryRun(moleculeID string, subgraph *TemplateSubgraph, wispChildren []*types.Issue, keepChildren bool) {
	fmt.Printf("\nDry run: would squash %d ephemeral children of %s\n\n", len(wispChildren), moleculeID)
	fmt.Printf("Root: %s\n", subgraph.Root.Title)
	fmt.Printf("\nWisp children to squash:\n")
	for _, issue := range wispChildren {
		status := string(issue.Status)
		fmt.Printf("  - [%s] %s (%s)\n", status, issue.Title, issue.ID)
	}
	fmt.Printf("\nDigest preview:\n")
	digest := generateDigest(subgraph.Root, wispChildren)
	if len(digest) > 500 {
		fmt.Printf("%s...\n", digest[:500])
	} else {
		fmt.Printf("%s\n", digest)
	}
	if keepChildren {
		fmt.Printf("\n--keep-children: children would NOT be deleted\n")
	} else {
		fmt.Printf("\nChildren would be deleted after digest creation.\n")
	}
}

func renderSquashResult(result *SquashResult) error {
	if isJSONOutput() {
		return outputJSON(result)
	}
	fmt.Printf("%s Squashed molecule: %d children → 1 digest\n", ui.RenderPass("✓"), result.SquashedCount)
	fmt.Printf("  Digest ID: %s\n", result.DigestID)
	if result.DeletedCount > 0 {
		fmt.Printf("  Deleted: %d wisps\n", result.DeletedCount)
	} else if result.KeptChildren {
		fmt.Printf("  Children preserved (--keep-children)\n")
	}
	if result.WispSquash {
		fmt.Printf("  Root auto-closed: %s\n", result.MoleculeID)
	}
	return nil
}

func runMolSquash(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("mol squash"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("mol-squash")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	in := gatherMolSquashInput(cmd, args)

	if usesProxiedServer() {
		return runMolSquashProxiedServer(getRootContext(), in)
	}
	return runMolSquashDirect(in)
}

func runMolSquashDirect(in molSquashInput) error {
	if getStore() == nil {
		return HandleErrorWithHint("no database connection", diagHint())
	}
	moleculeID, err := utils.ResolvePartialID(getRootContext(), getStore(), in.moleculeArg)
	if err != nil {
		return HandleErrorRespectJSON("resolving molecule ID %s: %v", in.moleculeArg, err)
	}
	subgraph, err := loadTemplateSubgraph(getRootContext(), getStore(), moleculeID)
	if err != nil {
		return HandleErrorRespectJSON("loading molecule: %v", err)
	}
	wispChildren := wispChildrenOf(subgraph)
	if len(wispChildren) == 0 {
		return reportNoWispChildren(moleculeID)
	}
	if in.dryRun {
		renderSquashDryRun(moleculeID, subgraph, wispChildren, in.keepChildren)
		return nil
	}
	result, err := squashMolecule(getRootContext(), getStore(), subgraph.Root, wispChildren, in.keepChildren, in.summary, getActor())
	if err != nil {
		return HandleErrorRespectJSON("squashing molecule: %v", err)
	}
	return renderSquashResult(result)
}

func reportNoWispChildren(moleculeID string) error {
	if isJSONOutput() {
		return outputJSON(SquashResult{MoleculeID: moleculeID, SquashedCount: 0})
	}
	fmt.Printf("No ephemeral children found for molecule %s\n", moleculeID)
	return nil
}

// generateDigest creates a summary from the molecule execution
// Tier 2: Simple concatenation of titles and descriptions
// Tier 3 (future): AI-powered summarization using Haiku
func generateDigest(root *types.Issue, children []*types.Issue) string {
	var sb strings.Builder

	sb.WriteString("## Molecule Execution Summary\n\n")
	sb.WriteString(fmt.Sprintf("**Molecule**: %s\n", root.Title))
	sb.WriteString(fmt.Sprintf("**Steps**: %d\n\n", len(children)))

	// Count completed vs other statuses
	completed := 0
	inProgress := 0
	for _, c := range children {
		switch c.Status {
		case types.StatusClosed:
			completed++
		case types.StatusInProgress:
			inProgress++
		}
	}
	sb.WriteString(fmt.Sprintf("**Completed**: %d/%d\n", completed, len(children)))
	if inProgress > 0 {
		sb.WriteString(fmt.Sprintf("**In Progress**: %d\n", inProgress))
	}
	sb.WriteString("\n---\n\n")

	// List each step with its outcome
	sb.WriteString("### Steps\n\n")
	for i, child := range children {
		status := string(child.Status)
		sb.WriteString(fmt.Sprintf("%d. **[%s]** %s\n", i+1, status, child.Title))
		if child.Description != "" {
			// Include first 200 chars of description
			desc := child.Description
			if len(desc) > 200 {
				desc = desc[:200] + "..."
			}
			sb.WriteString(fmt.Sprintf("   %s\n", desc))
		}
		if child.CloseReason != "" {
			sb.WriteString(fmt.Sprintf("   *Outcome: %s*\n", child.CloseReason))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// squashMolecule performs the squash operation
// If summary is provided (non-empty), it's used as the digest content.
// Otherwise, generateDigest() creates a basic concatenation.
// This enables agents to provide AI-generated summaries while keeping bd as a pure tool.
func squashMolecule(ctx context.Context, s storage.DoltStorage, root *types.Issue, children []*types.Issue, keepChildren bool, summary string, actorName string) (*SquashResult, error) {
	if s == nil {
		return nil, fmt.Errorf("no database connection")
	}

	var result *SquashResult
	err := transact(ctx, s, fmt.Sprintf("bd: squash molecule %s", root.ID), func(tx storage.Transaction) error {
		r, err := squashMoleculeInto(ctx, storeMolWriter{DoltStorage: s, tx: tx}, root, children, keepChildren, summary, actorName)
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func squashMoleculeInto(ctx context.Context, w molWriter, root *types.Issue, children []*types.Issue, keepChildren bool, summary string, actorName string) (*SquashResult, error) {
	childIDs := issueIDs(children)
	digestIssue := newSquashDigest(root, children, summary)
	result := &SquashResult{
		MoleculeID:    root.ID,
		SquashedIDs:   childIDs,
		SquashedCount: len(children),
		KeptChildren:  keepChildren,
	}

	if err := w.CreateIssue(ctx, digestIssue, actorName); err != nil {
		return nil, fmt.Errorf("failed to create digest issue: %w", err)
	}
	result.DigestID = digestIssue.ID

	dep := &types.Dependency{
		IssueID:     digestIssue.ID,
		DependsOnID: root.ID,
		Type:        types.DepParentChild,
	}
	if err := w.AddDependency(ctx, dep, actorName); err != nil {
		return nil, fmt.Errorf("failed to link digest to root: %w", err)
	}

	deleted, err := deleteSquashedChildren(ctx, w, childIDs, keepChildren, actorName)
	if err != nil {
		return nil, err
	}
	result.DeletedCount = deleted
	if err := closeSquashedWispRoot(ctx, w, root, result, actorName); err != nil {
		return nil, err
	}
	return result, nil
}

func issueIDs(issues []*types.Issue) []string {
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	return ids
}

func newSquashDigest(root *types.Issue, children []*types.Issue, summary string) *types.Issue {
	if summary == "" {
		summary = generateDigest(root, children)
	}
	now := time.Now()
	return &types.Issue{
		IssueContent:  types.IssueContent{Title: fmt.Sprintf("Digest: %s", root.Title), Description: summary},
		IssueWorkflow: types.IssueWorkflow{Status: types.StatusClosed, Priority: root.Priority, IssueType: types.TypeTask},
		IssueTimes:    types.IssueTimes{CloseReason: fmt.Sprintf("Squashed from %d wisps", len(children)), ClosedAt: &now},
		IssueWisp:     types.IssueWisp{Ephemeral: false},
	}
}

func deleteSquashedChildren(ctx context.Context, w molWriter, childIDs []string, keep bool, actorName string) (int, error) {
	if keep {
		return 0, nil
	}
	for i, id := range childIDs {
		if err := w.DeleteIssue(ctx, id, actorName); err != nil {
			return i, fmt.Errorf("failed to delete child %s: %w", id, err)
		}
	}
	return len(childIDs), nil
}

func closeSquashedWispRoot(ctx context.Context, w molWriter, root *types.Issue, result *SquashResult, actorName string) error {
	if !root.Ephemeral {
		return nil
	}
	reason := fmt.Sprintf("Squashed: %d steps → digest %s", result.SquashedCount, result.DigestID)
	if err := w.CloseIssue(ctx, root.ID, reason, actorName); err != nil {
		return fmt.Errorf("failed to close wisp root %s: %w", root.ID, err)
	}
	if err := w.UpdateIssue(ctx, root.ID, map[string]interface{}{"wisp": false}, actorName); err != nil {
		return fmt.Errorf("failed to clear ephemeral flag on root %s: %w", root.ID, err)
	}
	result.WispSquash = true
	return nil
}

func init() {
	molCmd.AddCommand(newMolSquashCmd())
}

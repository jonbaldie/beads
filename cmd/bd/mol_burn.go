package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

var molBurnCmd = &cobra.Command{
	Use:   "burn <molecule-id> [molecule-id...]",
	Short: "Delete a molecule without creating a digest",
	Long: `Burn a molecule, deleting it without creating a digest.

Unlike squash (which creates a permanent digest before deletion), burn
completely removes the molecule with no trace. Use this for:
  - Abandoned patrol cycles
  - Crashed or failed workflows
  - Test/debug molecules you don't want to preserve

The burn operation differs based on molecule phase:
  - Wisp (ephemeral): Direct delete
  - Mol (persistent): Cascade delete (syncs to remotes)

CAUTION: This is a destructive operation. The molecule's data will be
permanently lost. If you want to preserve a summary, use 'bd mol squash'.

Example:
  bd mol burn bd-abc123              # Delete molecule with no trace
  bd mol burn bd-abc123 --dry-run    # Preview what would be deleted
  bd mol burn bd-abc123 --force      # Skip confirmation
  bd mol burn bd-a1 bd-b2 bd-c3      # Batch delete multiple wisps`,
	Args:          cobra.MinimumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runMolBurn,
}

// BurnResult holds the result of a burn operation
type BurnResult struct {
	MoleculeID   string   `json:"molecule_id"`
	DeletedIDs   []string `json:"deleted_ids"`
	DeletedCount int      `json:"deleted_count"`
}

// BatchBurnResult holds aggregated results when burning multiple molecules
type BatchBurnResult struct {
	Results      []BurnResult `json:"results"`
	TotalDeleted int          `json:"total_deleted"`
	FailedCount  int          `json:"failed_count"`
}

func runMolBurn(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("mol burn"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("mol-burn")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	dryRun, _ := cmd.Flags().GetBool("dry-run")
	force, _ := cmd.Flags().GetBool("force")
	if yes, _ := cmd.Flags().GetBool("yes"); yes {
		force = true
	}

	if usesProxiedServer() {
		return runMolBurnProxiedServer(getRootContext(), args, dryRun, force)
	}

	ctx := getRootContext()

	if getStore() == nil {
		return HandleErrorWithHint("no database connection", diagHint())
	}

	if len(args) == 1 {
		return burnSingleMolecule(ctx, args[0], dryRun, force)
	}

	return burnMultipleMolecules(ctx, args, dryRun, force)
}

func burnSingleMolecule(ctx context.Context, moleculeID string, dryRun, force bool) error {
	resolvedID, err := utils.ResolvePartialID(ctx, getStore(), moleculeID)
	if err != nil {
		return HandleErrorRespectJSON("resolving molecule ID %s: %v", moleculeID, err)
	}

	rootIssue, err := getStore().GetIssue(ctx, resolvedID)
	if err != nil {
		return HandleErrorRespectJSON("loading molecule: %v", err)
	}

	if rootIssue.Ephemeral {
		return burnWispMolecule(ctx, resolvedID, dryRun, force)
	}
	return burnPersistentMolecule(ctx, resolvedID, dryRun, force)
}

func burnMultipleMolecules(ctx context.Context, moleculeIDs []string, dryRun, force bool) error {
	targets := collectBurnTargets(ctx, moleculeIDs)
	if targets.empty() {
		return renderEmptyBatchBurn(targets.failedResolve)
	}
	if dryRun {
		renderBatchBurnDryRun(targets)
		return nil
	}
	if !force && !isJSONOutput() && !confirmBatchBurn(targets) {
		return nil
	}

	batchResult, err := burnBatchTargets(ctx, targets)
	if err != nil {
		return err
	}
	if isJSONOutput() {
		return outputJSON(batchResult)
	}
	fmt.Printf("%s Burned %d molecule(s): %d issues deleted\n", ui.RenderPass("✓"), len(targets.wispIDs)+len(targets.persistentIDs), batchResult.TotalDeleted)
	if batchResult.FailedCount > 0 {
		fmt.Printf("  %d failed\n", batchResult.FailedCount)
	}
	return nil
}

type burnTargets struct {
	wispIDs       []string
	persistentIDs []string
	failedResolve []string
}

func (targets burnTargets) empty() bool {
	return len(targets.wispIDs) == 0 && len(targets.persistentIDs) == 0
}

func collectBurnTargets(ctx context.Context, moleculeIDs []string) burnTargets {
	targets := burnTargets{}
	for _, moleculeID := range moleculeIDs {
		resolvedID, err := utils.ResolvePartialID(ctx, getStore(), moleculeID)
		if err != nil {
			recordBurnTargetFailure(&targets, moleculeID, "resolve", moleculeID, err)
			continue
		}
		issue, err := getStore().GetIssue(ctx, resolvedID)
		if err != nil {
			recordBurnTargetFailure(&targets, moleculeID, "load", resolvedID, err)
			continue
		}
		if issue.Ephemeral {
			targets.wispIDs = append(targets.wispIDs, resolvedID)
		} else {
			targets.persistentIDs = append(targets.persistentIDs, resolvedID)
		}
	}
	return targets
}

func recordBurnTargetFailure(targets *burnTargets, originalID, action, displayID string, err error) {
	if !isJSONOutput() {
		fmt.Fprintf(os.Stderr, "Warning: failed to %s %s: %v\n", action, displayID, err)
	}
	targets.failedResolve = append(targets.failedResolve, originalID)
}

func renderEmptyBatchBurn(failedResolve []string) error {
	if isJSONOutput() {
		return outputJSON(BatchBurnResult{FailedCount: len(failedResolve)})
	}
	fmt.Println("No valid molecules to burn")
	return nil
}

func renderBatchBurnDryRun(targets burnTargets) {
	if isJSONOutput() {
		return
	}
	fmt.Printf("\nDry run: would burn %d wisp(s) and %d persistent molecule(s)\n", len(targets.wispIDs), len(targets.persistentIDs))
	renderBurnTargetIDs("Wisps to delete:", targets.wispIDs)
	renderBurnTargetIDs("Persistent molecules to delete:", targets.persistentIDs)
	renderBurnTargetIDs(fmt.Sprintf("Failed to resolve (%d):", len(targets.failedResolve)), targets.failedResolve)
}

func renderBurnTargetIDs(title string, ids []string) {
	if len(ids) == 0 {
		return
	}
	fmt.Printf("\n%s\n", title)
	for _, id := range ids {
		fmt.Printf("  - %s\n", id)
	}
}

func confirmBatchBurn(targets burnTargets) bool {
	fmt.Printf("About to burn %d wisp(s) and %d persistent molecule(s)\n", len(targets.wispIDs), len(targets.persistentIDs))
	fmt.Printf("This will permanently delete all molecule data with no digest.\n")
	fmt.Printf("\nContinue? [y/N] ")
	var response string
	_, _ = fmt.Scanln(&response)
	if response == "y" || response == "Y" {
		return true
	}
	fmt.Println("Canceled.")
	return false
}

func burnBatchTargets(ctx context.Context, targets burnTargets) (BatchBurnResult, error) {
	result := BatchBurnResult{
		Results:     make([]BurnResult, 0),
		FailedCount: len(targets.failedResolve),
	}
	if len(targets.wispIDs) > 0 {
		burnBatchWisps(ctx, targets.wispIDs, &result)
	}
	for _, id := range targets.persistentIDs {
		subgraph, err := loadTemplateSubgraph(ctx, getStore(), id)
		if err != nil {
			if !isJSONOutput() {
				fmt.Fprintf(os.Stderr, "Warning: failed to load subgraph for %s: %v\n", id, err)
			}
			result.FailedCount++
			continue
		}
		burnResult, err := burnPersistentSubgraph(id, subgraph)
		if err != nil {
			return result, err
		}
		result.TotalDeleted += burnResult.DeletedCount
		result.Results = append(result.Results, burnResult)
	}
	return result, nil
}

func burnBatchWisps(ctx context.Context, ids []string, result *BatchBurnResult) {
	burnResult, err := burnWisps(ctx, getStore(), ids, getActor())
	if err != nil {
		if !isJSONOutput() {
			fmt.Fprintf(os.Stderr, "Error burning wisps: %v\n", err)
		}
		return
	}
	result.TotalDeleted += burnResult.DeletedCount
	result.Results = append(result.Results, *burnResult)
}

func burnPersistentSubgraph(id string, subgraph *TemplateSubgraph) (BurnResult, error) {
	issueIDs := make([]string, 0, len(subgraph.Issues))
	for _, issue := range subgraph.Issues {
		issueIDs = append(issueIDs, issue.ID)
	}
	if err := deleteBatch(nil, issueIDs, true, false, false, false, false, "mol burn"); err != nil {
		return BurnResult{}, HandleErrorRespectJSON("%v", err)
	}
	return BurnResult{MoleculeID: id, DeletedIDs: issueIDs, DeletedCount: len(issueIDs)}, nil
}

func burnWispMolecule(ctx context.Context, resolvedID string, dryRun, force bool) error {
	subgraph, err := loadTemplateSubgraph(ctx, getStore(), resolvedID)
	if err != nil {
		return HandleErrorRespectJSON("loading wisp molecule: %v", err)
	}

	wispIDs := collectWispIDs(subgraph)

	if len(wispIDs) == 0 {
		return renderEmptyWispBurn(resolvedID)
	}

	if dryRun {
		renderWispBurnDryRun(resolvedID, subgraph, wispIDs)
		return nil
	}

	if !force && !isJSONOutput() && !confirmWispBurn(resolvedID, len(wispIDs)) {
		return nil
	}

	result, err := burnWisps(ctx, getStore(), wispIDs, getActor())
	if err != nil {
		return HandleErrorRespectJSON("burning wisp: %v", err)
	}
	result.MoleculeID = resolvedID

	return renderWispBurnResult(result, resolvedID)
}

func burnPersistentMolecule(ctx context.Context, resolvedID string, dryRun, force bool) error {
	subgraph, err := loadTemplateSubgraph(ctx, getStore(), resolvedID)
	if err != nil {
		return HandleErrorRespectJSON("loading molecule: %v", err)
	}

	issueIDs := collectIssueIDs(subgraph)

	if len(issueIDs) == 0 {
		return renderEmptyPersistentBurn(resolvedID)
	}

	if dryRun {
		renderPersistentBurnDryRun(resolvedID, subgraph, issueIDs)
		return nil
	}

	if !force && !isJSONOutput() && !confirmPersistentBurn(resolvedID, len(issueIDs)) {
		return nil
	}

	if err := deleteBatch(nil, issueIDs, true, false, false, isJSONOutput(), false, "mol burn"); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	return nil
}

func collectWispIDs(subgraph *TemplateSubgraph) []string {
	wispIDs := make([]string, 0, len(subgraph.Issues))
	for _, issue := range subgraph.Issues {
		if issue.Ephemeral {
			wispIDs = append(wispIDs, issue.ID)
		}
	}
	return wispIDs
}

func collectIssueIDs(subgraph *TemplateSubgraph) []string {
	issueIDs := make([]string, 0, len(subgraph.Issues))
	for _, issue := range subgraph.Issues {
		issueIDs = append(issueIDs, issue.ID)
	}
	return issueIDs
}

func renderEmptyWispBurn(resolvedID string) error {
	if isJSONOutput() {
		return outputJSON(BurnResult{MoleculeID: resolvedID, DeletedCount: 0})
	}
	fmt.Printf("No wisp issues found for molecule %s\n", resolvedID)
	return nil
}

func renderEmptyPersistentBurn(resolvedID string) error {
	if isJSONOutput() {
		return outputJSON(BurnResult{MoleculeID: resolvedID, DeletedCount: 0})
	}
	fmt.Printf("No issues found for molecule %s\n", resolvedID)
	return nil
}

func renderWispBurnResult(result *BurnResult, resolvedID string) error {
	if isJSONOutput() {
		return outputJSON(result)
	}
	fmt.Printf("%s Burned wisp: %d issues deleted\n", ui.RenderPass("✓"), result.DeletedCount)
	fmt.Printf("  Ephemeral: %s\n", resolvedID)
	fmt.Printf("  No digest created.\n")
	return nil
}

func renderWispBurnDryRun(resolvedID string, subgraph *TemplateSubgraph, wispIDs []string) {
	fmt.Printf("\nDry run: would burn wisp %s\n\n", resolvedID)
	fmt.Printf("Root: %s\n", subgraph.Root.Title)
	fmt.Printf("\nWisp issues to delete (%d total):\n", len(wispIDs))
	for _, issue := range subgraph.Issues {
		if issue.Ephemeral {
			renderBurnIssue(issue, subgraph.Root.ID)
		}
	}
	fmt.Printf("\nNo digest will be created (use 'bd mol squash' to create one).\n")
}

func renderPersistentBurnDryRun(resolvedID string, subgraph *TemplateSubgraph, issueIDs []string) {
	fmt.Printf("\nDry run: would burn mol %s\n\n", resolvedID)
	fmt.Printf("Root: %s\n", subgraph.Root.Title)
	fmt.Printf("\nIssues to delete (%d total):\n", len(issueIDs))
	for _, issue := range subgraph.Issues {
		renderBurnIssue(issue, subgraph.Root.ID)
	}
	fmt.Printf("\nNote: Persistent mol - deletions sync to remotes.\n")
	fmt.Printf("No digest will be created (use 'bd mol squash' to create one).\n")
}

func renderBurnIssue(issue *types.Issue, rootID string) {
	status := string(issue.Status)
	if issue.ID == rootID {
		fmt.Printf("  - [%s] %s (%s) [ROOT]\n", status, issue.Title, issue.ID)
		return
	}
	fmt.Printf("  - [%s] %s (%s)\n", status, issue.Title, issue.ID)
}

func confirmWispBurn(resolvedID string, issueCount int) bool {
	fmt.Printf("About to burn wisp %s (%d issues)\n", resolvedID, issueCount)
	fmt.Printf("This will permanently delete all wisp data with no digest.\n")
	fmt.Printf("Use 'bd mol squash' instead if you want to preserve a summary.\n")
	return confirmBurn()
}

func confirmPersistentBurn(resolvedID string, issueCount int) bool {
	fmt.Printf("About to burn mol %s (%d issues)\n", resolvedID, issueCount)
	fmt.Printf("This will permanently delete all molecule data with no digest.\n")
	fmt.Printf("Note: Persistent mol - deletions sync to remotes.\n")
	fmt.Printf("Use 'bd mol squash' instead if you want to preserve a summary.\n")
	return confirmBurn()
}

func confirmBurn() bool {
	fmt.Printf("\nContinue? [y/N] ")
	var response string
	_, _ = fmt.Scanln(&response)
	if response == "y" || response == "Y" {
		return true
	}
	fmt.Println("Canceled.")
	return false
}

// burnWisps deletes all wisp issues atomically within a single transaction.
// If any delete fails, the entire operation is rolled back to prevent partial deletion.
func burnWisps(ctx context.Context, s storage.DoltStorage, ids []string, actorName string) (*BurnResult, error) {
	var result *BurnResult
	err := transact(ctx, s, "bd: burn wisps", func(tx storage.Transaction) error {
		r, err := burnWispsInto(ctx, storeMolWriter{DoltStorage: s, tx: tx}, ids, actorName)
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

func burnWispsInto(ctx context.Context, w molWriter, ids []string, actorName string) (*BurnResult, error) {
	result := &BurnResult{
		DeletedIDs: make([]string, 0, len(ids)),
	}
	for _, id := range ids {
		if err := w.DeleteIssue(ctx, id, actorName); err != nil {
			return nil, fmt.Errorf("failed to delete wisp %s: %w", id, err)
		}
		result.DeletedIDs = append(result.DeletedIDs, id)
		result.DeletedCount++
	}
	return result, nil
}

func init() {
	molBurnCmd.Flags().Bool("dry-run", false, "Preview what would be deleted")
	molBurnCmd.Flags().Bool("force", false, "Skip confirmation prompt")
	molBurnCmd.Flags().BoolP("yes", "y", false, "Alias for --force (skip confirmation)")
	_ = molBurnCmd.Flags().MarkHidden("yes")

	molCmd.AddCommand(molBurnCmd)
}

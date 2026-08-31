package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	storeissueops "github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

func newDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <issue-id> [issue-id...]",
		GroupID: "issues",
		Short:   "Delete one or more issues and clean up references",
		Long: `Delete one or more issues and clean up all references to them.
This command will:
1. Remove all dependency links (any type, both directions) involving the issues
2. Update text references to "[deleted:ID]" in directly connected issues
3. Permanently delete the issues from the database

This is a destructive operation that cannot be undone. Use with caution.

BATCH DELETION:
Delete multiple issues at once:
  bd delete bd-1 bd-2 bd-3 --force

Delete from file (one ID per line):
  bd delete --from-file deletions.txt --force

Preview before deleting:
  bd delete --from-file deletions.txt --dry-run

DEPENDENCY HANDLING (the same on a local database and against a team server):
Default: Fails if any issue has dependents not in deletion set
  bd delete bd-1 bd-2

Cascade: Recursively delete all dependents
  bd delete bd-1 --cascade --force

Force: Delete and orphan dependents
  bd delete bd-1 --force`,
		Args:          cobra.MinimumNArgs(0),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runDeleteCommand,
	}
}

type deleteCommandOptions struct {
	fromFile string
	force    bool
	dryRun   bool
	cascade  bool
}

func runDeleteCommand(cmd *cobra.Command, args []string) error {
	if err := CheckReadonly("delete"); err != nil {
		return err
	}
	evt := metrics.NewCommandEvent("delete")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if usesProxiedServer() {
		return runDeleteProxiedServer(cmd, getRootContext(), args)
	}
	options := readDeleteCommandOptions(cmd)
	issueIDs, err := loadAndValidateDeleteIDs(cmd, args, options.fromFile)
	if err != nil {
		return err
	}
	if err := ensureDeleteStore(); err != nil {
		return err
	}
	if len(issueIDs) > 1 || options.cascade {
		return runDeleteBatchCommand(cmd, issueIDs, options)
	}
	return runDeleteSingleCommand(getRootContext(), issueIDs[0], options)
}

func loadAndValidateDeleteIDs(cmd *cobra.Command, args []string, fromFile string) ([]string, error) {
	issueIDs, err := loadDeleteCommandIDs(args, fromFile)
	if err != nil {
		return nil, HandleError("reading file: %v", err)
	}
	if len(issueIDs) == 0 {
		_ = cmd.Usage()
		return nil, HandleError("no issue IDs provided")
	}
	return issueIDs, nil
}

func ensureDeleteStore() error {
	if getStore() != nil {
		return nil
	}
	if err := ensureStoreActive(); err != nil {
		return HandleError("%v", err)
	}
	return nil
}

func readDeleteCommandOptions(cmd *cobra.Command) deleteCommandOptions {
	fromFile, _ := cmd.Flags().GetString("from-file")
	force, _ := cmd.Flags().GetBool("force")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	cascade, _ := cmd.Flags().GetBool("cascade")
	return deleteCommandOptions{fromFile: fromFile, force: force, dryRun: dryRun, cascade: cascade}
}

func loadDeleteCommandIDs(args []string, fromFile string) ([]string, error) {
	issueIDs := make([]string, 0, len(args))
	issueIDs = append(issueIDs, args...)
	if fromFile != "" {
		fileIDs, err := readIssueIDsFromFile(fromFile)
		if err != nil {
			return nil, err
		}
		issueIDs = append(issueIDs, fileIDs...)
	}
	return uniqueStrings(issueIDs), nil
}

func runDeleteBatchCommand(cmd *cobra.Command, issueIDs []string, options deleteCommandOptions) error {
	if err := deleteBatch(cmd, issueIDs, options.force, options.dryRun, options.cascade, isJSONOutput(), false); err != nil {
		if _, ok := exitCodeFromError(err); ok {
			return err
		}
		return HandleError("%v", err)
	}
	return nil
}

func runDeleteSingleCommand(ctx context.Context, requestedID string, options deleteCommandOptions) error {
	// Get the issue to be deleted, using prefix-based routing. Resolution stays
	// in the front door: DeleteRequest.IDs are exact after this point.
	routedResult, err := resolveAndGetIssueForMutation(ctx, getStore(), requestedID)
	if err != nil {
		if isNotFoundErr(err) {
			return HandleError("issue %s not found", requestedID)
		}
		return HandleError("%v", err)
	}
	defer routedResult.Close()
	issueID := routedResult.ResolvedID
	request := issueops.DeleteRequest{
		Actor:  getActor(),
		IDs:    []string{issueID},
		Force:  options.force,
		DryRun: options.dryRun || !options.force,
	}
	result, err := executeSingleDelete(ctx, routedResult.Store, request)
	if request.DryRun {
		return handleSingleDeletePreview(ctx, routedResult.Store, issueID, routedResult.Issue, options.dryRun, isJSONOutput(), result, err)
	}
	if err != nil {
		return HandleError("deleting issue: %v", err)
	}
	commandDidWrite.Store(true)
	return renderSingleDeleteSuccess(issueID, result, isJSONOutput())
}

func handleSingleDeletePreview(ctx context.Context, activeStore storage.DoltStorage, issueID string, issue *types.Issue, dryRun, jsonOutput bool, result issueops.DeleteResult, deleteErr error) error {
	issues := map[string]*types.Issue{issueID: issue}
	if deleteErr != nil {
		if previewErr := outputDeletionPreview([]string{issueID}, issues, false, dryRun, nil, deleteErr, jsonOutput); previewErr != nil {
			return previewErr
		}
		if jsonOutput {
			return outputJSONError(deleteErr, "")
		}
		return HandleError("previewing deletion: %v", deleteErr)
	}
	if jsonOutput || isQuiet() {
		return outputDeletionPreview([]string{issueID}, issues, false, dryRun, &result, nil, jsonOutput)
	}
	return renderSingleDeletePreview(ctx, activeStore, issueID, issue, dryRun, result)
}

func executeSingleDelete(ctx context.Context, activeStore storage.DoltStorage, request issueops.DeleteRequest) (issueops.DeleteResult, error) {
	deleter, err := activeStore.Deleter()
	if err != nil {
		return issueops.DeleteResult{}, HandleError("%v", err)
	}
	opsCtx, err := issueOpsContext(ctx)
	if err != nil {
		return issueops.DeleteResult{}, HandleError("%v", err)
	}
	return deleter.Delete(opsCtx, request)
}

func renderSingleDeleteSuccess(issueID string, result issueops.DeleteResult, jsonOutput bool) error {
	if jsonOutput {
		return outputJSON(map[string]interface{}{
			"deleted":              issueID,
			"dependencies_removed": result.Dependencies,
			"references_updated":   result.ReferencesUpdated,
		})
	}
	fmt.Printf("%s Deleted %s\n", ui.RenderPass("✓"), issueID)
	fmt.Printf("  Removed %d dependency link(s)\n", result.Dependencies)
	fmt.Printf("  Updated text references in %d issue(s)\n", result.ReferencesUpdated)
	return nil
}

// renderSingleDeletePreview prints the human preview for a one-issue delete. The
// counts come from the role's dry run; the edge listing and the "which neighbors
// cite this id" lines are reads this handler makes, because a delete answers
// with an effect rather than with rows.
func renderSingleDeletePreview(
	ctx context.Context, activeStore storage.DoltStorage,
	issueID string, issue *types.Issue, dryRun bool, result issueops.DeleteResult,
) error {
	preview, err := loadSingleDeletePreview(ctx, activeStore, issueID)
	if err != nil {
		return err
	}
	renderSingleDeleteHeader(issueID, issue)
	renderSingleDeleteDependencies(preview.depRecords, preview.dependents, issueID)
	renderSingleDeleteReferences(preview.connectedIssues, issueID)
	renderSingleDeleteOutcome(dryRun, result, issueID)
	return nil
}

type singleDeletePreview struct {
	connectedIssues map[string]*types.Issue
	dependents      []*types.Issue
	depRecords      []*types.Dependency
}

func loadSingleDeletePreview(ctx context.Context, activeStore storage.DoltStorage, issueID string) (singleDeletePreview, error) {
	preview := singleDeletePreview{connectedIssues: make(map[string]*types.Issue)}
	deps, err := activeStore.GetDependencies(ctx, issueID)
	if err != nil {
		return singleDeletePreview{}, HandleError("getting dependencies: %v", err)
	}
	for _, dep := range deps {
		preview.connectedIssues[dep.ID] = dep
	}
	preview.dependents, err = activeStore.GetDependents(ctx, issueID)
	if err != nil {
		return singleDeletePreview{}, HandleError("getting dependents: %v", err)
	}
	for _, dependent := range preview.dependents {
		preview.connectedIssues[dependent.ID] = dependent
	}
	preview.depRecords, err = activeStore.GetDependencyRecords(ctx, issueID)
	if err != nil {
		return singleDeletePreview{}, HandleError("getting dependency records: %v", err)
	}
	return preview, nil
}

func renderSingleDeleteHeader(issueID string, issue *types.Issue) {
	fmt.Printf("\n%s\n", ui.RenderFail("⚠️  DELETE PREVIEW"))
	fmt.Printf("\nIssue to delete:\n")
	fmt.Printf("  %s: %s\n", issueID, issue.Title)
}

func renderSingleDeleteDependencies(depRecords []*types.Dependency, dependents []*types.Issue, issueID string) {
	totalDeps := len(depRecords) + len(dependents)
	if totalDeps == 0 {
		return
	}
	fmt.Printf("\nDependency links to remove: %d\n", totalDeps)
	for _, dep := range depRecords {
		fmt.Printf("  %s → %s (%s)\n", dep.IssueID, dep.DependsOnID, dep.Type)
	}
	for _, dep := range dependents {
		fmt.Printf("  %s → %s (inbound)\n", dep.ID, issueID)
	}
}

func renderSingleDeleteReferences(connectedIssues map[string]*types.Issue, issueID string) {
	if len(connectedIssues) == 0 {
		return
	}
	fmt.Printf("\nConnected issues where text references will be updated:\n")
	re := storeissueops.DeletedReferencePattern(issueID)
	issuesWithRefs := 0
	for id, connIssue := range connectedIssues {
		if !issueHasDeletedReference(connIssue, re) {
			continue
		}
		fmt.Printf("  %s: %s\n", id, connIssue.Title)
		issuesWithRefs++
	}
	if issuesWithRefs == 0 {
		fmt.Printf("  (none have text references)\n")
	}
}

func issueHasDeletedReference(issue *types.Issue, re interface{ MatchString(string) bool }) bool {
	return re.MatchString(issue.Description) ||
		(issue.Notes != "" && re.MatchString(issue.Notes)) ||
		(issue.Design != "" && re.MatchString(issue.Design)) ||
		(issue.AcceptanceCriteria != "" && re.MatchString(issue.AcceptanceCriteria))
}

func renderSingleDeleteOutcome(dryRun bool, result issueops.DeleteResult, issueID string) {
	if dryRun {
		fmt.Printf("\nWould delete: %d issues\n", result.Deleted)
		fmt.Printf("Would remove: %d dependencies, %d labels, %d events\n", result.Dependencies, result.Labels, result.Events)
		if len(result.Orphaned) > 0 {
			fmt.Printf("Would orphan: %d issues\n", len(result.Orphaned))
		}
		fmt.Printf("\n(Dry-run mode - no changes made)\n")
		return
	}
	fmt.Printf("\n%s\n", ui.RenderWarn("This operation cannot be undone!"))
	fmt.Printf("To proceed, run: %s\n\n", ui.RenderWarn("bd delete "+issueID+" --force"))
}

// deleteIssue removes an issue from the database.
func deleteIssue(ctx context.Context, issueID string) error {
	return getStore().DeleteIssue(ctx, issueID)
}

// deleteBatch is the multi-id and cascade path, shared by `bd delete`,
// `bd cleanup`, `bd wisp gc` and `bd mol burn`.
//
// It resolves the ids the way this front door always has - prefix matching and
// cross-repository routing, which issueops.DeleteRequest deliberately does not
// do - and hands the RESOLVED ids to the role.
//
//nolint:unparam // cmd parameter required for potential future use
func deleteBatch(_ *cobra.Command, issueIDs []string, force bool, dryRun bool, cascade bool, jsonOutput bool, _ bool, _ ...string) error {
	if getStore() == nil {
		if err := ensureStoreActive(); err != nil {
			return err
		}
	}
	ctx := getRootContext()
	targets, err := resolveDeleteBatchTargets(ctx, issueIDs)
	if err != nil {
		return err
	}
	if targets.routedStore != nil {
		defer func() { _ = targets.routedStore.Close() }()
	}
	// --force is the confirmation as well as the orphan mode, so an unconfirmed
	// run asks the role what it WOULD do; see the single-id path.
	request := issueops.DeleteRequest{
		Actor:   getActor(),
		IDs:     targets.resolvedIDs,
		Cascade: cascade,
		Force:   force,
		DryRun:  dryRun || !force,
	}
	result, err := executeDeleteBatch(ctx, targets.store(), request)
	if request.DryRun {
		return handleDeleteBatchPreview(targets, cascade, dryRun, jsonOutput, result, err)
	}
	if err != nil {
		return err
	}

	commandDidWrite.Store(true)

	// NO COMMIT COMPENSATION HERE, for the reason the single-id path gives:
	// the role versions the deletion itself, on the store the rows were
	// deleted from, and defers it in batch and off modes.
	return renderDeleteBatchSuccess(targets.resolvedIDs, result, jsonOutput)
}

type deleteBatchTargets struct {
	issues      map[string]*types.Issue
	resolvedIDs []string
	routedStore storage.DoltStorage
}

func (t deleteBatchTargets) store() storage.DoltStorage {
	if t.routedStore != nil {
		return t.routedStore
	}
	return getStore()
}

func resolveDeleteBatchTargets(ctx context.Context, issueIDs []string) (deleteBatchTargets, error) {
	targets := deleteBatchTargets{
		issues:      make(map[string]*types.Issue),
		resolvedIDs: make([]string, 0, len(issueIDs)),
	}
	var notFound []string
	for _, id := range issueIDs {
		result, err := resolveAndGetIssueForMutation(ctx, getStore(), id)
		if err != nil {
			if isNotFoundErr(err) {
				notFound = append(notFound, id)
				continue
			}
			return deleteBatchTargets{}, fmt.Errorf("getting issue %s: %v", id, err)
		}
		addDeleteBatchTarget(&targets, result)
	}
	if len(notFound) > 0 {
		if targets.routedStore != nil {
			_ = targets.routedStore.Close()
		}
		return deleteBatchTargets{}, fmt.Errorf("issues not found: %s", strings.Join(notFound, ", "))
	}
	return targets, nil
}

func addDeleteBatchTarget(targets *deleteBatchTargets, result *RoutedResult) {
	targets.issues[result.ResolvedID] = result.Issue
	targets.resolvedIDs = append(targets.resolvedIDs, result.ResolvedID)
	if result.Routed && targets.routedStore == nil {
		targets.routedStore = result.Store
		return
	}
	result.Close()
}

func executeDeleteBatch(ctx context.Context, batchStore storage.DoltStorage, request issueops.DeleteRequest) (issueops.DeleteResult, error) {
	deleter, err := batchStore.Deleter()
	if err != nil {
		return issueops.DeleteResult{}, err
	}
	opsCtx, err := issueOpsContext(ctx)
	if err != nil {
		return issueops.DeleteResult{}, HandleError("%v", err)
	}
	return deleter.Delete(opsCtx, request)
}

func handleDeleteBatchPreview(targets deleteBatchTargets, cascade, dryRun, jsonOutput bool, result issueops.DeleteResult, depError error) error {
	if depError != nil {
		if previewErr := outputDeletionPreview(targets.resolvedIDs, targets.issues, cascade, dryRun, nil, depError, jsonOutput); previewErr != nil {
			return previewErr
		}
		if jsonOutput {
			return outputJSONError(depError, "")
		}
		return depError
	}
	if previewErr := outputDeletionPreview(targets.resolvedIDs, targets.issues, cascade, dryRun, &result, nil, jsonOutput); previewErr != nil {
		return previewErr
	}
	if !dryRun && !jsonOutput && !isQuiet() {
		renderDeleteBatchConfirmation(targets.resolvedIDs, cascade)
	}
	return nil
}

func renderDeleteBatchConfirmation(resolvedIDs []string, cascade bool) {
	fmt.Printf("\n%s\n", ui.RenderWarn("This operation cannot be undone!"))
	if cascade {
		fmt.Printf("To proceed with cascade deletion, run: %s\n",
			ui.RenderWarn("bd delete "+strings.Join(resolvedIDs, " ")+" --cascade --force"))
		return
	}
	fmt.Printf("To proceed, run: %s\n",
		ui.RenderWarn("bd delete "+strings.Join(resolvedIDs, " ")+" --force"))
}

func renderDeleteBatchSuccess(resolvedIDs []string, result issueops.DeleteResult, jsonOutput bool) error {
	if jsonOutput {
		return outputJSON(map[string]interface{}{
			"deleted":              resolvedIDs,
			"deleted_count":        result.Deleted,
			"dependencies_removed": result.Dependencies,
			"labels_removed":       result.Labels,
			"events_removed":       result.Events,
			"references_updated":   result.ReferencesUpdated,
			"orphaned_issues":      result.Orphaned,
		})
	}
	fmt.Printf("%s Deleted %d issue(s)\n", ui.RenderPass("✓"), result.Deleted)
	fmt.Printf("  Removed %d dependency link(s)\n", result.Dependencies)
	fmt.Printf("  Removed %d label(s)\n", result.Labels)
	fmt.Printf("  Removed %d event(s)\n", result.Events)
	fmt.Printf("  Updated text references in %d issue(s)\n", result.ReferencesUpdated)
	if len(result.Orphaned) > 0 {
		fmt.Printf("  %s Orphaned %d issue(s): %s\n",
			ui.RenderWarn("⚠"), len(result.Orphaned), strings.Join(result.Orphaned, ", "))
	}
	return nil
}

// outputDeletionPreview renders a deletion preview without exposing issue
// payloads in machine-readable or quiet output.
//
// result is the ROLE's dry run, or nil when the role refused: printing zeros
// beside a refusal would read as "nothing would have been deleted" rather than
// "we did not get that far".
func outputDeletionPreview(issueIDs []string, issues map[string]*types.Issue, cascade bool, dryRun bool, result *issueops.DeleteResult, depError error, jsonOutput bool) error {
	if jsonOutput {
		return outputJSONDeletionPreview(issueIDs, cascade, dryRun, result, depError)
	}
	if isQuiet() {
		return nil
	}

	fmt.Printf("\n%s\n", ui.RenderFail("⚠️  DELETE PREVIEW"))
	fmt.Printf("\nIssues to delete (%d):\n", len(issueIDs))
	renderDeletePreviewIssues(issueIDs, issues)
	if cascade {
		fmt.Printf("\n%s Cascade mode enabled - will also delete all dependent issues\n", ui.RenderWarn("⚠"))
	}
	if depError != nil {
		fmt.Printf("\n%s\n", ui.RenderFail(depError.Error()))
	}
	if result != nil {
		fmt.Printf("\nWould delete: %d issues\n", result.Deleted)
		fmt.Printf("Would remove: %d dependencies, %d labels, %d events\n",
			result.Dependencies, result.Labels, result.Events)
		if len(result.Orphaned) > 0 {
			fmt.Printf("Would orphan: %d issues\n", len(result.Orphaned))
		}
		if dryRun {
			fmt.Printf("\n(Dry-run mode - no changes made)\n")
		}
	}
	return nil
}

func outputJSONDeletionPreview(issueIDs []string, cascade, dryRun bool, result *issueops.DeleteResult, depError error) error {
	preview := map[string]interface{}{
		"preview":   true,
		"dry_run":   dryRun,
		"issue_ids": issueIDs,
		"cascade":   cascade,
	}
	if result != nil {
		preview["would_delete"] = result.Deleted
		preview["would_remove_dependencies"] = result.Dependencies
		preview["would_remove_labels"] = result.Labels
		preview["would_remove_events"] = result.Events
		preview["would_orphan"] = len(result.Orphaned)
	}
	if depError != nil {
		preview["error"] = depError.Error()
	}
	return outputJSON(preview)
}

func renderDeletePreviewIssues(issueIDs []string, issues map[string]*types.Issue) {
	for _, id := range issueIDs {
		issue := issues[id]
		if issue == nil {
			continue
		}
		fmt.Printf("  %s: %s\n", id, issue.Title)
	}
}

// readIssueIDsFromFile reads issue IDs from a file (one per line)
func readIssueIDsFromFile(filename string) ([]string, error) {
	// #nosec G304 - user-provided file path is intentional
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var ids []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ids = append(ids, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// uniqueStrings removes duplicates from a slice of strings
func uniqueStrings(slice []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(slice))
	for _, s := range slice {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

func init() {
	cmd := newDeleteCommand()
	cmd.Flags().BoolP("force", "f", false, "Actually delete (without this flag, shows preview)")
	cmd.Flags().String("from-file", "", "Read issue IDs from file (one per line)")
	cmd.Flags().Bool("dry-run", false, "Preview what would be deleted without making changes")
	cmd.Flags().Bool("cascade", false, "Recursively delete all dependent issues")
	cmd.ValidArgsFunction = issueIDCompletion
	rootCmd.AddCommand(cmd)
}

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
	"github.com/spf13/cobra"
)

var migrateCmd = &cobra.Command{
	Use:     "migrate",
	GroupID: "maint",
	Short:   "Database migration commands",
	Long: `Database migration and data transformation commands.

Without subcommand, checks and updates database metadata to current version.

Subcommands:
  hooks                            Plan git hook migration to marker-managed format
  issues                           Move issues between repositories
  schema                           Apply pending schema migrations (idempotent)
  sync                             Set up sync.branch workflow for multi-clone setups
  from-server-to-proxied-server           [EXPERIMENTAL] Switch server mode to proxied-server mode
  from-proxied-server-to-server           [EXPERIMENTAL] Switch proxied-server mode to server mode
  from-shared-server-to-proxied-server    [EXPERIMENTAL] Switch shared-server mode to proxied-server mode
  from-proxied-server-to-shared-server    [EXPERIMENTAL] Switch proxied-server mode to shared-server mode

On a remote-backed database with pending schema migrations bd refuses to
migrate in place (#4259): migrating two clones independently forks the schema
so bd dolt pull can no longer merge — the break is silent and unrecoverable.
Use --force to confirm you are the single designated migrator, after which you
should publish the migrated schema with 'bd dolt push'. The env-var equivalent
BD_ALLOW_REMOTE_MIGRATE=1 remains supported for scripted/CI use.
`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if usesProxiedServer() {
			return HandleErrorRespectJSON("migrate is not supported in proxied-server mode")
		}
		evt := metrics.NewCommandEvent("migrate")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		autoYes, _ := cmd.Flags().GetBool("yes")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		updateRepoID, _ := cmd.Flags().GetBool("update-repo-id")
		inspect, _ := cmd.Flags().GetBool("inspect")

		if !dryRun && !inspect {
			if err := CheckReadonly("migrate"); err != nil {
				return err
			}
		}

		if updateRepoID {
			return handleUpdateRepoID(dryRun, autoYes)
		}

		if inspect {
			return handleInspect()
		}

		beadsDir := beads.FindBeadsDir()
		if beadsDir == "" {
			if isJSONOutput() {
				if jerr := outputJSON(map[string]interface{}{
					"error":   "no_beads_directory",
					"message": activeWorkspaceNotFoundMessage() + " " + diagHint() + ".",
				}); jerr != nil {
					return jerr
				}
				return SilentExit()
			}
			return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
		}

		cfg, err := loadOrCreateConfig(beadsDir)
		if err != nil {
			if isJSONOutput() {
				if jerr := outputJSON(map[string]interface{}{
					"error":   "config_load_failed",
					"message": err.Error(),
				}); jerr != nil {
					return jerr
				}
				return SilentExit()
			}
			return HandleError("failed to load config: %v", err)
		}

		return handleDoltMetadataUpdate(cfg, beadsDir, dryRun)
	},
}

// handleDoltMetadataUpdate handles version metadata updates for Dolt backends.
// beadsDir is the resolved .beads directory (honoring -C); repo-derived metadata
// is computed from it rather than the process cwd so `bd -C <dir> migrate`
// fingerprints the target repo, not the caller's (GH#4361).
func handleDoltMetadataUpdate(cfg *configfile.Config, beadsDir string, dryRun bool) error {
	ctx := getRootContext()
	store := getStore()
	if store == nil {
		return reportMissingDoltMetadataDatabase()
	}

	state := readDoltMetadataState(ctx, store)
	if state.isCurrent() {
		return reportCurrentDoltMetadata(state.currentVersion)
	}
	if dryRun {
		return reportDoltMetadataDryRun(state)
	}

	versionUpdated, repoIDSet, cloneIDSet, err := applyDoltMetadataUpdates(ctx, store, beadsDir, state)
	if err != nil {
		return err
	}

	if versionUpdated || repoIDSet || cloneIDSet {
		commandDidWrite.Store(true)
	}
	return reportDoltMetadataSuccess(cfg, versionUpdated, repoIDSet, cloneIDSet)
}

func applyDoltMetadataUpdates(ctx context.Context, store storage.DoltStorage, beadsDir string, state doltMetadataState) (bool, bool, bool, error) {
	versionUpdated := false
	if state.needsVersionUpdate {
		var err error
		versionUpdated, err = updateDoltMetadataVersion(ctx, store, state.currentVersion)
		if err != nil {
			return false, false, false, err
		}
	}
	repoIDSet := false
	if state.needsRepoID {
		repoIDSet = setDoltMetadataRepoID(ctx, store, beadsDir)
	}
	cloneIDSet := false
	if state.needsCloneID {
		cloneIDSet = setDoltMetadataCloneID(ctx, store, beadsDir)
	}
	return versionUpdated, repoIDSet, cloneIDSet, nil
}

type doltMetadataState struct {
	currentVersion     string
	needsVersionUpdate bool
	needsRepoID        bool
	needsCloneID       bool
}

func readDoltMetadataState(ctx context.Context, store storage.DoltStorage) doltMetadataState {
	currentVersion, _ := store.GetLocalMetadata(ctx, "bd_version")
	currentRepoID, _ := store.GetMetadata(ctx, "repo_id")
	currentCloneID, _ := store.GetMetadata(ctx, "clone_id")
	return doltMetadataState{
		currentVersion:     currentVersion,
		needsVersionUpdate: currentVersion != Version,
		needsRepoID:        currentRepoID == "",
		needsCloneID:       currentCloneID == "",
	}
}

func (s doltMetadataState) isCurrent() bool {
	return !s.needsVersionUpdate && !s.needsRepoID && !s.needsCloneID
}

func reportMissingDoltMetadataDatabase() error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":  "no_databases",
			"message": "No Dolt database found in .beads/",
		})
	}
	fmt.Fprintf(os.Stderr, "No Dolt database found. Run 'bd init' to create a new database.\n")
	return nil
}

func reportCurrentDoltMetadata(currentVersion string) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":  "current",
			"message": fmt.Sprintf("Dolt database already at version %s", Version),
		})
	}
	fmt.Printf("Dolt database version: %s\n", currentVersion)
	fmt.Printf("%s\n", ui.RenderPass("✓ Version matches"))
	fmt.Printf("%s\n", ui.RenderPass("✓ All metadata fields present"))
	return nil
}

func reportDoltMetadataDryRun(state doltMetadataState) error {
	dryRunResult := map[string]interface{}{
		"dry_run":              true,
		"needs_version_update": state.needsVersionUpdate,
		"needs_repo_id":        state.needsRepoID,
		"needs_clone_id":       state.needsCloneID,
	}
	if state.needsVersionUpdate {
		dryRunResult["current_version"] = state.currentVersion
		dryRunResult["target_version"] = Version
	}
	if isJSONOutput() {
		return outputJSON(dryRunResult)
	}
	fmt.Println("Dry run mode - no changes will be made")
	if state.needsVersionUpdate {
		fmt.Printf("Would update Dolt version: %s → %s\n", state.currentVersion, Version)
	}
	if state.needsRepoID {
		fmt.Println("Would set repo_id")
	}
	if state.needsCloneID {
		fmt.Println("Would set clone_id")
	}
	return nil
}

func updateDoltMetadataVersion(ctx context.Context, store storage.DoltStorage, currentVersion string) (bool, error) {
	if !isJSONOutput() {
		fmt.Printf("Updating Dolt schema version: %s → %s\n", currentVersion, Version)
	}
	detectAndSetDoltIssuePrefix(ctx, store)
	if err := store.SetLocalMetadata(ctx, "bd_version", Version); err != nil {
		if isJSONOutput() {
			if jerr := outputJSON(map[string]interface{}{
				"error":   "version_update_failed",
				"message": err.Error(),
			}); jerr != nil {
				return false, jerr
			}
			return false, SilentExit()
		}
		return false, HandleError("failed to update version: %v", err)
	}
	if !isJSONOutput() {
		fmt.Printf("%s\n", ui.RenderPass("✓ Version updated"))
	}
	return true, nil
}

func detectAndSetDoltIssuePrefix(ctx context.Context, store storage.DoltStorage) {
	prefix, err := store.GetConfig(ctx, "issue_prefix")
	if err != nil || prefix != "" {
		return
	}
	issues, err := store.SearchIssues(ctx, "", types.IssueFilter{})
	if err != nil || len(issues) == 0 {
		return
	}
	detectedPrefix := utils.ExtractIssuePrefix(issues[0].ID)
	if detectedPrefix == "" {
		return
	}
	if err := store.SetConfig(ctx, "issue_prefix", detectedPrefix); err != nil {
		if !isJSONOutput() {
			fmt.Fprintf(os.Stderr, "Warning: failed to set issue prefix: %v\n", err)
		}
		return
	}
	if !isJSONOutput() {
		fmt.Printf("%s\n", ui.RenderPass(fmt.Sprintf("✓ Detected and set issue prefix: %s", detectedPrefix)))
	}
}

func setDoltMetadataRepoID(ctx context.Context, store storage.DoltStorage, beadsDir string) bool {
	computed, err := beads.ComputeRepoIDForPath(beadsDir)
	if err != nil {
		if !isJSONOutput() {
			fmt.Fprintf(os.Stderr, "Warning: could not compute repo_id: %v\n", err)
		}
		return false
	}
	if err := store.SetMetadata(ctx, "repo_id", computed); err != nil {
		if !isJSONOutput() {
			fmt.Fprintf(os.Stderr, "Warning: failed to set repo_id: %v\n", err)
		}
		return false
	}
	if !isJSONOutput() {
		fmt.Printf("%s\n", ui.RenderPass(fmt.Sprintf("✓ Set repo_id: %s", truncateID(computed, 8))))
	}
	return true
}

func setDoltMetadataCloneID(ctx context.Context, store storage.DoltStorage, beadsDir string) bool {
	computed, err := beads.GetCloneIDForPath(beadsDir)
	if err != nil {
		if !isJSONOutput() {
			fmt.Fprintf(os.Stderr, "Warning: could not compute clone_id: %v\n", err)
		}
		return false
	}
	if err := store.SetMetadata(ctx, "clone_id", computed); err != nil {
		if !isJSONOutput() {
			fmt.Fprintf(os.Stderr, "Warning: failed to set clone_id: %v\n", err)
		}
		return false
	}
	if !isJSONOutput() {
		fmt.Printf("%s\n", ui.RenderPass(fmt.Sprintf("✓ Set clone_id: %s", truncateID(computed, 8))))
	}
	return true
}

func reportDoltMetadataSuccess(cfg *configfile.Config, versionUpdated bool, repoIDSet bool, cloneIDSet bool) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"status":           "success",
			"current_database": cfg.Database,
			"backend":          "dolt",
			"version":          Version,
			"version_updated":  versionUpdated,
			"repo_id_set":      repoIDSet,
			"clone_id_set":     cloneIDSet,
		})
	}
	fmt.Printf("\nDolt database: %s (version %s)\n", cfg.Database, Version)
	return nil
}

// truncateID safely truncates an ID string to maxLen characters.
func truncateID(id string, maxLen int) string {
	if len(id) <= maxLen {
		return id
	}
	return id[:maxLen]
}

// loadOrCreateConfig loads metadata.json or creates default if not found
func loadOrCreateConfig(beadsDir string) (*configfile.Config, error) {
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return nil, err
	}

	// Create default if no config exists
	if cfg == nil {
		cfg = configfile.DefaultConfig()
	}

	return cfg, nil
}

// pathHashRepoIDStampNotice returns the propagation warning for replacing an
// existing repository ID with a path-derived one, or "" when none applies
// (no stored id, no change, or a remote-derived new id).
func pathHashRepoIDStampNotice(oldRepoID, newRepoID string, source beads.RepoIDSource) string {
	if oldRepoID == "" || oldRepoID == newRepoID || source != beads.RepoIDSourcePath {
		return ""
	}
	return "Warning: stamping a path-hash repository ID (this checkout has no origin remote).\n" +
		"It is local to this host but will propagate to every clone on the next sync.\n" +
		"On a synced clone, keep the stored ID instead (see 'bd doctor').\n"
}

func handleUpdateRepoID(dryRun bool, autoYes bool) error {
	beadsDir, err := repoIDUpdateBeadsDir()
	if err != nil {
		return err
	}
	newRepoID, newRepoIDSource, err := beads.ComputeRepoIDForPathWithSource(beadsDir)
	if err != nil {
		return reportRepoIDComputeFailure(err)
	}

	store := getStore()
	if store == nil {
		return HandleError("no database — run 'bd init' first")
	}

	ctx := getRootContext()
	oldRepoID, err := readExistingRepoID(ctx, store)
	if err != nil {
		return reportRepoIDReadFailure(err)
	}

	oldDisplay := repoIDDisplay(oldRepoID)

	if dryRun {
		return reportRepoIDDryRun(oldDisplay, newRepoID)
	}

	if !shouldProceedWithRepoIDUpdate(oldRepoID, newRepoID, autoYes, newRepoIDSource, oldDisplay) {
		return nil
	}

	return applyRepoIDUpdate(ctx, store, oldRepoID, oldDisplay, newRepoID, newRepoIDSource, autoYes)
}

func shouldProceedWithRepoIDUpdate(oldRepoID string, newRepoID string, autoYes bool, source beads.RepoIDSource, oldDisplay string) bool {
	if oldRepoID == "" || oldRepoID == newRepoID || autoYes || isJSONOutput() {
		return true
	}
	return confirmRepoIDChange(oldDisplay, newRepoID, source)
}

func applyRepoIDUpdate(ctx context.Context, store storage.DoltStorage, oldRepoID string, oldDisplay string, newRepoID string, source beads.RepoIDSource, autoYes bool) error {
	// bd-ek28z: --yes and --json skip the confirm block above, so scripted
	// callers stamped a host-local path hash with no warning at all — the
	// GH#4361 recurrence hole. Print the notice (not the prompt) on those
	// paths too.
	pathHashNotice := pathHashRepoIDStampNotice(oldRepoID, newRepoID, source)
	if pathHashNotice != "" && (autoYes || isJSONOutput()) {
		fmt.Fprint(os.Stderr, pathHashNotice)
	}

	if err := store.SetMetadata(ctx, "repo_id", newRepoID); err != nil {
		return reportRepoIDWriteFailure(err)
	}

	commandDidWrite.Store(true)
	return reportRepoIDSuccess(oldDisplay, newRepoID, source, pathHashNotice)
}

func repoIDUpdateBeadsDir() (string, error) {
	beadsDir := beads.FindBeadsDir()
	if beadsDir != "" {
		return beadsDir, nil
	}
	if isJSONOutput() {
		if jerr := outputJSON(map[string]interface{}{
			"error":   "no_database",
			"message": "No beads database found. " + diagHint() + ".",
		}); jerr != nil {
			return "", jerr
		}
		return "", SilentExit()
	}
	return "", HandleErrorWithHint("no beads database found", diagHint())
}

func reportRepoIDComputeFailure(err error) error {
	if isJSONOutput() {
		if jerr := outputJSON(map[string]interface{}{
			"error":   "compute_failed",
			"message": err.Error(),
		}); jerr != nil {
			return jerr
		}
		return SilentExit()
	}
	return HandleError("failed to compute repository ID: %v", err)
}

func readExistingRepoID(ctx context.Context, store storage.DoltStorage) (string, error) {
	oldRepoID, err := store.GetMetadata(ctx, "repo_id")
	if err != nil && err.Error() != "metadata key not found: repo_id" {
		return "", err
	}
	return oldRepoID, nil
}

func reportRepoIDReadFailure(err error) error {
	if isJSONOutput() {
		if jerr := outputJSON(map[string]interface{}{
			"error":   "read_failed",
			"message": err.Error(),
		}); jerr != nil {
			return jerr
		}
		return SilentExit()
	}
	return HandleError("failed to read repo_id: %v", err)
}

func repoIDDisplay(repoID string) string {
	if len(repoID) >= 8 {
		return repoID[:8]
	}
	return "none"
}

func reportRepoIDDryRun(oldDisplay string, newRepoID string) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"dry_run":     true,
			"old_repo_id": oldDisplay,
			"new_repo_id": truncateID(newRepoID, 8),
		})
	}
	fmt.Println("Dry run mode - no changes will be made")
	fmt.Printf("Would update repository ID:\n")
	fmt.Printf("  Old: %s\n", oldDisplay)
	fmt.Printf("  New: %s\n", truncateID(newRepoID, 8))
	return nil
}

func confirmRepoIDChange(oldDisplay string, newRepoID string, source beads.RepoIDSource) bool {
	fmt.Printf("WARNING: Changing repository ID can break sync if other clones exist.\n")
	// bd-46vla: repo_id lives in the versioned metadata table, so the new
	// value propagates to every clone on the next sync. A path-fallback id
	// (no origin remote here) is host-local — stamping it into shared
	// state is almost never right on a synced clone.
	if source == beads.RepoIDSourcePath {
		fmt.Printf("The new ID is a path hash (this checkout has no origin remote); it is\n")
		fmt.Printf("local to this host but will propagate to every clone on the next sync.\n")
		fmt.Printf("On a synced clone, keep the stored ID instead (see 'bd doctor').\n")
	}
	fmt.Printf("\n")
	fmt.Printf("Current repo ID: %s\n", oldDisplay)
	fmt.Printf("New repo ID:     %s\n\n", truncateID(newRepoID, 8))
	fmt.Printf("Continue? [y/N] ")
	var response string
	_, _ = fmt.Scanln(&response)
	if strings.ToLower(response) == "y" || strings.ToLower(response) == "yes" {
		return true
	}
	fmt.Println("Canceled")
	return false
}

func reportRepoIDWriteFailure(err error) error {
	if isJSONOutput() {
		if jerr := outputJSON(map[string]interface{}{
			"error":   "update_failed",
			"message": err.Error(),
		}); jerr != nil {
			return jerr
		}
		return SilentExit()
	}
	return HandleError("failed to update repo_id: %v", err)
}

func reportRepoIDSuccess(oldDisplay string, newRepoID string, source beads.RepoIDSource, pathHashNotice string) error {
	if isJSONOutput() {
		payload := map[string]interface{}{
			"status":         "success",
			"old_repo_id":    oldDisplay,
			"new_repo_id":    truncateID(newRepoID, 8),
			"repo_id_source": string(source),
		}
		if pathHashNotice != "" {
			payload["warning"] = "new repository ID is a path hash (no origin remote); it will propagate to every clone on the next sync"
		}
		return outputJSON(payload)
	}
	fmt.Printf("%s\n\n", ui.RenderPass("✓ Repository ID updated"))
	fmt.Printf("  Old: %s\n", oldDisplay)
	fmt.Printf("  New: %s\n", truncateID(newRepoID, 8))
	return nil
}

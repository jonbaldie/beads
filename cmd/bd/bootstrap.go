package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/beads/cmd/bd/doctor/fix"
	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
	"github.com/spf13/cobra"
)

var resolveBootstrapAuthoritativeMetadata = fix.ResolveAuthoritativeServerMetadata

type bootstrapServerProbeConfig struct {
	host     string
	port     int
	user     string
	pass     string
	database string
	tls      bool
}

type bootstrapServerDBCheck struct {
	Exists    bool
	Reachable bool
	Err       error
}

// bootstrapRetryDelay is the sleep function used between server-DB-not-found
// retries. Injectable for testing.
var bootstrapRetryDelay = func(d time.Duration) { time.Sleep(d) }

var checkBootstrapServerDB = func(probeCfg bootstrapServerProbeConfig) bootstrapServerDBCheck {
	host := probeCfg.host
	port := probeCfg.port
	dbName := probeCfg.database
	dsn := doltutil.ServerDSN{
		Host:     host,
		Port:     port,
		User:     probeCfg.user,
		Password: probeCfg.pass,
		TLS:      probeCfg.tls,
	}.String()

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return bootstrapServerDBCheck{Reachable: false, Err: err}
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return bootstrapServerDBCheck{Reachable: false, Err: err}
	}

	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return bootstrapServerDBCheck{Reachable: true, Err: err}
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return bootstrapServerDBCheck{Reachable: true, Err: err}
		}
		if name == dbName {
			return bootstrapServerDBCheck{Exists: true, Reachable: true}
		}
	}
	if err := rows.Err(); err != nil {
		return bootstrapServerDBCheck{Reachable: true, Err: err}
	}

	return bootstrapServerDBCheck{Exists: false, Reachable: true}
}

var bootstrapCmd = &cobra.Command{
	Use:     "bootstrap",
	GroupID: "setup",
	Short:   "Non-destructive database setup for fresh clones and recovery",
	Long: `Bootstrap sets up the beads database without destroying existing data.
Unlike 'bd init --force', bootstrap will never delete existing issues.

Bootstrap auto-detects the right action:
  • If sync.remote is configured: clones from the remote
  • If git origin has Dolt data (refs/dolt/data): clones from git and wires origin for future push/pull
  • If .beads/backup/*.jsonl exists: restores from backup
  • If .beads/issues.jsonl exists: imports from git-tracked JSONL
  • If no database exists: creates a fresh one
  • If database already exists: validates and reports status

This is the recommended command for:
  • Setting up beads on a fresh clone
  • Recovering after moving to a new machine
  • Repairing a broken database configuration

Non-interactive mode (--non-interactive, --yes/-y, or BD_NON_INTERACTIVE=1):
  Skips the confirmation prompt before executing the bootstrap plan.
  Also auto-detected when stdin is not a terminal or CI=true is set.

Examples:
  bd bootstrap              # Auto-detect and set up
  bd bootstrap --dry-run    # Show what would be done
  bd bootstrap --json       # Output plan as JSON
  bd bootstrap --yes        # Skip confirmation prompt
`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("bootstrap")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		dryRun, _ := cmd.Flags().GetBool("dry-run")
		yesFlag, _ := cmd.Flags().GetBool("yes")
		nonInteractiveFlag, _ := cmd.Flags().GetBool("non-interactive")

		// Resolve non-interactive mode: flag > env var > CI env > terminal detection.
		nonInteractive := isNonInteractiveBootstrap(yesFlag || nonInteractiveFlag)

		// Find beads directory
		beadsDir := beads.FindBeadsDir()
		if beadsDir == "" {
			// No .beads directory exists yet. Before giving up, probe the
			// git remote for Dolt data stored in git (refs/dolt/data). This
			// is the "fresh second clone" case: clone1 pushed Beads state
			// to a git remote, and clone2 needs to bootstrap from it.
			// Only applies to git remotes — Dolt-native remotes (DoltHub,
			// S3, etc.) should be configured via sync.remote. (GH#2792)
			//
			// If found, synthesize the theoretical .beads path and fall
			// through to the normal detectBootstrapAction + executeBootstrapPlan
			// flow. Actual directory creation is deferred to executeSyncAction
			// to preserve --dry-run semantics.
			if isGitRepo() && !isBareGitRepo() {
				if originURL, err := gitOriginGetURL(); err == nil && originURL != "" {
					if gitOriginHasDoltDataRef() {
						if fallbackDir := beads.GetWorktreeFallbackBeadsDir(); fallbackDir != "" {
							beadsDir = fallbackDir
						} else {
							cwd, err := os.Getwd()
							if err != nil {
								return HandleError("failed to get working directory: %v", err)
							}
							beadsDir = filepath.Join(cwd, ".beads")
						}
					}
				}
			}
		}

		if beadsDir == "" {
			if isJSONOutput() {
				if err := outputJSON(noWorkspaceBootstrapPayload()); err != nil {
					return err
				}
				return SilentExit()
			}
			fmt.Fprintf(os.Stderr, "Hint: %s\n", diagHint())
			fmt.Fprintf(os.Stderr, "Bootstrap is for existing projects that need database setup.\n")
			return HandleError("%s", activeWorkspaceNotFoundMessage())
		}

		if err := guardLegacyUpgradeWorkspace(beadsDir); err != nil {
			return HandleError("%v", err)
		}

		// Load config from .beads/metadata.json. When the beadsDir was
		// synthesized (fresh clone or rig with no local .beads), the file
		// won't exist. In that case, walk up parent directories to find a
		// workspace-level metadata.json that contains the correct database
		// name (e.g. dolt_database). Without this, server-mode rigs get the
		// default name "beads" instead of their configured name. (GH#3029)
		cfg, err := configfile.Load(beadsDir)
		if err != nil {
			return HandleError("failed to load %s: %v; no storage database was opened or modified; fix or restore metadata.json and retry", configfile.ConfigPath(beadsDir), err)
		}
		if cfg == nil {
			cfg, err = findParentConfig(beadsDir)
			if err != nil {
				return HandleError("failed to load ancestor storage metadata: %v; no storage database was opened or modified; fix or restore metadata.json and retry", err)
			}
		}
		if cfg == nil {
			cfg = configfile.DefaultConfig()
		}
		if err := requireBootstrapDoltBackend(cfg); err != nil {
			return HandleError("%v", err)
		}

		resolvedCfg, repairMsg, err := applyBootstrapMetadataRepair(beadsDir, cfg, !dryRun)
		if err != nil {
			return HandleError("failed to reconcile shared-server metadata: %v", err)
		}
		if resolvedCfg != nil {
			cfg = resolvedCfg
		}

		// Determine action based on state
		plan := detectBootstrapAction(beadsDir, cfg)

		if isJSONOutput() {
			if err := outputJSON(plan); err != nil {
				return err
			}
			if plan.Action == "none" || dryRun {
				return nil
			}
		} else {
			if repairMsg != "" {
				fmt.Fprintf(os.Stderr, "Bootstrap metadata repair: %s\n", repairMsg)
			}
			printBootstrapPlan(plan)
			if plan.Action == "none" || dryRun {
				return nil
			}
		}

		if err := executeBootstrapPlan(plan, cfg, nonInteractive); err != nil {
			return HandleError("Bootstrap failed: %v", err)
		}
		return nil
	},
}

func applyBootstrapMetadataRepair(beadsDir string, cfg *configfile.Config, apply bool) (*configfile.Config, string, error) {
	if beadsDir == "" {
		return cfg, "", nil
	}
	if _, err := os.Stat(beadsDir); err != nil {
		return cfg, "", nil
	}
	resolved, msg, err := resolveBootstrapAuthoritativeMetadata(filepath.Dir(beadsDir), apply)
	if err != nil {
		return nil, "", err
	}
	if resolved == nil {
		return cfg, msg, nil
	}
	return resolved, msg, nil
}

// BootstrapPlan describes what bootstrap will do.
type BootstrapPlan struct {
	Action      string `json:"action"` // "sync", "restore", "jsonl-import", "init", "none"
	Reason      string `json:"reason"` // Human-readable explanation
	BeadsDir    string `json:"beads_dir"`
	Database    string `json:"database"`
	SyncRemote  string `json:"sync_remote,omitempty"`
	BackupDir   string `json:"backup_dir,omitempty"`
	JSONLFile   string `json:"jsonl_file,omitempty"`
	HasExisting bool   `json:"has_existing"`
}

func noWorkspaceBootstrapPayload() map[string]interface{} {
	return map[string]interface{}{
		"action":     "none",
		"reason":     activeWorkspaceNotFoundError(),
		"suggestion": diagHint(),
	}
}

func requireBootstrapDoltBackend(cfg *configfile.Config) error {
	if err := validateConfiguredBackend(cfg); err != nil {
		return err
	}
	// A registered extension backend passes validateConfiguredBackend so its
	// existing workspaces can be opened, but every bd bootstrap action
	// (sync, restore, jsonl-import, init) provisions or imports Dolt. Registered
	// backends are open/discover-only, so reject them here — before
	// detectBootstrapAction or executeBootstrapPlan — mirroring bd init's
	// fail-closed gate. Downstream registrants supply their own workspace
	// provisioning path.
	if cfg != nil && cfg.GetBackend() != configfile.BackendDolt {
		return fmt.Errorf("backend %q cannot be bootstrapped by bd bootstrap; it can only open an existing workspace (bd bootstrap provisions %q, the default)", cfg.GetBackend(), configfile.BackendDolt)
	}
	return nil
}

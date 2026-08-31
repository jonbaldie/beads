package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/storage/schema"
)

func executeBootstrapPlan(plan BootstrapPlan, cfg *configfile.Config, nonInteractive bool) error {
	if err := requireBootstrapDoltBackend(cfg); err != nil {
		return err
	}
	if !confirmPrompt("Proceed?", nonInteractive) {
		fmt.Fprintf(os.Stderr, "Aborted.\n")
		return nil
	}

	ctx := context.Background()

	// Workspace operation gate: every bootstrap action below replaces or
	// creates workspace state (sync/restore/jsonl-import/init), and
	// bootstrap is a skip-store command that the PersistentPreRunE
	// chokepoint never gates. This is the single point where the target
	// workspace is known and no state has been touched yet, so acquire the
	// workspace + physical-root gates EXCLUSIVELY here for the rest of the
	// plan execution. Exclusive failures are hard errors: bootstrap
	// refuses to run over live bd activity rather than pretend.
	//
	// The resolved PhysicalRoots for plan.BeadsDir do not necessarily cover
	// the directory the actions below actually open — executeInitAction,
	// executeRestoreAction, and executeJSONLImportAction all open
	// doltserver.ResolveDoltDir(plan.BeadsDir) directly (e.g. .beads/dolt
	// for an embedded-metadata workspace). Pass it as an extraRoot,
	// mirroring how bd init passes its own resolved db path, so the
	// physical directory bootstrap writes is actually gated.
	gateHandle, gateErr := acquireExclusiveWorkspaceGates(ctx, plan.BeadsDir, "bd bootstrap "+plan.Action,
		doltserver.ResolveDoltDir(plan.BeadsDir))
	if gateErr != nil {
		return fmt.Errorf("bd bootstrap refuses to run over live bd activity on this workspace: %w", gateErr)
	}
	defer func() { _ = gateHandle.Release() }()

	switch plan.Action {
	case "sync":
		return executeSyncAction(ctx, plan, cfg)
	case "restore":
		return executeRestoreAction(ctx, plan, cfg)
	case "jsonl-import":
		return executeJSONLImportAction(ctx, plan, cfg)
	case "init":
		return executeInitAction(ctx, plan, cfg)
	}
	return nil
}

func executeInitAction(ctx context.Context, plan BootstrapPlan, cfg *configfile.Config) error {
	prefix := inferPrefix(cfg)
	dbName := cfg.GetDoltDatabase()

	s, err := newDoltStore(ctx, &dolt.Config{
		Path:     doltserver.ResolveDoltDir(plan.BeadsDir),
		Database: dbName,
		RemoteOptions: dolt.RemoteOptions{
			CreateIfMissing: true,
		},
		ServerOptions: dolt.ServerOptions{
			AutoStart: true,
		},
		BeadsDir: plan.BeadsDir,
	})
	if err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		return fmt.Errorf("set issue prefix: %w", err)
	}
	if err := s.CommitWithConfig(ctx, "bd bootstrap"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Created fresh database with prefix %q\n", prefix)
	return nil
}

func executeRestoreAction(ctx context.Context, plan BootstrapPlan, cfg *configfile.Config) error {
	prefix := inferPrefix(cfg)
	dbName := cfg.GetDoltDatabase()

	s, err := newDoltStore(ctx, &dolt.Config{
		Path:     doltserver.ResolveDoltDir(plan.BeadsDir),
		Database: dbName,
		RemoteOptions: dolt.RemoteOptions{
			CreateIfMissing: true,
		},
		ServerOptions: dolt.ServerOptions{
			AutoStart: true,
		},
		BeadsDir: plan.BeadsDir,
	})
	if err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		return fmt.Errorf("set issue prefix: %w", err)
	}
	if err := s.CommitWithConfig(ctx, "bd bootstrap: init"); err != nil {
		return fmt.Errorf("commit init: %w", err)
	}

	if err := runBackupRestore(ctx, s, plan.BackupDir, false); err != nil {
		return fmt.Errorf("restore from backup: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Restored from backup\n")
	return nil
}

func executeJSONLImportAction(ctx context.Context, plan BootstrapPlan, cfg *configfile.Config) error {
	prefix := inferPrefix(cfg)
	dbName := cfg.GetDoltDatabase()

	s, err := newDoltStore(ctx, &dolt.Config{
		Path:     doltserver.ResolveDoltDir(plan.BeadsDir),
		Database: dbName,
		RemoteOptions: dolt.RemoteOptions{
			CreateIfMissing: true,
		},
		ServerOptions: dolt.ServerOptions{
			AutoStart: true,
		},
		BeadsDir: plan.BeadsDir,
	})
	if err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.SetConfig(ctx, "issue_prefix", prefix); err != nil {
		return fmt.Errorf("set issue prefix: %w", err)
	}
	if err := s.CommitWithConfig(ctx, "bd bootstrap: init"); err != nil {
		return fmt.Errorf("commit init: %w", err)
	}

	count, err := importFromLocalJSONL(ctx, s, plan.JSONLFile)
	if err != nil {
		return fmt.Errorf("import from JSONL: %w", err)
	}

	if err := s.Commit(ctx, "bd bootstrap: import from issues.jsonl"); err != nil {
		return fmt.Errorf("commit import: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Imported %d issues from %s\n", count, plan.JSONLFile)
	return nil
}

func executeSyncAction(ctx context.Context, plan BootstrapPlan, cfg *configfile.Config) error {
	// Ensure .beads directory exists — it may not in the "fresh clone"
	// bootstrap path where we detected remote data before .beads was
	// created. Deferred here to preserve --dry-run semantics. (GH#2792)
	if err := os.MkdirAll(plan.BeadsDir, 0o750); err != nil {
		return fmt.Errorf("create beads directory: %w", err)
	}

	dbName := cfg.GetDoltDatabase()
	if err := cloneFromRemote(ctx, plan.BeadsDir, plan.SyncRemote, dbName, cfg); err != nil {
		return err
	}

	// Finalize the bootstrapped workspace so subsequent bd commands can open
	// the cloned database. Without metadata.json and config.yaml,
	// configfile.Load() returns nil, callers fall back to the default
	// dolt_database name, and bd loses track of the cloned database —
	// producing "no beads configuration found" and "Error 1105: no database
	// selected" on bd status / bd dolt push in fresh clones. Every other
	// bootstrap action (init, restore, jsonl-import) writes these files via
	// newDoltStore + createConfigYaml; the sync path historically did not.
	// (GH#3201)
	if err := finalizeSyncedBootstrap(plan.BeadsDir, plan.SyncRemote, cfg, dbName); err != nil {
		return err
	}

	// Open and close the store to ensure dolt_ignore'd wisp tables are
	// created in the working set. Clone does not include these tables
	// (they are never committed), so they must be recreated after clone.
	// Both embedded and server mode handle this in their store init paths.
	warmupStore, err := newDoltStoreFromConfig(ctx, plan.BeadsDir)
	if err != nil {
		// #4259: the cloned remote is behind this binary, so the remote-migrate
		// gate held migration for an explicit operator decision. Surface that
		// now with bootstrap-specific guidance and a non-zero exit. Returning
		// silent success here (as this path once did) sent operators in a
		// loop: the first real command failed with the gate message, whose
		// generic "adopt" remedy is `bd bootstrap` — which re-clones the same
		// behind database and silently "succeeds" again (bd-6dnrw.31).
		var gateErr *schema.RemoteMigrateGateError
		if errors.As(err, &gateErr) {
			if !isJSONOutput() {
				printBootstrapRemoteBehindGuidance(os.Stderr, gateErr, plan.SyncRemote, "bd bootstrap")
			}
			unit := "migrations"
			if gateErr.Pending == 1 {
				unit = "migration"
			}
			return fmt.Errorf("clone from %s succeeded, but the database needs %d schema %s (v%d -> v%d) that bd will not auto-apply to a remote-backed database (#4259)",
				plan.SyncRemote, gateErr.Pending, unit, gateErr.CurrentVersion, gateErr.LatestVersion)
		}
		// Non-fatal: wisp tables will be created on the next command that
		// opens the store. Warn so the user knows to retry if they hit
		// "table not found: wisp_*" errors.
		fmt.Fprintf(os.Stderr, "Warning: post-clone store init failed (wisp tables may be missing): %v\n", err)
		return nil
	}
	configureInitDoltRemote(ctx, warmupStore, plan.SyncRemote, false)
	_ = warmupStore.Close()

	return nil
}

// printBootstrapRemoteBehindGuidance explains a remote-migrate gate refusal in
// bootstrap terms. The gate's generic remedy ("adopt the migrated database:
// bd bootstrap") is wrong from inside a bootstrap-style clone — the database
// was just cloned from the remote, so the REMOTE is what is behind this binary
// and re-cloning can never help. The way out is exactly one designated machine
// migrating and pushing. rerunCmd is the command the operator just ran ("bd
// bootstrap", "bd init") so the don't-bother-retrying line names it.
func printBootstrapRemoteBehindGuidance(w io.Writer, e *schema.RemoteMigrateGateError, syncRemote, rerunCmd string) {
	unit := "migrations"
	if e.Pending == 1 {
		unit = "migration"
	}
	fmt.Fprintf(w, "\nThe database cloned from %s needs %d schema %s (v%d -> v%d).\n",
		syncRemote, e.Pending, unit, e.CurrentVersion, e.LatestVersion)
	fmt.Fprint(w,
		"  bd will not migrate it automatically: migrating clones independently forks\n"+
			"  the schema so `bd dolt pull` can no longer merge (#4259).\n"+
			"\n"+
			"  Re-running `"+rerunCmd+"` will NOT fix this — the remote itself is behind.\n"+
			"  Choose one:\n"+
			"    • This machine is the designated migrator (exactly ONE machine should be):\n"+
			"        bd migrate --force\n"+
			"        bd dolt push\n"+
			"      then other machines re-run `bd bootstrap` to adopt the migrated database.\n"+
			"    • Another machine is the designated migrator: wait for it to push, then\n"+
			"      re-run `bd bootstrap`, or keep using a bd version that matches the remote.\n\n")
}

// finalizeSyncedBootstrap writes metadata.json and config.yaml after a
// successful sync clone, matching the on-disk layout that bd init produces.
// It is idempotent: re-running over an already-finalized workspace leaves
// existing files intact (createConfigYaml skips if config.yaml exists; the
// metadata.json write is a full rewrite that preserves caller fields).
func finalizeSyncedBootstrap(beadsDir, syncRemote string, cfg *configfile.Config, dbName string) error {
	prepareSyncedBootstrapConfig(cfg, dbName)
	if err := saveSyncedBootstrapFiles(beadsDir, cfg); err != nil {
		return err
	}
	return persistSyncedBootstrapRemote(beadsDir, syncRemote)
}

func prepareSyncedBootstrapConfig(cfg *configfile.Config, dbName string) {
	// Preserve whatever upstream fields were already set in cfg (which may
	// be DefaultConfig when metadata.json was absent, or a parent workspace
	// config propagated by findParentConfig), then fill in the bits required by
	// configfile.Load consumers.
	cfg.Backend = configfile.BackendDolt
	cfg.DoltDatabase = dbName
	cfg.DoltMode = syncedBootstrapDoltMode(cfg)
	// Mirror init's convention: metadata.json database points at the Dolt
	// directory rather than the legacy "beads.db" placeholder.
	if cfg.Database == "" || cfg.Database == beads.CanonicalDatabaseName {
		cfg.Database = "dolt"
	}
}

func syncedBootstrapDoltMode(cfg *configfile.Config) string {
	if cfg.IsDoltProxiedServerMode() {
		return configfile.DoltModeProxiedServer
	}
	if cfg.IsDoltServerMode() || doltserver.IsSharedServerMode() {
		return configfile.DoltModeServer
	}
	return configfile.DoltModeEmbedded
}

func saveSyncedBootstrapFiles(beadsDir string, cfg *configfile.Config) error {
	if err := cfg.Save(beadsDir); err != nil {
		return fmt.Errorf("write metadata.json: %w", err)
	}
	if err := createConfigYaml(beadsDir, false, ""); err != nil {
		return fmt.Errorf("create config.yaml: %w", err)
	}
	if err := doctor.EnsureGitignoreForBeadsDir(beadsDir); err != nil {
		return fmt.Errorf("ensure .beads/.gitignore: %w", err)
	}
	return nil
}

func persistSyncedBootstrapRemote(beadsDir, syncRemote string) error {
	// Persist sync.remote so subsequent fresh clones (and bd bootstrap retries)
	// can rediscover the remote without re-probing origin refs.
	if syncRemote == "" {
		return nil
	}
	if err := config.SetYamlConfigInDir(beadsDir, "sync.remote", syncRemote); err != nil {
		return fmt.Errorf("persist sync.remote to config.yaml: %w", err)
	}
	return nil
}

type remoteCloneMode int

const (
	remoteCloneAuto remoteCloneMode = iota
	remoteCloneEmbedded
	remoteCloneExternalServer
	remoteCloneCLI
)

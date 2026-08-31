package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/issueops"
	"github.com/spf13/cobra"
)

// applyInitGatewayCredential routes the hand-built init Dolt config through the
// gateway credential machinery. When BEADS_DOLT_CREDENTIAL_COMMAND is configured,
// its short-lived token becomes the connection username, the config is marked as
// targeting an authenticating gateway server (so the store skips schema init and
// the SHOW/CREATE DATABASE probe), and local auto-start is disabled — a gateway
// server is externally managed, and spawning a local dolt server would shadow it.
//
// It runs only when the config targets a server (ServerMode). An embedded init
// never presents a username, so an ambient credential command must not run — or
// fail — an embedded open. This mirrors the canonical open path
// (internal/storage/dolt/open.go), which gates the command on
// IsDoltServerMode()||IsSharedServerMode(). Without this gate, a plain embedded
// `bd init` on a host that exports BEADS_DOLT_CREDENTIAL_COMMAND would drag the
// gateway machinery into an ordinary local init and abort it with a misleading
// provisioning-contract error, leaving a half-initialized .beads/. Gateway init
// always reaches here with ServerMode set: init forces server mode for a shared
// server and hard-fails a remote dolt host that lacks server mode.
//
// The main command path applies this via applyResolvedConfig; init builds its own
// config and must mirror the behavior. Within server mode it is still a no-op when
// no command is configured or when a --server-user flag already preset the
// username. It fails closed: a configured-but-failing command aborts init.
func applyInitGatewayCredential(ctx context.Context, beadsDir string, doltCfg *dolt.Config) error {
	if !doltCfg.ServerMode {
		return nil
	}
	fileCfg, _ := configfile.Load(beadsDir)
	if fileCfg == nil {
		fileCfg = configfile.DefaultConfig()
	}
	applied, err := dolt.ApplyGatewayCredential(ctx, fileCfg, doltCfg)
	if err != nil {
		return err
	}
	if applied {
		// ApplyGatewayCredential sets DisableAutoStart, but the hand-built init
		// config path never runs ApplyCLIAutoStart to translate that into the
		// AutoStart field the store's auto-start block actually consults.
		doltCfg.AutoStart = false
	}
	return nil
}

// resolveInitIssuePrefix decides which issue_prefix init would set. readErr is
// the error (if any) from reading the workspace's identity out of the database.
//
// Non-gateway (legacy, unchanged): if none is configured yet, return the
// sanitized prefix (dots -> underscores so issue IDs stay valid identifiers); if
// one exists, return "" — there is nothing to set, because init must not clobber
// a prefix a shared database already carries. readErr is ignored here, exactly
// as legacy init ignored it. Gateway: the prefix is server-provisioned, so an
// existing value is adopted. A missing value is a provisioning-contract
// violation — bd will not choose a prefix for a hosted database — but only when
// the read genuinely succeeded and returned empty: a read error means we could
// not consult the server, so it is surfaced as the transient failure it is
// rather than misdiagnosed as an unprovisioned database.
//
// Whether the prefix may be WRITTEN is not decided here: that is the same
// question as whether the substrate is unidentified, and issueops.Bootstrapper
// answers it inside the transaction it writes in.
func resolveInitIssuePrefix(gateway bool, existing, dbName, prefix string, readErr error) (value string, err error) {
	if existing != "" {
		return "", nil
	}
	if gateway {
		if readErr != nil {
			return "", fmt.Errorf(
				"reading issue_prefix from hosted database %q: %w", dbName, readErr)
		}
		return "", fmt.Errorf(
			"hosted database %q has no issue_prefix -- provisioning-contract violation; "+
				"bd will not choose one for a hosted database (re-provision server-side, then re-run init)",
			dbName)
	}
	return strings.ReplaceAll(prefix, ".", "_"), nil
}

// resolveInitProjectID decides init's project identity by reconciling the local
// metadata.json id (localID; "" when none is set yet) with the _project_id read
// from the database (adoptedFromDB; "" when absent or not consulted). readErr is
// the error, if any, from that read. changed reports whether the resolved id
// differs from localID, so the caller can surface the reconciliation.
//
// Gateway: the hosted database's identity is server-authoritative, so an adopted
// server id always wins and is reconciled onto local even when localID is already
// set. A re-init or orchestrator-preseeded workspace must not keep a stale local
// id: for Gateway specifically, init's CreateIfMissing:true open still skips the
// storage identity verifier (store.go verifyProjectIdentity/newServerMode — see
// its dbAlreadyExisted comment for why Gateway is exempt from the
// otherwise-CreateIfMissing:true check), so a stale id would be saved as success
// and every later normal open would then hard-fail with PROJECT IDENTITY
// MISMATCH. This CreateIfMissing skip is Gateway-only: a non-gateway
// CreateIfMissing:true init against an already-existing database now DOES run
// the verifier (GH#4637 Part A) and fails before reaching this reconciliation.
// A missing server id is a provisioning-contract violation bd will not mint
// over — even when a local id already exists — and a read error is surfaced as
// the transient failure it is, so a flaky connection is not misdiagnosed as an
// unprovisioned database.
//
// Non-gateway (legacy, unchanged): a non-empty localID is kept as-is (readErr
// ignored, exactly as before); otherwise an adopted id wins (another rig already
// chose it; minting a new one would break cross-project verification), else a
// fresh identity is generated.
func resolveInitProjectID(gateway bool, localID, adoptedFromDB, dbName string, readErr error) (value string, changed bool, err error) {
	if gateway {
		if adoptedFromDB != "" {
			return adoptedFromDB, adoptedFromDB != localID, nil
		}
		if readErr != nil {
			return "", false, fmt.Errorf(
				"reading project identity (_project_id) from hosted database %q: %w", dbName, readErr)
		}
		return "", false, fmt.Errorf(
			"hosted database %q has no provisioned project identity (_project_id) -- "+
				"provisioning-contract violation; bd will not mint an identity for a hosted database",
			dbName)
	}
	if localID != "" {
		return localID, false, nil
	}
	if adoptedFromDB != "" {
		return adoptedFromDB, true, nil
	}
	return configfile.GenerateProjectID(), true, nil
}

// shouldConsultInitProjectID reports whether init must read _project_id from the
// database before resolving the local identity.
//
// Gateway: always — the hosted server owns the identity, so init reconciles it on
// every run, including a re-init or preseeded metadata.json that already carries a
// project_id. Skipping the read when a local id was already set was the bug that
// let init save a stale id and made every later normal open hard-fail PROJECT
// IDENTITY MISMATCH.
//
// Non-gateway (legacy, unchanged): only when no local id exists yet and the
// database is a pre-existing shared/bootstrapped one worth adopting from
// (--database set or bootstrapped-from-remote). A fresh local-only init mints its
// own id without a read.
func shouldConsultInitProjectID(gateway bool, localID, database string, bootstrappedFromRemote bool) bool {
	if gateway {
		return true
	}
	return localID == "" && (database != "" || bootstrappedFromRemote)
}

// shouldWriteProjectIDLocally reports whether init should write _project_id back
// to the database. Non-gateway writes it for cross-project verification; gateway
// does not — the identity is server-authoritative, and the credential may be
// read-only.
func shouldWriteProjectIDLocally(gateway bool, projectID string) bool {
	return !gateway && projectID != ""
}

// shouldWriteInitStateToDB reports whether init may write clone-local tracking
// state into the database — the bd_version / repo_id / clone_id / last_import_time
// metadata and the initial-state Dolt commit.
//
// Non-gateway (legacy, unchanged): true — these writes give diagnostics accurate
// data and leave a clean, committed working set.
//
// Gateway: false. The database is a shared, server-owned store: repo_id and
// clone_id are per-clone fingerprints that cross-project verification (doctor,
// migrate) reads back, so a last-init-wins overwrite there produces false mismatch
// diagnostics for every other clone; and the credential may be read-only, in which
// case each write and the DOLT_COMMIT would merely spew per-init warnings. In
// gateway mode bd adopts and verifies server state instead of writing it, exactly
// as shouldWriteProjectIDLocally already suppresses the _project_id write-back.
func shouldWriteInitStateToDB(gateway bool) bool {
	return !gateway
}

// shouldInitSharedGlobalDB reports whether init should manage the local shared
// Dolt server and provision its beads_global database — starting the server,
// creating the global database, initializing its schema, and seeding its config.
//
// Shared-server mode owns that local infrastructure, so it does. Gateway mode
// does not: a gateway connects to a remote authenticating server that provisions
// databases server-side under no-create/no-schema/no-write semantics. Running the
// shared-global path there would either start a shadow local server or drive
// create/schema/write operations against the gateway — the global initializer
// rebuilds its own dolt.Config without the Gateway flag and with CreateIfMissing
// set, so it cannot preserve gateway semantics. init therefore skips the whole
// shared-global path whenever the connection is gateway-managed, mirroring how
// shouldWriteInitStateToDB / shouldWriteProjectIDLocally already suppress
// server-owned writes.
func shouldInitSharedGlobalDB(sharedServer, sharedServerMode, gateway bool) bool {
	return (sharedServer || sharedServerMode) && !gateway
}

// warnHalfIdentifiedSubstrate reports a substrate carrying one identity marker
// and not the other.
//
// Both halves are named because they fail differently. Without a project id,
// cross-project verification has nothing to compare and each rig mints its own
// local one, so the divergence is invisible until something backfills the
// database. Without a prefix, the substrate cannot name an issue and every
// later open reports the workspace as uninitialized.
func warnHalfIdentifiedSubstrate(found issueops.VerifyIdentityResult) {
	if isQuiet() {
		return
	}
	switch {
	case found.Prefix != "" && found.ProjectID == "":
		fmt.Fprintf(os.Stderr, "%s the database has an issue prefix (%s) but no project identity.\n"+
			"  bd will not complete a half-identified database. This workspace's metadata.json now carries a\n"+
			"  project id that only THIS clone knows; another clone's init will mint a different one, and the\n"+
			"  first `bd doctor --fix` will backfill the database from whichever clone ran it — after which the\n"+
			"  others refuse to open with PROJECT IDENTITY MISMATCH.\n"+
			"  Settle it deliberately: run `bd doctor --fix` from the clone whose identity should win, then\n"+
			"  re-init the others.\n",
			ui.RenderWarn("WARNING:"), found.Prefix)
	case found.ProjectID != "" && found.Prefix == "":
		fmt.Fprintf(os.Stderr, "%s the database has a project identity but no issue prefix.\n"+
			"  bd will not complete a half-identified database, and a substrate with no prefix cannot name an\n"+
			"  issue: later commands will report this workspace as uninitialized.\n"+
			"  Set it deliberately with `bd config set issue_prefix <prefix>`.\n",
			ui.RenderWarn("WARNING:"))
	}
}

// seedInitWorkspaceIdentity records the workspace's identity through
// issueops.Bootstrapper, or adopts the one already on the substrate.
//
// VERIFY, THEN BOOTSTRAP OR ADOPT. The role REFUSES an already-identified
// substrate, so asking first is how a front door tells a workspace it may
// identify from one it must leave alone. That refusal is what makes `bd init`
// safe to run against a database another rig is already minting ids in: before
// this, the proxied route rewrote both markers every time.
//
// found is the identity the caller already read with InitVerifier, in the same
// snapshot it read the prefix in. projectID is "" only when init composed no
// metadata.json — an explicit BEADS_DIR whose Dolt data lives elsewhere — and the
// workspace's own metadata.json is then the place to ask.
func seedInitWorkspaceIdentity(
	ctx context.Context,
	store storage.DoltStorage,
	found issueops.VerifyIdentityResult,
	prefix, projectID, beadsDir string,
) error {
	if store == nil {
		return nil
	}
	if found.Prefix != "" || found.ProjectID != "" {
		// Adopt. The caller has already reconciled metadata.json against
		// found.ProjectID; re-stamping the substrate would say nothing new.
		//
		// A HALF-IDENTIFIED SUBSTRATE IS SAID OUT LOUD. Bootstrapper refuses to
		// complete one on purpose — it cannot tell a half-written bootstrap
		// from a deliberately half-provisioned database, and guessing wrong
		// destroys the one it did not mean — so init cannot fix this and must
		// not leave the operator thinking it did. Silence here is what arms the
		// failure: with no _project_id to adopt, every rig's init mints a
		// DIFFERENT local one, and the first `bd doctor --fix` backfills the
		// database from whichever rig ran it, after which every other rig's
		// open hard-fails PROJECT IDENTITY MISMATCH with advice that
		// misdiagnoses the cause.
		warnHalfIdentifiedSubstrate(found)
		return nil
	}
	projectID = resolveSeedInitProjectID(projectID, beadsDir)

	bootstrapper, err := store.Bootstrapper()
	if err != nil {
		return fmt.Errorf("failed to reach the workspace identity: %v", err)
	}
	_, err = bootstrapper.Bootstrap(ctx, issueops.BootstrapRequest{
		Prefix:    prefix,
		ProjectID: projectID,
	})
	if err != nil {
		return fmt.Errorf("failed to record the workspace identity: %v", err)
	}
	return nil
}

func resolveSeedInitProjectID(projectID, beadsDir string) string {
	if projectID != "" {
		return projectID
	}
	// No metadata.json was composed on this path. Take the id the workspace
	// already records so the substrate and the file agree, and mint one only
	// when neither exists: a prefix without an identity is the
	// half-bootstrapped state the role refuses to complete later.
	if existing, err := configfile.Load(beadsDir); err == nil && existing != nil {
		projectID = existing.ProjectID
	}
	if projectID == "" {
		projectID = configfile.GenerateProjectID()
	}
	return projectID
}

var initCmd = &cobra.Command{
	Use:           "init",
	GroupID:       "setup",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "Initialize bd in the current directory",
	Long: `Initialize bd in the current directory by creating a .beads/ directory
and its storage (a Dolt database by default). Optionally specify a custom issue prefix.

Dolt is the default and only supported storage backend, with full version
control (history, branching, sync).

Use --database to specify an existing server database name, overriding the
default prefix-based naming. This is useful when an external tool (e.g. an orchestrator)
has already created the database.

With --stealth: configures per-repository git settings for invisible beads usage:
  • .git/info/exclude to prevent beads files from being committed
  Perfect for personal use without affecting repo collaborators.
  To set up a specific AI tool, run: bd setup <claude|cursor|aider|...> --stealth

By default, beads uses an embedded Dolt engine (no external server needed).
Pass --server to use an external dolt sql-server instead. In server mode,
set connection details with --server-host, --server-port, and --server-user.
Password should be set via BEADS_DOLT_PASSWORD environment variable.

Auto-export is optional. When enabled, bd exports issues to
.beads/issues.jsonl after write commands (throttled to once per 60s). This is
for viewers (bv), interchange, and issue-level migration; not backup.
Cross-machine sync and backups use Dolt remotes/backups, not JSONL import/export.
To enable: bd config set export.auto true

Non-interactive mode (--non-interactive or BD_NON_INTERACTIVE=1):
  Skips all interactive prompts, using sensible defaults:
  • Role defaults to "maintainer" (override with --role)
  • Fork exclude auto-configured when fork detected
  • Auto-export left at default (disabled)
  • --contributor uses defaults (planning repo at ~/.beads-planning, no prompts)
  • --team is rejected (wizard requires a server URL)
  Also auto-detected when stdin is not a terminal or CI=true is set.`,
	RunE: runInit,
}

func init() {
	initCmd.Flags().StringP("prefix", "p", "", "Issue prefix (default: current directory name)")
	initCmd.Flags().BoolP("quiet", "q", false, "Suppress output (quiet mode)")
	initCmd.Flags().Bool("contributor", false, "Run OSS contributor setup wizard")
	initCmd.Flags().String("planning-repo", "", "Planning repo path for --contributor (default: ~/.beads-planning)")
	initCmd.Flags().Bool("team", false, "Run team workflow setup wizard")
	initCmd.Flags().Bool("stealth", false, "Enable stealth mode: global gitattributes and gitignore, no local repo tracking")
	initCmd.Flags().Bool("setup-exclude", false, "Configure .git/info/exclude to keep beads files local (for forks)")
	initCmd.Flags().Bool("skip-hooks", false, "Skip git hooks installation")
	initCmd.Flags().Bool("skip-agents", false, "Skip AGENTS.md and Claude/Codex/Cursor setup generation")
	initCmd.Flags().Bool("force", false, "Deprecated alias for --reinit-local. Bypasses only the LOCAL data-safety guard; does NOT authorize remote divergence (see 'bd help init-safety').")
	initCmd.Flags().Bool("reinit-local", false, "Re-initialize local .beads/ over existing local data. Does NOT authorize remote divergence; see --discard-remote.")
	initCmd.Flags().Bool("discard-remote", false, "Authorize discarding the configured remote's Dolt history when re-initializing. Requires --destroy-token in non-interactive mode; see 'bd help init-safety'.")
	initCmd.Flags().Bool("from-jsonl", false, "Import issues from configured import.path; refuses remote history unless --discard-remote authorizes replacement")
	initCmd.Flags().Bool("init-if-missing", false, "If the workspace is already initialized, skip init and exit 0 instead of failing (idempotent init for scaffolds)")
	initCmd.Flags().String("destroy-token", "", "Explicit confirmation token for destructive re-init in non-interactive mode (format: 'DESTROY-<prefix>')")
	initCmd.Flags().String("agents-template", "", "Path to custom AGENTS.md template (overrides embedded default)")
	initCmd.Flags().String("agents-profile", "", "AGENTS.md profile: 'minimal' (default, pointer to bd prime) or 'full' (complete command reference)")
	initCmd.Flags().String("agents-file", "", "Custom filename for agent instructions (default: AGENTS.md)")
	initCmd.Flags().String("remote", "", "Dolt remote URL to clone from and persist as sync.remote")

	// Non-interactive mode for CI/cloud agents
	initCmd.Flags().Bool("non-interactive", false, "Skip all interactive prompts (auto-detected in CI or non-TTY environments)")
	initCmd.Flags().String("role", "", "Set beads role without prompting: \"maintainer\" or \"contributor\"")

	// Backend selection: Dolt is the default and only supported backend.
	initCmd.Flags().String("backend", "", "Storage backend: dolt (default). Removed backends (postgres, mysql, sqlite) print migration guidance.")
	// Keep the short-lived removed-backend flags as hidden parser tombstones. A
	// legacy invocation can then reach the backend rollback guidance instead of
	// failing early with an opaque "unknown flag" error. The current backend rejects
	// these flags explicitly above; they are never consumed as connection data.
	for _, legacyFlag := range removedBackendInitFlags {
		initCmd.Flags().String(legacyFlag.name, "", legacyFlag.usage)
		_ = initCmd.Flags().MarkHidden(legacyFlag.name)
	}

	// Dolt server connection flags
	initCmd.Flags().Bool("server", false, "Use external dolt sql-server instead of embedded engine")
	initCmd.Flags().String("server-host", "", "Dolt server host (default: 127.0.0.1)")
	initCmd.Flags().Bool("server-tls", false, "Require TLS for the init-time Dolt server connection (overrides BEADS_DOLT_SERVER_TLS for this run; not persisted - set the env var or credentials file for later commands)")
	initCmd.Flags().Int("server-port", 0, "Dolt server port (default: 3307)")
	initCmd.Flags().String("server-socket", "", "Unix domain socket path (overrides host/port; pass '' to ignore an ambient BEADS_DOLT_SERVER_SOCKET and use TCP)")
	initCmd.Flags().String("server-user", "", "Dolt server MySQL user (default: root)")
	initCmd.Flags().Bool("shared-server", false, "Enable shared Dolt server mode (all projects share one server at ~/.beads/shared-server/)")
	initCmd.Flags().Bool("external", false, "Server is externally managed (skip server startup); use with --shared-server or --server")
	initCmd.Flags().Bool("debug", false, "Run the managed Dolt sql-server with --loglevel=debug and CPU profiling (--prof cpu). Persisted to config.yaml as dolt.debug. No effect on externally-managed servers.")
	initCmd.Flags().Bool("proxied-server", false, "[EXPERIMENTAL] Use a per-workspace proxied dolt sql-server (proxy + child dolt) rooted at .beads/dolt")
	initCmd.Flags().Bool("team-server", false, "[EXPERIMENTAL] The shared database's schema is managed by beads-team-server (bts): bd never creates the database or runs schema migrations, only verifies the schema version (proxied-server mode only). Not related to --team.")
	initCmd.Flags().String("proxied-server-config-path", "", "[EXPERIMENTAL] Absolute path to an existing dolt sql-server YAML config (proxied-server mode only). When set, bd uses this file instead of auto-generating one. Relative paths are rejected. Managed mode requires listener.host to be a numeric loopback IP (hostnames including localhost, non-loopback addresses, listener.socket, remotesapi, and cluster config are rejected); the same policy applies to BEADS_PROXIED_SERVER_CONFIG.")
	initCmd.Flags().String("proxied-server-log-path", "", "[EXPERIMENTAL] Absolute path to the proxied dolt sql-server log file (proxied-server mode only). Default: <beadsDir>/dolt/server.log. Relative paths are rejected.")
	initCmd.Flags().String("proxied-server-root-path", "", "[EXPERIMENTAL] Absolute directory holding the proxied dolt sql-server's lockfiles, pidfiles, and child .dolt repository (proxied-server mode only). Default: <beadsDir>/dolt. May not exist yet — bd will create it. Relative paths are rejected.")
	initCmd.Flags().Int("proxied-server-port", 0, "[EXPERIMENTAL] Fixed TCP port for the proxy's loopback listener (proxied-server mode only). Default 0 = an OS-assigned free port. Startup fails if the port is already in use.")
	initCmd.Flags().Duration("proxied-server-idle-timeout", 0, "[EXPERIMENTAL] Idle duration after which the proxy shuts down its loopback listener and backend (proxied-server mode only). Omit for the built-in default (30s); 0 keeps the proxy and backend alive indefinitely; a positive value sets the window.")
	initCmd.Flags().String("proxied-server-external-host", "", "[EXPERIMENTAL] Hostname or IP of an externally-managed dolt sql-server the proxy should front (proxied-server mode only). Mutually exclusive with --proxied-server-external-socket-path.")
	initCmd.Flags().Int("proxied-server-external-port", 0, "[EXPERIMENTAL] TCP port of the externally-managed dolt sql-server (proxied-server mode only). Required when --proxied-server-external-host is set.")
	initCmd.Flags().String("proxied-server-external-socket-path", "", "[EXPERIMENTAL] Absolute unix socket path of the externally-managed dolt sql-server (proxied-server mode only). Mutually exclusive with --proxied-server-external-host. Relative paths are rejected.")
	initCmd.Flags().String("proxied-server-external-user", "", "[EXPERIMENTAL] MySQL user for the externally-managed dolt sql-server (proxied-server mode only). Defaults to \"root\" when empty. Password is read at runtime from $BEADS_PROXIED_SERVER_EXTERNAL_PASSWORD and is never persisted to disk.")
	initCmd.Flags().Bool("proxied-server-external-tls", false, "[EXPERIMENTAL] Require TLS when connecting to the externally-managed dolt sql-server (proxied-server mode only).")
	initCmd.Flags().String("proxied-server-external-tls-ca-cert-path", "", "[EXPERIMENTAL] Absolute path to a CA certificate (PEM) used to verify the externally-managed dolt sql-server. Empty uses the system trust store. Relative paths are rejected.")
	initCmd.Flags().String("proxied-server-external-tls-cert-path", "", "[EXPERIMENTAL] Absolute path to a client TLS certificate (for mTLS to the externally-managed dolt sql-server). Must be paired with --proxied-server-external-tls-key-path. Relative paths are rejected.")
	initCmd.Flags().String("proxied-server-external-tls-key-path", "", "[EXPERIMENTAL] Absolute path to the client TLS private key (for mTLS to the externally-managed dolt sql-server). Must be paired with --proxied-server-external-tls-cert-path. Relative paths are rejected.")
	initCmd.Flags().String("proxied-server-external-tls-server-name", "", "[EXPERIMENTAL] Server name to verify in the external dolt sql-server's TLS certificate. Defaults to the external host. Required with a unix socket unless --proxied-server-external-tls-skip-verify is set.")
	initCmd.Flags().Bool("proxied-server-external-tls-skip-verify", false, "[EXPERIMENTAL] Skip TLS certificate verification for the external dolt sql-server. Insecure; testing only.")
	initCmd.Flags().Duration("proxied-server-external-keep-alive", 0, "[EXPERIMENTAL] TCP keepalive period for the proxy→external connection. Zero uses the package default (30s).")

	rootCmd.AddCommand(initCmd)
}

var removedBackendInitFlags = []struct {
	name      string
	usage     string
	origin    string
	rationale string
}{
	{name: "pg-url", usage: "Legacy PostgreSQL connection URL (removed backend compatibility only)", origin: "the removed PostgreSQL/MySQL initialization paths", rationale: configfile.RemovedBackendRationale},
	{name: "pg-schema", usage: "Legacy PostgreSQL schema (removed backend compatibility only)", origin: "the removed PostgreSQL/MySQL initialization paths", rationale: configfile.RemovedBackendRationale},
	{name: "mysql-url", usage: "Legacy MySQL connection URL (removed backend compatibility only)", origin: "the removed PostgreSQL/MySQL initialization paths", rationale: configfile.RemovedBackendRationale},
	{name: "mysql-database", usage: "Legacy MySQL database (removed backend compatibility only)", origin: "the removed PostgreSQL/MySQL initialization paths", rationale: configfile.RemovedBackendRationale},
	{name: "sqlite-path", usage: "Legacy SQLite database file (removed backend compatibility only)", origin: "the removed SQLite initialization path", rationale: configfile.RemovedSQLiteRationale},
}

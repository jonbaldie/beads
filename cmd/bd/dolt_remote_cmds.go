package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/doltutil"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// staleDatabasePrefixes lists database name prefixes that
// `bd dolt clean-databases` will drop. This is the cleanup side of the
// test/prod split. Two sibling lists must converge with it (be-avn):
//   - internal/storage/dolt/store.go:testDatabasePrefixes (firewall side)
//   - .gc/system/packs/dolt/formulas/mol-dog-stale-db.toml (city formula)
//
// The firewall list, this cleanup list, and the formula list MUST
// converge — operators rely on consistent semantics across `bd dolt
// clean-databases`, the SQL-side firewall, and `gc dolt cleanup`.
//
// Origin of each prefix:
//   - testdb_     : applyConfigDefaults derives this for BEADS_TEST_MODE=1
//     without an explicit Database (FNV hash of cfg.Path).
//   - beads_test  : convention for hand-written integration tests.
//   - beads_pt    : property-test fixtures.
//   - beads_vr    : version-roundtrip / migration fixtures.
//   - doctest_    : `bd doctor` self-check fixtures.
//   - doctortest_ : older `bd doctor` fixture name (kept for back-compat).
//   - benchdb_    : per-bench scratch DBs (cmd/bd/template_test.go
//     newTemplateBenchmarkStore, format `benchdb_<unixnano>`).
var staleDatabasePrefixes = []string{
	"testdb_",
	"beads_test",
	"beads_pt",
	"beads_vr",
	"doctest_",
	"doctortest_",
	"benchdb_",
}

var doltCleanDatabasesCmd = &cobra.Command{
	Use:           "clean-databases",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "Drop stale test databases from the Dolt server",
	Long: `Identify and drop leftover test and agent databases that accumulate
on the shared Dolt server from interrupted test runs and terminated agents.

Stale database prefixes: testdb_*, beads_test*, beads_pt*, beads_vr*, doctest_*, doctortest_*, benchdb_*

These waste server memory and can degrade performance under concurrent load.
Use --dry-run to see what would be dropped without actually dropping.

DROP DATABASE only marks a database as dropped; Dolt keeps its directory
under .dolt_dropped_databases/ so it can be restored with
CALL DOLT_UNDROP(name) until an explicit purge — disk is not reclaimed
until then. Pass --purge-dropped to run CALL DOLT_PURGE_DROPPED_DATABASES()
after cleanup.

--purge-dropped is SERVER-GLOBAL and IRREVERSIBLE. Dolt has no way to scope
the purge to only the databases this run dropped: it permanently deletes
every dropped-but-not-yet-purged database on the server, including ones
dropped by something else entirely (e.g. an operator's accidental
DROP DATABASE on an unrelated database that was still recoverable via
DOLT_UNDROP). It also purges pre-existing residue from earlier
clean-databases runs even if this run finds no stale databases to drop.
Only pass it when nothing else on the server may be relying on DOLT_UNDROP
recovery.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		beadsDir := selectedDoltBeadsDir()
		if beadsDir == "" {
			return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
		}
		if _, err := loadDoltBackendConfig(beadsDir); err != nil {
			return HandleError("%v", err)
		}
		if !usesSQLServer() {
			return HandleError("'bd dolt clean-databases' is not supported in embedded mode (no Dolt server)")
		}
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		purgeDropped, _ := cmd.Flags().GetBool("purge-dropped")
		opts := cleanDatabasesOptions{dryRun: dryRun, purgeDropped: purgeDropped}

		if usesProxiedServer() {
			return runDoltCleanDatabasesProxied(getRootContext(), beadsDir, opts)
		}

		// Connect directly to the Dolt server via config instead of getStore(),
		// which isn't initialized for dolt subcommands (beads-9vt).
		db, cleanup, err := openDoltServerConnection()
		if err != nil {
			return err
		}
		defer cleanup()

		return cleanDatabases(getRootContext(), db, opts)
	},
}

// shouldPurgeDroppedDatabases reports whether clean-databases should invoke
// the (server-global, irreversible) purge. It gates purely on the
// --purge-dropped flag and deliberately ignores droppedCount: a prior
// clean-databases run may have left dropped-but-unpurged residue that this
// run's SHOW DATABASES scan never sees (the residue is already gone from
// SHOW DATABASES the moment it was dropped), so --purge-dropped must still
// fire the purge even when this run drops nothing. Extracted as a pure
// function so the gating contract itself — not just the SQL-level purge
// mechanism — has direct unit coverage.
func shouldPurgeDroppedDatabases(purgeDropped bool, droppedCount int) bool {
	_ = droppedCount // deliberately unused — see doc comment above
	return purgeDropped
}

// purgeDroppedDatabases issues Dolt's DOLT_PURGE_DROPPED_DATABASES() stored
// procedure, which permanently deletes database directories that DROP
// DATABASE only moved into .dolt_dropped_databases/. This is server-global:
// Dolt has no way to scope it to a particular set of databases, so it
// purges every dropped-but-not-yet-purged database on the server, not just
// ones this process dropped. Extracted so tests can drive it directly
// against a live test server without going through the full
// clean-databases command wiring (config loading, SHOW DATABASES scan,
// batching/backoff).
func purgeDroppedDatabases(ctx context.Context, conn versioncontrolops.DBConn) error {
	purgeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, err := conn.ExecContext(purgeCtx, "CALL DOLT_PURGE_DROPPED_DATABASES()")
	return err
}

// --- Dolt remote management commands ---

type doltRemoteAddStore interface {
	ListRemotes(ctx context.Context) ([]storage.RemoteInfo, error)
	AddRemote(ctx context.Context, name, url string) error
	RemoveRemote(ctx context.Context, name string) error
}

type doltRemoteAddResult struct {
	Canceled bool
}

type doltRemoteOverwriteConfirmer func(surface, name, existingURL, newURL string) bool

func confirmDoltRemoteOverwrite(surface, name, existingURL, newURL string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return true
	}
	fmt.Printf("  Remote %q already exists on %s: %s\n", name, surface, existingURL)
	fmt.Printf("  Overwrite with: %s\n", newURL)
	fmt.Print("  Overwrite? (y/N): ")
	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "y" || response == "yes"
}

func findDoltRemoteURL(remotes []storage.RemoteInfo, name string) string {
	for _, remote := range remotes {
		if remote.Name == name {
			return remote.URL
		}
	}
	return ""
}

func resolveDoltRemoteForAdd(st doltRemoteAddStore, remotes []storage.RemoteInfo, name string) (string, bool) {
	existingURL := findDoltRemoteURL(remotes, name)
	if existingURL != "" {
		return existingURL, false
	}
	// An empty listing is not proof the remote is absent: a freshly
	// (auto-)started sql-server can report empty dolt_remotes while the
	// remote is persisted on disk (GH#2118, wy-6k7f7). Recover the
	// persisted URL so the add gets the same match/confirm treatment it
	// would after the window, instead of silently writing over an
	// invisible remote.
	existingURL = findDoltRemoteURL(persistedRemoteInfosFor(st), name)
	return existingURL, existingURL != ""
}

func replaceDoltRemote(ctx context.Context, st doltRemoteAddStore, name, url string, existingFromDiskOnly bool) error {
	if err := st.RemoveRemote(ctx, name); err != nil {
		// A remote known only from disk may not be removable through a
		// cold-started server that doesn't see it yet; the confirmed add
		// below is what establishes the new URL either way.
		if !existingFromDiskOnly {
			return fmt.Errorf("remove existing remote %s: %w", name, err)
		}
	}
	if err := st.AddRemote(ctx, name, url); err != nil {
		return fmt.Errorf("add remote %s: %w", name, err)
	}
	return nil
}

func ensureDoltRemote(ctx context.Context, st doltRemoteAddStore, name, url string, confirm doltRemoteOverwriteConfirmer) (doltRemoteAddResult, error) {
	remotes, err := st.ListRemotes(ctx)
	if err != nil {
		return doltRemoteAddResult{}, fmt.Errorf("list existing remotes: %w", err)
	}

	existingURL, existingFromDiskOnly := resolveDoltRemoteForAdd(st, remotes, name)
	if existingURL == "" {
		if err := st.AddRemote(ctx, name, url); err != nil {
			return doltRemoteAddResult{}, fmt.Errorf("add remote %s: %w", name, err)
		}
		return doltRemoteAddResult{}, nil
	}

	if doltutil.RemoteURLsMatch(existingURL, url) {
		return doltRemoteAddResult{}, nil
	}

	if !confirm("SQL server", name, existingURL, url) {
		return doltRemoteAddResult{Canceled: true}, nil
	}
	if err := replaceDoltRemote(ctx, st, name, url, existingFromDiskOnly); err != nil {
		return doltRemoteAddResult{}, err
	}
	return doltRemoteAddResult{}, nil
}

var doltRemoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Manage Dolt remotes",
	Long: `Manage Dolt remotes for push/pull replication.

Subcommands:
  add <name> <url>     Add a new remote
  list                 List all configured remotes
  remove <name>        Remove a remote
  reset-data <name>    Replace a remote's data plane after a history squash`,
}

var doltRemoteAddCmd = &cobra.Command{
	Use:           "add <name> <url>",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "Add a Dolt remote",
	Args:          cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if isDoltLocalOnly() {
			fmt.Fprintln(os.Stderr, "Error: cannot add Dolt remote: remote sync is disabled (dolt.local-only=true).")
			fmt.Fprintln(os.Stderr, "To re-enable remote sync: bd config unset dolt.local-only")
			return SilentExit()
		}
		allowGitOrigin, _ := cmd.Flags().GetBool("allow-git-origin")
		if doltRemoteMatchesGitOrigin(args[1]) {
			if !allowGitOrigin {
				fmt.Fprintf(os.Stderr, "Error: refusing to add %q as a Dolt remote — this URL matches the git origin.\n", args[1])
				fmt.Fprintln(os.Stderr, "  Hint: use --allow-git-origin to proceed anyway (e.g. monorepo layout).")
				fmt.Fprintln(os.Stderr, "  Hint: or set dolt.local-only=true to disable remote sync entirely.")
				return SilentExit()
			}
			fmt.Fprintf(os.Stderr, "Warning: %q matches the git origin — proceeding because --allow-git-origin is set.\n", args[1])
		}
		ctx := context.Background()
		st := getStore()
		if st == nil {
			return HandleError("no store available")
		}
		name, url := args[0], args[1]

		result, err := ensureDoltRemote(ctx, st, name, url, confirmDoltRemoteOverwrite)
		if err != nil {
			if isJSONOutput() {
				_ = outputJSONError(err, "remote_add_failed")
			} else {
				fmt.Fprintf(os.Stderr, "Error adding remote: %v\n", err)
			}
			return SilentExit()
		}
		if result.Canceled {
			fmt.Println("Canceled.")
			return nil
		}

		if name == "origin" {
			if err := config.SetYamlConfig("sync.remote", url); err != nil {
				return HandleError("failed to persist sync.remote to config.yaml: %v", err)
			}
			if isGitRepo() {
				commitBeadsConfig("bd: update sync.remote")
			}
		}

		if isJSONOutput() {
			if err := outputJSON(map[string]interface{}{
				"name": name,
				"url":  url,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			}
		} else {
			fmt.Printf("Added remote %q → %s\n", name, url)
		}
		return nil
	},
}

var doltRemoteListCmd = &cobra.Command{
	Use:           "list",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "List configured Dolt remotes",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		st := getStore()
		if st == nil {
			return HandleError("no store available")
		}

		remotes, err := st.ListRemotes(ctx)
		if err != nil {
			if isJSONOutput() {
				_ = outputJSONError(err, "remote_list_failed")
			} else {
				fmt.Fprintf(os.Stderr, "Error listing remotes: %v\n", err)
			}
			return SilentExit()
		}

		if isJSONOutput() {
			if err := outputJSON(formatDoltRemoteListJSON(remotes)); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			}
			return nil
		}

		if len(remotes) == 0 {
			fmt.Println("No remotes configured.")
			return nil
		}

		for _, r := range remotes {
			fmt.Printf("%-20s %s\n", r.Name, r.URL)
		}
		return nil
	},
}

type doltRemoteListJSON struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SQLURL string `json:"sql_url,omitempty"`
	CLIURL string `json:"cli_url,omitempty"`
	Status string `json:"status"`
}

func formatDoltRemoteListJSON(remotes []storage.RemoteInfo) []doltRemoteListJSON {
	out := make([]doltRemoteListJSON, 0, len(remotes))
	for _, r := range remotes {
		out = append(out, doltRemoteListJSON{
			Name:   r.Name,
			URL:    r.URL,
			SQLURL: r.URL,
			Status: "ok",
		})
	}
	return out
}

var doltRemoteRemoveCmd = &cobra.Command{
	Use:           "remove <name>",
	Short:         "Remove a Dolt remote",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		name := args[0]

		if usesProxiedServer() {
			return runDoltRemoteRemoveProxied(ctx, name)
		}

		st := getStore()
		if st == nil {
			return HandleError("no store available")
		}

		if err := st.RemoveRemote(ctx, name); err != nil {
			if isJSONOutput() {
				_ = outputJSONError(err, "remote_remove_failed")
			} else {
				fmt.Fprintf(os.Stderr, "Error removing remote: %v\n", err)
			}
			return SilentExit()
		}

		if name == "origin" {
			if current := config.GetYamlConfig("sync.remote"); current != "" {
				if err := config.UnsetYamlConfig("sync.remote"); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to clear sync.remote from config.yaml: %v\n", err)
				}
				if isGitRepo() {
					commitBeadsConfig("bd: clear sync.remote")
				}
			}
		}

		if isJSONOutput() {
			if err := outputJSON(map[string]interface{}{
				"name":    name,
				"removed": true,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			}
		} else {
			fmt.Printf("Removed remote %q\n", name)
		}
		return nil
	},
}

// isTimeoutError checks if an error is a context deadline exceeded or timeout.
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if err == context.DeadlineExceeded {
		return true
	}
	// Check for net.Error timeout (covers TCP and MySQL driver timeouts)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Also catch wrapped context.DeadlineExceeded
	return errors.Is(err, context.DeadlineExceeded)
}

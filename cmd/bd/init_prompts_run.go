package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// serverConnEnvMutation is one pending change to the process environment.
// unset records the "remove this variable" case, which is distinct from
// setting it to the empty string for every downstream resolver.
type serverConnEnvMutation struct {
	key   string
	value string
	unset bool
}

// resolveExplicitServerConnEnv turns the explicit --server-* flags into the
// set of environment changes promoteExplicitServerConnFlags will apply. It
// validates every flag before returning, so a later invalid flag cannot leave
// an earlier valid one already applied to the process environment.
//
// Because a socket outranks host/port everywhere downstream, selecting TCP
// explicitly (--server-host or --server-port) also clears an ambient
// BEADS_DOLT_SERVER_SOCKET unless --server-socket was itself given. An
// explicitly empty --server-socket clears the ambient socket too: empty is
// the documented "use TCP" value (see configfile.GetDoltServerSocket).
// Changed-but-empty host/user and out-of-range port values fail explicitly
// rather than being silently ignored.
func resolveExplicitServerConnEnv(cmd *cobra.Command) ([]serverConnEnvMutation, error) {
	var muts []serverConnEnvMutation
	var err error
	if muts, err = appendExplicitServerHostMutation(cmd, muts); err != nil {
		return nil, err
	}
	if muts, err = appendExplicitServerPortMutation(cmd, muts); err != nil {
		return nil, err
	}
	if muts, err = appendExplicitServerUserMutation(cmd, muts); err != nil {
		return nil, err
	}
	muts = appendExplicitServerSocketMutation(cmd, muts)
	muts = appendExplicitServerTLSMutation(cmd, muts)
	return muts, nil
}

func appendExplicitServerHostMutation(cmd *cobra.Command, muts []serverConnEnvMutation) ([]serverConnEnvMutation, error) {
	if !cmd.Flags().Changed("server-host") {
		return muts, nil
	}
	v, _ := cmd.Flags().GetString("server-host")
	if v == "" {
		return nil, fmt.Errorf("--server-host cannot be empty; omit the flag to use BEADS_DOLT_SERVER_HOST or the default (%s)", configfile.DefaultDoltServerHost)
	}
	return append(muts, serverConnEnvMutation{key: "BEADS_DOLT_SERVER_HOST", value: v}), nil
}

func appendExplicitServerPortMutation(cmd *cobra.Command, muts []serverConnEnvMutation) ([]serverConnEnvMutation, error) {
	if !cmd.Flags().Changed("server-port") {
		return muts, nil
	}
	v, _ := cmd.Flags().GetInt("server-port")
	if v < 1 || v > 65535 {
		return nil, fmt.Errorf("--server-port must be between 1 and 65535, got %d; omit the flag to use BEADS_DOLT_SERVER_PORT or the default (%d)", v, configfile.DefaultDoltServerPort)
	}
	return append(muts, serverConnEnvMutation{key: "BEADS_DOLT_SERVER_PORT", value: strconv.Itoa(v)}), nil
}

func appendExplicitServerUserMutation(cmd *cobra.Command, muts []serverConnEnvMutation) ([]serverConnEnvMutation, error) {
	if !cmd.Flags().Changed("server-user") {
		return muts, nil
	}
	v, _ := cmd.Flags().GetString("server-user")
	if v == "" {
		return nil, fmt.Errorf("--server-user cannot be empty; omit the flag to use BEADS_DOLT_SERVER_USER or the default (%s)", configfile.DefaultDoltServerUser)
	}
	return append(muts, serverConnEnvMutation{key: "BEADS_DOLT_SERVER_USER", value: v}), nil
}

func appendExplicitServerSocketMutation(cmd *cobra.Command, muts []serverConnEnvMutation) []serverConnEnvMutation {
	if cmd.Flags().Changed("server-socket") {
		v, _ := cmd.Flags().GetString("server-socket")
		if v == "" {
			return append(muts, serverConnEnvMutation{key: "BEADS_DOLT_SERVER_SOCKET", unset: true})
		}
		return append(muts, serverConnEnvMutation{key: "BEADS_DOLT_SERVER_SOCKET", value: v})
	}
	if cmd.Flags().Changed("server-host") || cmd.Flags().Changed("server-port") {
		return append(muts, serverConnEnvMutation{key: "BEADS_DOLT_SERVER_SOCKET", unset: true})
	}
	return muts
}

func appendExplicitServerTLSMutation(cmd *cobra.Command, muts []serverConnEnvMutation) []serverConnEnvMutation {
	if !cmd.Flags().Changed("server-tls") {
		return muts
	}
	v, _ := cmd.Flags().GetBool("server-tls")
	tls := "0"
	if v {
		tls = "1"
	}
	return append(muts, serverConnEnvMutation{key: "BEADS_DOLT_SERVER_TLS", value: tls})
}

// promoteExplicitServerConnFlags makes an explicit --server-host/--server-port/
// --server-user/--server-socket/--server-tls flag outrank the corresponding
// BEADS_DOLT_SERVER_* environment variable. Every downstream resolver
// (configfile getters, doltserver DefaultConfig) consults the environment
// first, so without promotion a stale shell-profile value silently redirects
// init to a different server than the one named on the command line.
//
// Callers must invoke this only once server mode is resolved: in embedded
// mode the connection flags are recorded in metadata.json but must not reach
// the environment, or init trips its own "embedded mode has no host/port"
// guard.
//
// The returned restore function puts the environment back exactly as it was,
// including variables that were absent. One CLI process runs one init, so the
// mutation is invisible there, but any in-process caller (the test binary, an
// embedding host) would otherwise inherit this invocation's overrides.
// Callers should defer it on every return path.
//
// Nothing here is persisted to metadata.json (TLS in particular stays
// env/credentials-file configured, per bd dolt help).
func promoteExplicitServerConnFlags(cmd *cobra.Command) (func(), error) {
	noop := func() {}
	muts, err := resolveExplicitServerConnEnv(cmd)
	if err != nil {
		return noop, err
	}
	if len(muts) == 0 {
		return noop, nil
	}

	type savedEnv struct {
		value   string
		present bool
	}
	saved := make(map[string]savedEnv, len(muts))
	restore := func() {
		for key, prev := range saved {
			// Nothing actionable remains if the restore itself fails: the
			// process is either exiting or already past the init it scoped.
			if prev.present {
				_ = os.Setenv(key, prev.value)
				continue
			}
			_ = os.Unsetenv(key)
		}
	}

	for _, mut := range muts {
		if _, seen := saved[mut.key]; !seen {
			value, present := os.LookupEnv(mut.key)
			saved[mut.key] = savedEnv{value: value, present: present}
		}
		var applyErr error
		if mut.unset {
			applyErr = os.Unsetenv(mut.key)
		} else {
			applyErr = os.Setenv(mut.key, mut.value)
		}
		if applyErr != nil {
			restore()
			return noop, fmt.Errorf("applying %s: %w", mut.key, applyErr)
		}
	}
	return restore, nil
}

func initDoltServerTLSFromEnv() bool {
	return (&configfile.Config{}).GetDoltServerTLS()
}

func initTimeCloneConfig(serverMode bool, serverHost string, serverPort int, serverSocket, serverUser, dbName string) *configfile.Config {
	cfg := configfile.DefaultConfig()
	cfg.Backend = configfile.BackendDolt
	cfg.DoltDatabase = dbName
	if serverMode {
		cfg.DoltMode = configfile.DoltModeServer
		cfg.DoltServerHost = configfile.DefaultDoltServerHost
		cfg.DoltServerUser = configfile.DefaultDoltServerUser
	} else {
		cfg.DoltMode = configfile.DoltModeEmbedded
	}
	if serverHost != "" {
		cfg.DoltServerHost = serverHost
	}
	if serverPort != 0 {
		cfg.DoltServerPort = serverPort
	}
	if serverSocket != "" {
		cfg.DoltServerSocket = serverSocket
	}
	if serverUser != "" {
		cfg.DoltServerUser = serverUser
	}
	return cfg
}

func persistInitSyncRemote(beadsDir, initRemote, syncURL string, syncFromRemote, syncURLFromConfig, syncURLFromGitOrigin bool) error {
	if initRemote != "" {
		return config.SetYamlConfigInDir(beadsDir, "sync.remote", initRemote)
	}
	if !shouldWireInitRemote(syncURL, syncFromRemote, syncURLFromConfig, syncURLFromGitOrigin) {
		return nil
	}
	if existing := config.GetStringFromDir(beadsDir, "sync.remote"); existing != "" {
		return nil
	}
	return config.SetYamlConfigInDir(beadsDir, "sync.remote", syncURL)
}

func isEmptyRemoteCloneError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "contains no dolt data")
}

type initStateCommitter interface {
	CommitWithConfig(context.Context, string) error
}

// commitInitState commits the configuration init deliberately created as well
// as the initial schema state. The ordinary Commit contract excludes config to
// avoid sweeping unrelated stale values, so using it here leaves every fresh
// database dirty immediately after a successful init.
func commitInitState(ctx context.Context, store initStateCommitter) error {
	return store.CommitWithConfig(ctx, "bd init")
}

// verifyMetadata writes a metadata field and verifies the write succeeded.
// Returns true if write+verify succeeded, false with warning if either failed.
func verifyMetadata(ctx context.Context, store storage.DoltStorage, key, value string) bool {
	if err := store.SetMetadata(ctx, key, value); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write %s metadata: %v\n", key, err)
		if usesSQLServer() {
			fmt.Fprintf(os.Stderr, "  Run 'bd doctor --fix' to repair.\n")
		}
		return false
	}
	// Verify read-back
	readBack, err := store.GetMetadata(ctx, key)
	if err != nil || readBack != value {
		fmt.Fprintf(os.Stderr, "Warning: %s metadata write did not persist (wrote %q, read %q)\n", key, value, readBack)
		if usesSQLServer() {
			fmt.Fprintf(os.Stderr, "  Run 'bd doctor --fix' to repair.\n")
		}
		return false
	}
	return true
}

// initGlobalDatabaseConfig opens a store connection to the beads_global database
// and seeds its configuration (issue prefix, project ID). The database must already
// exist (created by EnsureGlobalDatabase). This function is idempotent — it only
// sets config values that are not already present.
func initGlobalDatabaseConfig(ctx context.Context, projectCfg *dolt.Config, quiet bool) {
	globalCfg := &dolt.Config{
		Path:     projectCfg.Path,
		BeadsDir: projectCfg.BeadsDir,
		Database: doltserver.GlobalDatabaseName,
		ServerOptions: dolt.ServerOptions{
			ServerHost:     projectCfg.ServerHost,
			ServerPort:     projectCfg.ServerPort,
			ServerUser:     projectCfg.ServerUser,
			ServerPassword: projectCfg.ServerPassword,
			ServerMode:     true,
			AutoStart:      false,
		},
		RemoteOptions: dolt.RemoteOptions{
			CreateIfMissing: true,
		},
		// server is already running,
	}

	globalStore, err := newDoltStore(ctx, globalCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to open global database: %v\n", err)
		return
	}
	defer func() { _ = globalStore.Close() }()

	// Set issue prefix (only if not already configured)
	existing, _ := globalStore.GetConfig(ctx, "issue_prefix")
	if existing == "" {
		if err := globalStore.SetConfig(ctx, "issue_prefix", doltserver.GlobalIssuePrefix); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to set global issue prefix: %v\n", err)
		}
	}

	// Set well-known project ID for the global database
	existingID, _ := globalStore.GetMetadata(ctx, "_project_id")
	if existingID == "" {
		if err := globalStore.SetMetadata(ctx, "_project_id", doltserver.GlobalProjectID); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to set global project ID: %v\n", err)
		}
	}

	if !quiet {
		fmt.Printf("  %s Global database schema initialized\n", ui.RenderPass("✓"))
	}
}

func resolveInitDoltMode(proxiedFlag, sharedFlag, serverFlag bool) string {
	if proxiedFlag || os.Getenv("BEADS_DOLT_PROXIED_SERVER") == "1" {
		return "proxied-server"
	}
	shared := os.Getenv("BEADS_DOLT_SHARED_SERVER")
	if sharedFlag || shared == "1" || strings.EqualFold(shared, "true") {
		return "shared-server"
	}
	if serverFlag || os.Getenv("BEADS_DOLT_SERVER_MODE") == "1" {
		return "server"
	}
	return "embedded"
}

package main

import (
	"github.com/spf13/cobra"
)

var doltCmd = &cobra.Command{
	Use:     "dolt",
	GroupID: "setup",
	Short:   "Configure Dolt database settings",
	Long: `Configure and manage Dolt database settings and server lifecycle.

Beads uses a dolt sql-server for all database operations. The server is
auto-started transparently when needed. Use these commands for explicit
control or diagnostics.

Server lifecycle:
  bd dolt start        Start the Dolt server for this project
  bd dolt stop         Stop the Dolt server for this project
  bd dolt status       Show Dolt server status

Configuration:
  bd dolt show         Show current Dolt configuration with connection test
  bd dolt set <k> <v>  Set a configuration value
  bd dolt test         Test server connection

Version control:
  bd dolt commit       Commit pending changes
  bd dolt push         Push commits to Dolt remote
  bd dolt pull         Pull commits from Dolt remote

Remote management:
  bd dolt remote add <name> <url>   Add a Dolt remote
  bd dolt remote list                List configured remotes
  bd dolt remote remove <name>       Remove a Dolt remote

Configuration keys for 'bd dolt set':
  database  Database name (default: issue prefix or "beads")
  host      Server host (default: 127.0.0.1)
  port      Server port (auto-detected; override with bd dolt set port <N>)
  user      MySQL user (default: root)
  data-dir  Custom dolt data directory (absolute path; default: .beads/dolt)

Remote server authentication (password + TLS) is NOT stored via 'bd dolt set'
(keeps secrets out of metadata.json). Configure them with:

  BEADS_DOLT_PASSWORD       Server password (highest priority)
  BEADS_DOLT_SERVER_TLS     Enable TLS (set to "1" or "true")
  BEADS_DOLT_SERVER_USER    MySQL user override (else use 'bd dolt set user')
  BEADS_CREDENTIALS_FILE    Optional path to credentials file

  Default credentials file: ~/.config/beads/credentials (Linux/macOS)
                            %APPDATA%\beads\credentials (Windows)
  Format (INI, section = host:port of the resolved connection):
    [127.0.0.1:3307]
    password = secret

  Password resolution: BEADS_DOLT_PASSWORD → credentials [host:port] → empty.
  Full reference: docs/architecture/dolt.md (Environment Variables / Credentials).

Flags for 'bd dolt set':
  --update-config  Also write to config.yaml for team-wide defaults

Examples:
  bd dolt set database myproject
  bd dolt set host 192.168.1.100 --update-config
  bd dolt set data-dir /home/user/.beads-dolt/myproject
  export BEADS_DOLT_PASSWORD=... BEADS_DOLT_SERVER_TLS=1
  bd dolt test`,
}

var doltShowCmd = &cobra.Command{
	Use:           "show",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "Show current Dolt configuration with connection status",
	RunE: func(cmd *cobra.Command, args []string) error {
		return showDoltConfig(true)
	},
}

var doltSetCmd = &cobra.Command{
	Use:           "set <key> <value>",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "Set a Dolt configuration value",
	Long: `Set a Dolt configuration value in metadata.json.

Keys:
  database  Database name (default: issue prefix or "beads")
  host      Server host (default: 127.0.0.1)
  port      Server port (auto-detected; override with bd dolt set port <N>)
  user      MySQL user (default: root)
  data-dir  Custom dolt data directory (absolute path; default: .beads/dolt)

There is no 'password' or 'tls' key here on purpose — secrets and TLS must
not land in metadata.json. Use environment variables or the credentials file:

  BEADS_DOLT_PASSWORD     Server password (highest priority)
  BEADS_DOLT_SERVER_TLS   Enable TLS ("1" or "true")
  BEADS_CREDENTIALS_FILE  Optional override path for credentials

  Default credentials file: ~/.config/beads/credentials
  Format:
    [host:port]
    password = secret

  See: bd dolt --help and docs/architecture/dolt.md

Use --update-config to also write to config.yaml for team-wide defaults.

Examples:
  bd dolt set database myproject
  bd dolt set host 192.168.1.100
  bd dolt set port 3307 --update-config
  bd dolt set data-dir /home/user/.beads-dolt/myproject
  export BEADS_DOLT_PASSWORD=... BEADS_DOLT_SERVER_TLS=1`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadsDir := selectedDoltBeadsDir()
		if beadsDir == "" {
			return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
		}
		if _, err := loadDoltBackendConfig(beadsDir); err != nil {
			return HandleError("%v", err)
		}
		if !usesSQLServer() {
			return HandleError("'bd dolt set' is not supported in embedded mode (no Dolt server)")
		}
		key := args[0]
		value := args[1]
		updateConfig, _ := cmd.Flags().GetBool("update-config")
		return setDoltConfig(key, value, updateConfig)
	},
}

var doltTestCmd = &cobra.Command{
	Use:           "test",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "Test connection to Dolt server",
	Long: `Test the connection to the configured Dolt server.

This verifies that:
  1. The server is reachable at the configured host:port
  2. The connection can be established

Use this before switching to server mode to ensure the server is running.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		beadsDir := selectedDoltBeadsDir()
		if beadsDir == "" {
			return HandleErrorWithHint(activeWorkspaceNotFoundError(), diagHint())
		}
		if _, err := loadDoltBackendConfig(beadsDir); err != nil {
			return HandleError("%v", err)
		}
		if !usesSQLServer() {
			return HandleError("'bd dolt test' is not supported in embedded mode (no Dolt server)")
		}
		return testDoltConnection()
	},
}

func init() {
	doltSetCmd.Flags().Bool("update-config", false, "Also write to config.yaml for team-wide defaults")
	doltStopCmd.Flags().Bool("force", false, "Force stop (proxied recovery still requires a bd/dolt executable match)")
	doltPushCmd.Flags().Bool("force", false, "Force push (overwrite remote changes)")
	doltPushCmd.Flags().String("remote", "", "Push to a specific named remote instead of the default")
	doltPushCmd.Flags().BoolP("yes", "y", false, "Consent to adopting a Dolt remote derived from git origin when none is configured")
	doltPushCmd.Flags().Bool("no-adopt", false, "Never derive a Dolt remote from git origin (also BD_NO_REMOTE_ADOPT=1)")
	doltPullCmd.Flags().String("remote", "", "Pull from a specific named remote instead of the default")
	doltPullCmd.Flags().String("strategy", "", "Conflict resolution strategy for conflicts the auto-resolver declines: 'ours' or 'theirs' (embedded storage only, #4992)")
	doltCommitCmd.Flags().StringP("message", "m", "", "Commit message (default: auto-generated)")
	doltCleanDatabasesCmd.Flags().Bool("dry-run", false, "Show what would be dropped without dropping")
	doltCleanDatabasesCmd.Flags().Bool("purge-dropped", false, "After dropping, also run CALL DOLT_PURGE_DROPPED_DATABASES() — server-global and irreversible, see --help")
	doltRemoteAddCmd.Flags().Bool("allow-git-origin", false, "Allow adding a Dolt remote whose URL matches the git origin (proceed with a warning instead of aborting)")
	doltRemoteResetDataCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt (required in non-interactive use)")
	doltRemoteCmd.AddCommand(doltRemoteAddCmd)
	doltRemoteCmd.AddCommand(doltRemoteListCmd)
	doltRemoteCmd.AddCommand(doltRemoteRemoveCmd)
	doltRemoteCmd.AddCommand(doltRemoteResetDataCmd)
	doltCmd.AddCommand(doltShowCmd)
	doltCmd.AddCommand(doltSetCmd)
	doltCmd.AddCommand(doltTestCmd)
	doltCmd.AddCommand(doltCommitCmd)
	doltCmd.AddCommand(doltPushCmd)
	doltCmd.AddCommand(doltPullCmd)
	doltCmd.AddCommand(doltStartCmd)
	doltCmd.AddCommand(doltStopCmd)
	doltCmd.AddCommand(doltStatusCmd)
	doltCmd.AddCommand(doltKillallCmd)
	doltCmd.AddCommand(doltCleanDatabasesCmd)
	doltCmd.AddCommand(doltRemoteCmd)
	rootCmd.AddCommand(doltCmd)
}

//go:build cgo

package main

import (
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/spf13/cobra"
)

type federationSyncOptions struct {
	peer     string
	strategy string
}

type federationStatusOptions struct {
	peer string
}

type federationAddPeerOptions struct {
	user     string
	password string
	sov      string
}

func federationSyncOptionsFromCommand(cmd *cobra.Command) federationSyncOptions {
	flags := cmd.Flags()
	peer, _ := flags.GetString("peer")
	strategy, _ := flags.GetString("strategy")
	return federationSyncOptions{peer: peer, strategy: strategy}
}

func federationStatusOptionsFromCommand(cmd *cobra.Command) federationStatusOptions {
	peer, _ := cmd.Flags().GetString("peer")
	return federationStatusOptions{peer: peer}
}

func federationAddPeerOptionsFromCommand(cmd *cobra.Command) federationAddPeerOptions {
	flags := cmd.Flags()
	user, _ := flags.GetString("user")
	password, _ := flags.GetString("password")
	sov, _ := flags.GetString("sovereignty")
	return federationAddPeerOptions{user: user, password: password, sov: sov}
}

var federationCmd = &cobra.Command{
	Use:     "federation",
	GroupID: "sync",
	Short:   "Manage peer-to-peer federation with other workspaces",
	Long: `Manage peer-to-peer federation between Dolt-backed beads databases.

Federation enables synchronized issue tracking across multiple workspaces,
each maintaining their own Dolt database while sharing updates via remotes.

Requires the Dolt storage backend.`,
}

var federationSyncCmd = &cobra.Command{
	Use:   "sync [--peer name]",
	Short: "Synchronize with a peer town",
	Long: `Pull from and push to peer towns.

Without --peer, syncs with all configured peers.
With --peer, syncs only with the specified peer.

Handles merge conflicts using the configured strategy:
  --strategy ours    Keep local changes on conflict
  --strategy theirs  Accept remote changes on conflict

If no strategy is specified and conflicts occur, the sync will pause
and report which tables have conflicts for manual resolution.

Examples:
  bd federation sync                      # Sync with all peers
  bd federation sync --peer town-beta     # Sync with specific peer
  bd federation sync --strategy theirs    # Auto-resolve using remote values`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runFederationSync,
}

var federationStatusCmd = &cobra.Command{
	Use:   "status [--peer name]",
	Short: "Show federation sync status",
	Long: `Show synchronization status with peer towns.

Displays:
  - Configured peers and their URLs
  - Commits ahead/behind each peer
  - Whether there are unresolved conflicts

Examples:
  bd federation status                    # Status for all peers
  bd federation status --peer town-beta   # Status for specific peer`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runFederationStatus,
}

var federationAddPeerCmd = &cobra.Command{
	Use:   "add-peer <name> <url>",
	Short: "Add a federation peer with optional SQL credentials",
	Long: `Add a new federation peer remote with optional SQL user authentication.

The URL can be:
  - dolthub://org/repo      DoltHub hosted repository
  - host:port/database      Direct dolt sql-server connection
  - file:///path/to/repo    Local file path (for testing)

Credentials are encrypted and stored locally. They are used automatically
when syncing with the peer. If --user is provided without --password,
you will be prompted for the password interactively.

Examples:
  bd federation add-peer town-beta dolthub://acme/town-beta-beads
  bd federation add-peer town-gamma 192.168.1.100:3306/beads --user sync-bot
  bd federation add-peer partner https://partner.example.com/beads --user admin --password secret`,
	Args:          cobra.ExactArgs(2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runFederationAddPeer,
}

var federationRemovePeerCmd = &cobra.Command{
	Use:           "remove-peer <name>",
	Short:         "Remove a federation peer",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runFederationRemovePeer,
}

var federationListPeersCmd = &cobra.Command{
	Use:           "list-peers",
	Short:         "List configured federation peers",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runFederationListPeers,
}

func init() {
	// Add subcommands
	federationCmd.AddCommand(federationSyncCmd)
	federationCmd.AddCommand(federationStatusCmd)
	federationCmd.AddCommand(federationAddPeerCmd)
	federationCmd.AddCommand(federationRemovePeerCmd)
	federationCmd.AddCommand(federationListPeersCmd)

	// Flags for sync
	federationSyncCmd.Flags().String("peer", "", "Specific peer to sync with")
	federationSyncCmd.Flags().String("strategy", "", "Conflict resolution strategy (ours|theirs)")

	// Flags for status
	federationStatusCmd.Flags().String("peer", "", "Specific peer to check")

	// Flags for add-peer (SQL user authentication)
	federationAddPeerCmd.Flags().StringP("user", "u", "", "SQL username for authentication")
	federationAddPeerCmd.Flags().StringP("password", "p", "", "SQL password (prompted if --user set without --password)")
	federationAddPeerCmd.Flags().String("sovereignty", "", "Sovereignty tier (T1, T2, T3, T4)")

	rootCmd.AddCommand(federationCmd)
}

func getFederatedStore() (storage.DoltStorage, error) {
	if getStore() == nil {
		return nil, fmt.Errorf("no store available")
	}
	return getStore(), nil
}

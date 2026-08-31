//go:build cgo

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func runFederationSync(cmd *cobra.Command, _ []string) error {
	opts := federationSyncOptionsFromCommand(cmd)
	if usesProxiedServer() {
		return HandleErrorRespectJSON("federation sync is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("federation-sync")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := rootCtx

	ds, err := getFederatedStore()
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if err := validateFederationStrategy(opts.strategy); err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	peers, err := federationPeerNames(ctx, ds, opts.peer)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if len(peers) == 0 {
		return HandleErrorRespectJSON("no federation peers configured (use 'bd federation add-peer' to add peers)")
	}

	results := syncFederationPeers(ctx, ds, peers, opts.strategy)

	if jsonOutput {
		return outputJSON(map[string]interface{}{
			"peers":   peers,
			"results": results,
		})
	}
	return nil
}

func validateFederationStrategy(strategy string) error {
	if strategy != "" && strategy != "ours" && strategy != "theirs" {
		return fmt.Errorf("invalid strategy %q: must be 'ours' or 'theirs'", strategy)
	}
	return nil
}

func federationPeerNames(ctx context.Context, ds storage.DoltStorage, selected string) ([]string, error) {
	if selected != "" {
		return []string{selected}, nil
	}
	remotes, err := ds.ListRemotes(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list peers: %w", err)
	}
	peers := make([]string, 0, len(remotes))
	for _, remote := range remotes {
		if remote.Name != "origin" {
			peers = append(peers, remote.Name)
		}
	}
	return peers, nil
}

func syncFederationPeers(ctx context.Context, ds storage.DoltStorage, peers []string, strategy string) []*storage.SyncResult {
	results := make([]*storage.SyncResult, 0, len(peers))
	for _, peer := range peers {
		if !jsonOutput {
			fmt.Printf("%s Syncing with %s...\n", ui.RenderAccent("🔄"), peer)
		}

		result, err := ds.Sync(ctx, peer, strategy)
		results = append(results, result)
		if err != nil {
			if !jsonOutput {
				fmt.Printf("  %s %v\n", ui.RenderFail("✗"), err)
			}
			continue
		}
		if !jsonOutput {
			printFederationSyncResult(result, strategy)
		}
	}
	return results
}

func printFederationSyncResult(result *storage.SyncResult, strategy string) {
	if result.Fetched {
		fmt.Printf("  %s Fetched\n", ui.RenderPass("✓"))
	}
	printFederationMergeResult(result)
	printFederationConflicts(result, strategy)
	if result.Pushed {
		fmt.Printf("  %s Pushed\n", ui.RenderPass("✓"))
	} else if result.PushError != nil {
		fmt.Printf("  %s Push skipped: %v\n", ui.RenderMuted("○"), result.PushError)
	}
}

func printFederationMergeResult(result *storage.SyncResult) {
	if !result.Merged {
		return
	}
	fmt.Printf("  %s Merged", ui.RenderPass("✓"))
	if result.PulledCommits > 0 {
		fmt.Printf(" (%d commits)", result.PulledCommits)
	}
	fmt.Println()
}

func printFederationConflicts(result *storage.SyncResult, strategy string) {
	if len(result.Conflicts) == 0 {
		return
	}
	if result.ConflictsResolved {
		fmt.Printf("  %s Resolved %d conflicts using %s strategy\n",
			ui.RenderPass("✓"), len(result.Conflicts), strategy)
		return
	}
	fmt.Printf("  %s %d conflicts need resolution\n",
		ui.RenderWarn("⚠"), len(result.Conflicts))
	for _, conflict := range result.Conflicts {
		fmt.Printf("    - %s\n", conflict.Field)
	}
}

func runFederationStatus(cmd *cobra.Command, _ []string) error {
	opts := federationStatusOptionsFromCommand(cmd)
	if usesProxiedServer() {
		return HandleErrorRespectJSON("federation status is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("federation-status")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := rootCtx

	ds, err := getFederatedStore()
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	allRemotes, err := ds.ListRemotes(ctx)
	if err != nil {
		return HandleErrorRespectJSON("failed to list remotes: %v", err)
	}
	remoteURLs := federationRemoteURLs(allRemotes)
	peers := federationStatusPeers(allRemotes, opts.peer)

	if len(peers) == 0 {
		if jsonOutput {
			return outputJSON(map[string]interface{}{
				"peers":          []string{},
				"pendingChanges": 0,
			})
		}
		fmt.Println("No federation peers configured.")
		return nil
	}

	pendingChanges := federationPendingChanges(ctx, ds)
	peerStatuses := loadFederationPeerStatuses(ctx, ds, peers, remoteURLs)

	if jsonOutput {
		return outputJSON(map[string]interface{}{
			"peers":          peerStatuses,
			"pendingChanges": pendingChanges,
		})
	}

	printFederationStatus(pendingChanges, peerStatuses)
	return nil
}

type federationPeerStatus struct {
	Status     *storage.SyncStatus
	URL        string
	Reachable  bool
	ReachError string
}

func federationRemoteURLs(remotes []storage.RemoteInfo) map[string]string {
	remoteURLs := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		remoteURLs[remote.Name] = remote.URL
	}
	return remoteURLs
}

func federationStatusPeers(remotes []storage.RemoteInfo, selected string) []string {
	if selected != "" {
		return []string{selected}
	}
	peers := make([]string, 0, len(remotes))
	for _, remote := range remotes {
		peers = append(peers, remote.Name)
	}
	return peers
}

func federationPendingChanges(ctx context.Context, ds storage.DoltStorage) int {
	doltStatus, _ := ds.Status(ctx)
	if doltStatus == nil {
		return 0
	}
	return len(doltStatus.Staged) + len(doltStatus.Unstaged)
}

func loadFederationPeerStatuses(ctx context.Context, ds storage.DoltStorage, peers []string, remoteURLs map[string]string) []federationPeerStatus {
	peerStatuses := make([]federationPeerStatus, 0, len(peers))
	for _, peer := range peers {
		status, _ := ds.SyncStatus(ctx, peer)
		ps := federationPeerStatus{Status: status, URL: remoteURLs[peer]}
		fetchErr := ds.Fetch(ctx, peer)
		if fetchErr == nil {
			ps.Reachable = true
			ps.Status, _ = ds.SyncStatus(ctx, peer)
		} else {
			ps.ReachError = fetchErr.Error()
		}
		peerStatuses = append(peerStatuses, ps)
	}
	return peerStatuses
}

func printFederationStatus(pendingChanges int, peerStatuses []federationPeerStatus) {
	fmt.Printf("\n%s Federation Status:\n\n", ui.RenderAccent("🌐"))
	if pendingChanges > 0 {
		fmt.Printf("  %s %d pending local changes\n\n", ui.RenderWarn("⚠"), pendingChanges)
	}
	for _, ps := range peerStatuses {
		printFederationPeerStatus(ps)
	}
}

func printFederationPeerStatus(ps federationPeerStatus) {
	status := ps.Status
	fmt.Printf("  %s  %s\n", ui.RenderAccent(status.Peer), ui.RenderMuted(ps.URL))
	if ps.Reachable {
		fmt.Printf("    %s Reachable\n", ui.RenderPass("✓"))
	} else {
		fmt.Printf("    %s Unreachable: %s\n", ui.RenderFail("✗"), ps.ReachError)
	}
	if status.LocalAhead >= 0 {
		fmt.Printf("    Ahead:  %d commits\n", status.LocalAhead)
		fmt.Printf("    Behind: %d commits\n", status.LocalBehind)
	} else {
		fmt.Printf("    Sync:   %s\n", ui.RenderMuted("not fetched yet"))
	}
	if !status.LastSync.IsZero() {
		fmt.Printf("    Last sync: %s\n", status.LastSync.Format("2006-01-02 15:04:05"))
	}
	if status.HasConflicts {
		fmt.Printf("    %s Unresolved conflicts\n", ui.RenderWarn("⚠"))
	}
	fmt.Println()
}

func runFederationAddPeer(cmd *cobra.Command, args []string) error {
	opts := federationAddPeerOptionsFromCommand(cmd)
	if usesProxiedServer() {
		return HandleErrorRespectJSON("federation add-peer is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("federation-add-peer")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := rootCtx

	name := args[0]
	url := args[1]

	password, err := federationPassword(opts.user, opts.password)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	sov, err := normalizeFederationSovereignty(opts.sov)
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}

	if err := addFederationPeer(ctx, name, url, opts.user, password, sov); err != nil {
		return HandleErrorRespectJSON("failed to add peer: %v", err)
	}

	if jsonOutput {
		return outputJSON(map[string]interface{}{
			"added":       name,
			"url":         url,
			"has_auth":    opts.user != "",
			"sovereignty": sov,
		})
	}

	printFederationPeerAdded(name, url, opts.user, sov)
	return nil
}

func federationPassword(user, password string) (string, error) {
	if user == "" || password != "" {
		return password, nil
	}
	fmt.Fprint(os.Stderr, "Password: ")
	pwBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("failed to read password: %w", err)
	}
	return string(pwBytes), nil
}

func normalizeFederationSovereignty(sov string) (string, error) {
	if sov == "" {
		return "", nil
	}
	normalized := strings.ToUpper(sov)
	if normalized != "T1" && normalized != "T2" && normalized != "T3" && normalized != "T4" {
		return "", fmt.Errorf("invalid sovereignty tier: %s (must be T1, T2, T3, or T4)", sov)
	}
	return normalized, nil
}

func addFederationPeer(ctx context.Context, name, url, user, password, sov string) error {
	if user == "" {
		return getStore().AddRemote(ctx, name, url)
	}
	return getStore().AddFederationPeer(ctx, &storage.FederationPeer{
		Name:        name,
		RemoteURL:   url,
		Username:    user,
		Password:    password,
		Sovereignty: sov,
	})
}

func printFederationPeerAdded(name, url, user, sov string) {
	fmt.Printf("Added peer %s: %s\n", ui.RenderAccent(name), url)
	if user != "" {
		fmt.Printf("  User: %s (credentials stored)\n", user)
	}
	if sov != "" {
		fmt.Printf("  Sovereignty: %s\n", sov)
	}
}

func runFederationRemovePeer(_ *cobra.Command, args []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("federation remove-peer is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("federation-remove-peer")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := rootCtx

	name := args[0]

	if err := getStore().RemoveRemote(ctx, name); err != nil {
		return HandleErrorRespectJSON("failed to remove peer: %v", err)
	}

	if jsonOutput {
		return outputJSON(map[string]interface{}{
			"removed": name,
		})
	}

	fmt.Printf("Removed peer: %s\n", name)
	return nil
}

func runFederationListPeers(_ *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("federation list-peers is not supported in proxied-server mode")
	}
	evt := metrics.NewCommandEvent("federation-list-peers")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	ctx := rootCtx

	remotes, err := getStore().ListRemotes(ctx)
	if err != nil {
		return HandleErrorRespectJSON("failed to list peers: %v", err)
	}

	if jsonOutput {
		return outputJSON(formatFederationPeerListJSON(remotes))
	}

	if len(remotes) == 0 {
		fmt.Println("No federation peers configured.")
		return nil
	}

	fmt.Printf("\n%s Federation Peers:\n\n", ui.RenderAccent("🌐"))
	for _, r := range remotes {
		fmt.Printf("  %s  %s\n", ui.RenderAccent(r.Name), ui.RenderMuted(r.URL))
	}
	fmt.Println()
	return nil
}

type federationPeerListJSON struct {
	Name string `json:"Name"`
	URL  string `json:"URL"`
}

func formatFederationPeerListJSON(remotes []storage.RemoteInfo) []federationPeerListJSON {
	out := make([]federationPeerListJSON, 0, len(remotes))
	for _, r := range remotes {
		out = append(out, federationPeerListJSON{
			Name: r.Name,
			URL:  r.URL,
		})
	}
	return out
}

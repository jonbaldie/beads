package doctor

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dolt"
)

// CheckLegacyCLIRemotes warns when legacy filesystem CLI remotes are not
// represented in SQL, because bd now treats SQL remotes as the source of truth.
func CheckLegacyCLIRemotes(path string) DoctorCheck {
	backend, beadsDir := getBackendAndBeadsDir(path)
	if backend != configfile.BackendDolt {
		return DoctorCheck{
			Name:     "Dolt Remote Migration",
			Status:   StatusOK,
			Message:  "N/A (non-Dolt backend)",
			Category: CategoryFederation,
		}
	}

	doltPath := getDatabasePath(beadsDir)
	if _, err := os.Stat(doltPath); os.IsNotExist(err) {
		return DoctorCheck{
			Name:     "Dolt Remote Migration",
			Status:   StatusOK,
			Message:  "N/A (no dolt database)",
			Category: CategoryFederation,
		}
	}

	ctx := context.Background()
	store, err := dolt.New(ctx, doltServerConfig(beadsDir, doltPath))
	if err != nil {
		return DoctorCheck{
			Name:     "Dolt Remote Migration",
			Status:   StatusOK,
			Message:  "Skipped (database unavailable)",
			Detail:   err.Error(),
			Category: CategoryFederation,
		}
	}
	defer func() { _ = store.Close() }()

	sqlByName, err := listSQLRemoteURLs(ctx, store)
	if err != nil {
		return DoctorCheck{
			Name:     "Dolt Remote Migration",
			Status:   StatusOK,
			Message:  "Skipped (SQL remotes unavailable)",
			Detail:   err.Error(),
			Category: CategoryFederation,
		}
	}
	inspected, missing, inspectErrors := inspectLegacyCLIRemotes(store, sqlByName)

	if len(inspected) == 0 && len(inspectErrors) > 0 {
		return DoctorCheck{
			Name:     "Dolt Remote Migration",
			Status:   StatusOK,
			Message:  "No legacy CLI remote check available",
			Detail:   strings.Join(inspectErrors, "\n"),
			Category: CategoryFederation,
		}
	}

	if len(missing) == 0 {
		return DoctorCheck{
			Name:     "Dolt Remote Migration",
			Status:   StatusOK,
			Message:  "No legacy CLI-only remotes detected",
			Category: CategoryFederation,
		}
	}

	return DoctorCheck{
		Name:     "Dolt Remote Migration",
		Status:   StatusWarning,
		Message:  fmt.Sprintf("%d legacy CLI remote(s) not visible through SQL", len(missing)),
		Detail:   fmt.Sprintf("Inspected CLI directories:\n%s\nRemotes: %s\nbd dolt remote list, push, and pull use SQL remotes as the source of truth.", strings.Join(inspected, "\n"), strings.Join(missing, ", ")),
		Fix:      "Re-register each remote with 'bd dolt remote add <name> <url>' so it is stored in SQL.",
		Category: CategoryFederation,
	}
}

// CheckFederationRemotesAPI checks if the remotesapi port is accessible for federation.
// This is the port used for peer-to-peer sync operations.
func CheckFederationRemotesAPI(path string) DoctorCheck {
	backend, beadsDir := getBackendAndBeadsDir(path)

	if backend != configfile.BackendDolt {
		return federationNotApplicable("Federation remotesapi", "N/A (non-Dolt backend)")
	}

	doltPath := getDatabasePath(beadsDir)
	if _, err := os.Stat(doltPath); os.IsNotExist(err) {
		return federationNotApplicable("Federation remotesapi", "N/A (no dolt database)")
	}

	// Check if dolt server is running using doltserver.IsRunning which
	// correctly resolves PID file paths (in beadsDir, not doltPath)
	// and handles orchestrator daemon PID files.
	serverState, _ := doltserver.IsRunning(beadsDir)
	if serverState == nil || !serverState.Running {
		return checkFederationRemotesAPINotRunning(context.Background(), beadsDir, doltPath)
	}

	// Server is running - check if any federation peers are configured before
	// probing the remotesapi port. Without peers, remotesapi is irrelevant.
	{
		ctx := context.Background()
		store, err := dolt.New(ctx, doltServerConfig(beadsDir, doltPath))
		if err == nil {
			hasPeers := hasFederationPeers(ctx, store)
			_ = store.Close()
			if !hasPeers {
				return federationNotApplicable("Federation remotesapi", "N/A (no federation peers configured)")
			}
		}
	}

	// Server is running and peers are configured - check if remotesapi port is accessible.
	// Read port from config instead of hardcoding 8080.
	return probeFederationRemotesAPI(serverState, federationRemotesAPIPort(beadsDir))
}

func inspectFederationPeerStatuses(ctx context.Context, store *dolt.DoltStore, remotes []storage.RemoteInfo) ([]string, []string, []string) {
	var reachable, unreachable []string
	var statusDetails []string
	for _, remote := range remotes {
		if remote.Name == "origin" {
			continue
		}

		status, err := store.SyncStatus(ctx, remote.Name)
		if err != nil {
			unreachable = append(unreachable, remote.Name)
			statusDetails = append(statusDetails, fmt.Sprintf("%s: %v", remote.Name, err))
			continue
		}
		reachable = append(reachable, remote.Name)
		if status.LocalAhead > 0 || status.LocalBehind > 0 {
			statusDetails = append(statusDetails, fmt.Sprintf("%s: %d ahead, %d behind",
				remote.Name, status.LocalAhead, status.LocalBehind))
		}
	}
	return reachable, unreachable, statusDetails
}

func federationPeerConnectivityResult(reachable, unreachable, statusDetails []string) DoctorCheck {
	if len(reachable) == 0 && len(unreachable) == 0 {
		return DoctorCheck{
			Name:     "Peer Connectivity",
			Status:   StatusOK,
			Message:  "No federation peers configured (only origin remote)",
			Category: CategoryFederation,
		}
	}

	if len(unreachable) > 0 {
		return DoctorCheck{
			Name:     "Peer Connectivity",
			Status:   StatusWarning,
			Message:  fmt.Sprintf("%d/%d peers unreachable", len(unreachable), len(reachable)+len(unreachable)),
			Detail:   strings.Join(statusDetails, "\n"),
			Fix:      "Check peer URLs and network connectivity",
			Category: CategoryFederation,
		}
	}

	detail := ""
	if len(statusDetails) > 0 {
		detail = strings.Join(statusDetails, "\n")
	}
	return DoctorCheck{
		Name:     "Peer Connectivity",
		Status:   StatusOK,
		Message:  fmt.Sprintf("%d peers reachable", len(reachable)),
		Detail:   detail,
		Category: CategoryFederation,
	}
}

// CheckFederationPeerConnectivity checks if configured peer remotes are reachable.
func CheckFederationPeerConnectivity(path string) DoctorCheck {
	backend, beadsDir := getBackendAndBeadsDir(path)

	if backend != configfile.BackendDolt {
		return federationNotApplicable("Peer Connectivity", "N/A (non-Dolt backend)")
	}

	doltPath := getDatabasePath(beadsDir)
	if _, err := os.Stat(doltPath); os.IsNotExist(err) {
		return federationNotApplicable("Peer Connectivity", "N/A (no dolt database)")
	}

	ctx := context.Background()
	store, err := dolt.New(ctx, doltServerConfig(beadsDir, doltPath))
	if err != nil {
		return DoctorCheck{
			Name:     "Peer Connectivity",
			Status:   StatusWarning,
			Message:  "Unable to open database",
			Detail:   err.Error(),
			Category: CategoryFederation,
		}
	}
	defer func() { _ = store.Close() }()

	remotes, err := store.ListRemotes(ctx)
	if err != nil {
		return DoctorCheck{
			Name:     "Peer Connectivity",
			Status:   StatusWarning,
			Message:  "Unable to list remotes",
			Detail:   err.Error(),
			Category: CategoryFederation,
		}
	}

	if len(remotes) == 0 {
		return DoctorCheck{
			Name:     "Peer Connectivity",
			Status:   StatusOK,
			Message:  "No peers configured",
			Category: CategoryFederation,
		}
	}

	// SyncStatus reads the cached peer state and does not require a network call.
	reachable, unreachable, statusDetails := inspectFederationPeerStatuses(ctx, store, remotes)
	return federationPeerConnectivityResult(reachable, unreachable, statusDetails)
}

func collectFederationStaleness(ctx context.Context, store *dolt.DoltStore, remotes []storage.RemoteInfo) ([]string, int) {
	var staleWarnings []string
	totalBehind := 0
	for _, remote := range remotes {
		if remote.Name == "origin" {
			continue
		}

		status, err := store.SyncStatus(ctx, remote.Name)
		if err != nil {
			continue
		}
		if status.LocalBehind == 0 {
			continue
		}

		totalBehind += status.LocalBehind
		staleWarnings = append(staleWarnings, fmt.Sprintf("%s: %d commits behind",
			remote.Name, status.LocalBehind))
	}
	return staleWarnings, totalBehind
}

// CheckFederationSyncStaleness checks for stale sync status with peers.
func CheckFederationSyncStaleness(path string) DoctorCheck {
	backend, beadsDir := getBackendAndBeadsDir(path)

	if backend != configfile.BackendDolt {
		return federationNotApplicable("Sync Staleness", "N/A (non-Dolt backend)")
	}

	doltPath := getDatabasePath(beadsDir)
	if _, err := os.Stat(doltPath); os.IsNotExist(err) {
		return federationNotApplicable("Sync Staleness", "N/A (no dolt database)")
	}

	ctx := context.Background()
	store, err := dolt.New(ctx, doltServerConfig(beadsDir, doltPath))
	if err != nil {
		return DoctorCheck{
			Name:     "Sync Staleness",
			Status:   StatusWarning,
			Message:  "Unable to open database",
			Detail:   err.Error(),
			Category: CategoryFederation,
		}
	}
	defer func() { _ = store.Close() }()

	remotes, err := store.ListRemotes(ctx)
	if err != nil || len(remotes) == 0 {
		return DoctorCheck{
			Name:     "Sync Staleness",
			Status:   StatusOK,
			Message:  "No peers configured",
			Category: CategoryFederation,
		}
	}

	staleWarnings, totalBehind := collectFederationStaleness(ctx, store, remotes)

	if len(staleWarnings) == 0 {
		return DoctorCheck{
			Name:     "Sync Staleness",
			Status:   StatusOK,
			Message:  "Sync is up to date",
			Category: CategoryFederation,
		}
	}

	return DoctorCheck{
		Name:     "Sync Staleness",
		Status:   StatusWarning,
		Message:  fmt.Sprintf("%d total commits behind peers", totalBehind),
		Detail:   strings.Join(staleWarnings, "\n"),
		Fix:      "Run 'bd federation sync' to synchronize with peers",
		Category: CategoryFederation,
	}
}

func isMissingConflictsTable(err error) bool {
	return strings.Contains(err.Error(), "no such table") || strings.Contains(err.Error(), "doesn't exist")
}

func formatFederationConflictDetails(conflicts []storage.Conflict) ([]string, int) {
	issueConflicts := make(map[string][]string)
	for _, conflict := range conflicts {
		issueConflicts[conflict.IssueID] = append(issueConflicts[conflict.IssueID], conflict.Field)
	}

	details := make([]string, 0, len(issueConflicts))
	for issueID, fields := range issueConflicts {
		details = append(details, fmt.Sprintf("%s: %s", issueID, strings.Join(fields, ", ")))
	}
	return details, len(issueConflicts)
}

// CheckFederationConflicts checks for unresolved merge conflicts.
func CheckFederationConflicts(path string) DoctorCheck {
	backend, beadsDir := getBackendAndBeadsDir(path)

	if backend != configfile.BackendDolt {
		return federationNotApplicable("Federation Conflicts", "N/A (non-Dolt backend)")
	}

	doltPath := getDatabasePath(beadsDir)
	if _, err := os.Stat(doltPath); os.IsNotExist(err) {
		return federationNotApplicable("Federation Conflicts", "N/A (no dolt database)")
	}

	ctx := context.Background()
	store, err := dolt.New(ctx, doltServerConfig(beadsDir, doltPath))
	if err != nil {
		return DoctorCheck{
			Name:     "Federation Conflicts",
			Status:   StatusWarning,
			Message:  "Unable to open database",
			Detail:   err.Error(),
			Category: CategoryFederation,
		}
	}
	defer func() { _ = store.Close() }()

	conflicts, err := store.GetConflicts(ctx)
	if err != nil {
		// Some errors are expected (e.g., no conflicts table)
		if isMissingConflictsTable(err) {
			return DoctorCheck{
				Name:     "Federation Conflicts",
				Status:   StatusOK,
				Message:  "No conflicts",
				Category: CategoryFederation,
			}
		}
		return DoctorCheck{
			Name:     "Federation Conflicts",
			Status:   StatusWarning,
			Message:  "Unable to check conflicts",
			Detail:   err.Error(),
			Category: CategoryFederation,
		}
	}

	if len(conflicts) == 0 {
		return DoctorCheck{
			Name:     "Federation Conflicts",
			Status:   StatusOK,
			Message:  "No conflicts",
			Category: CategoryFederation,
		}
	}

	details, issueCount := formatFederationConflictDetails(conflicts)

	return DoctorCheck{
		Name:     "Federation Conflicts",
		Status:   StatusError,
		Message:  fmt.Sprintf("%d unresolved conflicts in %d issues", len(conflicts), issueCount),
		Detail:   strings.Join(details, "\n"),
		Fix:      "Run 'bd federation sync --strategy ours|theirs' to resolve conflicts",
		Category: CategoryFederation,
	}
}

func doltServerReachable(beadsDir string) bool {
	cfg, _ := configfile.Load(beadsDir)
	if cfg == nil {
		return false
	}
	host := cfg.GetDoltServerHost()
	port := doltserver.DefaultConfig(beadsDir).Port
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	_, err := doltserver.ProbeSQLServer("tcp", addr, 2*time.Second)
	return err == nil
}

// CheckDoltServerModeMismatch checks for mismatch between Dolt init and server mode.
// This detects cases where:
// - Server mode is expected but no server is running
// - Embedded mode is being used when server mode should be used (federation with peers)
func CheckDoltServerModeMismatch(path string) DoctorCheck {
	backend, beadsDir := getBackendAndBeadsDir(path)

	if backend != configfile.BackendDolt {
		return federationNotApplicable("Dolt Mode", "N/A (non-Dolt backend)")
	}

	doltPath := getDatabasePath(beadsDir)
	if _, err := os.Stat(doltPath); os.IsNotExist(err) {
		return federationNotApplicable("Dolt Mode", "N/A (no dolt database)")
	}

	serverReachable := doltServerReachable(beadsDir)

	// Open storage to check for remotes
	ctx := context.Background()
	store, err := dolt.New(ctx, doltServerConfig(beadsDir, doltPath))
	if err != nil {
		return DoctorCheck{
			Name:     "Dolt Mode",
			Status:   StatusWarning,
			Message:  "Unable to open database",
			Detail:   err.Error(),
			Category: CategoryFederation,
		}
	}
	defer func() { _ = store.Close() }()

	// Check for configured remotes
	remotes, err := store.ListRemotes(ctx)
	if err != nil {
		return DoctorCheck{
			Name:     "Dolt Mode",
			Status:   StatusWarning,
			Message:  "Unable to list remotes",
			Detail:   err.Error(),
			Category: CategoryFederation,
		}
	}

	peerCount := countFederationPeers(remotes)

	// Determine expected vs actual mode
	if peerCount > 0 && !serverReachable {
		return DoctorCheck{
			Name:     "Dolt Mode",
			Status:   StatusWarning,
			Message:  fmt.Sprintf("Server not reachable with %d peers configured", peerCount),
			Detail:   "Federation with peers requires a running dolt sql-server",
			Fix:      "Start dolt sql-server manually",
			Category: CategoryFederation,
		}
	}

	if serverReachable {
		return DoctorCheck{
			Name:     "Dolt Mode",
			Status:   StatusOK,
			Message:  "Server mode (connected)",
			Detail:   fmt.Sprintf("%d peers configured", peerCount),
			Category: CategoryFederation,
		}
	}

	return DoctorCheck{
		Name:     "Dolt Mode",
		Status:   StatusOK,
		Message:  "Embedded mode",
		Detail:   "No federation peers configured",
		Category: CategoryFederation,
	}
}

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/ui"
	"golang.org/x/term"
)

func isNonInteractiveInit(flagValue bool) bool {
	if flagValue {
		return true
	}
	if v := os.Getenv("BD_NON_INTERACTIVE"); v != "" {
		if v == "1" || v == "true" {
			return true
		}
		// Explicit BD_NON_INTERACTIVE=0/false forces interactive mode,
		// overriding CI and terminal detection.
		return false
	}
	if v := os.Getenv("CI"); v == "true" || v == "1" {
		return true
	}
	return !term.IsTerminal(int(os.Stdin.Fd()))
}

// shouldPromptForRole returns true if we should prompt the user for their role.
// Skips prompt in non-interactive contexts (CI, scripts, piped input).
func shouldPromptForRole() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// getBeadsRole reads the beads.role git config value.
// Returns the role and true if configured, or empty string and false if not set.
func getBeadsRole() (string, bool) {
	cmd := exec.Command("git", "config", "--get", "beads.role")
	output, err := cmd.Output()
	if err != nil {
		return "", false
	}
	role := strings.TrimSpace(string(output))
	if role == "" {
		return "", false
	}
	return role, true
}

// setBeadsRole writes the beads.role git config value.
func setBeadsRole(role string) error {
	cmd := exec.Command("git", "config", "beads.role", role)
	return cmd.Run()
}

// promptContributorMode prompts the user to determine if they are a contributor.
// Returns true if the user indicates they are a contributor, false otherwise.
//
// Behavior:
// - If beads.role is already set: shows current role, offers to change
// - If not set: prompts "Contributing to someone else's repo? [y/N]"
// - Sets git config beads.role based on answer
func promptContributorMode() (isContributor bool, err error) {
	ctx := getRootContext()
	reader := bufio.NewReader(os.Stdin)

	// Check if role is already configured
	existingRole, hasRole := getBeadsRole()
	if hasRole {
		fmt.Printf("\n%s Already configured as: %s\n", ui.RenderAccent("▶"), ui.RenderBold(existingRole))
		fmt.Print("Change role? [y/N]: ")

		response, err := readLineWithContext(ctx, reader, os.Stdin)
		if err != nil {
			return false, fmt.Errorf("failed to read input: %w", err)
		}
		response = strings.TrimSpace(strings.ToLower(response))

		if response != "y" && response != "yes" {
			// Keep existing role
			return existingRole == "contributor", nil
		}
		// Fall through to re-prompt
		fmt.Println()
	}

	// Prompt for role
	fmt.Print("Contributing to someone else's repo? [y/N]: ")

	response, err := readLineWithContext(ctx, reader, os.Stdin)
	if err != nil {
		return false, fmt.Errorf("failed to read input: %w", err)
	}
	response = strings.TrimSpace(strings.ToLower(response))

	isContributor = response == "y" || response == "yes"

	// Set the role in git config
	role := "maintainer"
	if isContributor {
		role = "contributor"
	}

	if err := setBeadsRole(role); err != nil {
		return isContributor, fmt.Errorf("failed to set beads.role config: %w", err)
	}

	return isContributor, nil
}

// promptAutoExport asks the user whether to enable optional auto-export.
// Returns true to enable it, false to leave it disabled.
func promptAutoExport() (bool, error) {
	fmt.Printf("\n%s Auto-export can keep .beads/issues.jsonl up to date after write commands.\n", ui.RenderAccent("▶"))
	fmt.Println("  This optional JSONL export is useful for viewers (bv), interchange, and issue-level migration.")
	fmt.Println("  Dolt remotes/backups handle cross-machine sync and backup.")
	fmt.Print("\nEnable auto-export? [y/N]: ")

	reader := bufio.NewReader(os.Stdin)
	response, err := readLineWithContext(getRootContext(), reader, os.Stdin)
	if err != nil {
		if isCanceled(err) {
			return true, err
		}
		response = ""
	}
	response = strings.TrimSpace(strings.ToLower(response))

	// Default to no. Users and integrations can enable it explicitly.
	return response == "y" || response == "yes", nil
}

func shouldWireInitRemote(syncURL string, syncFromRemote, syncURLFromConfig, syncURLFromGitOrigin bool) bool {
	// A git origin is a valid Dolt remote even before refs/dolt/data exists:
	// the first `bd dolt push` creates the Dolt ref on that git remote. We
	// still keep explicit empty --remote as an opt-out because that leaves
	// syncURL empty and syncURLFromGitOrigin false.
	return syncURL != "" && (syncFromRemote || syncURLFromConfig || syncURLFromGitOrigin)
}

func shouldConfigureInitDoltRemote(syncURL string, syncFromRemote, syncURLFromConfig, syncURLFromGitOrigin, localOnly bool) bool {
	return !localOnly && shouldWireInitRemote(syncURL, syncFromRemote, syncURLFromConfig, syncURLFromGitOrigin)
}

// shouldWriteInitDoltRemote reports whether init should actually configure (write)
// the Dolt "origin" remote on the store. It layers gateway suppression on top of
// shouldConfigureInitDoltRemote: a gateway is a passive client of a server-owned
// database, so AddRemote's CALL DOLT_REMOTE('add', ...) would mutate shared remote
// state on a writable credential (or merely warn per-init on a read-only one) —
// exactly the server-owned write the other gateway gates (shouldInitSharedGlobalDB,
// shouldWriteInitStateToDB, shouldWriteProjectIDLocally) already suppress. The
// rendered agent-instruction HasRemote flag intentionally keeps using the raw
// shouldConfigureInitDoltRemote decision, since the git remote still exists for
// documentation even when gateway sync does not use dolt push.
func shouldWriteInitDoltRemote(gateway bool, syncURL string, syncFromRemote, syncURLFromConfig, syncURLFromGitOrigin, localOnly bool) bool {
	return !gateway && shouldConfigureInitDoltRemote(syncURL, syncFromRemote, syncURLFromConfig, syncURLFromGitOrigin, localOnly)
}

// resolveRemoteHasDoltDataProbe fails closed: a probe error is UNKNOWN, and
// ADR-0002 treats unknown as has-data (refuse) rather than no-data. Returns
// the resolved bool for CheckRemoteSafety plus a note naming the probe
// failure (empty on success) so the caller can tell the user why.
func resolveRemoteHasDoltDataProbe(syncURL string, hasData bool, err error) (bool, string) {
	if err != nil {
		return true, fmt.Sprintf("could not verify refs/dolt/data on %s (%v); treating remote as having Dolt history", syncURL, err)
	}
	return hasData, ""
}

// handleRemoteSafetyDecision applies a CheckRemoteSafety decision at an init
// remote-divergence checkpoint. It returns (bootstrap, err): bootstrap is true
// when the caller should clone/bootstrap from the remote, and err is a non-nil
// *exitError when init must abort with a specific exit code.
//
// It must NOT call os.Exit: this runs inside the init RunE body, whose deferred
// metrics CloseEventAndAdd only fires on a normal return. Every refusal path
// returns an *exitError so the caller can propagate it up through RunE and keep
// the usage-metrics close intact (see errors.go).
func handleRemoteSafetyDecision(decision RemoteSafetyDecision, prefix, syncURL, destroyToken string, remoteHasDoltData func() (bool, error), observedRemoteHasDoltData bool, confirmed *bool) (bool, error) {
	switch decision.Action {
	case ActionRefuseDivergence, ActionRequireDestroyToken:
		fmt.Fprintf(os.Stderr, "\n%s\n\n", decision.UserMessage)
		return false, &exitError{Code: decision.ExitCode}
	case ActionBootstrap:
		return true, nil
	case ActionProceedWithDivergence:
		if err := confirmRemoteDestruction(decision, prefix, syncURL, destroyToken, confirmed); err != nil {
			return false, err
		}
		if err := verifyRemoteStateAfterConfirmation(remoteHasDoltData, observedRemoteHasDoltData); err != nil {
			return false, err
		}
	}
	return false, nil
}

func confirmRemoteDestruction(decision RemoteSafetyDecision, prefix, syncURL, destroyToken string, confirmed *bool) error {
	if decision.Reason != "authorized-divergence" || !term.IsTerminal(int(os.Stdin.Fd())) || destroyToken != "" || *confirmed {
		return nil
	}
	expected := FormatDestroyToken(prefix)
	fmt.Fprintf(os.Stderr, "\n%s You are about to discard the remote's Dolt history.\n\n", ui.RenderWarn("WARNING:"))
	fmt.Fprintf(os.Stderr, "  Remote: %s\n", syncURL)
	fmt.Fprintf(os.Stderr, "  Type %q to confirm: ", expected)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	if strings.TrimSpace(scanner.Text()) != expected {
		fmt.Fprintf(os.Stderr, "\nAborted. See 'bd help init-safety' for details.\n")
		return &exitError{Code: ExitDestroyTokenMissing}
	}
	*confirmed = true
	return nil
}

func verifyRemoteStateAfterConfirmation(remoteHasDoltData func() (bool, error), observedRemoteHasDoltData bool) error {
	if remoteHasDoltData == nil {
		return nil
	}
	// A probe error here is UNKNOWN, not "changed" — a transient
	// network blip on the confirmation probe must not abort a
	// destroy-token flow the user already confirmed.
	current, err := remoteHasDoltData()
	if err == nil && current != observedRemoteHasDoltData {
		fmt.Fprintf(os.Stderr, "\nAborted: remote state changed during confirmation. Re-run to re-verify intent.\n")
		return &exitError{Code: ExitRemoteDivergenceRefused}
	}
	return nil
}

func configureInitDoltRemote(ctx context.Context, store storage.DoltStorage, syncURL string, quiet bool) {
	hasRemote, _ := store.HasRemote(ctx, "origin")
	if hasRemote {
		return
	}
	if err := store.AddRemote(ctx, "origin", syncURL); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to add remote 'origin': %v\n", err)
		return
	}
	if !quiet {
		fmt.Printf("  %s Configured Dolt remote: origin → %s\n", ui.RenderPass("✓"), syncURL)
	}
}

func printInitNoDoltRemoteWarning(withExportNote bool) {
	fmt.Fprintf(os.Stderr, "\n%s No Dolt remote configured\n", ui.RenderWarn("⚠"))
	if withExportNote {
		fmt.Fprintln(os.Stderr, "  Issues are stored in local Dolt. .beads/issues.jsonl is an export,")
		fmt.Fprintln(os.Stderr, "  not cross-machine sync or the source of truth.")
	}
	if originURL, err := gitOriginGetURL(); err == nil && originURL != "" {
		fmt.Fprintln(os.Stderr, "  To use your git origin for durable sync, run:")
		fmt.Fprintf(os.Stderr, "    %s\n", ui.RenderAccent("bd dolt remote add origin "+normalizeRemoteURL(originURL)))
		fmt.Fprintf(os.Stderr, "    %s\n\n", ui.RenderAccent("bd dolt push"))
		return
	}
	fmt.Fprintln(os.Stderr, "  To enable durable sync, add a git origin and then run:")
	fmt.Fprintf(os.Stderr, "    %s\n", ui.RenderAccent("bd dolt push"))
}

type initSyncRemoteSource int

const (
	initSyncRemoteNone initSyncRemoteSource = iota
	initSyncRemoteExplicit
	initSyncRemoteConfigured
)

func resolveInitConfiguredSyncRemote(initRemote string, initRemoteChanged bool, resolveConfiguredRemote func() string) (string, initSyncRemoteSource) {
	if initRemoteChanged {
		return initRemote, initSyncRemoteExplicit
	}
	if syncURL := resolveConfiguredRemote(); syncURL != "" {
		return syncURL, initSyncRemoteConfigured
	}
	return "", initSyncRemoteNone
}

func initRemoteCloneMode(initServerMode, externalServer bool) remoteCloneMode {
	if !initServerMode {
		return remoteCloneEmbedded
	}
	if externalServer {
		return remoteCloneExternalServer
	}
	return remoteCloneCLI
}

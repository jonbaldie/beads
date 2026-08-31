package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/ui"
	"golang.org/x/term"
)

func guardInitExistingWorkspace(st *initRunContext) error {
	if st.safety.reinitLocal {
		return nil
	}
	err := checkExistingBeadsData(st.ident.prefix)
	if err == nil {
		return nil
	}
	if st.safety.initIfMissing && errors.Is(err, errWorkspaceAlreadyInitialized) {
		return skipInitIfMissing(st)
	}
	return fmt.Errorf("%v", err)
}

func skipInitIfMissing(st *initRunContext) error {
	existing := existingWorkspaceDBName()
	if st.cmd.Flags().Changed("database") {
		if initIfMissingDatabaseMismatch(existing, st.ident.database) {
			return fmt.Errorf("workspace already initialized as database %q, but --database %q was requested.\nRemove --database (or pass a matching value) to reuse the existing workspace", existing, st.ident.database)
		}
	} else if st.cmd.Flags().Changed("prefix") {
		if initIfMissingPrefixMismatch(existing, st.ident.prefix) {
			return fmt.Errorf("workspace already initialized as database %q, but --prefix %q was requested.\nRemove --prefix (or pass a matching value) to reuse the existing workspace", existing, st.ident.prefix)
		}
	}
	if !st.safety.quiet {
		fmt.Fprintln(os.Stderr, "Skipping init: workspace already initialized.")
	}
	return errInitSkipped
}

var errInitSkipped = errors.New("workspace already initialized")

func confirmInitReinit(st *initRunContext) error {
	if !st.safety.reinitLocal {
		return nil
	}
	count, err := countExistingIssues(st.ident.prefix)
	if err != nil || count <= 0 {
		return nil
	}
	printInitReinitWarning(count)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return confirmInitReinitInteractive(count)
	}
	return confirmInitReinitToken(st, count)
}

func printInitReinitWarning(count int) {
	fmt.Fprintf(os.Stderr, "\n%s Re-initializing will destroy the existing database.\n\n", ui.RenderWarn("WARNING:"))
	fmt.Fprintf(os.Stderr, "  Existing issues: %d\n\n", count)
	fmt.Fprintf(os.Stderr, "  This action CANNOT be undone. All issues, dependencies, and\n")
	fmt.Fprintf(os.Stderr, "  Dolt commit history will be permanently lost.\n\n")
	fmt.Fprintf(os.Stderr, "  Before proceeding, consider:\n")
	fmt.Fprintf(os.Stderr, "    bd export > issue-export.jsonl    # Export issue records, not full DB state\n")
	fmt.Fprintf(os.Stderr, "    bd dolt status              # Check if this is a server config issue\n\n")
}

func confirmInitReinitInteractive(count int) error {
	fmt.Fprintf(os.Stderr, "Type 'destroy %d issues' to confirm: ", count)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	expected := fmt.Sprintf("destroy %d issues", count)
	if strings.TrimSpace(scanner.Text()) != expected {
		fmt.Fprintf(os.Stderr, "\nAborted. Database was NOT modified.\n")
		return &exitError{Code: ExitLocalExistsRefused}
	}
	return nil
}

func confirmInitReinitToken(st *initRunContext, count int) error {
	expectedToken := FormatDestroyToken(st.ident.prefix)
	if st.safety.destroyToken == expectedToken {
		fmt.Fprintf(os.Stderr, "Destroy token accepted. Proceeding with re-initialization.\n")
		return nil
	}
	fmt.Fprintf(os.Stderr, "Refusing to destroy %d issues in non-interactive mode.\n", count)
	fmt.Fprintf(os.Stderr, "  See 'bd help init-safety' for the required --destroy-token format.\n")
	fmt.Fprintf(os.Stderr, "  Or export issue records first: bd export > issue-export.jsonl\n")
	return &exitError{Code: ExitDestroyTokenMissing}
}

func setupInitStealthAndPrefix(st *initRunContext) error {
	if st.safety.stealth {
		if err := setupStealthMode(!st.safety.quiet); err != nil {
			return fmt.Errorf("setting up stealth mode: %v", err)
		}
		st.safety.skipHooks = true
	}
	if getDBPath() == "" {
		if envDB := os.Getenv("BEADS_DB"); envDB != "" {
			setDBPath(envDB)
		}
	}
	return applyInitIssuePrefix(st)
}

func applyInitIssuePrefix(st *initRunContext) error {
	prefix := st.ident.prefix
	if prefix == "" {
		prefix = config.GetString("issue-prefix")
	}
	if prefix == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current directory: %v", err)
		}
		prefix = filepath.Base(cwd)
	}
	st.ident.prefix = normalizeIssuePrefix(prefix)
	return nil
}

func checkInitRemoteSafety(st *initRunContext) error {
	earlySyncURL, earlyRemoteSource := resolveInitConfiguredSyncRemote(st.ident.initRemote, st.ident.initRemoteChanged, resolveSyncRemote)
	earlySyncURL, hasData, note, err := probeInitEarlyRemote(st, earlySyncURL, earlyRemoteSource)
	if err != nil {
		return err
	}
	if earlySyncURL == "" {
		return nil
	}
	if note != "" {
		fmt.Fprintf(os.Stderr, "%s %s\n", ui.RenderWarn("!"), note)
	}
	return applyInitEarlyRemoteSafety(st, earlySyncURL, earlyRemoteSource, hasData)
}

func probeInitEarlyRemote(st *initRunContext, url string, source initSyncRemoteSource) (string, bool, string, error) {
	switch source {
	case initSyncRemoteExplicit:
		hasData, note := probeInitExplicitRemote(st, url)
		return url, hasData, note, nil
	case initSyncRemoteConfigured:
		hasData, note := probeInitConfiguredRemote(url)
		return url, hasData, note, nil
	default:
		return probeInitOriginRemote(st)
	}
}

func probeInitExplicitRemote(st *initRunContext, url string) (bool, string) {
	if !st.safety.fromJSONL {
		return false, ""
	}
	found, err := gitRemoteHasDoltDataRefStatus(url)
	return resolveRemoteHasDoltDataProbe(url, found, err)
}

func probeInitConfiguredRemote(url string) (bool, string) {
	found, err := gitRemoteHasDoltDataRefStatus(url)
	return resolveRemoteHasDoltDataProbe(url, found, err)
}

func probeInitOriginRemote(st *initRunContext) (string, bool, string, error) {
	if st.safety.stealth || !isGitRepo() || isBareGitRepo() {
		return "", false, "", nil
	}
	originURL, err := gitOriginGetURL()
	if err != nil || originURL == "" {
		return "", false, "", nil
	}
	url := normalizeRemoteURL(originURL)
	found, probeErr := gitOriginHasDoltDataRefStatus()
	hasData, note := resolveRemoteHasDoltDataProbe(url, found, probeErr)
	return url, hasData, note, nil
}

func applyInitEarlyRemoteSafety(st *initRunContext, earlySyncURL string, source initSyncRemoteSource, hasData bool) error {
	earlyDecision := CheckRemoteSafety(RemoteSafetyInput{
		Force:             st.safety.force,
		ReinitLocal:       st.safety.reinitLocal,
		FromJSONL:         st.safety.fromJSONL,
		DiscardRemote:     st.safety.discardRemote,
		DestroyToken:      st.safety.destroyToken,
		ExpectedToken:     FormatDestroyToken(st.ident.prefix),
		RemoteHasDoltData: hasData,
		IsInteractive:     term.IsTerminal(int(os.Stdin.Fd())),
	})
	_, err := handleRemoteSafetyDecision(earlyDecision, st.ident.prefix, earlySyncURL, st.safety.destroyToken, func() (bool, error) {
		switch source {
		case initSyncRemoteExplicit, initSyncRemoteConfigured:
			return gitRemoteHasDoltDataRefStatus(earlySyncURL)
		default:
			return gitOriginHasDoltDataRefStatus()
		}
	}, hasData, &st.resolved.remote.divergenceConfirmed)
	return err
}

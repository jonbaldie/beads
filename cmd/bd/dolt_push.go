package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/versioncontrolops"
	"github.com/spf13/cobra"
)

// isRemoteNotFoundErr checks whether the error is a Dolt "remote not found"
// error. This typically happens when the remote was added via `dolt remote add`
// (filesystem config) but not via `bd dolt remote add` (which also registers it
// in the SQL server's dolt_remotes table).
func isRemoteNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "remote") && strings.Contains(msg, "not found")
}

// remoteLister is the narrow store surface needed to confirm the structured
// no-remote-configured state.
type remoteLister interface {
	ListRemotes(ctx context.Context) ([]storage.RemoteInfo, error)
}

// persistedRemoteProber is implemented by stores that can check on-disk
// remote persistence (.dolt/repo_state.json) independently of the SQL
// server's dolt_remotes table (server-mode DoltStore).
type persistedRemoteProber interface {
	HasPersistedRemote() bool
}

// isConfirmedNoRemote reports whether a push/pull failure is the benign
// "no remote configured" case that may exit 0. isRemoteNotFoundErr alone is a
// loose string match that also fires on deleted/renamed remote-side repos,
// missing remote branches, and typoed remote names — real sync failures that
// must keep a non-zero exit so agents and CI notice (bd-6dnrw.7). Only an
// actually-empty dolt_remotes table makes the skip safe; if the remotes can't
// be listed, treat the failure as real. An empty table alone is still not
// proof in server mode: a freshly auto-started sql-server can report empty
// dolt_remotes at cold start even though remotes are persisted on disk
// (GH#2118) — the same reason the remote-migrate gate reads repo_state.json
// directly — so the on-disk probe must agree before the skip fires
// (bd-578h9.10).
func isConfirmedNoRemote(ctx context.Context, st remoteLister, err error) bool {
	if !isRemoteNotFoundErr(err) {
		return false
	}
	return hasNoRemoteConfigured(ctx, st)
}

// hasNoRemoteConfigured is the structural half of isConfirmedNoRemote: the
// positive proof that this rig really has no remote, independent of how the
// failure worded itself. It is what actually makes an exit-0 skip safe, so a
// caller with a different (broader) error classification — `bd sync`, which
// runs on a timer and must not fail every tick on a solo rig — can reuse the
// proof without loosening it.
func hasNoRemoteConfigured(ctx context.Context, st remoteLister) bool {
	configured, listErr := hasConfiguredRemote(ctx, st)
	return listErr == nil && !configured
}

// hasConfiguredRemote decides, from evidence, whether a rig has a Dolt remote
// at all. It is the one decider for the push/sync no-remote question: both
// hasNoRemoteConfigured (the `bd sync` / `bd dolt push|pull` exit-0 gate) and
// adoptGitOriginRemoteForPush go through it. The MUTATING siblings —
// `bd config apply`, ensureDoltRemote, and the git-protocol CLI push route —
// consult the same persisted evidence via PersistedRemoteInfos (wy-6k7f7);
// the read-only doctor/drift diagnostics still judge from len(ListRemotes)
// alone and can report a false "no origin remote" during the cold-start
// window. Do not assume a new remote-related decision is covered by this
// function: route it here.
//
// Two sources of evidence, because dolt_remotes alone is not enough: a
// server-mode rig whose sql-server has just cold-started can report an EMPTY
// dolt_remotes while the remote IS persisted on disk in .dolt/repo_state.json
// (GH#2118), so the on-disk probe gets a veto. The probe has to be reached
// through the storage decorator chain, since HasPersistedRemote is not part of
// storage.DoltStorage and the store bd holds is all but always decorated —
// that is wy-xtv17.
//
// A failed listing is neither evidence: it returns the error, and callers must
// not read it as "no remote". Having two sibling functions decide this from
// different evidence is what left `bd dolt push` still trusting an empty
// dolt_remotes after wy-xtv17 hardened the no-remote gate (wy-82hc5), so the
// rule lives here once.
func hasConfiguredRemote(ctx context.Context, st remoteLister) (bool, error) {
	remotes, listErr := st.ListRemotes(ctx)
	if listErr != nil {
		return false, listErr
	}
	if len(remotes) > 0 {
		return true, nil
	}
	if prober, ok := persistedRemoteProberFor(st); ok && prober.HasPersistedRemote() {
		return true, nil
	}
	return false, nil
}

// persistedRemoteProberFor finds the on-disk remote probe behind any chain of
// storage decorators.
//
// HasPersistedRemote is not part of storage.DoltStorage — only the concrete
// *dolt.DoltStore implements it — while the store bd actually holds is the
// composed chain caller → HookFiringStore → InstrumentedStorage → DoltStore
// (wireStorageDecorators). The hook layer is present on essentially every rig:
// main.go builds a hook runner whenever there is a dbPath, whether or not any
// hook scripts exist, so only no-hooks:true / BD_NO_HOOKS=1 leaves it off.
// Asserting straight on the passed store therefore all but always failed,
// silently skipping the GH#2118 cold-start probe and letting `bd sync` /
// `bd dolt push|pull` report "no remote configured" and exit 0 forever on a rig
// whose remote is persisted in .dolt/repo_state.json (wy-xtv17).
//
// It peels via storage.Unwrapper, the same contract storage.UnwrapStore uses,
// rather than calling UnwrapStore itself: this helper takes the narrow
// remoteLister, not a storage.DoltStorage. A store that implements the probe
// directly is honored before any peeling, so test doubles and any future
// decorator that forwards HasPersistedRemote keep working.
func persistedRemoteProberFor(st remoteLister) (persistedRemoteProber, bool) {
	for {
		if prober, ok := st.(persistedRemoteProber); ok {
			return prober, true
		}
		u, ok := st.(storage.Unwrapper)
		if !ok {
			return nil, false
		}
		inner := u.Unwrap()
		if inner == nil {
			return nil, false
		}
		st = inner
	}
}

// persistedRemoteInfoLister is the recovery-grade sibling of
// persistedRemoteProber: stores that can enumerate the on-disk remotes
// (name AND url) rather than only report their existence (server-mode
// DoltStore). Callers use it in the GH#2118 cold-start window to act on the
// invisible remote's actual URL instead of merely refusing (wy-6k7f7).
type persistedRemoteInfoLister interface {
	PersistedRemoteInfos() []storage.RemoteInfo
}

// persistedRemoteInfosFor finds the on-disk remote enumeration behind any
// chain of storage decorators, peeling exactly like persistedRemoteProberFor
// (a direct implementer is honored before any peeling, so test doubles work).
// Returns nil when no store in the chain can enumerate persisted remotes —
// embedded rigs, where GH#2118 cannot occur.
func persistedRemoteInfosFor(st any) []storage.RemoteInfo {
	for {
		if lister, ok := st.(persistedRemoteInfoLister); ok {
			return lister.PersistedRemoteInfos()
		}
		u, ok := st.(storage.Unwrapper)
		if !ok {
			return nil
		}
		inner := u.Unwrap()
		if inner == nil {
			return nil
		}
		st = inner
	}
}

// isDivergedHistoryErr checks whether the error indicates that local and remote
// Dolt histories have diverged. This happens when independent pushes create
// separate commit histories with no common merge base (e.g., two agents
// bootstrapping from scratch and pushing to the same remote, or a local
// database being re-initialized while the remote retains the old history).
func isDivergedHistoryErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no common ancestor") ||
		strings.Contains(msg, "can't find common ancestor") ||
		strings.Contains(msg, "cannot find common ancestor")
}

// isAncestorPKMismatchErr reports Dolt's hard refusal to merge a table whose
// primary key set differs across the merging histories or in their common
// ancestor. The classification lives in dberrors so the cross-upgrade merge
// test (internal/storage/dolt) can pin it against a real Dolt refusal; see
// dberrors.IsAncestorPKMismatch for the full background (#4259).
func isAncestorPKMismatchErr(err error) bool {
	return dberrors.IsAncestorPKMismatch(err)
}

// ancestorPKMismatchTable extracts the table name from a Dolt
// different-primary-keys merge refusal, or "" if it cannot be determined.
func ancestorPKMismatchTable(err error) string {
	return dberrors.AncestorPKMismatchTable(err)
}

// printAncestorPKMismatchGuidance prints recovery guidance when a Dolt merge
// is refused because a table's primary key set differs across the merging
// histories or in their common ancestor. Unlike row conflicts, this cannot be
// auto-resolved and does not converge on retry; the clones must be
// re-converged through one canonical clone.
func printAncestorPKMismatchGuidance(err error) {
	w := os.Stderr
	table := ancestorPKMismatchTable(err)
	fmt.Fprintln(w, "")
	if table != "" {
		fmt.Fprintf(w, "Dolt refused to merge: table %q has different primary keys across\n", table)
	} else {
		fmt.Fprintln(w, "Dolt refused to merge: a table has different primary keys across")
	}
	fmt.Fprintln(w, "the local and remote histories (or in their common ancestor).")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "This is a schema fork: two clones reshaped the table's primary key")
	fmt.Fprintln(w, "independently, usually by upgrading bd (and so running schema migrations)")
	fmt.Fprintln(w, "separately on each clone while un-synced changes existed on both sides.")
	fmt.Fprintln(w, "Retrying will not help — these histories can no longer be merged.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Recovery (bootstrap from one canonical clone):")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  1. Pick ONE clone as canonical (usually the most complete/up-to-date),")
	fmt.Fprintln(w, "     upgrade bd there, and make the remote authoritative:")
	fmt.Fprintln(w, "       bd dolt push --force")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  2. On EVERY other clone, save local-only work, re-clone, re-apply:")
	fmt.Fprintln(w, "       bd export --all -o /tmp/beads-local.jsonl")
	fmt.Fprintln(w, "       rm -rf .beads/dolt")
	fmt.Fprintln(w, "       bd bootstrap")
	fmt.Fprintln(w, "       bd import /tmp/beads-local.jsonl")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Full playbook (and how to prevent this during upgrades):")
	fmt.Fprintln(w, "  https://github.com/gastownhall/beads/blob/main/docs/recovery/init-safety.md#pk-fork-refused")
}

// printNoRemoteGuidance prints an informational message (to stdout) when
// push or pull is attempted but no Dolt remote is configured. Exits 0 because
// the absence of a remote is a valid configuration — not an error.
func printNoRemoteGuidance() {
	fmt.Println("No remote is configured — skipping.")
	fmt.Println("")
	fmt.Println("For solo use, pushing is optional — your issues are stored locally")
	fmt.Println("in .beads/ and versioned by Dolt automatically.")
	fmt.Println("")
	fmt.Println("To set up remote sync (for backup or team sharing):")
	fmt.Println("  bd dolt remote add origin <url>")
	fmt.Println("  bd dolt push")
	fmt.Println("")
	fmt.Println("Supported remote URLs:")
	fmt.Println("  • GitHub (via git):   git+ssh://git@github.com/org/repo.git")
	fmt.Println("  • DoltHub:            https://doltremoteapi.dolthub.com/org/repo")
	fmt.Println("  • Azure Blob Storage: az://account.blob.core.windows.net/container/path")
}

// adoptGitOriginRemoteForPush gives a rig with no Dolt remote the one its git
// origin implies, so `bd dolt push` works out of the box.
//
// It asks hasConfiguredRemote rather than reading len(ListRemotes) itself: an
// empty dolt_remotes is not proof of a remote-less rig during the GH#2118
// cold-start window, and adopting there re-derives the remote from git.
// Usually that is the same URL and the AddRemote is a harmless re-add, but
// when the persisted remote and the git origin disagree — a Dolt remote
// deliberately pointed elsewhere, or a renamed/redirected origin — the rig
// starts pushing somewhere else on the strength of a stale listing (wy-82hc5).
//
// Adoption requires consent (#5068). The policy is decided by
// decideRemoteAdoption in dolt_remote_adopt.go and applied here, after the URL
// is known and before anything is written: nothing below this point is
// reachable without either --yes or an interactive confirmation.
func adoptGitOriginRemoteForPush(ctx context.Context, st storage.DoltStorage, policy adoptPolicy, optIn adoptOptIn) (bool, error) {
	configured, err := hasConfiguredRemote(ctx, st)
	if err != nil || configured {
		return false, err
	}
	// Deriving the URL comes first and is read-only (`git remote get-url`).
	// Workspace resolution is deliberately NOT done yet: selectedDoltBeadsDir
	// calls prepareSelectedNoDBContext, which mutates process and on-disk
	// workspace state, and nothing may mutate before consent is established.
	originURL, err := gitOriginGetURLForActiveRepo(ctx)
	if err != nil || originURL == "" {
		return false, nil
	}
	remoteURL := normalizeRemoteURL(originURL)

	if proceed, err := applyAdoptionConsent(remoteURL, policy, optIn); err != nil || !proceed {
		return false, err
	}
	return persistAdoptedGitOriginRemote(ctx, st, remoteURL)
}

func persistAdoptedGitOriginRemote(
	ctx context.Context,
	st storage.DoltStorage,
	remoteURL string,
) (bool, error) {
	beadsDir := selectedDoltBeadsDir()
	if beadsDir == "" {
		return false, fmt.Errorf("no active beads workspace")
	}

	if err := st.AddRemote(ctx, "origin", remoteURL); err != nil {
		return false, err
	}

	if err := config.SetYamlConfigInDir(beadsDir, "sync.remote", remoteURL); err != nil {
		return false, fmt.Errorf("failed to persist sync.remote to config.yaml: %w", err)
	}
	fmt.Fprintln(os.Stderr, "Committing .beads/config.yaml (sync.remote) under your git identity.")
	commitBeadsConfigForActiveRepo(ctx, "bd: update sync.remote")
	return true, nil
}

// printDivergedHistoryGuidance prints recovery guidance when push/pull fails
// due to diverged local and remote histories.
func printDivergedHistoryGuidance(_ string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Local and remote Dolt histories have diverged.")
	fmt.Fprintln(os.Stderr, "This means the local database and the remote have independent commit")
	fmt.Fprintln(os.Stderr, "histories with no common merge base.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Recovery options:")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  1. Keep remote, discard local (recommended if remote is authoritative):")
	fmt.Fprintln(os.Stderr, "       bd bootstrap              # re-clone from remote")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  2. Keep local, overwrite remote (if local is authoritative):")
	fmt.Fprintln(os.Stderr, "       bd dolt push --force       # force-push local history to remote")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  3. Manual recovery (re-initialize local database):")
	fmt.Fprintln(os.Stderr, "       rm -rf .beads/dolt         # delete local Dolt database")
	fmt.Fprintln(os.Stderr, "       bd bootstrap              # re-clone from remote")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Tip: This usually happens when multiple agents independently initialize")
	fmt.Fprintln(os.Stderr, "databases and push to the same remote. Use 'bd bootstrap' to clone an")
	fmt.Fprintln(os.Stderr, "existing remote instead of 'bd init' to avoid divergent histories.")
}

var doltPushCmd = &cobra.Command{
	Use:           "push",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "Push commits to Dolt remote",
	Long: `Push local Dolt commits to the configured remote.

Requires a Dolt remote to be configured in the database directory. With no
remote configured, bd can adopt one derived from git origin — only with
consent: interactively, or via --yes; --no-adopt or BD_NO_REMOTE_ADOPT=1
disables adoption entirely.
For Hosted Dolt, set DOLT_REMOTE_USER and DOLT_REMOTE_PASSWORD environment
variables for authentication.

Use --force to overwrite remote changes (e.g., when the remote has
uncommitted changes in its working set).

Use --remote to push to a specific named remote instead of the default.
The remote must already exist (see 'bd dolt remote add').`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if config.GetBool("no-push") {
			fmt.Println("skipping push: rig is local-only (no-push: true)")
			return nil
		}
		if isDoltLocalOnly() {
			if isJSONOutput() {
				if err := outputJSONRaw(map[string]string{"status": "disabled", "reason": "dolt.local-only=true"}); err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				}
				return nil
			}
			fmt.Println("Remote sync is disabled for this project (dolt.local-only=true).")
			fmt.Println("Your issues are stored locally in .beads/.")
			fmt.Println("To re-enable remote sync: bd config unset dolt.local-only")
			return nil
		}
		ctx := context.Background()
		st := getStore()
		if st == nil {
			return HandleError("no store available")
		}
		force, _ := cmd.Flags().GetBool("force")
		remote, _ := cmd.Flags().GetString("remote")
		if remote != "" {
			fmt.Printf("Pushing to Dolt remote %q...\n", remote)
			if err := st.PushRemote(ctx, remote, force); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				if isRemoteNotFoundErr(err) {
					fmt.Fprintf(os.Stderr, "\nRemote %q is not configured.\n", remote)
					fmt.Fprintln(os.Stderr, "Use 'bd dolt remote add <name> <url>' to add it.")
					fmt.Fprintln(os.Stderr, "Use 'bd dolt remote list' to see configured remotes.")
				} else if isAncestorPKMismatchErr(err) {
					printAncestorPKMismatchGuidance(err)
				} else if isDivergedHistoryErr(err) {
					printDivergedHistoryGuidance("push --force")
				}
				return SilentExit()
			}
			fmt.Println("Push complete.")
			return nil
		}
		assumeYes, _ := cmd.Flags().GetBool("yes")
		noAdopt, _ := cmd.Flags().GetBool("no-adopt")
		policy := currentAdoptPolicy(assumeYes, noAdopt, stdinIsTerminal(), isJSONOutput())
		if adopted, err := adoptGitOriginRemoteForPush(ctx, st, policy, pushAdoptOptIn); err != nil {
			return HandleError("%v", err)
		} else if adopted {
			fmt.Println("Configured Dolt remote origin from git origin.")
		}
		fmt.Println("Pushing to Dolt remote...")

		var pushErr error
		if force {
			pushErr = st.ForcePush(ctx)
		} else {
			pushErr = st.Push(ctx)
		}
		if pushErr != nil {
			if isConfirmedNoRemote(ctx, st, pushErr) {
				printNoRemoteGuidance()
				return nil
			}
			fmt.Fprintf(os.Stderr, "Error: %v\n", pushErr)
			if isAncestorPKMismatchErr(pushErr) {
				printAncestorPKMismatchGuidance(pushErr)
			} else if isDivergedHistoryErr(pushErr) {
				op := "push"
				if force {
					op = "push --force"
				}
				printDivergedHistoryGuidance(op)
			}
			return SilentExit()
		}
		fmt.Println("Push complete.")
		return nil
	},
}

var doltPullCmd = &cobra.Command{
	Use:           "pull",
	SilenceUsage:  true,
	SilenceErrors: true,
	Short:         "Pull commits from Dolt remote",
	Long: `Pull commits from the configured Dolt remote into the local database.

Requires a Dolt remote to be configured in the database directory.
For Hosted Dolt, set DOLT_REMOTE_USER and DOLT_REMOTE_PASSWORD environment
variables for authentication.

Use --remote to pull from a specific named remote instead of the default.
The remote must already exist (see 'bd dolt remote add').

Use --strategy ours|theirs to resolve conflicts the auto-resolver declines
(e.g. both sides edited the same issue since the last sync) instead of
aborting the pull for manual resolution. Embedded storage only (#4992); on
server-mode/sql-server storage use 'bd conflicts resolve' after a pull that
reports conflicts.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if isDoltLocalOnly() {
			if isJSONOutput() {
				if err := outputJSONRaw(map[string]string{"status": "disabled", "reason": "dolt.local-only=true"}); err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				}
				return nil
			}
			fmt.Println("Remote sync is disabled for this project (dolt.local-only=true).")
			fmt.Println("Nothing to pull.")
			fmt.Println("To re-enable remote sync: bd config unset dolt.local-only")
			return nil
		}
		ctx := context.Background()
		st := getStore()
		if st == nil {
			return HandleError("no store available")
		}
		remote, _ := cmd.Flags().GetString("remote")
		strategy, _ := cmd.Flags().GetString("strategy")
		if strategy != "" {
			if err := versioncontrolops.ValidateConflictStrategy(strategy); err != nil {
				return HandleError("%v", err)
			}
		}
		var puller storage.StrategicPuller
		if strategy != "" {
			var ok bool
			puller, ok = storage.UnwrapStore(st).(storage.StrategicPuller)
			if !ok {
				return HandleError("storage backend %T does not support --strategy pulls (#4992): only embedded storage does; on server-mode/sql-server storage, resolve conflicts with 'bd conflicts resolve' or the raw dolt CLI", storage.UnwrapStore(st))
			}
		}
		if remote != "" {
			fmt.Printf("Pulling from Dolt remote %q...\n", remote)
			var err error
			if strategy != "" {
				err = puller.PullRemoteWithStrategy(ctx, remote, strategy)
			} else {
				err = st.PullRemote(ctx, remote)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				if isRemoteNotFoundErr(err) {
					fmt.Fprintf(os.Stderr, "\nRemote %q is not configured.\n", remote)
					fmt.Fprintln(os.Stderr, "Use 'bd dolt remote add <name> <url>' to add it.")
					fmt.Fprintln(os.Stderr, "Use 'bd dolt remote list' to see configured remotes.")
				} else if isAncestorPKMismatchErr(err) {
					printAncestorPKMismatchGuidance(err)
				} else if isDivergedHistoryErr(err) {
					printDivergedHistoryGuidance("pull")
				}
				return SilentExit()
			}
			fmt.Println("Pull complete.")
			return nil
		}
		fmt.Println("Pulling from Dolt remote...")
		var err error
		if strategy != "" {
			err = puller.PullWithStrategy(ctx, strategy)
		} else {
			err = st.Pull(ctx)
		}
		if err != nil {
			if isConfirmedNoRemote(ctx, st, err) {
				printNoRemoteGuidance()
				return nil
			}
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			if isAncestorPKMismatchErr(err) {
				printAncestorPKMismatchGuidance(err)
			} else if isDivergedHistoryErr(err) {
				printDivergedHistoryGuidance("pull")
			}
			return SilentExit()
		}
		fmt.Println("Pull complete.")
		return nil
	},
}

var doltCommitCmd = &cobra.Command{
	Use:   "commit",
	Short: "Create a Dolt commit from pending changes",
	Long: `Create a Dolt commit from any uncommitted changes in the working set.

This is the primary commit point for batch mode. When auto-commit is set to
"batch", changes accumulate in the working set across multiple bd commands and
are committed together here with a descriptive summary message.

Also useful before push operations that require a clean working set, or when
auto-commit was off or changes were made externally.

For more options (--stdin, custom messages), see: bd vc commit`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		st := getStore()
		if st == nil {
			return HandleError("no store available")
		}
		msg, _ := cmd.Flags().GetString("message")
		if msg == "" {
			msg = fmt.Sprintf("bd: dolt commit (auto-commit) by %s", getActor())
		}
		beforeHash, beforeErr := st.GetCurrentCommit(ctx)
		if err := st.Commit(ctx, msg); err != nil {
			if isDoltNothingToCommit(err) {
				fmt.Println("Nothing to commit.")
				return nil
			}
			return HandleError("%v", err)
		}
		setCommandDidExplicitDoltCommit(true)

		// A store whose Commit tolerates nothing-to-commit (e.g. the embedded
		// store) returns a nil error even when HEAD did not move. Detect that
		// case here instead of relying on the error, so both backends report
		// the same "nothing to commit" outcome.
		if afterHash, afterErr := st.GetCurrentCommit(ctx); beforeErr == nil && afterErr == nil && afterHash == beforeHash {
			fmt.Println("Nothing to commit.")
			return nil
		}

		fmt.Println("Committed.")
		return nil
	},
}

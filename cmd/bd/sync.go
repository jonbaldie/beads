package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/spf13/cobra"
)

// Exit codes for `bd sync`, chosen so a sync timer can branch on the outcome
// without parsing output (wy-jpd3.4). The reference deployment this verb
// generalizes — a two-machine beads federation on a 60-second timer — needs
// exactly three distinctions: it worked, a human must resolve a merge conflict,
// or another replica kept winning the push race and the next tick should just
// try again.
//
//	0  synced (or nothing to do)
//	1  error (transport, auth, storage — the usual bd failure code)
//	2  merge conflict; the sync halted and nothing was pushed. NOT auto-resolved.
//	3  retries exhausted on a transient, self-healing condition (another replica
//	   kept winning the push race, or a concurrent writer kept the working set
//	   dirty); retry on the next tick.
//	4  the dirty working set blocking the is_blocked repair is NOT transient: the
//	   same pending graph edits have blocked every tick for a while and nothing
//	   is advancing. Retrying will never publish; an operator must resolve it.
const (
	ExitSyncConflict         = 2
	ExitSyncRetriesExhausted = 3
	ExitSyncDirtyStuck       = 4
)

// defaultSyncAttempts bounds the pull->push cycle. Three is the production
// default of the reference implementation: a push race resolves on the first
// retry in practice, and an unbounded loop under a busy fleet never converges.
const defaultSyncAttempts = 3

// Sync outcome statuses, also the "status" value in --json output.
const (
	syncStatusOK               = "ok"
	syncStatusConflict         = "conflict"
	syncStatusRetriesExhausted = "retries-exhausted"
	syncStatusDirtyStuck       = "dirty-stuck"
	syncStatusDisabled         = "disabled"
	syncStatusNoRemote         = "no-remote"
)

// Kinds of per-attempt transient failure recorded in syncOutcome.Transients.
const (
	syncTransientPushRace   = "push-race"
	syncTransientDirtyGraph = "dirty-graph"
)

var syncCmd = &cobra.Command{
	Use:     "sync",
	GroupID: "sync",
	Short:   "Pull, check for conflicts, repair is_blocked, and push (the federation loop)",
	Long: `Run one full synchronization cycle against the Dolt remote.

This is the loop every multi-machine beads deployment otherwise hand-rolls in
shell:

  1. pull from the remote
  2. check for merge conflicts POSITIVELY, from the merge's own conflict rows
     and from Dolt's conflict tables — never inferred from the pull's exit
     status, which is not a trustworthy conflict signal in either direction
  3. recompute the denormalized is_blocked flag, so dependency edges merged in
     from another replica do not leave 'bd ready' stale
  4. push, retrying a bounded number of times when another replica wins the
     push race

The repair in step 3 refuses to run while another writer has uncommitted changes
to issues/dependencies. That is transient and not this sync's doing, so it is
retried on the same budget as a push race rather than failing the run. A working
set that is NOT transient exits 4 instead, because no amount of retrying will
ever publish and only an operator can clear it. Two kinds of evidence say so:
constraint violations on the dirty tables are detected positively and escalate
on the very attempt that finds them; an abandoned uncommitted edit has no such
positive signal, so it is only inferred once the same pending graph edits have
blocked every attempt of several consecutive runs.

Conflicts sync cannot resolve safely are NEVER auto-resolved: it halts before
recomputing or pushing and exits 2, and repeated runs keep halting the same way
until an operator resolves the divergence. (The pull underneath does auto-settle
the conflict classes it can settle convergently — machine-local metadata,
audit-only dependency rows, and last-write-wins on issue cells. Anything beyond
those halts here.) Whether the halted merge was aborted or left live in the
working set depends on the pull route, so the halt message reports which.

Exit codes (a sync timer can branch on these without parsing output):

  0  synced, or nothing to do
  1  error (transport, auth, storage)
  2  merge conflict — halted, nothing pushed, resolve it by hand
  3  retries exhausted (push race, or a concurrent writer's dirty working set)
     — transient, nothing pushed, retry on the next tick
  4  the dirty working set is stuck, not busy: identical pending graph edits
     blocked every attempt of several consecutive runs — nothing pushed, and no
     later tick will publish until an operator clears it

On the default-remote path, a rig with no Dolt remote yet but a git origin
configured adopts that origin as its Dolt remote first, exactly as 'bd dolt push'
does — so 'bd sync' works as a first-time federation bring-up step instead of
reporting 'no remote' and doing nothing. Passing --remote never adopts anything.

This is not 'bd federation sync', which syncs with named peer towns and takes a
--strategy ours|theirs to resolve whatever conflicts it meets. 'bd sync' targets
the configured remote and has no such switch: what it cannot settle, it halts on.

Examples:
  bd sync                        # sync with the default remote
  bd sync --remote mini          # sync with a specific remote
  bd sync --attempts 5           # allow more push-race retries
  bd sync --json                 # machine-parseable outcome`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runSyncCommand,
}

func init() {
	syncCmd.Flags().String("remote", "", "Sync with a specific named remote instead of the default")
	syncCmd.Flags().Int("attempts", defaultSyncAttempts, "Maximum pull/push attempts before reporting a transient retry exhaustion (exit 3)")
	syncCmd.Flags().BoolP("yes", "y", false, "Consent to adopting a Dolt remote derived from git origin when none is configured")
	syncCmd.Flags().Bool("no-adopt", false, "Never derive a Dolt remote from git origin (also BD_NO_REMOTE_ADOPT=1)")
	rootCmd.AddCommand(syncCmd)
}

// syncAdoptGitOrigin is runSyncCommand's git-origin adoption step, held in a
// variable purely as a test seam. Adoption's own machinery — resolving the
// active workspace, shelling out to `git remote get-url origin`, writing
// sync.remote into config.yaml and committing it — is exercised against the
// real thing in dolt_test.go; letting it run for real from a runSyncCommand
// unit test would mutate whatever repo the tests happen to be run from. The
// production binding is pinned by TestSyncAdoptGitOriginIsWiredToAdoption.
var syncAdoptGitOrigin func(context.Context, storage.DoltStorage, adoptPolicy, adoptOptIn) (bool, error) = adoptGitOriginRemoteForPush

func runSyncCommand(cmd *cobra.Command, _ []string) error {
	if usesProxiedServer() {
		return HandleErrorRespectJSON("sync is not supported in proxied-server mode")
	}
	if err := CheckReadonly("sync"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("sync")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	if handled, err := handleDisabledSync(); handled {
		return err
	}

	input, err := gatherSyncCommandInput(cmd)
	if err != nil {
		return err
	}
	st, recomputer, err := loadSyncCommandStore()
	if err != nil {
		return err
	}
	if err := adoptSyncRemoteIfNeeded(cmd, input.remote, st); err != nil {
		return err
	}

	ops := buildSyncCommandOps(st, recomputer, input.remote, input.noPush)
	configureSyncProgress(&ops)
	out, err := runSyncLoop(getRootContext(), ops, input.attempts)
	markSkippedSyncPush(out, input.noPush)
	if err != nil {
		return handleSyncCommandError(input.remote, st, err)
	}
	return finishSyncCommand(out, input.noPush)
}

func markSkippedSyncPush(out *syncOutcome, noPush bool) {
	if noPush && out.Status == syncStatusOK {
		out.Pushed = false
		out.PushSkipped = true
	}
}

type syncCommandInput struct {
	attempts int
	remote   string
	noPush   bool
}

func handleDisabledSync() (bool, error) {
	if !isDoltLocalOnly() {
		return false, nil
	}
	if isJSONOutput() {
		return true, outputJSON(&syncOutcome{Status: syncStatusDisabled})
	}
	fmt.Println("Remote sync is disabled for this project (dolt.local-only=true).")
	fmt.Println("To re-enable remote sync: bd config unset dolt.local-only")
	return true, nil
}

func gatherSyncCommandInput(cmd *cobra.Command) (syncCommandInput, error) {
	attempts, _ := cmd.Flags().GetInt("attempts")
	if attempts < 1 {
		return syncCommandInput{}, HandleErrorRespectJSON("--attempts must be at least 1 (got %d)", attempts)
	}
	remote, _ := cmd.Flags().GetString("remote")
	return syncCommandInput{
		attempts: attempts,
		remote:   remote,
		noPush:   config.GetBool("no-push"),
	}, nil
}

func loadSyncCommandStore() (storage.DoltStorage, storage.BlockedRecomputer, error) {
	st := getStore()
	if st == nil {
		return nil, nil, HandleErrorRespectJSON("no store available")
	}
	recomputer, ok := storage.UnwrapStore(st).(storage.BlockedRecomputer)
	if !ok {
		return nil, nil, HandleErrorRespectJSON("storage backend does not support is_blocked recompute")
	}
	return st, recomputer, nil
}

// adoptSyncRemoteIfNeeded mirrors `bd dolt push`: on the default-remote path,
// a git origin can supply the Dolt remote for a first-time federation rig.
func adoptSyncRemoteIfNeeded(cmd *cobra.Command, remote string, st storage.DoltStorage) error {
	if remote != "" {
		return nil
	}
	syncYes, _ := cmd.Flags().GetBool("yes")
	syncNoAdopt, _ := cmd.Flags().GetBool("no-adopt")
	adopted, err := syncAdoptGitOrigin(getRootContext(), st, currentAdoptPolicy(syncYes, syncNoAdopt, stdinIsTerminal(), isJSONOutput()), syncAdoptOptIn)
	if err != nil {
		return HandleErrorRespectJSON("sync failed: adopting git origin as Dolt remote: %v", err)
	}
	if adopted && !isJSONOutput() && !isQuiet() {
		fmt.Println("sync: configured Dolt remote origin from git origin.")
	}
	return nil
}

func buildSyncCommandOps(st storage.DoltStorage, recomputer storage.BlockedRecomputer, remote string, noPush bool) syncOps {
	return syncOps{
		pull:             syncPullOp(st, remote),
		conflicts:        syncConflictsOp(st),
		recompute:        syncRecomputeOp(recomputer),
		dirtyFingerprint: dirtyGraphFingerprintOp(st),
		mergeBlockers:    mergeBlockersOp(st),
		push:             syncPushOp(st, remote, noPush),
	}
}

func syncPullOp(st storage.DoltStorage, remote string) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		var err error
		if remote != "" {
			err = st.PullRemote(ctx, remote)
		} else {
			err = st.Pull(ctx)
		}
		return classifyPullError(err)
	}
}

func syncConflictsOp(st storage.DoltStorage) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		cs, err := st.GetConflicts(ctx)
		if err != nil {
			return nil, err
		}
		return conflictTables(cs), nil
	}
}

func syncRecomputeOp(recomputer storage.BlockedRecomputer) func(context.Context) (int, error) {
	return recomputer.RecomputeAllBlocked
}

func syncPushOp(st storage.DoltStorage, remote string, noPush bool) func(context.Context) error {
	return func(ctx context.Context) error {
		if noPush {
			return nil
		}
		if remote != "" {
			return st.PushRemote(ctx, remote, false)
		}
		return st.Push(ctx)
	}
}

func configureSyncProgress(ops *syncOps) {
	// Per-step progress is non-essential output -q exists to silence: this
	// verb's whole point is running on a short timer.
	if !isJSONOutput() && !isQuiet() {
		ops.progress = func(format string, args ...interface{}) {
			fmt.Printf("sync: "+format+"\n", args...)
		}
	}
}

func handleSyncCommandError(remote string, st storage.DoltStorage, err error) error {
	// A rig with no remote at all is the benign solo case the dolt verbs already
	// exit 0 on — only when emptiness is confirmed and only on the default path.
	if remote == "" && isNoRemoteConfiguredErr(err) && hasNoRemoteConfigured(getRootContext(), st) {
		if isJSONOutput() {
			return outputJSON(&syncOutcome{Status: syncStatusNoRemote})
		}
		if !isQuiet() {
			printNoRemoteGuidance()
		}
		return nil
	}
	exitErr := HandleErrorRespectJSON("sync failed: %v", err)
	if !isJSONOutput() {
		printSyncErrorGuidance(remote, err)
	}
	return exitErr
}

func finishSyncCommand(out *syncOutcome, noPush bool) error {
	// Cross-tick half of the stuck detector, before reporting so output and the
	// exit code agree on what this run was.
	applyDirtyProgress(out, time.Now())
	if isJSONOutput() {
		if err := outputJSON(out); err != nil {
			return HandleError("%v", err)
		}
	} else {
		printSyncOutcome(out, noPush)
	}
	return syncCommandExit(out.Status)
}

func syncCommandExit(status string) error {
	switch status {
	case syncStatusConflict:
		return &exitError{Code: ExitSyncConflict}
	case syncStatusRetriesExhausted:
		return &exitError{Code: ExitSyncRetriesExhausted}
	case syncStatusDirtyStuck:
		return &exitError{Code: ExitSyncDirtyStuck}
	default:
		return nil
	}
}

// mergeBlockersOp builds the loop's positive constraint-violation hook, or nil
// when this store cannot answer the question — an unimplemented interface
// must leave the detector silent, never guessing. Bound to the same st the
// rest of ops closes over rather than the package's legacy store global,
// which getStore() can diverge from once cmdCtx is in play.
func mergeBlockersOp(st storage.DoltStorage) func(context.Context) (storage.MergeBlockers, error) {
	inspector, ok := storage.UnwrapStore(st).(storage.MergeBlockerInspector)
	if !ok {
		return nil
	}
	return inspector.GetMergeBlockers
}

// dirtyGraphFingerprintOp builds the loop's dirty-graph evidence hook, or nil
// when this store cannot answer the question — an unimplemented interface must
// leave the detector silent, never guessing.
func dirtyGraphFingerprintOp(st storage.DoltStorage) func(context.Context) (string, error) {
	accessor, ok := storage.UnwrapStore(st).(storage.RawDBAccessor)
	if !ok {
		return nil
	}
	db := accessor.DB()
	if db == nil {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		return issueops.DirtyGraphFingerprint(ctx, db)
	}
}

// printSyncErrorGuidance reuses the recovery guidance the dolt push/pull verbs
// print, so a failed sync is as actionable as the hand-rolled loop it replaces.
// The classifiers match on the message, so they still fire through the step
// wrapping runSyncLoop adds.

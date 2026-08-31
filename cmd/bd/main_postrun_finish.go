package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"time"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/storage/embeddeddolt"
	"github.com/jonbaldie/beads/internal/telemetry"
	"github.com/spf13/cobra"
)

func finishPersistentProxiedPostRun(cmd *cobra.Command) {
	if shouldAutoPruneEventsJournal(cmd) {
		maybeAutoPruneEventsJournal(getRootContext(), beads.FindBeadsDir())
	}
	if provider := getUOWProvider(); provider != nil {
		_ = provider.Close(getRootContext())
		setUOWProvider(nil)
	}
}

func finishPersistentEmbeddedPostRun(cmd *cobra.Command) error {
	if runsPostCommandMaintenance(cmd.Name(), isReadonlyMode()) {
		if err := runPersistentPostMaintenance(cmd); err != nil {
			return err
		}
	}
	storeMutex.Lock()
	setStoreActive(false)
	storeMutex.Unlock()
	if getStore() != nil {
		_ = getStore().Close()
		setStore(nil)
	}
	return nil
}

func runPersistentPostMaintenance(cmd *cobra.Command) error {
	if commandDidWrite.Load() && !isCommandDidExplicitDoltCommit() {
		if err := runPostRunAutoCommit(getRootContext(), doltAutoCommitParams{Command: cmd.Name()}); err != nil {
			return HandleError("dolt auto-commit failed: %v", err)
		}
	}
	if err := runPersistentTipAutoCommit(); err != nil {
		return err
	}
	runPostRunAutoBackup(getRootContext())
	if shouldRunPostCommandAutoExport(cmd) {
		if err := runPostRunAutoExport(getRootContext(), commandAllowsEmptyAutoExport(cmd)); err != nil {
			return HandleError("%v", err)
		}
	}
	if !isReadOnlyCommand(cmd.Name()) {
		runPostRunAutoPush(getRootContext())
	}
	if shouldAutoPruneEventsJournal(cmd) {
		maybeAutoPruneEventsJournal(getRootContext(), beads.FindBeadsDir())
	}
	return nil
}

func runPersistentTipAutoCommit() error {
	if !isCommandDidWriteTipMetadata() || len(getCommandTipIDsShown()) == 0 {
		return nil
	}
	mode, err := getDoltAutoCommitMode()
	if err != nil {
		return HandleError("dolt tip auto-commit failed: %v", err)
	}
	if mode != doltAutoCommitOn {
		return nil
	}
	refused, err := writePersistentTipMetadata()
	if err != nil || refused {
		return err
	}
	return commitPersistentTipMetadata()
}

func writePersistentTipMetadata() (bool, error) {
	for tipID := range getCommandTipIDsShown() {
		key := fmt.Sprintf("tip_%s_last_shown", tipID)
		value := time.Now().Format(time.RFC3339)
		if err := getStore().SetLocalMetadata(getRootContext(), key, value); err != nil {
			if errors.Is(err, embeddeddolt.ErrReadOnly) {
				debug.Logf("tip auto-commit: store is read-only, skipping tip metadata: %v", err)
				return true, nil
			}
			return false, HandleError("dolt tip auto-commit failed: %v", err)
		}
	}
	return false, nil
}

func commitPersistentTipMetadata() error {
	ids := make([]string, 0, len(getCommandTipIDsShown()))
	for tipID := range getCommandTipIDsShown() {
		ids = append(ids, tipID)
	}
	msg := formatDoltAutoCommitMessage("tip", getActor(), ids)
	if err := runPostRunAutoCommit(getRootContext(), doltAutoCommitParams{Command: "tip", MessageOverride: msg}); err != nil {
		return HandleError("dolt tip auto-commit failed: %v", err)
	}
	return nil
}

func shutdownPersistentTelemetryAndProfiles() {
	if span := currentCommandSpan(); span != nil {
		span.End()
		setCommandSpan(nil)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	telemetry.Shutdown(shutdownCtx)
	shutdownCancel()

	if getProfileFile() != nil {
		pprof.StopCPUProfile()
		_ = getProfileFile().Close()
	}
	if getTraceFile() != nil {
		trace.Stop()
		_ = getTraceFile().Close()
	}
	writePersistentHeapProfile()
	writePersistentMemStats()
}

func writePersistentHeapProfile() {
	heapDest := getMemProfilePath()
	if heapDest == "" {
		heapDest = os.Getenv("BEADS_MEM_PROFILE")
	}
	if heapDest == "" {
		return
	}
	if os.Getenv("BEADS_MEM_PROFILE_NOGC") == "" {
		runtime.GC()
	}
	if f, err := os.Create(heapDest); err == nil { // #nosec G304 -- user-supplied profiling path
		_ = pprof.WriteHeapProfile(f)
		_ = f.Close()
	}
}

func writePersistentMemStats() {
	statsDest := os.Getenv("BEADS_MEM_STATS")
	if statsDest == "" {
		return
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if f, err := os.Create(statsDest); err == nil { // #nosec G304 -- user-supplied profiling path
		fmt.Fprintf(f, "HeapAlloc=%d HeapSys=%d HeapInuse=%d HeapObjects=%d\n",
			ms.HeapAlloc, ms.HeapSys, ms.HeapInuse, ms.HeapObjects)
		_ = f.Close()
	}
}

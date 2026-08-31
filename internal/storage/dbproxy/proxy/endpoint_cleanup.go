package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/atomicfile"
	"github.com/jonbaldie/beads/internal/lockfile"
	"github.com/jonbaldie/beads/internal/procid"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/pidfile"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/util"
)

type unverifiableProcessChecks struct {
	LiveEstablished bool
	LegacyProxy     bool
}

func unverifiableProcessError(
	operation string,
	recordPath string,
	pid int,
	cause error,
	checks unverifiableProcessChecks,
) error {
	liveness := ""
	if checks.LiveEstablished {
		liveness = " live"
	}
	stopGuidance := "stop the recorded process with the binary that started it"
	if checks.LegacyProxy && checks.LiveEstablished {
		stopGuidance = "stop the pre-upgrade proxy with the old bd binary"
	}
	return &unverifiableLifecycleError{message: fmt.Sprintf(
		"%s refused for unverifiable%s process pid %d recorded at %s: %v; %s, then quarantine the record manually by renaming %s to %s.stale-<unix-timestamp> before retrying",
		operation,
		liveness,
		pid,
		recordPath,
		cause,
		stopGuidance,
		recordPath,
		recordPath,
	)}
}

func probeUnverifiablePID(pid int) (dead bool, live bool, err error) {
	if pid <= 0 {
		return false, false, errors.New("record has no valid pid")
	}
	_, err = procid.Capture(pid)
	if err == nil {
		return false, true, nil
	}
	if procid.IsProcessGone(err) {
		return true, false, nil
	}
	return false, false, err
}

func openRecordedProcess(pf *pidfile.PidFile) (*procid.Handle, bool, error) {
	handle, err := procid.Open(pf.Pid, procid.Token(pf.Birth))
	if err == nil {
		return handle, false, nil
	}
	if procid.IsProcessGone(err) {
		return nil, true, nil
	}

	current, captureErr := procid.Capture(pf.Pid)
	if captureErr != nil {
		if procid.IsProcessGone(captureErr) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("open process identity: %w (recheck: %v)", err, captureErr)
	}
	if current != procid.Token(pf.Birth) {
		// A different birth token proves the recorded process exited, even
		// when the numeric PID has since been recycled.
		return nil, true, nil
	}
	return nil, false, fmt.Errorf("open verified process handle: %w", err)
}

func waitForRecordedProcessExit(pf *pidfile.PidFile, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		matched, err := procid.Verify(pf.Pid, procid.Token(pf.Birth))
		if err != nil {
			return err
		}
		if !matched {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout (%s) waiting for process exit", timeout)
		}
		time.Sleep(shutdownConfirmPoll)
	}
}

func killSpawnedChild(child *spawnedProxyChild) error {
	select {
	case <-child.done:
		return nil
	default:
	}
	if child.handle == nil {
		// No verified handle could be opened at spawn time. The child is our
		// own un-reaped process, so its PID cannot have been recycled and a
		// direct kill is safe; the alternative is leaking a live proxy child.
		if err := child.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("kill spawned proxy child pid %d: %w", child.cmd.Process.Pid, err)
		}
	} else if err := child.handle.Kill(); err != nil {
		return err
	}
	timer := time.NewTimer(shutdownConfirmDeadline)
	defer timer.Stop()
	select {
	case <-child.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("timeout (%s) waiting for pid %d", shutdownConfirmDeadline, child.cmd.Process.Pid)
	}
}

func readStopEpoch(rootDir string) (string, error) {
	path := filepath.Join(rootDir, stopEpochFileName)
	data, err := os.ReadFile(path) // #nosec G304 - path is a fixed control filename under the workspace proxy root
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func advanceStopEpoch(rootDir string) error {
	epoch := strconv.FormatInt(time.Now().UnixNano(), 10) +
		"-" + strconv.Itoa(os.Getpid()) +
		"-" + strconv.FormatUint(stopEpochSequence.Add(1), 10)
	if err := atomicfile.WriteFile(filepath.Join(rootDir, stopEpochFileName), []byte(epoch+"\n"), 0o600); err != nil {
		return err
	}
	return nil
}

func stopEpochChanged(rootDir, expected string) (bool, error) {
	current, err := readStopEpoch(rootDir)
	if err != nil {
		return false, fmt.Errorf("read proxy stop epoch: %w", err)
	}
	return current != expected, nil
}

func writeSpawnMarker(rootDir string, marker spawnMarker) error {
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if err := atomicfile.WriteFile(filepath.Join(rootDir, spawnMarkerFileName), data, 0o600); err != nil {
		return fmt.Errorf("write spawn marker: %w", err)
	}
	return nil
}

func readSpawnMarker(rootDir string) (*spawnMarker, error) {
	path := filepath.Join(rootDir, spawnMarkerFileName)
	data, err := os.ReadFile(path) // #nosec G304 - path is a fixed control filename under the workspace proxy root
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var marker spawnMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if marker.Schema != 1 || marker.PID <= 0 || marker.Birth == "" {
		return nil, fmt.Errorf("invalid spawn marker %s", path)
	}
	return &marker, nil
}

func inspectSpawnMarkerLocked(rootDir string) (bool, error) {
	marker, err := readSpawnMarker(rootDir)
	if err != nil {
		return false, fmt.Errorf("inspect proxy spawn marker: %w", err)
	}
	if marker == nil {
		return false, nil
	}
	matched, err := procid.Verify(marker.PID, procid.Token(marker.Birth))
	if err != nil {
		return false, fmt.Errorf("verify proxy spawn owner pid %d: %w", marker.PID, err)
	}
	if matched {
		return true, nil
	}
	if err := os.Remove(filepath.Join(rootDir, spawnMarkerFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("remove dead spawn marker: %w", err)
	}
	return false, nil
}

func clearSpawnMarkerAfterLock(rootDir string) error {
	err := os.Remove(filepath.Join(rootDir, spawnMarkerFileName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func tryClearOwnSpawnMarker(rootDir string, own spawnMarker) (bool, error) {
	marker, err := readSpawnMarker(rootDir)
	if err != nil || marker == nil {
		return true, err
	}
	if *marker != own {
		return true, nil
	}

	lock, err := util.TryLock(filepath.Join(rootDir, LockFileName))
	if err == nil {
		current, readErr := readSpawnMarker(rootDir)
		if readErr == nil && current != nil && *current == own {
			readErr = clearSpawnMarkerAfterLock(rootDir)
		}
		lock.Unlock()
		return true, readErr
	}
	if !lockfile.IsLocked(err) {
		return true, err
	}
	return false, nil
}

func clearOwnSpawnMarker(rootDir string, own spawnMarker) error {
	deadline := time.Now().Add(shutdownConfirmDeadline)
	for {
		done, err := tryClearOwnSpawnMarker(rootDir, own)
		if done {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout clearing spawn marker %s", filepath.Join(rootDir, spawnMarkerFileName))
		}
		time.Sleep(shutdownConfirmPoll)
	}
}

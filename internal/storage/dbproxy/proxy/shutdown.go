package proxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jonbaldie/beads/internal/lockfile"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/identity"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/pidfile"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/server"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/util"
)

const (
	shutdownConfirmDeadline = 5 * time.Second
	shutdownConfirmPoll     = 50 * time.Millisecond
	shutdownPostKillMinimum = 2 * time.Second
)

type killRecordChecks struct {
	VerifyRoot       bool
	CheckSpawnMarker bool
}

type shutdownError struct {
	proxyErr   error
	backendErr error
}

func (e *shutdownError) Error() string {
	switch {
	case e.proxyErr == nil:
		return fmt.Sprintf("proxy.Shutdown partial: proxy stopped; backend left running: %v", e.backendErr)
	case e.backendErr == nil:
		return fmt.Sprintf("proxy.Shutdown partial: backend stopped; proxy left running: %v", e.proxyErr)
	default:
		return fmt.Sprintf(
			"proxy.Shutdown failed: proxy left running: %v; backend left running: %v",
			e.proxyErr,
			e.backendErr,
		)
	}
}

func (e *shutdownError) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.proxyErr != nil {
		errs = append(errs, e.proxyErr)
	}
	if e.backendErr != nil {
		errs = append(errs, e.backendErr)
	}
	return errs
}

// CanForceStopUnverified reports whether err is a Shutdown refusal whose
// remaining processes are all unverifiable. ForceStopUnverified covers both
// the proxy and backend records, and a pre-v2 managed-local deployment leaves
// BOTH as unverifiable legacy records, so either side (or both) may carry the
// refusal. It must not be used for unrelated shutdown failures.
func CanForceStopUnverified(err error) bool {
	var shutdownErr *shutdownError
	if !errors.As(err, &shutdownErr) {
		return false
	}
	proxyRecoverable := shutdownErr.proxyErr == nil ||
		errors.Is(shutdownErr.proxyErr, ErrUnverifiableProcess)
	backendRecoverable := shutdownErr.backendErr == nil ||
		errors.Is(shutdownErr.backendErr, ErrUnverifiableProcess)
	return proxyRecoverable && backendRecoverable
}

// Shutdown stops the verified proxy and backend processes for rootDir.
//
// Advancing the stop epoch first makes every start attempt which began before
// this call terminally abort instead of retrying after its child is stopped.
// The proxy spawn marker covers the smaller release-lock-before-exec window:
// Shutdown waits for that marked attempt to either acquire proxy.lock or fail,
// then verifies and stops whatever it published. All waits are bounded by
// shutdownConfirmDeadline.
func Shutdown(rootDir string) error {
	if err := advanceStopEpoch(rootDir); err != nil {
		return fmt.Errorf("proxy.Shutdown: publish stop epoch: %w", err)
	}

	proxyLock, proxyErr := stopAndAcquire(
		rootDir,
		LockFileName,
		PIDFileName,
		pidfile.KindProxy,
		killRecordChecks{
			VerifyRoot:       true,
			CheckSpawnMarker: true,
		},
	)
	if proxyLock != nil {
		defer proxyLock.Unlock()
	}

	backendLock, backendErr := stopAndAcquire(
		rootDir,
		server.LockFileName,
		server.PIDFileName,
		pidfile.KindDoltBackend,
		killRecordChecks{VerifyRoot: true},
	)
	if backendLock != nil {
		backendLock.Unlock()
	}

	switch {
	case proxyErr == nil && backendErr == nil:
		return nil
	default:
		return &shutdownError{proxyErr: proxyErr, backendErr: backendErr}
	}
}

func ControlFilePaths(rootDir string) []string {
	paths := []string{
		filepath.Join(rootDir, PIDFileName),
		filepath.Join(rootDir, LockFileName),
		filepath.Join(rootDir, LogFileName),
		filepath.Join(rootDir, identity.SecretFileName),
		filepath.Join(rootDir, spawnMarkerFileName),
		filepath.Join(rootDir, stopEpochFileName),
		filepath.Join(rootDir, server.PIDFileName),
		filepath.Join(rootDir, server.LockFileName),
	}
	for _, name := range []string{PIDFileName, server.PIDFileName} {
		matches, err := filepath.Glob(filepath.Join(rootDir, name+".stale-*"))
		if err == nil {
			paths = append(paths, matches...)
		}
	}
	return paths
}

func PurgeControlFiles(rootDir string) []error {
	var errs []error
	for _, path := range ControlFilePaths(rootDir) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errs
}

// stopAndAcquire returns with lockName held after the recorded process is
// absent. Holding proxy.lock across backend cleanup prevents a fresh start
// from slipping between the two shutdown phases.
func stopAndAcquire(
	rootDir string,
	lockName string,
	pidName string,
	wantKind string,
	checks killRecordChecks,
) (*util.Lock, error) {
	lockPath := filepath.Join(rootDir, lockName)
	recordPath := pidfile.Path(rootDir, pidName)
	deadline := time.Now().Add(shutdownConfirmDeadline)
	var stopped *pidfile.PidFile

	for {
		lock, err := util.TryLock(lockPath)
		switch {
		case err == nil:
			attempt, attemptErr := stopWithLock(rootDir, pidName, wantKind, checks, lock, recordPath, deadline, stopped)
			if attemptErr != nil {
				return nil, attemptErr
			}
			if attempt.retry {
				stopped = attempt.stopped
				continue
			}
			return attempt.lock, nil

		case !lockfile.IsLocked(err):
			return nil, fmt.Errorf("probe %s: %w", lockPath, err)
		}

		held, heldErr := inspectHeldLock(rootDir, pidName, wantKind, checks, recordPath)
		if heldErr != nil {
			return nil, heldErr
		}
		if held.stopped != nil {
			stopped = held.stopped
		}

		if time.Now().After(deadline) {
			return nil, shutdownAcquireTimeout(lockPath, recordPath, held)
		}
		time.Sleep(shutdownConfirmPoll)
	}
}

type heldStop struct {
	record  *pidfile.PidFile
	stopped *pidfile.PidFile
}

func shutdownAcquireTimeout(lockPath, recordPath string, held heldStop) error {
	pid := 0
	if held.record != nil {
		pid = held.record.Pid
	}
	return fmt.Errorf(
		"timeout (%s) acquiring %s after inspecting pid %d at %s; stop the lock owner with the binary that started it, then retry",
		shutdownConfirmDeadline,
		lockPath,
		pid,
		recordPath,
	)
}

func inspectHeldLock(
	rootDir string,
	pidName string,
	wantKind string,
	checks killRecordChecks,
	recordPath string,
) (heldStop, error) {
	pf, readErr := pidfile.Read(rootDir, pidName)
	if readErr != nil {
		if isMalformedPIDFileError(readErr) {
			return heldStop{}, unverifiableProcessError(
				"shutdown", recordPath, 0, readErr, unverifiableProcessChecks{},
			)
		}
		return heldStop{}, fmt.Errorf("read held-lock record %s: %w", recordPath, readErr)
	}
	if pf == nil {
		return heldStop{}, nil
	}
	if validateErr := validateKillRecord(rootDir, pf, wantKind, checks); validateErr != nil {
		return inspectHeldInvalid(recordPath, pf, wantKind, validateErr)
	}
	return inspectHeldVerified(pf, recordPath)
}

func inspectHeldInvalid(recordPath string, pf *pidfile.PidFile, wantKind string, validateErr error) (heldStop, error) {
	dead, live, probeErr := classifyInvalidKillRecord(pf, validateErr)
	if dead {
		// A dead malformed or legacy record can only be quarantined after the
		// held lock becomes available, so continue polling.
		return heldStop{}, nil
	}
	if probeErr != nil {
		validateErr = errors.Join(validateErr, fmt.Errorf("probe recorded pid: %w", probeErr))
	}
	return heldStop{}, unverifiableProcessError(
		"shutdown", recordPath, pf.Pid, validateErr,
		unverifiableProcessChecks{
			LiveEstablished: live,
			LegacyProxy:     live && wantKind == pidfile.KindProxy && errors.Is(validateErr, pidfile.ErrLegacySchema),
		},
	)
}

func inspectHeldVerified(pf *pidfile.PidFile, recordPath string) (heldStop, error) {
	handle, dead, openErr := openRecordedProcess(pf)
	if openErr != nil {
		return heldStop{}, unverifiableProcessError(
			"shutdown", recordPath, pf.Pid, openErr, unverifiableProcessChecks{},
		)
	}
	if dead {
		return heldStop{record: pf}, nil
	}
	if killErr := handle.Kill(); killErr != nil {
		_ = handle.Close()
		return heldStop{}, fmt.Errorf("kill verified pid %d from %s: %w", pf.Pid, recordPath, killErr)
	}
	if closeErr := handle.Close(); closeErr != nil {
		return heldStop{}, fmt.Errorf("close verified process handle for pid %d: %w", pf.Pid, closeErr)
	}
	return heldStop{record: pf, stopped: pf}, nil
}

type stopAttempt struct {
	lock    *util.Lock
	stopped *pidfile.PidFile
	retry   bool
}

func stopWithLock(
	rootDir string,
	pidName string,
	wantKind string,
	checks killRecordChecks,
	lock *util.Lock,
	recordPath string,
	deadline time.Time,
	stopped *pidfile.PidFile,
) (stopAttempt, error) {
	if checks.CheckSpawnMarker {
		attempt, handled, markerErr := handleSpawnMarker(rootDir, lock, deadline, stopped)
		if handled {
			return attempt, markerErr
		}
	}

	pf, readErr := readStopRecordWithLock(rootDir, pidName, recordPath, lock)
	if readErr != nil {
		return stopAttempt{}, readErr
	}
	if pf == nil {
		return stopAttempt{lock: lock}, nil
	}
	if stopped != nil && *pf == *stopped {
		if removeErr := pidfile.Remove(rootDir, pidName); removeErr != nil {
			lock.Unlock()
			return stopAttempt{}, fmt.Errorf("remove stopped process record %s: %w", recordPath, removeErr)
		}
		return stopAttempt{lock: lock}, nil
	}
	if validateErr := validateKillRecord(rootDir, pf, wantKind, checks); validateErr != nil {
		return stopInvalidRecordWithLock(rootDir, pidName, recordPath, pf, wantKind, lock, validateErr)
	}
	return stopVerifiedRecordWithLock(rootDir, pidName, recordPath, pf, lock, deadline)
}

func readStopRecordWithLock(rootDir, pidName, recordPath string, lock *util.Lock) (*pidfile.PidFile, error) {
	pf, readErr := pidfile.Read(rootDir, pidName)
	if readErr == nil {
		return pf, nil
	}
	lock.Unlock()
	if isMalformedPIDFileError(readErr) {
		return nil, unverifiableProcessError(
			"shutdown", recordPath, 0, readErr, unverifiableProcessChecks{},
		)
	}
	return nil, fmt.Errorf("read %s: %w", recordPath, readErr)
}

func handleSpawnMarker(rootDir string, lock *util.Lock, deadline time.Time, stopped *pidfile.PidFile) (stopAttempt, bool, error) {
	active, markerErr := inspectSpawnMarkerLocked(rootDir)
	if markerErr != nil {
		lock.Unlock()
		return stopAttempt{}, true, markerErr
	}
	if !active {
		return stopAttempt{}, false, nil
	}
	lock.Unlock()
	if time.Now().After(deadline) {
		return stopAttempt{}, true, fmt.Errorf(
			"timeout (%s) waiting for spawn marker %s; wait for the in-progress start to finish, then retry",
			shutdownConfirmDeadline,
			filepath.Join(rootDir, spawnMarkerFileName),
		)
	}
	time.Sleep(shutdownConfirmPoll)
	return stopAttempt{stopped: stopped, retry: true}, true, nil
}

func stopVerifiedRecordWithLock(
	rootDir string,
	pidName string,
	recordPath string,
	pf *pidfile.PidFile,
	lock *util.Lock,
	deadline time.Time,
) (stopAttempt, error) {
	handle, dead, openErr := openRecordedProcess(pf)
	if openErr != nil {
		lock.Unlock()
		return stopAttempt{}, unverifiableProcessError(
			"shutdown", recordPath, pf.Pid, openErr, unverifiableProcessChecks{},
		)
	}
	if dead {
		if _, quarantineErr := quarantineRecord(rootDir, pidName, time.Now()); quarantineErr != nil {
			lock.Unlock()
			return stopAttempt{}, fmt.Errorf("quarantine dead process record %s: %w", recordPath, quarantineErr)
		}
		return stopAttempt{lock: lock}, nil
	}
	if killErr := handle.Kill(); killErr != nil {
		_ = handle.Close()
		lock.Unlock()
		return stopAttempt{}, fmt.Errorf("kill verified pid %d from %s: %w", pf.Pid, recordPath, killErr)
	}
	if closeErr := handle.Close(); closeErr != nil {
		lock.Unlock()
		return stopAttempt{}, fmt.Errorf("close verified process handle for pid %d: %w", pf.Pid, closeErr)
	}
	waitBudget := max(time.Until(deadline), shutdownPostKillMinimum)
	if waitErr := waitForRecordedProcessExit(pf, waitBudget); waitErr != nil {
		lock.Unlock()
		return stopAttempt{}, fmt.Errorf("confirm verified pid %d stopped: %w", pf.Pid, waitErr)
	}
	if removeErr := pidfile.Remove(rootDir, pidName); removeErr != nil {
		lock.Unlock()
		return stopAttempt{}, fmt.Errorf("remove stopped process record %s: %w", recordPath, removeErr)
	}
	return stopAttempt{lock: lock}, nil
}

func stopInvalidRecordWithLock(
	rootDir string,
	pidName string,
	recordPath string,
	pf *pidfile.PidFile,
	wantKind string,
	lock *util.Lock,
	validateErr error,
) (stopAttempt, error) {
	dead, live, probeErr := classifyInvalidKillRecord(pf, validateErr)
	if dead {
		if _, quarantineErr := quarantineRecord(rootDir, pidName, time.Now()); quarantineErr != nil {
			lock.Unlock()
			return stopAttempt{}, fmt.Errorf("quarantine dead unverifiable process record %s: %w", recordPath, quarantineErr)
		}
		return stopAttempt{lock: lock}, nil
	}
	if probeErr != nil {
		validateErr = errors.Join(validateErr, fmt.Errorf("probe recorded pid: %w", probeErr))
	}
	lock.Unlock()
	return stopAttempt{}, unverifiableProcessError(
		"shutdown",
		recordPath,
		pf.Pid,
		validateErr,
		unverifiableProcessChecks{
			LiveEstablished: live,
			LegacyProxy: live &&
				wantKind == pidfile.KindProxy &&
				errors.Is(validateErr, pidfile.ErrLegacySchema),
		},
	)
}

func validateKillRecord(
	rootDir string,
	pf *pidfile.PidFile,
	wantKind string,
	checks killRecordChecks,
) error {
	if err := pf.ValidateV2(wantKind); err != nil {
		return err
	}
	if !checks.VerifyRoot {
		return nil
	}
	rootID, err := identity.RootID(rootDir)
	if err != nil {
		return fmt.Errorf("resolve workspace identity: %w", err)
	}
	if pf.RootID != rootID {
		return fmt.Errorf("root identity mismatch (record has %q, workspace has %q)", pf.RootID, rootID)
	}
	return nil
}

func classifyInvalidKillRecord(pf *pidfile.PidFile, validationErr error) (dead bool, live bool, err error) {
	if !isPIDFileValidationError(validationErr) {
		return false, false, nil
	}
	return probeUnverifiablePID(pf.Pid)
}

func isPIDFileValidationError(err error) bool {
	return errors.Is(err, pidfile.ErrLegacySchema) ||
		errors.Is(err, pidfile.ErrBadPid) ||
		errors.Is(err, pidfile.ErrBadPort) ||
		errors.Is(err, pidfile.ErrKindMismatch) ||
		errors.Is(err, pidfile.ErrMissingBirth)
}

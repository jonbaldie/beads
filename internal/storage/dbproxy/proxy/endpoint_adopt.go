package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/lockfile"
	"github.com/jonbaldie/beads/internal/procid"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/identity"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/pidfile"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/server"
	"github.com/jonbaldie/beads/internal/storage/dbproxy/util"
)

func IsRunning(rootDir string) (bool, int) {
	discovery := readAndDial(rootDir)
	if discovery.status != adoptionAdopted {
		return false, 0
	}
	return true, discovery.pidfile.Pid
}

func readProxyRecord(rootDir string) (*pidfile.PidFile, adoptionResult, bool) {
	pf, err := pidfile.Read(rootDir, PIDFileName)
	if err != nil {
		if isMalformedPIDFileError(err) {
			return nil, adoptionResult{status: adoptionMalformed, err: err}, false
		}
		return nil, adoptionResult{status: adoptionIOErr, err: err}, false
	}
	if pf == nil {
		return nil, adoptionResult{status: adoptionNoRecord}, false
	}
	if err := pf.ValidateV2(pidfile.KindProxy); err != nil {
		if errors.Is(err, pidfile.ErrLegacySchema) {
			return pf, adoptionResult{status: adoptionLegacy, pidfile: pf, err: err}, false
		}
		return pf, adoptionResult{status: adoptionMalformed, pidfile: pf, err: err}, false
	}
	return pf, adoptionResult{}, true
}

func proxyReplyMatchesWorkspace(reply *identity.IdentReply, pf *pidfile.PidFile, expectedRootID string) bool {
	return reply.RootID == expectedRootID && reply.RootID == pf.RootID
}

func proxyReplyMatchesProcess(reply *identity.IdentReply, pf *pidfile.PidFile) bool {
	return reply.PID == pf.Pid && reply.Birth == pf.Birth && reply.UpstreamID == pf.UpstreamID
}

func proxyReplyMatchesPorts(reply *identity.IdentReply, pf *pidfile.PidFile) bool {
	return reply.DataPort == pf.Port && reply.ControlPort == pf.ControlPort
}

func proxyReplyMatchesRecord(reply *identity.IdentReply, pf *pidfile.PidFile, expectedRootID string) bool {
	if reply.Schema < pidfile.SchemaV2 || reply.Role != pidfile.KindProxy {
		return false
	}
	return proxyReplyMatchesWorkspace(reply, pf, expectedRootID) &&
		proxyReplyMatchesProcess(reply, pf) &&
		proxyReplyMatchesPorts(reply, pf)
}

func authenticateProxyRecord(rootDir string, pf *pidfile.PidFile) adoptionResult {
	expectedRootID, err := resolveRootIdentity(rootDir)
	if err != nil {
		return adoptionResult{status: adoptionUnverifiable, pidfile: pf, err: err}
	}
	secret, err := readControlSecret(rootDir)
	if err != nil {
		return adoptionResult{status: adoptionUnverifiable, pidfile: pf, err: err}
	}
	reply, err := identity.Identify("127.0.0.1", pf.ControlPort, secret, identityProbeTimeout)
	if err != nil {
		return adoptionResult{status: adoptionIdentityMismatch, pidfile: pf, err: err}
	}
	if !proxyReplyMatchesRecord(reply, pf, expectedRootID) {
		return adoptionResult{
			status:  adoptionIdentityMismatch,
			pidfile: pf,
			err:     errors.New("authenticated proxy identity does not match its pidfile or workspace"),
		}
	}
	ep := Endpoint{Host: "127.0.0.1", Port: pf.Port}
	if !probePort(ep, identityProbeTimeout) {
		return adoptionResult{
			status:  adoptionIdentityMismatch,
			pidfile: pf,
			err:     fmt.Errorf("authenticated proxy data port %d is not accepting connections", pf.Port),
		}
	}
	return adoptionResult{status: adoptionAdopted, endpoint: ep, pidfile: pf}
}

func readAndDial(rootDir string) adoptionResult {
	pf, result, ok := readProxyRecord(rootDir)
	if !ok {
		return result
	}
	matched, err := verifyProcessIdentity(pf.Pid, procid.Token(pf.Birth))
	if err != nil {
		// Discovery must not turn an identity-probe failure into a fatal
		// pidfile I/O error. In particular, Windows ERROR_ACCESS_DENIED on a
		// recycled PID belongs in this non-adopting path; proxy.lock still
		// gates quarantine and replacement.
		return adoptionResult{
			status:  adoptionUnverifiable,
			pidfile: pf,
			err:     fmt.Errorf("verify proxy pid %d: %w", pf.Pid, err),
		}
	}
	if !matched {
		return adoptionResult{status: adoptionStaleDead, pidfile: pf}
	}
	return authenticateProxyRecord(rootDir, pf)
}

func probePort(ep Endpoint, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", ep.Address(), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func isMalformedPIDFileError(err error) bool {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &syntaxErr) || errors.As(err, &typeErr)
}

func quarantineForSpawn(rootDir string, discovery adoptionResult) error {
	switch discovery.status {
	case adoptionNoRecord:
		return nil
	case adoptionStaleDead:
		log.Printf("dbproxy: quarantining stale dead proxy record %s before restart", pidfile.Path(rootDir, PIDFileName))
	case adoptionIdentityMismatch:
		log.Printf(
			"dbproxy: refusing proxy identity at %s (%v); quarantining only its record and starting a fresh proxy",
			pidfile.Path(rootDir, PIDFileName), discovery.err,
		)
	case adoptionUnverifiable:
		log.Printf(
			"dbproxy: proxy identity at %s could not be verified (%v); quarantining only its record under proxy.lock and starting a fresh proxy",
			pidfile.Path(rootDir, PIDFileName), discovery.err,
		)
	case adoptionLegacy:
		log.Printf(
			"dbproxy: quarantining legacy proxy record %s; a pre-upgrade proxy may still be running and must be stopped with the old bd binary if it does not idle-exit",
			pidfile.Path(rootDir, PIDFileName),
		)
	case adoptionMalformed:
		log.Printf("dbproxy: quarantining malformed proxy record %s (%v) before restart", pidfile.Path(rootDir, PIDFileName), discovery.err)
	default:
		return fmt.Errorf("cannot spawn from proxy discovery status %s", discovery.status)
	}
	target, err := quarantineRecord(rootDir, PIDFileName, time.Now())
	if err != nil {
		return fmt.Errorf("quarantine proxy record: %w", err)
	}
	log.Printf("dbproxy: preserved proxy record as %s", target)
	return nil
}

func quarantineRecord(rootDir, name string, now time.Time) (string, error) {
	source := pidfile.Path(rootDir, name)
	for stamp := now.Unix(); ; stamp++ {
		target := filepath.Join(rootDir, name+".stale-"+strconv.FormatInt(stamp, 10))
		if _, err := os.Lstat(target); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("inspect quarantine target %s: %w", target, err)
		}
		if err := os.Rename(source, target); err != nil {
			return "", fmt.Errorf("rename %s to %s: %w", source, target, err)
		}
		return target, nil
	}
}

func quarantineTimestamp(name string) (int64, bool) {
	prefixes := []string{
		PIDFileName + ".stale-",
		server.PIDFileName + ".stale-",
	}
	for _, prefix := range prefixes {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		stamp, err := strconv.ParseInt(strings.TrimPrefix(name, prefix), 10, 64)
		return stamp, err == nil
	}
	return 0, false
}

func sweepOldQuarantines(rootDir string, now time.Time) error {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return err
	}
	cutoff := now.Add(-quarantineRetention).Unix()
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		stamp, ok := quarantineTimestamp(entry.Name())
		if !ok || stamp >= cutoff {
			continue
		}
		if err := os.Remove(filepath.Join(rootDir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove expired quarantine %s: %w", entry.Name(), err))
		}
	}
	return errors.Join(errs...)
}

func readBackendRecordForCleanup(rootDir string) (string, *pidfile.PidFile, error) {
	recordPath := pidfile.Path(rootDir, server.PIDFileName)
	pf, readErr := pidfile.Read(rootDir, server.PIDFileName)
	childLockPath := filepath.Join(rootDir, server.LockFileName)
	childLock, lockErr := util.TryLock(childLockPath)
	switch {
	case lockErr == nil:
		childLock.Unlock()
	case lockfile.IsLocked(lockErr):
		pid := 0
		if pf != nil {
			pid = pf.Pid
		}
		return recordPath, nil, fmt.Errorf(
			"refusing backend cleanup: %s is held while proxy lock is free (recorded pid %d at %s); stop the process holding the child lock or remove the stale lock owner before retrying",
			childLockPath, pid, recordPath,
		)
	default:
		return recordPath, nil, fmt.Errorf("probe backend lock %s: %w", childLockPath, lockErr)
	}

	if readErr != nil {
		if isMalformedPIDFileError(readErr) {
			return recordPath, nil, unverifiableProcessError("backend cleanup", recordPath, 0, readErr, unverifiableProcessChecks{})
		}
		return recordPath, nil, fmt.Errorf("read backend record %s: %w", recordPath, readErr)
	}
	return recordPath, pf, nil
}

func cleanupUnverifiableBackendRecord(rootDir, recordPath string, pf *pidfile.PidFile, validationErr error) error {
	dead, live, probeErr := probeUnverifiablePID(pf.Pid)
	if dead {
		if _, quarantineErr := quarantineRecord(rootDir, server.PIDFileName, time.Now()); quarantineErr != nil {
			return fmt.Errorf("quarantine dead unverifiable backend record: %w", quarantineErr)
		}
		return nil
	}
	if probeErr != nil {
		validationErr = errors.Join(validationErr, fmt.Errorf("probe recorded pid: %w", probeErr))
	}
	return unverifiableProcessError(
		"backend cleanup",
		recordPath,
		pf.Pid,
		validationErr,
		unverifiableProcessChecks{LiveEstablished: live},
	)
}

func terminateVerifiedOrphanBackend(rootDir, recordPath string, pf *pidfile.PidFile, handle *procid.Handle) error {
	if err := handle.Kill(); err != nil {
		_ = handle.Close()
		return fmt.Errorf("kill verified orphan backend pid %d from %s: %w", pf.Pid, recordPath, err)
	}
	// Close before waiting: an open handle on Windows keeps the dead PID
	// allocated, so procid.Verify would keep matching it and the exit wait
	// below would always time out.
	if err := handle.Close(); err != nil {
		return fmt.Errorf("close verified backend process handle for pid %d: %w", pf.Pid, err)
	}
	if err := waitForRecordedProcessExit(pf, backendExitTimeout); err != nil {
		return fmt.Errorf("wait for verified orphan backend pid %d: %w", pf.Pid, err)
	}
	if _, err := quarantineRecord(rootDir, server.PIDFileName, time.Now()); err != nil {
		return fmt.Errorf("quarantine orphan backend record: %w", err)
	}
	return nil
}

func cleanupVerifiedOrphanBackend(rootDir, recordPath string, pf *pidfile.PidFile) error {
	expectedRootID, err := identity.RootID(rootDir)
	if err != nil {
		return fmt.Errorf("resolve backend root identity: %w", err)
	}
	if pf.RootID != expectedRootID {
		return unverifiableProcessError(
			"backend cleanup",
			recordPath,
			pf.Pid,
			fmt.Errorf("root identity mismatch (record has %q, workspace has %q)", pf.RootID, expectedRootID),
			unverifiableProcessChecks{},
		)
	}

	handle, dead, err := openRecordedProcess(pf)
	if err != nil {
		return unverifiableProcessError("backend cleanup", recordPath, pf.Pid, err, unverifiableProcessChecks{})
	}
	if dead {
		if _, err := quarantineRecord(rootDir, server.PIDFileName, time.Now()); err != nil {
			return fmt.Errorf("quarantine dead backend record: %w", err)
		}
		return nil
	}
	return terminateVerifiedOrphanBackend(rootDir, recordPath, pf, handle)
}

func cleanupOrphanBackend(rootDir string) error {
	recordPath, pf, err := readBackendRecordForCleanup(rootDir)
	if err != nil {
		return err
	}
	if pf == nil {
		return nil
	}
	if err := pf.ValidateV2(pidfile.KindDoltBackend); err != nil {
		return cleanupUnverifiableBackendRecord(rootDir, recordPath, pf, err)
	}
	return cleanupVerifiedOrphanBackend(rootDir, recordPath, pf)
}

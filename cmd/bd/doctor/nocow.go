package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// CheckBtrfsNoCOW verifies that FS_NOCOW_FL is set on the dolt data
// directory under .beads/ when running on Linux btrfs. Without this flag,
// dolt's append-only write path triggers kworker thrashing because every
// small append forces btrfs to read-modify-write-recompress an existing
// compressed extent.
//
// On non-Linux platforms the check short-circuits to StatusOK because the
// flag does not exist outside Linux. On Linux but non-btrfs filesystems
// the check also returns StatusOK because the flag is a no-op there.
//
// The check reports a warning when the flag is missing on a btrfs dolt
// directory, along with a fix suggestion. `bd doctor --fix` (via
// FixBtrfsNoCOW) applies the flag but also warns that existing files inside
// need to be rewritten to pick it up.
func CheckBtrfsNoCOW(path string) DoctorCheck {
	const name = "Btrfs NoCOW (dolt)"
	if skip, ok := btrfsNoCOWSkip(path, name); ok {
		return skip
	}
	beadsDir := ResolveBeadsDirForRepo(path)
	missing, anyBtrfs, fail := scanBtrfsNoCOWTargets(name, collectBtrfsNoCOWTargets(beadsDir))
	if fail != nil {
		return *fail
	}
	return reportBtrfsNoCOW(name, missing, anyBtrfs)
}

func btrfsNoCOWSkip(path, name string) (DoctorCheck, bool) {
	if runtime.GOOS != "linux" {
		return DoctorCheck{
			Name:     name,
			Status:   StatusOK,
			Message:  "Not applicable (non-Linux platform)",
			Category: CategoryPerformance,
		}, true
	}
	beadsDir := ResolveBeadsDirForRepo(path)
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return DoctorCheck{
			Name:     name,
			Status:   StatusOK,
			Message:  "No .beads directory to check",
			Category: CategoryPerformance,
		}, true
	}
	return DoctorCheck{}, false
}

func collectBtrfsNoCOWTargets(beadsDir string) []string {
	// The dolt data directory is what actually matters for the hot write
	// path, but FS_NOCOW_FL on .beads/ itself is enough because new subdirs
	// inherit it. We check both: the ancestor (`.beads/`) is the one init
	// sets, and any existing dolt data dir is what dolt is actively writing
	// to. If either is missing the flag, warn.
	targets := []string{beadsDir}
	for _, sub := range []string{"dolt", "embeddeddolt"} {
		p := filepath.Join(beadsDir, sub)
		if _, err := os.Stat(p); err == nil {
			targets = append(targets, p)
		}
	}
	return targets
}

func scanBtrfsNoCOWTargets(name string, targets []string) (missing []string, anyBtrfs bool, fail *DoctorCheck) {
	// Only warn for paths that live on btrfs — the flag is meaningless on
	// ext4/xfs/tmpfs and reporting would just be noise.
	for _, t := range targets {
		onBtrfs, err := isBtrfs(t)
		if err != nil || !onBtrfs {
			continue
		}
		anyBtrfs = true
		set, err := hasNoCOW(t)
		if err != nil {
			// Real ioctl failure (not "unsupported"). Report as warning so
			// the user knows something is off, but don't error out.
			check := DoctorCheck{
				Name:     name,
				Status:   StatusWarning,
				Message:  fmt.Sprintf("Failed to read FS_NOCOW_FL on %s", t),
				Detail:   err.Error(),
				Category: CategoryPerformance,
			}
			return nil, true, &check
		}
		if !set {
			missing = append(missing, t)
		}
	}
	return missing, anyBtrfs, nil
}

func reportBtrfsNoCOW(name string, missing []string, anyBtrfs bool) DoctorCheck {
	if !anyBtrfs {
		return DoctorCheck{
			Name:     name,
			Status:   StatusOK,
			Message:  "Not on btrfs (no action needed)",
			Category: CategoryPerformance,
		}
	}
	if len(missing) == 0 {
		return DoctorCheck{
			Name:     name,
			Status:   StatusOK,
			Message:  "FS_NOCOW_FL set on dolt data directory",
			Category: CategoryPerformance,
		}
	}
	detail := "btrfs transparent compression causes kworker thrashing on dolt's\n" +
		"append-only write path. Affected paths:\n"
	for _, m := range missing {
		detail += "  " + m + "\n"
	}
	detail += "\nNote: setting the flag only affects newly-created files. Existing\n" +
		"files inside the directory must be rewritten (e.g. mv away and back)\n" +
		"to pick up the new flag."
	return DoctorCheck{
		Name:     name,
		Status:   StatusWarning,
		Message:  fmt.Sprintf("FS_NOCOW_FL missing on %d btrfs dolt path(s)", len(missing)),
		Detail:   detail,
		Fix:      "Run 'bd doctor --fix' to apply the flag; then 'mv .beads/dolt /tmp/d && mv /tmp/d .beads/dolt' to rewrite existing files.",
		Category: CategoryPerformance,
	}
}

// FixBtrfsNoCOW applies FS_NOCOW_FL to the .beads/ directory and to any
// existing dolt data subdirectories. Returns a human-readable summary of
// what was done, plus a warning that existing files inside still need to
// be relocated (via mv-to-tmp; mv-back) to actually pick up the new flag —
// the inode attribute only influences new files created after it is set.
//
// On non-Linux or non-btrfs this is a no-op and returns a message to that
// effect.
func FixBtrfsNoCOW(path string) (string, error) {
	beadsDir, skip, err := prepareBtrfsNoCOWFix(path)
	if err != nil {
		return "", err
	}
	if skip != "" {
		return skip, nil
	}
	applied, err := applyBtrfsNoCOWTargets(collectBtrfsNoCOWTargets(beadsDir))
	if err != nil {
		return "", err
	}
	return reportBtrfsNoCOWFix(applied), nil
}

func prepareBtrfsNoCOWFix(path string) (beadsDir, skip string, err error) {
	if runtime.GOOS != "linux" {
		return "", "FS_NOCOW_FL fix skipped: not on Linux", nil
	}
	beadsDir = ResolveBeadsDirForRepo(path)
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return "", "", fmt.Errorf(".beads directory not found at %s", beadsDir)
	}
	onBtrfs, err := isBtrfs(beadsDir)
	if err != nil {
		return "", "", fmt.Errorf("failed to statfs %s: %w", beadsDir, err)
	}
	if !onBtrfs {
		return "", "FS_NOCOW_FL fix skipped: not on btrfs", nil
	}
	return beadsDir, "", nil
}

func applyBtrfsNoCOWTargets(targets []string) ([]string, error) {
	var applied []string
	for _, t := range targets {
		if err := applyNoCOW(t); err != nil {
			return nil, fmt.Errorf("failed to set FS_NOCOW_FL on %s: %w", t, err)
		}
		applied = append(applied, t)
	}
	return applied, nil
}

func reportBtrfsNoCOWFix(applied []string) string {
	msg := fmt.Sprintf("Applied FS_NOCOW_FL to %d path(s):\n", len(applied))
	for _, a := range applied {
		msg += "  " + a + "\n"
	}
	msg += "\nWARNING: existing files inside these directories still carry the\n" +
		"old compression state. To fully benefit, relocate and restore the data:\n" +
		"  mv .beads/dolt /tmp/beads-dolt-reloc && mv /tmp/beads-dolt-reloc .beads/dolt\n" +
		"Stop the dolt server first if it is running."
	return msg
}

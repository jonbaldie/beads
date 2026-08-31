package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/cmd/bd/doctor/fix"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/storage/dolt"
)

// CheckInstallation verifies that .beads directory exists
func CheckInstallation(path string) DoctorCheck {
	beadsDir := ResolveBeadsDirForRepo(path)
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		// Auto-detect prefix from directory name
		prefix := filepath.Base(path)
		prefix = strings.TrimRight(prefix, "-")

		return DoctorCheck{
			Name:    "Installation",
			Status:  StatusError,
			Message: "No .beads/ directory found",
			Fix:     fmt.Sprintf("Run 'bd init --prefix %s' to initialize beads", prefix),
		}
	}

	return DoctorCheck{
		Name:    "Installation",
		Status:  StatusOK,
		Message: ".beads/ directory found",
	}
}

// CheckPermissions verifies that .beads directory and database are readable/writable.
// Opens its own store; prefer CheckPermissionsWithStore when a shared store is available.
func CheckPermissions(path string) DoctorCheck {
	beadsDir := ResolveBeadsDirForRepo(path)

	// Check if .beads/ is writable
	testFile := filepath.Join(beadsDir, ".doctor-test-write")
	if err := os.WriteFile(testFile, []byte("test"), 0600); err != nil {
		return DoctorCheck{
			Name:    "Permissions",
			Status:  StatusError,
			Message: ".beads/ directory is not writable",
			Fix:     "Run 'bd doctor --fix' to fix permissions",
		}
	}
	_ = os.Remove(testFile) // Clean up test file (intentionally ignore error)

	// Check Dolt database directory permissions
	cfg, err := configfile.Load(beadsDir)
	if err == nil && cfg != nil && cfg.GetBackend() == configfile.BackendDolt {
		doltPath := getDatabasePath(beadsDir)
		if info, err := os.Stat(doltPath); err == nil {
			if !info.IsDir() {
				return DoctorCheck{
					Name:    "Permissions",
					Status:  StatusError,
					Message: "dolt/ is not a directory",
					Fix:     "Run 'bd doctor --fix' to fix permissions",
				}
			}
			// Try to open Dolt store read-only to verify accessibility
			ctx := context.Background()
			store, err := dolt.NewFromConfigWithCLIOptions(ctx, beadsDir, &dolt.Config{ReadOnly: true})
			if err != nil {
				return DoctorCheck{
					Name:    "Permissions",
					Status:  StatusError,
					Message: "Dolt database exists but cannot be opened",
					Detail:  err.Error(),
					Fix:     "Run 'bd doctor --fix' to fix permissions",
				}
			}
			_ = store.Close()
		}
	}

	return DoctorCheck{
		Name:    "Permissions",
		Status:  StatusOK,
		Message: "All permissions OK",
	}
}

// CheckPermissionsWithStore verifies permissions using a shared store (GH#2636).
// If the shared store was opened successfully, the database is accessible.
func CheckPermissionsWithStore(path string, ss *SharedStore) DoctorCheck {
	beadsDir := beadsDirFromSharedStore(path, ss)
	if err := probeBeadsWritable(beadsDir); err != nil {
		return DoctorCheck{
			Name:    "Permissions",
			Status:  StatusError,
			Message: ".beads/ directory is not writable",
			Fix:     "Run 'bd doctor --fix' to fix permissions",
		}
	}
	if check, done := checkDoltPermissions(beadsDir, ss); done {
		return check
	}
	return okPermissions()
}

func probeBeadsWritable(beadsDir string) error {
	testFile := filepath.Join(beadsDir, ".doctor-test-write")
	if err := os.WriteFile(testFile, []byte("test"), 0600); err != nil {
		return err
	}
	_ = os.Remove(testFile)
	return nil
}

func checkDoltPermissions(beadsDir string, ss *SharedStore) (DoctorCheck, bool) {
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil || cfg.GetBackend() != configfile.BackendDolt {
		return DoctorCheck{}, false
	}
	if cfg.IsDoltServerMode() {
		return doltServerPermissions(ss.Store()), true
	}
	return doltDirPermissions(beadsDir, ss.Store())
}

func doltServerPermissions(store *dolt.DoltStore) DoctorCheck {
	if store == nil {
		return DoctorCheck{
			Name:    "Permissions",
			Status:  StatusError,
			Message: "Unable to verify Dolt server-backed database permissions",
			Fix:     "Check 'bd dolt status' for server availability, then re-run 'bd doctor'",
		}
	}
	return okPermissions()
}

func doltDirPermissions(beadsDir string, store *dolt.DoltStore) (DoctorCheck, bool) {
	info, err := os.Stat(getDatabasePath(beadsDir))
	if err != nil {
		return DoctorCheck{}, false
	}
	if !info.IsDir() {
		return DoctorCheck{
			Name:    "Permissions",
			Status:  StatusError,
			Message: "dolt/ is not a directory",
			Fix:     "Run 'bd doctor --fix' to fix permissions",
		}, true
	}
	if store == nil {
		return DoctorCheck{
			Name:    "Permissions",
			Status:  StatusError,
			Message: "Dolt database exists but cannot be opened",
			Fix:     "Run 'bd doctor --fix' to fix permissions",
		}, true
	}
	return DoctorCheck{}, false
}

func okPermissions() DoctorCheck {
	return DoctorCheck{
		Name:    "Permissions",
		Status:  StatusOK,
		Message: "All permissions OK",
	}
}

// CheckUntrackedBeadsFiles checks for untracked .beads/*.jsonl files that should be committed.
// This check only applies to legacy (non-Dolt) backends where JSONL files are the data store.
// In sync-branch mode, JSONL files are intentionally untracked in working branches
// and only committed to the dedicated sync branch (GH#858).
func CheckUntrackedBeadsFiles(path string) DoctorCheck {
	if check, skip := skipUntrackedBeadsCheck(path); skip {
		return check
	}
	untrackedJSONL, check, ok := listUntrackedJSONL(resolvedBeadsRepoRoot(path))
	if !ok {
		return check
	}
	return reportUntrackedJSONL(untrackedJSONL)
}

func skipUntrackedBeadsCheck(path string) (DoctorCheck, bool) {
	backend, _ := getBackendAndBeadsDir(path)
	if backend == configfile.BackendDolt {
		return DoctorCheck{
			Name:    "Untracked Files",
			Status:  StatusOK,
			Message: "N/A (Dolt backend stores data on server)",
		}, true
	}
	if _, err := os.Stat(ResolveBeadsDirForRepo(path)); os.IsNotExist(err) {
		return DoctorCheck{
			Name:    "Untracked Files",
			Status:  StatusOK,
			Message: "N/A (no .beads directory)",
		}, true
	}
	if _, err := git.GetGitDir(); err != nil {
		return DoctorCheck{
			Name:    "Untracked Files",
			Status:  StatusOK,
			Message: "N/A (not a git repository)",
		}, true
	}
	return DoctorCheck{}, false
}

func listUntrackedJSONL(repoRoot string) ([]string, DoctorCheck, bool) {
	cmd := exec.Command("git", "status", "--porcelain", ".beads/")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		return nil, DoctorCheck{
			Name:    "Untracked Files",
			Status:  StatusWarning,
			Message: "Unable to check git status",
			Detail:  err.Error(),
		}, false
	}
	return parseUntrackedJSONL(string(output)), DoctorCheck{}, true
}

func parseUntrackedJSONL(output string) []string {
	var untrackedJSONL []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "?? ") {
			continue
		}
		file := strings.TrimPrefix(line, "?? ")
		if strings.HasSuffix(file, ".jsonl") {
			untrackedJSONL = append(untrackedJSONL, filepath.Base(file))
		}
	}
	return untrackedJSONL
}

func reportUntrackedJSONL(untrackedJSONL []string) DoctorCheck {
	if len(untrackedJSONL) == 0 {
		return DoctorCheck{
			Name:    "Untracked Files",
			Status:  StatusOK,
			Message: "All .beads/*.jsonl files are tracked",
		}
	}
	return DoctorCheck{
		Name:    "Untracked Files",
		Status:  StatusWarning,
		Message: fmt.Sprintf("Untracked JSONL files: %s", strings.Join(untrackedJSONL, ", ")),
		Detail:  "These files should be committed to propagate changes to other clones",
		Fix:     "Run 'bd doctor --fix' to stage and commit untracked files, or manually: git add .beads/*.jsonl && git commit",
	}
}

// FixPermissions fixes file permission issues in the .beads directory
func FixPermissions(path string) error {
	return fix.Permissions(path)
}

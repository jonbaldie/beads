package fix

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/dolt"
)

var repoFingerprintReadLine = readLineUnbuffered

// readLineUnbuffered reads a line from stdin without buffering.
// This avoids consuming input past the newline, keeping stdin available
// for any further prompts in the same session.
func readLineUnbuffered() (string, error) {
	var result []byte
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return string(result), err
		}
		if n == 1 {
			c := buf[0] // #nosec G602 -- n==1 guarantees buf has 1 byte
			if c == '\n' {
				return string(result), nil
			}
			result = append(result, c)
		}
	}
}

// updateRepoIDInProcess updates the repo_id metadata directly in the Dolt store,
// avoiding subprocess lock contention. (GH#1805)
func updateRepoIDInProcess(path string, beadsDir string, autoYes bool) error {
	ctx := context.Background()
	newRepoID, err := beads.ComputeRepoIDForPath(path)
	if err != nil {
		return fmt.Errorf("failed to compute repository ID: %w", err)
	}
	store, err := dolt.NewFromConfig(ctx, beadsDir)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer func() { _ = store.Close() }()
	oldRepoID, _ := store.GetMetadata(ctx, "repo_id")
	oldDisplay, newDisplay := repoIDDisplays(oldRepoID, newRepoID)
	proceed, err := confirmRepoIDChange(oldRepoID, newRepoID, oldDisplay, newDisplay, autoYes)
	if err != nil || !proceed {
		return err
	}
	return writeRepoID(ctx, store, newRepoID, oldDisplay, newDisplay)
}

func repoIDDisplays(oldRepoID, newRepoID string) (oldDisplay, newDisplay string) {
	oldDisplay = "none"
	if len(oldRepoID) >= 8 {
		oldDisplay = oldRepoID[:8]
	}
	newDisplay = newRepoID
	if len(newDisplay) >= 8 {
		newDisplay = newDisplay[:8]
	}
	return oldDisplay, newDisplay
}

func confirmRepoIDChange(oldRepoID, newRepoID, oldDisplay, newDisplay string, autoYes bool) (bool, error) {
	if oldRepoID == "" || oldRepoID == newRepoID || autoYes {
		return true, nil
	}
	fmt.Printf("  WARNING: Changing repository ID can break sync if other clones exist.\n\n")
	fmt.Printf("  Current repo ID: %s\n", oldDisplay)
	fmt.Printf("  New repo ID:     %s\n\n", newDisplay)
	fmt.Printf("  Continue? [y/N] ")
	response, err := repoFingerprintReadLine()
	if err != nil {
		return false, fmt.Errorf("failed to read input: %w", err)
	}
	response = strings.TrimSpace(strings.ToLower(response))
	if response != "y" && response != "yes" {
		fmt.Println("  → Canceled")
		return false, nil
	}
	return true, nil
}

func writeRepoID(ctx context.Context, store *dolt.DoltStore, newRepoID, oldDisplay, newDisplay string) error {
	if err := store.SetMetadata(ctx, "repo_id", newRepoID); err != nil {
		return fmt.Errorf("failed to update repo_id: %w", err)
	}
	fmt.Printf("  ✓ Repository ID updated (old: %s, new: %s)\n", oldDisplay, newDisplay)
	return nil
}

// RepoFingerprint fixes repo fingerprint mismatches by prompting the user
// for which action to take. This is interactive because the consequences
// differ significantly between options:
//  1. Update repo ID (if URL changed or bd upgraded)
//  2. Reinitialize database (if wrong database was copied)
//  3. Skip (do nothing)
//
// All operations are performed in-process to avoid Dolt lock contention
// that occurs when spawning bd subcommands. (GH#1805)
func RepoFingerprint(path string, autoYes bool) error {
	beadsDir, err := resolvedWorkspaceBeadsDir(path)
	if err != nil {
		return err
	}
	if autoYes {
		fmt.Println("  → Auto mode (--yes): updating repo ID in-process...")
		return updateRepoIDInProcess(path, beadsDir, true)
	}
	response, err := promptRepoFingerprintChoice()
	if err != nil {
		return err
	}
	return applyRepoFingerprintChoice(path, beadsDir, response)
}

func promptRepoFingerprintChoice() (string, error) {
	fmt.Println("\n  Repo fingerprint mismatch detected. Choose an action:")
	fmt.Println()
	fmt.Println("    [1] Update repo ID (if git remote URL changed or bd was upgraded)")
	fmt.Println("    [2] Reinitialize database (if wrong .beads was copied here)")
	fmt.Println("    [s] Skip (do nothing)")
	fmt.Println()
	fmt.Print("  Choice [1/2/s]: ")
	response, err := repoFingerprintReadLine()
	if err != nil {
		return "", fmt.Errorf("failed to read input: %w", err)
	}
	return strings.TrimSpace(strings.ToLower(response)), nil
}

func applyRepoFingerprintChoice(path, beadsDir, response string) error {
	switch response {
	case "1":
		return updateRepoIDInProcess(path, beadsDir, false)
	case "2":
		return reinitializeFingerprintDatabase(beadsDir)
	case "s", "":
		fmt.Println("  → Skipped")
		return nil
	default:
		fmt.Printf("  → Unrecognized input '%s', skipping\n", response)
		return nil
	}
}

func reinitializeFingerprintDatabase(beadsDir string) error {
	cfg, cfgErr := configfile.Load(beadsDir)
	if cfgErr != nil || cfg == nil {
		cfg = configfile.DefaultConfig()
	}
	dbPath := cfg.DatabasePath(beadsDir)
	proceed, err := confirmFingerprintReinit(dbPath)
	if err != nil || !proceed {
		return err
	}
	if err := removeFingerprintDatabase(dbPath, cfg.GetBackend() == configfile.BackendDolt); err != nil {
		return err
	}
	fmt.Println("  → Reinitializing database from JSONL...")
	ctx := context.Background()
	store, err := dolt.NewFromConfig(ctx, beadsDir)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer func() { _ = store.Close() }()
	fmt.Println("  ✓ Database reinitialized")
	return nil
}

func confirmFingerprintReinit(dbPath string) (bool, error) {
	fmt.Printf("  ⚠️  This will DELETE %s. Continue? [y/N]: ", dbPath)
	confirm, err := repoFingerprintReadLine()
	if err != nil {
		return false, fmt.Errorf("failed to read confirmation: %w", err)
	}
	confirm = strings.TrimSpace(strings.ToLower(confirm))
	if confirm != "y" && confirm != "yes" {
		fmt.Println("  → Skipped (canceled)")
		return false, nil
	}
	return true, nil
}

func removeFingerprintDatabase(dbPath string, isDolt bool) error {
	fmt.Printf("  → Removing %s...\n", dbPath)
	if isDolt {
		if err := os.RemoveAll(dbPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove Dolt database: %w", err)
		}
		return nil
	}
	if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove database: %w", err)
	}
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	return nil
}

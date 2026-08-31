package fix

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/configfile"
)

// Permissions fixes file permission issues in the .beads directory
func Permissions(path string) error {
	dirs, err := resolveWorkspaceBeadsDirs(path)
	if err != nil {
		return err
	}
	beadsDir, info, err := beadsDirForPermissions(dirs)
	if err != nil {
		return err
	}
	if beadsDir == "" {
		return nil
	}
	if err := ensurePerm(beadsDir, info, 0700, "failed to fix .beads directory permissions"); err != nil {
		return err
	}
	return fixDatabasePermissions(dirs.resolved)
}

func beadsDirForPermissions(dirs workspaceBeadsDirs) (string, os.FileInfo, error) {
	beadsDir := dirs.local
	// Use Lstat to detect symlinks - we shouldn't chmod symlinked directories
	// as this would change the target's permissions (problematic on NixOS).
	info, err := os.Lstat(beadsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", nil, fmt.Errorf("failed to stat .beads directory: %w", err)
		}
		beadsDir = dirs.resolved
		info, err = os.Lstat(beadsDir)
		if err != nil {
			return "", nil, fmt.Errorf("failed to stat .beads directory: %w", err)
		}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", nil, nil // Symlink permissions are not meaningful on Unix
	}
	return beadsDir, info, nil
}

func ensurePerm(path string, info os.FileInfo, mode os.FileMode, failMsg string) error {
	if info.Mode().Perm() == mode {
		return nil
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("%s: %w", failMsg, err)
	}
	return nil
}

func fixDatabasePermissions(beadsDirResolved string) error {
	var dbPath string
	if cfg, err := configfile.Load(beadsDirResolved); err == nil && cfg != nil {
		dbPath = cfg.DatabasePath(beadsDirResolved)
	} else {
		dbPath = filepath.Join(beadsDirResolved, "beads.db")
	}
	dbInfo, err := os.Lstat(dbPath)
	if err != nil {
		return nil
	}
	if dbInfo.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if dbInfo.IsDir() {
		return ensurePerm(dbPath, dbInfo, 0700, "failed to fix database directory permissions")
	}
	return ensurePerm(dbPath, dbInfo, 0600, "failed to fix database permissions")
}

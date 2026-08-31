package fix

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/dolt"
)

type serverDatabaseMetadata struct {
	Name      string
	HasSchema bool
	ProjectID string
}

var listServerMetadataDatabases = inspectServerMetadataDatabases

// FixMissingMetadata checks and repairs missing metadata fields in a Dolt database.
// Fields checked: bd_version, repo_id, clone_id.
// The bdVersion parameter should be the current CLI version string (from the caller
// in package main, since this package cannot import it directly).
// Returns nil if all fields are present or successfully repaired.
func FixMissingMetadata(path string, bdVersion string) error {
	beadsDir, err := resolvedWorkspaceBeadsDir(path)
	if err != nil {
		return err
	}

	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		return nil // Can't load config, nothing to fix
	}
	if cfg == nil {
		return nil // No config file, nothing to fix
	}
	if cfg.GetBackend() != configfile.BackendDolt {
		return nil // Not a Dolt backend, nothing to fix
	}

	ctx := context.Background()

	store, err := dolt.NewFromConfig(ctx, beadsDir)
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	repaired, err := repairMissingMetadata(ctx, store, path, bdVersion)
	if err != nil {
		return err
	}

	// Report results (FR-011: count and names; FR-012: silent if none)
	if len(repaired) > 0 {
		fmt.Printf("  Repaired %d metadata field(s): %s\n", len(repaired), strings.Join(repaired, ", "))
	}

	return nil
}

func repairMissingMetadata(ctx context.Context, store *dolt.DoltStore, path, bdVersion string) ([]string, error) {
	var repaired []string
	if ok, err := repairMissingBDVersion(ctx, store, bdVersion); err != nil {
		return nil, err
	} else if ok {
		repaired = append(repaired, "bd_version")
	}
	if ok, err := repairMissingRepoID(ctx, store, path); err != nil {
		return nil, err
	} else if ok {
		repaired = append(repaired, "repo_id")
	}
	if ok, err := repairMissingCloneID(ctx, store, path); err != nil {
		return nil, err
	} else if ok {
		repaired = append(repaired, "clone_id")
	}
	return repaired, nil
}

func repairMissingBDVersion(ctx context.Context, store *dolt.DoltStore, bdVersion string) (bool, error) {
	val, err := store.GetLocalMetadata(ctx, "bd_version")
	if err != nil || val != "" || bdVersion == "" {
		return false, nil
	}
	if err := store.SetLocalMetadata(ctx, "bd_version", bdVersion); err != nil {
		return false, fmt.Errorf("failed to set bd_version local metadata: %w", err)
	}
	return true, nil
}

func repairMissingRepoID(ctx context.Context, store *dolt.DoltStore, path string) (bool, error) {
	val, err := store.GetMetadata(ctx, "repo_id")
	if err != nil || val != "" {
		return false, nil
	}
	repoID, err := beads.ComputeRepoIDForPath(path)
	if err != nil {
		// Non-git environment: warn and skip (FR-015)
		fmt.Printf("  Warning: could not compute repo_id (not in a git repo?): %v\n", err)
		return false, nil
	}
	if err := store.SetMetadata(ctx, "repo_id", repoID); err != nil {
		return false, fmt.Errorf("failed to set repo_id metadata: %w", err)
	}
	return true, nil
}

func repairMissingCloneID(ctx context.Context, store *dolt.DoltStore, path string) (bool, error) {
	val, err := store.GetMetadata(ctx, "clone_id")
	if err != nil || val != "" {
		return false, nil
	}
	cloneID, err := beads.GetCloneIDForPath(path)
	if err != nil {
		// Non-standard environment: warn and skip (FR-016)
		fmt.Printf("  Warning: could not compute clone_id: %v\n", err)
		return false, nil
	}
	if err := store.SetMetadata(ctx, "clone_id", cloneID); err != nil {
		return false, fmt.Errorf("failed to set clone_id metadata: %w", err)
	}
	return true, nil
}

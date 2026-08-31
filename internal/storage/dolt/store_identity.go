package dolt

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/configfile"
)

// verifyProjectIdentity checks that the database belongs to the expected project.
// If both the local metadata.json and the database have a project_id, they must match.
// Returns nil if verification passes or is not applicable (missing IDs = old setup).
func (s *DoltStore) verifyProjectIdentity(ctx context.Context, beadsDir string) error {
	if beadsDir == "" {
		return nil // can't verify without knowing beadsDir
	}

	// Load local project ID from metadata.json
	metaCfg, err := configfile.Load(beadsDir)
	if err != nil || metaCfg == nil {
		return nil // no local config — skip verification
	}
	localID := metaCfg.ProjectID
	if localID == "" {
		return nil // old-style metadata.json without project_id — skip
	}

	// Read project ID from database metadata table
	dbID, err := s.GetMetadata(ctx, "_project_id")
	if err != nil || dbID == "" {
		return nil // old database without project_id — skip
	}

	if localID != dbID {
		return fmt.Errorf(
			"PROJECT IDENTITY MISMATCH — refusing to connect\n\n"+
				"  Local project ID (metadata.json):  %s\n"+
				"  Database project ID:               %s\n\n"+
				"This means the Dolt server is serving a DIFFERENT project's database.\n"+
				"This can happen when:\n"+
				"  - Another project's server is running on the same port\n"+
				"  - The server restarted with a different data directory\n\n"+
				"To diagnose: bd dolt status\n"+
				"Do NOT run 'bd init' — your data likely exists, just on a different server.",
			localID, dbID)
	}
	return nil
}

func (s *DoltStore) verifyGlobalProjectIdentity(ctx context.Context, beadsDir string) error {
	if beadsDir == "" {
		return nil
	}

	metaCfg, err := configfile.Load(beadsDir)
	if err != nil || metaCfg == nil {
		return nil
	}
	expectedID := metaCfg.GlobalProjectID
	if expectedID == "" {
		return nil
	}

	dbID, err := s.GetMetadata(ctx, "_project_id")
	if err != nil || dbID == "" {
		return nil
	}

	if expectedID != dbID {
		return fmt.Errorf(
			"GLOBAL PROJECT IDENTITY MISMATCH — refusing to connect\n\n"+
				"  Expected global project ID (metadata.json): %s\n"+
				"  Database project ID:                        %s\n\n"+
				expectedID, dbID)
	}
	return nil
}

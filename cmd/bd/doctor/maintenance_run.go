package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/types"
)

// CheckPatrolPollution detects patrol digest and session ended beads that pollute the database.
// These beads are created during patrol operations and should not persist in the database.
//
// Patterns detected:
// - Patrol digests: titles matching "Digest: mol-*-patrol"
// - Session ended beads: titles matching "Session ended: *"
func CheckPatrolPollution(path string) DoctorCheck {
	issues, err := loadMaintenanceIssues(path)
	if err != nil {
		return DoctorCheck{
			Name:     "Patrol Pollution",
			Status:   StatusOK,
			Message:  maintenanceIssuesUnavailableMessage,
			Category: CategoryMaintenance,
		}
	}

	return checkPatrolPollutionForIssues(issues)
}

// checkPatrolPollutionForIssues is the core logic for CheckPatrolPollution,
// operating on a slice of issues directly.
func checkPatrolPollutionForIssues(issues []*types.Issue) DoctorCheck {
	result := detectPatrolPollution(issues)

	// Check thresholds
	hasPatrolPollution := result.PatrolDigestCount > PatrolDigestThreshold
	hasSessionPollution := result.SessionBeadCount > SessionBeadThreshold

	if !hasPatrolPollution && !hasSessionPollution {
		return DoctorCheck{
			Name:     "Patrol Pollution",
			Status:   StatusOK,
			Message:  "No patrol pollution detected",
			Category: CategoryMaintenance,
		}
	}

	// Build warning message
	var warnings []string
	if hasPatrolPollution {
		warnings = append(warnings, fmt.Sprintf("%d patrol digest beads (should be 0)", result.PatrolDigestCount))
	}
	if hasSessionPollution {
		warnings = append(warnings, fmt.Sprintf("%d session ended beads (should be wisps)", result.SessionBeadCount))
	}

	// Build detail with sample IDs
	var details []string
	if len(result.PatrolDigestIDs) > 0 {
		details = append(details, fmt.Sprintf("Patrol digests: %v", result.PatrolDigestIDs))
	}
	if len(result.SessionBeadIDs) > 0 {
		details = append(details, fmt.Sprintf("Session beads: %v", result.SessionBeadIDs))
	}

	return DoctorCheck{
		Name:     "Patrol Pollution",
		Status:   StatusWarning,
		Message:  strings.Join(warnings, ", "),
		Detail:   strings.Join(details, "; "),
		Fix:      "Run 'bd doctor --fix' to clean up patrol pollution",
		Category: CategoryMaintenance,
	}
}

// detectPatrolPollution scans issues for patrol pollution patterns.
func detectPatrolPollution(issues []*types.Issue) patrolPollutionResult {
	var result patrolPollutionResult

	for _, issue := range issues {
		if issue == nil {
			continue
		}
		switch classifyPatrolIssue(issue.Title) {
		case patrolIssueDigest:
			result.PatrolDigestCount++
			if len(result.PatrolDigestIDs) < 3 {
				result.PatrolDigestIDs = append(result.PatrolDigestIDs, issue.ID)
			}
		case patrolIssueSessionEnded:
			result.SessionBeadCount++
			if len(result.SessionBeadIDs) < 3 {
				result.SessionBeadIDs = append(result.SessionBeadIDs, issue.ID)
			}
		}
	}

	return result
}

// getPatrolPollutionIDs returns all IDs of patrol pollution beads for deletion
func getPatrolPollutionIDs(path string) ([]string, error) {
	issues, err := loadMaintenanceIssues(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load issues: %w", err)
	}

	var ids []string
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		switch classifyPatrolIssue(issue.Title) {
		case patrolIssueDigest, patrolIssueSessionEnded:
			ids = append(ids, issue.ID)
		}
	}

	return ids, nil
}

// loadMaintenanceIssues loads issues for maintenance checks.
// It prefers Dolt (source of truth) and falls back to legacy JSONL for
// backwards compatibility with non-Dolt installations.
func loadMaintenanceIssues(path string) ([]*types.Issue, error) {
	beadsDir := ResolveBeadsDirForRepo(path)

	issues, err := loadMaintenanceIssuesFromDatabase(beadsDir)
	if err == nil {
		return issues, nil
	}

	issues, jsonlErr := loadMaintenanceIssuesFromJSONL(beadsDir)
	if jsonlErr == nil {
		return issues, nil
	}

	return nil, fmt.Errorf("database read failed: %w; JSONL fallback read failed: %v", err, jsonlErr)
}

func loadMaintenanceIssuesFromDatabase(beadsDir string) ([]*types.Issue, error) {
	ctx := context.Background()
	store, err := dolt.NewFromConfigWithCLIOptions(ctx, beadsDir, &dolt.Config{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()

	ephemeral := false
	return store.SearchIssues(ctx, "", types.IssueFilter{IssueFilterFlags: types.IssueFilterFlags{Ephemeral: &ephemeral}})
}

func loadMaintenanceIssuesFromJSONL(beadsDir string) ([]*types.Issue, error) {
	jsonlPath := filepath.Join(beadsDir, "issues.jsonl")
	file, err := os.Open(jsonlPath) // #nosec G304 - path constructed safely
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var issues []*types.Issue
	decoder := json.NewDecoder(file)
	for {
		issue := &types.Issue{}
		if err := decoder.Decode(issue); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		issues = append(issues, issue)
	}

	return issues, nil
}

func loadMisclassifiedWispIssues(path string) ([]*types.Issue, error) {
	beadsDir := ResolveBeadsDirForRepo(path)

	issues, err := loadMisclassifiedWispIssuesFromDatabase(beadsDir)
	if err == nil {
		return issues, nil
	}

	issues, jsonlErr := loadMisclassifiedWispIssuesFromJSONL(beadsDir)
	if jsonlErr == nil {
		return issues, nil
	}

	return nil, fmt.Errorf("database read failed: %w; JSONL fallback read failed: %v", err, jsonlErr)
}

func loadMisclassifiedWispIssuesFromDatabase(beadsDir string) ([]*types.Issue, error) {
	db, store, err := openStoreDB(beadsDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()

	rows, err := db.Query(
		"SELECT id FROM issues WHERE id LIKE ? AND (ephemeral = 0 OR ephemeral IS NULL)",
		"%-wisp-%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var issues []*types.Issue
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		issues = append(issues, &types.Issue{IssueID: types.IssueID{ID: id}, IssueWisp: types.IssueWisp{Ephemeral: false}})
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return issues, nil
}

func loadMisclassifiedWispIssuesFromJSONL(beadsDir string) ([]*types.Issue, error) {
	issues, err := loadMaintenanceIssuesFromJSONL(beadsDir)
	if err != nil {
		return nil, err
	}

	var filtered []*types.Issue
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		if strings.Contains(issue.ID, "-wisp-") && !issue.Ephemeral {
			filtered = append(filtered, issue)
		}
	}
	return filtered, nil
}

func classifyPatrolIssue(title string) patrolIssueKind {
	switch {
	case strings.HasPrefix(title, "Digest: mol-") && strings.HasSuffix(title, "-patrol"):
		return patrolIssueDigest
	case strings.HasPrefix(title, "Session ended:"):
		return patrolIssueSessionEnded
	default:
		return patrolIssueNone
	}
}

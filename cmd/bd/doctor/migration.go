package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// PendingMigration represents a single pending migration
type PendingMigration struct {
	Name        string // e.g., "sync"
	Description string // e.g., "Configure sync branch for multi-clone setup"
	Command     string // e.g., "bd migrate sync beads-sync"
	Priority    int    // 1 = critical, 2 = recommended, 3 = optional
}

// DetectPendingMigrations detects all pending migrations for a beads directory
func DetectPendingMigrations(path string) []PendingMigration {
	var pending []PendingMigration

	beadsDir := ResolveBeadsDirForRepo(path)

	// Skip if .beads doesn't exist
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return pending
	}

	hookPlan, err := PlanHookMigration(path)
	if err == nil && hookPlan.IsGitRepo && hookPlan.NeedsMigrationCount > 0 {
		description := fmt.Sprintf("Git hook migration needed for %d hook(s)", hookPlan.NeedsMigrationCount)
		command := "bd doctor --fix"
		priority := 2
		if hookPlan.BrokenMarkerCount > 0 {
			description = fmt.Sprintf(
				"%s (%d with broken markers)",
				description,
				hookPlan.BrokenMarkerCount,
			)
			priority = 1
		}

		pending = append(pending, PendingMigration{
			Name:        "hooks",
			Description: description,
			Command:     command,
			Priority:    priority,
		})
	}

	return pending
}

// CheckPendingMigrations returns a doctor check summarizing all pending migrations
func CheckPendingMigrations(path string) DoctorCheck {
	pending := DetectPendingMigrations(path)
	if len(pending) == 0 {
		return DoctorCheck{
			Name:     "Pending Migrations",
			Status:   StatusOK,
			Message:  "None required",
			Category: CategoryMaintenance,
		}
	}
	details, fixes := formatPendingMigrations(pending)
	return DoctorCheck{
		Name:     "Pending Migrations",
		Status:   pendingMigrationStatus(pending),
		Message:  fmt.Sprintf("%d available", len(pending)),
		Detail:   strings.Join(details, "\n"),
		Fix:      strings.Join(fixes, "\n"),
		Category: CategoryMaintenance,
	}
}

func pendingMigrationPriorityLabel(priority int) string {
	switch priority {
	case 1:
		return " [critical]"
	case 2:
		return " [recommended]"
	case 3:
		return " [optional]"
	}
	return ""
}

func formatPendingMigrations(pending []PendingMigration) ([]string, []string) {
	var details, fixes []string
	for _, m := range pending {
		details = append(details, fmt.Sprintf("• %s: %s%s", m.Name, m.Description, pendingMigrationPriorityLabel(m.Priority)))
		fixes = append(fixes, m.Command)
	}
	return details, fixes
}

func pendingMigrationStatus(pending []PendingMigration) string {
	status := StatusOK
	for _, m := range pending {
		if m.Priority == 1 {
			return StatusError
		}
		if m.Priority == 2 && status != StatusError {
			status = StatusWarning
		}
	}
	return status
}

// hasGitRemote checks if the repository has a git remote
func hasGitRemote(repoPath string) bool {
	cmd := exec.Command("git", "remote")
	cmd.Dir = repoPath
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(output))) > 0
}

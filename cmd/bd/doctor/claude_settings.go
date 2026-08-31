package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CheckClaudeSettingsHealth validates that Claude Code settings files are well-formed JSON.
// Malformed settings silently break hooks and plugin detection.
// repoPath is the project root directory.
func CheckClaudeSettingsHealth(repoPath string) DoctorCheck {
	home, err := os.UserHomeDir()
	if err != nil {
		return DoctorCheck{
			Name:    "Claude Settings Health",
			Status:  StatusOK,
			Message: "N/A (unable to determine home directory)",
		}
	}

	settingsFiles := []struct {
		path  string
		label string
	}{
		{filepath.Join(home, ".claude", "settings.json"), "~/.claude/settings.json"},
		{filepath.Join(repoPath, ".claude", "settings.json"), ".claude/settings.json"},
		{filepath.Join(repoPath, ".claude", "settings.local.json"), ".claude/settings.local.json"},
	}

	var malformed []string
	var checked int
	for _, sf := range settingsFiles {
		data, err := os.ReadFile(sf.path) // #nosec G304 -- paths are constructed from known safe locations
		if err != nil {
			continue // File doesn't exist, skip
		}
		checked++
		var parsed map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err != nil {
			malformed = append(malformed, fmt.Sprintf("%s: %v", sf.label, err))
		}
	}

	if checked == 0 {
		return DoctorCheck{
			Name:    "Claude Settings Health",
			Status:  StatusOK,
			Message: "No Claude Code settings files found",
		}
	}

	if len(malformed) > 0 {
		return DoctorCheck{
			Name:    "Claude Settings Health",
			Status:  StatusError,
			Message: fmt.Sprintf("%d malformed settings file(s)", len(malformed)),
			Detail:  strings.Join(malformed, "\n"),
			Fix:     "Fix the JSON syntax in the listed file(s). Malformed settings break hooks and plugin detection.",
		}
	}

	return DoctorCheck{
		Name:    "Claude Settings Health",
		Status:  StatusOK,
		Message: fmt.Sprintf("%d settings file(s) valid", checked),
	}
}

// CheckClaudeHookCompleteness verifies that when hooks are installed,
// SessionStart is covered. Claude Code fires SessionStart on startup, resume,
// clear, and after compaction, so current bd prime context injection only needs
// SessionStart.
// repoPath is the project root directory.
func CheckClaudeHookCompleteness(repoPath string) DoctorCheck {
	home, err := os.UserHomeDir()
	if err != nil {
		return DoctorCheck{
			Name:    "Claude Hook Completeness",
			Status:  StatusOK,
			Message: "N/A (unable to determine home directory)",
		}
	}

	settingsFiles := []string{
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(repoPath, ".claude", "settings.json"),
		filepath.Join(repoPath, ".claude", "settings.local.json"),
	}

	var hasAnyHook bool
	var hasSessionStart bool

	for _, sf := range settingsFiles {
		ss, pc := checkHookEvents(sf)
		if ss || pc {
			hasAnyHook = true
		}
		if ss {
			hasSessionStart = true
		}
	}

	if !hasAnyHook {
		// No hooks installed at all - CheckClaude already reports this
		return DoctorCheck{
			Name:    "Claude Hook Completeness",
			Status:  StatusOK,
			Message: "N/A (no hooks installed)",
		}
	}

	if hasSessionStart {
		return DoctorCheck{
			Name:    "Claude Hook Completeness",
			Status:  StatusOK,
			Message: "SessionStart hook present",
		}
	}

	return DoctorCheck{
		Name:    "Claude Hook Completeness",
		Status:  StatusWarning,
		Message: "Missing hook event(s): SessionStart",
		Detail:  "SessionStart injects context on new sessions and after compaction.",
		Fix: "Run 'bd setup claude' to install hooks, or\n" +
			"install the beads plugin which includes hooks automatically.",
	}
}

// checkHookEvents returns which bd-prime hook events are present in a settings file.
func checkHookEvents(settingsPath string) (hasSessionStart, hasPreCompact bool) {
	hooks, ok := loadClaudeHooksMap(settingsPath)
	if !ok {
		return false, false
	}
	return eventHasBeadsPrime(hooks, "SessionStart"), eventHasBeadsPrime(hooks, "PreCompact")
}

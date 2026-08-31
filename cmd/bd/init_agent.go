package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/templates/agents"
	"github.com/jonbaldie/beads/internal/ui"
)

// addAgentsInstructions creates or updates the agents file with embedded template content.
// agentFile is the target filename (e.g. "AGENTS.md" or "BEADS.md").
// If templatePath is non-empty, the custom template file is used instead of the embedded default.
// profile controls which template variant to render (full or minimal); defaults to minimal.
// opts controls conditional content (e.g. omitting bd dolt push when no remote is configured).
func addAgentsInstructions(agentFile string, verbose bool, templatePath string, profile agents.Profile, opts agents.RenderOpts) {
	if profile == "" {
		profile = agents.ProfileMinimal
	}

	if err := updateAgentFile(agentFile, verbose, templatePath, profile, opts); err != nil {
		// Non-fatal - continue with other files
		if verbose {
			fmt.Fprintf(os.Stderr, "Warning: failed to update %s: %v\n", agentFile, err)
		}
	}
}

// updateAgentFile creates or updates an agent instructions file with embedded template content.
// When a beads section already exists (legacy or current), it is updated to the latest
// versioned format so that `bd init` never silently locks in stale sections.
// If the file already has a full profile and a minimal profile is requested, the full
// profile is preserved to avoid information loss.
func updateAgentFile(filename string, verbose bool, templatePath string, profile agents.Profile, opts agents.RenderOpts) error {
	//nolint:gosec // G304: filename validated by config.ValidateAgentsFile or defaulted to AGENTS.md
	content, err := os.ReadFile(filename)
	if os.IsNotExist(err) {
		return createAgentFile(filename, verbose, templatePath, profile, opts)
	}
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", filename, err)
	}
	contentStr := string(content)
	if strings.Contains(contentStr, "BEGIN BEADS INTEGRATION") {
		return refreshAgentFile(filename, contentStr, verbose, profile, opts)
	}
	return appendAgentSection(filename, contentStr, verbose, profile, opts)
}

func createAgentFile(filename string, verbose bool, templatePath string, profile agents.Profile, opts agents.RenderOpts) error {
	newContent, err := agentFileTemplate(templatePath)
	if err != nil {
		return err
	}
	// Replace the beads section with the requested profile.
	// EmbeddedDefault() ships with profile:full; swap to the requested profile
	// (which defaults to minimal). Also handles legacy markers without profile metadata.
	if strings.Contains(newContent, "BEGIN BEADS INTEGRATION") {
		if replaced, changed, err := agents.ReplaceSectionWithOpts(newContent, profile, opts); err == nil && changed {
			newContent = replaced
		}
	}
	// #nosec G306 - markdown needs to be readable
	if err := os.WriteFile(filename, []byte(newContent), 0644); err != nil {
		return fmt.Errorf("failed to create %s: %w", filename, err)
	}
	if verbose {
		fmt.Printf("  %s Created %s with agent instructions\n", ui.RenderPass("✓"), filename)
	}
	return nil
}

func agentFileTemplate(templatePath string) (string, error) {
	if templatePath == "" {
		return agents.EmbeddedDefault(), nil
	}
	//nolint:gosec // G304: templatePath comes from --agents-template flag
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to read template %s: %w", templatePath, err)
	}
	return string(data), nil
}

func refreshAgentFile(filename, contentStr string, verbose bool, profile agents.Profile, opts agents.RenderOpts) error {
	effectiveProfile := preserveAgentProfile(filename, contentStr, verbose, profile)
	updated, changed, replaceErr := agents.ReplaceSectionWithOpts(contentStr, effectiveProfile, opts)
	if replaceErr != nil {
		return fmt.Errorf("failed to update beads section in %s: %w", filename, replaceErr)
	}
	if !changed {
		if verbose {
			fmt.Printf("  %s already has current agent instructions\n", filename)
		}
		return nil
	}
	// #nosec G306 - markdown needs to be readable
	if err := os.WriteFile(filename, []byte(updated), 0644); err != nil {
		return fmt.Errorf("failed to update %s: %w", filename, err)
	}
	if verbose {
		fmt.Printf("  %s Updated beads section in %s to latest format\n", ui.RenderPass("✓"), filename)
	}
	return nil
}

func preserveAgentProfile(filename, contentStr string, verbose bool, profile agents.Profile) agents.Profile {
	existingMeta := agents.ParseMarker(contentStr[strings.Index(contentStr, "<!-- BEGIN BEADS INTEGRATION"):])
	if existingMeta == nil || existingMeta.Profile != agents.ProfileFull || profile != agents.ProfileMinimal {
		return profile
	}
	if verbose {
		fmt.Printf("  ℹ %s already has full profile; preserving (higher-information) content\n", filename)
	}
	return agents.ProfileFull
}

func appendAgentSection(filename, contentStr string, verbose bool, profile agents.Profile, opts agents.RenderOpts) error {
	newContent := contentStr
	if !strings.HasSuffix(newContent, "\n") {
		newContent += "\n"
	}
	newContent += "\n" + agents.RenderSectionWithOpts(profile, opts)
	// #nosec G306 - markdown needs to be readable
	if err := os.WriteFile(filename, []byte(newContent), 0644); err != nil {
		return fmt.Errorf("failed to update %s: %w", filename, err)
	}
	if verbose {
		fmt.Printf("  %s Added agent instructions to %s\n", ui.RenderPass("✓"), filename)
	}
	return nil
}

// setupClaudeSettings creates or updates .claude/settings.local.json with onboard instruction
func setupClaudeSettings(verbose bool) error {
	settingsPath := filepath.Join(".claude", "settings.local.json")
	if err := os.MkdirAll(".claude", 0755); err != nil {
		return fmt.Errorf("failed to create .claude directory: %w", err)
	}
	existingSettings, err := loadClaudeSettings(settingsPath)
	if err != nil {
		return err
	}
	if applyClaudePrimePrompt(existingSettings, verbose) {
		return nil
	}
	updatedContent, err := json.MarshalIndent(existingSettings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings JSON: %w", err)
	}
	// #nosec G306 - config file needs 0644
	if err := os.WriteFile(settingsPath, updatedContent, 0644); err != nil {
		return fmt.Errorf("failed to write claude settings: %w", err)
	}
	if verbose {
		fmt.Printf("Configured Claude settings with bd prime instruction\n")
	}
	return nil
}

func loadClaudeSettings(settingsPath string) (map[string]interface{}, error) {
	// #nosec G304 - user config path
	content, err := os.ReadFile(settingsPath)
	if os.IsNotExist(err) {
		return make(map[string]interface{}), nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read existing %s: %w", settingsPath, err)
	}
	var existingSettings map[string]interface{}
	if err := json.Unmarshal(content, &existingSettings); err != nil {
		return nil, fmt.Errorf("existing %s contains invalid JSON: %w\nPlease fix the JSON syntax manually before running bd init", settingsPath, err)
	}
	return existingSettings, nil
}

func applyClaudePrimePrompt(existingSettings map[string]interface{}, verbose bool) bool {
	primePrompt := "Before starting any work, run 'bd prime' to understand the current project state and available issues."
	promptValue, exists := existingSettings["prompt"]
	if !exists {
		existingSettings["prompt"] = primePrompt
		return false
	}
	promptStr, ok := promptValue.(string)
	if !ok {
		existingSettings["prompt"] = primePrompt
		return false
	}
	if strings.Contains(promptStr, "bd prime") {
		if verbose {
			fmt.Printf("Claude settings already configured with bd prime instruction\n")
		}
		return true
	}
	if strings.Contains(promptStr, "bd onboard") {
		existingSettings["prompt"] = strings.ReplaceAll(promptStr, "bd onboard", "bd prime")
		return false
	}
	existingSettings["prompt"] = promptStr + "\n\n" + primePrompt
	return false
}

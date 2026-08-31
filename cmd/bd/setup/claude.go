package setup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/templates/agents"
)

var (
	claudeEnvProvider     = defaultClaudeEnv
	errClaudeHooksMissing = errors.New("claude hooks not installed")
)

const claudeInstructionsFile = "CLAUDE.md"

var claudeAgentsIntegration = agentsIntegration{
	name:         "Claude Code",
	setupCommand: "bd setup claude",
	profile:      agents.ProfileMinimal,
}

type claudeEnv struct {
	stdout     io.Writer
	stderr     io.Writer
	homeDir    string
	projectDir string
	ensureDir  func(string, os.FileMode) error
	readFile   func(string) ([]byte, error)
	writeFile  func(string, []byte) error
}

func defaultClaudeEnv() (claudeEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return claudeEnv{}, fmt.Errorf("home directory: %w", err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return claudeEnv{}, fmt.Errorf("working directory: %w", err)
	}
	return claudeEnv{
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		homeDir:    home,
		projectDir: workDir,
		ensureDir:  EnsureDir,
		readFile:   os.ReadFile,
		writeFile: func(path string, data []byte) error {
			return atomicWriteFile(path, data)
		},
	}, nil
}

func projectSettingsPath(base string) string {
	return filepath.Join(base, ".claude", "settings.json")
}

func legacyProjectSettingsPath(base string) string {
	return filepath.Join(base, ".claude", "settings.local.json")
}

func globalSettingsPath(home string) string {
	return filepath.Join(home, ".claude", "settings.json")
}

func claudeAgentsEnv(env claudeEnv) agentsEnv {
	ae, _ := claudeAgentsEnvRedirect(env)
	return ae
}

// claudeAgentsEnvRedirect is claudeAgentsEnv plus a bool reporting whether the
// AGENTS.md redirect activated, so callers that need to clean up a stale
// CLAUDE.md block (installClaude, removeClaude) can tell the redirected case
// apart from the plain CLAUDE.md-is-authoritative case.
func claudeAgentsEnvRedirect(env claudeEnv) (agentsEnv, bool) {
	claudePath := filepath.Join(env.projectDir, claudeInstructionsFile)

	// If CLAUDE.md is a thin stub that imports AGENTS.md via the @-include
	// convention (Claude Code expands @-imports), redirect the managed beads
	// section to AGENTS.md instead of duplicating it in the stub. This matches
	// the shared-authoritative-file pattern used by repos that keep AGENTS.md
	// as the single source of agent instructions.
	agentsFile := config.SafeAgentsFile()
	agentsPath := filepath.Join(env.projectDir, agentsFile)
	if data, err := env.readFile(claudePath); err == nil {
		if isAgentsImportStub(string(data), agentsFile) {
			if _, err := env.readFile(agentsPath); err == nil {
				return agentsEnv{
					agentsPath: agentsPath,
					stdout:     env.stdout,
					stderr:     env.stderr,
				}, true
			}
		}
	}

	return agentsEnv{
		agentsPath: claudePath,
		stdout:     env.stdout,
		stderr:     env.stderr,
	}, false
}

// stripStaleClaudeBlock removes a beads-managed block left behind in CLAUDE.md
// by an older bd version, once the AGENTS.md redirect is active. Older bd
// releases wrote the managed block directly into CLAUDE.md; a project that has
// since adopted the "@AGENTS.md" import-stub pattern would otherwise carry a
// stale duplicate of that block alongside the one now maintained in AGENTS.md.
func stripStaleClaudeBlock(env claudeEnv) error {
	claudePath := filepath.Join(env.projectDir, claudeInstructionsFile)
	data, err := env.readFile(claudePath)
	if err != nil {
		return nil
	}

	content := string(data)
	if !containsBeadsMarker(content) {
		return nil
	}

	newContent := removeBeadsSection(content)
	if err := env.writeFile(claudePath, []byte(newContent)); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(env.stdout, "✓ Removed stale beads block from %s (now redirected to %s)\n", claudeInstructionsFile, config.SafeAgentsFile())
	return nil
}

// isAgentsImportStub reports whether content contains an @-include directive
// for the given agents file (e.g. "@AGENTS.md" on its own line), indicating
// the file is a thin stub that imports shared agent instructions from the
// agents file rather than carrying its own content.
//
// Directives inside fenced code blocks do not count. Claude Code does not expand
// an @-import that is shown as code, so a file that merely documents the pattern
// is not a stub — and treating it as one is not a cosmetic misread: the caller
// redirects the managed block to AGENTS.md and then stripStaleClaudeBlock deletes
// the block that was in CLAUDE.md. A fenced example would silently relocate
// content out of the file that was, in fact, authoritative.
//
// Only fences are skipped, not four-space-indented blocks. An indented line is
// ambiguous in a way a fence is not — it is equally the continuation of a list
// item, which is a plausible place to put a real directive — so treating
// indentation as code would trade this false positive for a false negative.
func isAgentsImportStub(content, agentsFile string) bool {
	directives := []string{"@" + agentsFile, "@./" + agentsFile}
	fence := ""
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)

		if marker := codeFenceMarker(trimmed); marker != "" {
			switch {
			case fence == "":
				fence = marker
			case marker == fence:
				// A closing fence must match the character the block opened
				// with, so ``` inside a ~~~ block does not end it.
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}

		for _, directive := range directives {
			if trimmed == directive {
				return true
			}
		}
	}
	return false
}

// codeFenceMarker returns the fence character ("`" or "~") if the already-trimmed
// line opens or closes a fenced code block, and "" otherwise. CommonMark requires
// at least three of the same character; an info string ("```go") may follow.
func codeFenceMarker(trimmed string) string {
	for _, ch := range []string{"`", "~"} {
		if strings.HasPrefix(trimmed, strings.Repeat(ch, 3)) {
			return ch
		}
	}
	return ""
}

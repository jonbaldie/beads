package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/beads"
	internalbeads "github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/spf13/cobra"
)

type primeOptions struct {
	fullMode          bool
	mcpMode           bool
	stealthMode       bool
	exportMode        bool
	memoriesOnly      bool
	noMemories        bool
	hookJSONMode      bool
	maxMemories       int
	maxMemoryChars    int
	maxMemoriesSet    bool
	maxMemoryCharsSet bool
}

func primeOptionsFromCommand(cmd *cobra.Command) primeOptions {
	flags := cmd.Flags()
	fullMode, _ := flags.GetBool("full")
	mcpMode, _ := flags.GetBool("mcp")
	stealthMode, _ := flags.GetBool("stealth")
	exportMode, _ := flags.GetBool("export")
	memoriesOnly, _ := flags.GetBool("memories-only")
	noMemories, _ := flags.GetBool("no-memories")
	hookJSONMode, _ := flags.GetBool("hook-json")
	maxMemories, _ := flags.GetInt("max-memories")
	maxMemoryChars, _ := flags.GetInt("max-memory-chars")
	return primeOptions{
		fullMode:          fullMode,
		mcpMode:           mcpMode,
		stealthMode:       stealthMode,
		exportMode:        exportMode,
		memoriesOnly:      memoriesOnly,
		noMemories:        noMemories,
		hookJSONMode:      hookJSONMode,
		maxMemories:       maxMemories,
		maxMemoryChars:    maxMemoryChars,
		maxMemoriesSet:    flags.Changed("max-memories"),
		maxMemoryCharsSet: flags.Changed("max-memory-chars"),
	}
}

const (
	primeStoreTimeoutEnv     = "BEADS_PRIME_TIMEOUT"
	primeStoreTimeoutDefault = 10 * time.Second
)

var ensureStoreActiveForPrime = ensureStoreActiveWithContext

func primeStoreTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(primeStoreTimeoutEnv))
	if raw == "" {
		return primeStoreTimeoutDefault
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d > 0 {
			return d
		}
		return primeStoreTimeoutDefault
	}
	if d, err := time.ParseDuration(raw + "s"); err == nil {
		if d > 0 {
			return d
		}
		return primeStoreTimeoutDefault
	}
	return primeStoreTimeoutDefault
}

// resolveGlobalPrimePath returns the path to ~/.config/beads/PRIME.md if it
// exists. configDirOverride is used for testing; pass "" for production.
func resolveGlobalPrimePath(configDirOverride string) string {
	var configDir string
	if configDirOverride != "" {
		configDir = configDirOverride
	} else {
		var err error
		configDir, err = os.UserConfigDir()
		if err != nil {
			return ""
		}
	}
	p := filepath.Join(configDir, "beads", "PRIME.md")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

var primeCmd = &cobra.Command{
	Use:     "prime",
	GroupID: "setup",
	Short:   "Output AI-optimized workflow context",
	Long: `Output essential Beads workflow context in AI-optimized markdown format.

Automatically detects if MCP server is active and adapts output:
- MCP mode: Brief workflow reminders (~50 tokens)
- CLI mode: Full command reference (~1-2k tokens)

Designed for Claude Code, Gemini CLI, and Codex SessionStart hooks to prevent
agents from forgetting bd workflow after context compaction.

Config options:
- no-git-ops: When true, outputs stealth mode (no git commands in session close protocol).
  Set via: bd config set no-git-ops true
  Useful when you want to control when commits happen manually.
- agent.profile: Explicit policy profile for git/commit authority wording
  (conservative | minimal | team-maintainer; default conservative).
  Set via: bd config set agent.profile team-maintainer
  Or per-session: BD_AGENT_PROFILE=team-maintainer (env var takes precedence).
  See docs/getting-started/ide-setup.md#policy-profiles for what each profile means.

	Workflow customization:
	- Place a .beads/PRIME.md file in the local clone or resolved workspace to override the default workflow text. Persistent memories (from bd remember) are still appended so memory injection keeps working under a custom template.
	- Use --export to dump the default content for customization.
	- Use --memories-only for hook contexts that should inject only persistent memories; this returns only the memories section even when a custom PRIME.md is present.
	- Use --no-memories to omit the persistent memories section (useful when the memories section is large and would dominate a context budget). --memories-only takes precedence if both are set.

Memory injection caps:
	Large memory sets can exceed what a session-start hook host will ingest,
	and hosts truncate silently. Cap what prime injects with --max-memories N
	and/or --max-memory-chars N (or the prime.max-memories /
	prime.max-memory-chars config keys; an explicit flag wins, and an explicit
	0 forces unlimited). Caps apply at whole-memory boundaries, at least one
	memory is always emitted, and a banner ahead of the entries reports how
	many were elided and how to browse the rest with bd memories.
	--max-memory-chars caps the total bytes of the injected memory entries;
	the section header and elision banner are excluded from the budget.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := primeOptionsFromCommand(cmd)
		evt := metrics.NewCommandEvent("prime")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		emit := func(content string) {
			if opts.hookJSONMode {
				_ = outputHookJSON(os.Stdout, content)
			} else {
				fmt.Print(content)
			}
		}

		beadsDir := beads.FindBeadsDir()
		if beadsDir == "" {
			// Silent exit with success enables cross-platform hook integration.
			// Under --hook-json still emit a valid empty envelope.
			if opts.hookJSONMode {
				_ = outputHookJSON(os.Stdout, "")
			}
			return nil
		}

		// Detect MCP mode (unless overridden by flags)
		mcpMode := isMCPActive()
		if opts.fullMode {
			mcpMode = false
		}
		if opts.mcpMode {
			mcpMode = true
		}

		stealthMode := opts.stealthMode || config.GetBool("no-git-ops")

		// --memories-only is the primary memory-injection path for hook contexts
		// (e.g. PreCompact). It must return ONLY the persistent memories section,
		// regardless of any custom PRIME.md override or --export (GH#3941).
		// Handle it before the custom-PRIME branch so a custom PRIME.md can never
		// suppress memory injection.
		if opts.memoriesOnly {
			var buf bytes.Buffer
			if err := outputMemoriesOnlyContextWithOptions(&buf, opts); err != nil {
				// Suppress all errors - silent exit with success.
				if opts.hookJSONMode {
					_ = outputHookJSON(os.Stdout, "")
				}
				return nil
			}
			emit(buf.String())
			return nil
		}

		// Check for custom PRIME.md override (unless --export flag).
		// A custom PRIME.md replaces the default workflow text, but the persistent
		// memories section is still appended (when present) so `bd remember` keeps
		// working under a custom template — matching the default-template behavior
		// (GH#3941).
		if !opts.exportMode {
			if content, ok := readCustomPrimeContent(beadsDir); ok {
				if !opts.noMemories {
					if mem := formatMemoriesForPrimeWithOptions(false, opts); mem != "" {
						content += mem
					}
				}
				emit(content)
				return nil
			}
		}

		var buf bytes.Buffer
		if err := outputPrimeContextWithPrimeOptions(&buf, mcpMode, stealthMode, opts); err != nil {
			// Errors are suppressed by design for hook integration.
			if opts.hookJSONMode {
				_ = outputHookJSON(os.Stdout, "")
			}
			return nil
		}
		// Append the AGENTS.md/CLAUDE.md divergence reminder only when both
		// files are independent regulars carrying the bd marker; otherwise this
		// adds nothing (zero output, negligible cost).
		buf.WriteString(primeDivergenceReminder(""))
		emit(buf.String())
		return nil
	},
}

func init() {
	primeCmd.Flags().Bool("full", false, "Force full CLI output (ignore MCP detection)")
	primeCmd.Flags().Bool("mcp", false, "Force MCP mode (minimal output)")
	primeCmd.Flags().Bool("stealth", false, "Stealth mode (no git operations, flush only)")
	primeCmd.Flags().Bool("export", false, "Output default content (ignores PRIME.md override)")
	primeCmd.Flags().Bool("memories-only", false, "Output only persistent memories for compact hook contexts")
	primeCmd.Flags().Bool("no-memories", false, "Omit the persistent memories section (ignored when --memories-only is set, which wins)")
	primeCmd.Flags().Bool("hook-json", false, "Wrap output in the SessionStart hook JSON envelope (Claude Code, Gemini CLI, Codex)")
	primeCmd.Flags().Int("max-memories", 0, "Cap injected persistent memories to N entries (0 = unlimited; falls back to the prime.max-memories config key)")
	primeCmd.Flags().Int("max-memory-chars", 0, "Cap the total bytes of injected memory entries, at whole-memory boundaries; section header and banner are not counted (0 = unlimited; falls back to the prime.max-memory-chars config key)")
	rootCmd.AddCommand(primeCmd)
}

// readCustomPrimeContent returns the contents of a custom PRIME.md override and
// true when one is found. It checks, in priority order: the local .beads/PRIME.md
// (clone-specific customization), the redirected workspace PRIME.md (shared
// customization), then the global ~/.config/beads/PRIME.md. It returns ("", false)
// when no override exists, so callers fall through to the generated default.
func readCustomPrimeContent(beadsDir string) (string, bool) {
	localPrimePath := filepath.Join(".beads", "PRIME.md")
	// Try local first (user's clone-specific customization).
	// #nosec G304 -- path is relative to cwd
	if content, err := os.ReadFile(localPrimePath); err == nil {
		return string(content), true
	}
	// Fall back to redirected location (shared customization).
	redirectedPrimePath := filepath.Join(beadsDir, "PRIME.md")
	// #nosec G304 -- path is constructed from beadsDir which we control
	if content, err := os.ReadFile(redirectedPrimePath); err == nil {
		return string(content), true
	}
	// Fall back to global config (~/.config/beads/PRIME.md).
	if globalPath := resolveGlobalPrimePath(""); globalPath != "" {
		// #nosec G304 -- path constructed from UserConfigDir which we control
		if content, err := os.ReadFile(globalPath); err == nil {
			return string(content), true
		}
	}
	return "", false
}

// outputHookJSON wraps content in the SessionStart hook JSON envelope shared
// by Claude Code, Gemini CLI, and Codex. All three require stdout to be valid
// JSON — no plain text may be emitted alongside it. See:
// https://geminicli.com/docs/hooks/reference/
func outputHookJSON(w io.Writer, content string) error {
	type hookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	envelope := struct {
		HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
	}{
		HookSpecificOutput: hookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: content,
		},
	}
	return json.NewEncoder(w).Encode(envelope)
}

// isMCPActive detects if MCP server is currently active
func isMCPActive() bool {
	// Get home directory with fallback
	home, err := os.UserHomeDir()
	if err != nil {
		// Fallback to HOME environment variable
		home = os.Getenv("HOME")
		if home == "" {
			// Can't determine home directory, assume no MCP
			return false
		}
	}

	settingsPath := filepath.Join(home, ".claude/settings.json")
	// #nosec G304 -- settings path derived from user home directory
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return false
	}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}

	// Check mcpServers section for beads
	mcpServers, ok := settings["mcpServers"].(map[string]interface{})
	if !ok {
		return false
	}

	// Look for beads server (any key containing "beads")
	for key := range mcpServers {
		if strings.Contains(strings.ToLower(key), "beads") {
			return true
		}
	}

	return false
}

// isEphemeralBranch detects if current branch has no upstream (ephemeral/local-only)
var isEphemeralBranch = func() bool {
	// git rev-parse --abbrev-ref --symbolic-full-name @{u}
	// Returns error code 128 if no upstream configured
	rc, err := internalbeads.GetRepoContext()
	if err != nil {
		return true // Default to ephemeral if we can't determine context
	}
	cmd := rc.GitCmdCWD(context.Background(), "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	return cmd.Run() != nil
}

// primeNoPushConfigured reports whether the "no-push" config flag is set
// (stubbable for tests).
var primeNoPushConfigured = func() bool {
	return config.GetBool("no-push")
}

// primeAgentProfile reports the explicit agent.profile knob (gh#3423,
// follow-up to #4220), resolved via BD_AGENT_PROFILE env override / config
// key with a safe fallback to conservative (stubbable for tests).
var primeAgentProfile = func() config.AgentProfile {
	return config.GetAgentProfile()
}

// primeHasGitRemote detects if any git remote is configured (stubbable for tests)
var primeHasGitRemote = func() bool {
	rc, err := internalbeads.GetRepoContext()
	if err != nil {
		return false
	}
	cmd := rc.GitCmdCWD(context.Background(), "remote")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}

// primeHasSyncRemote detects if a Dolt sync remote is configured (stubbable for tests)
var primeHasSyncRemote = func() bool {
	return resolveSyncRemote() != ""
}

// primeDoltSyncBullets returns the "bd dolt push"/"bd dolt pull" bullet
// lines for the Sync & Collaboration section, in the requested order, and
// an empty string when no Dolt sync remote is configured (doltSync == false,
// gh#4130). This is independent of the git-remote axis (localOnly) that
// drives git push/pull hints — the two axes must not be conflated
// (gh#4230 review).
func primeDoltSyncBullets(doltSync bool, pushFirst bool) string {
	if !doltSync {
		return ""
	}
	if pushFirst {
		return "- `bd dolt push` - Push beads to Dolt remote\n" +
			"- `bd dolt pull` - Pull beads from Dolt remote\n"
	}
	return "- `bd dolt pull` - Pull beads updates from Dolt remote\n" +
		"- `bd dolt push` - Push beads to Dolt remote\n"
}

// getRedirectNotice returns a notice string if beads is redirected
func getRedirectNotice(verbose bool) string {
	redirectInfo := beads.GetRedirectInfo()
	if !redirectInfo.IsRedirected {
		return ""
	}

	if verbose {
		return fmt.Sprintf(`> ⚠️ **Redirected**: Local .beads → %s
> You share issues with other clones using this redirect.

`, redirectInfo.TargetDir)
	}
	return fmt.Sprintf("**Note**: Beads redirected to %s (shared with other clones)\n\n", redirectInfo.TargetDir)
}

// Package ui provides terminal styling for beads CLI output.
// Uses the Ayu color theme with adaptive light/dark mode support.
package ui

import (
	"fmt"
	"image/color"
	"os"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/jonbaldie/beads/internal/types"
)

const darkBackgroundEnv = "BD_UI_DARK_BACKGROUND"

func init() {
	if !ShouldUseColor() {
		return
	}
	// Detect dark background for adaptive colors.
	// Only probed when color is enabled (prevents OSC 11 leaks in hook contexts).
	setDarkBackground(lipgloss.HasDarkBackground(os.Stdin, os.Stdout))
}

// DisableColors makes all style accessors return plain styles.
// Called from hook contexts to prevent ANSI escape sequence leaks.
func DisableColors() {
	_ = os.Setenv("NO_COLOR", "1")
}

func setDarkBackground(isDark bool) {
	value := "0"
	if isDark {
		value = "1"
	}
	_ = os.Setenv(darkBackgroundEnv, value)
}

// IsAgentMode returns true if the CLI is running in agent-optimized mode.
// This is triggered by:
//   - BD_AGENT_MODE=1 environment variable (explicit)
//   - CLAUDE_CODE environment variable (auto-detect Claude Code)
//
// Agent mode provides ultra-compact output optimized for LLM context windows.
func IsAgentMode() bool {
	if os.Getenv("BD_AGENT_MODE") == "1" {
		return true
	}
	// Auto-detect Claude Code environment
	if os.Getenv("CLAUDE_CODE") != "" {
		return true
	}
	return false
}

// Ayu theme color palette.
// Dark: https://terminalcolors.com/themes/ayu/dark/
// Light: https://terminalcolors.com/themes/ayu/light/
// Source: https://github.com/ayu-theme/ayu-colors
func semanticColor(light, dark string) color.Color {
	if !ShouldUseColor() {
		return lipgloss.NoColor{}
	}
	if os.Getenv(darkBackgroundEnv) == "1" {
		return lipgloss.Color(dark)
	}
	return lipgloss.Color(light)
}

func styleWithForeground(foreground color.Color) lipgloss.Style {
	if _, ok := foreground.(lipgloss.NoColor); ok {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Foreground(foreground)
}

func ColorPass() color.Color   { return semanticColor("#86b300", "#c2d94c") }
func ColorWarn() color.Color   { return semanticColor("#f2ae49", "#ffb454") }
func ColorFail() color.Color   { return semanticColor("#f07171", "#f07178") }
func ColorMuted() color.Color  { return semanticColor("#828c99", "#6c7680") }
func ColorAccent() color.Color { return semanticColor("#399ee6", "#59c2ff") }

func ColorStatusOpen() color.Color       { return lipgloss.NoColor{} }
func ColorStatusInProgress() color.Color { return semanticColor("#f2ae49", "#ffb454") }
func ColorStatusClosed() color.Color     { return semanticColor("#9099a1", "#8090a0") }
func ColorStatusBlocked() color.Color    { return semanticColor("#f07171", "#f26d78") }
func ColorStatusPinned() color.Color     { return semanticColor("#d2a6ff", "#d2a6ff") }
func ColorStatusHooked() color.Color     { return semanticColor("#59c2ff", "#59c2ff") }

func ColorPriorityP0() color.Color { return semanticColor("#f07171", "#f07178") }
func ColorPriorityP1() color.Color { return semanticColor("#ff8f40", "#ff8f40") }
func ColorPriorityP2() color.Color { return semanticColor("#e6b450", "#e6b450") }
func ColorPriorityP3() color.Color { return lipgloss.NoColor{} }
func ColorPriorityP4() color.Color { return lipgloss.NoColor{} }

func ColorTypeBug() color.Color     { return semanticColor("#f07171", "#f26d78") }
func ColorTypeFeature() color.Color { return lipgloss.NoColor{} }
func ColorTypeTask() color.Color    { return lipgloss.NoColor{} }
func ColorTypeEpic() color.Color    { return semanticColor("#d2a6ff", "#d2a6ff") }
func ColorTypeChore() color.Color   { return lipgloss.NoColor{} }
func ColorID() color.Color          { return lipgloss.NoColor{} }

func PassStyle() lipgloss.Style   { return styleWithForeground(ColorPass()) }
func WarnStyle() lipgloss.Style   { return styleWithForeground(ColorWarn()) }
func FailStyle() lipgloss.Style   { return styleWithForeground(ColorFail()) }
func MutedStyle() lipgloss.Style  { return styleWithForeground(ColorMuted()) }
func AccentStyle() lipgloss.Style { return styleWithForeground(ColorAccent()) }
func IDStyle() lipgloss.Style     { return styleWithForeground(ColorID()) }

func StatusOpenStyle() lipgloss.Style       { return styleWithForeground(ColorStatusOpen()) }
func StatusInProgressStyle() lipgloss.Style { return styleWithForeground(ColorStatusInProgress()) }
func StatusClosedStyle() lipgloss.Style     { return styleWithForeground(ColorStatusClosed()) }
func StatusBlockedStyle() lipgloss.Style    { return styleWithForeground(ColorStatusBlocked()) }
func StatusPinnedStyle() lipgloss.Style     { return styleWithForeground(ColorStatusPinned()) }
func StatusHookedStyle() lipgloss.Style     { return styleWithForeground(ColorStatusHooked()) }

func priorityStyle(foreground color.Color, bold bool) lipgloss.Style {
	style := styleWithForeground(foreground)
	if bold && ShouldUseColor() {
		return style.Bold(true)
	}
	return style
}

func PriorityP0Style() lipgloss.Style { return priorityStyle(ColorPriorityP0(), true) }
func PriorityP1Style() lipgloss.Style { return priorityStyle(ColorPriorityP1(), false) }
func PriorityP2Style() lipgloss.Style { return priorityStyle(ColorPriorityP2(), false) }
func PriorityP3Style() lipgloss.Style { return priorityStyle(ColorPriorityP3(), false) }
func PriorityP4Style() lipgloss.Style { return priorityStyle(ColorPriorityP4(), false) }

func TypeBugStyle() lipgloss.Style     { return styleWithForeground(ColorTypeBug()) }
func TypeFeatureStyle() lipgloss.Style { return styleWithForeground(ColorTypeFeature()) }
func TypeTaskStyle() lipgloss.Style    { return styleWithForeground(ColorTypeTask()) }
func TypeEpicStyle() lipgloss.Style    { return styleWithForeground(ColorTypeEpic()) }
func TypeChoreStyle() lipgloss.Style   { return styleWithForeground(ColorTypeChore()) }

func CategoryStyle() lipgloss.Style {
	if !ShouldUseColor() {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Bold(true).Foreground(ColorAccent())
}

func BoldStyle() lipgloss.Style {
	if !ShouldUseColor() {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Bold(true)
}

// CommandStyle returns command names with subtle adaptive contrast.
func CommandStyle() lipgloss.Style {
	return styleWithForeground(semanticColor("#5c6166", "#bfbdb6"))
}

// Status icons - consistent semantic indicators
const (
	IconPass = "✓"
	IconWarn = "⚠"
	IconFail = "✖"
	IconSkip = "-"
	IconInfo = "ℹ"
)

// Issue status icons - used consistently across all commands
// Design principle: icons > text labels for scannability
// IMPORTANT: Use small Unicode symbols, NOT emoji-style icons (🔴🟠 etc.)
// Emoji blobs cause cognitive overload and break visual consistency
const (
	StatusIconOpen       = "○" // available to work (hollow circle)
	StatusIconInProgress = "◐" // active work (half-filled)
	StatusIconBlocked    = "●" // needs attention (filled circle)
	StatusIconClosed     = "✓" // completed (checkmark)
	StatusIconDeferred   = "❄" // scheduled for later (snowflake)
	StatusIconPinned     = "📌" // elevated priority
	StatusIconCustom     = "◇" // custom/uncategorized status (diamond)
)

// RenderStatusIcon returns the appropriate icon for a status with semantic coloring.
// This is the canonical source for status icon rendering - use this everywhere.
// For custom statuses, call RenderStatusIconWithCategory for category-aware rendering.
func RenderStatusIcon(status string) string {
	switch status {
	case "open":
		return StatusIconOpen // no color - available but not urgent
	case "in_progress":
		return StatusInProgressStyle().Render(StatusIconInProgress)
	case "blocked":
		return StatusBlockedStyle().Render(StatusIconBlocked)
	case "closed":
		return StatusClosedStyle().Render(StatusIconClosed)
	case "deferred":
		return MutedStyle().Render(StatusIconDeferred)
	case "pinned":
		return StatusPinnedStyle().Render(StatusIconPinned)
	default:
		return StatusIconCustom // custom/unknown status
	}
}

// RenderStatusIconWithCategory returns the icon for a status, using category
// to determine icon/color for custom statuses.
func RenderStatusIconWithCategory(status string, category types.StatusCategory) string {
	if icon := RenderStatusIcon(status); icon != StatusIconCustom {
		return icon
	}
	return renderCategoryStatusIcon(category)
}

func renderCategoryStatusIcon(category types.StatusCategory) string {
	switch category {
	case types.CategoryActive:
		return StatusIconOpen
	case types.CategoryWIP:
		return StatusInProgressStyle().Render(StatusIconInProgress)
	case types.CategoryDone:
		return StatusClosedStyle().Render(StatusIconClosed)
	case types.CategoryFrozen:
		return MutedStyle().Render(StatusIconDeferred)
	default:
		return StatusIconCustom
	}
}

// GetStatusIcon returns just the icon character without styling
// Useful when you need to apply custom styling or for non-TTY output
func GetStatusIcon(status string) string {
	switch status {
	case "open":
		return StatusIconOpen
	case "in_progress":
		return StatusIconInProgress
	case "blocked":
		return StatusIconBlocked
	case "closed":
		return StatusIconClosed
	case "deferred":
		return StatusIconDeferred
	case "pinned":
		return StatusIconPinned
	default:
		return StatusIconCustom
	}
}

// GetStatusIconWithCategory returns the icon character for a status using category fallback.
func GetStatusIconWithCategory(status string, category types.StatusCategory) string {
	if icon := GetStatusIcon(status); icon != StatusIconCustom {
		return icon
	}
	return getCategoryStatusIcon(category)
}

func getCategoryStatusIcon(category types.StatusCategory) string {
	switch category {
	case types.CategoryActive:
		return StatusIconOpen
	case types.CategoryWIP:
		return StatusIconInProgress
	case types.CategoryDone:
		return StatusIconClosed
	case types.CategoryFrozen:
		return StatusIconDeferred
	default:
		return StatusIconCustom
	}
}

// GetStatusStyle returns the lipgloss style for a given status
// Use this when you need to apply the semantic color to custom text
// Example: ui.GetStatusStyle("in_progress").Render(myCustomText)
func GetStatusStyle(status string) lipgloss.Style {
	switch status {
	case "in_progress":
		return StatusInProgressStyle()
	case "blocked":
		return StatusBlockedStyle()
	case "closed":
		return StatusClosedStyle()
	case "deferred":
		return MutedStyle()
	case "pinned":
		return StatusPinnedStyle()
	case "hooked":
		return StatusHookedStyle()
	default: // open and others - no special styling
		return lipgloss.NewStyle()
	}
}

// Tree characters for hierarchical display
const (
	TreeChild  = "⎿ "  // child indicator
	TreeLast   = "└─ " // last child / detail line
	TreeIndent = "  "  // 2-space indent per level
)

// Separators
const (
	SeparatorLight = "──────────────────────────────────────────"
	SeparatorHeavy = "══════════════════════════════════════════"
)

// RenderPass renders text with pass (green) styling
func RenderPass(s string) string {
	return PassStyle().Render(s)
}

// RenderWarn renders text with warning (yellow) styling
func RenderWarn(s string) string {
	return WarnStyle().Render(s)
}

// RenderFail renders text with fail (red) styling
func RenderFail(s string) string {
	return FailStyle().Render(s)
}

// RenderMuted renders text with muted (gray) styling
func RenderMuted(s string) string {
	return MutedStyle().Render(s)
}

// RenderAccent renders text with accent (blue) styling
func RenderAccent(s string) string {
	return AccentStyle().Render(s)
}

// RenderCategory renders a category header in uppercase with accent color
func RenderCategory(s string) string {
	return CategoryStyle().Render(strings.ToUpper(s))
}

// RenderSeparator renders the light separator line in muted color
func RenderSeparator() string {
	return MutedStyle().Render(SeparatorLight)
}

// RenderPassIcon renders the pass icon with styling
func RenderPassIcon() string {
	return PassStyle().Render(IconPass)
}

// RenderWarnIcon renders the warning icon with styling
func RenderWarnIcon() string {
	return WarnStyle().Render(IconWarn)
}

// RenderFailIcon renders the fail icon with styling
func RenderFailIcon() string {
	return FailStyle().Render(IconFail)
}

// RenderSkipIcon renders the skip icon with styling
func RenderSkipIcon() string {
	return MutedStyle().Render(IconSkip)
}

// RenderInfoIcon renders the info icon with styling
func RenderInfoIcon() string {
	return AccentStyle().Render(IconInfo)
}

// === Issue Component Renderers ===

// RenderID renders an issue ID with semantic styling
func RenderID(id string) string {
	return IDStyle().Render(id)
}

// RenderStatus renders a status with semantic styling
// in_progress/blocked/pinned get color; open/closed use standard text
func RenderStatus(status string) string {
	switch status {
	case "in_progress":
		return StatusInProgressStyle().Render(status)
	case "blocked":
		return StatusBlockedStyle().Render(status)
	case "pinned":
		return StatusPinnedStyle().Render(status)
	case "hooked":
		return StatusHookedStyle().Render(status)
	case "closed":
		return StatusClosedStyle().Render(status)
	default: // open and others
		return StatusOpenStyle().Render(status)
	}
}

// RenderPriority renders a priority level with semantic styling.
// Format: P0 (label only). Status blocked uses ● (StatusIconBlocked); reusing
// that glyph for priority made agents misread "● P3" as blocked (GH#4996).
// P0/P1/P2 get color; P3/P4 use standard text.
func RenderPriority(priority int) string {
	label := fmt.Sprintf("P%d", priority)
	switch priority {
	case 0:
		return PriorityP0Style().Render(label)
	case 1:
		return PriorityP1Style().Render(label)
	case 2:
		return PriorityP2Style().Render(label)
	case 3:
		return PriorityP3Style().Render(label)
	case 4:
		return PriorityP4Style().Render(label)
	default:
		return label
	}
}

// RenderPriorityCompact is an alias of RenderPriority (no glyph difference
// remains after GH#4996).
func RenderPriorityCompact(priority int) string {
	return RenderPriority(priority)
}

// RenderType renders an issue type with semantic styling
// bugs and epics get color; all other types use standard text
// Note: Orchestrator-specific types (agent, role, rig) now fall through to default
func RenderType(issueType string) string {
	switch issueType {
	case "bug":
		return TypeBugStyle().Render(issueType)
	case "feature":
		return TypeFeatureStyle().Render(issueType)
	case "task":
		return TypeTaskStyle().Render(issueType)
	case "epic":
		return TypeEpicStyle().Render(issueType)
	case "chore":
		return TypeChoreStyle().Render(issueType)
	default:
		return issueType
	}
}

// RenderIssueCompact renders a compact one-line issue summary
// Format: ID [Priority] [Type] Status - Title
// When status is "closed", the entire line is dimmed to show it's done
func RenderIssueCompact(id string, priority int, issueType, status, title string) string {
	line := fmt.Sprintf("%s [P%d] [%s] %s - %s",
		id, priority, issueType, status, title)
	if status == "closed" {
		// Entire line is dimmed - visually shows "done"
		return StatusClosedStyle().Render(line)
	}
	return fmt.Sprintf("%s [%s] [%s] %s - %s",
		RenderID(id),
		RenderPriority(priority),
		RenderType(issueType),
		RenderStatus(status),
		title,
	)
}

// RenderPriorityForStatus renders priority with color only if not closed
func RenderPriorityForStatus(priority int, status string) string {
	if status == "closed" {
		return fmt.Sprintf("P%d", priority)
	}
	return RenderPriority(priority)
}

// RenderTypeForStatus renders type with color only if not closed
func RenderTypeForStatus(issueType, status string) string {
	if status == "closed" {
		return issueType
	}
	return RenderType(issueType)
}

// RenderClosedLine renders an entire line in the closed/dimmed style
func RenderClosedLine(line string) string {
	return StatusClosedStyle().Render(line)
}

// RenderBold renders text in bold
func RenderBold(s string) string {
	return BoldStyle().Render(s)
}

// RenderCommand renders a command name with subtle styling
func RenderCommand(s string) string {
	return CommandStyle().Render(s)
}

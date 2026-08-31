package main

import (
	"strings"
	"testing"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/jonbaldie/beads/internal/ui"
)

func TestApplyNoColorFlag(t *testing.T) {
	savedFlag := noColorFlag
	t.Cleanup(func() {
		noColorFlag = savedFlag
	})
	t.Setenv("BD_GIT_HOOK", "")
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")

	t.Run("flag unset leaves styles untouched", func(t *testing.T) {
		noColorFlag = false
		applyNoColorFlag()
		if _, ok := ui.ColorAccent().(lipgloss.NoColor); ok {
			t.Error("colors disabled even though --no-color was not set")
		}
	})

	t.Run("flag set disables colors", func(t *testing.T) {
		noColorFlag = true
		applyNoColorFlag()
		if _, ok := ui.ColorAccent().(lipgloss.NoColor); !ok {
			t.Errorf("ColorAccent not reset to NoColor, got %T", ui.ColorAccent())
		}
		if out := ui.AccentStyle().Render("hi"); strings.ContainsRune(out, '\x1b') {
			t.Errorf("AccentStyle still emits ANSI after --no-color: %q", out)
		}
	})
}

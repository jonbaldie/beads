package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func installCodexNativeHooks(env codexEnv, global bool) error {
	if global {
		_, _ = fmt.Fprintln(env.stdout, "Installing Codex native hooks globally...")
	} else {
		_, _ = fmt.Fprintln(env.stdout, "Installing Codex native hooks for this project...")
	}
	if err := installCodexFeatureFlag(env, global); err != nil {
		return err
	}
	if codexBeadsPluginEnabled(env, global) {
		_, _ = fmt.Fprintln(env.stdout, "✓ Beads Codex plugin detected — hooks are plugin-managed, skipping hooks.json fallback")
		return nil
	}
	if err := installCodexHooksJSON(env, global); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(env.stdout, "✓ Codex native hooks installed")
	return nil
}

func checkCodexNativeHooks(env codexEnv, global bool) error {
	if !codexConfigHasHooksFeature(env, global) {
		_, _ = fmt.Fprintf(env.stdout, "✗ Codex hooks feature flag not enabled: %s\n", codexConfigPath(env, global))
		_, _ = fmt.Fprintf(env.stdout, "  Run: %s\n", codexSetupCommand(global))
		return errCodexFeatureMissing
	}
	if codexBeadsPluginEnabled(env, global) {
		_, _ = fmt.Fprintln(env.stdout, "✓ Codex native hooks provided by beads plugin (plugin-managed)")
		return nil
	}
	if !codexHooksJSONCurrent(env, global) {
		_, _ = fmt.Fprintf(env.stdout, "⚠ Codex hooks missing or stale: %s\n", codexHooksPath(env, global))
		_, _ = fmt.Fprintf(env.stdout, "  Run: %s\n", codexSetupCommand(global))
		return errCodexHooksStale
	}
	_, _ = fmt.Fprintf(env.stdout, "✓ Codex native hooks installed: %s\n", codexHooksPath(env, global))
	return nil
}

func removeCodexNativeHooks(env codexEnv, global bool) error {
	if err := removeCodexHooksJSON(env, global); err != nil {
		return err
	}
	return removeCodexFeatureFlag(env, global)
}

func installCodexFeatureFlag(env codexEnv, global bool) error {
	path := codexConfigPath(env, global)
	if err := env.ensureDir(filepath.Dir(path), 0o755); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: %v\n", err)
		return err
	}
	current := ""
	if data, err := env.readFile(path); err == nil {
		current = string(data)
	} else if !os.IsNotExist(err) {
		_, _ = fmt.Fprintf(env.stderr, "Error: read %s: %v\n", path, err)
		return err
	}
	next := upsertCodexHooksFeature(current)
	if err := env.writeFile(path, []byte(next)); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: write %s: %v\n", path, err)
		return err
	}
	return nil
}

func removeCodexFeatureFlag(env codexEnv, global bool) error {
	path := codexConfigPath(env, global)
	data, err := env.readFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: read %s: %v\n", path, err)
		return err
	}
	next := removeCodexHooksFeature(string(data))
	if err := env.writeFile(path, []byte(next)); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: write %s: %v\n", path, err)
		return err
	}
	return nil
}

func codexConfigHasHooksFeature(env codexEnv, global bool) bool {
	data, err := env.readFile(codexConfigPath(env, global))
	if err != nil {
		return false
	}
	return codexHooksFeatureEnabled(string(data))
}

func codexBeadsPluginEnabled(env codexEnv, global bool) bool {
	paths := []string{codexConfigPath(env, global)}
	if !global {
		paths = append(paths, codexConfigPath(env, true))
	}
	for _, path := range paths {
		data, err := env.readFile(path)
		if err != nil {
			continue
		}
		if codexConfigEnablesBeadsPlugin(string(data)) {
			return true
		}
	}
	return false
}

func codexConfigEnablesBeadsPlugin(content string) bool {
	inBeadsPlugin := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			table := strings.Trim(trimmed, "[]")
			// Plugin tables are [plugins.<name>] or [plugins."<name>@<marketplace>"]
			// (TOML quotes a key containing "@"). Match the <name> segment
			// exactly: a substring test (GH#4244) mistakes any "*beads*" plugin
			// (e.g. design-to-beads) for the beads hook plugin and wrongly skips
			// the hooks write.
			inBeadsPlugin = false
			if rest, ok := strings.CutPrefix(table, "plugins."); ok {
				name, _, _ := strings.Cut(strings.ToLower(strings.Trim(rest, "\"'")), "@")
				inBeadsPlugin = name == "beads"
			}
			continue
		}
		if inBeadsPlugin && codexConfigLineKey(trimmed) == "enabled" {
			parts := strings.SplitN(trimmed, "=", 2)
			return len(parts) == 2 && strings.TrimSpace(parts[1]) == "true"
		}
	}
	return false
}

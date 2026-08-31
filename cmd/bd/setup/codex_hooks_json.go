package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func installCodexHooksJSON(env codexEnv, global bool) error {
	path := codexHooksPath(env, global)
	if err := env.ensureDir(filepath.Dir(path), 0o755); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: %v\n", err)
		return err
	}
	current := map[string]interface{}{}
	data, err := env.readFile(path)
	if err != nil && !os.IsNotExist(err) {
		_, _ = fmt.Fprintf(env.stderr, "Error: read %s: %v\n", path, err)
		return err
	}
	if err == nil {
		if current, err = parseHooksJSON(data); err != nil {
			_, _ = fmt.Fprintf(env.stderr, "Error: parse %s: %v\n", path, err)
			return err
		}
	}
	upsertCodexManagedHooks(current)
	out, err := marshalHooksJSON(current)
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: marshal %s: %v\n", path, err)
		return err
	}
	if err := env.writeFile(path, out); err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: write %s: %v\n", path, err)
		return err
	}
	return nil
}

func removeCodexHooksJSON(env codexEnv, global bool) error {
	path := codexHooksPath(env, global)
	data, err := env.readFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: read %s: %v\n", path, err)
		return err
	}
	current, err := parseHooksJSON(data)
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "Error: parse %s: %v\n", path, err)
		return err
	}
	removeCodexManagedHooks(current)
	if len(current) == 0 {
		if err := env.removeFile(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	next, err := marshalHooksJSON(current)
	if err != nil {
		return err
	}
	return env.writeFile(path, next)
}

func codexHooksJSONCurrent(env codexEnv, global bool) bool {
	data, err := env.readFile(codexHooksPath(env, global))
	if err != nil {
		return false
	}
	current, err := parseHooksJSON(data)
	if err != nil {
		return false
	}
	return codexManagedHooksCurrent(current)
}

func upsertCodexHooksFeature(content string) string {
	lines, featuresSeen, flagSeen := rewriteCodexHooksLines(strings.Split(content, "\n"))
	if featuresSeen && !flagSeen {
		lines = insertCodexHooksFlag(lines)
	}
	next := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if !featuresSeen {
		next = appendCodexFeaturesSection(next)
	}
	return next + "\n"
}

func rewriteCodexHooksLines(lines []string) ([]string, bool, bool) {
	inFeatures := false
	featuresSeen := false
	flagSeen := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inFeatures = trimmed == "[features]"
			if inFeatures {
				featuresSeen = true
			}
			continue
		}
		key := codexConfigLineKey(trimmed)
		if inFeatures && key == "codex_hooks" {
			lines[i] = ""
			continue
		}
		if inFeatures && key == "hooks" {
			lines[i] = "hooks = true"
			flagSeen = true
		}
	}
	return lines, featuresSeen, flagSeen
}

func insertCodexHooksFlag(lines []string) []string {
	out := make([]string, 0, len(lines)+1)
	inserted := false
	inFeatures := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if inFeatures && !inserted {
				out = append(out, "hooks = true")
				inserted = true
			}
			inFeatures = trimmed == "[features]"
		}
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	if inFeatures && !inserted {
		out = append(out, "hooks = true")
	}
	return out
}

func appendCodexFeaturesSection(next string) string {
	if strings.TrimSpace(next) != "" {
		next += "\n\n"
	}
	return next + "[features]\nhooks = true"
}

func removeCodexHooksFeature(content string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	inFeatures := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inFeatures = trimmed == "[features]"
			out = append(out, line)
			continue
		}
		key := codexConfigLineKey(trimmed)
		if inFeatures && (key == "hooks" || key == "codex_hooks") {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

func codexHooksFeatureEnabled(content string) bool {
	inFeatures := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inFeatures = trimmed == "[features]"
			continue
		}
		if inFeatures && codexConfigLineKey(trimmed) == "hooks" {
			parts := strings.SplitN(trimmed, "=", 2)
			return len(parts) == 2 && strings.TrimSpace(parts[1]) == "true"
		}
	}
	return false
}

func codexConfigLineKey(trimmed string) string {
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	parts := strings.SplitN(trimmed, "=", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func codexManagedHooks() map[string]interface{} {
	return map[string]interface{}{
		"SessionStart":     []interface{}{codexHookEntry("startup|resume|clear", "bd codex-hook SessionStart", "Loading Beads context")},
		"PreCompact":       []interface{}{codexHookEntry("manual|auto", "bd codex-hook PreCompact", "Checking Beads context")},
		"PostCompact":      []interface{}{codexHookEntry("manual|auto", "bd codex-hook PostCompact", "Scheduling Beads context refresh")},
		"UserPromptSubmit": []interface{}{codexHookEntry("", "bd codex-hook UserPromptSubmit", "Refreshing Beads context")},
	}
}

func codexHookEntry(matcher, command, status string) map[string]interface{} {
	entry := map[string]interface{}{
		"hooks": []interface{}{
			map[string]interface{}{
				"type":          "command",
				"command":       command,
				"statusMessage": status,
			},
		},
	}
	if matcher != "" {
		entry["matcher"] = matcher
	}
	return entry
}

func upsertCodexManagedHooks(config map[string]interface{}) {
	hooks, ok := config["hooks"].(map[string]interface{})
	if !ok {
		hooks = map[string]interface{}{}
		config["hooks"] = hooks
	}
	for event, entries := range codexManagedHooks() {
		removeCodexManagedHookEvent(hooks, event)
		hooks[event] = append(toInterfaceSlice(hooks[event]), entries.([]interface{})...)
	}
}

func removeCodexManagedHooks(config map[string]interface{}) {
	hooks, ok := config["hooks"].(map[string]interface{})
	if !ok {
		return
	}
	for event := range codexManagedHooks() {
		removeCodexManagedHookEvent(hooks, event)
	}
	if len(hooks) == 0 {
		delete(config, "hooks")
	}
}

func codexManagedHooksCurrent(config map[string]interface{}) bool {
	hooks, ok := config["hooks"].(map[string]interface{})
	if !ok {
		return false
	}
	for _, entry := range toInterfaceSlice(hooks["SessionStart"]) {
		if codexHookEntryHasLegacySessionStart(entry) {
			return false
		}
	}
	for event, wantEntries := range codexManagedHooks() {
		want, _ := wantEntries.([]interface{})
		for _, wantEntry := range want {
			if !codexHookEntriesContain(toInterfaceSlice(hooks[event]), wantEntry) {
				return false
			}
		}
	}
	return true
}

// codexBeadsHooksPresent reports whether a parsed hooks.json config contains any
// bd-managed Codex hook entry ("bd codex-hook " command) or the exact legacy
// SessionStart pipeline. It mirrors
// cursorBeadsHooksPresent so codexIntegrationInstalled can detect a hooks-only
// Codex install (no AGENTS.md section) the same way cursorIntegrationInstalledAt
// detects a hooks-only Cursor install.
func codexBeadsHooksPresent(config map[string]interface{}) bool {
	hooks, ok := config["hooks"].(map[string]interface{})
	if !ok {
		return false
	}
	for event := range codexManagedHooks() {
		for _, entry := range toInterfaceSlice(hooks[event]) {
			if codexHookEntryManaged(entry) || (event == "SessionStart" && codexHookEntryHasLegacySessionStart(entry)) {
				return true
			}
		}
	}
	return false
}

func removeCodexManagedHookEvent(hooks map[string]interface{}, event string) {
	entries := toInterfaceSlice(hooks[event])
	filtered := entries[:0]
	for _, entry := range entries {
		if codexHookEntryManaged(entry) {
			continue
		}
		if event == "SessionStart" && !removeCodexLegacySessionStartCommand(entry) {
			continue
		}
		filtered = append(filtered, entry)
	}
	if len(filtered) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = filtered
	}
}

func codexHookEntriesContain(entries []interface{}, want interface{}) bool {
	wantJSON, _ := json.Marshal(want)
	for _, entry := range entries {
		gotJSON, _ := json.Marshal(entry)
		if string(gotJSON) == string(wantJSON) {
			return true
		}
	}
	return false
}

func codexHookEntryManaged(entry interface{}) bool {
	entryMap, ok := entry.(map[string]interface{})
	if !ok {
		return false
	}
	commands := toInterfaceSlice(entryMap["hooks"])
	for _, command := range commands {
		commandMap, ok := command.(map[string]interface{})
		if !ok {
			continue
		}
		if commandString, _ := commandMap["command"].(string); strings.HasPrefix(commandString, "bd codex-hook ") {
			return true
		}
	}
	return false
}

func codexHookEntryHasLegacySessionStart(entry interface{}) bool {
	entryMap, ok := entry.(map[string]interface{})
	if !ok {
		return false
	}
	for _, hook := range toInterfaceSlice(entryMap["hooks"]) {
		hookMap, ok := hook.(map[string]interface{})
		if !ok {
			continue
		}
		command, _ := hookMap["command"].(string)
		if strings.TrimSpace(command) == codexLegacySessionStartCommand {
			return true
		}
	}
	return false
}

// removeCodexLegacySessionStartCommand removes only the exact historical
// SessionStart pipeline from an entry. It reports whether the entry still has
// hooks and should be preserved.
func removeCodexLegacySessionStartCommand(entry interface{}) bool {
	entryMap, ok := entry.(map[string]interface{})
	if !ok {
		return true
	}
	hooks := toInterfaceSlice(entryMap["hooks"])
	if hooks == nil {
		return true
	}
	filtered := hooks[:0]
	removed := false
	for _, hook := range hooks {
		hookMap, ok := hook.(map[string]interface{})
		if ok {
			command, _ := hookMap["command"].(string)
			if strings.TrimSpace(command) == codexLegacySessionStartCommand {
				removed = true
				continue
			}
		}
		filtered = append(filtered, hook)
	}
	if !removed {
		return true
	}
	if len(filtered) == 0 {
		return false
	}
	entryMap["hooks"] = filtered
	return true
}

func toInterfaceSlice(value interface{}) []interface{} {
	switch typed := value.(type) {
	case []interface{}:
		return typed
	default:
		return nil
	}
}

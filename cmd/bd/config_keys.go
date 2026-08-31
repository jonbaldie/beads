package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/tracker"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

var configSetManyCmd = &cobra.Command{
	Use:   "set-many <key=value>...",
	Short: "Set multiple configuration values in one operation",
	Long: `Set multiple configuration values at once with a single auto-commit and auto-push.

Each argument must be in key=value format. All values are validated before
any writes occur. This is faster and less noisy than separate 'bd config set'
calls, especially in CI.

Examples:
  bd config set-many ado.state_map.open=New ado.state_map.closed=Closed
  bd config set-many jira.url=https://example.atlassian.net jira.project=PROJ`,
	Args:          cobra.MinimumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("config-set-many")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		type kvPair struct {
			key, value string
		}
		pairs := make([]kvPair, 0, len(args))
		for _, arg := range args {
			idx := strings.Index(arg, "=")
			if idx <= 0 {
				return HandleError("invalid argument %q (expected key=value format)", arg)
			}
			pairs = append(pairs, kvPair{key: arg[:idx], value: arg[idx+1:]})
		}

		for _, p := range pairs {
			// The same refusal `bd config set` makes, and it was MISSING here:
			// `bd config set-many issue_prefix=x` re-prefixed the workspace
			// behind the guard the single-key verb enforces. This verb does not
			// go through issueops.WorkspaceConfig (see the write below), so it
			// makes the refusal itself, before any pair is written.
			if msg, rejected := rejectProtectedConfigKey(p.key); rejected {
				fmt.Fprintln(os.Stderr, msg)
				return SilentExit()
			}
			if p.key == "beads.role" {
				validRoles := map[string]bool{"maintainer": true, "contributor": true}
				if !validRoles[p.value] {
					return HandleError("invalid role %q (valid values: maintainer, contributor)", p.value)
				}
			}
			if p.key == "status.custom" && p.value != "" {
				if _, err := types.ParseCustomStatusConfig(p.value); err != nil {
					return HandleError("invalid status.custom value: %v", err)
				}
			}
			if strings.HasPrefix(p.key, "storage-class.") {
				if err := validateStorageClassConfig(p.key, p.value); err != nil {
					return HandleError("%v", err)
				}
			}
		}

		var yamlPairs, gitPairs, dbPairs []kvPair
		for _, p := range pairs {
			if config.IsYamlOnlyKey(p.key) {
				yamlPairs = append(yamlPairs, p)
			} else if p.key == "beads.role" {
				gitPairs = append(gitPairs, p)
			} else {
				dbPairs = append(dbPairs, p)
			}
		}

		if !forceGitTrackedEnabled(cmd) {
			for _, p := range yamlPairs {
				if err := config.CheckSecretKeyGitSafety(p.key); err != nil {
					return HandleError("%v", err)
				}
			}
		}

		for _, p := range yamlPairs {
			var setErr error
			if config.IsUserGlobalKey(p.key) {
				setErr = config.SetUserYamlConfig(p.key, p.value)
			} else {
				setErr = config.SetYamlConfig(p.key, p.value)
			}
			if setErr != nil {
				return HandleError("setting config %s: %v", p.key, setErr)
			}
		}

		for _, p := range gitPairs {
			cmd := exec.Command("git", "config", "beads.role", p.value) //nolint:gosec // value is validated against allowlist above
			if err := cmd.Run(); err != nil {
				return HandleError("setting %s in git config: %v", p.key, err)
			}
		}

		// SET-MANY IS NOT ON issueops.WorkspaceConfig, and the reason is the
		// property TestProxiedServerConfigSetMany pins: the whole batch is ONE
		// Dolt commit, which is the entire point of the verb ("faster and less
		// noisy than separate calls, especially in CI"). The role writes one
		// setting per call and commits each, so routing this through it would
		// turn a three-key batch into three commits.
		if len(dbPairs) > 0 {
			if usesProxiedServer() {
				keys := make([]string, len(dbPairs))
				values := make([]string, len(dbPairs))
				for i, p := range dbPairs {
					keys[i] = p.key
					values[i] = p.value
				}
				if err := runConfigSetManyProxiedServer(getRootContext(), keys, values); err != nil {
					return err
				}
			} else {
				if err := ensureDirectMode("config set-many requires direct database access"); err != nil {
					return HandleError("%v", err)
				}
				for _, p := range dbPairs {
					if err := getStore().SetConfig(getRootContext(), p.key, p.value); err != nil {
						return HandleError("setting config %s: %v", p.key, err)
					}
				}
				commandDidWrite.Store(true)
			}
		}

		if isJSONOutput() {
			results := make([]map[string]string, 0, len(pairs))
			for _, p := range pairs {
				location := "database"
				if config.IsUserGlobalKey(p.key) {
					location = config.UserConfigYamlDisplayPath()
				} else if config.IsYamlOnlyKey(p.key) {
					location = "config.yaml"
				} else if p.key == "beads.role" {
					location = "git config"
				}
				results = append(results, map[string]string{
					"key":      p.key,
					"value":    p.value,
					"location": location,
				})
			}
			if err := outputJSON(results); err != nil {
				return err
			}
		} else {
			for _, p := range pairs {
				location := ""
				if config.IsUserGlobalKey(p.key) {
					location = fmt.Sprintf(" (in %s)", config.UserConfigYamlDisplayPath())
				} else if config.IsYamlOnlyKey(p.key) {
					location = " (in config.yaml)"
				} else if p.key == "beads.role" {
					location = " (in git config)"
				}
				fmt.Printf("Set %s = %s%s\n", p.key, p.value, location)
			}
		}
		return nil
	},
}

// recognizedConfigPrefixes lists valid top-level config namespaces.
// Keys under custom.* are always accepted (user-extensible).
//
// Tracker namespaces (jira., linear., github., ado., ...) are NOT listed here:
// they are derived from the tracker registry at runtime via
// allRecognizedConfigPrefixes, so the recognizer cannot drift out of sync when
// a new tracker is added (GH#4427).
var recognizedConfigPrefixes = []string{
	"export.", "import.", "dolt.", "custom.",
	"status.", "types.", "doctor.suppress.", "routing.", "sync.", "git.",
	"directory.", "repos.", "external_projects.", "validation.",
	"hierarchy.", "ai.", "backup.", "federation.", "metrics.", "agent.",
	"claim.", "storage-class.",
}

// validateStorageClassConfig validates a storage-class.<type> per-type
// default at config-set time (Protocol v0.1 C-OQ1: values are validated when
// set, not discovered broken at create time). The key suffix must name an
// issue type and the value must be a storage class.
func validateStorageClassConfig(key, value string) error {
	suffix := strings.TrimPrefix(key, "storage-class.")
	if suffix == "" || strings.Contains(suffix, ".") {
		return fmt.Errorf("invalid key %q: expected storage-class.<issue-type> (e.g. storage-class.event)", key)
	}
	// The key suffix must be a canonical, known issue type: create-time lookup
	// keys on the Normalize()d type (resolveStorageClass), so an alias like
	// storage-class.feat or a typo like storage-class.taks would pass set-time
	// validation and then silently never match — the C-OQ1 failure mode this
	// validator exists to prevent.
	issueType := types.IssueType(suffix)
	if canonical := issueType.Normalize(); canonical != issueType {
		return fmt.Errorf("invalid key %q: %q is an alias of %q, and create-time lookup uses the canonical type; set storage-class.%s instead", key, suffix, canonical, canonical)
	}
	if !issueType.IsValidWithCustom(loadEmbeddedCustomTypes()) {
		return fmt.Errorf("invalid key %q: unknown issue type %q (use a built-in type, or add it to types.custom first)", key, suffix)
	}
	if _, err := types.ParseStorageClass(value); err != nil {
		return err
	}
	return nil
}

// allRecognizedConfigPrefixes returns the static namespaces plus the prefix of
// every registered tracker ("ado.", "jira.", ...). Deriving tracker prefixes
// from the registry keeps config-key recognition in sync with the set of
// trackers compiled into bd instead of a hand-maintained allowlist (GH#4427).
func allRecognizedConfigPrefixes() []string {
	names := tracker.List()
	prefixes := make([]string, 0, len(recognizedConfigPrefixes)+len(names))
	prefixes = append(prefixes, recognizedConfigPrefixes...)
	for _, name := range names {
		prefixes = append(prefixes, name+".")
	}
	return prefixes
}

// recognizedConfigKeys lists valid non-namespaced config keys.
var recognizedConfigKeys = map[string]bool{
	"no-db": true, "json": true, "db": true, "actor": true,
	"identity": true, "no-push": true, "no-git-ops": true,
	"node_id":                    true, // replica identity for the lease guard (read from yaml/env, never the DB)
	"create.require-description": true, "beads.role": true,
	"auto_compact_enabled": true, "schema_version": true,
	"output.title-length": true,
	"prime.max-memories":  true, "prime.max-memory-chars": true,
	// The events-journal family. All four are startup settings that land in
	// config.yaml (config.YamlOnlyKeys), and every one of them is documented as
	// a `bd config set` invocation — including the auto-prune opt-out, where an
	// unrecognized-key warning next to a command that DID take effect reads as
	// "that did not work" on the one setting whose whole purpose is to stop bd
	// deleting records.
	"events-journal": true, "events-journal-auto-prune": true,
	"events-journal-retain-days": true, "events-journal-retain-rows": true,
}

func isRecognizedConfigKey(key string) bool {
	if recognizedConfigKeys[key] {
		return true
	}
	for _, prefix := range allRecognizedConfigPrefixes() {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// rejectProtectedConfigKey rejects keys that are owned by a dedicated
// lifecycle command (init/rename) rather than 'bd config set'. The canonical
// example is issue_prefix: 'bd create' reads YAML "issue-prefix"
// then DB "issue_prefix", while 'bd config set' would land in DB
// "issue-prefix" — a third key no reader consults. Accepting either the
// dash or underscore form silently produces a write that looks like it
// succeeded but is never visible to 'bd create'. Reject both and point the
// user at the right command.
func rejectProtectedConfigKey(key string) (string, bool) {
	switch key {
	case "issue_prefix", "issue-prefix":
		return "Error: issue_prefix cannot be set via 'bd config set'.\n" +
			"  - New project:       bd init --prefix <prefix>\n" +
			"  - Fresh clone:       bd bootstrap\n" +
			"  - Rename existing:   bd rename-prefix <new-prefix>", true
	}
	return "", false
}

// suggestConfigKey tries to find a close match for a mistyped key by checking
// if the key's prefix is a known prefix with a typo. Returns empty string if
// no suggestion can be made.
func suggestConfigKey(key string) string {
	parts := strings.SplitN(key, ".", 2)
	if len(parts) < 2 {
		return ""
	}
	prefix := parts[0] + "."

	bestMatch := ""
	bestDist := 3 // max edit distance to suggest
	for _, known := range allRecognizedConfigPrefixes() {
		knownPrefix := strings.TrimSuffix(known, ".")
		d := levenshteinDistance(parts[0], knownPrefix)
		if d > 0 && d < bestDist {
			bestDist = d
			bestMatch = known + parts[1]
		}
	}
	_ = prefix
	return bestMatch
}

func levenshteinDistance(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, min(prev[j]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func init() {
	configSetCmd.Flags().Bool("force-git-tracked", false, "Allow writing secret keys to git-tracked config files (use with caution)")
	configSetManyCmd.Flags().Bool("force-git-tracked", false, "Allow writing secret keys to git-tracked config files (use with caution)")

	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configSetManyCmd)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configListCmd)
	configCmd.AddCommand(configUnsetCmd)
	configCmd.AddCommand(configValidateCmd)
	rootCmd.AddCommand(configCmd)
}

func forceGitTrackedEnabled(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	enabled, err := cmd.Flags().GetBool("force-git-tracked")
	return err == nil && enabled
}

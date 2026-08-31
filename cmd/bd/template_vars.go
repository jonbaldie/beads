package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

// extractVariables finds all {{variable}} patterns in text.
// Handlebars control keywords like "else", "this" are excluded.
func extractVariables(text string) []string {
	matches := variablePattern.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool)
	var vars []string
	for _, match := range matches {
		if len(match) >= 2 && !seen[match[1]] {
			name := match[1]
			// Skip Handlebars control keywords
			if isHandlebarsKeyword(name) {
				continue
			}
			vars = append(vars, name)
			seen[name] = true
		}
	}
	return vars
}

// isHandlebarsKeyword returns true for Handlebars control keywords
// that look like variables but aren't (e.g., "else", "this").
func isHandlebarsKeyword(name string) bool {
	switch name {
	case "else", "this", "root", "index", "key", "first", "last":
		return true
	default:
		return false
	}
}

// extractAllVariables finds all variables across the entire subgraph
func extractAllVariables(subgraph *TemplateSubgraph) []string {
	allText := ""
	for _, issue := range subgraph.Issues {
		allText += issue.Title + " " + issue.Description + " "
		allText += issue.Design + " " + issue.AcceptanceCriteria + " " + issue.Notes + " "
	}
	return extractVariables(allText)
}

// extractRequiredVariables returns only variables that don't have defaults.
// If VarDefs is available (from a cooked formula), uses it to filter out defaulted vars.
// Otherwise, falls back to returning all variables.
func extractRequiredVariables(subgraph *TemplateSubgraph) []string {
	allVars := extractAllVariables(subgraph)

	// If no VarDefs, assume all variables are required (legacy template behavior)
	if subgraph.VarDefs == nil {
		return allVars
	}

	// VarDefs exists (from a cooked formula) - only declared variables matter.
	// Variables in text but NOT in VarDefs are ignored - they're documentation
	// handlebars meant for LLM agents, not formula input variables (gt-ky9loa).
	var required []string
	for _, v := range allVars {
		def, exists := subgraph.VarDefs[v]
		if !exists {
			// Not a declared formula variable - skip (documentation handlebars)
			continue
		}
		// A declared variable is required if it has no default.
		// nil Default = no default specified (must provide).
		// Non-nil Default (including &"") = has explicit default (optional).
		if def.Default == nil {
			required = append(required, v)
		}
	}
	return required
}

// applyVariableDefaults merges formula default values with provided variables.
// Returns a new map with defaults applied for any missing variables.
func applyVariableDefaults(vars map[string]string, subgraph *TemplateSubgraph) map[string]string {
	if subgraph.VarDefs == nil {
		return vars
	}

	result := make(map[string]string)
	for k, v := range vars {
		result[k] = v
	}

	// Apply defaults for missing variables (including empty-string defaults)
	for name, def := range subgraph.VarDefs {
		if _, exists := result[name]; !exists && def.Default != nil {
			result[name] = *def.Default
		}
	}

	return result
}

// substituteVariables replaces {{variable}} with values
func substituteVariables(text string, vars map[string]string) string {
	return variablePattern.ReplaceAllStringFunc(text, func(match string) string {
		// Extract variable name from {{name}}
		name := match[2 : len(match)-2]
		if val, ok := vars[name]; ok {
			return val
		}
		return match // Leave unchanged if not found
	})
}

// substituteMetadataRepo substitutes {{variable}} placeholders in an issue's
// metadata.repo value (SF2 follow-up). A formula gate step's `repo` selector
// (e.g. repo = "{{gate_repo}}") is stored literally on the persisted proto's
// metadata by createGateIssue/persistCookFormula - `bd cook --persist` keeps
// the proto reusable across pours rather than substituting at compile time.
// Substitution instead needs to happen at the same point as every other
// var-bearing issue field (Title, Description, AwaitID, ...): here, in
// cloneSubgraphInto, when a proto is poured/spawned into real issues.
//
// Restricted to gh:* gate types (SF4), matching createGateIssue's write-side
// rule: `repo` on a human/timer/bead gate is unrelated, ordinary metadata,
// not a GitHub repo selector, so it must not be touched here either.
//
// Metadata is arbitrary JSON on any issue, so this only touches a top-level
// string-valued "repo" key; anything else (missing key, non-object, non-
// string value) is left untouched for githubRepoFromIssue to validate at
// check time. The round-trip unmarshals into map[string]json.RawMessage
// rather than map[string]interface{} and replaces only the "repo" entry, so
// every OTHER key's value survives byte-identical - interface{} would
// mangle numbers to float64, and a full re-marshal of decoded values can
// reshuffle nested object keys and HTML-escape strings that were never
// touched.
func substituteMetadataRepo(metadata json.RawMessage, awaitType string, vars map[string]string) json.RawMessage {
	if len(metadata) == 0 || !isGitHubGateType(awaitType) {
		return metadata
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &raw); err != nil {
		return metadata
	}

	repoRaw, hasRepo := raw["repo"]
	if !hasRepo {
		return metadata
	}

	var repoStr string
	if err := json.Unmarshal(repoRaw, &repoStr); err != nil {
		// Non-string (e.g. null) repo value: leave untouched for
		// githubRepoFromIssue to reject at check time.
		return metadata
	}

	substituted := substituteVariables(repoStr, vars)
	if substituted == repoStr {
		return metadata
	}

	substitutedJSON, err := marshalNoHTMLEscape(substituted)
	if err != nil {
		return metadata
	}
	raw["repo"] = substitutedJSON

	out, err := marshalNoHTMLEscape(raw)
	if err != nil {
		return metadata
	}
	return out
}

// marshalNoHTMLEscape is json.Marshal without HTML-escaping '<', '>', and
// '&' - the stdlib's json.Marshal escapes them by default (aimed at
// embedding JSON in HTML), which would silently corrupt an unrelated
// metadata value round-tripped through substituteMetadataRepo.
func marshalNoHTMLEscape(v interface{}) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder.Encode appends a trailing newline; callers embed this
	// result as a json.RawMessage value, which must not carry one.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// generateBondedID creates a custom ID for dynamically bonded molecules.
// When bonding a proto to a parent molecule, this generates IDs like:
//   - Root: parent.childref (e.g., "patrol-x7k.arm-ace")
//   - Children: parent.childref.step (e.g., "patrol-x7k.arm-ace.capture")
//
// The childRef is variable-substituted before use.
// Returns empty string if not a bonded operation (opts.ParentID empty).
func generateBondedID(oldID string, rootID string, opts CloneOptions) (string, error) {
	if opts.ParentID == "" {
		return "", nil // Not a bonded operation
	}

	// Substitute variables in childRef
	childRef := substituteVariables(opts.ChildRef, opts.Vars)

	// Validate childRef after substitution
	if childRef == "" {
		return "", fmt.Errorf("childRef is empty after variable substitution")
	}
	if !bondedIDPattern.MatchString(childRef) {
		return "", fmt.Errorf("invalid childRef '%s': must be alphanumeric, dash, underscore, or dot only", childRef)
	}

	if oldID == rootID {
		// Root issue: parent.childref
		newID := fmt.Sprintf("%s.%s", opts.ParentID, childRef)
		return newID, nil
	}

	// Child issue: parent.childref.relative
	// Extract the relative portion of the old ID (part after root)
	relativeID := getRelativeID(oldID, rootID)
	if relativeID == "" {
		// No hierarchical relationship - use a suffix from the old ID to ensure uniqueness.
		// Extract the last part of the old ID (after any prefix or dash)
		suffix := extractIDSuffix(oldID)
		newID := fmt.Sprintf("%s.%s.%s", opts.ParentID, childRef, suffix)
		return newID, nil
	}

	newID := fmt.Sprintf("%s.%s.%s", opts.ParentID, childRef, relativeID)
	return newID, nil
}

// extractIDSuffix extracts a suffix from an ID for use when IDs aren't hierarchical.
// For "patrol-abc123", returns "abc123".
// For "bd-xyz.1", returns "1".
// This ensures child IDs remain unique when bonding.
func extractIDSuffix(id string) string {
	// First try to get the part after the last dot (for hierarchical IDs)
	if lastDot := strings.LastIndex(id, "."); lastDot >= 0 {
		return id[lastDot+1:]
	}
	// Otherwise, get the part after the last dash (for prefix-hash IDs)
	if lastDash := strings.LastIndex(id, "-"); lastDash >= 0 {
		return id[lastDash+1:]
	}
	// Fallback: use the whole ID
	return id
}

// getRelativeID extracts the relative portion of a child ID from its parent.
// For example: getRelativeID("bd-abc.step1.sub", "bd-abc") returns "step1.sub"
// Returns empty string if oldID equals rootID or doesn't start with rootID.
func getRelativeID(oldID, rootID string) string {
	if oldID == rootID {
		return ""
	}
	// Check if oldID starts with rootID followed by a dot
	prefix := rootID + "."
	if strings.HasPrefix(oldID, prefix) {
		return oldID[len(prefix):]
	}
	return ""
}

// flattenUnregisteredIssueTypes flattens issue types that are neither
// built-in nor already registered in types.custom, printing a warning
// naming each flattened type. Issues with children (the DependsOnID side
// of a parent-child dep) flatten to epic — matching the default for
// undeclared parent step types — and leaves flatten to task.
// Materializing a formula must not silently grow the type whitelist — a
// typo'd step type would become a permanently registered custom type — so
// unregistered types degrade instead; operators opt in with bd config set
// types.custom before pouring. Without the flatten, issue creation fails
// with "invalid issue type" on the first unregistered bead.
// (GH#3213, GH#5443)
func flattenUnregisteredIssueTypes(ctx context.Context, s configReader, issues []*types.Issue, deps []*types.Dependency) error {
	unknown := collectUnregisteredIssueTypes(issues)
	if len(unknown) == 0 {
		return nil
	}

	if err := removeRegisteredIssueTypes(ctx, s, unknown); err != nil {
		return err
	}
	if len(unknown) == 0 {
		return nil
	}

	warnAboutUnregisteredIssueTypes(unknown)
	flattenIssuesWithUnregisteredTypes(issues, deps, unknown)
	return nil
}

func collectUnregisteredIssueTypes(issues []*types.Issue) map[types.IssueType]bool {
	// Seed with every non-built-in type used by the issues, then remove the
	// registered ones below; what survives is unknown. IsBuiltIn (not
	// IsValid) matches the validator this check exists to satisfy:
	// IsValidWithCustom short-circuits on IsBuiltIn, so types like "event"
	// need no types.custom entry.
	unknown := make(map[types.IssueType]bool)
	for _, issue := range issues {
		t := issue.IssueType
		if t == "" || t.IsBuiltIn() {
			continue
		}
		unknown[t] = true
	}
	return unknown
}

func removeRegisteredIssueTypes(ctx context.Context, s configReader, unknown map[types.IssueType]bool) error {
	// Match insert validation's sources: the types.custom config value
	// (kept in step with the custom_types table by SyncConfigTables)
	// overlaid with config.yaml-declared types. Read through s so a
	// transaction-bound caller sees in-transaction registration.
	existing, err := s.GetConfig(ctx, "types.custom")
	if err != nil {
		// Don't degrade to "nothing registered": a transient read failure
		// would silently flatten types the operator did register.
		return fmt.Errorf("reading types.custom: %w", err)
	}
	for _, t := range issueops.ParseTypesConfigValue(existing) {
		delete(unknown, types.IssueType(t))
	}
	for _, t := range config.GetCustomTypesFromYAML() {
		delete(unknown, types.IssueType(t))
	}
	return nil
}

func warnAboutUnregisteredIssueTypes(unknown map[types.IssueType]bool) {
	names := make([]string, 0, len(unknown))
	for t := range unknown {
		names = append(names, string(t))
	}
	sort.Strings(names)
	WarnError("flattening unregistered issue type(s) to task (epic for steps with children): %s (register with bd config set types.custom to keep them)", strings.Join(names, ", "))
}

func flattenIssuesWithUnregisteredTypes(issues []*types.Issue, deps []*types.Dependency, unknown map[types.IssueType]bool) {
	hasChildren := make(map[string]bool)
	for _, dep := range deps {
		if dep.Type == types.DepParentChild {
			hasChildren[dep.DependsOnID] = true
		}
	}
	for _, issue := range issues {
		if unknown[issue.IssueType] {
			if hasChildren[issue.ID] {
				issue.IssueType = types.TypeEpic
			} else {
				issue.IssueType = types.TypeTask
			}
		}
	}
}

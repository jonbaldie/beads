package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/metrics"
)

func runRulesAudit(cmd *cobra.Command, _ []string) error {
	evt := metrics.NewCommandEvent("rules-audit")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	rulesPath, _ := cmd.Flags().GetString("path")
	threshold, _ := cmd.Flags().GetFloat64("threshold")

	result, err := RunAudit(rulesPath, threshold)
	if err != nil {
		return HandleErrorRespectJSON("rules audit failed: %v", err)
	}
	return renderRulesAudit(rulesPath, threshold, result)
}

func renderRulesAudit(rulesPath string, threshold float64, result *AuditResult) error {
	if isJSONOutput() {
		return outputJSON(result)
	}

	fmt.Printf("Rules Audit — %s\n", rulesPath)
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	fmt.Println("Summary:")
	fmt.Printf("  Total rules:        %d\n", result.TotalRules)
	fmt.Printf("  Token estimate:     ~%d\n", result.TokenEstimate)
	fmt.Printf("  Contradictions:     %d\n", len(result.Contradictions))

	printRulesAuditMergeSummary(result.MergeCandidates)
	fmt.Println()

	printRulesAuditContradictions(result.Contradictions)
	printRulesAuditMergeCandidates(threshold, result.MergeCandidates)
	return nil
}

func printRulesAuditMergeSummary(candidates []MergeCandidate) {
	if len(candidates) == 0 {
		fmt.Println("  Merge candidates:   0")
		return
	}
	mergeRuleCount := 0
	for _, candidate := range candidates {
		mergeRuleCount += len(candidate.Rules)
	}
	fmt.Printf("  Merge candidates:   %d groups (%d rules)\n", len(candidates), mergeRuleCount)
}

func printRulesAuditContradictions(contradictions []ContradictionReport) {
	if len(contradictions) == 0 {
		return
	}
	fmt.Println("Contradictions:")
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "  Rule A\tRule B\tTension\n")
	fmt.Fprintf(tw, "  ------\t------\t-------\n")
	for _, contradiction := range contradictions {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", contradiction.RuleA, contradiction.RuleB, contradiction.Tension)
	}
	_ = tw.Flush()
	fmt.Println()
}

func printRulesAuditMergeCandidates(threshold float64, candidates []MergeCandidate) {
	if len(candidates) == 0 {
		return
	}
	fmt.Printf("Merge Candidates (similarity > %.2f):\n", threshold)
	for i, candidate := range candidates {
		fmt.Printf("  Group %d — %q (score: %.2f)\n", i+1, candidate.GroupLabel, candidate.Score)
		for _, rule := range candidate.Rules {
			fmt.Printf("    → %s\n", rule)
		}
		suggested := strings.ReplaceAll(candidate.GroupLabel, " ", "-") + ".md"
		fmt.Printf("    Suggested: merge into %s\n\n", suggested)
	}
	fmt.Printf("Run `bd rules compact --auto` to apply suggested merges.\n")
}

func runRulesCompact(cmd *cobra.Command, _ []string) error {
	if err := CheckReadonly("rules compact"); err != nil {
		return err
	}

	evt := metrics.NewCommandEvent("rules-compact")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	rulesPath, _ := cmd.Flags().GetString("path")
	groupNames, _ := cmd.Flags().GetStringSlice("group")
	autoMode, _ := cmd.Flags().GetBool("auto")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	if !autoMode && len(groupNames) == 0 {
		return HandleErrorRespectJSON("specify --group <rule1,rule2,...> or --auto")
	}

	if autoMode {
		return runAutoRulesCompact(rulesPath, dryRun)
	}
	return runExplicitRulesCompact(rulesPath, groupNames, dryRun)
}

func runAutoRulesCompact(rulesPath string, dryRun bool) error {
	result, err := RunAudit(rulesPath, 0.6)
	if err != nil {
		return HandleErrorRespectJSON("audit for auto-compact failed: %v", err)
	}

	if len(result.MergeCandidates) == 0 {
		if isJSONOutput() {
			return outputJSON(map[string]string{"status": "no merge candidates found"})
		}
		fmt.Println("No merge candidates found.")
		return nil
	}

	results := compactAutoGroups(rulesPath, result.MergeCandidates, dryRun)
	if isJSONOutput() {
		return outputJSON(results)
	}
	return nil
}

func compactAutoGroups(rulesPath string, candidates []MergeCandidate, dryRun bool) []compactResult {
	var results []compactResult
	for _, candidate := range candidates {
		result, ok := compactAutoGroup(rulesPath, candidate, dryRun)
		if ok {
			results = append(results, result)
		}
	}
	return results
}

func compactAutoGroup(rulesPath string, candidate MergeCandidate, dryRun bool) (compactResult, bool) {
	groupRules := loadAutoCompactRules(rulesPath, candidate.Rules)
	merged, err := CompactRules(groupRules, candidate.GroupLabel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: compact group %q failed: %v\n", candidate.GroupLabel, err)
		return compactResult{}, false
	}

	applied, ok := applyAutoCompactGroup(rulesPath, candidate, merged, dryRun)
	if !ok {
		return compactResult{}, false
	}

	result := compactResult{
		Group:   candidate.GroupLabel,
		Output:  merged,
		Rules:   len(groupRules),
		Applied: applied,
	}
	if !isJSONOutput() {
		printAutoCompactResult(candidate.GroupLabel, merged, dryRun)
	}
	return result, true
}

func loadAutoCompactRules(rulesPath string, ruleNames []string) []RuleFile {
	var groupRules []RuleFile
	for _, ruleName := range ruleNames {
		path := filepath.Join(rulesPath, ruleName)
		rf, err := ParseRuleFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: skipping %s: %v\n", ruleName, err)
			continue
		}
		groupRules = append(groupRules, rf)
	}
	return groupRules
}

func applyAutoCompactGroup(rulesPath string, candidate MergeCandidate, merged string, dryRun bool) (bool, bool) {
	if dryRun {
		return false, true
	}

	outName := strings.ReplaceAll(candidate.GroupLabel, " ", "-") + ".md"
	outPath := filepath.Join(rulesPath, outName)
	if err := os.WriteFile(outPath, []byte(merged), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", outPath, err)
		return false, false
	}
	for _, ruleName := range candidate.Rules {
		srcPath := filepath.Join(rulesPath, ruleName)
		_ = os.Remove(srcPath)
	}
	return true, true
}

func printAutoCompactResult(groupLabel, merged string, dryRun bool) {
	if dryRun {
		fmt.Printf("Preview merge → %s.md:\n", groupLabel)
	} else {
		fmt.Printf("Merged → %s.md:\n", groupLabel)
	}
	fmt.Println(strings.Repeat("─", 40))
	fmt.Print(merged)
	fmt.Println(strings.Repeat("─", 40))
	fmt.Println()
}

func runExplicitRulesCompact(rulesPath string, groupNames []string, dryRun bool) error {
	groupRules, err := loadExplicitCompactRules(rulesPath, groupNames)
	if err != nil {
		return err
	}

	if len(groupRules) < 2 {
		return HandleErrorRespectJSON("need at least 2 rules to merge")
	}

	label := findGroupLabel(groupRules, makeRange(len(groupRules)))
	merged, err := CompactRules(groupRules, label)
	if err != nil {
		return HandleErrorRespectJSON("compact failed: %v", err)
	}

	if isJSONOutput() {
		return outputJSON(map[string]any{
			"group":   label,
			"output":  merged,
			"rules":   len(groupRules),
			"dry_run": dryRun,
		})
	}

	outName := strings.ReplaceAll(label, " ", "-") + ".md"
	if dryRun {
		fmt.Printf("Preview merge → %s:\n", outName)
	} else {
		fmt.Printf("Merged → %s:\n", outName)
	}
	fmt.Println(strings.Repeat("─", 40))
	fmt.Print(merged)
	fmt.Println(strings.Repeat("─", 40))

	if !dryRun {
		outPath := filepath.Join(rulesPath, outName)
		if err := os.WriteFile(outPath, []byte(merged), 0o600); err != nil {
			return HandleErrorRespectJSON("write merged file: %v", err)
		}
		for _, rf := range groupRules {
			_ = os.Remove(rf.Path)
		}
		fmt.Printf("\nCreated %s, deleted %d source files.\n", outName, len(groupRules))
	}
	return nil
}

func loadExplicitCompactRules(rulesPath string, groupNames []string) ([]RuleFile, error) {
	var groupRules []RuleFile
	for _, name := range groupNames {
		if !strings.HasSuffix(name, ".md") {
			name = name + ".md"
		}
		path := filepath.Join(rulesPath, name)
		rf, err := ParseRuleFile(path)
		if err != nil {
			return nil, HandleErrorRespectJSON("cannot read rule %s: %v", name, err)
		}
		groupRules = append(groupRules, rf)
	}
	return groupRules, nil
}

// titleCase capitalizes the first letter of each word.
func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// makeRange returns a slice [0, 1, ..., n-1].
func makeRange(n int) []int {
	r := make([]int, n)
	for i := range r {
		r[i] = i
	}
	return r
}

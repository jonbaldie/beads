package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// DetectContradictions finds opposing directives across rule pairs.
// Two rules contradict when they share scope (jaccard >= scopeThreshold)
// and have opposing directives (same verb in Do vs Don't, or antonym pairs).
func DetectContradictions(rules []RuleFile, scopeThreshold float64) []ContradictionReport {
	var reports []ContradictionReport

	ruleCount := len(rules)
	for i := 0; i < ruleCount; i++ {
		for j := i + 1; j < ruleCount; j++ {
			a, b := rules[i], rules[j]
			score := JaccardSimilarity(a.Keywords, b.Keywords)
			if score < scopeThreshold {
				continue
			}

			// Check for direct contradictions: verb in A's Do appears in B's Don't
			if c := findDirectContradiction(a, b, score); c != nil {
				reports = append(reports, *c)
				continue
			}
			// Check reverse direction
			if c := findDirectContradiction(b, a, score); c != nil {
				// Swap labels since we checked b->a
				c.RuleA, c.RuleB = a.Name+".md", b.Name+".md"
				reports = append(reports, *c)
				continue
			}

			// Check for antonym pair contradictions in Do vs Do
			if c := findAntonymContradiction(a, b, score); c != nil {
				reports = append(reports, *c)
			}
		}
	}

	return reports
}

// findDirectContradiction checks if a verb from A's Do appears in B's Don't.
func findDirectContradiction(a, b RuleFile, scopeScore float64) *ContradictionReport {
	aDoWords := extractActionWords(a.DoLines)
	bDontWords := extractActionWords(b.DontLines)

	for word, doLine := range aDoWords {
		if dontLine, ok := bDontWords[word]; ok {
			tension := truncateTension(
				fmt.Sprintf("%q vs %q", summarizeLine(doLine), summarizeLine(dontLine)),
			)
			return &ContradictionReport{
				RuleA:      a.Name + ".md",
				RuleB:      b.Name + ".md",
				Tension:    tension,
				DoLineA:    doLine,
				DontLineB:  dontLine,
				ScopeScore: scopeScore,
			}
		}
	}
	return nil
}

// findAntonymContradiction checks if A's Do words have antonyms in B's Do words.
func findAntonymContradiction(a, b RuleFile, scopeScore float64) *ContradictionReport {
	aDoWords := extractActionWords(a.DoLines)
	bDoWords := extractActionWords(b.DoLines)

	for wordA, lineA := range aDoWords {
		antonyms, ok := antonymPairs[wordA]
		if !ok {
			continue
		}
		for _, ant := range antonyms {
			if lineB, ok := bDoWords[ant]; ok {
				tension := truncateTension(
					fmt.Sprintf("%q vs %q", summarizeLine(lineA), summarizeLine(lineB)),
				)
				return &ContradictionReport{
					RuleA:      a.Name + ".md",
					RuleB:      b.Name + ".md",
					Tension:    tension,
					DoLineA:    lineA,
					DontLineB:  lineB,
					ScopeScore: scopeScore,
				}
			}
		}
	}
	return nil
}

// extractActionWords returns a map of lowercase action words to their source line.
func extractActionWords(lines []string) map[string]string {
	result := make(map[string]string)
	for _, line := range lines {
		words := tokenizeWords(line)
		for _, w := range words {
			w = strings.ToLower(w)
			if len(w) >= 2 && !stopWords[w] {
				if _, exists := result[w]; !exists {
					result[w] = line
				}
			}
		}
	}
	return result
}

// summarizeLine truncates a line for display in tension descriptions.
func summarizeLine(line string) string {
	if len(line) > 40 {
		return line[:37] + "..."
	}
	return line
}

// truncateTension ensures tension string fits in a table cell.
func truncateTension(s string) string {
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}

// FindMergeCandidates groups rules by keyword overlap using single-linkage clustering.
func FindMergeCandidates(rules []RuleFile, threshold float64) []MergeCandidate {
	n := len(rules)
	if n < 2 {
		return nil
	}
	pairs := buildMergePairs(rules, threshold)
	if len(pairs) == 0 {
		return nil
	}
	uf := newMergeUnionFind(n)
	for _, p := range pairs {
		uf.union(p.i, p.j)
	}
	groups := collectMergeGroups(uf, n)
	return buildMergeCandidates(rules, groups)
}

type mergePair struct {
	i, j int
}

func buildMergePairs(rules []RuleFile, threshold float64) []mergePair {
	ruleCount := len(rules)
	var pairs []mergePair
	for i := 0; i < ruleCount; i++ {
		for j := i + 1; j < ruleCount; j++ {
			score := JaccardSimilarity(rules[i].Keywords, rules[j].Keywords)
			if score >= threshold {
				pairs = append(pairs, mergePair{i, j})
			}
		}
	}
	return pairs
}

type mergeUnionFind struct {
	parent []int
}

func newMergeUnionFind(size int) *mergeUnionFind {
	parent := make([]int, size)
	for i := range parent {
		parent[i] = i
	}
	return &mergeUnionFind{parent: parent}
}

func (uf *mergeUnionFind) find(index int) int {
	if uf.parent[index] != index {
		uf.parent[index] = uf.find(uf.parent[index])
	}
	return uf.parent[index]
}

func (uf *mergeUnionFind) union(a, b int) {
	rootA, rootB := uf.find(a), uf.find(b)
	if rootA != rootB {
		uf.parent[rootA] = rootB
	}
}

func collectMergeGroups(uf *mergeUnionFind, ruleCount int) map[int][]int {
	groups := make(map[int][]int)
	for i := 0; i < ruleCount; i++ {
		root := uf.find(i)
		groups[root] = append(groups[root], i)
	}
	return groups
}

func buildMergeCandidates(rules []RuleFile, groups map[int][]int) []MergeCandidate {
	var candidates []MergeCandidate
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		candidates = append(candidates, buildMergeCandidate(rules, members))
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	return candidates
}

func buildMergeCandidate(rules []RuleFile, members []int) MergeCandidate {
	label := findGroupLabel(rules, members)
	return MergeCandidate{
		GroupLabel: label,
		Rules:      mergeRuleNames(rules, members),
		Score:      roundTo2(averageMergeScore(rules, members)),
	}
}

func averageMergeScore(rules []RuleFile, members []int) float64 {
	memberCount := len(members)
	var totalScore float64
	pairCount := 0
	for mi := 0; mi < memberCount; mi++ {
		for mj := mi + 1; mj < memberCount; mj++ {
			totalScore += JaccardSimilarity(rules[members[mi]].Keywords, rules[members[mj]].Keywords)
			pairCount++
		}
	}
	if pairCount == 0 {
		return 0
	}
	return totalScore / float64(pairCount)
}

func mergeRuleNames(rules []RuleFile, members []int) []string {
	var names []string
	for _, index := range members {
		names = append(names, rules[index].Name+".md")
	}
	sort.Strings(names)
	return names
}

// findGroupLabel finds the most common keyword across a group of rules.
func findGroupLabel(rules []RuleFile, indices []int) string {
	freq := make(map[string]int)
	for _, idx := range indices {
		for _, kw := range rules[idx].Keywords {
			freq[kw]++
		}
	}

	bestWord := "rules"
	bestCount := 0
	for w, c := range freq {
		if c > bestCount || (c == bestCount && w < bestWord) {
			bestWord = w
			bestCount = c
		}
	}
	return bestWord
}

// roundTo2 rounds a float to 2 decimal places.
func roundTo2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}

// CompactRules merges a group of rules into a single composite markdown file.
func CompactRules(rules []RuleFile, groupLabel string) (string, error) {
	if len(rules) == 0 {
		return "", fmt.Errorf("no rules to compact")
	}
	doLines, dontLines := collectCompactLines(rules)
	return formatCompactRules(groupLabel, doLines, dontLines, rules), nil
}

func collectCompactLines(rules []RuleFile) (doLines, dontLines []string) {
	seenDo := make(map[string]bool)
	seenDont := make(map[string]bool)
	for _, r := range rules {
		for _, line := range r.DoLines {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !seenDo[trimmed] {
				seenDo[trimmed] = true
				doLines = append(doLines, trimmed)
			}
		}
		for _, line := range r.DontLines {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !seenDont[trimmed] {
				seenDont[trimmed] = true
				dontLines = append(dontLines, trimmed)
			}
		}
	}
	return doLines, dontLines
}

func formatCompactRules(groupLabel string, doLines, dontLines []string, rules []RuleFile) string {
	var sb strings.Builder
	sb.WriteString("# ")
	sb.WriteString(titleCase(groupLabel))
	sb.WriteString("\n")

	if len(doLines) > 0 {
		sb.WriteString("**Do:** ")
		sb.WriteString(strings.Join(doLines, ". "))
		sb.WriteString("\n")
	}
	if len(dontLines) > 0 {
		sb.WriteString("**Don't:** ")
		sb.WriteString(strings.Join(dontLines, ". "))
		sb.WriteString("\n")
	}

	// Source attribution
	var sourceNames []string
	for _, r := range rules {
		sourceNames = append(sourceNames, r.Name+".md")
	}
	sb.WriteString("\nSource rules: ")
	sb.WriteString(strings.Join(sourceNames, ", "))
	sb.WriteString("\n")

	return sb.String()
}

// RunAudit is the top-level orchestrator for `bd rules audit`.
func RunAudit(rulesDir string, threshold float64) (*AuditResult, error) {
	rules, totalTokens, err := readRuleFiles(rulesDir)
	if err != nil {
		return nil, err
	}

	result := &AuditResult{
		TotalRules:    len(rules),
		TokenEstimate: totalTokens,
	}

	if len(rules) < 2 {
		return completeAuditResult(result, rules), nil
	}

	result.Contradictions = DetectContradictions(rules, 0.3)
	result.MergeCandidates = FindMergeCandidates(rules, threshold)
	return completeAuditResult(result, rules), nil
}

func readRuleFiles(rulesDir string) ([]RuleFile, int, error) {
	entries, err := os.ReadDir(rulesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("read rules directory: %w", err)
	}
	var rules []RuleFile
	totalTokens := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		rf, err := ParseRuleFile(filepath.Join(rulesDir, entry.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: skipping %s: %v\n", entry.Name(), err)
			continue
		}
		rules = append(rules, rf)
		totalTokens += rf.Tokens
	}
	return rules, totalTokens, nil
}

func completeAuditResult(result *AuditResult, rules []RuleFile) *AuditResult {
	result.Rules = rules
	if result.Contradictions == nil {
		result.Contradictions = []ContradictionReport{}
	}
	if result.MergeCandidates == nil {
		result.MergeCandidates = []MergeCandidate{}
	}
	return result
}

// --- Cobra Commands ---

var rulesCmd = &cobra.Command{
	Use:     "rules",
	Short:   "Audit and compact Claude rules",
	GroupID: "maint",
}

var rulesAuditCmd = &cobra.Command{
	Use:           "audit",
	Short:         "Scan rules for contradictions and merge opportunities",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runRulesAudit,
}

var rulesCompactCmd = &cobra.Command{
	Use:           "compact",
	Short:         "Merge related rules into composites",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runRulesCompact,
}

func init() {
	// Audit command flags
	rulesAuditCmd.Flags().String("path", ".claude/rules/", "Path to rules directory")
	rulesAuditCmd.Flags().Float64("threshold", 0.6, "Jaccard similarity threshold")

	// Compact command flags
	rulesCompactCmd.Flags().String("path", ".claude/rules/", "Path to rules directory")
	rulesCompactCmd.Flags().StringSlice("group", nil, "Rule names to merge")
	rulesCompactCmd.Flags().Bool("auto", false, "Apply audit suggestions")
	rulesCompactCmd.Flags().Bool("dry-run", false, "Preview without applying")

	// Register subcommands
	rulesCmd.AddCommand(rulesAuditCmd)
	rulesCmd.AddCommand(rulesCompactCmd)
	rootCmd.AddCommand(rulesCmd)
}

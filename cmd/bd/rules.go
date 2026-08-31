package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// --- Types ---

// RuleFile represents a parsed .claude/rules/*.md file.
type RuleFile struct {
	Path      string   `json:"path"`
	Name      string   `json:"name"`
	Title     string   `json:"title"`
	DoLines   []string `json:"do_lines"`
	DontLines []string `json:"dont_lines"`
	Body      string   `json:"body,omitempty"`
	Keywords  []string `json:"keywords"`
	Tokens    int      `json:"tokens"`
}

// ContradictionReport describes a tension between two rules.
type ContradictionReport struct {
	RuleA      string  `json:"rule_a"`
	RuleB      string  `json:"rule_b"`
	Tension    string  `json:"tension"`
	DoLineA    string  `json:"do_line_a"`
	DontLineB  string  `json:"dont_line_b"`
	ScopeScore float64 `json:"scope_score"`
}

// MergeCandidate represents a group of rules that could be combined.
type MergeCandidate struct {
	GroupLabel string   `json:"group_label"`
	Rules      []string `json:"rules"`
	Score      float64  `json:"score"`
}

type compactResult struct {
	Group   string `json:"group"`
	Output  string `json:"output"`
	Rules   int    `json:"rules_merged"`
	Applied bool   `json:"applied"`
}

// AuditResult is the full output of `bd rules audit`.
type AuditResult struct {
	TotalRules      int                   `json:"total_rules"`
	TokenEstimate   int                   `json:"token_estimate"`
	Contradictions  []ContradictionReport `json:"contradictions"`
	MergeCandidates []MergeCandidate      `json:"merge_candidates"`
	Rules           []RuleFile            `json:"rules,omitempty"`
}

// --- Stop Words ---

var stopWords = map[string]bool{
	"the": true, "a": true, "is": true, "to": true, "for": true,
	"and": true, "or": true, "in": true, "of": true, "it": true,
	"that": true, "this": true, "with": true, "be": true, "not": true,
	"do": true, "don't": true, "use": true, "when": true, "before": true,
	"after": true, "should": true, "must": true, "always": true, "never": true,
	"an": true, "are": true, "as": true, "at": true, "by": true,
	"from": true, "has": true, "have": true, "if": true, "on": true,
	"was": true, "were": true, "will": true, "you": true, "your": true,
}

// --- Antonym Pairs ---

// antonymPairs maps words to their antonyms for contradiction detection.
var antonymPairs = map[string][]string{
	"block":    {"proceed", "parallel"},
	"proceed":  {"block"},
	"parallel": {"block"},
	"verbose":  {"minimize", "concise"},
	"minimize": {"verbose"},
	"concise":  {"verbose"},
	"spawn":    {"reuse"},
	"reuse":    {"spawn"},
	"wait":     {"skip"},
	"skip":     {"wait"},
	"log":      {"suppress"},
	"suppress": {"log"},
}

// --- Regex Patterns ---

var (
	headingRe = regexp.MustCompile(`(?m)^#\s+(.+)`)
	doRe      = regexp.MustCompile(`(?i)^\*\*Do:?\*\*:?\s*(.*)`)
	dontRe    = regexp.MustCompile(`(?i)^\*\*Don'?t:?\*\*:?\s*(.*)`)
)

// --- Core Functions ---

// ParseRuleFile reads a .md file and extracts structured rule data.
func ParseRuleFile(path string) (RuleFile, error) {
	// #nosec G304 -- path comes from controlled filepath.Join of user-specified rules directory
	data, err := os.ReadFile(path)
	if err != nil {
		return RuleFile{}, fmt.Errorf("read rule file %s: %w", path, err)
	}

	content := string(data)
	name := strings.TrimSuffix(filepath.Base(path), ".md")

	rf := RuleFile{
		Path: path,
		Name: name,
		Body: content,
		// Rough token estimate: 1 token ~ 4 chars
		Tokens: len(content) / 4,
	}

	// Extract title from first heading
	if m := headingRe.FindStringSubmatch(content); len(m) > 1 {
		rf.Title = strings.TrimSpace(m[1])
	} else {
		rf.Title = name
	}

	// Extract Do and Don't blocks
	rf.DoLines, rf.DontLines = extractAllDirectives(content)

	// Extract keywords from Do/Don't lines first, fallback to body
	allDirectives := append(rf.DoLines, rf.DontLines...)
	if len(allDirectives) > 0 {
		rf.Keywords = ExtractKeywords(allDirectives)
	} else {
		rf.Keywords = ExtractKeywords([]string{content})
	}

	return rf, nil
}

// extractAllDirectives parses Do and Don't blocks from rule content.
// Don't patterns are checked first to avoid false matches (since "Don't" contains "Do").
func extractAllDirectives(content string) (doLines, dontLines []string) {
	lines := strings.Split(content, "\n")

	// blockType: 0=none, 1=do, 2=dont
	blockType := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if next, text, ok := directiveHeader(line); ok {
			blockType = next
			appendDirectiveText(blockType, text, &doLines, &dontLines)
			continue
		}
		blockType = appendDirectiveContinuation(blockType, trimmed, &doLines, &dontLines)
	}
	return doLines, dontLines
}

func directiveHeader(line string) (blockType int, text string, ok bool) {
	// Check Don't first (it contains "Do", so must be checked before Do).
	if m := dontRe.FindStringSubmatch(line); len(m) > 1 {
		return 2, strings.TrimSpace(m[1]), true
	}
	if m := doRe.FindStringSubmatch(line); len(m) > 1 {
		return 1, strings.TrimSpace(m[1]), true
	}
	return 0, "", false
}

func appendDirectiveText(blockType int, text string, doLines, dontLines *[]string) {
	if text == "" {
		return
	}
	if blockType == 1 {
		*doLines = append(*doLines, text)
		return
	}
	*dontLines = append(*dontLines, text)
}

func appendDirectiveContinuation(blockType int, trimmed string, doLines, dontLines *[]string) int {
	if blockType == 0 {
		return 0
	}
	if isDirectiveBullet(trimmed) {
		appendDirectiveText(blockType, strings.TrimLeft(trimmed, "-* "), doLines, dontLines)
		return blockType
	}
	if isDirectiveTerminator(trimmed) {
		return 0
	}
	appendDirectiveText(blockType, trimmed, doLines, dontLines)
	return blockType
}

func isDirectiveBullet(trimmed string) bool {
	return strings.HasPrefix(trimmed, "-") || (strings.HasPrefix(trimmed, "*") && !strings.HasPrefix(trimmed, "**"))
}

func isDirectiveTerminator(trimmed string) bool {
	return trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "**")
}

// ExtractKeywords tokenizes lines, removes stop words, lowercases, and deduplicates.
func ExtractKeywords(lines []string) []string {
	seen := make(map[string]bool)
	var keywords []string

	for _, line := range lines {
		words := tokenizeWords(line)
		for _, w := range words {
			w = strings.ToLower(w)
			if len(w) < 2 {
				continue
			}
			if stopWords[w] {
				continue
			}
			if !seen[w] {
				seen[w] = true
				keywords = append(keywords, w)
			}
		}
	}

	sort.Strings(keywords)
	return keywords
}

// tokenize splits text into words, stripping punctuation.
func tokenizeWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
}

// JaccardSimilarity computes keyword overlap between two keyword sets.
func JaccardSimilarity(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0.0
	}

	setA := keywordSet(a)
	setB := keywordSet(b)
	intersection := keywordIntersection(setA, setB)
	union := keywordUnionSize(setA, setB)

	if union == 0 {
		return 0.0
	}
	return float64(intersection) / float64(union)
}

func keywordSet(words []string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, word := range words {
		set[word] = true
	}
	return set
}

func keywordIntersection(a, b map[string]bool) int {
	intersection := 0
	for word := range a {
		if b[word] {
			intersection++
		}
	}
	return intersection
}

func keywordUnionSize(a, b map[string]bool) int {
	union := len(a)
	for word := range b {
		if !a[word] {
			union++
		}
	}
	return union
}

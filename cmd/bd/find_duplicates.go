package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

var findDuplicatesCmd = &cobra.Command{
	Use:     "find-duplicates",
	Aliases: []string{"find-dups"},
	GroupID: "views",
	Short:   "Find semantically similar issues using text analysis or AI",
	Long: `Find issues that are semantically similar but not exact duplicates.

Unlike 'bd duplicates' which finds exact content matches, find-duplicates
uses text similarity or AI to find issues that discuss the same topic
with different wording.

Approaches:
  mechanical  Token-based text similarity (default, no API key needed)
  ai          LLM-based semantic comparison (requires ANTHROPIC_API_KEY, MINIMAX_API_KEY, or ai.api_key)

The mechanical approach tokenizes titles and descriptions, then computes
Jaccard similarity between all issue pairs. It's fast and free but may
miss semantically similar issues with very different wording.

The AI approach sends candidate pairs to an Anthropic-compatible model for semantic comparison.
It first uses mechanical pre-filtering to reduce the number of API calls,
then asks the LLM to judge whether the remaining pairs are true duplicates.

Examples:
  bd find-duplicates                       # Mechanical similarity (default)
  bd find-duplicates --threshold 0.4       # Lower threshold = more results
  bd find-duplicates --method ai           # Use AI for semantic comparison
  bd find-duplicates --status open         # Only check open issues
  bd find-duplicates --limit 20            # Show top 20 pairs
  bd find-duplicates --json                # JSON output`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runFindDuplicates,
}

func init() {
	findDuplicatesCmd.Flags().String("method", "mechanical", "Detection method: mechanical, ai")
	findDuplicatesCmd.Flags().Float64("threshold", 0.5, "Similarity threshold (0.0-1.0, lower = more results)")
	findDuplicatesCmd.Flags().StringP("status", "s", "", "Filter by status (default: non-closed)")
	findDuplicatesCmd.Flags().IntP("limit", "n", 50, "Maximum number of pairs to show")
	findDuplicatesCmd.Flags().String("model", "", "AI model to use (only with --method ai; default from config ai.model)")
	// Defensive row cap (be-x42v): exits 2 on overage, default disabled.
	addMaxRowsFlag(findDuplicatesCmd)
	rootCmd.AddCommand(findDuplicatesCmd)
}

// duplicatePair represents a pair of potentially duplicate issues.
type duplicatePair struct {
	IssueA     *types.Issue `json:"issue_a"`
	IssueB     *types.Issue `json:"issue_b"`
	Similarity float64      `json:"similarity"`
	Method     string       `json:"method"`
	Reason     string       `json:"reason,omitempty"`
}

type findDuplicatesOptions struct {
	method    string
	threshold float64
	status    string
	limit     int
	model     string
}

func runFindDuplicates(cmd *cobra.Command, _ []string) error {
	evt := metrics.NewCommandEvent("find-duplicates")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	options, err := findDuplicatesOptionsFromCommand(cmd)
	if err != nil {
		return err
	}
	return executeFindDuplicates(cmd, options)
}

func findDuplicatesOptionsFromCommand(cmd *cobra.Command) (findDuplicatesOptions, error) {
	method, _ := cmd.Flags().GetString("method")
	threshold, _ := cmd.Flags().GetFloat64("threshold")
	status, _ := cmd.Flags().GetString("status")
	limit, _ := cmd.Flags().GetInt("limit")
	model, _ := cmd.Flags().GetString("model")
	if model == "" {
		_, keySource := config.ResolveAIAPIKey("")
		model = config.DefaultAIModelFor(keySource)
	}
	options := findDuplicatesOptions{
		method:    method,
		threshold: threshold,
		status:    status,
		limit:     limit,
		model:     model,
	}
	if err := validateFindDuplicatesOptions(options); err != nil {
		return findDuplicatesOptions{}, err
	}
	return options, nil
}

func validateFindDuplicatesOptions(options findDuplicatesOptions) error {
	if options.method != "mechanical" && options.method != "ai" {
		return HandleErrorRespectJSON("invalid method %q (use: mechanical, ai)", options.method)
	}
	if options.method == "ai" {
		if apiKey, _ := config.ResolveAIAPIKey(""); apiKey == "" {
			return HandleErrorRespectJSON("--method ai requires ANTHROPIC_API_KEY, MINIMAX_API_KEY, or ai.api_key in config")
		}
	}
	return nil
}

func executeFindDuplicates(cmd *cobra.Command, options findDuplicatesOptions) error {
	maxRows, maxRowsSource, err := resolveMaxRows(cmd)
	if err != nil {
		return err
	}
	filter := findDuplicatesFilter(options.status, maxRows, maxRowsSource)

	if usesProxiedServer() {
		// maxRows was already resolved above (to build filter); reject using
		// that value directly instead of re-resolving via
		// rejectMaxRowsUnderProxiedServer, which would call resolveMaxRows a
		// second time and double any malformed-env warning it emits.
		if err := rejectResolvedMaxRowsUnderProxiedServer(maxRows); err != nil {
			return err
		}
		return runFindDuplicatesProxiedServer(getRootContext(), filter, options.status, options.method, options.threshold, options.limit, options.model)
	}

	issues, err := getStore().SearchIssues(getRootContext(), "", filter)
	if err != nil {
		if capErr := handleMaxRowsError(err); capErr != nil {
			return capErr
		}
		return HandleErrorRespectJSON("fetching issues: %v", err)
	}
	issues = filterClosedIfNoStatus(issues, options.status)

	return reportFindDuplicates(getRootContext(), issues, options.method, options.threshold, options.limit, options.model)
}

func findDuplicatesFilter(status string, maxRows int, maxRowsSource string) types.IssueFilter {
	filter := types.IssueFilter{
		IssueFilterPage: types.IssueFilterPage{
			MaxRows:       maxRows,
			MaxRowsSource: maxRowsSource,
		},
	}
	if status != "" && status != "all" {
		s := types.Status(status)
		filter.Status = &s
	}
	return filter
}

func filterClosedIfNoStatus(issues []*types.Issue, status string) []*types.Issue {
	if status != "" {
		return issues
	}
	var filtered []*types.Issue
	for _, issue := range issues {
		if issue.Status != types.StatusClosed {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}

func reportFindDuplicates(ctx context.Context, issues []*types.Issue, method string, threshold float64, limit int, model string) error {
	if len(issues) < 2 {
		return reportTooFewDuplicateIssues()
	}

	pairs := findDuplicatePairs(ctx, issues, method, threshold, model)
	pairs = sortAndLimitDuplicatePairs(pairs, limit)

	if isJSONOutput() {
		return outputDuplicatePairsJSON(pairs, method, threshold)
	}
	return printDuplicatePairs(pairs, threshold)
}

func reportTooFewDuplicateIssues() error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"pairs": []interface{}{},
			"count": 0,
		})
	}
	fmt.Println("Not enough issues to compare (need at least 2)")
	return nil
}

func findDuplicatePairs(ctx context.Context, issues []*types.Issue, method string, threshold float64, model string) []duplicatePair {
	switch method {
	case "mechanical":
		return findMechanicalDuplicates(issues, threshold)
	case "ai":
		return findAIDuplicates(ctx, issues, threshold, model)
	default:
		return nil
	}
}

func sortAndLimitDuplicatePairs(pairs []duplicatePair, limit int) []duplicatePair {
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].Similarity > pairs[j].Similarity
	})
	if limit > 0 && len(pairs) > limit {
		return pairs[:limit]
	}
	return pairs
}

func outputDuplicatePairsJSON(pairs []duplicatePair, method string, threshold float64) error {
	type pairJSON struct {
		IssueAID    string  `json:"issue_a_id"`
		IssueBID    string  `json:"issue_b_id"`
		IssueATitle string  `json:"issue_a_title"`
		IssueBTitle string  `json:"issue_b_title"`
		Similarity  float64 `json:"similarity"`
		Method      string  `json:"method"`
		Reason      string  `json:"reason,omitempty"`
	}
	jsonPairs := make([]pairJSON, len(pairs))
	for i, p := range pairs {
		jsonPairs[i] = pairJSON{
			IssueAID:    p.IssueA.ID,
			IssueBID:    p.IssueB.ID,
			IssueATitle: p.IssueA.Title,
			IssueBTitle: p.IssueB.Title,
			Similarity:  p.Similarity,
			Method:      p.Method,
			Reason:      p.Reason,
		}
	}
	return outputJSON(map[string]interface{}{
		"pairs":     jsonPairs,
		"count":     len(jsonPairs),
		"method":    method,
		"threshold": threshold,
	})
}

func printDuplicatePairs(pairs []duplicatePair, threshold float64) error {
	if len(pairs) == 0 {
		fmt.Printf("No similar issues found (threshold: %.0f%%)\n", threshold*100)
		return nil
	}

	fmt.Printf("%s Found %d potential duplicate pair(s) (threshold: %.0f%%):\n\n",
		ui.RenderWarn("🔍"), len(pairs), threshold*100)

	for i, p := range pairs {
		pct := p.Similarity * 100
		fmt.Printf("%s Pair %d (%.0f%% similar):\n", ui.RenderAccent("━━"), i+1, pct)
		fmt.Printf("  %s %s\n", ui.RenderPass(p.IssueA.ID), p.IssueA.Title)
		fmt.Printf("  %s %s\n", ui.RenderPass(p.IssueB.ID), p.IssueB.Title)
		if p.Reason != "" {
			fmt.Printf("  %s %s\n", ui.RenderAccent("Reason:"), p.Reason)
		}
		fmt.Printf("  %s bd show %s %s\n\n", ui.RenderAccent("Compare:"), p.IssueA.ID, p.IssueB.ID)
	}
	return nil
}

// tokenize splits text into lowercase word tokens, removing punctuation.
func tokenize(text string) map[string]int {
	tokens := make(map[string]int)
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-'
	})
	for _, w := range words {
		if len(w) > 1 { // Skip single chars
			tokens[w]++
		}
	}
	return tokens
}

// issueText returns the combined text content of an issue for comparison.
func issueText(issue *types.Issue) string {
	parts := []string{issue.Title}
	if issue.Description != "" {
		parts = append(parts, issue.Description)
	}
	return strings.Join(parts, " ")
}

// jaccardSimilarity computes the Jaccard similarity between two token sets.
func jaccardSimilarity(a, b map[string]int) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}

	intersection, union := jaccardSharedCounts(a, b)
	union += jaccardUniqueCount(b, a)

	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func jaccardSharedCounts(a, b map[string]int) (int, int) {
	intersection := 0
	union := 0
	for token, countA := range a {
		countB, ok := b[token]
		if !ok {
			union += countA
			continue
		}
		intersection += minInt(countA, countB)
		union += maxInt(countA, countB)
	}
	return intersection, union
}

func jaccardUniqueCount(source, compared map[string]int) int {
	unique := 0
	for token, count := range source {
		if _, ok := compared[token]; !ok {
			unique += count
		}
	}
	return unique
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// cosineSimilarity computes the cosine similarity between two token vectors.
func cosineSimilarity(a, b map[string]int) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}

	dotProduct := 0.0
	magA := 0.0
	magB := 0.0

	for token, countA := range a {
		fa := float64(countA)
		magA += fa * fa
		if countB, ok := b[token]; ok {
			dotProduct += fa * float64(countB)
		}
	}
	for _, countB := range b {
		fb := float64(countB)
		magB += fb * fb
	}

	if magA == 0 || magB == 0 {
		return 0
	}
	return dotProduct / (math.Sqrt(magA) * math.Sqrt(magB))
}

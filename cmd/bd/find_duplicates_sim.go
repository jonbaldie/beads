package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/telemetry"
	"github.com/jonbaldie/beads/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

type tokenizedIssue struct {
	issue  *types.Issue
	tokens map[string]int
}

// findMechanicalDuplicates finds similar issues using token-based text similarity.
func findMechanicalDuplicates(issues []*types.Issue, threshold float64) []duplicatePair {
	return matchingMechanicalPairs(tokenizeDuplicateIssues(issues), threshold)
}

func tokenizeDuplicateIssues(issues []*types.Issue) []tokenizedIssue {
	items := make([]tokenizedIssue, len(issues))
	for i, issue := range issues {
		items[i] = tokenizedIssue{
			issue:  issue,
			tokens: tokenize(issueText(issue)),
		}
	}
	return items
}

func matchingMechanicalPairs(items []tokenizedIssue, threshold float64) []duplicatePair {
	var pairs []duplicatePair

	// Compare all pairs
	itemCount := len(items)
	for i := 0; i < itemCount; i++ {
		for j := i + 1; j < itemCount; j++ {
			// Use average of Jaccard and cosine for better accuracy
			jaccard := jaccardSimilarity(items[i].tokens, items[j].tokens)
			cosine := cosineSimilarity(items[i].tokens, items[j].tokens)
			similarity := (jaccard + cosine) / 2

			if similarity >= threshold {
				pairs = append(pairs, duplicatePair{
					IssueA:     items[i].issue,
					IssueB:     items[j].issue,
					Similarity: similarity,
					Method:     "mechanical",
				})
			}
		}
	}

	return pairs
}

// findAIDuplicates uses LLM-based semantic comparison to find duplicates.
// It first pre-filters with mechanical similarity to reduce API calls.
func findAIDuplicates(ctx context.Context, issues []*types.Issue, threshold float64, model string) []duplicatePair {
	candidates := prepareAIDuplicateCandidates(issues, threshold)
	if len(candidates) == 0 {
		return nil
	}

	candidates = limitAIDuplicateCandidates(candidates)

	fmt.Fprintf(os.Stderr, "Analyzing %d candidate pairs with AI...\n", len(candidates))

	client := newDuplicateAIClient()
	return analyzeDuplicateBatches(ctx, client, model, candidates, threshold)
}

func prepareAIDuplicateCandidates(issues []*types.Issue, threshold float64) []duplicatePair {
	// Cast a wider net for pre-filtering.
	preFilterThreshold := threshold * 0.5
	if preFilterThreshold < 0.15 {
		preFilterThreshold = 0.15
	}
	return findMechanicalDuplicates(issues, preFilterThreshold)
}

func limitAIDuplicateCandidates(candidates []duplicatePair) []duplicatePair {
	const maxCandidates = 100
	if len(candidates) <= maxCandidates {
		return candidates
	}
	// Sort by mechanical similarity and take the top candidates.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Similarity > candidates[j].Similarity
	})
	return candidates[:maxCandidates]
}

func newDuplicateAIClient() anthropic.Client {
	apiKey, keySource := config.ResolveAIAPIKey("")
	clientOptions := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL := config.DefaultAIBaseURL(keySource); baseURL != "" {
		clientOptions = append(clientOptions, option.WithBaseURL(baseURL))
	}
	return anthropic.NewClient(clientOptions...)
}

func analyzeDuplicateBatches(ctx context.Context, client anthropic.Client, model string, candidates []duplicatePair, threshold float64) []duplicatePair {
	var pairs []duplicatePair

	// Batch candidates into groups for efficient API usage
	const batchSize = 10
	candidateCount := len(candidates)
	for i := 0; i < candidateCount; i += batchSize {
		end := i + batchSize
		if end > candidateCount {
			end = candidateCount
		}
		batch := candidates[i:end]

		results := analyzeWithAI(ctx, client, model, batch)
		pairs = appendDuplicateResults(pairs, results, threshold)
	}

	return pairs
}

func appendDuplicateResults(pairs, results []duplicatePair, threshold float64) []duplicatePair {
	for _, result := range results {
		if result.Similarity >= threshold {
			pairs = append(pairs, result)
		}
	}
	return pairs
}

// analyzeWithAI sends a batch of candidate pairs to the LLM for semantic comparison.
func analyzeWithAI(ctx context.Context, client anthropic.Client, model anthropic.Model, candidates []duplicatePair) []duplicatePair {
	if len(candidates) == 0 {
		return nil
	}

	prompt := duplicateAnalysisPrompt(candidates)
	message, err := requestDuplicateAIAnalysis(ctx, client, model, prompt, len(candidates))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: AI analysis failed: %v\n", err)
		// Fall back to mechanical scores
		return candidates
	}
	if !duplicateAIResponseIsText(message) {
		fmt.Fprintf(os.Stderr, "Warning: unexpected AI response format\n")
		return candidates
	}

	results, err := parseDuplicateAIResponse(message.Content[0].Text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to parse AI response: %v\n", err)
		return candidates
	}
	return duplicatePairsFromAIResults(candidates, results)
}

func duplicateAnalysisPrompt(candidates []duplicatePair) string {
	var sb strings.Builder
	sb.WriteString("You are analyzing issue pairs to determine if they are semantic duplicates.\n")
	sb.WriteString("For each pair, determine if they describe the same problem/task/feature.\n")
	sb.WriteString("Respond with a JSON array of objects, one per pair, with fields:\n")
	sb.WriteString("  - pair_index (int): 0-based index of the pair\n")
	sb.WriteString("  - is_duplicate (bool): true if semantically the same issue\n")
	sb.WriteString("  - confidence (float): 0.0-1.0 how confident you are\n")
	sb.WriteString("  - reason (string): brief explanation\n\n")
	sb.WriteString("Respond ONLY with the JSON array, no other text.\n\n")

	for i, c := range candidates {
		fmt.Fprintf(&sb, "--- Pair %d ---\n", i)
		appendDuplicateIssuePrompt(&sb, "Issue A", c.IssueA)
		appendDuplicateIssuePrompt(&sb, "Issue B", c.IssueB)
		sb.WriteString("\n")
	}
	return sb.String()
}

func appendDuplicateIssuePrompt(sb *strings.Builder, label string, issue *types.Issue) {
	fmt.Fprintf(sb, "%s [%s]: %s\n", label, issue.ID, issue.Title)
	if issue.Description != "" {
		fmt.Fprintf(sb, "  Description: %s\n", truncateDuplicateDescription(issue.Description))
	}
}

func truncateDuplicateDescription(description string) string {
	if len(description) > 500 {
		return description[:500] + "..."
	}
	return description
}

func requestDuplicateAIAnalysis(ctx context.Context, client anthropic.Client, model anthropic.Model, prompt string, batchSize int) (*anthropic.Message, error) {
	tracer := telemetry.Tracer("github.com/jonbaldie/beads/ai")
	aiCtx, aiSpan := tracer.Start(ctx, "anthropic.messages.new")
	aiSpan.SetAttributes(
		attribute.String("bd.ai.model", model),
		attribute.String("bd.ai.operation", "find_duplicates"),
		attribute.Int("bd.ai.batch_size", batchSize),
	)
	t0 := time.Now()
	message, err := client.Messages.New(aiCtx, anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: 2048,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		aiSpan.RecordError(err)
		aiSpan.SetStatus(codes.Error, err.Error())
		aiSpan.End()
		return nil, err
	}
	aiSpan.SetAttributes(
		attribute.Int64("bd.ai.input_tokens", message.Usage.InputTokens),
		attribute.Int64("bd.ai.output_tokens", message.Usage.OutputTokens),
		attribute.Float64("bd.ai.duration_ms", float64(time.Since(t0).Milliseconds())),
	)
	aiSpan.End()
	return message, nil
}

func duplicateAIResponseIsText(message *anthropic.Message) bool {
	return message != nil && len(message.Content) > 0 && message.Content[0].Type == "text"
}

type duplicateAIResult struct {
	PairIndex   int     `json:"pair_index"`
	IsDuplicate bool    `json:"is_duplicate"`
	Confidence  float64 `json:"confidence"`
	Reason      string  `json:"reason"`
}

func parseDuplicateAIResponse(responseText string) ([]duplicateAIResult, error) {
	// Try to extract JSON from the response (handle markdown code blocks).
	jsonText := responseText
	if idx := strings.Index(jsonText, "["); idx >= 0 {
		jsonText = jsonText[idx:]
	}
	if idx := strings.LastIndex(jsonText, "]"); idx >= 0 {
		jsonText = jsonText[:idx+1]
	}

	var results []duplicateAIResult
	if err := json.Unmarshal([]byte(jsonText), &results); err != nil {
		return nil, err
	}
	return results, nil
}

func duplicatePairsFromAIResults(candidates []duplicatePair, results []duplicateAIResult) []duplicatePair {
	var pairs []duplicatePair
	for _, result := range results {
		if result.PairIndex < 0 || result.PairIndex >= len(candidates) || !result.IsDuplicate {
			continue
		}
		candidate := candidates[result.PairIndex]
		pairs = append(pairs, duplicatePair{
			IssueA:     candidate.IssueA,
			IssueB:     candidate.IssueB,
			Similarity: result.Confidence,
			Method:     "ai",
			Reason:     result.Reason,
		})
	}
	return pairs
}

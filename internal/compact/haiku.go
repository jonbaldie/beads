// Package compact provides AI-powered issue compaction using Claude Haiku.
package compact

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"text/template"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jonbaldie/beads/internal/audit"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/telemetry"
	"github.com/jonbaldie/beads/internal/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	maxRetries     = 3
	initialBackoff = 1 * time.Second
)

// errAPIKeyRequired is returned when an API key is needed but not provided.
var errAPIKeyRequired = errors.New("API key required")

// haikuClient wraps the Anthropic API for issue summarization.
type haikuClient struct {
	client         anthropic.Client
	model          anthropic.Model
	apiKeySource   config.AIAPIKeySource
	baseURL        string
	tier1Template  *template.Template
	maxRetries     int
	initialBackoff time.Duration
	auditEnabled   bool
	auditActor     string
	metrics        aiMetrics
}

// newHaikuClient creates a new Haiku API client.
// API key resolution order: ANTHROPIC_API_KEY env var > MINIMAX_API_KEY env var > ai.api_key config > explicit apiKey parameter.
func newHaikuClient(apiKey string) (*haikuClient, error) {
	apiKey, keySource := config.ResolveAIAPIKey(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("%w: set ANTHROPIC_API_KEY, MINIMAX_API_KEY, or ai.api_key in config", errAPIKeyRequired)
	}

	clientOptions := []option.RequestOption{option.WithAPIKey(apiKey)}
	baseURL := config.DefaultAIBaseURL(keySource)
	if baseURL != "" {
		clientOptions = append(clientOptions, option.WithBaseURL(baseURL))
	}

	client := anthropic.NewClient(clientOptions...)

	tier1Tmpl, err := template.New("tier1").Parse(tier1PromptTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse tier1 template: %w", err)
	}

	return &haikuClient{
		client:         client,
		model:          config.DefaultAIModelFor(keySource),
		apiKeySource:   keySource,
		baseURL:        baseURL,
		tier1Template:  tier1Tmpl,
		maxRetries:     maxRetries,
		initialBackoff: initialBackoff,
		metrics:        initAIMetrics(),
	}, nil
}

// SummarizeTier1 creates a structured summary of an issue (Summary, Key Decisions, Resolution).
func (h *haikuClient) SummarizeTier1(ctx context.Context, issue *types.Issue) (string, error) {
	prompt, err := h.renderTier1Prompt(issue)
	if err != nil {
		return "", fmt.Errorf("failed to render prompt: %w", err)
	}

	resp, callErr := h.callWithRetry(ctx, prompt)
	if h.auditEnabled {
		// Best-effort: never fail compaction because audit logging failed.
		e := &audit.Entry{
			Kind:     "llm_call",
			Actor:    h.auditActor,
			IssueID:  issue.ID,
			Model:    h.model,
			Prompt:   prompt,
			Response: resp,
		}
		if callErr != nil {
			e.Error = callErr.Error()
		}
		_, _ = audit.Append(e) // Best effort: audit logging must never fail compaction
	}
	return resp, callErr
}

// aiMetrics holds OTel instruments for Anthropic API calls.
type aiMetrics struct {
	inputTokens  metric.Int64Counter
	outputTokens metric.Int64Counter
	duration     metric.Float64Histogram
}

func initAIMetrics() aiMetrics {
	m := telemetry.Meter("github.com/jonbaldie/beads/ai")
	inputTokens, _ := m.Int64Counter("bd.ai.input_tokens",
		metric.WithDescription("Anthropic API input tokens consumed"),
		metric.WithUnit("{token}"),
	)
	outputTokens, _ := m.Int64Counter("bd.ai.output_tokens",
		metric.WithDescription("Anthropic API output tokens generated"),
		metric.WithUnit("{token}"),
	)
	duration, _ := m.Float64Histogram("bd.ai.request.duration",
		metric.WithDescription("Anthropic API request duration in milliseconds"),
		metric.WithUnit("ms"),
	)
	return aiMetrics{inputTokens: inputTokens, outputTokens: outputTokens, duration: duration}
}

func (h *haikuClient) callWithRetry(ctx context.Context, prompt string) (string, error) {
	tracer := telemetry.Tracer("github.com/jonbaldie/beads/ai")
	ctx, span := tracer.Start(ctx, "anthropic.messages.new")
	defer span.End()
	span.SetAttributes(
		attribute.String("bd.ai.model", h.model),
		attribute.String("bd.ai.operation", "compact"),
	)

	var lastErr error
	params := anthropic.MessageNewParams{
		Model:     h.model,
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	}

	for attempt := 0; attempt <= h.maxRetries; attempt++ {
		if err := waitRetry(ctx, h.initialBackoff, attempt); err != nil {
			return "", err
		}

		message, ms, err := h.callMessage(ctx, params)

		if err == nil {
			return h.processMessage(ctx, span, message, ms, attempt)
		}

		lastErr = err

		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		if err := nonRetryableError(span, err); err != nil {
			return "", err
		}
	}

	if lastErr != nil {
		span.RecordError(lastErr)
		span.SetStatus(codes.Error, lastErr.Error())
	}
	return "", fmt.Errorf("failed after %d retries: %w", h.maxRetries+1, lastErr)
}

func waitRetry(ctx context.Context, initialBackoff time.Duration, attempt int) error {
	if attempt == 0 {
		return nil
	}
	backoff := initialBackoff * time.Duration(math.Pow(2, float64(attempt-1)))
	select {
	case <-time.After(backoff):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *haikuClient) callMessage(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, float64, error) {
	started := time.Now()
	message, err := h.client.Messages.New(ctx, params)
	return message, float64(time.Since(started).Milliseconds()), err
}

func (h *haikuClient) processMessage(ctx context.Context, span trace.Span, message *anthropic.Message, ms float64, attempt int) (string, error) {
	modelAttr := attribute.String("bd.ai.model", h.model)
	if h.metrics.inputTokens != nil {
		h.metrics.inputTokens.Add(ctx, message.Usage.InputTokens, metric.WithAttributes(modelAttr))
		h.metrics.outputTokens.Add(ctx, message.Usage.OutputTokens, metric.WithAttributes(modelAttr))
		h.metrics.duration.Record(ctx, ms, metric.WithAttributes(modelAttr))
	}
	span.SetAttributes(
		attribute.Int64("bd.ai.input_tokens", message.Usage.InputTokens),
		attribute.Int64("bd.ai.output_tokens", message.Usage.OutputTokens),
		attribute.Int("bd.ai.attempts", attempt+1),
	)
	if len(message.Content) == 0 {
		return "", fmt.Errorf("unexpected response format: no content blocks")
	}
	content := message.Content[0]
	if content.Type != "text" {
		return "", fmt.Errorf("unexpected response format: not a text block (type=%s)", content.Type)
	}
	return content.Text, nil
}

func nonRetryableError(span trace.Span, err error) error {
	if isRetryable(err) {
		return nil
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	return fmt.Errorf("non-retryable error: %w", err)
}

func isRetryable(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		statusCode := apiErr.StatusCode
		if statusCode == 429 || statusCode >= 500 {
			return true
		}
		return false
	}

	return false
}

type tier1Data struct {
	Title              string
	Description        string
	Design             string
	AcceptanceCriteria string
	Notes              string
}

func (h *haikuClient) renderTier1Prompt(issue *types.Issue) (string, error) {
	var buf []byte
	w := &bytesWriter{buf: buf}

	data := tier1Data{
		Title:              issue.Title,
		Description:        issue.Description,
		Design:             issue.Design,
		AcceptanceCriteria: issue.AcceptanceCriteria,
		Notes:              issue.Notes,
	}

	if err := h.tier1Template.Execute(w, data); err != nil {
		return "", err
	}
	return string(w.buf), nil
}

type bytesWriter struct {
	buf []byte
}

func (w *bytesWriter) Write(p []byte) (n int, err error) {
	w.buf = append(w.buf, p...)
	return len(p), nil
}

const tier1PromptTemplate = `You are summarizing a closed software issue for long-term storage. Your goal is to COMPRESS the content - the output MUST be significantly shorter than the input while preserving key technical decisions and outcomes.

**Title:** {{.Title}}

**Description:**
{{.Description}}

{{if .Design}}**Design:**
{{.Design}}
{{end}}

{{if .AcceptanceCriteria}}**Acceptance Criteria:**
{{.AcceptanceCriteria}}
{{end}}

{{if .Notes}}**Notes:**
{{.Notes}}
{{end}}

IMPORTANT: Your summary must be shorter than the original. Be concise and eliminate redundancy.

Provide a summary in this exact format:

**Summary:** [2-3 concise sentences covering what was done and why]

**Key Decisions:** [Brief bullet points of only the most important technical choices]

**Resolution:** [One sentence on final outcome and lasting impact]`

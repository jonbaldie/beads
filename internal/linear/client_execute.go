package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/debug"
)

// Execute sends a GraphQL request to the Linear API.
// Handles rate limiting with server-hint-aware backoff: when a 429 response
// includes a Retry-After header, that delay is preferred over the computed
// exponential backoff. A circuit breaker returns ErrRateLimitExhausted when
// remaining quota drops below the configured floor (linear.rate_limit_floor).
// OAuth clients also invalidate and retry once on 401 responses.
func (c *clientTransport) Execute(ctx context.Context, req *GraphQLRequest) (json.RawMessage, error) {
	data, statusCode, err := c.executeOnce(ctx, req)
	if err == nil {
		return data, nil
	}

	// On 401 with OAuth, invalidate token and retry once.
	if statusCode == http.StatusUnauthorized && c.AuthMode == AuthModeOAuth {
		debug.Logf("oauth: received 401, invalidating token and retrying")
		c.TokenManager.Invalidate()
		data, _, retryErr := c.executeOnce(ctx, req)
		if retryErr != nil {
			return nil, retryErr
		}
		return data, nil
	}

	return nil, err
}

// executeOnce performs the actual HTTP request loop with rate-limit retries.
// Returns the response data, the last HTTP status code encountered, and any error.
func (c *clientTransport) executeOnce(ctx context.Context, req *GraphQLRequest) (json.RawMessage, int, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	var lastErr error
	var lastStatus int
	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if rlErr := linearCircuitBreakerError(c); rlErr != nil {
			return nil, lastStatus, rlErr
		}

		result := c.executeAttempt(ctx, body, attempt)
		if result.status != 0 {
			lastStatus = result.status
		}
		if result.err == nil {
			return result.data, lastStatus, nil
		}
		lastErr = result.err
		if !result.retry {
			return nil, lastStatus, result.err
		}
	}

	return nil, lastStatus, fmt.Errorf("max retries (%d) exceeded: %w", MaxRetries+1, lastErr)
}

type linearAttemptResult struct {
	data   json.RawMessage
	status int
	retry  bool
	err    error
}

func (c *clientTransport) executeAttempt(ctx context.Context, body []byte, attempt int) linearAttemptResult {
	httpReq, err := c.newRequest(ctx, body)
	if err != nil {
		return linearAttemptResult{err: err}
	}

	resp, err := c.HTTPClient.Do(httpReq) //nolint:gosec // G704: endpoint is the explicitly configured Linear API endpoint.
	if err != nil {
		return linearAttemptResult{err: fmt.Errorf("request failed (attempt %d/%d): %w", attempt+1, MaxRetries+1, err)}
	}

	respBody, err := readLinearResponseBody(resp)
	if err != nil {
		return linearAttemptResult{err: fmt.Errorf("failed to read response (attempt %d/%d): %w", attempt+1, MaxRetries+1, err)}
	}

	return c.processAttemptResponse(ctx, resp, respBody, attempt)
}

func (c *clientTransport) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	authValue, err := linearAuthHeader(c)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", authValue)
	return httpReq, nil
}

func readLinearResponseBody(resp *http.Response) ([]byte, error) {
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
	_ = resp.Body.Close() // Best effort: HTTP body close; connection may be reused regardless
	return respBody, err
}

func (c *clientTransport) processAttemptResponse(ctx context.Context, resp *http.Response, respBody []byte, attempt int) linearAttemptResult {
	status := resp.StatusCode
	rl := parseRateLimitHeaders(resp.Header)
	recordLinearRateLimitHeaders(c, rl)

	if status == http.StatusTooManyRequests {
		return c.retryAttempt(ctx, status, attempt, rl.RetryAfter)
	}
	if status < 200 || status >= 300 {
		return linearAttemptResult{status: status, err: fmt.Errorf("API error: %s (status %d)", string(respBody), status)}
	}

	data, err := parseLinearGraphQLResponse(respBody)
	return linearAttemptResult{data: data, status: status, err: err}
}

func (c *clientTransport) retryAttempt(ctx context.Context, status, attempt int, retryAfter time.Duration) linearAttemptResult {
	delay := retryAfter
	if delay == 0 {
		delay = RetryDelay * time.Duration(1<<attempt) // Exponential backoff
		if half := int64(delay / 2); half > 0 {
			delay += time.Duration(rand.Int64N(half)) //nolint:gosec // G404: jitter for retry backoff does not need crypto rand
		}
	} else if delay > MaxRetryAfterDelay {
		fmt.Fprintf(os.Stderr, "linear: Retry-After %v exceeds cap %v; using cap\n", delay, MaxRetryAfterDelay)
		delay = MaxRetryAfterDelay
	}

	lastErr := fmt.Errorf("rate limited (attempt %d/%d), retrying after %v", attempt+1, MaxRetries+1, delay)
	select {
	case <-ctx.Done():
		return linearAttemptResult{status: status, err: ctx.Err()}
	case <-time.After(delay):
		return linearAttemptResult{status: status, retry: true, err: lastErr}
	}
}

func parseLinearGraphQLResponse(respBody []byte) (json.RawMessage, error) {
	var gqlResp struct {
		Data   json.RawMessage `json:"data"`
		Errors []GraphQLError  `json:"errors,omitempty"`
	}
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w (body: %s)", err, string(respBody))
	}

	if len(gqlResp.Errors) > 0 {
		errMsgs := make([]string, len(gqlResp.Errors))
		for i, e := range gqlResp.Errors {
			errMsgs[i] = e.Message
		}
		return nil, fmt.Errorf("GraphQL errors: %s", strings.Join(errMsgs, "; "))
	}

	return gqlResp.Data, nil
}

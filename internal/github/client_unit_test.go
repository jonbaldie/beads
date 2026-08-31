package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type trackingReadCloser struct {
	reader io.Reader
	err    error
	closed bool
}

func (r *trackingReadCloser) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	return r.reader.Read(p)
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestMarshalGitHubRequestBody(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		body, err := marshalGitHubRequestBody(nil)
		if err != nil || body != nil {
			t.Fatalf("marshalGitHubRequestBody(nil) = (%q, %v), want (nil, nil)", body, err)
		}
	})

	t.Run("value", func(t *testing.T) {
		body, err := marshalGitHubRequestBody(map[string]string{"name": "beads"})
		if err != nil || string(body) != `{"name":"beads"}` {
			t.Fatalf("marshalGitHubRequestBody(value) = (%q, %v)", body, err)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		body, err := marshalGitHubRequestBody(func() {})
		if err == nil || body != nil || !strings.Contains(err.Error(), "failed to marshal request body") {
			t.Fatalf("marshalGitHubRequestBody(func) = (%q, %v), want wrapped error", body, err)
		}
	})
}

func TestNewGitHubRequest(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "caller")
	client := NewClient("secret", "owner", "repo")

	t.Run("without body", func(t *testing.T) {
		req, err := client.newGitHubRequest(ctx, http.MethodGet, "https://example.test/issues", nil)
		if err != nil {
			t.Fatal(err)
		}
		if req.Context().Value(contextKey{}) != "caller" {
			t.Fatal("request did not retain caller context")
		}
		if req.Header.Get("Authorization") != "Bearer secret" ||
			req.Header.Get(headerAccept) != "application/vnd.github+json" ||
			req.Header.Get(headerAPIVersion) != "2022-11-28" {
			t.Fatalf("unexpected headers: %v", req.Header)
		}
		if got := req.Header.Get(headerContentType); got != "" {
			t.Fatalf("Content-Type = %q, want empty", got)
		}
	})

	t.Run("with body", func(t *testing.T) {
		req, err := client.newGitHubRequest(ctx, http.MethodPost, "https://example.test/issues", []byte(`{"x":1}`))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != `{"x":1}` || req.Header.Get(headerContentType) != "application/json" {
			t.Fatalf("body/content type = (%q, %q)", got, req.Header.Get(headerContentType))
		}
	})

	t.Run("invalid method", func(t *testing.T) {
		req, err := client.newGitHubRequest(ctx, "BAD METHOD", "https://example.test", nil)
		if err == nil || req != nil || !strings.Contains(err.Error(), "failed to create request") {
			t.Fatalf("newGitHubRequest(invalid) = (%v, %v), want wrapped error", req, err)
		}
	})
}

func TestReadGitHubResponseClosesBody(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		body := &trackingReadCloser{reader: strings.NewReader("response")}
		got, err := readGitHubResponse(&http.Response{Body: body})
		if err != nil || string(got) != "response" || !body.closed {
			t.Fatalf("readGitHubResponse = (%q, %v), closed=%v", got, err, body.closed)
		}
	})

	t.Run("read error", func(t *testing.T) {
		wantErr := errors.New("read failed")
		body := &trackingReadCloser{err: wantErr}
		got, err := readGitHubResponse(&http.Response{Body: body})
		if got != nil || !errors.Is(err, wantErr) || !body.closed {
			t.Fatalf("readGitHubResponse = (%q, %v), closed=%v", got, err, body.closed)
		}
	})
}

func TestEvaluateGitHubResponseStatusBoundaries(t *testing.T) {
	client := NewClient("token", "owner", "repo")
	retry := RetryConfig{MaxRetries: 1, BaseDelay: time.Millisecond, MaxBackoff: time.Second}
	headers := http.Header{"X-Test": []string{"value"}}

	for _, status := range []int{http.StatusOK, 299} {
		action := client.evaluateGitHubResponse(&http.Response{StatusCode: status, Header: headers}, []byte("ok"), "url", 0, retry)
		if string(action.body) != "ok" || action.headers.Get("X-Test") != "value" || action.err != nil {
			t.Fatalf("status %d action = %+v", status, action)
		}
	}
	for _, status := range []int{199, 300} {
		action := client.evaluateGitHubResponse(&http.Response{StatusCode: status, Header: headers}, []byte("bad"), "url", 0, retry)
		if action.err == nil || action.body != nil || action.headers != nil {
			t.Fatalf("status %d action = %+v, want API error", status, action)
		}
	}

	action := client.evaluateGitHubResponse(&http.Response{StatusCode: http.StatusForbidden, Header: headers}, []byte(`{"message":"denied"}`), "request-url", 0, retry)
	var authErr *AuthError
	if !errors.As(action.err, &authErr) || authErr.StatusCode != http.StatusForbidden || authErr.Message != "denied" || authErr.URL != "request-url" {
		t.Fatalf("forbidden action error = %#v", action.err)
	}
}

func TestHandleGitHubAttempt(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		body := []byte("ok")
		headers := http.Header{"X-Test": []string{"yes"}}
		out := handleGitHubAttempt(githubResponseAction{body: body, headers: headers}, new(error), new(*RateLimitError))
		if string(out.body) != "ok" || out.headers.Get("X-Test") != "yes" {
			t.Fatalf("outcome = %+v", out)
		}
	})

	t.Run("rate limit retry", func(t *testing.T) {
		rlErr := &RateLimitError{StatusCode: http.StatusTooManyRequests}
		var lastErr error
		var lastRateLimit *RateLimitError
		out := handleGitHubAttempt(githubResponseAction{rateLimit: rlErr, retry: true, retryDelay: 3 * time.Second}, &lastErr, &lastRateLimit)
		if lastRateLimit != rlErr || lastErr != rlErr || !out.retry || out.retryDelay != 3*time.Second {
			t.Fatalf("outcome=%+v lastErr=%v lastRateLimit=%p", out, lastErr, lastRateLimit)
		}
	})

	t.Run("terminal error", func(t *testing.T) {
		wantErr := errors.New("terminal")
		out := handleGitHubAttempt(githubResponseAction{err: wantErr}, new(error), new(*RateLimitError))
		if !errors.Is(out.terminalErr, wantErr) || out.retry {
			t.Fatalf("outcome = %+v", out)
		}
	})

	t.Run("immediate retry", func(t *testing.T) {
		wantErr := errors.New("temporary")
		var lastErr error
		out := handleGitHubAttempt(githubResponseAction{err: wantErr, retry: true, immediateRetry: true}, &lastErr, new(*RateLimitError))
		if lastErr != wantErr || out.retry || out.terminalErr != nil {
			t.Fatalf("outcome=%+v lastErr=%v", out, lastErr)
		}
	})

	t.Run("delayed retry", func(t *testing.T) {
		wantErr := errors.New("temporary")
		var lastErr error
		out := handleGitHubAttempt(githubResponseAction{err: wantErr, retry: true, retryDelay: time.Second}, &lastErr, new(*RateLimitError))
		if lastErr != wantErr || !out.retry || out.retryDelay != time.Second {
			t.Fatalf("outcome=%+v lastErr=%v", out, lastErr)
		}
	})
}

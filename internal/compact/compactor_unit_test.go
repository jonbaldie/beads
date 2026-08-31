package compact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jonbaldie/beads/internal/types"
)

type stubStore struct {
	checkEligibilityFn func(context.Context, string, int) (bool, string, error)
	getIssueFn         func(context.Context, string) (*types.Issue, error)
	snapshotIssueFn    func(context.Context, string, int) error
	updateIssueFn      func(context.Context, string, map[string]interface{}, string) error
	applyCompactionFn  func(context.Context, string, int, int, int, string) error
	addCommentFn       func(context.Context, string, string, string) error
}

func (s *stubStore) CheckEligibility(ctx context.Context, issueID string, tier int) (bool, string, error) {
	if s.checkEligibilityFn != nil {
		return s.checkEligibilityFn(ctx, issueID, tier)
	}
	return false, "", nil
}

func (s *stubStore) GetIssue(ctx context.Context, issueID string) (*types.Issue, error) {
	if s.getIssueFn != nil {
		return s.getIssueFn(ctx, issueID)
	}
	return nil, fmt.Errorf("GetIssue not stubbed")
}

func (s *stubStore) SnapshotIssue(ctx context.Context, issueID string, tier int) error {
	if s.snapshotIssueFn != nil {
		return s.snapshotIssueFn(ctx, issueID, tier)
	}
	return nil
}

func (s *stubStore) UpdateIssue(ctx context.Context, issueID string, updates map[string]interface{}, actor string) error {
	if s.updateIssueFn != nil {
		return s.updateIssueFn(ctx, issueID, updates, actor)
	}
	return nil
}

func (s *stubStore) ApplyCompaction(ctx context.Context, issueID string, tier int, originalSize int, compactedSize int, commitHash string) error {
	if s.applyCompactionFn != nil {
		return s.applyCompactionFn(ctx, issueID, tier, originalSize, compactedSize, commitHash)
	}
	return nil
}

func (s *stubStore) AddComment(ctx context.Context, issueID, actor, comment string) error {
	if s.addCommentFn != nil {
		return s.addCommentFn(ctx, issueID, actor, comment)
	}
	return nil
}

type stubSummarizer struct {
	summary string
	err     error
	calls   int
	mu      sync.Mutex
}

func (s *stubSummarizer) SummarizeTier1(ctx context.Context, issue *types.Issue) (string, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.summary, s.err
}

func (s *stubSummarizer) getCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func stubIssue() *types.Issue {
	return &types.Issue{
		IssueID: types.IssueID{
			ID: "bd-123",
		},
		IssueContent: types.IssueContent{
			Title:              "Fix login",
			Description:        strings.Repeat("A", 20),
			Design:             strings.Repeat("B", 10),
			Notes:              strings.Repeat("C", 5),
			AcceptanceCriteria: "done",
		},
		IssueWorkflow: types.IssueWorkflow{
			Status: types.StatusClosed,
		},
	}
}

func TestIssueContentSizeSumsEveryCompactedField(t *testing.T) {
	issue := &types.Issue{IssueContent: types.IssueContent{
		Description:        "aa",
		Design:             "bbb",
		Notes:              "ccccc",
		AcceptanceCriteria: "ddddddd",
	}}
	if got := issueContentSize(issue); got != 17 {
		t.Fatalf("issueContentSize() = %d, want 17", got)
	}
}

func TestCompactTier1PropagatesContextAndExactSizes(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "sentinel")
	assertContext := func(got context.Context) {
		t.Helper()
		if got == nil || got.Value(contextKey{}) != "sentinel" {
			t.Fatalf("context = %v, want caller context", got)
		}
	}
	issue := stubIssue()
	originalSize := issueContentSize(issue)
	const summary = "short"
	store := &stubStore{
		checkEligibilityFn: func(got context.Context, id string, tier int) (bool, string, error) {
			assertContext(got)
			return true, "", nil
		},
		getIssueFn: func(got context.Context, id string) (*types.Issue, error) {
			assertContext(got)
			return issue, nil
		},
		snapshotIssueFn: func(got context.Context, id string, tier int) error {
			assertContext(got)
			return nil
		},
		updateIssueFn: func(got context.Context, id string, updates map[string]interface{}, actor string) error {
			assertContext(got)
			return nil
		},
		applyCompactionFn: func(got context.Context, id string, tier, original, compacted int, hash string) error {
			assertContext(got)
			if original != originalSize || compacted != len(summary) {
				t.Fatalf("sizes = %d -> %d, want %d -> %d", original, compacted, originalSize, len(summary))
			}
			return nil
		},
		addCommentFn: func(got context.Context, id, actor, comment string) error {
			assertContext(got)
			want := fmt.Sprintf("saved %d", originalSize-len(summary))
			if !strings.Contains(comment, want) {
				t.Fatalf("comment = %q, want %q", comment, want)
			}
			return nil
		},
	}
	c := &Compactor{store: store, summarizer: &stubSummarizer{summary: summary}, config: &Config{}}
	if err := c.CompactTier1(ctx, issue.ID); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureSummarySmallerBoundary(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "sentinel")
	comments := 0
	c := &Compactor{store: &stubStore{addCommentFn: func(got context.Context, _ string, _ string, _ string) error {
		if got == nil || got.Value(contextKey{}) != "sentinel" {
			t.Fatalf("context = %v, want caller context", got)
		}
		comments++
		return nil
	}}}
	if err := c.ensureSummarySmaller(ctx, "bd-1", 10, 9); err != nil {
		t.Fatalf("shorter summary: %v", err)
	}
	if err := c.ensureSummarySmaller(ctx, "bd-1", 10, 10); err == nil {
		t.Fatal("equal-size summary returned nil error")
	}
	if comments != 1 {
		t.Fatalf("warning comments = %d, want 1", comments)
	}
}

func withGitHash(t *testing.T, hash string) func() {
	orig := gitExec
	gitExec = func(string, ...string) ([]byte, error) {
		return []byte(hash), nil
	}
	return func() { gitExec = orig }
}

func TestCompactTier1_Success(t *testing.T) {
	cleanup := withGitHash(t, "deadbeef\n")
	t.Cleanup(cleanup)

	updateCalled := false
	applyCalled := false
	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) { return true, "", nil },
		getIssueFn:         func(context.Context, string) (*types.Issue, error) { return stubIssue(), nil },
		updateIssueFn: func(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
			updateCalled = true
			if updates["description"].(string) != "short" {
				t.Fatalf("expected summarized description")
			}
			if updates["design"].(string) != "" {
				t.Fatalf("design should be cleared")
			}
			return nil
		},
		applyCompactionFn: func(ctx context.Context, id string, tier, original, compacted int, hash string) error {
			applyCalled = true
			if hash != "deadbeef" {
				t.Fatalf("unexpected hash %q", hash)
			}
			return nil
		},
		addCommentFn: func(ctx context.Context, id, actor, comment string) error {
			if !strings.Contains(comment, "saved") {
				t.Fatalf("unexpected comment %q", comment)
			}
			return nil
		},
	}
	summary := &stubSummarizer{summary: "short"}
	c := &Compactor{store: store, summarizer: summary, config: &Config{}}

	if err := c.CompactTier1(context.Background(), "bd-123"); err != nil {
		t.Fatalf("CompactTier1 unexpected error: %v", err)
	}
	if summary.calls != 1 {
		t.Fatalf("expected summarizer used once, got %d", summary.calls)
	}
	if !updateCalled || !applyCalled {
		t.Fatalf("expected update/apply to be called")
	}
}

// TestCompactTier1_SnapshotBeforeOverwrite is the data-safety guard: the
// pre-compaction snapshot must be taken BEFORE the destructive UpdateIssue, so
// compaction is always reversible.
func TestCompactTier1_SnapshotBeforeOverwrite(t *testing.T) {
	cleanup := withGitHash(t, "deadbeef\n")
	t.Cleanup(cleanup)

	var order []string
	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) { return true, "", nil },
		getIssueFn:         func(context.Context, string) (*types.Issue, error) { return stubIssue(), nil },
		snapshotIssueFn: func(ctx context.Context, id string, tier int) error {
			if tier != 1 {
				t.Fatalf("expected snapshot tier 1, got %d", tier)
			}
			order = append(order, "snapshot")
			return nil
		},
		updateIssueFn: func(context.Context, string, map[string]interface{}, string) error {
			order = append(order, "update")
			return nil
		},
	}
	summary := &stubSummarizer{summary: "short"}
	c := &Compactor{store: store, summarizer: summary, config: &Config{}}

	if err := c.CompactTier1(context.Background(), "bd-123"); err != nil {
		t.Fatalf("CompactTier1 unexpected error: %v", err)
	}
	if len(order) != 2 || order[0] != "snapshot" || order[1] != "update" {
		t.Fatalf("expected snapshot before update, got %v", order)
	}
}

// TestCompactTier1_SnapshotError verifies that a failed archive aborts the
// compaction so the original content is never overwritten without a snapshot.
func TestCompactTier1_SnapshotError(t *testing.T) {
	updateCalled := false
	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) { return true, "", nil },
		getIssueFn:         func(context.Context, string) (*types.Issue, error) { return stubIssue(), nil },
		snapshotIssueFn:    func(context.Context, string, int) error { return errors.New("disk full") },
		updateIssueFn: func(context.Context, string, map[string]interface{}, string) error {
			updateCalled = true
			return nil
		},
	}
	summary := &stubSummarizer{summary: "short"}
	c := &Compactor{store: store, summarizer: summary, config: &Config{}}

	err := c.CompactTier1(context.Background(), "bd-123")
	if err == nil || !strings.Contains(err.Error(), "archive pre-compaction snapshot") {
		t.Fatalf("expected snapshot error, got %v", err)
	}
	if updateCalled {
		t.Fatalf("issue was overwritten despite snapshot failure")
	}
}

func TestCompactTier1_Ineligible(t *testing.T) {
	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) { return false, "recently compacted", nil },
	}
	c := &Compactor{store: store, config: &Config{}}

	err := c.CompactTier1(context.Background(), "bd-123")
	if err == nil || !strings.Contains(err.Error(), "recently compacted") {
		t.Fatalf("expected ineligible error, got %v", err)
	}
}

func TestCompactTier1_SummaryNotSmaller(t *testing.T) {
	commentCalled := false
	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) { return true, "", nil },
		getIssueFn:         func(context.Context, string) (*types.Issue, error) { return stubIssue(), nil },
		addCommentFn: func(ctx context.Context, id, actor, comment string) error {
			commentCalled = true
			if !strings.Contains(comment, "Tier 1 compaction skipped") {
				t.Fatalf("unexpected comment %q", comment)
			}
			return nil
		},
	}
	summary := &stubSummarizer{summary: strings.Repeat("X", 40)}
	c := &Compactor{store: store, summarizer: summary, config: &Config{}}

	err := c.CompactTier1(context.Background(), "bd-123")
	if err == nil || !strings.Contains(err.Error(), "compaction would increase size") {
		t.Fatalf("expected size error, got %v", err)
	}
	if !commentCalled {
		t.Fatalf("expected warning comment to be recorded")
	}
}

func TestCompactTier1_UpdateError(t *testing.T) {
	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) { return true, "", nil },
		getIssueFn:         func(context.Context, string) (*types.Issue, error) { return stubIssue(), nil },
		updateIssueFn:      func(context.Context, string, map[string]interface{}, string) error { return errors.New("boom") },
	}
	summary := &stubSummarizer{summary: "short"}
	c := &Compactor{store: store, summarizer: summary, config: &Config{}}

	err := c.CompactTier1(context.Background(), "bd-123")
	if err == nil || !strings.Contains(err.Error(), "failed to update issue") {
		t.Fatalf("expected update error, got %v", err)
	}
}

// --- New constructor tests ---

func TestNew_NilConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("MINIMAX_API_KEY", "")
	store := &stubStore{}
	c, err := New(store, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.config.Concurrency != defaultConcurrency {
		t.Errorf("expected default concurrency %d, got %d", defaultConcurrency, c.config.Concurrency)
	}
	if !c.config.DryRun {
		t.Error("expected DryRun to be set when no API key")
	}
}

func TestNew_DryRunExplicit(t *testing.T) {
	store := &stubStore{}
	c, err := New(store, "", &Config{DryRun: true, Concurrency: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.config.Concurrency != 3 {
		t.Errorf("expected concurrency 3, got %d", c.config.Concurrency)
	}
	if c.summarizer != nil {
		t.Error("expected nil summarizer in dry run")
	}
}

func TestNew_NoAPIKeyFallsToDryRun(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("MINIMAX_API_KEY", "")
	store := &stubStore{}
	c, err := New(store, "", &Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.config.DryRun {
		t.Error("expected DryRun to be set when no API key")
	}
}

func TestNew_WithAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("MINIMAX_API_KEY", "")
	store := &stubStore{}
	c, err := New(store, "test-key-123", &Config{Concurrency: 2, AuditEnabled: true, Actor: "testbot"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.config.Concurrency != 2 {
		t.Errorf("expected concurrency 2, got %d", c.config.Concurrency)
	}
	if c.summarizer == nil {
		t.Error("expected non-nil summarizer with API key")
	}
	if c.config.APIKey != "test-key-123" {
		t.Errorf("expected API key to be set, got %q", c.config.APIKey)
	}
}

func TestNew_ZeroConcurrency(t *testing.T) {
	store := &stubStore{}
	c, err := New(store, "", &Config{DryRun: true, Concurrency: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.config.Concurrency != defaultConcurrency {
		t.Errorf("expected default concurrency %d for zero value, got %d", defaultConcurrency, c.config.Concurrency)
	}
}

func TestNew_NegativeConcurrency(t *testing.T) {
	store := &stubStore{}
	c, err := New(store, "", &Config{DryRun: true, Concurrency: -1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.config.Concurrency != defaultConcurrency {
		t.Errorf("expected default concurrency %d for negative value, got %d", defaultConcurrency, c.config.Concurrency)
	}
}

func TestNew_EnvKeyOverridesParam(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "env-key")
	store := &stubStore{}
	c, err := New(store, "param-key", &Config{Concurrency: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.summarizer == nil {
		t.Error("expected non-nil summarizer when env key set")
	}
}

// --- CompactTier1 additional error path tests ---

func TestCompactTier1_CancelledContext(t *testing.T) {
	c := &Compactor{store: &stubStore{}, config: &Config{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.CompactTier1(ctx, "bd-123")
	if err == nil {
		t.Fatal("expected error")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestCompactTier1_EligibilityCheckError(t *testing.T) {
	cause := errors.New("db error")
	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) {
			return false, "", cause
		},
	}
	c := &Compactor{store: store, config: &Config{}}

	err := c.CompactTier1(context.Background(), "bd-123")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "failed to verify eligibility") {
		t.Errorf("unexpected error: %v", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("error does not wrap cause: %v", err)
	}
}

func TestCompactTier1_IneligibleNoReason(t *testing.T) {
	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) { return false, "", nil },
	}
	c := &Compactor{store: store, config: &Config{}}

	err := c.CompactTier1(context.Background(), "bd-123")
	if err == nil {
		t.Fatal("expected error")
	}
	expected := "issue bd-123 is not eligible for Tier 1 compaction"
	if err.Error() != expected {
		t.Errorf("expected %q, got %q", expected, err.Error())
	}
}

func TestCompactTier1_GetIssueFetchError(t *testing.T) {
	cause := errors.New("fetch error")
	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) { return true, "", nil },
		getIssueFn: func(context.Context, string) (*types.Issue, error) {
			return nil, cause
		},
	}
	c := &Compactor{store: store, config: &Config{}}

	err := c.CompactTier1(context.Background(), "bd-123")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "failed to fetch issue") {
		t.Errorf("unexpected error: %v", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("error does not wrap cause: %v", err)
	}
}

func TestCompactTier1_SummarizerError(t *testing.T) {
	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) { return true, "", nil },
		getIssueFn:         func(context.Context, string) (*types.Issue, error) { return stubIssue(), nil },
	}
	summary := &stubSummarizer{err: errors.New("API error")}
	c := &Compactor{store: store, summarizer: summary, config: &Config{}}

	err := c.CompactTier1(context.Background(), "bd-123")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "failed to summarize") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCompactTier1_ApplyCompactionError(t *testing.T) {
	cleanup := withGitHash(t, "abc\n")
	t.Cleanup(cleanup)

	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) { return true, "", nil },
		getIssueFn:         func(context.Context, string) (*types.Issue, error) { return stubIssue(), nil },
		updateIssueFn:      func(context.Context, string, map[string]interface{}, string) error { return nil },
		applyCompactionFn:  func(context.Context, string, int, int, int, string) error { return errors.New("apply failed") },
	}
	summary := &stubSummarizer{summary: "short"}
	c := &Compactor{store: store, summarizer: summary, config: &Config{}}

	err := c.CompactTier1(context.Background(), "bd-123")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "failed to apply compaction metadata") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCompactTier1_AddCommentError(t *testing.T) {
	cleanup := withGitHash(t, "abc\n")
	t.Cleanup(cleanup)

	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) { return true, "", nil },
		getIssueFn:         func(context.Context, string) (*types.Issue, error) { return stubIssue(), nil },
		updateIssueFn:      func(context.Context, string, map[string]interface{}, string) error { return nil },
		applyCompactionFn:  func(context.Context, string, int, int, int, string) error { return nil },
		addCommentFn:       func(context.Context, string, string, string) error { return errors.New("comment failed") },
	}
	summary := &stubSummarizer{summary: "short"}
	c := &Compactor{store: store, summarizer: summary, config: &Config{}}

	err := c.CompactTier1(context.Background(), "bd-123")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "failed to add compaction comment") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCompactTier1_SummaryNotSmaller_CommentError(t *testing.T) {
	cause := errors.New("comment failed")
	store := &stubStore{
		checkEligibilityFn: func(context.Context, string, int) (bool, string, error) { return true, "", nil },
		getIssueFn:         func(context.Context, string) (*types.Issue, error) { return stubIssue(), nil },
		addCommentFn:       func(context.Context, string, string, string) error { return cause },
	}
	summary := &stubSummarizer{summary: strings.Repeat("X", 40)}
	c := &Compactor{store: store, summarizer: summary, config: &Config{}}

	err := c.CompactTier1(context.Background(), "bd-123")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "failed to record warning") {
		t.Errorf("unexpected error: %v", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("error does not wrap cause: %v", err)
	}
}

// --- CompactTier1Batch additional tests ---

func TestCompactTier1Batch_GetIssueError(t *testing.T) {
	store := &stubStore{
		getIssueFn: func(ctx context.Context, id string) (*types.Issue, error) {
			return nil, errors.New("not found")
		},
	}
	c := &Compactor{store: store, config: &Config{Concurrency: 1}}

	results, err := c.CompactTier1Batch(context.Background(), []string{"bd-1"})
	if err != nil {
		t.Fatalf("batch should not return top-level error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err == nil {
		t.Error("expected error in result")
	}
}

func TestCompactTier1Batch_Empty(t *testing.T) {
	c := &Compactor{store: &stubStore{}, config: &Config{Concurrency: 1}}

	results, err := c.CompactTier1Batch(context.Background(), []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestCompactTier1Batch_MixedResults(t *testing.T) {
	cleanup := withGitHash(t, "cafebabe\n")
	t.Cleanup(cleanup)

	var mu sync.Mutex
	updated := make(map[string]int)
	applied := make(map[string]int)
	store := &stubStore{
		checkEligibilityFn: func(ctx context.Context, id string, tier int) (bool, string, error) {
			switch id {
			case "bd-1":
				return true, "", nil
			case "bd-2":
				return false, "not eligible", nil
			default:
				return false, "", fmt.Errorf("unexpected id %s", id)
			}
		},
		getIssueFn: func(ctx context.Context, id string) (*types.Issue, error) {
			issue := stubIssue()
			issue.ID = id
			return issue, nil
		},
		updateIssueFn: func(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
			mu.Lock()
			updated[id]++
			mu.Unlock()
			return nil
		},
		applyCompactionFn: func(ctx context.Context, id string, tier, original, compacted int, hash string) error {
			mu.Lock()
			applied[id]++
			mu.Unlock()
			return nil
		},
		addCommentFn: func(context.Context, string, string, string) error { return nil },
	}
	summary := &stubSummarizer{summary: "short"}
	c := &Compactor{store: store, summarizer: summary, config: &Config{Concurrency: 2}}

	results, err := c.CompactTier1Batch(context.Background(), []string{"bd-1", "bd-2"})
	if err != nil {
		t.Fatalf("CompactTier1Batch unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	resMap := map[string]*BatchResult{}
	for _, r := range results {
		resMap[r.IssueID] = &r
	}

	if res := resMap["bd-1"]; res == nil || res.Err != nil || res.CompactedSize == 0 {
		t.Fatalf("expected success result for bd-1, got %+v", res)
	}
	if res := resMap["bd-2"]; res == nil || res.Err == nil || !strings.Contains(res.Err.Error(), "not eligible") {
		t.Fatalf("expected ineligible error for bd-2, got %+v", res)
	}
	if updated["bd-1"] != 1 || applied["bd-1"] != 1 {
		t.Fatalf("expected store operations for bd-1 exactly once")
	}
	if updated["bd-2"] != 0 || applied["bd-2"] != 0 {
		t.Fatalf("bd-2 should not be processed")
	}
	if summary.calls != 1 {
		t.Fatalf("summarizer should run once; got %d", summary.calls)
	}
}

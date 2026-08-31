package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

// CreateIssue creates a new issue.
// Delegates SQL work to issueops; handles Dolt versioning for non-ephemeral issues.
func (s *DoltStore) CreateIssue(ctx context.Context, issue *types.Issue, actor string) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.createIssue(ctx, issue, actor)
	})
}

func (s *DoltStore) createIssue(ctx context.Context, issue *types.Issue, actor string) error {
	if issue == nil {
		return fmt.Errorf("issue must not be nil")
	}

	prepareIssueForCreate(s, ctx, issue)
	result, err := s.createIssueInTx(ctx, issue, actor)
	if err != nil {
		return err
	}
	return s.commitCreatedIssue(ctx, issue, result)
}

func prepareIssueForCreate(s *DoltStore, ctx context.Context, issue *types.Issue) {
	// Route to wisps table if ephemeral, no-history, or infra type.
	useWispsTable := issue.Ephemeral || issue.NoHistory || s.IsInfraTypeCtx(ctx, issue.IssueType)
	if useWispsTable && !issue.NoHistory {
		issue.Ephemeral = true // infra types get marked ephemeral (legacy behavior)
	}
}

func (s *DoltStore) createIssueInTx(ctx context.Context, issue *types.Issue, actor string) (issueops.CreateIssueResult, error) {
	var result issueops.CreateIssueResult
	err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
		// SkipPrefixValidation matches legacy behavior: single-issue path does
		// not validate prefixes for explicit IDs.
		bc, err := issueops.NewBatchContext(ctx, tx, storage.BatchCreateOptions{SkipPrefixValidation: true})
		if err != nil {
			return err
		}
		result, err = issueops.CreateIssueInTxWithResult(ctx, tx, bc, issue, actor)
		return err
	})
	return result, err
}

func (s *DoltStore) commitCreatedIssue(ctx context.Context, issue *types.Issue, result issueops.CreateIssueResult) error {
	// Dolt versioning — wisps and no-history issues skip DOLT_COMMIT.
	if issue.Ephemeral || issue.NoHistory {
		return nil
	}
	return s.doltAddAndCommit(ctx, createIssueCommitTables(ctx, issue, result), fmt.Sprintf("bd: create %s", issue.ID))
}

func createIssueCommitTables(ctx context.Context, issue *types.Issue, result issueops.CreateIssueResult) []string {
	return sortedDirtyTables(issueops.CreateIssueDirtyTables(ctx, issue, result))
}

func createIssuesCommitTables(ctx context.Context, issues []*types.Issue, result issueops.CreateIssuesResult) []string {
	return sortedDirtyTables(issueops.CreateIssuesDirtyTables(ctx, issues, result))
}

func sortedDirtyTables(dirty map[string]bool) []string {
	if len(dirty) == 0 {
		return nil
	}
	tables := make([]string, 0, len(dirty))
	for table := range dirty {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	return tables
}

// CreateIssues creates multiple issues in a single transaction
func (s *DoltStore) CreateIssues(ctx context.Context, issues []*types.Issue, actor string) error {
	return s.CreateIssuesWithFullOptions(ctx, issues, actor, storage.BatchCreateOptions{
		SkipPrefixValidation: false,
	})
}

// CreateIssuesWithFullOptions creates multiple issues with full options control.
// Delegates SQL work to issueops; handles Dolt versioning for non-ephemeral batches.
func (s *DoltStore) CreateIssuesWithFullOptions(ctx context.Context, issues []*types.Issue, actor string, opts storage.BatchCreateOptions) error {
	return s.withCircuitWrite(ctx, func(ctx context.Context) error {
		return s.createIssuesWithFullOptions(ctx, issues, actor, opts)
	})
}

func (s *DoltStore) createIssuesWithFullOptions(ctx context.Context, issues []*types.Issue, actor string, opts storage.BatchCreateOptions) error {
	if len(issues) == 0 {
		return nil
	}

	// All-wisps fast path: one SQL transaction, no Dolt versioning.
	// Covers both ephemeral issues and no-history issues (both skip DOLT_COMMIT).
	if issueops.AllWisps(issues) {
		for _, issue := range issues {
			if !issue.NoHistory {
				issue.Ephemeral = true
			}
		}
		return s.withRetryTx(ctx, func(tx *sql.Tx) error {
			_, err := issueops.CreateIssuesInTxWithResult(ctx, tx, issues, actor, opts)
			return err
		})
	}

	var result issueops.CreateIssuesResult
	if err := s.withRetryTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = issueops.CreateIssuesInTxWithResult(ctx, tx, issues, actor, opts)
		return err
	}); err != nil {
		return err
	}

	// GH#2455: Stage only the tables we modified, then commit without -A.
	return s.doltAddAndCommit(ctx,
		createIssuesCommitTables(ctx, issues, result),
		fmt.Sprintf("bd: create %d issue(s)", len(issues)))
}

// GetIssue retrieves an issue by ID.
// Returns storage.ErrNotFound (wrapped) if the issue does not exist.
func (s *DoltStore) GetIssue(ctx context.Context, id string) (*types.Issue, error) {
	var issue *types.Issue
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		issue, err = issueops.GetIssueInTx(ctx, tx, id)
		return err
	})
	return issue, err
}

// GetIssueByExternalRef retrieves an issue by external reference.
// Returns storage.ErrNotFound (wrapped) if no issue with the given external reference exists.
func (s *DoltStore) GetIssueByExternalRef(ctx context.Context, externalRef string) (*types.Issue, error) {
	var id string
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = issueops.GetIssueByExternalRefInTx(ctx, tx, externalRef)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetIssue(ctx, id)
}

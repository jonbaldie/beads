package domain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/types"
)

func (u *issueCreateModule) CreateIssue(ctx context.Context, params CreateIssueParams, actor string) (CreateIssueResult, error) {
	return u.create(ctx, params, actor, false)
}

func (u *issueCreateModule) CreateWisp(ctx context.Context, params CreateIssueParams, actor string) (CreateIssueResult, error) {
	return u.create(ctx, params, actor, true)
}

func (u *issueCreateModule) create(ctx context.Context, params CreateIssueParams, actor string, useWisp bool) (CreateIssueResult, error) {
	if params.Issue == nil {
		return CreateIssueResult{}, fmt.Errorf("create: Issue must not be nil")
	}
	issue := params.Issue
	prepareIssueForCreate(issue)

	if err := prepareCreateIssue(ctx, u, issue, params, actor, useWisp); err != nil {
		return CreateIssueResult{}, err
	}

	insertOpts := InsertIssueOpts{UseWispsTable: useWisp, CreateOnly: params.CreateOnly}
	if err := u.issueRepo.Insert(ctx, issue, actor, insertOpts); err != nil {
		return CreateIssueResult{}, fmt.Errorf("create: insert: %w", err)
	}

	if err := insertCreateComments(ctx, u.commentRepo, issue, params.Comments, actor, useWisp); err != nil {
		return CreateIssueResult{}, err
	}

	result := CreateIssueResult{Issue: issue}
	if err := completeCreateIssue(ctx, u, issue, params, actor, useWisp, &result); err != nil {
		return result, err
	}

	return result, nil
}

func prepareIssueForCreate(issue *types.Issue) {
	if issue.Status == "" {
		issue.Status = types.StatusOpen
	}
	// Set CreatedAt before the mint path: GenerateHashID hashes timestamp,
	// so it must be stable across the candidate loop and the eventual
	// db.Insert (which would otherwise normalize a zero value to a later
	// time and break candidate reproducibility on retry).
	if issue.CreatedAt.IsZero() {
		issue.CreatedAt = time.Now().UTC()
	}
}

func prepareCreateIssue(ctx context.Context, u *issueCreateModule, issue *types.Issue, params CreateIssueParams, actor string, useWisp bool) error {
	if err := u.assignCreateID(ctx, issue, params, actor, useWisp); err != nil {
		return err
	}
	if err := validateCreateOnlyID(ctx, u.cfgRepo, issue.ID, params); err != nil {
		return err
	}
	inheritCreateSource(ctx, u.lookup, issue, params.DiscoveredFromParent)
	return nil
}

func completeCreateIssue(ctx context.Context, u *issueCreateModule, issue *types.Issue, params CreateIssueParams, actor string, useWisp bool, result *CreateIssueResult) error {
	if err := insertCreateParent(ctx, u.depRepo, issue.ID, params.ParentID, actor, useWisp); err != nil {
		return err
	}
	result.PostCreateWrites = params.ParentID != ""

	inherited, err := inheritedCreateLabels(ctx, u.issueRepo, u.labelRepo, params)
	if err != nil {
		return err
	}
	result.InheritedLabels = inherited
	return completeCreateAssociations(ctx, u, issue, params, actor, useWisp, result)
}

func completeCreateAssociations(ctx context.Context, u *issueCreateModule, issue *types.Issue, params CreateIssueParams, actor string, useWisp bool, result *CreateIssueResult) error {
	if err := insertCreateLabels(ctx, u.labelRepo, issue.ID, params.Labels, result.InheritedLabels, actor, useWisp); err != nil {
		return err
	}
	result.PostCreateWrites = result.PostCreateWrites || len(params.Labels) > 0 || len(result.InheritedLabels) > 0

	if err := insertCreateDependencies(ctx, u.issueRepo, u.depRepo, issue.ID, params.Dependencies, actor); err != nil {
		return err
	}
	result.PostCreateWrites = result.PostCreateWrites || len(params.Dependencies) > 0

	if err := insertCreateWait(ctx, u.depRepo, issue.ID, params.WaitsFor, actor, useWisp); err != nil {
		return err
	}
	result.PostCreateWrites = result.PostCreateWrites || params.WaitsFor != nil
	return nil
}

func (u *issueCreateModule) assignCreateID(ctx context.Context, issue *types.Issue, params CreateIssueParams, actor string, useWisp bool) error {
	switch {
	case params.ExplicitID != "":
		issue.ID = params.ExplicitID
	case params.ParentID != "":
		childID, err := u.counterRepo.NextChildID(ctx, params.ParentID, ChildCounterOpts{UseWispsTable: useWisp})
		if err != nil {
			return fmt.Errorf("create: next child ID for %s: %w", params.ParentID, err)
		}
		issue.ID = childID
	case issue.ID == "":
		minted, err := u.mintTopLevelID(ctx, issue, actor, useWisp)
		if err != nil {
			return fmt.Errorf("create: mint top-level ID: %w", err)
		}
		issue.ID = minted
	}
	return nil
}

func validateCreateOnlyID(ctx context.Context, cfgRepo ConfigSQLRepository, id string, params CreateIssueParams) error {
	if !params.CreateOnly || params.ExplicitID == "" || params.ForcePrefix {
		return nil
	}
	configuredPrefix, err := cfgRepo.GetConfig(ctx, "issue_prefix")
	if err != nil {
		return fmt.Errorf("create: read issue prefix: %w", err)
	}
	allowedPrefixes, err := cfgRepo.GetConfig(ctx, "allowed_prefixes")
	if err != nil {
		return fmt.Errorf("create: read allowed prefixes: %w", err)
	}
	if err := validateExplicitIDPrefix(id, strings.TrimSuffix(configuredPrefix, "-"), allowedPrefixes); err != nil {
		return fmt.Errorf("create: explicit ID prefix: %w", err)
	}
	return nil
}

func inheritCreateSource(ctx context.Context, lookup *issueLookupModule, issue *types.Issue, parentID string) {
	if parentID == "" {
		return
	}
	if parent, err := lookup.GetIssue(ctx, parentID); err == nil && parent.SourceRepo != "" {
		issue.SourceRepo = parent.SourceRepo
	}
}

func insertCreateComments(ctx context.Context, repo CommentSQLRepository, issue *types.Issue, comments []*types.Comment, actor string, useWisp bool) error {
	if len(comments) == 0 {
		return nil
	}
	issue.Comments = make([]*types.Comment, 0, len(comments))
	for _, comment := range comments {
		if comment == nil {
			return fmt.Errorf("create: comment must not be nil")
		}
		copy := *comment
		copy.IssueID = issue.ID
		if copy.Author == "" {
			copy.Author = actor
		}
		inserted, err := repo.InsertRecord(ctx, &copy, CommentOpts{UseWispsTable: useWisp})
		if err != nil {
			return fmt.Errorf("create: insert comment: %w", err)
		}
		issue.Comments = append(issue.Comments, inserted)
	}
	return nil
}

func insertCreateParent(ctx context.Context, repo DependencySQLRepository, issueID, parentID, actor string, useWisp bool) error {
	if parentID == "" {
		return nil
	}
	dep := &types.Dependency{IssueID: issueID, DependsOnID: parentID, Type: types.DepParentChild}
	if err := repo.Insert(ctx, dep, actor, DepInsertOpts{UseWispsTable: useWisp}); err != nil {
		return fmt.Errorf("create: add parent-child dep: %w", err)
	}
	return nil
}

func inheritedCreateLabels(ctx context.Context, issueRepo IssueSQLRepository, labelRepo LabelSQLRepository, params CreateIssueParams) ([]string, error) {
	if !params.InheritLabelsFromParent || params.ParentID == "" {
		return nil, nil
	}
	parentIsWisp, err := isWispID(ctx, issueRepo, params.ParentID)
	if err != nil {
		return nil, fmt.Errorf("create: determine parent tier for label inheritance from %s: %w", params.ParentID, err)
	}
	parentLabels, err := labelRepo.List(ctx, params.ParentID, LabelOpts{UseWispsTable: parentIsWisp})
	if dberrors.IsTableNotExist(err) {
		// Older schemas may lack the wisp label table; nothing to inherit.
		return nil, nil
	}
	if err != nil {
		// Swallowing this silently created children missing their inherited
		// labels (bd-6dnrw.44 P3); the create is transactional, so failing loud
		// is safe.
		return nil, fmt.Errorf("create: read parent labels for inheritance from %s: %w", params.ParentID, err)
	}
	existing := make(map[string]bool, len(params.Labels))
	for _, label := range params.Labels {
		existing[label] = true
	}
	inherited := make([]string, 0, len(parentLabels))
	for _, label := range parentLabels {
		if !existing[label] {
			inherited = append(inherited, label)
		}
	}
	return inherited, nil
}

func insertCreateLabels(ctx context.Context, repo LabelSQLRepository, issueID string, labels, inherited []string, actor string, useWisp bool) error {
	for _, label := range labels {
		if err := repo.Insert(ctx, issueID, label, actor, LabelOpts{UseWispsTable: useWisp}); err != nil {
			return fmt.Errorf("create: add label %s: %w", label, err)
		}
	}
	for _, label := range inherited {
		if err := repo.Insert(ctx, issueID, label, actor, LabelOpts{UseWispsTable: useWisp}); err != nil {
			return fmt.Errorf("create: add inherited label %s: %w", label, err)
		}
	}
	return nil
}

func insertCreateDependencies(ctx context.Context, issueRepo IssueSQLRepository, depRepo DependencySQLRepository, issueID string, specs []DependencySpec, actor string) error {
	for _, spec := range specs {
		dep := &types.Dependency{
			IssueID:     issueID,
			DependsOnID: spec.TargetID,
			Type:        spec.Type,
			Metadata:    spec.Metadata,
			ThreadID:    spec.ThreadID,
		}
		if spec.SwapDirection {
			dep.IssueID, dep.DependsOnID = dep.DependsOnID, dep.IssueID
		}
		depSourceIsWisp, err := isWispID(ctx, issueRepo, dep.IssueID)
		if err != nil {
			return fmt.Errorf("create: determine dep source tier for %s: %w", dep.IssueID, err)
		}
		if err := depRepo.Insert(ctx, dep, actor, DepInsertOpts{UseWispsTable: depSourceIsWisp}); err != nil {
			return fmt.Errorf("create: add dep %s -> %s: %w", dep.IssueID, dep.DependsOnID, err)
		}
	}
	return nil
}

func insertCreateWait(ctx context.Context, repo DependencySQLRepository, issueID string, waits *WaitsForSpec, actor string, useWisp bool) error {
	if waits == nil {
		return nil
	}
	// Spawner identity is the depends_on_id; metadata carries the gate.
	dep, err := types.NewWaitsForDependency(issueID, waits.SpawnerID, waits.Gate)
	if err != nil {
		return fmt.Errorf("create: marshal waits-for meta: %w", err)
	}
	if err := repo.Insert(ctx, dep, actor, DepInsertOpts{UseWispsTable: useWisp}); err != nil {
		return fmt.Errorf("create: add waits-for: %w", err)
	}
	return nil
}

func validateExplicitIDPrefix(id, prefix, allowedPrefixes string) error {
	if strings.HasPrefix(id, prefix+"-") {
		return nil
	}
	for _, allowed := range strings.Split(allowedPrefixes, ",") {
		allowed = strings.TrimSpace(allowed)
		if allowed != "" && strings.HasPrefix(id, allowed+"-") {
			return nil
		}
	}
	return fmt.Errorf("%w: issue ID %s does not match configured prefix %s", storage.ErrPrefixMismatch, id, prefix)
}

func (u *issueCreateModule) CreateIssues(ctx context.Context, params []CreateIssueParams, actor string) (CreateIssuesResult, error) {
	return u.createMany(ctx, params, actor, false)
}

func (u *issueCreateModule) CreateWisps(ctx context.Context, params []CreateIssueParams, actor string) (CreateIssuesResult, error) {
	return u.createMany(ctx, params, actor, true)
}

func (u *issueCreateModule) createMany(ctx context.Context, params []CreateIssueParams, actor string, useWisp bool) (CreateIssuesResult, error) {
	result := CreateIssuesResult{}
	for i := range params {
		r, err := u.create(ctx, params[i], actor, useWisp)
		if err != nil {
			return result, fmt.Errorf("createMany[%d]: %w", i, err)
		}
		result.Issues = append(result.Issues, r.Issue)
	}
	return result, nil
}

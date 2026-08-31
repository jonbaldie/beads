package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/spf13/cobra"
)

// createFormRawInput holds the raw string values from the form UI.
// This struct encapsulates all form fields before parsing/conversion.
type createFormRawInput struct {
	Title       string
	Description string
	IssueType   string
	Priority    string // String from select, e.g., "0", "1", "2"
	Assignee    string
	Labels      string // Comma-separated
	Design      string
	Acceptance  string
	ExternalRef string
	Deps        string // Comma-separated, format: "type:id" or "id"
}

// createFormValues holds the parsed values from the create-form input.
// This struct is used to pass form data to the issue creation logic,
// allowing the creation logic to be tested independently of the form UI.
type createFormValues struct {
	Title              string
	Description        string
	IssueType          string
	Priority           int
	Assignee           string
	Labels             []string
	Design             string
	AcceptanceCriteria string
	ExternalRef        string
	Dependencies       []string
	ParentID           string // Parent issue ID for hierarchical child creation
}

// parseCreateFormInput parses raw form input into a createFormValues struct.
// It handles comma-separated labels and dependencies, and converts priority strings.
func parseCreateFormInput(raw *createFormRawInput) *createFormValues {
	// Parse priority
	priority, err := strconv.Atoi(raw.Priority)
	if err != nil {
		priority = 2 // Default to medium if parsing fails
	}

	// Parse labels
	var labels []string
	if raw.Labels != "" {
		for _, l := range strings.Split(raw.Labels, ",") {
			l = strings.TrimSpace(l)
			if l != "" {
				labels = append(labels, l)
			}
		}
	}

	// Parse dependencies
	var deps []string
	if raw.Deps != "" {
		for _, d := range strings.Split(raw.Deps, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				deps = append(deps, d)
			}
		}
	}

	return &createFormValues{
		Title:              raw.Title,
		Description:        raw.Description,
		IssueType:          raw.IssueType,
		Priority:           priority,
		Assignee:           raw.Assignee,
		Labels:             labels,
		Design:             raw.Design,
		AcceptanceCriteria: raw.Acceptance,
		ExternalRef:        raw.ExternalRef,
		Dependencies:       deps,
	}
}

// CreateIssueFromFormValues creates an issue from the given form values.
// It returns the created issue and any error that occurred.
// This function handles parent-child relationships, labels, dependencies,
// and source_repo inheritance.
func CreateIssueFromFormValues(ctx context.Context, s storage.DoltStorage, fv *createFormValues, actor string) (*types.Issue, error) {
	explicitID, inheritedLabels, ctx, err := reserveCreateFormParent(ctx, s, fv.ParentID)
	if err != nil {
		return nil, err
	}
	issue := newIssueFromFormValues(fv, explicitID, inheritedLabels)
	inheritCreateFormSourceRepo(ctx, s, fv, issue)
	edges := createDepEdges{parentID: fv.ParentID, specs: parseCreateFormDepSpecs(fv.Dependencies)}
	// The issue and its edges (parent-child per GH#1983, plus form deps)
	// commit in one transaction; a failed edge rolls back the create instead
	// of leaving a dep-less issue behind (same contract as bd create).
	if err := createIssueWithDeps(ctx, s, issue, actor, edges); err != nil {
		return nil, fmt.Errorf("failed to create issue: %w", err)
	}
	if err := maybeCommitBareCreateForm(ctx, s, issue, edges); err != nil {
		return nil, err
	}
	return issue, nil
}

func reserveCreateFormParent(ctx context.Context, s storage.DoltStorage, parentID string) (string, []string, context.Context, error) {
	if parentID == "" {
		return "", nil, ctx, nil
	}
	if _, err := s.GetIssue(ctx, parentID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", nil, ctx, fmt.Errorf("parent issue %s not found", parentID)
		}
		return "", nil, ctx, fmt.Errorf("failed to check parent issue: %w", err)
	}
	childID, err := s.GetNextChildID(ctx, parentID)
	if err != nil {
		return "", nil, ctx, fmt.Errorf("failed to generate child ID: %w", err)
	}
	ctx = storage.WithReservedChildCounter(ctx, parentID, childID)
	// Inherit parent labels (GH#2100), matching bd create --parent behavior
	inheritedLabels, _ := s.GetLabels(ctx, parentID)
	return childID, inheritedLabels, ctx, nil
}

func newIssueFromFormValues(fv *createFormValues, explicitID string, inheritedLabels []string) *types.Issue {
	var externalRefPtr *string
	if fv.ExternalRef != "" {
		externalRefPtr = &fv.ExternalRef
	}
	issue := &types.Issue{
		IssueContent: types.IssueContent{
			Title:              fv.Title,
			Description:        fv.Description,
			Design:             fv.Design,
			AcceptanceCriteria: fv.AcceptanceCriteria,
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  fv.Priority,
			IssueType: types.IssueType(fv.IssueType).Normalize(),
			Assignee:  fv.Assignee,
		},
		IssueTimes: types.IssueTimes{
			CreatedBy: getActorWithGit(),
		},
		IssueMeta: types.IssueMeta{
			ExternalRef: externalRefPtr,
		},
		IssueGraph: types.IssueGraph{
			// GH#748: track who created the issue
			Labels: mergeCreateLabels(fv.Labels, inheritedLabels),
		},
	}
	if explicitID != "" {
		issue.ID = explicitID
	}
	return issue
}

func inheritCreateFormSourceRepo(ctx context.Context, s storage.DoltStorage, fv *createFormValues, issue *types.Issue) {
	dfParent := discoveredFromParent(fv.Dependencies)
	if dfParent == "" {
		return
	}
	parentIssue, err := s.GetIssue(ctx, dfParent)
	if err == nil && parentIssue != nil && parentIssue.SourceRepo != "" {
		issue.SourceRepo = parentIssue.SourceRepo
	}
}

func parseCreateFormDepSpecs(deps []string) []domain.DependencySpec {
	// Parse dependency specs before creating anything. The form keeps its
	// historical lenient parsing (warn and skip malformed entries), but the
	// edges that do parse commit atomically with the create below.
	var depSpecs []domain.DependencySpec
	for _, depSpec := range deps {
		depSpec = strings.TrimSpace(depSpec)
		if depSpec == "" {
			continue
		}
		depType, dependsOnID := parseCreateFormDepSpec(depSpec)
		if !depType.IsValid() {
			fmt.Fprintf(os.Stderr, "Warning: invalid dependency type '%s' (valid: blocks, related, parent-child, discovered-from)\n", depType)
			continue
		}
		depSpecs = append(depSpecs, domain.DependencySpec{Type: depType, TargetID: dependsOnID})
	}
	return depSpecs
}

func parseCreateFormDepSpec(depSpec string) (types.DependencyType, string) {
	if !strings.Contains(depSpec, ":") {
		return types.DepBlocks, depSpec
	}
	parts := strings.SplitN(depSpec, ":", 2)
	return types.DependencyType(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1])
}

func maybeCommitBareCreateForm(ctx context.Context, s storage.DoltStorage, issue *types.Issue, edges createDepEdges) error {
	if !edges.empty() {
		return nil
	}
	// Bare create: preserve the embedded-mode follow-up Dolt commit.
	// The deps path commits inside its transaction instead.
	shouldCommit, err := shouldCommitCreatePostWrites(issue, false)
	if err != nil {
		return fmt.Errorf("dolt auto-commit: %w", err)
	}
	if !shouldCommit {
		return nil
	}
	if err := s.Commit(ctx, fmt.Sprintf("bd: create %s", issue.ID)); err != nil && !isDoltNothingToCommit(err) {
		WarnError("failed to commit post-create metadata: %v", err)
	}
	return nil
}

var createFormCmd = &cobra.Command{
	Use:     "create-form",
	GroupID: "issues",
	Short:   "Create a new issue using an interactive form",
	Long: `Create a new issue using an interactive terminal form.

This command provides a user-friendly form interface for creating issues,
with fields for title, description, type, priority, labels, and more.

Use --parent to create a sub-issue under an existing parent issue.
The child will get an auto-generated hierarchical ID (e.g., parent-id.1).

The form uses keyboard navigation:
  - Tab/Shift+Tab: Move between fields
  - Enter: Submit the form (on the last field or submit button)
  - Ctrl+C: Cancel and exit
  - Arrow keys: Navigate within select fields`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return HandleErrorRespectJSON("create-form is not supported in proxied-server mode")
		}
		if err := CheckReadonly("create-form"); err != nil {
			return err
		}

		evt := metrics.NewCommandEvent("create-form")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		return runCreateForm(cmd)
	},
}

func runCreateForm(cmd *cobra.Command) error {
	parentID, _ := cmd.Flags().GetString("parent")
	raw := &createFormRawInput{}
	if err := runCreateIssueForm(raw); err != nil {
		return err
	}

	fv := parseCreateFormInput(raw)
	fv.ParentID = parentID

	issue, err := CreateIssueFromFormValues(getRootContext(), getStore(), fv, getActor())
	if err != nil {
		return HandleError("%v", err)
	}

	if isJSONOutput() {
		return outputJSON(issue)
	}
	printCreatedIssue(issue)
	return nil
}

func runCreateIssueForm(raw *createFormRawInput) error {
	form := huh.NewForm(
		newCreateFormCoreGroup(raw),
		newCreateFormMetaGroup(raw),
		newCreateFormNotesGroup(raw),
		newCreateFormSubmitGroup(raw),
	).WithTheme(huh.ThemeFunc(huh.ThemeDracula))
	err := form.Run()
	if err == nil {
		return nil
	}
	if err == huh.ErrUserAborted {
		fmt.Fprintln(os.Stderr, "Issue creation canceled.")
		return nil
	}
	return HandleError("form error: %v", err)
}

func newCreateFormCoreGroup(raw *createFormRawInput) *huh.Group {
	return huh.NewGroup(
		huh.NewInput().
			Title("Title").
			Description("Brief summary of the issue (required)").
			Placeholder("e.g., Fix authentication bug in login handler").
			Value(&raw.Title).
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return fmt.Errorf("title is required")
				}
				return types.ValidateIssueTitle(s)
			}),
		huh.NewText().
			Title("Description").
			Description("Detailed context about the issue").
			Placeholder("Explain why this issue exists and what needs to be done...").
			CharLimit(5000).
			Value(&raw.Description),
		huh.NewSelect[string]().
			Title("Type").
			Description("Categorize the kind of work").
			Options(
				huh.NewOption("Task", "task"),
				huh.NewOption("Bug", "bug"),
				huh.NewOption("Feature", "feature"),
				huh.NewOption("Epic", "epic"),
				huh.NewOption("Chore", "chore"),
			).
			Value(&raw.IssueType),
		huh.NewSelect[string]().
			Title("Priority").
			Description("Set urgency level").
			Options(
				huh.NewOption("P0 - Critical", "0"),
				huh.NewOption("P1 - High", "1"),
				huh.NewOption("P2 - Medium (default)", "2"),
				huh.NewOption("P3 - Low", "3"),
				huh.NewOption("P4 - Backlog", "4"),
			).
			Value(&raw.Priority),
	)
}

func newCreateFormMetaGroup(raw *createFormRawInput) *huh.Group {
	return huh.NewGroup(
		huh.NewInput().
			Title("Assignee").
			Description("Who should work on this? (optional)").
			Placeholder("username or email").
			Value(&raw.Assignee),
		huh.NewInput().
			Title("Labels").
			Description("Comma-separated tags (optional)").
			Placeholder("e.g., urgent, backend, needs-review").
			Value(&raw.Labels),
		huh.NewInput().
			Title("External Reference").
			Description("Link to external tracker (optional)").
			Placeholder("e.g., gh-123, jira-ABC-456").
			Value(&raw.ExternalRef),
	)
}

func newCreateFormNotesGroup(raw *createFormRawInput) *huh.Group {
	return huh.NewGroup(
		huh.NewText().
			Title("Design Notes").
			Description("Technical approach or design details (optional)").
			Placeholder("Describe the implementation approach...").
			CharLimit(5000).
			Value(&raw.Design),
		huh.NewText().
			Title("Acceptance Criteria").
			Description("How do we know this is done? (optional)").
			Placeholder("List the criteria for completion...").
			CharLimit(5000).
			Value(&raw.Acceptance),
	)
}

func newCreateFormSubmitGroup(raw *createFormRawInput) *huh.Group {
	return huh.NewGroup(
		huh.NewInput().
			Title("Dependencies").
			Description("Format: type:id or just id (optional)").
			Placeholder("e.g., discovered-from:bd-20, blocks:bd-15").
			Value(&raw.Deps),
		huh.NewConfirm().
			Title("Create this issue?").
			Affirmative("Create").
			Negative("Cancel"),
	)
}

func printCreatedIssue(issue *types.Issue) {
	fmt.Printf("\n%s Created issue: %s\n", ui.RenderPass("✓"), formatFeedbackID(issue.ID, issue.Title))
	fmt.Printf("  Type:     %s\n", issue.IssueType)
	fmt.Printf("  Priority: P%d\n", issue.Priority)
	fmt.Printf("  Status:   %s\n", issue.Status)
	if issue.Assignee != "" {
		fmt.Printf("  Assignee: %s\n", issue.Assignee)
	}
	if issue.Description != "" {
		desc := issue.Description
		if len(desc) > 100 {
			desc = desc[:97] + "..."
		}
		fmt.Printf("  Description: %s\n", desc)
	}
}

func init() {
	// Note: --json flag is defined as a persistent flag in main.go
	createFormCmd.Flags().String("parent", "", "Parent issue ID for creating a hierarchical child (e.g., 'bd-a3f8e9')")
	rootCmd.AddCommand(createFormCmd)
}

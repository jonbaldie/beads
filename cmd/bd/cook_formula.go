package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/formula"
	"github.com/jonbaldie/beads/internal/types"
)

// cookFormulaToSubgraph creates an in-memory TemplateSubgraph from a resolved formula.
// This is the ephemeral proto implementation - no database storage.
// The returned subgraph can be passed directly to cloneSubgraph for instantiation.
//
//nolint:unparam // error return kept for API consistency with future error handling
func cookFormulaToSubgraph(f *formula.Formula, protoID string) (*TemplateSubgraph, error) {
	// Map step ID -> created issue
	issueMap := make(map[string]*types.Issue)

	// Collect all issues and dependencies
	var issues []*types.Issue
	var deps []*types.Dependency

	// Determine root title: use {{title}} placeholder if the variable is defined,
	// otherwise fall back to formula name (GH#852)
	rootTitle := f.Formula
	if _, hasTitle := f.Vars["title"]; hasTitle {
		rootTitle = "{{title}}"
	}

	// Determine root description: use {{desc}} placeholder if the variable is defined,
	// otherwise fall back to formula description (GH#852)
	rootDesc := f.Description
	if _, hasDesc := f.Vars["desc"]; hasDesc {
		rootDesc = "{{desc}}"
	}

	// Create root proto molecule
	rootIssue := &types.Issue{
		IssueID: types.IssueID{
			ID: protoID,
		},
		IssueContent: types.IssueContent{
			Title:       rootTitle,
			Description: rootDesc,
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: types.TypeMolecule,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		IssueWisp: types.IssueWisp{
			IsTemplate: true,
		},
	}
	issues = append(issues, rootIssue)
	issueMap[protoID] = rootIssue

	// Collect issues for each step (use protoID as parent for step IDs)
	// The unified collectSteps builds both issueMap and idMapping
	idMapping := make(map[string]string)
	collectSteps(f.Steps, protoID, idMapping, issueMap, &issues, &deps, nil) // nil = keep labels on issues

	// Collect dependencies from depends_on using the idMapping built above
	for _, step := range f.Steps {
		collectDependencies(step, idMapping, &deps)
	}

	return &TemplateSubgraph{
		Root:         rootIssue,
		Issues:       issues,
		Dependencies: deps,
		IssueMap:     issueMap,
	}, nil
}

// createGateIssue creates a gate issue for a step with a Gate field.
// Gate issues have type=gate and block the step they guard.
// Returns the gate issue and its ID.
func createGateIssue(step *formula.Step, parentID string) *types.Issue {
	if step.Gate == nil {
		return nil
	}

	// Generate gate issue ID: {parentID}.gate-{step.ID}
	gateID := fmt.Sprintf("%s.gate-%s", parentID, step.ID)

	// Build title from gate type and ID
	title := fmt.Sprintf("Gate: %s", step.Gate.Type)
	awaitID := gateAwaitID(step.Gate)
	if awaitID != "" {
		title = fmt.Sprintf("Gate: %s %s", step.Gate.Type, awaitID)
	}

	// Parse timeout if specified
	var timeout time.Duration
	if step.Gate.Timeout != "" {
		if parsed, err := time.ParseDuration(step.Gate.Timeout); err == nil {
			timeout = parsed
		}
	}

	gateIssue := &types.Issue{
		IssueID: types.IssueID{
			ID: gateID,
		},
		IssueContent: types.IssueContent{
			Title:       title,
			Description: fmt.Sprintf("Async gate for step %s", step.ID),
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  2,
			IssueType: "gate",
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		IssueWisp: types.IssueWisp{
			IsTemplate: true,
		},
		IssueCoord: types.IssueCoord{
			AwaitType: step.Gate.Type,
			AwaitID:   awaitID,
			Timeout:   timeout,
		},
	}

	// Propagate the formula-declared repo selector (SF2), matching the
	// declarative `metadata.repo` selector documented for ad-hoc gates.
	// Malformed values are left for check time (githubRepoFromIssue) to
	// reject, consistent with how a check-time-only value on any other
	// gate created outside `bd gate create` is validated.
	//
	// Restricted to gh:* gate types (SF4), same as repoMetadataForGate: a
	// `repo` field on a human/timer/bead gate step is unrelated, ordinary
	// metadata, not a GitHub repo selector, so only gh:run/gh:pr gates
	// write it here.
	if isGitHubGateType(step.Gate.Type) && step.Gate.Repo != "" {
		if metaJSON, err := json.Marshal(map[string]string{"repo": step.Gate.Repo}); err == nil {
			gateIssue.Metadata = metaJSON
		}
	}

	return gateIssue
}

func gateAwaitID(gate *formula.Gate) string {
	if gate == nil {
		return ""
	}
	if gate.AwaitID != "" {
		return gate.AwaitID
	}
	return gate.ID
}

// processStepToIssue converts a formula.Step to a types.Issue.
// The issue includes all fields including Labels populated from step.Labels and waits_for.
// This is the shared core logic used by both DB-persisted and in-memory cooking.
func processStepToIssue(step *formula.Step, parentID string) *types.Issue {
	// Generate issue ID (formula-name.step-id)
	issueID := fmt.Sprintf("%s.%s", parentID, step.ID)

	// Determine issue type. A parent step with no declared type defaults to
	// epic; a declared type is honored even when the step has children
	// (GH#5443).
	declaredType := strings.TrimSpace(step.Type)
	issueType := stepTypeToIssueType(declaredType)
	if len(step.Children) > 0 && declaredType == "" {
		issueType = types.TypeEpic
	}

	// Determine priority
	priority := 2
	if step.Priority != nil {
		priority = *step.Priority
	}

	issue := &types.Issue{
		IssueID: types.IssueID{
			ID: issueID,
		},
		IssueContent: types.IssueContent{
			Title: step.Title,
			// Keep {{variables}} for substitution at pour time
			Description: step.Description,
			Notes:       step.Notes,
		},
		IssueWorkflow: types.IssueWorkflow{
			Status:    types.StatusOpen,
			Priority:  priority,
			IssueType: issueType,
			Assignee:  step.Assignee,
		},
		IssueTimes: types.IssueTimes{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		IssueWisp: types.IssueWisp{
			IsTemplate: true,
		},
		IssueCoord: types.IssueCoord{
			SourceFormula: step.SourceFormula,
			// Source tracing
			SourceLocation: step.SourceLocation,
		},
		// Source tracing,
	}

	// Populate labels from step
	issue.Labels = append(issue.Labels, step.Labels...)

	// Add gate label for waits_for field
	if step.WaitsFor != "" {
		gateLabel := fmt.Sprintf("gate:%s", step.WaitsFor)
		issue.Labels = append(issue.Labels, gateLabel)
	}

	// Carry step metadata through to the issue (GH#3341).
	if len(step.Metadata) > 0 {
		if metaJSON, err := json.Marshal(step.Metadata); err == nil {
			issue.Metadata = metaJSON
		}
	}

	return issue
}

// collectSteps collects issues and dependencies for steps and their children.
// This is the unified implementation used by both DB-persisted and in-memory cooking.
//
// Parameters:
//   - idMapping: step.ID → issue.ID (always populated, used for dependency resolution)
//   - issueMap: issue.ID → issue (optional, nil for DB path, populated for in-memory path)
//   - labelHandler: callback for each label (if nil, labels stay on issue; if set, labels are
//     extracted and issue.Labels is cleared - use for DB path)
func collectSteps(steps []*formula.Step, parentID string,
	idMapping map[string]string,
	issueMap map[string]*types.Issue,
	issues *[]*types.Issue,
	deps *[]*types.Dependency,
	labelHandler func(issueID, label string)) {

	for _, step := range steps {
		collectStep(step, parentID, idMapping, issueMap, issues, deps, labelHandler)
	}
}

func collectStep(step *formula.Step, parentID string,
	idMapping map[string]string,
	issueMap map[string]*types.Issue,
	issues *[]*types.Issue,
	deps *[]*types.Dependency,
	labelHandler func(issueID, label string)) {
	issue := processStepToIssue(step, parentID)
	*issues = append(*issues, issue)
	idMapping[step.ID] = issue.ID
	if issueMap != nil {
		issueMap[issue.ID] = issue
	}
	extractCookLabels(issue, labelHandler)
	*deps = append(*deps, &types.Dependency{
		IssueID:     issue.ID,
		DependsOnID: parentID,
		Type:        types.DepParentChild,
	})
	if step.Gate != nil {
		collectGateIssue(step, parentID, issue, idMapping, issueMap, issues, deps, labelHandler)
	}
	if len(step.Children) > 0 {
		collectSteps(step.Children, issue.ID, idMapping, issueMap, issues, deps, labelHandler)
	}
}

func extractCookLabels(issue *types.Issue, labelHandler func(issueID, label string)) {
	if labelHandler == nil {
		return
	}
	for _, label := range issue.Labels {
		labelHandler(issue.ID, label)
	}
	issue.Labels = nil // DB stores labels separately
}

func collectGateIssue(step *formula.Step, parentID string, issue *types.Issue,
	idMapping map[string]string,
	issueMap map[string]*types.Issue,
	issues *[]*types.Issue,
	deps *[]*types.Dependency,
	labelHandler func(issueID, label string)) {
	gateIssue := createGateIssue(step, parentID)
	*issues = append(*issues, gateIssue)
	gateKey := fmt.Sprintf("gate-%s", step.ID)
	idMapping[gateKey] = gateIssue.ID
	if issueMap != nil {
		issueMap[gateIssue.ID] = gateIssue
	}
	extractCookLabels(gateIssue, labelHandler)
	*deps = append(*deps, &types.Dependency{
		IssueID:     gateIssue.ID,
		DependsOnID: parentID,
		Type:        types.DepParentChild,
	})
	*deps = append(*deps, &types.Dependency{
		IssueID:     issue.ID,
		DependsOnID: gateIssue.ID,
		Type:        types.DepBlocks,
	})
}

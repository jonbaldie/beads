package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/jonbaldie/beads/internal/utils"
)

// bd swarm status / validate in proxied-server mode (bd-gen49).
//
// Both commands are pure computed reads: the epic's child graph is loaded and
// analyzed in memory, nothing is written, so the whole body runs inside one
// read-only unit of work via uow.RunTxRead — no commit message, no Dolt
// commit, no commandDidWrite. The shared traversal and rendering code
// (analyzeEpicForSwarm, getSwarmStatus, renderSwarmAnalysis, renderSwarmStatus)
// is exactly the classic route's; the only proxied-specific piece is
// uowMolReader, the existing adapter that satisfies the SwarmStorage seam (and
// utils.PartialIDResolverStore) over the UOW use-cases.
//
// swarm create and swarm list stay gated: no downstream consumer, and their
// classic refusals in swarm.go are untouched.

// runSwarmValidateProxiedServer ports `bd swarm validate` to proxied-server mode.
func runSwarmValidateProxiedServer(cmd *cobra.Command, ctx context.Context, args []string) error {
	evt := metrics.NewCommandEvent("swarm-validate")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	verbose, _ := cmd.Flags().GetBool("verbose")
	if getUOWProvider() == nil {
		return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
	}
	analysis, err := uow.RunTxRead(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (*SwarmAnalysis, error) {
		return loadSwarmValidate(ctx, uw, args[0])
	})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	return printSwarmValidate(analysis, verbose)
}

func loadSwarmValidate(ctx context.Context, uw uow.UnitOfWork, rawID string) (*SwarmAnalysis, error) {
	r := uowMolReader{uw: uw}
	epicID, err := utils.ResolvePartialID(ctx, r, rawID)
	if err != nil {
		return nil, fmt.Errorf("epic '%s' not found: %v", rawID, err)
	}
	epic, err := uw.IssueUseCase().GetIssue(ctx, epicID)
	if gateProxiedNotFound(err) || (err == nil && epic == nil) {
		// Classic reports a nil epic after a successful resolve this way;
		// keep the message for parity.
		return nil, fmt.Errorf("epic '%s' not found", epicID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get epic: %v", err)
	}
	if epic.IssueType != types.TypeEpic && epic.IssueType != "molecule" {
		return nil, fmt.Errorf("'%s' is not an epic or molecule (type: %s)", epicID, epic.IssueType)
	}
	analysis, err := analyzeEpicForSwarm(ctx, r, epic)
	if err != nil {
		return nil, fmt.Errorf("failed to analyze epic: %v", err)
	}
	return analysis, nil
}

func printSwarmValidate(analysis *SwarmAnalysis, verbose bool) error {
	if !verbose {
		analysis.Issues = nil
	}
	if isJSONOutput() {
		if jerr := outputJSON(analysis); jerr != nil {
			return jerr
		}
	} else {
		renderSwarmAnalysis(analysis)
	}
	if !analysis.Swarmable {
		return SilentExit()
	}
	return nil
}

// runSwarmStatusProxiedServer ports `bd swarm status` to proxied-server mode.
func runSwarmStatusProxiedServer(_ *cobra.Command, ctx context.Context, args []string) error {
	evt := metrics.NewCommandEvent("swarm-status")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()
	if getUOWProvider() == nil {
		return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
	}
	status, err := uow.RunTxRead(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (*SwarmStatus, error) {
		return loadSwarmStatus(ctx, uw, args[0])
	})
	if err != nil {
		return HandleErrorRespectJSON("%v", err)
	}
	if isJSONOutput() {
		return outputJSON(status)
	}
	renderSwarmStatus(status)
	return nil
}

func loadSwarmStatus(ctx context.Context, uw uow.UnitOfWork, rawID string) (*SwarmStatus, error) {
	r := uowMolReader{uw: uw}
	issueID, err := utils.ResolvePartialID(ctx, r, rawID)
	if err != nil {
		return nil, fmt.Errorf("issue '%s' not found: %v", rawID, err)
	}
	issue, err := uw.IssueUseCase().GetIssue(ctx, issueID)
	if gateProxiedNotFound(err) || (err == nil && issue == nil) {
		return nil, fmt.Errorf("issue '%s' not found", issueID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get issue: %v", err)
	}
	epic, err := swarmStatusEpic(ctx, uw, r, issue, issueID)
	if err != nil {
		return nil, err
	}
	status, err := getSwarmStatus(ctx, r, epic)
	if err != nil {
		return nil, fmt.Errorf("failed to get swarm status: %v", err)
	}
	return status, nil
}

func swarmStatusEpic(ctx context.Context, uw uow.UnitOfWork, r uowMolReader, issue *types.Issue, issueID string) (*types.Issue, error) {
	if issue.IssueType == "molecule" && issue.MolType == types.MolTypeSwarm {
		return linkedSwarmEpic(ctx, uw, r, issue, issueID)
	}
	if issue.IssueType == types.TypeEpic || issue.IssueType == "molecule" {
		return issue, nil
	}
	return nil, fmt.Errorf("'%s' is not an epic or swarm molecule (type: %s)", issueID, issue.IssueType)
}

func linkedSwarmEpic(ctx context.Context, uw uow.UnitOfWork, r uowMolReader, issue *types.Issue, issueID string) (*types.Issue, error) {
	deps, err := r.GetDependencyRecords(ctx, issue.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get swarm dependencies: %v", err)
	}
	for _, dep := range deps {
		if dep.Type != types.DepRelatesTo {
			continue
		}
		epic, err := uw.IssueUseCase().GetIssue(ctx, dep.DependsOnID)
		if err != nil {
			return nil, fmt.Errorf("failed to get linked epic: %v", err)
		}
		return epic, nil
	}
	return nil, fmt.Errorf("swarm molecule '%s' has no linked epic", issueID)
}

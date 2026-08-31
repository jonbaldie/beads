package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

var migrateIssuesCmd = &cobra.Command{
	Use:   "issues",
	Short: "Move issues between repositories",
	Long: `Move issues from one source repository to another with filtering and dependency preservation.

This command updates the source_repo field for selected issues, allowing you to:
- Move contributor planning issues to upstream repository
- Reorganize issues across multi-phase repositories
- Consolidate issues from multiple repos

Examples:
  # Preview migration from planning repo to current repo
  bd migrate-issues --from ~/.beads-planning --to . --dry-run

  # Move all open P1 bugs
  bd migrate-issues --from ~/repo1 --to ~/repo2 --priority 1 --type bug --status open

  # Move specific issues with their dependencies
  bd migrate-issues --from . --to ~/archive --id bd-abc --id bd-xyz --include closure

  # Move issues with label filter
  bd migrate-issues --from . --to ~/feature-work --label frontend --label urgent`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		evt := metrics.NewCommandEvent("migrate-issues")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		dryRun, _ := cmd.Flags().GetBool("dry-run")

		if !dryRun {
			if err := CheckReadonly("migrate-issues"); err != nil {
				return err
			}
		}

		ctx := getRootContext()

		from, _ := cmd.Flags().GetString("from")
		to, _ := cmd.Flags().GetString("to")
		statusStr, _ := cmd.Flags().GetString("status")
		priorityInt, _ := cmd.Flags().GetInt("priority")
		typeStr, _ := cmd.Flags().GetString("type")
		labels, _ := cmd.Flags().GetStringSlice("label")
		ids, _ := cmd.Flags().GetStringSlice("id")
		idsFile, _ := cmd.Flags().GetString("ids-file")
		include, _ := cmd.Flags().GetString("include")
		withinFromOnly, _ := cmd.Flags().GetBool("within-from-only")
		strict, _ := cmd.Flags().GetBool("strict")
		yes, _ := cmd.Flags().GetBool("yes")

		if from == "" || to == "" {
			return HandleErrorRespectJSON("both --from and --to flags are required")
		}

		if from == to {
			return HandleErrorRespectJSON("--from and --to must be different repositories")
		}

		if idsFile != "" {
			fileIDs, err := loadIDsFromFile(idsFile)
			if err != nil {
				return HandleErrorRespectJSON("reading IDs file: %v", err)
			}
			ids = append(ids, fileIDs...)
		}

		if err := executeMigrateIssues(ctx, migrateIssuesParams{
			from:           from,
			to:             to,
			status:         statusStr,
			priority:       priorityInt,
			issueType:      typeStr,
			labels:         labels,
			ids:            ids,
			include:        include,
			withinFromOnly: withinFromOnly,
			dryRun:         dryRun,
			strict:         strict,
			yes:            yes,
		}); err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		return nil
	},
}

type migrateIssuesParams struct {
	from           string
	to             string
	status         string
	priority       int
	issueType      string
	labels         []string
	ids            []string
	include        string
	withinFromOnly bool
	dryRun         bool
	strict         bool
	yes            bool
}

type migrationPlan struct {
	TotalSelected     int      `json:"total_selected"`
	AddedByDependency int      `json:"added_by_dependency"`
	IncomingEdges     int      `json:"incoming_edges"`
	OutgoingEdges     int      `json:"outgoing_edges"`
	Orphans           int      `json:"orphans"`
	OrphanSamples     []string `json:"orphan_samples,omitempty"`
	IssueIDs          []string `json:"issue_ids"`
	From              string   `json:"from"`
	To                string   `json:"to"`
}

func executeMigrateIssues(ctx context.Context, p migrateIssuesParams) error {
	s := getStore() // use global Storage interface

	if err := validateRepos(ctx, s, p.from, p.to, p.strict); err != nil {
		return err
	}

	candidates, err := findCandidateIssues(ctx, s, p)
	if err != nil {
		return fmt.Errorf("failed to find candidate issues: %w", err)
	}
	if len(candidates) == 0 {
		return reportNoMigrationCandidates()
	}

	migrationSet, dependencyStats, err := expandMigrationSet(ctx, s, candidates, p)
	if err != nil {
		return fmt.Errorf("failed to compute migration set: %w", err)
	}

	orphans, err := checkOrphanedDependencies(ctx, s)
	if err != nil {
		return fmt.Errorf("failed to check dependencies: %w", err)
	}
	if err := validateMigrationOrphans(orphans, p.strict); err != nil {
		return err
	}

	plan := buildMigrationPlan(candidates, migrationSet, dependencyStats, orphans, p.from, p.to)
	if err := displayMigrationPlan(plan, p.dryRun); err != nil {
		return err
	}
	return applyMigrationPlan(ctx, s, migrationSet, plan, p)
}

func reportNoMigrationCandidates() error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"message": "No issues match the specified filters",
		})
	}
	fmt.Println("Nothing to do: no issues match the specified filters")
	return nil
}

func validateMigrationOrphans(orphans []string, strict bool) error {
	if len(orphans) > 0 && strict {
		return fmt.Errorf("strict mode: found %d orphaned dependencies", len(orphans))
	}
	return nil
}

func applyMigrationPlan(ctx context.Context, s storage.DoltStorage, migrationSet []string, plan migrationPlan, p migrateIssuesParams) error {
	if p.dryRun {
		return nil
	}
	if !p.yes && !isJSONOutput() && !confirmMigration(plan) {
		fmt.Println("Migration canceled")
		return nil
	}
	if err := executeMigration(ctx, s, migrationSet, p.to); err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	return reportMigrationSuccess(migrationSet, plan, p)
}

func reportMigrationSuccess(migrationSet []string, plan migrationPlan, p migrateIssuesParams) error {
	if isJSONOutput() {
		return outputJSON(map[string]interface{}{
			"success": true,
			"message": fmt.Sprintf("Migrated %d issues from %s to %s", len(migrationSet), p.from, p.to),
			"plan":    plan,
		})
	}
	fmt.Printf("\n✓ Successfully migrated %d issues from %s to %s\n", len(migrationSet), p.from, p.to)
	return nil
}

func validateRepos(ctx context.Context, s storage.DoltStorage, from, to string, strict bool) error {
	// migrate-issues is a round-trip path — opt out of BEADS_MAX_ROWS
	// (designer §4.1) so a misconfigured env doesn't abort migration.
	// Check if source repo has any issues
	fromIssues, err := s.SearchIssues(ctx, "", types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			Limit: 1,
		},
		IssueFilterFlags: types.IssueFilterFlags{
			SourceRepo: &from,
		},
		IssueFilterPage: types.IssueFilterPage{
			MaxRows:       0,
			MaxRowsSource: "",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to check source repository: %w", err)
	}

	if len(fromIssues) == 0 {
		msg := fmt.Sprintf("source repository '%s' has no issues", from)
		if strict {
			return fmt.Errorf("%s", msg)
		}
		if !isJSONOutput() {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", msg)
		}
	}

	// Check if destination repo exists (just a warning)
	toIssues, err := s.SearchIssues(ctx, "", types.IssueFilter{
		IssueFilterCore: types.IssueFilterCore{
			Limit: 1,
		},
		IssueFilterFlags: types.IssueFilterFlags{
			SourceRepo: &to,
		},
		IssueFilterPage: types.IssueFilterPage{
			MaxRows:       0,
			MaxRowsSource: "",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to check destination repository: %w", err)
	}

	if len(toIssues) == 0 && !isJSONOutput() {
		fmt.Fprintf(os.Stderr, "Info: destination repository '%s' will be created\n", to)
	}

	return nil
}

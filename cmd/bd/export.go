package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/atomicfile"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
	"github.com/spf13/cobra"
)

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export issues to JSONL format",
	Long: `Export all issues to JSONL (newline-delimited JSON) format.

Each line is a complete JSON object representing one issue, including its
labels, dependencies, and comments.

This command is for issue export, migration, and interoperability. It exports
records from the issues table; it is not a full database backup and does not
capture Dolt branches, commit history, working-set state, or non-issue tables.
For supported full backup/restore flows, use 'bd backup init', 'bd backup sync',
and 'bd backup restore'.

By default, exports only regular issues (excluding infrastructure beads
like agents, roles, and messages). Use --all to include everything.

Memories (from 'bd remember') are excluded by default because they may
contain sensitive agent context. Use --include-memories or --all to
include them.

EXAMPLES:
  bd export                              # Export issues to stdout
  bd export -o issues.jsonl              # Export issues to file
  bd export --include-memories           # Export issues + memories
  bd export --all -o full.jsonl          # Include infra + templates + gates + memories
  bd export --scrub -o clean.jsonl       # Exclude test/pollution records`,
	GroupID:       "sync",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runExport,
}

type exportOptions struct {
	output          string
	all             bool
	includeInfra    bool
	scrub           bool
	noMemories      bool
	includeMemories bool
	excludeOwners   []string
	verbose         bool
}

func exportOptionsFromCommand(cmd *cobra.Command) exportOptions {
	if cmd == nil {
		return exportOptions{}
	}
	flags := cmd.Flags()
	output, _ := flags.GetString("output")
	all, _ := flags.GetBool("all")
	includeInfra, _ := flags.GetBool("include-infra")
	scrub, _ := flags.GetBool("scrub")
	noMemories, _ := flags.GetBool("no-memories")
	includeMemories, _ := flags.GetBool("include-memories")
	excludeOwners, _ := flags.GetStringArray("exclude-owner")
	verbose, _ := flags.GetBool("verbose")
	return exportOptions{
		output:          output,
		all:             all,
		includeInfra:    includeInfra,
		scrub:           scrub,
		noMemories:      noMemories,
		includeMemories: includeMemories,
		excludeOwners:   excludeOwners,
		verbose:         verbose,
	}
}

func init() {
	exportCmd.Flags().StringP("output", "o", "", "Output file path (default: stdout)")
	exportCmd.Flags().Bool("all", false, "Include all records (infra, templates, gates, memories)")
	exportCmd.Flags().Bool("include-infra", false, "Include infrastructure beads (agents, roles, messages)")
	exportCmd.Flags().Bool("scrub", false, "Exclude test/pollution records")
	exportCmd.Flags().Bool("include-memories", false, "Include persistent memories (from 'bd remember') in the export")
	exportCmd.Flags().Bool("no-memories", false, "Exclude persistent memories (deprecated: now the default)")
	_ = exportCmd.Flags().MarkHidden("no-memories")
	exportCmd.Flags().StringArray("exclude-owner", nil, "Exclude issues created by this identity (repeatable; also reads export.exclude_owners config)")
	exportCmd.Flags().Bool("verbose", false, "Print filtered issue count when owners are excluded")
	rootCmd.AddCommand(exportCmd)
}

func runExport(cmd *cobra.Command, _ []string) error {
	evt := metrics.NewCommandEvent("export")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	return runExportWithOptions(getRootContext(), exportOptionsFromCommand(cmd))
}

func runExportWithOptions(ctx context.Context, opts exportOptions) error {
	if usesProxiedServer() {
		if getUOWProvider() == nil {
			return HandleErrorRespectJSON("proxied-server UOW provider not initialized")
		}
		// Run the ENTIRE read set inside one read transaction so the exported
		// issues, labels, dependencies, comments, and memories are a single
		// consistent snapshot. Export is read-only: RunTxRead never commits
		// (the attempt is always rolled back on close).
		_, err := uow.RunTxRead(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (struct{}, error) {
			return struct{}{}, runExportFromSource(ctx, &uowExportSource{uw: uw}, opts)
		})
		return err
	}

	return runExportFromSource(ctx, storeExportSource{}, opts)
}

// runExportFromSource is the whole classic export body with the storage reads
// routed through an exportSource. Everything downstream of the reads —
// filtering, sanitizeZeroTime, record shaping, marshal order, atomic file
// handling, the stderr summary — is shared verbatim between the embedded and
// proxied-server modes, which is what keeps the two outputs byte-identical.
func runExportFromSource(ctx context.Context, src exportSource, opts exportOptions) error {
	result, err := performExport(ctx, src, opts)
	if err != nil {
		return err
	}
	printExportSummary(opts, result.issueCount, result.memoryCount, result.filteredOwnerCount)
	return nil
}

type exportResult struct {
	issueCount         int
	memoryCount        int
	filteredOwnerCount int
}

func performExport(ctx context.Context, src exportSource, opts exportOptions) (exportResult, error) {
	w, aw, err := createExportWriter(opts.output)
	if err != nil {
		return exportResult{}, HandleErrorRespectJSON("failed to create output file: %v", err)
	}
	if aw != nil {
		defer func() { _ = aw.Abort() }()
	}

	result, err := writeExportData(ctx, src, opts, w)
	if err != nil {
		return exportResult{}, err
	}
	if err := closeExportWriter(aw); err != nil {
		return exportResult{}, err
	}
	return result, nil
}

func writeExportData(ctx context.Context, src exportSource, opts exportOptions, w io.Writer) (exportResult, error) {
	issues, filteredOwnerCount, err := loadExportIssues(ctx, src, opts)
	if err != nil {
		return exportResult{}, err
	}
	if len(issues) == 0 && opts.noMemories {
		if opts.output != "" {
			fmt.Fprintln(os.Stderr, "No issues to export.")
		}
		return exportResult{}, nil
	}

	rel, err := src.LoadExportRelations(ctx, issues)
	if err != nil {
		return exportResult{}, HandleErrorRespectJSON("failed to load relational data: %v", err)
	}
	wispPlane, err := classifyExportWispPlane(ctx, src, issues)
	if err != nil {
		return exportResult{}, HandleErrorRespectJSON("failed to classify wisp-plane rows: %v", err)
	}
	populateExportRelations(issues, rel)

	count, err := writeExportIssues(w, issues, rel, wispPlane)
	if err != nil {
		return exportResult{}, err
	}
	memoryCount, err := writeExportMemoriesIfEnabled(ctx, src, opts, w)
	if err != nil {
		return exportResult{}, err
	}
	return exportResult{issueCount: count, memoryCount: memoryCount, filteredOwnerCount: filteredOwnerCount}, nil
}

func closeExportWriter(aw *atomicfile.Writer) error {
	if aw == nil {
		return nil
	}
	if err := aw.Close(); err != nil {
		return HandleErrorRespectJSON("failed to finalize export file: %v", err)
	}
	return nil
}

func createExportWriter(output string) (io.Writer, *atomicfile.Writer, error) {
	if output == "" {
		return os.Stdout, nil, nil
	}
	aw, err := atomicfile.Create(output, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return aw, aw, nil
}

func loadExportIssues(ctx context.Context, src exportSource, opts exportOptions) ([]*types.Issue, int, error) {
	issues, err := src.SearchIssues(ctx, "", buildExportFilter(ctx, src, opts))
	if err != nil {
		return nil, 0, HandleErrorRespectJSON("failed to search issues: %v", err)
	}
	if opts.scrub {
		issues = filterOutPollution(issues)
	}

	ownerExcludes := buildOwnerExcludeSet(ctx, src, opts.excludeOwners)
	if len(ownerExcludes) == 0 {
		return issues, 0, nil
	}
	before := len(issues)
	filtered := filterOutOwners(issues, ownerExcludes)
	return filtered, before - len(filtered), nil
}

func buildExportFilter(ctx context.Context, src exportSource, opts exportOptions) types.IssueFilter {
	filter := types.IssueFilter{IssueFilterCore: types.IssueFilterCore{Limit: 0}, IssueFilterPage: types.IssueFilterPage{MaxRows: 0, MaxRowsSource: ""}}
	if !opts.all && !opts.includeInfra {
		for _, issueType := range exportInfraTypes(ctx, src) {
			filter.ExcludeTypes = append(filter.ExcludeTypes, types.IssueType(issueType))
		}
	}
	if !opts.all {
		isTemplate := false
		filter.IsTemplate = &isTemplate
		persistentOnly := false
		filter.Ephemeral = &persistentOnly
	}
	return filter
}

func exportInfraTypes(ctx context.Context, src exportSource) []string {
	infraSet := src.GetInfraTypes(ctx)
	infraTypes := make([]string, 0, len(infraSet))
	for issueType := range infraSet {
		infraTypes = append(infraTypes, issueType)
	}
	if len(infraTypes) == 0 {
		return domain.DefaultInfraTypes()
	}
	return infraTypes
}

func classifyExportWispPlane(ctx context.Context, src exportSource, issues []*types.Issue) (map[string]bool, error) {
	noHistoryIDs := make([]string, 0)
	for _, issue := range issues {
		if issue.NoHistory && !issue.Ephemeral {
			noHistoryIDs = append(noHistoryIDs, issue.ID)
		}
	}
	if len(noHistoryIDs) == 0 {
		return map[string]bool{}, nil
	}
	return src.WispPlaneIDs(ctx, noHistoryIDs)
}

func populateExportRelations(issues []*types.Issue, rel exportRelations) {
	for _, issue := range issues {
		issue.Labels = rel.labels[issue.ID]
		issue.Dependencies = rel.deps[issue.ID]
		issue.Comments = rel.comments[issue.ID]
	}
}

func writeExportIssues(w io.Writer, issues []*types.Issue, rel exportRelations, wispPlane map[string]bool) (int, error) {
	count := 0
	for _, issue := range issues {
		counts := rel.depCounts[issue.ID]
		if counts == nil {
			counts = &types.DependencyCounts{}
		}
		sanitizeZeroTime(issue)
		record := &exportIssueRecord{
			RecordType: "issue",
			IssueWithCounts: &types.IssueWithCounts{
				Issue:           issue,
				DependencyCount: counts.DependencyCount,
				DependentCount:  counts.DependentCount,
				CommentCount:    rel.commentCounts[issue.ID],
			},
			WispPlane: wispPlane[issue.ID],
		}
		data, err := json.Marshal(record)
		if err != nil {
			return count, HandleErrorRespectJSON("failed to marshal issue %s: %v", issue.ID, err)
		}
		if err := writeExportJSONLine(w, data); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func writeExportJSONLine(w io.Writer, data []byte) error {
	if _, err := w.Write(data); err != nil {
		return HandleErrorRespectJSON("failed to write: %v", err)
	}
	if _, err := w.Write([]byte{'\n'}); err != nil {
		return HandleErrorRespectJSON("failed to write newline: %v", err)
	}
	return nil
}

func exportMemoriesEnabled(opts exportOptions) bool {
	return (opts.includeMemories || opts.all) && !opts.noMemories
}

func writeExportMemoriesIfEnabled(ctx context.Context, src exportSource, opts exportOptions, w io.Writer) (int, error) {
	if !exportMemoriesEnabled(opts) {
		return 0, nil
	}
	return writeExportMemories(ctx, src, w)
}

func writeExportMemories(ctx context.Context, src exportSource, w io.Writer) (int, error) {
	allConfig, err := src.GetAllConfig(ctx)
	if err != nil {
		return 0, HandleErrorRespectJSON("failed to read config for memories: %v", err)
	}
	fullPrefix := kvPrefix + memoryPrefix
	memKeys := make([]string, 0)
	for key := range allConfig {
		if strings.HasPrefix(key, fullPrefix) {
			memKeys = append(memKeys, key)
		}
	}
	sort.Strings(memKeys)
	count := 0
	for _, key := range memKeys {
		userKey := strings.TrimPrefix(key, fullPrefix)
		record := map[string]string{"_type": "memory", "key": userKey, "value": allConfig[key]}
		data, err := json.Marshal(record)
		if err != nil {
			return count, HandleErrorRespectJSON("failed to marshal memory %s: %v", userKey, err)
		}
		if err := writeExportJSONLine(w, data); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func printExportSummary(opts exportOptions, issueCount, memoryCount, filteredOwnerCount int) {
	if opts.output == "" {
		return
	}
	if memoryCount > 0 {
		fmt.Fprintf(os.Stderr, "Exported %d issues and %d memories to %s\n", issueCount, memoryCount, opts.output)
	} else {
		fmt.Fprintf(os.Stderr, "Exported %d issues to %s\n", issueCount, opts.output)
	}
	if opts.verbose && filteredOwnerCount > 0 {
		fmt.Fprintf(os.Stderr, "  (%d filtered as personal by owner exclusion)\n", filteredOwnerCount)
	}
}

// exportIssueRecord wraps IssueWithCounts with a _type discriminator so that
// every line in the JSONL export is self-describing. Memory lines already
// carry "_type":"memory"; this gives issue lines "_type":"issue". (GH#3271)
type exportIssueRecord struct {
	RecordType string `json:"_type"`
	*types.IssueWithCounts
	// WispPlane is the explicit wisps-plane marker (bd-r9uce): true when the
	// row lives in the WISPS table AND its flags alone cannot prove it (the
	// no_history shape; ephemeral rows are self-describing and stay
	// unstamped). Import routes by this marker — never by no_history — so a
	// promoted no-history wisp (durable issues-table row still carrying the
	// stray flag) round-trips to the durable plane instead of being silently
	// re-planed. Declared after the embedded struct so it serializes last.
	// Deliberately a FRESH key, not the legacy "wisp" alias key: pre-fix
	// binaries' alias branch would import a marked no-history wisp as
	// ephemeral (purge-eligible, export-excluded), so an unknown-to-them key
	// that degrades to flag routing is the data-safe choice (lion, #5368).
	WispPlane bool `json:"wisp_plane,omitempty"`
}

// sanitizeZeroTime replaces Go zero-value time.Time fields with Unix epoch.
// NULL datetime columns in Dolt scan as time.Time{} (year 0001-01-01), which
// causes json.Marshal to fail with "year outside of range [0,9999]". (GH#2488)
func sanitizeZeroTime(issue *types.Issue) {
	epoch := time.Unix(0, 0).UTC()
	if issue.CreatedAt.IsZero() {
		issue.CreatedAt = epoch
	}
	if issue.UpdatedAt.IsZero() {
		issue.UpdatedAt = epoch
	}
}

// filterOutPollution removes issues that look like test/pollution records.
func filterOutPollution(issues []*types.Issue) []*types.Issue {
	var clean []*types.Issue
	for _, issue := range issues {
		if !isTestIssue(issue.Title) {
			clean = append(clean, issue)
		}
	}
	return clean
}

// buildOwnerExcludeSet merges --exclude-owner flag values with the
// export.exclude_owners (and legacy export.exclude_owner) config entries.
// Returns the combined set as a map for O(1) lookup.
func buildOwnerExcludeSet(ctx context.Context, src exportSource, flagOwners []string) map[string]struct{} {
	set := make(map[string]struct{})
	addFlagOwners(set, flagOwners)
	// export.* keys are YAML-only (config.IsYamlOnlyKey returns true for the
	// "export." prefix), so bd config set stores them in config.yaml rather than
	// the database. Read from YAML first, then fall back to the database for any
	// instance that was written directly to the store.
	addYAMLExcludeOwners(set)
	// Also read from database for any value stored there directly.
	addDatabaseExcludeOwners(ctx, src, set)
	return set
}

func addFlagOwners(set map[string]struct{}, owners []string) {
	for _, owner := range owners {
		if owner != "" {
			set[owner] = struct{}{}
		}
	}
}

func addYAMLExcludeOwners(set map[string]struct{}) {
	if val := config.GetYamlConfig("export.exclude_owners"); val != "" {
		addOwnerList(set, val)
	}
	if val := config.GetYamlConfig("export.exclude_owner"); val != "" {
		set[strings.TrimSpace(val)] = struct{}{}
	}
}

func addDatabaseExcludeOwners(ctx context.Context, src exportSource, set map[string]struct{}) {
	if val, err := src.GetConfig(ctx, "export.exclude_owners"); err == nil && val != "" {
		addOwnerList(set, val)
	}
	if val, err := src.GetConfig(ctx, "export.exclude_owner"); err == nil && val != "" {
		set[strings.TrimSpace(val)] = struct{}{}
	}
}

func addOwnerList(set map[string]struct{}, value string) {
	for _, owner := range strings.Split(value, ",") {
		if owner = strings.TrimSpace(owner); owner != "" {
			set[owner] = struct{}{}
		}
	}
}

// filterOutOwners removes issues whose created_by identity is in the exclude set.
func filterOutOwners(issues []*types.Issue, exclude map[string]struct{}) []*types.Issue {
	var keep []*types.Issue
	for _, issue := range issues {
		if _, excluded := exclude[issue.CreatedBy]; !excluded {
			keep = append(keep, issue)
		}
	}
	return keep
}

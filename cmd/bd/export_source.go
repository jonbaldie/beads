package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/types"
)

// exportSource abstracts the storage reads `bd export` performs, so the one
// export body (runExportFromSource) serves both storage stacks:
//
//   - storeExportSource reads through the classic `store` global
//     (embedded / direct / server modes), preserving the exact pre-seam
//     call pattern.
//   - uowExportSource reads through a proxied-server unit of work, whose
//     domain use cases are plane-pinned (issues vs wisps tables) where the
//     classic bulk loaders partition internally — the impl queries both
//     planes with the full ID set and merges, so both modes read the same
//     rows regardless of which table an issue lives in.
//
// Everything downstream of these reads is shared code; the acceptance bar for
// the seam is byte-identical JSONL output across modes.
type exportSource interface {
	// GetInfraTypes returns the resolved infra-type set (config, YAML, or
	// hardcoded defaults). A nil/empty result makes the caller fall back to
	// domain.DefaultInfraTypes(), mirroring the pre-seam behavior.
	GetInfraTypes(ctx context.Context) map[string]bool
	// SearchIssues returns the full, untruncated result set for the export
	// filter (Limit=0, MaxRows=0). Implementations MUST fail rather than
	// return a truncated page — export is a data-integrity path.
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error)
	// GetConfig reads one config key from the database (used for the
	// export.exclude_owners owner-filter keys).
	GetConfig(ctx context.Context, key string) (string, error)
	// GetAllConfig reads the whole config table (used to extract kv.memory.*
	// rows when memories are exported).
	GetAllConfig(ctx context.Context) (map[string]string, error)
	// LoadExportRelations bulk-loads labels, dependency records, comments,
	// comment counts, and dependency counts for the searched issues.
	LoadExportRelations(ctx context.Context, issues []*types.Issue) (exportRelations, error)
	// WispPlaneIDs reports which of ids currently live in the WISPS table.
	// Export uses it to stamp the explicit "wisp_plane" marker on records
	// whose row flags are ambiguous: a no_history=true row is either an
	// unpromoted no-history wisp (wisps table) or a promoted one (durable
	// issues-table row that may still carry the stray flag), and only table
	// membership can tell them apart — import routes by the marker, so
	// mis-stamping a durable row would re-plane it and drop its relations
	// (bd-r9uce). Implementations must classify by table membership, never
	// by flags, and both modes must agree (byte-identity oracle).
	WispPlaneIDs(ctx context.Context, ids []string) (map[string]bool, error)
}

// storeWispPartitioner is the optional store capability WispPlaneIDs uses in
// classic mode. Both real stores (DoltStore, EmbeddedDoltStore) implement it;
// a store that does not simply gets no plane markers, which degrades to the
// data-safe side (import then routes bare no_history rows to the durable
// plane, where nothing is ever excluded from export).
type storeWispPartitioner interface {
	PartitionWispIDs(ctx context.Context, ids []string) (wispIDs, permIDs []string, err error)
}

// exportRelations carries the bulk-loaded relational data for the export set,
// keyed by issue ID.
type exportRelations struct {
	labels        map[string][]string
	deps          map[string][]*types.Dependency
	comments      map[string][]*types.Comment
	commentCounts map[string]int
	depCounts     map[string]*types.DependencyCounts
}

// errExportNoStore reports a classic-source read attempted with no store
// open. Callers of exportSource.GetConfig treat any error as "key unset",
// which reproduces the pre-seam `if store == nil` early return.
var errExportNoStore = errors.New("no store available")

// storeExportSource is the classic-mode exportSource over the `store` global.
type storeExportSource struct{}

func (storeExportSource) GetInfraTypes(ctx context.Context) map[string]bool {
	if getStore() == nil {
		return nil
	}
	return getStore().GetInfraTypes(ctx)
}

func (storeExportSource) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	return getStore().SearchIssues(ctx, query, filter)
}

func (storeExportSource) GetConfig(ctx context.Context, key string) (string, error) {
	if getStore() == nil {
		return "", errExportNoStore
	}
	return getStore().GetConfig(ctx, key)
}

func (storeExportSource) GetAllConfig(ctx context.Context) (map[string]string, error) {
	return getStore().GetAllConfig(ctx)
}

func (storeExportSource) LoadExportRelations(ctx context.Context, issues []*types.Issue) (exportRelations, error) {
	issueIDs := make([]string, len(issues))
	for i, issue := range issues {
		issueIDs[i] = issue.ID
	}

	// Individual bulk-load failures deliberately degrade to empty maps rather
	// than aborting the export — unchanged from the pre-seam classic behavior.
	labelsMap, _ := getStore().GetLabelsForIssues(ctx, issueIDs)
	allDeps, _ := getStore().GetDependencyRecordsForIssues(ctx, issueIDs)
	commentsMap, _ := getStore().GetCommentsForIssues(ctx, issueIDs)
	commentCounts, _ := getStore().GetCommentCounts(ctx, issueIDs)
	depCounts, _ := getStore().GetDependencyCounts(ctx, issueIDs)

	return exportRelations{
		labels:        labelsMap,
		deps:          allDeps,
		comments:      commentsMap,
		commentCounts: commentCounts,
		depCounts:     depCounts,
	}, nil
}

func (storeExportSource) WispPlaneIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	if len(ids) == 0 || getStore() == nil {
		return nil, nil
	}
	// The store global is decorator-wrapped (telemetry, hook firing); walk
	// Unwrap() down to the store that carries the partition capability.
	s := getStore()
	var p storeWispPartitioner
	for {
		if partitioner, ok := s.(storeWispPartitioner); ok {
			p = partitioner
			break
		}
		u, ok := s.(interface{ Unwrap() storage.DoltStorage })
		if !ok {
			return nil, nil
		}
		s = u.Unwrap()
		if s == nil {
			return nil, nil
		}
	}
	wispIDs, _, err := p.PartitionWispIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(wispIDs))
	for _, id := range wispIDs {
		set[id] = true
	}
	return set, nil
}

// uowExportSource is the proxied-server exportSource over a unit of work. All
// reads run inside the single read transaction runExport opened, so the
// export is one consistent snapshot.
type uowExportSource struct {
	uw uow.UnitOfWork
}

func (s *uowExportSource) GetInfraTypes(ctx context.Context) map[string]bool {
	infra, err := s.uw.ConfigUseCase().GetInfraTypes(ctx)
	if err != nil {
		// Match the classic source's error-free signature: an unreadable
		// config degrades to the caller's DefaultInfraTypes() fallback.
		return nil
	}
	return infra
}

func (s *uowExportSource) SearchIssues(ctx context.Context, query string, filter types.IssueFilter) ([]*types.Issue, error) {
	page, err := s.uw.IssueUseCase().SearchIssues(ctx, query, filter)
	if err != nil {
		return nil, err
	}
	// Export builds its filter with Limit=0/MaxRows=0, so the whole result
	// arrives as one page and HasMore can only be true if a limit creeps back
	// in. Guard anyway: an export must fail loudly, never truncate silently
	// (domain.SearchPage carries no continuation cursor to loop with).
	if page.HasMore {
		return nil, fmt.Errorf("issue search returned a truncated page (limit=%d); export must never truncate", filter.Limit)
	}
	return page.Items, nil
}

func (s *uowExportSource) GetConfig(ctx context.Context, key string) (string, error) {
	return s.uw.ConfigUseCase().GetConfig(ctx, key)
}

func (s *uowExportSource) GetAllConfig(ctx context.Context) (map[string]string, error) {
	return s.uw.ConfigUseCase().GetAllConfig(ctx)
}

// LoadExportRelations reproduces the classic bulk loaders over the
// plane-pinned domain use cases:
//
//   - Labels, comments, and comment counts: the classic loaders partition IDs
//     by wisps-table membership (issueops.PartitionWispIDsInTx). Row flags are
//     NOT a substitute for that partition — a promoted no-history wisp is a
//     durable issues-table row that (as wild data from the pre-bd-r9uce
//     promote) still carries NoHistory=true — so both
//     planes are queried with the FULL ID set and merged. The planes are
//     ID-disjoint (GH#4455 cross-table guard), so at most one plane returns
//     rows for any id and the merge cannot collide.
//   - Dependency records: domain GetIssueDependencyRecords already partitions
//     internally (same issueops helper), so one call covers both planes.
//   - Dependency counts: classic GetDependencyCountsInTx queries BOTH dep
//     tables for EVERY id and sums (a wisp blocking a durable issue
//     contributes to the durable issue's dependent_count), so both planes are
//     queried for the full ID set and summed here too.
//
// Wisp-plane reads tolerate a rig without the wisp tables (IsTableNotExist),
// mirroring the dependency-counts leg.
func (s *uowExportSource) LoadExportRelations(ctx context.Context, issues []*types.Issue) (exportRelations, error) {
	rel := exportRelations{
		labels:        map[string][]string{},
		deps:          map[string][]*types.Dependency{},
		comments:      map[string][]*types.Comment{},
		commentCounts: map[string]int{},
		depCounts:     map[string]*types.DependencyCounts{},
	}
	allIDs := make([]string, 0, len(issues))
	for _, issue := range issues {
		allIDs = append(allIDs, issue.ID)
	}
	if len(allIDs) == 0 {
		return rel, nil
	}
	if err := s.loadExportIssueRelations(ctx, allIDs, &rel); err != nil {
		return exportRelations{}, err
	}
	if err := s.loadExportWispRelations(ctx, allIDs, &rel); err != nil {
		return exportRelations{}, err
	}
	return s.loadExportDepRelations(ctx, allIDs, rel)
}

func (s *uowExportSource) loadExportIssueRelations(ctx context.Context, allIDs []string, rel *exportRelations) error {
	// BATCH READS, and the batch is what keeps them here. Export hydrates every
	// relation of N issues at once — labels, comments, dependencies, counts,
	// both planes — and no role answers "the labels of these N ids": Reader.Get
	// is one detail view per call, so a role-routed export would be N round
	// trips over a whole workspace. A bulk relation reader is the follow-up
	// (ga-2ltro.12). Every LabelUseCase call in this file is one of these.
	labels, err := s.uw.LabelUseCase().GetLabelsForIssues(ctx, allIDs) //nolint:forbidigo // bulk relation load; no role answers labels-for-N-ids
	if err != nil {
		return fmt.Errorf("load labels: %w", err)
	}
	mergeExportMap(rel.labels, labels)
	comments, err := s.uw.CommentUseCase().GetCommentsForIssues(ctx, allIDs)
	if err != nil {
		return fmt.Errorf("load comments: %w", err)
	}
	mergeExportMap(rel.comments, comments)
	counts, err := s.uw.CommentUseCase().GetCommentCounts(ctx, allIDs)
	if err != nil {
		return fmt.Errorf("load comment counts: %w", err)
	}
	mergeExportMap(rel.commentCounts, counts)
	return nil
}

func (s *uowExportSource) loadExportWispRelations(ctx context.Context, allIDs []string, rel *exportRelations) error {
	wispLabels, err := s.uw.LabelUseCase().GetLabelsForWisps(ctx, allIDs) //nolint:forbidigo // bulk relation load; see GetLabelsForIssues above
	if err := mergeOptionalExportMap(rel.labels, wispLabels, err, "load wisp labels"); err != nil {
		return err
	}
	wispComments, err := s.uw.CommentUseCase().GetCommentsForWisps(ctx, allIDs)
	if err := mergeOptionalExportMap(rel.comments, wispComments, err, "load wisp comments"); err != nil {
		return err
	}
	wispCounts, err := s.uw.CommentUseCase().GetWispCommentCounts(ctx, allIDs)
	return mergeOptionalExportMap(rel.commentCounts, wispCounts, err, "load wisp comment counts")
}

func (s *uowExportSource) loadExportDepRelations(ctx context.Context, allIDs []string, rel exportRelations) (exportRelations, error) {
	deps, err := s.uw.DependencyUseCase().GetIssueDependencyRecords(ctx, allIDs)
	if err != nil {
		return exportRelations{}, fmt.Errorf("load dependency records: %w", err)
	}
	rel.deps = deps
	depCounts, err := s.uw.DependencyUseCase().CountsByIssueIDs(ctx, allIDs)
	if err != nil {
		return exportRelations{}, fmt.Errorf("load dependency counts: %w", err)
	}
	rel.depCounts = depCounts
	wispDepCounts, err := s.uw.DependencyUseCase().CountsByWispIDs(ctx, allIDs)
	if err != nil {
		// A rig without the wisp tables simply has no wisp-plane edges.
		if !dberrors.IsTableNotExist(err) {
			return exportRelations{}, fmt.Errorf("load wisp dependency counts: %w", err)
		}
		return rel, nil
	}
	mergeWispDepCounts(rel.depCounts, wispDepCounts)
	return rel, nil
}

func mergeOptionalExportMap[V any](dst, src map[string]V, err error, wrap string) error {
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return nil
		}
		return fmt.Errorf("%s: %w", wrap, err)
	}
	mergeExportMap(dst, src)
	return nil
}

func mergeWispDepCounts(dst, src map[string]*types.DependencyCounts) {
	for id, wc := range src {
		if wc == nil || (wc.DependencyCount == 0 && wc.DependentCount == 0) {
			continue
		}
		c := dst[id]
		if c == nil {
			c = &types.DependencyCounts{}
			dst[id] = c
		}
		c.DependencyCount += wc.DependencyCount
		c.DependentCount += wc.DependentCount
	}
}

// WispPlaneIDs classifies by wisps-table membership through the plane-pinned
// GetWispsByIDs read (row flags are NOT a substitute — see the exportSource
// interface comment). A rig without the wisp tables has no wisp-plane rows,
// mirroring the tolerance of the wisp-plane legs in LoadExportRelations.
func (s *uowExportSource) WispPlaneIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	wisps, err := s.uw.IssueUseCase().GetWispsByIDs(ctx, ids)
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("wisp plane membership: %w", err)
	}
	set := make(map[string]bool, len(wisps))
	for _, w := range wisps {
		set[w.ID] = true
	}
	return set, nil
}

// mergeExportMap copies src entries into dst. The label/comment planes are
// disjoint by ID (an issue lives in exactly one of issues/wisps), so a plain
// overwrite-merge reassembles the classic partitioned loaders' single map.
func mergeExportMap[V any](dst, src map[string]V) {
	for k, v := range src {
		dst[k] = v
	}
}

package domain

import (
	"context"
	"time"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
	publicops "github.com/jonbaldie/beads/issueops"
)

type InsertIssueOpts struct {
	UseWispsTable bool
	CreateOnly    bool
}

type IssueTableOpts struct {
	UseWispsTable bool
}

type ClaimRowResult struct {
	Updated bool
	// CurrentAssignee is the assignee found on the 0-row disambiguation
	// read; CurrentAssigneeIsPool marks it as a claim.pools alias, so the
	// caller reports a status conflict instead of a held-by-someone
	// refusal (a pool alias never "holds" a claim).
	CurrentAssignee       string
	CurrentAssigneeIsPool bool
	CurrentStatus         types.Status
	StartedAtWasZero      bool
	OldIssue              *types.Issue
}

type IssueSQLRepository interface {
	Insert(ctx context.Context, issue *types.Issue, actor string, opts InsertIssueOpts) error
	InsertBatch(ctx context.Context, issues []*types.Issue, actor string, opts InsertIssueOpts) error
	MovePersistence(ctx context.Context, id string, mode types.PersistenceMode) (changed bool, err error)
	PromoteFromEphemeral(ctx context.Context, id, actor string) error
	Update(ctx context.Context, id string, updates map[string]any, actor string, opts IssueTableOpts) error
	// CompareAndSetMetadataKey runs the SHARED compare-and-set body on this
	// repository's transaction, which is how the unit-of-work provider reaches
	// the same function the two store backends wrap. It takes no table option:
	// the body routes both planes itself, the way the metadata merge does.
	//
	// The bool reports whether a row change actually LANDED, which the caller
	// needs and the result does not carry — a swap can hold its precondition
	// and write nothing (see issueops.MetadataCAS.CompareAndSetKey).
	CompareAndSetMetadataKey(ctx context.Context, plan storage.CompareAndSetKeyPlan) (publicops.CompareAndSetKeyResult, bool, error)
	// ReleaseIssue runs the SHARED claim-release body on this repository's
	// transaction, which is how the unit-of-work provider reaches the same
	// function the two store backends wrap. It takes no table option: the body
	// routes both planes itself, the way the compare-and-set above does.
	//
	// The bool reports whether a row write actually LANDED, which the caller
	// needs and the result does not carry in the shape it needs it: an
	// ephemeral release writes a row and versions nothing, and a caller whose
	// commit message is also what commits its SQL transaction must compose one
	// either way (see issueops.Releaser.Release).
	ReleaseIssue(ctx context.Context, req publicops.ReleaseRequest) (publicops.ReleaseResult, bool, error)
	Claim(ctx context.Context, id, actor string, opts IssueTableOpts) (ClaimRowResult, error)
	Get(ctx context.Context, id string, opts IssueTableOpts) (*types.Issue, error)
	AsOf(ctx context.Context, id, ref string) (*types.Issue, error)
	GetByIDs(ctx context.Context, ids []string, opts IssueTableOpts) ([]*types.Issue, error)
	Exists(ctx context.Context, id string, opts IssueTableOpts) (bool, error)
	CountForPrefix(ctx context.Context, prefix string, opts IssueTableOpts) (int, error)
	NextCounterID(ctx context.Context, prefix string) (int, error)
	SearchAcrossIssuesAndWisps(ctx context.Context, query string, filter types.IssueFilter) (SearchPage, error)
	SearchAcrossIssuesAndWispsWithCounts(ctx context.Context, query string, filter types.IssueFilter) (SearchCountsPage, error)
	SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error)
	GetReadyWork(ctx context.Context, filter types.WorkFilter) (SearchPage, error)
	GetReadyWorkWithCounts(ctx context.Context, filter types.WorkFilter) (SearchCountsPage, error)
	GetDescendants(ctx context.Context, rootID string, filter types.IssueFilter) ([]*types.Issue, error)
	Delete(ctx context.Context, id string, opts IssueTableOpts) error
	DeleteByIDs(ctx context.Context, ids []string, opts IssueTableOpts) (int, error)
	PartitionWispIDs(ctx context.Context, ids []string) (wispIDs, regularIDs []string, err error)
	FindAllDependents(ctx context.Context, ids []string) ([]string, error)
	FindWispDependentsRecursive(ctx context.Context, ids []string) (map[string]bool, error)
	AffectedByDeletion(ctx context.Context, issueIDs, wispIDs []string) (affectedIssues, affectedWisps []string, err error)
	RecomputeIsBlocked(ctx context.Context, issueIDs, wispIDs []string) error
	Close(ctx context.Context, id string, params CloseRowParams, actor string, opts IssueTableOpts) (CloseRowResult, error)
	CloseChecked(ctx context.Context, id string, params CloseRowParams, actor string, force bool) (CloseRowResult, error)
	Reopen(ctx context.Context, id string, params ReopenRowParams, actor string, opts IssueTableOpts) (ReopenRowResult, error)
	GetNewlyUnblockedByClose(ctx context.Context, closedID string) ([]*types.Issue, error)
	ClaimReadyIssue(ctx context.Context, filter types.WorkFilter, actor string) (*types.Issue, error)
	ClaimReadyWisp(ctx context.Context, filter types.WorkFilter, actor string) (*types.Issue, error)
	GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error)
	GetStatistics(ctx context.Context) (*types.Statistics, error)
	CountIssues(ctx context.Context, query string, filter types.IssueFilter) (int64, error)
	CountIssuesByGroup(ctx context.Context, filter types.IssueFilter, groupBy string) (map[string]int, error)
	History(ctx context.Context, id string) ([]*storage.HistoryEntry, error)
	IterEvents(ctx context.Context, id string, limit int) (storage.Iter[types.Event], error)
	GetStaleIssues(ctx context.Context, filter types.StaleFilter) ([]*types.Issue, error)
	GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error)
	UnclaimIssue(ctx context.Context, id, actor string, force bool) error
	UnclaimIssueIfAssignee(ctx context.Context, id, actor, expectedAssignee string) error
	HeartbeatIssue(ctx context.Context, id, actor string) error
	ReclaimExpiredLeases(ctx context.Context, olderThan time.Duration, filter types.ReclaimFilter, actor string) ([]types.ReclaimedLease, error)
	WakeExpiredDefers(ctx context.Context) (issues, wisps int, err error)
}

type CloseRowParams struct {
	Reason  string
	Session string
}

type CloseRowResult struct {
	Updated       bool
	AlreadyClosed bool
	IsWisp        bool
	OpenChildren  int
}

type ReopenRowParams struct {
	Reason string
}

type ReopenRowResult struct {
	Updated     bool
	AlreadyOpen bool
	IsWisp      bool
}

type DeleteIssuesParams struct {
	IDs                  []string
	Cascade              bool
	DryRun               bool
	UpdateTextReferences bool

	// EnforceCascadePolicy selects embedded-parity dependent handling
	// (issueops.DeleteIssuesInTx). When false — the legacy default kept for the
	// wisp/mol/gc/purge proxied paths and the single-ID convenience wrappers —
	// cascade expansion follows params.Cascade alone and Force is ignored. When
	// true, Cascade/Force choose the behavior:
	//   Cascade=true               → delete all transitive dependents
	//   Cascade=false, Force=false → refuse if any external dependent exists
	//                                (*DeleteBlockedError, naming the blockers)
	//   Cascade=false, Force=true  → orphan external dependents (delete only IDs)
	// One deliberate divergence from embedded: a wisp NAMED in IDs counts like
	// any other issue here and can trip the refusal, where embedded partitions
	// wisps out before the dependent check. Strictly safer; kept on purpose.
	EnforceCascadePolicy bool
	Force                bool
}

type DeleteIssuesResult struct {
	DeletedCount      int
	DependenciesCount int
	LabelsCount       int
	EventsCount       int
	ReferencesUpdated int
	// OrphanedIssues is NOT POPULATED BY ANY PATH TODAY, and is kept only
	// because `bd wisp gc --json` publishes it (always null) and this commit is
	// not changing that command's wire shape.
	//
	// It once described a force-delete policy this layer never implemented: the
	// two params that were supposed to select that policy were declared,
	// documented in detail and never read. The policy now lives in
	// issueops.Deleter, above the use case, and DeleteResult.Orphaned is where
	// the answer comes out.
	OrphanedIssues []string
}

type DeletePreview struct {
	Issues          map[string]*types.Issue
	ConnectedIssues map[string]*types.Issue
	DepRecords      map[string][]*types.Dependency
	NotFound        []string
}

type SearchPage struct {
	Items   []*types.Issue
	HasMore bool
}

type SearchCountsPage struct {
	Items   []*types.IssueWithCounts
	HasMore bool
}

type CreateIssueParams struct {
	Issue                   *types.Issue
	ExplicitID              string
	ParentID                string
	Labels                  []string
	InheritLabelsFromParent bool
	Dependencies            []DependencySpec
	WaitsFor                *WaitsForSpec
	DiscoveredFromParent    string
	ForcePrefix             bool
	CreateOnly              bool
	Comments                []*types.Comment
}

type DependencySpec struct {
	Type          types.DependencyType
	TargetID      string
	SwapDirection bool
	Metadata      string
	ThreadID      string
}

type WaitsForSpec struct {
	SpawnerID string
	Gate      string
}

type CreateIssueResult struct {
	Issue            *types.Issue
	InheritedLabels  []string
	PostCreateWrites bool
}

type CreateIssuesResult struct {
	Issues []*types.Issue
}

type GraphPlan struct {
	Nodes []GraphNode
	Edges []GraphEdge
}

type GraphNode struct {
	Key               string
	Issue             *types.Issue
	ParentKey         string
	ParentID          string
	Assignee          string
	AssignAfterCreate bool
	MetadataRefs      map[string]string
	Labels            []string
	Deps              []GraphNodeDep
}

// GraphNodeDep is an inline dependency on a single graph node. Target is
// resolved as a plan-local key first, then treated as a literal issue ID.
type GraphNodeDep struct {
	Type   types.DependencyType
	Target string
}

type GraphEdge struct {
	FromKey string
	FromID  string
	ToKey   string
	ToID    string
	Type    types.DependencyType
	// Gate and spawner describe fanout-gate metadata for waits-for edges;
	// SpawnerKey references a plan-local node and is resolved after IDs mint.
	Gate       string
	SpawnerKey string
	SpawnerID  string
	// ThreadID threads conversation edges (replies-to).
	ThreadID string
}

type GraphApplyResult struct {
	IDs map[string]string
}

type ClaimResult struct {
	AlreadyClaimed bool
	PriorAssignee  string
}

type ClaimReadyResult struct {
	Issue   *types.Issue
	Claimed bool
}

type UpdateSpec struct {
	Fields map[string]any
	Claim  bool
	// ExpectedVersion requires the current row version to match before any
	// claim or field writes.
	ExpectedVersion *int64
	Persistence     *types.PersistenceMode
	AddLabels       []string
	RemoveLabels    []string
	SetLabels       *[]string
	Reparent        *string

	// ExpectedAssignee and ExpectedStatus are the bd-wsqvw compare-and-set
	// guards (`bd update --if-assignee/--if-status`): when non-nil, the whole
	// update applies only if the issue's current assignee/status equals the
	// expected value, else ApplyUpdate refuses with
	// storage.ErrAssigneeMismatch/ErrStatusMismatch and nothing is written. A
	// non-nil pointer to "" means "expected unassigned". The guard read shares
	// the unit of work's transaction; a writer that commits mid-flight
	// collides on the row_lock rewrite at commit time and the caller's
	// whole-attempt retry re-checks the guards on the redo.
	ExpectedAssignee *string
	ExpectedStatus   *string
}

type IssueUseCase interface {
	GetIssue(ctx context.Context, id string) (*types.Issue, error)
	GetIssuesByIDs(ctx context.Context, ids []string) ([]*types.Issue, error)
	FindWispDependentsRecursive(ctx context.Context, ids []string) (map[string]bool, error)
	SearchIssues(ctx context.Context, query string, filter types.IssueFilter) (SearchPage, error)
	SearchIssuesWithCounts(ctx context.Context, query string, filter types.IssueFilter) (SearchCountsPage, error)
	SearchIssueIDs(ctx context.Context, query string, filter types.IssueFilter) ([]string, error)
	GetReadyWork(ctx context.Context, filter types.WorkFilter) (SearchPage, error)
	GetReadyWorkWithCounts(ctx context.Context, filter types.WorkFilter) (SearchCountsPage, error)
	GetDescendants(ctx context.Context, rootID string, filter types.IssueFilter) ([]*types.Issue, error)
	ClaimReadyIssue(ctx context.Context, filter types.WorkFilter, actor string) (ClaimReadyResult, error)
	GetBlockedIssues(ctx context.Context, filter types.WorkFilter) ([]*types.BlockedIssue, error)
	GetStatistics(ctx context.Context) (*types.Statistics, error)
	CountIssues(ctx context.Context, query string, filter types.IssueFilter) (int64, error)
	CountIssuesByGroup(ctx context.Context, filter types.IssueFilter, groupBy string) (map[string]int, error)
	History(ctx context.Context, id string) ([]*storage.HistoryEntry, error)
	IterEvents(ctx context.Context, id string, limit int) (storage.Iter[types.Event], error)
	GetStaleIssues(ctx context.Context, filter types.StaleFilter) ([]*types.Issue, error)
	GetEpicsEligibleForClosure(ctx context.Context) ([]*types.EpicStatus, error)
	Unclaim(ctx context.Context, id, actor string, force bool) error
	UnclaimIfAssignee(ctx context.Context, id, actor, expectedAssignee string) error
	Heartbeat(ctx context.Context, id, actor string) error
	ReclaimExpiredLeases(ctx context.Context, olderThan time.Duration, filter types.ReclaimFilter, actor string) ([]types.ReclaimedLease, error)
	WakeExpiredDefers(ctx context.Context) (issues, wisps int, err error)

	CreateIssue(ctx context.Context, params CreateIssueParams, actor string) (CreateIssueResult, error)
	CreateIssues(ctx context.Context, params []CreateIssueParams, actor string) (CreateIssuesResult, error)
	UpdateIssue(ctx context.Context, id string, updates map[string]any, actor string) error
	// CompareAndSetMetadataKey is the shape issueops.MetadataCAS publishes; see
	// IssueSQLRepository.CompareAndSetMetadataKey for what the bool carries.
	CompareAndSetMetadataKey(ctx context.Context, plan storage.CompareAndSetKeyPlan) (publicops.CompareAndSetKeyResult, bool, error)
	// ReleaseIssue is the shape issueops.Releaser publishes; see
	// IssueSQLRepository.ReleaseIssue for what the bool carries.
	ReleaseIssue(ctx context.Context, req publicops.ReleaseRequest) (publicops.ReleaseResult, bool, error)
	ClaimIssue(ctx context.Context, id, actor string) (ClaimResult, error)
	ClaimIssueIfOpen(ctx context.Context, id, actor string) (ClaimResult, error)
	CloseIssue(ctx context.Context, id string, params CloseIssueParams, actor string) (CloseIssueResult, error)
	CloseIssueChecked(ctx context.Context, id string, params CloseIssueParams, actor string, force bool) (CloseIssueResult, error)
	ReopenIssue(ctx context.Context, id string, params ReopenIssueParams, actor string) (ReopenIssueResult, error)
	CountOpenChildren(ctx context.Context, id string) (int, error)
	GetNewlyUnblockedByClose(ctx context.Context, closedID string) ([]*types.Issue, error)
	ApplyUpdate(ctx context.Context, id string, spec UpdateSpec, actor string) (*types.Issue, error)
	ApplyIssueGraph(ctx context.Context, plan GraphPlan, actor string) (GraphApplyResult, error)
	AsOf(ctx context.Context, id, ref string) (*types.Issue, error)
	DeleteIssue(ctx context.Context, id, actor string) (DeleteIssuesResult, error)
	DeleteIssues(ctx context.Context, params DeleteIssuesParams, actor string) (DeleteIssuesResult, error)
	PreviewDelete(ctx context.Context, ids []string) (DeletePreview, error)
	DeleteWisp(ctx context.Context, id, actor string) (DeleteIssuesResult, error)
	DeleteWisps(ctx context.Context, params DeleteIssuesParams, actor string) (DeleteIssuesResult, error)
	PreviewDeleteWisp(ctx context.Context, ids []string) (DeletePreview, error)

	GetWisp(ctx context.Context, id string) (*types.Issue, error)
	GetWispsByIDs(ctx context.Context, ids []string) ([]*types.Issue, error)
	CreateWisp(ctx context.Context, params CreateIssueParams, actor string) (CreateIssueResult, error)
	CreateWisps(ctx context.Context, params []CreateIssueParams, actor string) (CreateIssuesResult, error)
	UpdateWisp(ctx context.Context, id string, updates map[string]any, actor string) error
	ClaimWisp(ctx context.Context, id, actor string) (ClaimResult, error)
	ClaimWispIfOpen(ctx context.Context, id, actor string) (ClaimResult, error)
	CloseWisp(ctx context.Context, id string, params CloseIssueParams, actor string) (CloseIssueResult, error)
	CloseWispChecked(ctx context.Context, id string, params CloseIssueParams, actor string, force bool) (CloseIssueResult, error)
	ReopenWisp(ctx context.Context, id string, params ReopenIssueParams, actor string) (ReopenIssueResult, error)
	CountOpenWispChildren(ctx context.Context, id string) (int, error)
	GetNewlyUnblockedByCloseWisp(ctx context.Context, closedID string) ([]*types.Issue, error)
	ApplyWispGraph(ctx context.Context, plan GraphPlan, actor string) (GraphApplyResult, error)
	ClaimReadyWisp(ctx context.Context, filter types.WorkFilter, actor string) (ClaimReadyResult, error)
	PromoteWisp(ctx context.Context, id, actor string) error
}

type CloseIssueParams struct {
	Reason  string
	Session string
}

type CloseIssueResult struct {
	Issue        *types.Issue
	Closed       bool
	OpenChildren int
}

type ReopenIssueParams struct {
	Reason string
}

type ReopenIssueResult struct {
	Issue    *types.Issue
	Reopened bool
}

func NewIssueUseCase(
	issueRepo IssueSQLRepository,
	depRepo DependencySQLRepository,
	labelRepo LabelSQLRepository,
	counterRepo ChildCounterSQLRepository,
	commentRepo CommentSQLRepository,
	cfgRepo ConfigSQLRepository,
	eventsRepo EventsSQLRepository,
	labelUC LabelUseCase,
	depUC DependencyUseCase,
) IssueUseCase {
	deps := &issueUseCaseDeps{
		issueRepo:   issueRepo,
		depRepo:     depRepo,
		labelRepo:   labelRepo,
		counterRepo: counterRepo,
		commentRepo: commentRepo,
		cfgRepo:     cfgRepo,
		eventsRepo:  eventsRepo,
		labelUC:     labelUC,
		depUC:       depUC,
	}
	lookup := &issueLookupModule{issueUseCaseDeps: deps}
	search := &issueSearchModule{issueUseCaseDeps: deps}
	report := &issueReportModule{issueUseCaseDeps: deps}
	claims := &issueClaimModule{issueUseCaseDeps: deps}
	update := &issueUpdateModule{issueUseCaseDeps: deps, lookup: lookup, claims: claims}
	create := &issueCreateModule{issueUseCaseDeps: deps, lookup: lookup}
	graph := &issueGraphModule{issueUseCaseDeps: deps, creator: create}
	close := &issueCloseModule{issueUseCaseDeps: deps}
	children := &issueChildrenModule{issueUseCaseDeps: deps}
	claimReady := &issueClaimReadyModule{issueUseCaseDeps: deps, claims: claims}
	maintenance := &issueMaintenanceModule{issueUseCaseDeps: deps}
	deletes := &issueDeleteModule{deps: deps}

	return &issueUseCaseImpl{
		issueUseCaseModuleSet: &issueUseCaseModuleSet{
			issueReadModules: &issueReadModules{
				issueLookupModule: lookup,
				issueSearchModule: search,
				issueReportModule: report,
			},
			issueMutationModules: &issueMutationModules{
				issueUpdateModule: update,
				issueClaimModule:  claims,
				issueCreateModule: create,
				issueGraphModule:  graph,
			},
			issueLifecycleModules: &issueLifecycleModules{
				issueCloseModule:       close,
				issueChildrenModule:    children,
				issueClaimReadyModule:  claimReady,
				issueMaintenanceModule: maintenance,
			},
			issueDeleteModule: deletes,
		},
	}
}

// issueUseCaseDeps is the shared adapter set used by the private use-case
// modules. Keeping it behind the public IssueUseCase seam lets each module
// own one responsibility without duplicating repository wiring.
type issueUseCaseDeps struct {
	issueRepo   IssueSQLRepository
	depRepo     DependencySQLRepository
	labelRepo   LabelSQLRepository
	counterRepo ChildCounterSQLRepository
	commentRepo CommentSQLRepository
	cfgRepo     ConfigSQLRepository
	eventsRepo  EventsSQLRepository
	labelUC     LabelUseCase
	depUC       DependencyUseCase
}

type issueLookupModule struct{ *issueUseCaseDeps }
type issueSearchModule struct{ *issueUseCaseDeps }
type issueReportModule struct{ *issueUseCaseDeps }
type issueUpdateModule struct {
	*issueUseCaseDeps
	lookup *issueLookupModule
	claims *issueClaimModule
}
type issueClaimModule struct{ *issueUseCaseDeps }
type issueCreateModule struct {
	*issueUseCaseDeps
	lookup *issueLookupModule
}
type issueGraphModule struct {
	*issueUseCaseDeps
	creator *issueCreateModule
}
type issueCloseModule struct{ *issueUseCaseDeps }
type issueChildrenModule struct{ *issueUseCaseDeps }
type issueClaimReadyModule struct {
	*issueUseCaseDeps
	claims *issueClaimModule
}
type issueMaintenanceModule struct{ *issueUseCaseDeps }

type issueReadModules struct {
	*issueLookupModule
	*issueSearchModule
	*issueReportModule
}

type issueMutationModules struct {
	*issueUpdateModule
	*issueClaimModule
	*issueCreateModule
	*issueGraphModule
}

type issueLifecycleModules struct {
	*issueCloseModule
	*issueChildrenModule
	*issueClaimReadyModule
	*issueMaintenanceModule
}

type issueUseCaseModuleSet struct {
	*issueReadModules
	*issueMutationModules
	*issueLifecycleModules
	*issueDeleteModule
}

type issueUseCaseImpl struct{ *issueUseCaseModuleSet }

var _ IssueUseCase = (*issueUseCaseImpl)(nil)

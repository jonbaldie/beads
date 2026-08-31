package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/depid"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/storage/sqlbuild"
	"github.com/jonbaldie/beads/internal/types"
)

func NewDependencySQLRepository(runner Runner) domain.DependencySQLRepository {
	core := &dependencyRepositoryCore{
		runner: runner,
		events: NewEventsSQLRepository(runner),
	}
	cycle := &dependencyCycleRepository{dependencyRepositoryCore: core}
	validation := &dependencyValidationRepository{dependencyRepositoryCore: core}
	repo := &dependencySQLRepositoryImpl{
		dependencyInsertRepository: &dependencyInsertRepository{
			dependencyRepositoryCore: core,
			cycle:                    cycle,
			validation:               validation,
		},
		dependencyDeleteRepository:     &dependencyDeleteRepository{dependencyRepositoryCore: core},
		dependencyCycleRepository:      cycle,
		dependencyValidationRepository: validation,
		dependencyListRepository:       &dependencyListRepository{dependencyRepositoryCore: core},
		dependencyBlockingRepository: &dependencyBlockingRepository{
			dependencyRepositoryCore: core,
		},
		dependencyBulkRepository:     &dependencyBulkRepository{dependencyRepositoryCore: core},
		dependencyMetadataRepository: &dependencyMetadataRepository{dependencyRepositoryCore: core},
		dependencyTreeRepository:     &dependencyTreeRepository{dependencyRepositoryCore: core},
	}
	// Keep the composition explicit: the repository interface is implemented by
	// promoted role methods, and these references make that seam visible to
	// static analyzers as well as to readers.
	_ = repo.dependencyInsertRepository
	_ = repo.dependencyDeleteRepository
	_ = repo.dependencyCycleRepository
	_ = repo.dependencyValidationRepository
	_ = repo.dependencyListRepository
	_ = repo.dependencyBlockingRepository
	_ = repo.dependencyBulkRepository
	_ = repo.dependencyMetadataRepository
	_ = repo.dependencyTreeRepository
	return repo
}

type dependencySQLRepositoryImpl struct {
	*dependencyInsertRepository
	*dependencyDeleteRepository
	*dependencyCycleRepository
	*dependencyValidationRepository
	*dependencyListRepository
	*dependencyBlockingRepository
	*dependencyBulkRepository
	*dependencyMetadataRepository
	*dependencyTreeRepository
}

type dependencyRepositoryCore struct {
	runner Runner
	events domain.EventsSQLRepository
}

type dependencyInsertRepository struct {
	*dependencyRepositoryCore
	cycle      *dependencyCycleRepository
	validation *dependencyValidationRepository
}

type dependencyDeleteRepository struct {
	*dependencyRepositoryCore
}

type dependencyCycleRepository struct {
	*dependencyRepositoryCore
}

type dependencyValidationRepository struct {
	*dependencyRepositoryCore
}

type dependencyListRepository struct {
	*dependencyRepositoryCore
}

type dependencyBlockingRepository struct {
	*dependencyRepositoryCore
}

type dependencyBulkRepository struct {
	*dependencyRepositoryCore
}

type dependencyMetadataRepository struct {
	*dependencyRepositoryCore
}

type dependencyTreeRepository struct {
	*dependencyRepositoryCore
}

var _ domain.DependencySQLRepository = (*dependencySQLRepositoryImpl)(nil)

const depTargetExpr = sqlbuild.DepTargetExpr

const depSelectColumns = "issue_id, " + depTargetExpr + " AS depends_on_id, type, created_at, created_by, metadata, thread_id"

func pickDepTable(useWisps bool) string {
	if useWisps {
		return "wisp_dependencies"
	}
	return "dependencies"
}

// pickDepTargetColumn classifies an edge's target the same way the in-tx store
// bodies do, through issueops.IsExternalDepTarget: a target this database
// cannot hold — an "external:" reference or an issue belonging to another
// repository — goes to depends_on_external, which carries no foreign key.
// Only a target that could plausibly be local is probed against wisps and
// otherwise treated as a local issue, where fk_dep_issue_target still refuses
// an id that is genuinely missing.
func (r *dependencyValidationRepository) pickDepTargetColumn(ctx context.Context, issueID, dependsOnID string) (string, error) {
	if issueops.IsExternalDepTarget(issueID, dependsOnID) {
		return "depends_on_external", nil
	}
	var probe int
	err := r.runner.QueryRowContext(ctx, "SELECT 1 FROM wisps WHERE id = ? LIMIT 1", dependsOnID).Scan(&probe)
	switch {
	case err == nil:
		return "depends_on_wisp_id", nil
	case errors.Is(err, sql.ErrNoRows):
		return "depends_on_issue_id", nil
	case dberrors.IsTableNotExist(err):
		return "depends_on_issue_id", nil
	default:
		return "", fmt.Errorf("classify dep target %s: %w", dependsOnID, err)
	}
}

func (r *dependencyInsertRepository) Insert(ctx context.Context, dep *types.Dependency, actor string, opts domain.DepInsertOpts) error {
	metadata, err := prepareDependencyInsert(dep)
	if err != nil {
		return err
	}

	if err := validateDependencyInsert(ctx, r.validation, r.cycle, dep, opts); err != nil {
		return err
	}
	table := pickDepTable(opts.UseWispsTable)

	handled, err := handleExistingDependency(ctx, r.runner, dep, table, metadata)
	if err != nil || handled {
		return err
	}

	targetCol, err := r.validation.pickDepTargetColumn(ctx, dep.IssueID, dep.DependsOnID)
	if err != nil {
		return fmt.Errorf("db: DependencySQLRepository.Insert: %w", err)
	}

	if err := insertNewDependency(ctx, r.validation, dep, actor, metadata, table, targetCol, opts.UseWispsTable); err != nil {
		return err
	}
	if err := touchParentDependency(ctx, r.runner, dep, table); err != nil {
		return err
	}

	// Record the dependency_added event on the source's event table, matching the
	// embedded/issueops AddDependencyInTx path so the bd CLI and library callers
	// observe the same history from either write plumbing. Reached only on the
	// genuine new-edge path; the idempotent same-type refresh returned earlier.
	// Gated on EmitEvent so only the explicit dep verbs emit: create-with-deps
	// and reparent call Insert directly without it, so an implicit parent-child /
	// --deps / waits-for edge produces no event. The embedded structural paths
	// (createIssueWithDeps, reparent) match this by calling the plain,
	// no-event AddDependency/tx.AddDependency, whose issueops.AddDependencyInTx
	// EmitEvent gate is likewise unset — so both backends stay silent on implicit
	// edges and emit only for the explicit bd dep add / bd link verbs.
	if err := recordExplicitDependencyAddedEvent(ctx, r, dep, actor, opts); err != nil {
		return err
	}

	return maintainInsertedDependencyState(ctx, r.runner, r.validation, dep, opts, targetCol, metadata)
}

func validateDependencyInsert(ctx context.Context, validation *dependencyValidationRepository, cycleRepo *dependencyCycleRepository, dep *types.Dependency, opts domain.DepInsertOpts) error {
	if !opts.HierarchyValidated {
		if err := validation.ValidateBlockingHierarchy(ctx, dep); err != nil {
			return err
		}
	}
	if opts.CycleValidated || !types.IsSchedulingEdge(dep.Type) {
		return nil
	}
	cycle, err := cycleRepo.HasCycle(ctx, dep.IssueID, dep.DependsOnID)
	if err != nil {
		return fmt.Errorf("db: DependencySQLRepository.Insert: cycle check: %w", err)
	}
	if cycle {
		return domain.ErrDependencyCycle
	}
	return nil
}

func handleExistingDependency(ctx context.Context, runner Runner, dep *types.Dependency, table, metadata string) (bool, error) {
	existingType, err := lookupDependencyType(ctx, runner, table, dep.IssueID, dep.DependsOnID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: DependencySQLRepository.Insert: check existing: %w", err)
	}
	if existingType != string(dep.Type) {
		return true, &domain.DependencyTypeConflictError{
			IssueID:       dep.IssueID,
			DependsOnID:   dep.DependsOnID,
			ExistingType:  existingType,
			RequestedType: string(dep.Type),
		}
	}
	if err := refreshDependencyMetadata(ctx, runner, table, dep, metadata); err != nil {
		return true, fmt.Errorf("db: DependencySQLRepository.Insert: refresh metadata: %w", err)
	}
	// A same-type add refreshes edge metadata. It is an observable graph
	// mutation, so emit the complete replacement edge for replay.
	return true, issueops.RecordDepEventInTx(ctx, runner, issueops.EventDepAdd, dep.IssueID, string(dep.Type), dep.DependsOnID, metadata)
}

func insertNewDependency(ctx context.Context, validation *dependencyValidationRepository, dep *types.Dependency, actor, metadata, table, targetCol string, useWisps bool) error {
	if err := insertDependencyRow(ctx, validation.runner, dep, actor, metadata, table, targetCol); err != nil {
		if missing := validation.classifyMissingEndpoint(ctx, dep, useWisps, targetCol, err); missing != nil {
			return missing
		}
		return fmt.Errorf("db: DependencySQLRepository.Insert: %w", err)
	}
	return nil
}

func touchParentDependency(ctx context.Context, runner Runner, dep *types.Dependency, table string) error {
	if dep.Type != types.DepParentChild {
		return nil
	}
	if err := issueops.TouchDependencyCoordinationTableInTx(ctx, runner, dep.DependsOnID, table); err != nil {
		return fmt.Errorf("db: DependencySQLRepository.Insert: %w", err)
	}
	return nil
}

func recordExplicitDependencyAddedEvent(ctx context.Context, repo *dependencyInsertRepository, dep *types.Dependency, actor string, opts domain.DepInsertOpts) error {
	if !opts.EmitEvent {
		return nil
	}
	if err := repo.events.Record(ctx, domain.Event{
		IssueID:  dep.IssueID,
		Type:     types.EventDependencyAdded,
		Actor:    actor,
		NewValue: fmt.Sprintf("Added dependency: %s %s %s", dep.IssueID, dep.Type, dep.DependsOnID),
	}, domain.RecordEventOpts{UseWispsTable: opts.UseWispsTable}); err != nil {
		return fmt.Errorf("db: DependencySQLRepository.Insert: record dependency_added event: %w", err)
	}
	return nil
}

func prepareDependencyInsert(dep *types.Dependency) (string, error) {
	if dep == nil {
		return "", errors.New("db: DependencySQLRepository.Insert: dep must not be nil")
	}
	if dep.IssueID == "" {
		return "", errors.New("db: DependencySQLRepository.Insert: IssueID must not be empty")
	}
	if dep.DependsOnID == "" {
		return "", errors.New("db: DependencySQLRepository.Insert: DependsOnID must not be empty")
	}
	if dep.IssueID == dep.DependsOnID {
		// Lead with the sentinel so this defensive repo-layer guard renders like
		// every other self-dep site instead of appending the sentinel text.
		return "", fmt.Errorf("db: DependencySQLRepository.Insert: %w: %s cannot depend on itself", domain.ErrSelfDependency, dep.IssueID)
	}
	if dep.Metadata == "" {
		return "{}", nil
	}
	return dep.Metadata, nil
}

func lookupDependencyType(ctx context.Context, runner Runner, table, issueID, dependsOnID string) (string, error) {
	var existingType string
	//nolint:gosec // G201: table and depTargetExpr are hardcoded constants
	err := runner.QueryRowContext(ctx,
		fmt.Sprintf("SELECT type FROM %s WHERE issue_id = ? AND %s = ?", table, depTargetExpr),
		issueID, dependsOnID,
	).Scan(&existingType)
	return existingType, err
}

func refreshDependencyMetadata(ctx context.Context, runner Runner, table string, dep *types.Dependency, metadata string) error {
	//nolint:gosec // G201: table and depTargetExpr are hardcoded constants
	_, err := runner.ExecContext(ctx,
		fmt.Sprintf("UPDATE %s SET metadata = ? WHERE issue_id = ? AND %s = ?", table, depTargetExpr),
		metadata, dep.IssueID, dep.DependsOnID,
	)
	return err
}

func insertDependencyRow(ctx context.Context, runner Runner, dep *types.Dependency, actor, metadata, table, targetCol string) error {
	// Deterministic id keyed on (issue_id, target), the same derivation as the
	// embedded/issueops path, so server-mode dependency creation stays merge-safe
	// across clones and works once the DEFAULT (UUID()) is dropped (#4259).
	//nolint:gosec // G201: table is one of two hardcoded constants; targetCol is from pickDepTargetColumn
	_, err := runner.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (id, issue_id, %s, type, created_at, created_by, metadata, thread_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, table, targetCol),
		depid.New(dep.IssueID, dep.DependsOnID), dep.IssueID, dep.DependsOnID, string(dep.Type),
		time.Now().UTC(), actor, metadata, dep.ThreadID,
	)
	return err
}

func maintainInsertedDependencyState(ctx context.Context, runner Runner, validation *dependencyValidationRepository, dep *types.Dependency, opts domain.DepInsertOpts, targetCol, metadata string) error {
	srcIsWisp := opts.UseWispsTable
	var affectedIssues, affectedWisps []string
	var err error
	if srcIsWisp {
		affectedIssues, affectedWisps, err = issueops.AffectedByDepChangeForWispInTx(ctx, runner, dep.IssueID, dep.DependsOnID, dep.Type)
	} else {
		affectedIssues, affectedWisps, err = issueops.AffectedByDepChangeInTx(ctx, runner, dep.IssueID, dep.DependsOnID, dep.Type)
	}
	if err != nil {
		return fmt.Errorf("db: DependencySQLRepository.Insert: affected set: %w", err)
	}
	if dep.Type == types.DepBlocks || dep.Type == types.DepConditionalBlocks {
		if err := validation.markDirectBlockedSource(ctx, dep.IssueID, srcIsWisp, dep.DependsOnID, targetCol); err != nil {
			return fmt.Errorf("db: DependencySQLRepository.Insert: mark is_blocked: %w", err)
		}
		affectedIssues, affectedWisps = issueops.RemoveSourceFromAffected(dep.IssueID, srcIsWisp, affectedIssues, affectedWisps)
	}
	if dep.Type == types.DepParentChild {
		if err := issueops.RecomputeIsBlockedInTx(ctx, runner, affectedIssues, affectedWisps); err != nil {
			return fmt.Errorf("db: DependencySQLRepository.Insert: recompute is_blocked: %w", err)
		}
		return issueops.RecordDepEventInTx(ctx, runner, issueops.EventDepAdd, dep.IssueID, string(dep.Type), dep.DependsOnID, metadata)
	}
	if err := issueops.MarkIsBlockedInTx(ctx, runner, affectedIssues, affectedWisps); err != nil {
		return fmt.Errorf("db: DependencySQLRepository.Insert: mark is_blocked (affected): %w", err)
	}
	return issueops.RecordDepEventInTx(ctx, runner, issueops.EventDepAdd, dep.IssueID, string(dep.Type), dep.DependsOnID, metadata)
}

// classifyMissingEndpoint names the endpoint behind a foreign-key refusal,
// re-read on the same runner that refused the insert. The driver's message
// names its constraint, but taking identity out of driver prose is the thing a
// typed refusal exists to avoid, so the two endpoints are read back instead —
// only on the refusal path, so a bulk add still pays no probe per edge.
//
// The refusal is never downgraded to a probe's failure: anything the reads
// cannot settle returns nil and the caller keeps the original error.
func (r *dependencyValidationRepository) classifyMissingEndpoint(ctx context.Context, dep *types.Dependency, sourceIsWisp bool, targetCol string, insertErr error) error {
	if !dberrors.IsMissingForeignKeyTarget(insertErr) {
		return nil
	}
	sourceTable := "issues"
	if sourceIsWisp {
		sourceTable = "wisps"
	}
	sourceExists, probeErr := r.rowExists(ctx, sourceTable, dep.IssueID)
	if probeErr != nil {
		return nil
	}
	if !sourceExists {
		return issueops.MissingDependencySource(dep.IssueID, dep.DependsOnID)
	}

	var targetTable string
	switch targetCol {
	case "depends_on_issue_id":
		targetTable = "issues"
	case "depends_on_wisp_id":
		targetTable = "wisps"
	default:
		return nil
	}
	targetExists, probeErr := r.rowExists(ctx, targetTable, dep.DependsOnID)
	if probeErr != nil || targetExists {
		return nil
	}
	return issueops.MissingDependencyTarget(dep.IssueID, dep.DependsOnID)
}

func (r *dependencyValidationRepository) rowExists(ctx context.Context, table, id string) (bool, error) {
	var probe int
	//nolint:gosec // G201: table is one of the two hardcoded plane tables
	err := r.runner.QueryRowContext(ctx, fmt.Sprintf("SELECT 1 FROM %s WHERE id = ? LIMIT 1", table), id).Scan(&probe)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}

func (r *dependencyValidationRepository) ValidateBlockingHierarchy(ctx context.Context, dep *types.Dependency) error {
	if dep == nil {
		return errors.New("db: DependencySQLRepository.ValidateBlockingHierarchy: dep must not be nil")
	}
	if issueops.IsExternalDepTarget(dep.IssueID, dep.DependsOnID) {
		return nil
	}
	return issueops.CheckBlockingHierarchyInTx(ctx, r.runner, dep, nil)
}

// markDirectBlockedSource mirrors issueops.markDirectBlockingDependencySourceInTx:
// is_blocked is derived state, and ready-work queries filter on it directly
// (is_blocked = 0), so a blocking edge insert must set it on the source row
// while the target is still open. updated_at is pinned because recomputing
// derived state is not an edit.
func (r *dependencyValidationRepository) markDirectBlockedSource(ctx context.Context, source string, srcIsWisp bool, target, targetCol string) error {
	sourceTable := "issues"
	if srcIsWisp {
		sourceTable = "wisps"
	}
	var targetTable string
	switch targetCol {
	case "depends_on_issue_id":
		targetTable = "issues"
	case "depends_on_wisp_id":
		targetTable = "wisps"
	default:
		// External targets carry no local status to derive from.
		return nil
	}

	//nolint:gosec // G201: sourceTable/targetTable are hardcoded constants
	_, err := r.runner.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %s s SET s.is_blocked = 1, s.updated_at = s.updated_at
		WHERE s.id = ?
		  AND s.is_blocked = 0
		  AND s.status <> 'closed' AND s.status <> 'pinned'
		  AND EXISTS (
		    SELECT 1 FROM %s t
		    WHERE t.id = ?
		      AND t.status <> 'closed' AND t.status <> 'pinned'
		  )
	`, sourceTable, targetTable), source, target)
	return err
}

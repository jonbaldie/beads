package uow

import (
	"context"
	"testing"

	"github.com/jonbaldie/beads/internal/hooks"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/types"
	publicops "github.com/jonbaldie/beads/issueops"
)

// The notifying wrapper over a REAL provider.
//
// The fakes elsewhere in this package pin the firing rules; they cannot pin
// what a hook script is actually HANDED, because the payload is assembled from
// reads the fakes answer themselves. These run against the real use cases and
// the real transaction, which is the only place a claim about the payload is
// worth anything — and the only place the import path's raw-statement seam is
// exercised at all.
//
// ONE PROVIDER FOR THE WHOLE SUITE (it boots a real Dolt sql-server) and no
// t.Parallel, matching TestImporterUOW next door.
func TestNotifyingProviderOverARealProvider(t *testing.T) {
	ctx := context.Background()
	inner := newUOWRoleFixtureProvider(t, ctx, "nfy")
	runner := &notifyRunner{}
	provider := NewNotifyingProvider(inner, Sinks{Hook: runner})

	create := func(t *testing.T, params domain.CreateIssueParams) {
		t.Helper()
		if err := RunTx(ctx, provider, func(ctx context.Context, uw UnitOfWork) (string, error) {
			_, err := uw.IssueUseCase().CreateIssue(ctx, params, "notify-test")
			return "bd: create issue", err
		}); err != nil {
			t.Fatalf("create %s: %v", params.Issue.ID, err)
		}
	}

	t.Run("PayloadCarriesLabels", func(t *testing.T) {
		create(t, domain.CreateIssueParams{
			Issue:      &types.Issue{IssueID: types.IssueID{ID: "nfy-labels"}, IssueContent: types.IssueContent{Title: "Labelled"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}},
			ExplicitID: "nfy-labels",
			CreateOnly: true,
		})

		runner.reset()
		if err := RunTx(ctx, provider, func(ctx context.Context, uw UnitOfWork) (string, error) {
			return "bd: label issue", uw.LabelUseCase().AddLabel(ctx, "nfy-labels", "lane:hooks", "notify-test")
		}); err != nil {
			t.Fatalf("add label: %v", err)
		}

		fired := runner.snapshots()
		if len(fired) != 1 {
			t.Fatalf("fired %d hooks, want 1: %v", len(fired), runner.events())
		}
		// The label the write just added has to be ON the payload. A hook
		// script routes on labels, and an unlabeled issue reads to it as an
		// unrouted one — the DoltStorage plumbing re-reads the issue before it
		// fires, so a script has always been handed a hydrated row.
		if got := fired[0].Labels; len(got) != 1 || got[0] != "lane:hooks" {
			t.Fatalf("payload labels = %v, want [lane:hooks]", got)
		}
	})

	t.Run("CreatePayloadCarriesItsInitialLabels", func(t *testing.T) {
		runner.reset()
		create(t, domain.CreateIssueParams{
			Issue:      &types.Issue{IssueID: types.IssueID{ID: "nfy-initial"}, IssueContent: types.IssueContent{Title: "Born labelled"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}},
			ExplicitID: "nfy-initial",
			Labels:     []string{"lane:initial"},
			CreateOnly: true,
		})

		fired := runner.snapshots()
		if len(fired) != 1 || runner.events()[0].event != hooks.EventCreate {
			t.Fatalf("fired %v, want one create", runner.events())
		}
		// The DoltStorage plumbing strips the labels off its on_create and
		// replays them as synthetic on_updates (divergence 1 in the file
		// header). This carries them on the create, which is the information
		// that mattered.
		if got := fired[0].Labels; len(got) != 1 || got[0] != "lane:initial" {
			t.Fatalf("create payload labels = %v, want [lane:initial]", got)
		}
	})

	t.Run("ReverseEdgeTellsTheFarEndWithItsGraph", func(t *testing.T) {
		create(t, domain.CreateIssueParams{
			Issue:      &types.Issue{IssueID: types.IssueID{ID: "nfy-target"}, IssueContent: types.IssueContent{Title: "Existing"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}},
			ExplicitID: "nfy-target",
			CreateOnly: true,
		})

		runner.reset()
		create(t, domain.CreateIssueParams{
			Issue:      &types.Issue{IssueID: types.IssueID{ID: "nfy-source"}, IssueContent: types.IssueContent{Title: "New"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}},
			ExplicitID: "nfy-source",
			CreateOnly: true,
			// `bd create --blocks nfy-target`: the edge LEAVES the existing
			// issue, so the create changed a row it did not create.
			Dependencies: []domain.DependencySpec{
				{Type: types.DepBlocks, TargetID: "nfy-target", SwapDirection: true},
			},
		})

		want := []firedHook{{hooks.EventCreate, "nfy-source"}, {hooks.EventUpdate, "nfy-target"}}
		assertFired(t, runner.events(), want)

		// And the far end's payload carries the edge the create wrote, which is
		// the whole reason its watchers are being told.
		assertCarriesEdgeTo(t, runner.snapshots()[1], "nfy-source")
	})

	t.Run("ForwardEdgeUpdatesTheCreatedRowWithItsGraph", func(t *testing.T) {
		create(t, domain.CreateIssueParams{
			Issue:      &types.Issue{IssueID: types.IssueID{ID: "nfy-parent"}, IssueContent: types.IssueContent{Title: "Parent"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}},
			ExplicitID: "nfy-parent",
			CreateOnly: true,
		})

		runner.reset()
		create(t, domain.CreateIssueParams{
			Issue:      &types.Issue{IssueID: types.IssueID{ID: "nfy-child"}, IssueContent: types.IssueContent{Title: "Child"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}},
			ExplicitID: "nfy-child",
			CreateOnly: true,
			// `bd create --parent nfy-parent`: the edge leaves the NEW row.
			ParentID: "nfy-parent",
		})

		// The create event carries the row; the update that follows carries the
		// row's GRAPH, which is the only event that does. The DoltStorage
		// plumbing fires the same pair (CompleteIssueOperationCreate then
		// dependencyHookEvents).
		want := []firedHook{{hooks.EventCreate, "nfy-child"}, {hooks.EventUpdate, "nfy-child"}}
		assertFired(t, runner.events(), want)
		assertCarriesEdgeTo(t, runner.snapshots()[1], "nfy-parent")
	})

	t.Run("ReverseEdgeAcrossPlanesStillTellsTheFarEnd", func(t *testing.T) {
		// The far end lives on the OTHER plane. A plane-pinned read of it would
		// miss and drop the notification silently, which is the failure this
		// pins: the snapshot resolves both planes.
		if err := RunTx(ctx, provider, func(ctx context.Context, uw UnitOfWork) (string, error) {
			_, err := uw.IssueUseCase().CreateWisp(ctx, domain.CreateIssueParams{
				Issue: &types.Issue{
					IssueID: types.IssueID{
						ID: "nfy-wisp-target",
					},
					IssueContent: types.IssueContent{
						Title: "Ephemeral target",
					},
					IssueWorkflow: types.IssueWorkflow{
						Status:    types.StatusOpen,
						IssueType: types.TypeTask,
						Priority:  2,
					},
					IssueWisp: types.IssueWisp{
						Ephemeral: true,
					},
				},
				ExplicitID: "nfy-wisp-target",
				CreateOnly: true,
			}, "notify-test")
			return "bd: create wisp", err
		}); err != nil {
			t.Fatalf("create wisp: %v", err)
		}

		runner.reset()
		create(t, domain.CreateIssueParams{
			Issue:      &types.Issue{IssueID: types.IssueID{ID: "nfy-cross"}, IssueContent: types.IssueContent{Title: "Durable source"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}},
			ExplicitID: "nfy-cross",
			CreateOnly: true,
			Dependencies: []domain.DependencySpec{
				{Type: types.DepBlocks, TargetID: "nfy-wisp-target", SwapDirection: true},
			},
		})

		want := []firedHook{{hooks.EventCreate, "nfy-cross"}, {hooks.EventUpdate, "nfy-wisp-target"}}
		assertFired(t, runner.events(), want)
		assertCarriesEdgeTo(t, runner.snapshots()[1], "nfy-cross")
	})

	t.Run("GraphApplyReportsItsNodesAndEveryEdgeSource", func(t *testing.T) {
		create(t, domain.CreateIssueParams{
			Issue:      &types.Issue{IssueID: types.IssueID{ID: "nfy-graph-live"}, IssueContent: types.IssueContent{Title: "Lives outside the plan"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}},
			ExplicitID: "nfy-graph-live",
			CreateOnly: true,
		})

		runner.reset()
		plan := domain.GraphPlan{
			Nodes: []domain.GraphNode{
				{Key: "root", Issue: &types.Issue{IssueID: types.IssueID{ID: "nfy-graph-root"}, IssueContent: types.IssueContent{Title: "Plan root"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}}},
				{Key: "child", Issue: &types.Issue{IssueID: types.IssueID{ID: "nfy-graph-child"}, IssueContent: types.IssueContent{Title: "Plan child"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}}, ParentKey: "root"},
			},
			// The from-side is a row the plan did NOT create, so no create
			// event names it — only the edge update tells its watchers.
			Edges: []domain.GraphEdge{
				{FromID: "nfy-graph-live", ToKey: "root", Type: types.DepBlocks},
			},
		}
		if err := RunTx(ctx, provider, func(ctx context.Context, uw UnitOfWork) (string, error) {
			_, err := uw.IssueUseCase().ApplyIssueGraph(ctx, plan, "notify-test")
			return "bd: apply graph", err
		}); err != nil {
			t.Fatalf("ApplyIssueGraph: %v", err)
		}

		// Creates in node order, then one edge-carrying update per distinct
		// source: the child (its parent link) and the live row (the explicit
		// edge). The root is a create only — nothing leaves it.
		assertFired(t, runner.events(), []firedHook{
			{hooks.EventCreate, "nfy-graph-root"},
			{hooks.EventCreate, "nfy-graph-child"},
			{hooks.EventUpdate, "nfy-graph-child"},
			{hooks.EventUpdate, "nfy-graph-live"},
		})
		assertCarriesEdgeTo(t, runner.snapshots()[2], "nfy-graph-root")
		assertCarriesEdgeTo(t, runner.snapshots()[3], "nfy-graph-root")
	})

	t.Run("LifecycleCreateUpdateAndCloseFireHooks", func(t *testing.T) {
		source, ok := provider.(IssueLifecycleSource)
		if !ok {
			t.Fatalf("wrapped provider %T does not offer the IssueLifecycle accessor", provider)
		}
		lifecycle, err := source.IssueLifecycle()
		if err != nil {
			t.Fatalf("IssueLifecycle(): %v", err)
		}

		create(t, domain.CreateIssueParams{
			Issue:      &types.Issue{IssueID: types.IssueID{ID: "nfy-life-target"}, IssueContent: types.IssueContent{Title: "Existing"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}},
			ExplicitID: "nfy-life-target",
			CreateOnly: true,
		})

		runner.reset()
		if _, err := lifecycle.Create(ctx, publicops.CreateRequest{
			Actor: "notify-test",
			Issue: &types.Issue{IssueID: types.IssueID{ID: "nfy-life-src"}, IssueContent: types.IssueContent{Title: "Lifecycle create"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}},
			Dependencies: []publicops.CreateDependency{
				{Type: types.DepBlocks, TargetID: "nfy-life-target", Reverse: true},
			},
		}); err != nil {
			t.Fatalf("Lifecycle.Create: %v", err)
		}
		assertFired(t, runner.events(), []firedHook{
			{hooks.EventCreate, "nfy-life-src"},
			{hooks.EventUpdate, "nfy-life-target"},
		})
		assertCarriesEdgeTo(t, runner.snapshots()[1], "nfy-life-src")

		runner.reset()
		if _, err := lifecycle.Update(ctx, publicops.UpdateRequest{
			Actor:   "notify-test",
			IssueID: "nfy-life-src",
			Patch:   publicops.IssuePatch{Title: publicops.Field[string]{Set: true, Value: "renamed"}},
		}); err != nil {
			t.Fatalf("Lifecycle.Update: %v", err)
		}
		assertFired(t, runner.events(), []firedHook{{hooks.EventUpdate, "nfy-life-src"}})

		runner.reset()
		if _, err := lifecycle.Update(ctx, publicops.UpdateRequest{
			Actor:   "notify-test",
			IssueID: "nfy-life-src",
		}); err != nil {
			t.Fatalf("precondition-only Lifecycle.Update: %v", err)
		}
		if got := runner.events(); len(got) != 0 {
			t.Fatalf("expectation-only update fired %v, want nothing", got)
		}

		runner.reset()
		if _, err := lifecycle.Close(ctx, publicops.CloseRequest{Actor: "notify-test", IssueID: "nfy-life-src"}); err != nil {
			t.Fatalf("Lifecycle.Close: %v", err)
		}
		assertFired(t, runner.events(), []firedHook{{hooks.EventClose, "nfy-life-src"}})
	})

	t.Run("ImportRunsUnderTheWrapperAndFiresNothing", func(t *testing.T) {
		// The import role reaches the transaction's statement runner directly
		// (importer.go), which used to mean a type assertion on the concrete
		// unit of work — and that assertion FAILED through this wrapper, so
		// every proxied import in a workspace with hooks errored out. It peels
		// the decorator now.
		source, ok := provider.(ImporterSource)
		if !ok {
			t.Fatalf("wrapped provider %T does not offer the Importer accessor", provider)
		}
		importer, err := source.Importer()
		if err != nil {
			t.Fatalf("Importer(): %v", err)
		}

		runner.reset()
		result, err := importer.ImportBatch(ctx, publicops.ImportBatchRequest{
			Actor:  "notify-test",
			Source: "notifying_integration_test",
			Issues: []*types.Issue{
				{IssueID: types.IssueID{ID: "nfy-import-1"}, IssueContent: types.IssueContent{Title: "Imported one"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}},
				{IssueID: types.IssueID{ID: "nfy-import-2"}, IssueContent: types.IssueContent{Title: "Imported two"}, IssueWorkflow: types.IssueWorkflow{Status: types.StatusOpen, IssueType: types.TypeTask, Priority: 2}},
			},
		})
		if err != nil {
			t.Fatalf("ImportBatch through the wrapper: %v", err)
		}
		if result.Created != 2 {
			t.Fatalf("Created = %d, want 2", result.Created)
		}
		// Divergence 3 in the file header: the batch engine writes statements,
		// not use-case calls, so there is nothing for the recorder to see —
		// the same silence the DoltStorage plumbing's import keeps.
		if got := runner.events(); len(got) != 0 {
			t.Fatalf("import fired %v, want nothing", got)
		}

		// The rows really landed: an import that quietly wrote nothing would
		// satisfy every assertion above.
		if _, err := RunTxRead(ctx, provider, func(ctx context.Context, uw UnitOfWork) (struct{}, error) {
			issue, err := uw.IssueUseCase().GetIssue(ctx, "nfy-import-1")
			if err != nil {
				return struct{}{}, err
			}
			if issue == nil || issue.Title != "Imported one" {
				t.Fatalf("imported issue = %+v, want the row the batch named", issue)
			}
			return struct{}{}, nil
		}); err != nil {
			t.Fatalf("read back the imported issue: %v", err)
		}
	})
}

// assertCarriesEdgeTo fails unless the payload's dependency records name the
// far end of the edge the mutation wrote — the reason its watchers are being
// told at all.
func assertCarriesEdgeTo(t *testing.T, issue *types.Issue, dependsOn string) {
	t.Helper()
	for _, edge := range issue.Dependencies {
		if edge != nil && edge.DependsOnID == dependsOn {
			return
		}
	}
	t.Fatalf("payload for %s carried dependencies %+v, want the edge to %s", issue.ID, issue.Dependencies, dependsOn)
}

package httpapi

import (
	"errors"
	"net/http"

	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/issueops"
	"github.com/jonbaldie/beads/memoryops"
)

type serverIssueRoles struct {
	reader       issueops.Reader
	claimer      issueops.Claimer
	batchCloser  issueops.BatchCloser
	readyClaimer issueops.ReadyClaimer
	releaser     issueops.Releaser
	lifecycle    issueops.Lifecycle
	relations    issueops.Relations
	commenter    issueops.Commenter
	blocking     issueops.BlockingAnnotator
	dependencies issueops.DependencyEditor
	batchApplier issueops.BatchApplier
}

type serverGraphRoles struct {
	cycles       issueops.CycleDetector
	edges        issueops.EdgeReader
	edgeCounter  issueops.GraphCounter
	tree         issueops.TreeWalker
	readyCounter issueops.ReadyCounter
	counter      issueops.Counter
	querier      issueops.Querier
}

type serverWorkspaceRoles struct {
	settings     issueops.WorkspaceConfig
	stats        issueops.StatsReporter
	sweeper      issueops.Sweeper
	deleter      issueops.Deleter
	batchCreator issueops.BatchCreator
	metadataCAS  issueops.MetadataCAS
	memories     memoryops.Memories
}

type serverJournalRoles struct {
	eventsJournal storage.EventsJournalCursor
}

type serverRoles struct {
	issue     serverIssueRoles
	graph     serverGraphRoles
	workspace serverWorkspaceRoles
	journal   serverJournalRoles
}

// reader returns the issue-query surface for one request.
//
// On the ROLES source it is the configured role. There is nothing to build: a
// store's accessor already answered for its whole decorator chain when the
// caller called it, and this server opens no units of work on that path.
//
// On the PROVIDER source it is built per request rather than once at startup so
// that the units of work it opens are timed into THIS request's log line. That
// is the only reason: the role itself is stateless, and the accessor is the API
// on this seam exactly as it is on a store.
//
// The source is held by INTERFACE, not by the concrete wrapper. That is what
// makes uow.IssueReaderSource load-bearing rather than decorative: this call
// site type-checks against the accessor the provider seam publishes, so
// renaming or dropping it is a compile error here.
//
// EITHER WAY it goes out through checkedReader, which is what makes
// handleGetIssue's dereference of the detail view safe by construction again —
// see roles.go.
func (s *Server) reader(r *http.Request) (issueops.Reader, error) {
	if s.provider == nil {
		return checkedReader{inner: s.roles.issue.reader}, nil
	}
	var src uow.IssueReaderSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	rd, err := src.IssueReader()
	if err != nil {
		return nil, err
	}
	return checkedReader{inner: rd}, nil
}

// statsReporter returns the guarded summary-statistics surface for one request.
//
// Same two sources as reader() and claimer(), for the same reasons, and held by
// INTERFACE so uow.StatsReporterSource is load-bearing rather than decorative.
// No checked wrapper: issueops.StatsResult carries a VALUE, so there is no
// nil-with-nil-error answer for a handler to dereference. checkedReader exists
// because Reader.Get hands back a pointer.
func (s *Server) statsReporter(r *http.Request) (issueops.StatsReporter, error) {
	if s.provider == nil {
		return s.roles.workspace.stats, nil
	}
	var src uow.StatsReporterSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.StatsReporter()
}

// cycleDetector returns the guarded cycle-report surface for one request.
//
// Same two sources as reader() and claimer(), for the same reasons, and held by
// INTERFACE so uow.CycleDetectorSource is load-bearing rather than decorative.
// No checked wrapper: this report is a value whose slice a nil-safe range
// walks, so there is no dereference for a misbehaving implementation to turn
// into a panic.
func (s *Server) cycleDetector(r *http.Request) (issueops.CycleDetector, error) {
	if s.provider == nil {
		return s.roles.graph.cycles, nil
	}
	var src uow.CycleDetectorSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.CycleDetector()
}

// claimer returns the guarded atomic-claim surface for one request.
//
// It is the write-side twin of reader above, for all the same reasons: the
// configured role on the roles source, and on the provider source one built per
// request so its units of work are timed into THIS request's log line, held by
// INTERFACE so uow.IssueClaimerSource is load-bearing rather than decorative —
// and, from either source, wrapped in checkedClaimer.
func (s *Server) claimer(r *http.Request) (issueops.Claimer, error) {
	if s.provider == nil {
		return checkedClaimer{inner: s.roles.issue.claimer}, nil
	}
	var src uow.IssueClaimerSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	cl, err := src.IssueClaimer()
	if err != nil {
		return nil, err
	}
	return checkedClaimer{inner: cl}, nil
}

// batchCloser returns the many-issue close surface for one request, on the same
// terms as every role above and held by INTERFACE so uow.BatchCloserSource is
// load-bearing rather than decorative.
//
// Wrapped in checkedBatchCloser from either source, and the hazard it folds is
// not the one the other wrappers exist for. Nothing here dereferences the issue
// pointer an outcome carries; what this role owns instead is a POSITIONAL array
// the client reads against its own argument list, and checkedBatchCloser says
// what a miscounted or contentless entry in it costs.
func (s *Server) batchCloser(r *http.Request) (issueops.BatchCloser, error) {
	if s.provider == nil {
		return checkedBatchCloser{inner: s.roles.issue.batchCloser}, nil
	}
	var src uow.BatchCloserSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	closer, err := src.BatchCloser()
	if err != nil {
		return nil, err
	}
	return checkedBatchCloser{inner: closer}, nil
}

// readyClaimer returns the take-ready-work surface for one request.
//
// Built the same two ways as claimer above and for the same reasons, and held
// by INTERFACE so uow.ReadyClaimerSource is load-bearing rather than
// decorative.
//
// IT GOES OUT UNWRAPPED, and the difference from checkedClaimer is the whole
// reason that wrapper exists. That one folds a nil issue because handleClaim
// DEREFERENCES the pointer the role returned; this handler forwards it, and a
// nil is not even a fault here — it is the documented answer for an empty ready
// front. A wrapper would be ceremony that reads like a guarantee.
func (s *Server) readyClaimer(r *http.Request) (issueops.ReadyClaimer, error) {
	if s.provider == nil {
		return s.roles.issue.readyClaimer, nil
	}
	var src uow.ReadyClaimerSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.ReadyClaimer()
}

// releaser returns the claim-release surface for one request.
//
// Built the same two ways as claimer above and for the same reasons: the
// configured role on the roles source, and on the provider source one built per
// request so its units of work are timed into THIS request's log line, held by
// INTERFACE so uow.ReleaserSource is load-bearing rather than decorative — and,
// from either source, wrapped in checkedReleaser, because the handler
// dereferences the pointer the result carries.
func (s *Server) releaser(r *http.Request) (issueops.Releaser, error) {
	if s.provider == nil {
		return checkedReleaser{inner: s.roles.issue.releaser}, nil
	}
	var src uow.ReleaserSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	rel, err := src.Releaser()
	if err != nil {
		return nil, err
	}
	return checkedReleaser{inner: rel}, nil
}

// lifecycle returns the guarded issue-mutation surface for one request.
//
// Built the same two ways as claimer above and for the same reasons: the
// configured role on the roles source, and on the provider source one built per
// request so its units of work are timed into THIS request's log line, held by
// INTERFACE so uow.IssueLifecycleSource is load-bearing rather than decorative
// — and, from either source, wrapped in checkedLifecycle.
func (s *Server) lifecycle(r *http.Request) (issueops.Lifecycle, error) {
	if s.provider == nil {
		return checkedLifecycle{inner: s.roles.issue.lifecycle}, nil
	}
	var src uow.IssueLifecycleSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	lc, err := src.IssueLifecycle()
	if err != nil {
		return nil, err
	}
	return checkedLifecycle{inner: lc}, nil
}

// workspaceConfig returns the guarded workspace-settings surface for one
// request.
//
// Same two sources as reader and claimer above, for the same reasons, and held
// by INTERFACE so uow.WorkspaceConfigSource is load-bearing rather than
// decorative. No checked wrapper: both settings handlers read VALUES out of the
// result, so there is no pointer for a caller-supplied role to hand back nil
// in.
func (s *Server) workspaceConfig(r *http.Request) (issueops.WorkspaceConfig, error) {
	if s.provider == nil {
		return s.roles.workspace.settings, nil
	}
	var src uow.WorkspaceConfigSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.WorkspaceConfig()
}

// edgeReader returns the guarded stored-edge surface for one request.
//
// Same two sources as reader() and claimer(), for the same reasons, and held by
// INTERFACE so uow.EdgeReaderSource is load-bearing rather than decorative. No
// checked wrapper: this role answers with a VALUE, so no handler dereferences a
// pointer it returned — checkedReader exists for Get alone.
func (s *Server) edgeReader(r *http.Request) (issueops.EdgeReader, error) {
	if s.provider == nil {
		return s.roles.graph.edges, nil
	}
	var src uow.EdgeReaderSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.EdgeReader()
}

// graphCounter returns the edge-count surface for one request, built the same
// two ways as edgeReader above and held by INTERFACE so
// uow.GraphCounterSource is load-bearing rather than decorative.
//
// It goes out UNWRAPPED, for counter's reason: the role answers with a VALUE
// whose slice a nil-safe range walks, so no handler dereferences a pointer it
// returned and a checked wrapper would be ceremony that reads like a guarantee.
func (s *Server) graphCounter(r *http.Request) (issueops.GraphCounter, error) {
	if s.provider == nil {
		return s.roles.graph.edgeCounter, nil
	}
	var src uow.GraphCounterSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.GraphCounter()
}

// relations returns the single-anchor neighbor surface for one request, built
// the same two ways as edgeReader above and held by INTERFACE so
// uow.RelationsSource is load-bearing rather than decorative.
//
// It goes out UNWRAPPED even though the role answers with a slice of POINTERS,
// which is the one place this differs from checkedReader's argument. That
// wrapper exists because handleGetIssue DEREFERENCES the pointer a role handed
// back; here wireRelated drops a nil element the way wireItems and wireEdges
// already do, so there is nothing for a checked wrapper to make safe.
func (s *Server) relations(r *http.Request) (issueops.Relations, error) {
	if s.provider == nil {
		return s.roles.issue.relations, nil
	}
	var src uow.RelationsSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.IssueRelations()
}

// commenter returns the append-one-comment surface for one request, built the
// same two ways as every role above and held by INTERFACE so
// uow.CommenterSource is load-bearing rather than decorative.
//
// It goes out WRAPPED, unlike the reads beside it and for checkedClaimer's
// reason: handleAddComment dereferences the pointer the role answers with, so a
// caller-supplied role that reported success without a row would panic on a live
// server rather than reaching the generic 500 with the fault in the log.
func (s *Server) commenter(r *http.Request) (issueops.Commenter, error) {
	if s.provider == nil {
		return checkedCommenter{inner: s.roles.issue.commenter}, nil
	}
	var src uow.CommenterSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	c, err := src.Commenter()
	if err != nil {
		return nil, err
	}
	return checkedCommenter{inner: c}, nil
}

// blockingAnnotator returns the derived blocking-decoration surface for one
// request, on the same terms as every role above and held by INTERFACE so
// uow.BlockingAnnotatorSource is load-bearing rather than decorative. It goes
// out UNWRAPPED for the reason edgeReader's answer does: this role answers with
// a VALUE, and checkedReader exists for Get alone.
func (s *Server) blockingAnnotator(r *http.Request) (issueops.BlockingAnnotator, error) {
	if s.provider == nil {
		return s.roles.issue.blocking, nil
	}
	var src uow.BlockingAnnotatorSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.BlockingAnnotator()
}

// treeWalker returns the guarded dependency-tree surface for one request.
//
// Built the same two ways as its siblings and for the same reasons, held by
// INTERFACE so uow.TreeWalkerSource is load-bearing rather than decorative. No
// checked wrapper, for the reason cycleDetector gives: this role answers with a
// VALUE whose slice a nil-safe range walks.
func (s *Server) treeWalker(r *http.Request) (issueops.TreeWalker, error) {
	if s.provider == nil {
		return s.roles.graph.tree, nil
	}
	var src uow.TreeWalkerSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.TreeWalker()
}

// readyCounter returns the ready-count surface for one request, on the same
// terms as every role above and held by INTERFACE so uow.ReadyCounterSource is
// load-bearing rather than decorative.
//
// It goes out UNWRAPPED. checkedReader and checkedClaimer exist because their
// handlers dereference a POINTER a role returned; CountReady answers with a
// value, so a wrapper would be ceremony that reads like a guarantee.
func (s *Server) readyCounter(r *http.Request) (issueops.ReadyCounter, error) {
	if s.provider == nil {
		return s.roles.graph.readyCounter, nil
	}
	var src uow.ReadyCounterSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.ReadyCounter()
}

// counter returns the issue-count surface for one request, on the same terms as
// readyCounter above and held by INTERFACE so uow.CounterSource is load-bearing
// rather than decorative.
//
// It goes out UNWRAPPED, for readyCounter's reason: both of this role's methods
// answer with a VALUE, so a checked wrapper would be ceremony that reads like a
// guarantee. The one pointer-shaped thing in its result is CountByGroupResult's
// map, and the role promises an empty map rather than nil — a promise the
// handler does not have to trust, because a nil map ranges and marshals as an
// empty object either way.
func (s *Server) counter(r *http.Request) (issueops.Counter, error) {
	if s.provider == nil {
		return s.roles.graph.counter, nil
	}
	var src uow.CounterSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.Counter()
}

// querier returns the boolean-query surface for one request, on the same terms
// as every role above and held by INTERFACE so uow.QuerierSource is
// load-bearing rather than decorative. It goes out UNWRAPPED, like the counter
// and unlike checkedReader: a page is a value carrying a slice, so there is
// nothing for a wrapper to make safe.
func (s *Server) querier(r *http.Request) (issueops.Querier, error) {
	if s.provider == nil {
		return s.roles.graph.querier, nil
	}
	var src uow.QuerierSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.Querier()
}

// sweeper returns the guarded bulk-clearance surface for one request, on the
// same terms as every role above and held by INTERFACE so uow.SweeperSource is
// load-bearing rather than decorative. It goes out unwrapped: SweepResult is a
// VALUE, so there is no pointer for a caller-supplied role to hand back nil in.
//
// The role this returns is the ONLY thing standing between a POST body and a
// mass delete — the require-a-filter gate, the pinned protection and the
// closed_at recheck are all inside it — which is why the Config field it comes
// from is required rather than optional.
func (s *Server) sweeper(r *http.Request) (issueops.Sweeper, error) {
	if s.provider == nil {
		return s.roles.workspace.sweeper, nil
	}
	var src uow.SweeperSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.Sweeper()
}

// deleter returns the named-row erasure surface for one request, on the same
// terms as every role above and held by INTERFACE so uow.DeleterSource is
// load-bearing rather than decorative. It goes out unwrapped for the reason the
// sweeper does: DeleteResult is a VALUE.
//
// The role this returns is the only thing standing between a POST body and an
// orphaned dependency graph — the guard, the id resolution and the reference
// rewrite are all inside it — which is why the Config field it comes from is
// required rather than optional.
func (s *Server) deleter(r *http.Request) (issueops.Deleter, error) {
	if s.provider == nil {
		return s.roles.workspace.deleter, nil
	}
	var src uow.DeleterSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.Deleter()
}

// batchCreator returns the batch-create surface for one request, on the same
// terms as every role above and held by INTERFACE so uow.BatchCreatorSource is
// load-bearing rather than decorative.
//
// It goes out CHECKED, unlike the ready counter. CreateBatchResult carries a
// slice of POINTERS and the response body carries values, so the handler
// dereferences every one of them — the checkedClaimer hazard, N times over.
// See checkedBatchCreator.
func (s *Server) batchCreator(r *http.Request) (issueops.BatchCreator, error) {
	if s.provider == nil {
		return checkedBatchCreator{inner: s.roles.workspace.batchCreator}, nil
	}
	var src uow.BatchCreatorSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	creator, err := src.BatchCreator()
	if err != nil {
		return nil, err
	}
	return checkedBatchCreator{inner: creator}, nil
}

// dependencyEditor returns the guarded dependency-graph write surface for one
// request, on the same terms as every role above and held by INTERFACE so
// uow.DependencyEditorSource is load-bearing rather than decorative.
//
// It goes out UNWRAPPED, like the sweeper and the deleter: both of this role's
// results are VALUES, so no handler dereferences a pointer it returned.
//
// The role this returns owns every refusal the graph can raise — the cycle
// gate, the hierarchy rule, the type conflict and the endpoint existence checks
// — which is why the Config field it comes from is required rather than
// optional.
func (s *Server) dependencyEditor(r *http.Request) (issueops.DependencyEditor, error) {
	if s.provider == nil {
		return s.roles.issue.dependencies, nil
	}
	var src uow.DependencyEditorSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.DependencyEditor()
}

// metadataCAS returns the conditional metadata write for one request, on the
// same terms as every role above and held by INTERFACE so
// uow.MetadataCASSource is load-bearing rather than decorative.
//
// It goes out UNWRAPPED: the role's result is a VALUE whose only pointer member
// is an optional raw value the handler passes through without dereferencing.
func (s *Server) metadataCAS(r *http.Request) (issueops.MetadataCAS, error) {
	if s.provider == nil {
		return s.roles.workspace.metadataCAS, nil
	}
	var src uow.MetadataCASSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.MetadataCAS()
}

// batchApplier returns the ordered-plan write surface for one request, on the
// same terms as every role above and held by INTERFACE so
// uow.BatchApplierSource is load-bearing rather than decorative.
//
// It goes out UNWRAPPED, like the dependency editor: ApplyBatchResult is a
// VALUE, and the one pointer its items carry — the post-item issue snapshot —
// never reaches the wire, so no handler dereferences anything this role
// returned.
//
// The role owns every refusal this operation can raise: the ref graph, the
// as-modified preconditions, the close policy, the assignee fence and the end
// gate. That is why the Config field it comes from is required rather than
// optional.
func (s *Server) batchApplier(r *http.Request) (issueops.BatchApplier, error) {
	if s.provider == nil {
		return s.roles.issue.batchApplier, nil
	}
	var src uow.BatchApplierSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.BatchApplier()
}

// memories returns the persistent-memory surface for one request, on the same
// terms as every role above and held by INTERFACE so uow.MemoriesSource is
// load-bearing rather than decorative.
//
// It goes out UNWRAPPED: all four of this role's results are VALUES, so no
// handler dereferences a pointer it returned, and a miss is a Found field
// rather than a nil the wire would have to interpret.
func (s *Server) memories(r *http.Request) (memoryops.Memories, error) {
	if s.provider == nil {
		return s.roles.workspace.memories, nil
	}
	var src uow.MemoriesSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.Memories()
}

// eventsJournalCursor returns the journal read surface for one request, on the
// same terms as every role above and held by INTERFACE so
// uow.EventsJournalCursorSource is load-bearing rather than decorative.
//
// It goes out UNWRAPPED: a page is a value carrying a slice, so there is
// nothing for a checked wrapper to make safe — the querier's argument.
//
// The narrow type is the point of this accessor rather than an accident of it.
// The provider can also hand out uw.EventsJournalUseCase(), which PRUNES; going
// through a source that only publishes storage.EventsJournalCursor is what
// keeps the read-only promise a fact about what the handler is holding.
func (s *Server) eventsJournalCursor(r *http.Request) (storage.EventsJournalCursor, error) {
	if s.provider == nil {
		if s.roles.journal.eventsJournal == nil {
			// Unreachable through the handler, which refuses a disabled
			// workspace before asking for a reader, and Listen refuses an
			// enabled one with no reader. Said out loud rather than returned as
			// a nil interface for the reason WithUOW says its own: a 500 naming
			// the condition beats a panic on a live server if either of those
			// two gates is ever moved.
			return nil, errors.New("httpapi: this server has no events-journal reader; it was configured for a workspace with the journal off")
		}
		return s.roles.journal.eventsJournal, nil
	}
	var src uow.EventsJournalCursorSource = timedProvider{inner: s.provider, rec: requestInfo(r.Context())}
	return src.EventsJournalCursor()
}

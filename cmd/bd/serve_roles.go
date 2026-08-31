package main

import (
	"errors"
	"fmt"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/issueops"
	"github.com/jonbaldie/beads/memoryops"
)

// serveIssueRoles takes the roles this server answers from off the store the
// root command already opened.
//
// ONE PEEL, and never storage.UnwrapStore. bd's chain is
// HookFiringStore -> InstrumentedStorage -> raw, and every decorator publishes
// its own roles — that is what the accessors are for — so store.IssueClaimer()
// returns a claimer that runs the workspace's on_update script for every claim
// it lands. This server documents that hooks do not fire, so the hook layer has
// to come off; the telemetry layer beneath it must not, or every request this
// process serves goes unspanned and untimed. httpapi.Listen refuses a
// hook-firing role rather than trusting this comment, so getting it wrong is a
// startup error rather than a silent subprocess per claim.
//
// The assertion is conditional because a BD_NO_HOOKS=1 workspace has no hook
// layer to peel.
//
// It returns the WHOLE set httpapi.Config requires; Listen refuses a partial
// set (see checkDatabaseSource), so a role missing here is a startup failure
// rather than a nil dereference on the first request that reaches it.
func serveIssueRoles(src serveRoleSource, journalEnabled bool) (serveRoles, error) {
	var roles serveRoles
	if src == nil {
		// A set of nil roles would reach Listen as "no database source" —
		// true, and useless. Name the condition that actually happened.
		return roles, errors.New("no store is open for this workspace")
	}
	if hooked, ok := src.(*storage.HookFiringStore); ok {
		src = hooked.Unwrap()
	}

	// Each entry binds one Config field to the accessor that fills it, and
	// names itself in the failure.
	type binding struct {
		name string
		get  func() error
	}
	for _, b := range []binding{
		{"issue reader", func() (err error) { roles.issues.reader, err = src.IssueReader(); return }},
		{"issue claimer", func() (err error) { roles.issues.claimer, err = src.IssueClaimer(); return }},
		{"batch closer", func() (err error) { roles.issues.batchCloser, err = src.BatchCloser(); return }},
		{"ready claimer", func() (err error) { roles.issues.readyClaimer, err = src.ReadyClaimer(); return }},
		{"issue releaser", func() (err error) { roles.issues.releaser, err = src.Releaser(); return }},
		{"issue lifecycle", func() (err error) { roles.issues.lifecycle, err = src.IssueLifecycle(); return }},
		{"workspace config", func() (err error) { roles.workspace.settings, err = src.WorkspaceConfig(); return }},
		{"stats reporter", func() (err error) { roles.workspace.stats, err = src.StatsReporter(); return }},
		{"cycle detector", func() (err error) { roles.graph.cycles, err = src.CycleDetector(); return }},
		{"edge reader", func() (err error) { roles.graph.edges, err = src.EdgeReader(); return }},
		{"graph counter", func() (err error) { roles.graph.edgeCounter, err = src.GraphCounter(); return }},
		{"issue relations", func() (err error) { roles.issues.relations, err = src.IssueRelations(); return }},
		{"commenter", func() (err error) { roles.issues.commenter, err = src.Commenter(); return }},
		{"blocking annotator", func() (err error) { roles.issues.blocking, err = src.BlockingAnnotator(); return }},
		{"tree walker", func() (err error) { roles.graph.tree, err = src.TreeWalker(); return }},
		{"ready counter", func() (err error) { roles.graph.readyCounter, err = src.ReadyCounter(); return }},
		{"counter", func() (err error) { roles.graph.counter, err = src.Counter(); return }},
		{"querier", func() (err error) { roles.graph.querier, err = src.Querier(); return }},
		{"sweeper", func() (err error) { roles.workspace.sweeper, err = src.Sweeper(); return }},
		{"deleter", func() (err error) { roles.workspace.deleter, err = src.Deleter(); return }},
		{"batch creator", func() (err error) { roles.workspace.batchCreator, err = src.BatchCreator(); return }},
		{"dependency editor", func() (err error) { roles.issues.dependencyEditor, err = src.DependencyEditor(); return }},
		{"metadata cas", func() (err error) { roles.workspace.metadataCAS, err = src.MetadataCAS(); return }},
		{"batch applier", func() (err error) { roles.issues.batchApplier, err = src.BatchApplier(); return }},
		{"memories", func() (err error) { roles.workspace.memories, err = src.Memories(); return }},
		{"events journal", func() error {
			// storage.UnwrapStore rather than the ONE peel above, and that is not
			// an exception to this function's rule — it is the rule applied to a
			// capability that no decorator publishes. The hook and telemetry
			// layers wrap ROLES; neither implements the journal seam, so the
			// assertion has to reach the concrete store or it finds nothing at
			// all. Nothing is skipped by going the whole way: there is no
			// journal decorator to peel past.
			//
			// A ROLE REACHED BY ASSERTION IS NOT A ROLE OUTSIDE THE RULES.
			// journalops.Journal is a facade role with a contract tier and a
			// per-leg lock like every other; what it has no accessor for is
			// the reason issueops.Importer has none either — the capability is
			// not on storage.DoltStorage's published surface, so the census
			// that would otherwise miss it reads SOURCE rather than reflecting
			// over accessors (backend/conformance/role_coverage_scan_test.go).
			// The assertion below is what a front door does with such a role,
			// not a shortcut around one.
			cursor, ok := serveJournalCursor(src)
			if ok {
				roles.journal.eventsJournal = cursor
				return nil
			}
			// A backend that cannot read the journal is an ordinary backend
			// while the workspace records nothing — the journal is off by
			// default, and eventsjournal.Apply takes exactly this deal when it
			// binds activation at open time. Only an ENABLED workspace has
			// asked for something this backend cannot do, and the message is
			// the one `bd events` prints for the same condition.
			if journalEnabled {
				return fmt.Errorf("storage backend does not support the events journal")
			}
			return nil
		}},
	} {
		if err := b.get(); err != nil {
			return serveRoles{}, fmt.Errorf("%s: %w", b.name, err)
		}
	}
	return roles, nil
}

// serveJournalCursor is storage.UnwrapStore reached from serveRoleSource, which
// is narrower than the whole store UnwrapStore takes. A source that is not a
// store has no decorator to peel — the test stubs are exactly that — so it is
// asked for the seam as it stands.
//
// It survives the journal's promotion to a facade role (journalops.Journal,
// which storage.EventsJournalCursor aliases) unchanged, and deliberately: a
// role with no accessor is reached exactly this way. issueops.Importer set the
// precedent — one accessor, none on the store interface — and the apparatus
// that keeps such a role honest is the conformance census, which parses the
// facade packages for declarations instead of reflecting over the accessors a
// store hands out. There is no accessor for this function to have been
// replaced by.
func serveJournalCursor(src serveRoleSource) (storage.EventsJournalCursor, bool) {
	raw := any(src)
	if store, ok := src.(storage.DoltStorage); ok {
		raw = storage.UnwrapStore(store)
	}
	cursor, ok := raw.(storage.EventsJournalCursor)
	return cursor, ok
}

// serveRoles is the store-shaped database source, assembled once before Listen.
// It is deliberately NOT an httpapi.Config: the gate test in serve_test.go
// requires every httpapi.Config literal in this package to sit in a function
// that consulted serveDatabaseSource.
type serveRoles struct {
	issues    serveIssueRoleSet
	graph     serveGraphRoleSet
	workspace serveWorkspaceRoleSet
	journal   serveJournalRoleSet
}

// serveIssueRoleSet contains the issue mutation and annotation capabilities
// exposed by the server. It is embedded in serveRoles so the configuration
// wiring can keep naming the capabilities directly while the role families
// remain independently understandable.
type serveIssueRoleSet struct {
	reader  issueops.Reader
	claimer issueops.Claimer
	// batchCloser closes many issues as one transaction, behind
	// POST /v0/beads/issues:batchClose. Its accessor does NOT recurse through
	// the hook decorator today, so it is taken off the peeled store with the
	// rest for uniformity rather than out of necessity.
	batchCloser issueops.BatchCloser
	// readyClaimer is the atomic take of ready work, behind
	// POST /v0/beads/issues:claimNext. Its accessor does NOT recurse through
	// the hook decorator today, so it is taken off the peeled store with the
	// rest for uniformity rather than out of necessity.
	readyClaimer issueops.ReadyClaimer
	// releaser is the claim's inverse, behind
	// POST /v0/beads/issues/{id}:release. Its accessor recurses through the
	// hook decorator like the two below it — a release is an update, so
	// HookFiringStore.Releaser fires the workspace's on_update script — which
	// is why it comes off the PEELED store with the rest.
	releaser  issueops.Releaser
	lifecycle issueops.Lifecycle
	relations issueops.Relations
	commenter issueops.Commenter
	blocking  issueops.BlockingAnnotator
	// dependencyEditor is the second role here whose accessor recurses through
	// the hook decorator, so taking it off the peeled store is not optional:
	// HookFiringStore.DependencyEditor fires the workspace's update hook per
	// edited source issue, and this server documents that hooks do not fire.
	dependencyEditor issueops.DependencyEditor
	// metadataCAS is the conditional single-key metadata write. Its accessor
	// recurses through the hook decorator, so the ONE peel above is what keeps
	// this server from running the workspace's on_update script per swap.
	// batchApplier is the role that makes the ONE peel above matter most, and
	// the arithmetic is what makes it worth its own sentence. Its hook wrapper
	// fires FOUR vocabularies from one call — on_create for every created item,
	// on_update for every changed update AND once per distinct edge source, and
	// the close hooks for every close that landed — so one hundred-item plan
	// served from an unpeeled applier is up to a hundred of the workspace's own
	// subprocesses spawned inside a single HTTP request, holding a write
	// transaction open while they run. Every other role here costs at most one
	// per mutation.
	batchApplier issueops.BatchApplier
}

// serveGraphRoleSet contains the graph and readiness capabilities. These roles
// all answer questions about relationships or the shape of available work.
type serveGraphRoleSet struct {
	cycles       issueops.CycleDetector
	edges        issueops.EdgeReader
	edgeCounter  issueops.GraphCounter
	tree         issueops.TreeWalker
	readyCounter issueops.ReadyCounter
	counter      issueops.Counter
	querier      issueops.Querier
}

// serveWorkspaceRoleSet contains workspace-level and maintenance capabilities
// that are not part of an individual issue's mutation surface.
type serveWorkspaceRoleSet struct {
	settings     issueops.WorkspaceConfig
	stats        issueops.StatsReporter
	sweeper      issueops.Sweeper
	deleter      issueops.Deleter
	batchCreator issueops.BatchCreator
	metadataCAS  issueops.MetadataCAS
	// memories is the one role here that is not an issueops role: the memory
	// plane is user data riding in the config table under its own merge class,
	// so it has its own leaf package.
	memories memoryops.Memories
}

// serveJournalRoleSet isolates the accessor-less journal capability from the
// ordinary issueops and workspace role families.
type serveJournalRoleSet struct {
	// eventsJournal is the only role here that comes from a TYPE ASSERTION
	// rather than an accessor, because the journal is not part of DoltStorage's
	// published surface: it is a replay feed over engine state on a
	// dolt_ignored table that the two concrete stores implement and a backend
	// may not. Missing it is a startup error rather than a route that 500s,
	// which is the same deal every other role here takes.
	//
	// It IS a role — journalops.Journal, which storage.EventsJournalCursor
	// aliases — with its own contract tier and per-leg lock. Accessorless is
	// the issueops.Importer shape, not an exemption; see serveJournalCursor.
	eventsJournal storage.EventsJournalCursor
}

// serveResolvedMode labels the topology for the startup log line. Cosmetic —
// nothing dispatches on it — but the managed/external distinction is worth
// naming: an external dolt sql-server shares its max_connections budget with
// every other bd process pointed at it, and this server's pool is a claim on
// that budget.
//
// A registered backend is named instead of being given a Dolt mode, which it
// no longer has: GetContextInfo projects the Dolt-derived identity only for the
// Dolt backend (internal/storage/domain/context.go), so info.DoltMode is empty
// here and " (external dolt)" would read as a bare parenthetical about a
// topology this workspace is not on.
func serveResolvedMode(info domain.ContextInfo, db serveDatabase) string {
	if db.source == serveSourceStore {
		return db.backend + " (registered backend)"
	}
	if !usesProxiedServer() {
		// Server, external-server and shared-server: serve fronts the running
		// dolt sql-server rather than starting one, so from this process the
		// server is external even when Beads is what started it.
		return info.DoltMode + " (external dolt)"
	}
	client, err := configfile.LoadProxiedServerClientInfo(info.BeadsDir)
	if err == nil && client != nil && client.External != nil {
		return info.DoltMode + " (external dolt)"
	}
	return info.DoltMode + " (managed dolt)"
}

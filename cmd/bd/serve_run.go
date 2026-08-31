package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/eventsjournal"
	"github.com/jonbaldie/beads/internal/httpapi"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/storage/backends"
	"github.com/jonbaldie/beads/internal/storage/contextinfo"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/issueops"
	"github.com/jonbaldie/beads/memoryops"
)

func runServe() error {
	return runServeWithFlags(serveFlagOptionsFromCommand(serveCmd))
}

func runServeWithFlags(flags serveFlagOptions) error {
	// Flag validation first: it depends on nothing about the workspace, so the
	// refusal for a bad --addr or an unservable auth posture is the same in
	// every mode, and it lands before anything opens a database.
	opts, err := resolveServeConfigFromFlags(flags)
	if err != nil {
		return HandleError("%v", err)
	}
	if isReadonlyMode() {
		return HandleError("%v", errServeReadonly())
	}

	cwd, err := os.Getwd()
	if err != nil {
		return HandleError("cannot resolve working directory: %v", err)
	}
	info, err := contextinfo.NewContextProvider(cwd, Version).ContextUseCase().GetContextInfo(getRootContext())
	if err != nil {
		return HandleError("cannot resolve workspace context: %v", err)
	}

	// The classification reads the workspace's own configuration, so it runs
	// after the context resolves rather than before it. A directory with no
	// workspace at all therefore reports that it has none, instead of reporting
	// the embedded refusal for a workspace nobody found.
	db, err := serveDatabaseSource(info.BeadsDir)
	if err != nil {
		return HandleError("%v", err)
	}

	if db.source == serveSourceStore {
		return runServeStore(opts, info, db)
	}
	return runServeProvider(opts, info, db)
}

// runServeStore serves the store the root command already opened. Nothing is
// created here: the same backends.Lookup dispatch opened the store ordinary bd
// commands use, and opening another handle would double pools and conflict with
// backends that take an exclusive workspace lock. PersistentPostRunE closes it
// after this function returns, once the server has drained.
func runServeStore(opts serveOptions, info domain.ContextInfo, db serveDatabase) error {
	journalEnabled := eventsjournal.EnabledFor(info.BeadsDir)
	roles, err := serveIssueRoles(getStore(), journalEnabled)
	if err != nil {
		return HandleError("bd serve: %v", err)
	}
	defer startServeEventsJournalMaintenance(info.BeadsDir, getStore())()
	return serveListen(opts, httpapi.Config{
		SourceRoles: httpapi.SourceRoles{
			IssueRoles: httpapi.IssueRoles{
				Reader:            roles.issues.reader,
				Claimer:           roles.issues.claimer,
				BatchCloser:       roles.issues.batchCloser,
				ReadyClaimer:      roles.issues.readyClaimer,
				Releaser:          roles.issues.releaser,
				Lifecycle:         roles.issues.lifecycle,
				Relations:         roles.issues.relations,
				Commenter:         roles.issues.commenter,
				BlockingAnnotator: roles.issues.blocking,
				DependencyEditor:  roles.issues.dependencyEditor,
				BatchApplier:      roles.issues.batchApplier,
			},
			GraphRoles: httpapi.GraphRoles{
				CycleDetector: roles.graph.cycles,
				EdgeReader:    roles.graph.edges,
				GraphCounter:  roles.graph.edgeCounter,
				TreeWalker:    roles.graph.tree,
				ReadyCounter:  roles.graph.readyCounter,
				Counter:       roles.graph.counter,
				Querier:       roles.graph.querier,
			},
			WorkspaceRoles: httpapi.WorkspaceRoles{
				Settings:     roles.workspace.settings,
				Stats:        roles.workspace.stats,
				Sweeper:      roles.workspace.sweeper,
				Deleter:      roles.workspace.deleter,
				BatchCreator: roles.workspace.batchCreator,
				MetadataCAS:  roles.workspace.metadataCAS,
				Memories:     roles.workspace.memories,
			},
			JournalRoles: httpapi.JournalRoles{
				// Nil when this backend has no journal seam and the workspace never
				// asked for one; Listen requires it exactly when the flag below is
				// set, and serveIssueRoles has already refused the enabled case.
				EventsJournal: roles.journal.eventsJournal,
				// GET /v0/beads/events refuses outright on a workspace that records
				// nothing, because a disabled journal and an empty one are one answer
				// at the data level. Resolved ONCE above, so the role extraction, this
				// flag and the maintenance ticker cannot disagree about journaling.
				EventsJournalEnabled: journalEnabled,
			},
		},
		Workspace:     info,
		SchemaVersion: JSONSchemaVersion,
		Mode:          serveResolvedMode(info, db),
	})
}

// runServeProvider serves from the provider beneath the hook layer. `bd serve`
// documents that it runs no hooks — a user-controlled subprocess per mutation
// is an unbounded latency multiplier and an orphaned child at shutdown — while
// proxied mode wires a notifying provider so the CLI's own writes keep firing
// them. This is the unit-of-work twin of the HookFiringStore.Unwrap the
// store-shaped source takes.
func runServeProvider(opts serveOptions, info domain.ContextInfo, db serveDatabase) error {
	provider := uow.UnwrapProvider(getUOWProvider())
	if provider == nil {
		// Server, external-server and shared-server workspaces: PersistentPreRunE
		// builds a DoltStore for those and no unit-of-work provider, so serve
		// builds its own from the same connection settings the store used.
		//
		// On this arm that store stays open for the life of the process and
		// serve never touches it. It is not free either: its pool holds an idle
		// connection or two against the very server this process is about to
		// pool twenty more on. Worth knowing when sizing a shared Dolt server's
		// max_connections.
		topology, err := resolveServerModeUOWTopology(getRootContext(), info.BeadsDir)
		if err != nil {
			return HandleError("bd serve: %v", err)
		}
		// GET /v0/beads/context is the one endpoint automation is told to trust
		// for this server's identity, and its Database comes from metadata.json,
		// which knows nothing about --global. Report the database the provider
		// actually opened, or the handshake names one database while every
		// operation answers from another.
		//
		// The store source needs no such override: backend.Open consumes the
		// same metadata.json GetContextInfo just read, so the handshake and the
		// operations share one source of truth by construction.
		info.Database = topology.database
		p, err := newSQLServerUOWProvider(getRootContext(), info.BeadsDir, topology)
		if err != nil {
			return HandleError("bd serve: %v", err)
		}
		defer func() {
			// By the time this runs the signal context is already canceled, so a
			// close that inherited it could not do any of its work.
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(getRootContext()), providerCloseTimeout)
			defer cancel()
			if err := p.Close(closeCtx); err != nil {
				fmt.Fprintf(os.Stderr, "bd serve: closing the unit-of-work provider: %v\n", err)
			}
		}()
		provider = p
	}

	defer startServeEventsJournalMaintenance(info.BeadsDir, provider)()

	return serveListen(opts, httpapi.Config{
		Provider: provider,
		SourceRoles: httpapi.SourceRoles{
			JournalRoles: httpapi.JournalRoles{
				EventsJournalEnabled: eventsjournal.EnabledFor(info.BeadsDir),
			},
		},
		// No EventsJournal field on this arm: the provider carries the journal
		// read as one of its own capability accessors, exactly as it carries
		// every other role Listen would otherwise need spelled out. Activation
		// is still the workspace's answer and still has to be handed in — the
		// provider knows how to READ the journal, not whether this workspace has
		// one.
		Workspace:     info,
		SchemaVersion: JSONSchemaVersion,
		Mode:          serveResolvedMode(info, db),
	})
}

// startServeEventsJournalMaintenance runs events-journal retention for the life
// of the server and returns the function that stops it.
//
// A server is not a command, so it is excluded from the per-command maintenance
// net (runsPostCommandMaintenance) — including the writer-pays auto-prune
// trigger that fires after a CLI mutation. Without this, the one topology that
// journals fastest, because it is the one accepting concurrent HTTP mutations
// for hours, would be the one that never prunes. A ticker is the honest shape
// for a process with no command boundary: it polls, and the same persisted
// watermark every CLI process reads decides whether a pass is actually due, so
// a server and the CLIs beside it share one schedule rather than three.
//
// The stop is deferred by the caller so it runs after the server has drained:
// maintenance and requests overlap freely (both are ordinary transactions), but
// nothing should still be deleting when the provider closes underneath it.
func startServeEventsJournalMaintenance(beadsDir string, source any) func() {
	if !eventsjournal.EnabledFor(beadsDir) || !eventsjournal.AutoPruneEnabledFor(beadsDir) {
		return func() {}
	}
	// The plumbing this server answers FROM, not whatever the root pre-run left
	// in a global: on the server-mode arm serve builds its own provider, and
	// maintaining a journal through a different handle than the one writing it
	// is how a topology ends up pruning the wrong database.
	runner := eventsJournalMaintenanceRunnerFor(source)
	if runner == nil {
		return func() {}
	}
	return eventsjournal.StartAutoPruneTicker(getRootContext(), runner,
		eventsjournal.DefaultAutoPruneTickInterval,
		eventsJournalAutoPruneOptions(),
		reportEventsJournalAutoPrune)
}

// serveListen binds and runs. It is where the two database sources converge:
// everything past the source is the same server.
//
// The operator's options and the database source arrive separately because they
// are resolved at different times — the flags before anything opens a database,
// the source only once the workspace has been classified — and this is where
// they meet, once, for every arm.
//
// Graceful shutdown rides the signal context the root command already sets up
// (SIGINT/SIGTERM/SIGHUP). A proxied provider is closed where every proxied
// command closes it, in PersistentPostRunE — which in proxied mode does nothing
// else; the provider serve built for a server-mode workspace is closed by
// runServe's own defer, and a registered backend's store by the same
// PersistentPostRunE that opened it. None of those paths runs the auto-commit,
// export or push maintenance: proxied mode never had it, and serve is excluded
// from it by name (runsPostCommandMaintenance, cmd/bd/main.go).
func serveListen(opts serveOptions, cfg httpapi.Config) error {
	applyServeOptions(&cfg, opts)

	// The posture warning belongs here rather than on either source arm: it
	// reports what this bind does not protect, which is a property of the
	// listener both arms share.
	warnServePosture(os.Stderr, opts)

	srv, err := httpapi.Listen(cfg)
	if err != nil {
		return HandleError("%v", err)
	}
	return srv.Serve(getRootContext())
}

// warnServePosture says out loud what this deployment does not protect.
//
// Two different warnings, because they are two different exposures. Running
// unauthenticated beyond loopback is the one that was always here, and the
// operator now has to have asked for it by name. Running authenticated beyond
// loopback is the remaining gap: the token itself crosses the network in
// plaintext on every request, so a network that can read it can replay it.
//
// Nothing is printed for the loopback default, which is what it has always
// been.
func warnServePosture(w io.Writer, opts serveOptions) {
	if !opts.AllowNonLoopback {
		return
	}
	if opts.Auth == nil {
		fmt.Fprintf(w,
			"bd serve: WARNING: --insecure-no-auth binds %s beyond loopback with no authentication. "+
				"Any peer that can reach it can read every issue and claim work as any actor.\n",
			opts.Addr)
		return
	}
	fmt.Fprintf(w,
		"bd serve: WARNING: %s is bound beyond loopback with bearer authentication but NO TLS. "+
			"Tokens and issue data travel in plaintext; deploy it inside a trusted network boundary.\n",
		opts.Addr)
}

// serveSource names which of httpapi.Config's two database sources a workspace
// is served from.
type serveSource int

const (
	// serveSourceProvider is the unit-of-work provider: one unit of work per
	// request, timed into that request's uow_ms and drawn from a pool bd serve
	// bounds itself. Every Dolt SQL-server topology takes it.
	serveSourceProvider serveSource = iota
	// serveSourceStore is the role set, taken off the store the root command
	// already opened. A registered backend's facade is a store rather than a
	// unit-of-work provider, so this is the source it has.
	serveSourceStore
)

// String names the source. It exists so a failed comparison prints "store" or
// "provider" rather than an integer.
func (s serveSource) String() string {
	if s == serveSourceStore {
		return "store"
	}
	return "provider"
}

// serveDatabase is the resolved answer to the one question bd serve asks about
// a workspace before it binds anything: where does this server read and claim
// from.
type serveDatabase struct {
	// source is which of httpapi.Config's two database sources to build.
	source serveSource
	// backend is the registered backend's name on the store source, and empty
	// on the provider source.
	backend string
}

// serveDatabaseSource classifies the workspace. It is both the mode gate and
// the wiring decision, in one function, so the two can never disagree about one
// workspace.
//
// THE REGISTRY IS CONSULTED FIRST, and that ordering is not a preference.
// PersistentPreRunE dispatches the store open on backends.Lookup before
// anything looks at Dolt mode, so a registered workspace opens its registered
// store even with BEADS_DOLT_SHARED_SERVER=1 exported. Resolving it the other
// way here would build a Dolt unit-of-work provider over a non-Dolt store and
// answer HTTP from a different database than the CLI reaches in the same
// directory. Registry-first is also what closes the !cgo corner, where
// isEmbeddedMode is a constant false and a registered workspace would otherwise
// be handed to the Dolt provider and fail with a misleading Dolt error — or
// connect to a defaulted host and serve the wrong database.
//
// EMBEDDED DOLT IS PERMANENT, and this is the only place that refusal lives.
// Its commit protocol runs outside the SQL transaction on a separate
// connection, so the per-request atomicity this server's contract states would
// be a lie there. That is a property of the backend rather than of what has
// been built so far, which is also why no unit-of-work provider for it exists
// or will. Nothing downstream will catch a bypass: internal/httpapi cannot see
// the backend behind a role, and every store publishes every role accessor
// whatever it is. TestServeNamesOneDatabaseSourcePerServerItBuilds pins that
// the roles are only ever reached through here.
func serveDatabaseSource(beadsDir string) (serveDatabase, error) {
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		// Never classify past an unreadable metadata.json. The classification's
		// default is the embedded refusal, so falling back would refuse a
		// workspace whose real backend nobody managed to read.
		return serveDatabase{}, fmt.Errorf("load %s: %w", configfile.ConfigPath(beadsDir), err)
	}
	if backend := normalizeLoadedConfig(cfg).GetBackend(); backends.Registered(backend) {
		return serveDatabase{source: serveSourceStore, backend: backend}, nil
	}
	if isEmbeddedMode() {
		return serveDatabase{}, errServeEmbedded()
	}
	return serveDatabase{source: serveSourceProvider}, nil
}

// errServeReadonly refuses `bd --readonly serve`.
//
// AHEAD OF THE WORKSPACE, deliberately: every server this command builds
// publishes the same operation set, claim included, so the answer cannot depend
// on which database source the workspace resolves to. Putting it here is also
// what makes it one answer rather than two — the two sources degraded
// differently, and both silently.
//
//   - On the STORE source the root command opens the workspace through
//     backend.OpenReadOnly and serve takes its claimer off that store. The
//     server bound, GET /v0/beads/context went on advertising `issues.claim`
//     (the capability set is derived from the route table and knows nothing
//     about a CLI flag), and every claim answered 500 with the issue left open.
//   - On the PROVIDER source serve builds its own unit-of-work provider from
//     the workspace's connection settings, which carries no read-only posture,
//     so `--readonly` bought the operator nothing and every claim landed.
//     (Proxied mode never got here: the root pre-run already refuses strict
//     readonly for it.)
//
// REFUSING RATHER THAN NARROWING THE SURFACE. Dropping `issues.claim` from a
// read-only server's advertised capabilities would be a wire change — that list
// is the documented pre-flight a client checks — and it would make one
// operation's presence depend on a flag on the process that happened to start
// the server, which no client can discover before connecting. bd already
// answers this question the same way one layer down, where a backend that
// cannot guarantee mutation-free access is turned away rather than opened
// anyway (backendSupportsStrictReadonly, cmd/bd/main.go).
//
// The value is read from the global rather than a flag lookup because
// `readonly` is also a config key, and PersistentPreRunE has already folded
// both into readonlyMode by the time any RunE runs.
func errServeReadonly() error {
	return errors.New("bd serve is unavailable under strict readonly (--readonly, or readonly in config): " +
		"every server it binds publishes the issue-claim operation, and refusing to start is the only honest " +
		"answer — a server that advertised a claim it could never land would be worse than no server")
}

// errServeEmbedded is the PERMANENT refusal. The message says what the
// workspace is and what serve needs, and promises nothing further: the reason
// is the embedded backend's commit protocol (see serveDatabaseSource), which no
// amount of provider or role plumbing changes.
//
// The mode belongs in the message, not in ErrUnsupported.Backend: that field is
// documented as a BACKEND name and is the embryo of the pluggable-backend error
// taxonomy, so putting a topology string in it would hand every downstream
// errors.As a mixed backend/mode vocabulary.
func errServeEmbedded() error {
	return fmt.Errorf("%w: bd serve requires a Dolt SQL server; this workspace uses embedded Dolt",
		&storage.ErrUnsupported{Op: "serve", Backend: "embedded-dolt"})
}

// serveRoleSource is the surface serveIssueRoles reaches on the store, spelled
// out rather than taken as a whole storage.DoltStorage.
//
// THE POINT IS THE TEST STUBS. A stub that stands in for a store has to satisfy
// whatever this function asks for, and the only affordable way to satisfy a
// hundred-method interface is to embed it and leave it nil — which answers every
// accessor the stub forgot with a promoted method on a nil interface. That is a
// segfault inside the loop below rather than a compile error, and it has landed
// twice: once on GraphCounter in serve_source_test.go, and once on the same role
// in serve_store_identity_test.go, where no local -run pattern named the test
// and only a full-package CI shard found it.
//
// Named narrowly, a stub can DECLARE this set and assert it, so the next role
// added to the loop below is a build failure in the file that has to grow a
// method — with the method's name in the error.
//
// A whole store is one of these, which is what the assertion beneath it pins:
// the production call site passes exactly what it always did.
type serveRoleSource interface {
	IssueReader() (issueops.Reader, error)
	IssueClaimer() (issueops.Claimer, error)
	BatchCloser() (issueops.BatchCloser, error)
	ReadyClaimer() (issueops.ReadyClaimer, error)
	Releaser() (issueops.Releaser, error)
	IssueLifecycle() (issueops.Lifecycle, error)
	WorkspaceConfig() (issueops.WorkspaceConfig, error)
	StatsReporter() (issueops.StatsReporter, error)
	CycleDetector() (issueops.CycleDetector, error)
	EdgeReader() (issueops.EdgeReader, error)
	GraphCounter() (issueops.GraphCounter, error)
	IssueRelations() (issueops.Relations, error)
	Commenter() (issueops.Commenter, error)
	BlockingAnnotator() (issueops.BlockingAnnotator, error)
	TreeWalker() (issueops.TreeWalker, error)
	ReadyCounter() (issueops.ReadyCounter, error)
	Counter() (issueops.Counter, error)
	Querier() (issueops.Querier, error)
	Sweeper() (issueops.Sweeper, error)
	Deleter() (issueops.Deleter, error)
	BatchCreator() (issueops.BatchCreator, error)
	DependencyEditor() (issueops.DependencyEditor, error)
	MetadataCAS() (issueops.MetadataCAS, error)
	BatchApplier() (issueops.BatchApplier, error)
	Memories() (memoryops.Memories, error)
}

var _ serveRoleSource = storage.DoltStorage(nil)

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/fs"
	"github.com/jonbaldie/beads/internal/storage/git"
	"github.com/jonbaldie/beads/internal/ui"
)

type initProxiedServerInput struct {
	prefix            string
	database          string
	roleFlag          string
	initRemote        string
	initRemoteChanged bool
	initProxiedServerPaths
	externalConfig *configfile.ExternalDoltConfig
	initProxiedServerModes
}

type initProxiedServerPaths struct {
	serverConfigPath       string
	serverLogPath          string
	serverRootPath         string
	serverProxyPort        int
	serverProxyIdleTimeout time.Duration
}

type initProxiedServerModes struct {
	quiet          bool
	stealth        bool
	skipHooks      bool
	skipAgents     bool
	contributor    bool
	team           bool
	teamServer     bool
	fromJSONL      bool
	nonInteractive bool
}

// Keep the grouped input fields visible to file-local unused-code analysis;
// the command assembles the value in init.go and consumes its promoted fields
// throughout this file.
var _ = initProxiedServerInput{
	prefix:            "",
	database:          "",
	roleFlag:          "",
	initRemote:        "",
	initRemoteChanged: false,
	initProxiedServerPaths: initProxiedServerPaths{
		serverConfigPath:       "",
		serverLogPath:          "",
		serverRootPath:         "",
		serverProxyPort:        0,
		serverProxyIdleTimeout: 0,
	},
	externalConfig: nil,
	initProxiedServerModes: initProxiedServerModes{
		quiet:          false,
		stealth:        false,
		skipHooks:      false,
		skipAgents:     false,
		contributor:    false,
		team:           false,
		teamServer:     false,
		fromJSONL:      false,
		nonInteractive: false,
	},
}

func runInitProxiedServer(cmd *cobra.Command, ctx context.Context, in initProxiedServerInput) error {
	runtime, err := prepareProxiedInit(ctx, &in)
	if err != nil {
		return err
	}
	defer func() { _ = runtime.provider.Close(ctx) }()

	identity, err := resolveProxiedInitIdentity(ctx, in, runtime)
	if err != nil {
		return err
	}
	if err := adoptProxiedInitProjectID(runtime.beadsDir, in.teamServer, runtime.projectID, identity.projectID); err != nil {
		return err
	}
	return runInitProxiedServerTail(cmd, ctx, in, runInitTailContext{
		beadsDir:      runtime.beadsDir,
		prefix:        identity.prefix,
		dbName:        runtime.dbName,
		useLocalBeads: runtime.useLocalBeads,
		remoteURL:     runtime.remoteURL,
		fsUseCase:     runtime.fsUseCase,
		gitUC:         runtime.gitUC,
	})
}

type proxiedInitWorkspace struct {
	beadsDir              string
	dbName                string
	projectID             string
	prefix                string
	useLocalBeads         bool
	writeProjectGitignore bool
	fsUseCase             domain.BeadsDirFSUseCase
	gitUC                 domain.GitUseCase
}

type proxiedInitIdentity struct {
	prefix    string
	projectID string
}

func prepareProxiedInit(ctx context.Context, in *initProxiedServerInput) (proxiedInitRuntime, error) {
	if err := validateProxiedInitInput(*in); err != nil {
		return proxiedInitRuntime{}, err
	}
	if err := preflightProxiedInitDolt(ctx, *in); err != nil {
		return proxiedInitRuntime{}, err
	}
	if err := config.Initialize(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to initialize config: %v\n", err)
	}
	if err := checkExistingBeadsData(in.prefix); err != nil {
		return proxiedInitRuntime{}, err
	}
	workspace, err := resolveProxiedInitWorkspace(ctx, in)
	if err != nil {
		return proxiedInitRuntime{}, err
	}
	if err := initializeProxiedInitBeadsDir(ctx, *in, workspace); err != nil {
		return proxiedInitRuntime{}, err
	}
	runtime, err := openProxiedInitRuntime(ctx, *in, workspace)
	if err != nil {
		return proxiedInitRuntime{}, err
	}
	return runtime, nil
}

func validateProxiedInitInput(in initProxiedServerInput) error {
	switch {
	case in.fromJSONL:
		return fmt.Errorf("--from-jsonl is not supported with --proxied-server")
	case in.contributor:
		return fmt.Errorf("--contributor is not supported with --proxied-server")
	case in.team:
		return fmt.Errorf("--team is not supported with --proxied-server")
	default:
		return nil
	}
}

func preflightProxiedInitDolt(ctx context.Context, in initProxiedServerInput) error {
	if in.externalConfig != nil {
		return nil
	}
	// Preflight the external dolt binary before any .beads/ write below. The
	// shared version warning is emitted at most once for this invocation.
	_, err := resolveAndProbeDolt(ctx, "bd init --proxied-server", in.quiet || isQuiet())
	return err
}

func resolveProxiedInitWorkspace(ctx context.Context, in *initProxiedServerInput) (proxiedInitWorkspace, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return proxiedInitWorkspace{}, fmt.Errorf("failed to get current directory: %v", err)
	}
	fsProvider := fs.NewFileSystemProvider(cwd, newBeadsDirTemplates(), newFileSystemAdapters())
	fsUseCase := fsProvider.BeadsDirFSUseCase()
	gitUC := git.NewGitProvider(cwd).GitUseCase()
	if err := setupProxiedInitStealth(ctx, *in, fsUseCase); err != nil {
		return proxiedInitWorkspace{}, err
	}
	if in.stealth {
		in.skipHooks = true
	}
	return resolveProxiedInitWorkspaceAt(ctx, *in, cwd, fsUseCase, gitUC)
}

func resolveProxiedInitWorkspaceAt(ctx context.Context, in initProxiedServerInput, cwd string, fsUseCase domain.BeadsDirFSUseCase, gitUC domain.GitUseCase) (proxiedInitWorkspace, error) {
	prefix, err := resolveInitPrefix(in.prefix)
	if err != nil {
		return proxiedInitWorkspace{}, err
	}
	proxiedInit, err := fsUseCase.ResolveProxiedInit(ctx, domain.ResolveProxiedInitParams{
		Prefix: prefix,
		DBFlag: in.database,
	})
	if err != nil {
		return proxiedInitWorkspace{}, fmt.Errorf("resolving proxied init: %v", err)
	}
	if err := validateProxiedInitWorkspace(cwd, in, proxiedInit); err != nil {
		return proxiedInitWorkspace{}, err
	}
	if err := ensureProxiedInitGit(ctx, in, gitUC, proxiedInit.HasExplicit); err != nil {
		return proxiedInitWorkspace{}, err
	}
	return proxiedInitWorkspace{
		beadsDir:              proxiedInit.BeadsDir,
		dbName:                proxiedInit.DBName,
		projectID:             proxiedInit.ProjectID,
		prefix:                prefix,
		useLocalBeads:         !proxiedInit.HasExplicit || proxiedInit.IsLocal,
		writeProjectGitignore: proxiedInit.IsLocal,
		fsUseCase:             fsUseCase,
		gitUC:                 gitUC,
	}, nil
}

func setupProxiedInitStealth(ctx context.Context, in initProxiedServerInput, fsUseCase domain.BeadsDirFSUseCase) error {
	if !in.stealth {
		return nil
	}
	if err := fsUseCase.SetupStealthMode(ctx, !in.quiet); err != nil {
		return fmt.Errorf("setting up stealth mode: %v", err)
	}
	return nil
}

func validateProxiedInitWorkspace(cwd string, in initProxiedServerInput, proxiedInit domain.ResolveProxiedInitResult) error {
	if in.teamServer && proxiedInit.DBNameDerived {
		return fmt.Errorf(
			"--team-server requires --database (or an existing .beads/metadata.json naming the database): bd cannot guess the name of the bts-provisioned database (guessed %q from the prefix)",
			proxiedInit.DBName)
	}
	cleanCWD := filepath.Clean(cwd)
	if strings.Contains(cleanCWD, string(filepath.Separator)+".beads"+string(filepath.Separator)) || strings.HasSuffix(cleanCWD, string(filepath.Separator)+".beads") {
		return fmt.Errorf("cannot initialize bd inside a .beads directory\nCurrent directory: %s", cwd)
	}
	return nil
}

func ensureProxiedInitGit(ctx context.Context, in initProxiedServerInput, gitUC domain.GitUseCase, hasExplicitBeadsDir bool) error {
	if hasExplicitBeadsDir {
		return nil
	}
	res, err := gitUC.EnsureGitRepo(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize git repository: %v", err)
	}
	if res.DidInit && !in.quiet {
		fmt.Printf("  %s Initialized git repository\n", ui.RenderPass("✓"))
	}
	return nil
}

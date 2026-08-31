package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/uow"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/workapi"
	"github.com/jonbaldie/beads/issueops"
)

type proxiedInitRuntime struct {
	proxiedInitWorkspace
	provider  uow.UnitOfWorkProvider
	verifier  issueops.InitVerifier
	remoteURL string
	repoID    string
	cloneID   string
}

func initializeProxiedInitBeadsDir(ctx context.Context, in initProxiedServerInput, workspace proxiedInitWorkspace) error {
	metadataBody, err := composeProxiedServerMetadataJSON(proxiedMetadataInputs{
		dbName:     workspace.dbName,
		projectID:  workspace.projectID,
		teamServer: in.teamServer,
	})
	if err != nil {
		return fmt.Errorf("composing metadata.json: %v", err)
	}
	clientInfo, err := buildProxiedServerClientInfo(in.serverRootPath, in.serverConfigPath, in.serverLogPath, in.serverProxyPort, in.serverProxyIdleTimeout, in.externalConfig)
	if err != nil {
		return err
	}
	params := domain.InitializeBeadsDirParams{
		MetadataJSONBody:        metadataBody,
		ConfigYAMLBody:          renderInitConfigYAML("", false),
		ProxiedServerClientInfo: clientInfo,
		SetNoCOW:                true,
		WriteProjectGitignore:   workspace.writeProjectGitignore,
	}
	if workspace.useLocalBeads {
		params.LocalVersion = Version
	}
	result, err := workspace.fsUseCase.InitializeBeadsDir(ctx, params)
	if err != nil {
		return fmt.Errorf("initializing .beads directory: %v", err)
	}
	warnProxiedInitFilesystemIssues(in, workspace.beadsDir, result)
	return nil
}

func warnProxiedInitFilesystemIssues(in initProxiedServerInput, beadsDir string, result domain.InitializeBeadsDirResult) {
	if in.quiet {
		return
	}
	if result.NoCOWErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to set FS_NOCOW_FL on %s: %v\n", beadsDir, result.NoCOWErr)
	}
	if result.LocalVersionErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to initialize version tracking: %v\n", result.LocalVersionErr)
	}
}

func openProxiedInitRuntime(ctx context.Context, in initProxiedServerInput, workspace proxiedInitWorkspace) (proxiedInitRuntime, error) {
	provider, err := newProxiedServerUOWProviderAdopting(ctx, workspace.beadsDir, "")
	if err != nil {
		return proxiedInitRuntime{}, fmt.Errorf("failed to open uow provider: %v", err)
	}
	verifier, err := proxiedInitVerifier(provider)
	if err != nil {
		_ = provider.Close(ctx)
		return proxiedInitRuntime{}, HandleError("%v", err)
	}
	repoID, cloneID := proxiedInitTrackingIDs(in)
	return proxiedInitRuntime{
		proxiedInitWorkspace: workspace,
		provider:             provider,
		verifier:             verifier,
		remoteURL:            resolveProxiedInitRemoteURL(ctx, workspace.gitUC, in),
		repoID:               repoID,
		cloneID:              cloneID,
	}, nil
}

func proxiedInitTrackingIDs(in initProxiedServerInput) (repoID, cloneID string) {
	if id, err := beads.ComputeRepoID(); err == nil {
		repoID = id
	} else if !in.quiet {
		fmt.Fprintf(os.Stderr, "Warning: could not compute repository ID: %v\n", err)
	}
	if id, err := beads.GetCloneID(); err == nil {
		cloneID = id
	} else if !in.quiet {
		fmt.Fprintf(os.Stderr, "Warning: could not compute clone ID: %v\n", err)
	}
	return repoID, cloneID
}

func resolveProxiedInitIdentity(ctx context.Context, in initProxiedServerInput, runtime proxiedInitRuntime) (proxiedInitIdentity, error) {
	if in.teamServer {
		prefix, projectID, err := adoptTeamServerIdentity(ctx, runtime.verifier, runtime.dbName, runtime.prefix, in.prefix != "", runtime.projectID)
		if err != nil {
			return proxiedInitIdentity{}, HandleError("%v", err)
		}
		return proxiedInitIdentity{prefix: prefix, projectID: projectID}, nil
	}
	identity, err := resolveNonTeamProxiedInitIdentity(ctx, in, runtime)
	if err != nil {
		return proxiedInitIdentity{}, err
	}
	// Tracking state is refreshed for every non-team init, whether the database
	// identity was bootstrapped here or adopted from an existing database.
	if err := recordProxiedInitTrackingState(ctx, runtime.provider, runtime.repoID, runtime.cloneID); err != nil {
		return proxiedInitIdentity{}, HandleError("%v", err)
	}
	if runtime.remoteURL != "" {
		if err := configureProxiedInitDoltRemote(ctx, runtime.provider, runtime.remoteURL); err != nil {
			return proxiedInitIdentity{}, HandleError("%v", err)
		}
	}
	return identity, nil
}

func resolveNonTeamProxiedInitIdentity(ctx context.Context, in initProxiedServerInput, runtime proxiedInitRuntime) (proxiedInitIdentity, error) {
	existing, err := runtime.verifier.VerifyIdentity(ctx, issueops.VerifyIdentityRequest{})
	if err != nil {
		return proxiedInitIdentity{}, HandleError("reading project identity from database %q: %v", runtime.dbName, err)
	}
	if existing.Prefix != "" || existing.ProjectID != "" {
		if !in.quiet {
			fmt.Printf("  %s Adopted project identity from existing database\n", ui.RenderPass("✓"))
		}
		return proxiedInitIdentity{prefix: existing.Prefix, projectID: existing.ProjectID}, nil
	}
	return bootstrapProxiedInitIdentity(ctx, runtime)
}

func bootstrapProxiedInitIdentity(ctx context.Context, runtime proxiedInitRuntime) (proxiedInitIdentity, error) {
	bootstrapper, err := proxiedBootstrapper(runtime.provider)
	if err != nil {
		return proxiedInitIdentity{}, HandleError("%v", err)
	}
	result, err := bootstrapper.Bootstrap(ctx, issueops.BootstrapRequest{
		Prefix:    runtime.prefix,
		ProjectID: runtime.projectID,
	})
	if err != nil {
		return proxiedInitIdentity{}, HandleError("bootstrap project: %v", err)
	}
	return proxiedInitIdentity{prefix: result.Prefix, projectID: result.ProjectID}, nil
}

func adoptProxiedInitProjectID(beadsDir string, teamServer bool, localProjectID, adoptedProjectID string) error {
	if !teamServer || adoptedProjectID == localProjectID {
		return nil
	}
	fileCfg, err := configfile.Load(beadsDir)
	if err != nil || fileCfg == nil {
		return HandleError("failed to reload %s to adopt the provisioned project identity: %v", configfile.ConfigFileName, err)
	}
	fileCfg.ProjectID = adoptedProjectID
	if err := fileCfg.Save(beadsDir); err != nil {
		return HandleError("failed to save the provisioned project identity to %s: %v", configfile.ConfigFileName, err)
	}
	return nil
}

func resolveInitPrefix(flagPrefix string) (string, error) {
	prefix, err := configuredInitPrefix(flagPrefix)
	if err != nil {
		return "", err
	}
	return normalizeInitPrefix(prefix), nil
}

func configuredInitPrefix(flagPrefix string) (string, error) {
	if flagPrefix != "" {
		return flagPrefix, nil
	}
	if prefix := config.GetString("issue-prefix"); prefix != "" {
		return prefix, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current directory: %v", err)
	}
	return filepath.Base(cwd), nil
}

func normalizeInitPrefix(prefix string) string {
	prefix = strings.TrimLeft(prefix, ".")
	prefix = strings.TrimRight(prefix, "-")
	prefix = strings.ReplaceAll(prefix, ".", "_")
	if initPrefixNeedsNamespace(prefix) {
		prefix = "bd_" + prefix
	}
	return prefix
}

func initPrefixNeedsNamespace(prefix string) bool {
	if len(prefix) == 0 {
		return false
	}
	first := prefix[0]
	return !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_')
}

func resolveProxiedInitRemoteURL(ctx context.Context, gitUC domain.GitUseCase, in initProxiedServerInput) string {
	url, source := resolveInitConfiguredSyncRemote(in.initRemote, in.initRemoteChanged, resolveSyncRemote)
	if url != "" {
		return url
	}
	if source != initSyncRemoteNone {
		return ""
	}
	if !in.stealth {
		if originURL, err := gitUC.OriginRemoteURL(ctx); err == nil && originURL != "" {
			return normalizeRemoteURL(originURL)
		}
	}
	return ""
}

// proxiedInitVerifier and proxiedBootstrapper hand back the two identity
// surfaces through the provider's own capability accessors.
//
// They are asked for SEPARATELY, and team-server mode is what that separation
// is for: it holds a verifier and never obtains a bootstrapper, so the path bd
// must not write on cannot reach the write.
func proxiedInitVerifier(provider uow.UnitOfWorkProvider) (issueops.InitVerifier, error) {
	src, ok := provider.(uow.InitVerifierSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the identity-read surface", provider)
	}
	return src.InitVerifier()
}

func proxiedBootstrapper(provider uow.UnitOfWorkProvider) (issueops.Bootstrapper, error) {
	src, ok := provider.(uow.BootstrapperSource)
	if !ok {
		return nil, fmt.Errorf("proxied-server provider %T does not offer the identity-seeding surface", provider)
	}
	return src.Bootstrapper()
}

// recordProxiedInitTrackingState seeds the per-clone bookkeeping: the
// repository and clone fingerprints, the synced-at marker and the recorded
// binary version.
//
// It is separate from the identity because its LIFETIME is: the identity is
// written once and then adopted forever, while these four describe the clone
// running init and are refreshed every time it runs. In the refusable one-time
// write, a re-init on a shared database would silently stop recording them.
func recordProxiedInitTrackingState(ctx context.Context, provider uow.UnitOfWorkProvider, repoID, cloneID string) error {
	return uow.RunTx(ctx, provider, func(ctx context.Context, uw uow.UnitOfWork) (string, error) {
		cfg := uw.ConfigUseCase()
		// An absent fingerprint is recorded as nothing rather than as "": an
		// empty row reads back to cross-project verification as a clone whose
		// fingerprint failed to compute.
		if repoID != "" {
			if err := cfg.SetMetadata(ctx, "repo_id", repoID); err != nil {
				return "", fmt.Errorf("record repo_id: %w", err)
			}
		}
		if cloneID != "" {
			if err := cfg.SetMetadata(ctx, "clone_id", cloneID); err != nil {
				return "", fmt.Errorf("record clone_id: %w", err)
			}
		}
		if err := cfg.SetMetadata(ctx, "last_import_time", time.Now().UTC().Format(time.RFC3339)); err != nil {
			return "", fmt.Errorf("record last_import_time: %w", err)
		}
		if err := cfg.SetLocalMetadata(ctx, workapi.MetadataKeyVersion, Version); err != nil {
			return "", fmt.Errorf("record bd_version: %w", err)
		}
		return "bd init", nil
	})
}

// configureProxiedInitDoltRemote adds the sync remote, skipping a name that is
// already taken.
func configureProxiedInitDoltRemote(ctx context.Context, provider uow.UnitOfWorkProvider, remoteURL string) error {
	return uow.RunTx(ctx, provider, func(ctx context.Context, uw uow.UnitOfWork) (string, error) {
		remotes, err := uw.DoltRemoteUseCase().ListRemotes(ctx)
		if err != nil {
			return "", fmt.Errorf("list remotes: %w", err)
		}
		for _, r := range remotes {
			if r.Name == "origin" {
				return "", nil
			}
		}
		if err := uw.DoltRemoteUseCase().CreateRemote(ctx, "origin", remoteURL); err != nil {
			return "", fmt.Errorf("create remote origin: %w", err)
		}
		return "", nil
	})
}

// adoptTeamServerIdentity reads the bts-provisioned identity out of the shared
// database, following the gateway contract: adopt if present, hard error if
// absent — bd never writes identity in team-server mode.
//
// ABSENT means "unprovisioned, tell them to run bts init" and UNREADABLE means
// "the connection failed, say so"; keeping those apart is the InitVerifier
// role's promise. The two markers arrive as ONE snapshot, so the prefix and the
// project id cannot come from either side of a concurrent write.
func adoptTeamServerIdentity(ctx context.Context, verifier issueops.InitVerifier, dbName, localPrefix string, prefixIsExplicit bool, localProjectID string) (prefix, projectID string, err error) {
	identity, readErr := verifier.VerifyIdentity(ctx, issueops.VerifyIdentityRequest{})
	if _, err := resolveInitIssuePrefix(true, identity.Prefix, dbName, localPrefix, readErr); err != nil {
		if readErr == nil {
			return "", "", fmt.Errorf(
				"database %q has no project identity (config.issue_prefix) — provision it with 'bts init' (or heal an older bts database with 'bts migrate')",
				dbName)
		}
		return "", "", err
	}
	// An explicit --prefix that disagrees must not be silently ignored; a
	// merely derived prefix adopts silently.
	if prefixIsExplicit && identity.Prefix != localPrefix {
		return "", "", fmt.Errorf(
			"--prefix %q conflicts with issue_prefix %q provisioned in database %q; omit --prefix to adopt the provisioned one",
			localPrefix, identity.Prefix, dbName)
	}

	adoptedID, _, err := resolveInitProjectID(true, localProjectID, identity.ProjectID, dbName, readErr)
	if err != nil {
		if readErr == nil {
			return "", "", fmt.Errorf(
				"database %q has no project identity (metadata._project_id) — provision it with 'bts init' (or heal an older bts database with 'bts migrate')",
				dbName)
		}
		return "", "", err
	}
	return identity.Prefix, adoptedID, nil
}

type proxiedMetadataInputs struct {
	dbName     string
	projectID  string
	teamServer bool
}

func composeProxiedServerMetadataJSON(in proxiedMetadataInputs) ([]byte, error) {
	cfg := configfile.DefaultConfig()
	cfg.Backend = configfile.BackendDolt
	cfg.Database = "dolt"
	cfg.DoltDatabase = in.dbName
	cfg.DoltMode = configfile.DoltModeProxiedServer
	cfg.ProjectID = in.projectID
	cfg.DoltTeamServer = in.teamServer

	if filepath.IsAbs(cfg.DoltDataDir) {
		cfg.DoltDataDir = ""
	}

	return json.MarshalIndent(cfg, "", "  ")
}

func buildProxiedServerClientInfo(rootPath, configPath, logPath string, port int, idleTimeout time.Duration, external *configfile.ExternalDoltConfig) (*configfile.ProxiedServerClientInfo, error) {
	if !hasProxiedServerClientInfo(rootPath, configPath, logPath, port, idleTimeout, external) {
		return nil, nil
	}
	paths, err := cleanProxiedServerClientPaths(rootPath, configPath, logPath)
	if err != nil {
		return nil, err
	}
	if err := validateExternalProxiedServerConfig(external); err != nil {
		return nil, err
	}
	return &configfile.ProxiedServerClientInfo{
		RootPath:    paths.root,
		ConfigPath:  paths.config,
		LogPath:     paths.log,
		Port:        port,
		IdleTimeout: idleTimeout,
		External:    external,
	}, nil
}

func hasProxiedServerClientInfo(rootPath, configPath, logPath string, port int, idleTimeout time.Duration, external *configfile.ExternalDoltConfig) bool {
	return rootPath != "" || configPath != "" || logPath != "" || port != 0 || idleTimeout != 0 || external != nil
}

type proxiedServerClientPaths struct {
	root   string
	config string
	log    string
}

func cleanProxiedServerClientPaths(rootPath, configPath, logPath string) (proxiedServerClientPaths, error) {
	root, err := cleanProxiedServerClientPath(rootPath)
	if err != nil {
		return proxiedServerClientPaths{}, err
	}
	config, err := cleanProxiedServerClientPath(configPath)
	if err != nil {
		return proxiedServerClientPaths{}, err
	}
	log, err := cleanProxiedServerClientPath(logPath)
	if err != nil {
		return proxiedServerClientPaths{}, err
	}
	return proxiedServerClientPaths{root: root, config: config, log: log}, nil
}

func cleanProxiedServerClientPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("buildProxiedServerClientInfo: path %q is not absolute", path)
	}
	return filepath.Clean(path), nil
}

func validateExternalProxiedServerConfig(external *configfile.ExternalDoltConfig) error {
	if external == nil {
		return nil
	}
	if err := external.Validate(); err != nil {
		return fmt.Errorf("buildProxiedServerClientInfo: %w", err)
	}
	return nil
}

type runInitTailContext struct {
	beadsDir      string
	prefix        string
	dbName        string
	useLocalBeads bool
	remoteURL     string
	fsUseCase     domain.BeadsDirFSUseCase
	gitUC         domain.GitUseCase
}

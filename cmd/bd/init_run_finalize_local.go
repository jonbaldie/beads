package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/issueops"
)

type initIdentityState struct {
	dbIdentity         issueops.VerifyIdentityResult
	identityReadErr    error
	issuePrefix        string
	bootstrapProjectID string
}

func loadInitWorkspaceIdentity(args initFinalizeArgs) (*initIdentityState, error) {
	// Configuration metadata is essential for core functionality and must succeed.
	initVerifier, err := args.store.InitVerifier()
	if err != nil {
		_ = args.store.Close()
		return nil, fmt.Errorf("failed to reach the workspace identity: %v", err)
	}
	ident := &initIdentityState{}
	ident.dbIdentity, ident.identityReadErr = initVerifier.VerifyIdentity(args.ctx, issueops.VerifyIdentityRequest{})
	ident.issuePrefix, err = resolveInitIssuePrefix(args.doltCfg.Gateway, ident.dbIdentity.Prefix, args.ident.dbName, args.ident.prefix, ident.identityReadErr)
	if err != nil {
		_ = args.store.Close()
		return nil, err
	}
	return ident, nil
}

func writeInitLocalWorkspaceFiles(args initFinalizeArgs, ident *initIdentityState) error {
	if !args.ident.useLocalBeads {
		return nil
	}
	cfg, existingCfg, err := loadInitMetadataConfig(args.ident.beadsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load existing metadata.json: %v\n", err)
	}
	if err := applyInitProjectIdentity(args, ident, cfg); err != nil {
		return err
	}
	applyInitBackendMetadata(args, cfg, existingCfg)
	return persistInitLocalConfigFiles(args, ident, cfg)
}

func loadInitMetadataConfig(beadsDir string) (*configfile.Config, *configfile.Config, error) {
	existingCfg, err := configfile.Load(beadsDir)
	if existingCfg != nil {
		return existingCfg, existingCfg, err
	}
	return configfile.DefaultConfig(), existingCfg, err
}

func applyInitProjectIdentity(args initFinalizeArgs, ident *initIdentityState, cfg *configfile.Config) error {
	adoptedFromDB := ""
	if args.store != nil && shouldConsultInitProjectID(args.doltCfg.Gateway, cfg.ProjectID, args.ident.database, args.mode.bootstrappedFromRemote) {
		adoptedFromDB = ident.dbIdentity.ProjectID
	}
	localID := cfg.ProjectID
	resolvedID, identityChanged, err := resolveInitProjectID(args.doltCfg.Gateway, localID, adoptedFromDB, args.ident.dbName, ident.identityReadErr)
	if err != nil {
		_ = args.store.Close()
		return err
	}
	printInitProjectIdentityChange(args.mode.quiet, identityChanged, adoptedFromDB, localID)
	cfg.ProjectID = resolvedID
	return nil
}

func printInitProjectIdentityChange(quiet, identityChanged bool, adoptedFromDB, localID string) {
	if !identityChanged || adoptedFromDB == "" || quiet {
		return
	}
	if localID == "" {
		fmt.Printf("  %s Adopted project identity from existing database\n", ui.RenderPass("✓"))
		return
	}
	fmt.Printf("  %s Reconciled local project identity with hosted database\n", ui.RenderPass("✓"))
}

func applyInitBackendMetadata(args initFinalizeArgs, cfg *configfile.Config, existingCfg *configfile.Config) {
	cfg.Backend = args.ident.backend
	if args.ident.backend != configfile.BackendDolt {
		return
	}
	applyInitDoltDatabaseName(args, cfg)
	applyInitSharedDatabaseNames(args, cfg)
	applyInitDoltMode(args, cfg, existingCfg)
	applyInitDoltServerConn(args, cfg)
}

func applyInitDoltDatabaseName(args initFinalizeArgs, cfg *configfile.Config) {
	if cfg.Database == "" || cfg.Database == beads.CanonicalDatabaseName {
		cfg.Database = "dolt"
	}
	if args.ident.database != "" {
		cfg.DoltDatabase = args.ident.database
		return
	}
	if cfg.DoltDatabase == "" && args.ident.prefix != "" {
		cfg.DoltDatabase = strings.ReplaceAll(args.ident.prefix, "-", "_")
	}
}

func applyInitSharedDatabaseNames(args initFinalizeArgs, cfg *configfile.Config) {
	if args.mode.sharedServer || doltserver.IsSharedServerMode() {
		cfg.GlobalDoltDatabase = doltserver.GlobalDatabaseName
		cfg.GlobalProjectID = doltserver.GlobalProjectID
	}
}

func applyInitDoltMode(args initFinalizeArgs, cfg *configfile.Config, existingCfg *configfile.Config) {
	priorMode := ""
	if existingCfg != nil {
		priorMode = strings.ToLower(strings.TrimSpace(existingCfg.DoltMode))
	}
	switch {
	case usesProxiedServer():
		cfg.DoltMode = configfile.DoltModeProxiedServer
	case usesSQLServer():
		cfg.DoltMode = configfile.DoltModeServer
	default:
		cfg.DoltMode = configfile.DoltModeEmbedded
	}
	if priorMode != "" && priorMode != cfg.DoltMode && !args.mode.quiet {
		fmt.Fprintf(os.Stderr,
			"Connection mode changed: %s -> %s (recorded in .beads/metadata.json).\n",
			priorMode, cfg.DoltMode)
	}
}

func applyInitDoltServerConn(args initFinalizeArgs, cfg *configfile.Config) {
	if usesProxiedServer() {
		return
	}
	if args.ident.serverHost != "" {
		cfg.DoltServerHost = args.ident.serverHost
	}
	if args.ident.serverPort != 0 {
		cfg.DoltServerPort = args.ident.serverPort
	}
	if args.ident.serverSocket != "" {
		cfg.DoltServerSocket = args.ident.serverSocket
	}
	if args.ident.serverUser != "" {
		cfg.DoltServerUser = args.ident.serverUser
	}
}

func persistInitLocalConfigFiles(args initFinalizeArgs, ident *initIdentityState, cfg *configfile.Config) error {
	beadsDir := args.ident.beadsDir
	if err := cfg.Save(beadsDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create metadata.json: %v\n", err)
	}
	ident.bootstrapProjectID = cfg.ProjectID
	if err := createConfigYaml(beadsDir, false, ""); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create config.yaml: %v\n", err)
	}
	if err := persistInitSyncRemote(beadsDir, args.remote.initRemote, args.remote.syncURL, args.remote.syncFromRemote, args.remote.syncURLFromConfig, args.remote.syncURLFromGitOrigin); err != nil {
		return fmt.Errorf("failed to persist sync.remote to config.yaml: %v", err)
	}
	persistInitSharedAndDebug(args)
	persistInitStealthAndReadme(args)
	return nil
}

func persistInitSharedAndDebug(args initFinalizeArgs) {
	if args.mode.sharedServer || doltserver.IsSharedServerMode() {
		if err := config.SetYamlConfig("dolt.shared-server", "true"); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to enable shared server mode: %v\n", err)
		} else if !args.mode.quiet {
			fmt.Printf("  %s Shared server mode enabled\n", ui.RenderPass("✓"))
		}
	}
	if !args.mode.debugMode {
		return
	}
	if err := config.SetYamlConfig("dolt.debug", "true"); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to persist dolt.debug: %v\n", err)
		return
	}
	if args.mode.quiet {
		return
	}
	serverDir := doltserver.ResolveServerDir(args.ident.beadsDir)
	fmt.Printf("  %s Debug mode enabled\n", ui.RenderPass("✓"))
	fmt.Printf("      Server log:  %s\n", doltserver.LogPath(serverDir))
	fmt.Printf("      Profile dir: %s\n", doltserver.DebugProfileDir(args.ident.beadsDir))
	fmt.Printf("      Note: cpu.pprof is written when the server exits cleanly (bd dolt stop).\n")
}

func persistInitStealthAndReadme(args initFinalizeArgs) {
	if args.mode.stealth {
		if err := config.SaveConfigValue("no-git-ops", true, args.ident.beadsDir); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to set no-git-ops in config: %v\n", err)
		}
	}
	if err := createReadme(args.ident.beadsDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create README.md: %v\n", err)
	}
}

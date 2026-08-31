package main

import (
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	dbidentifier "github.com/jonbaldie/beads/internal/storage/domain/db"
	"github.com/jonbaldie/beads/internal/storage/schema"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"
)

func loadPersistentStoreConfig(beadsDir string) (*configfile.Config, error) {
	cfg, cfgErr := configfile.Load(beadsDir)
	if cfgErr != nil {
		return nil, HandleError("failed to load beads config from %s: %v (refusing to fall back to the embedded store; fix or restore metadata.json and retry)", beadsDir, cfgErr)
	}
	if backendErr := validateConfiguredBackend(cfg); backendErr != nil {
		return nil, HandleError("%v", backendErr)
	}
	if isReadonlyMode() && !backendSupportsStrictReadonly(cfg) {
		return nil, HandleError("strict readonly is unavailable for dolt proxied-server backend; refusing to open a store that cannot guarantee mutation-free access")
	}
	return cfg, nil
}

func applyPersistentStoreActorAndPolicy(cmd *cobra.Command) (bool, bool, rootStorePolicy) {
	setActor(getActorWithGit())
	if span := currentCommandSpan(); span != nil {
		span.SetAttributes(attribute.String("bd.actor", getActor()))
	}
	previewMode := isPreviewCommand(cmd)
	policy := effectiveRootStorePolicy(cmd.Name(), isReadonlyMode())
	useReadOnly := policy.readOnly || previewMode
	if policy.runMaintenance {
		if previewMode {
			trackBdVersionPreview()
		} else {
			trackBdVersion()
		}
	}
	return previewMode, useReadOnly, policy
}

func applyPersistentMigrateGates(cmd *cobra.Command, beadsDir string, policy rootStorePolicy, previewMode bool) error {
	forcedMigrate := isForcedMigrate(cmd)
	if forcedMigrate {
		if name := forcedMigratePreviewFlag(cmd); name != "" {
			return HandleError("--force cannot be combined with --%s: opening the store with the gate overridden applies pending migrations before the preview runs", name)
		}
	}
	schema.SetForceAllowRemoteMigrate(forcedMigrate)
	if policy.runMaintenance && !previewMode {
		autoMigrateOnVersionBump(beadsDir)
	}
	return nil
}

func newPersistentDoltConfig(cmd *cobra.Command, beadsDir string, useReadOnly, previewMode bool, policy rootStorePolicy) (*dolt.Config, string) {
	doltPath := doltserver.ResolveDoltDir(beadsDir)
	doltCfg := &dolt.Config{
		ReadOnly: useReadOnly,
		Preview:  previewMode,
		ServerOptions: dolt.ServerOptions{
			DisableAutoStart: policy.disableAutoStart,
		},
		BeadsDir:    beadsDir,
		LenientOpen: isWorkingSetReconcileCommand(cmd),
	}
	return doltCfg, doltPath
}

func applyPersistentDoltMetadata(beadsDir string, cfg *configfile.Config, doltCfg *dolt.Config) (*configfile.Config, error) {
	cfg, err := applyPersistentDoltConfigSource(beadsDir, cfg, doltCfg)
	if err != nil {
		return cfg, err
	}
	applyPersistentSharedServerFallback(doltCfg)
	if doltCfg.Database == "" {
		doltCfg.Database = configfile.DefaultDoltDatabase
	}
	doltCfg.SyncRemote = resolveSyncRemote()
	return cfg, nil
}

func applyPersistentDoltConfigSource(beadsDir string, cfg *configfile.Config, doltCfg *dolt.Config) (*configfile.Config, error) {
	if cfg == nil && configfile.DefaultConfig().HostImpliesServerMode() {
		logConfigDiscovery(beadsDir, "no metadata.json; host inference (GH#3545) selects server mode")
		cfg = configfile.DefaultConfig()
	}
	if cfg != nil {
		return cfg, applyPersistentKnownConfig(beadsDir, cfg, doltCfg)
	}
	logConfigDiscovery(beadsDir, "config discovery")
	fmt.Fprintf(os.Stderr, "warning: no beads configuration found in %s; using default database name %q\n", beadsDir, configfile.DefaultDoltDatabase)
	doltCfg.Database = configfile.DefaultDoltDatabase
	return cfg, nil
}

func applyPersistentSharedServerFallback(doltCfg *dolt.Config) {
	if !doltCfg.ServerMode && !doltCfg.ProxiedServer && doltserver.IsSharedServerMode() {
		doltCfg.ServerMode = true
		setServerMode(doltCfg.ServerMode)
	}
}

func applyPersistentKnownConfig(beadsDir string, cfg *configfile.Config, doltCfg *dolt.Config) error {
	warnSharedServerEmbeddedMismatch(cfg)
	doltCfg.ProxiedServer = cfg.IsDoltProxiedServerMode()
	setProxiedServerMode(doltCfg.ProxiedServer)
	doltCfg.ServerMode = cfg.IsDoltServerMode()
	if !doltCfg.ServerMode && !doltCfg.ProxiedServer && doltserver.IsSharedServerMode() {
		doltCfg.ServerMode = true
	}
	setServerMode(doltCfg.ServerMode)
	doltCfg.Database = cfg.GetDoltDatabase()
	if shouldLogDefaultDoltDatabase(cfg) {
		logConfigDiscovery(beadsDir, fmt.Sprintf("metadata loaded without dolt_database; using default database name %q", configfile.DefaultDoltDatabase))
	}
	if err := resolveDoltServerConnection(getRootContext(), beadsDir, cfg, doltCfg); err != nil {
		return HandleError("%v", err)
	}
	return nil
}

func applyPersistentDatabaseOverride(beadsDir, dbNameFromDBFlag string, doltCfg *dolt.Config) (string, error) {
	if isGlobalFlag() {
		if !doltserver.IsSharedServerMode() {
			return "", HandleError("--global requires shared-server mode (set BEADS_DOLT_SHARED_SERVER=1 or dolt.shared-server: true in config.yaml)")
		}
		doltCfg.Database = doltserver.GlobalDatabaseName
	}
	dolt.ApplyCLIAutoStart(beadsDir, doltCfg)
	databaseOverride := getDatabaseFlag()
	if dbNameFromDBFlag != "" {
		if databaseOverride != "" && databaseOverride != dbNameFromDBFlag {
			return "", HandleError("conflicting database selection: --db=%q vs --database=%q", dbNameFromDBFlag, databaseOverride)
		}
		databaseOverride = dbNameFromDBFlag
	}
	if databaseOverride != "" {
		if !isProxiedServerMode() {
			return "", HandleErrorRespectJSON("--database (or a --db value naming a database) is only supported in proxied-server mode")
		}
		if err := dbidentifier.ValidateIdentifier(databaseOverride); err != nil {
			return "", HandleErrorRespectJSON("%v", err)
		}
	}
	return databaseOverride, nil
}

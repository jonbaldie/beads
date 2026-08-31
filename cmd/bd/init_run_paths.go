package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jonbaldie/beads/cmd/bd/doctor"
	"github.com/jonbaldie/beads/internal/beads"
	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/configfile"
	"github.com/jonbaldie/beads/internal/doltserver"
	"github.com/jonbaldie/beads/internal/git"
	"github.com/jonbaldie/beads/internal/storage/dolt"
	"github.com/jonbaldie/beads/internal/ui"
	"github.com/jonbaldie/beads/internal/utils"
)

type initRunResolvedPaths struct {
	beadsDir, cwd, initDBPath, storagePath, dbName string
	hasExplicitBeadsDir, useLocalBeads             bool
}

func resolveInitPaths(st *initRunContext) error {
	st.resolved.paths.beadsDir = resolveInitBeadsDirForInit()
	st.resolved.paths.initDBPath = getDBPath()
	if st.resolved.paths.initDBPath == "" {
		st.resolved.paths.initDBPath = doltserver.ResolveDoltDir(st.resolved.paths.beadsDir)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %v", err)
	}
	st.resolved.paths.cwd = cwd
	st.resolved.paths.storagePath = doltserver.ResolveDoltDir(st.resolved.paths.beadsDir)
	st.resolved.paths.hasExplicitBeadsDir = os.Getenv("BEADS_DIR") != ""
	if st.proxied.enabled && st.resolved.externalConfig == nil {
		if err := validateManagedProxiedServerConfigAtInit(st.resolved.paths.beadsDir, st.proxied.paths.serverConfigPath, st.proxied.paths.serverRootPath); err != nil {
			return fmt.Errorf("managed proxied-server config: %w", err)
		}
	}
	return rejectInitInsideBeadsDir(cwd)
}

func resolveInitDatabaseName(st *initRunContext) error {
	beadsDir := st.resolved.paths.beadsDir
	dbName := ""
	if existingCfg, _ := configfile.Load(beadsDir); existingCfg != nil && existingCfg.DoltDatabase != "" {
		dbName = existingCfg.DoltDatabase
	} else if st.ident.prefix != "" {
		dbName = dbNameFromPrefix(st.ident.prefix)
	} else {
		dbName = "beads"
	}
	if st.ident.database != "" {
		dbName = st.ident.database
	}
	st.resolved.paths.dbName = dbName
	return validateInitDerivedDatabaseName(st)
}

func validateInitDerivedDatabaseName(st *initRunContext) error {
	if st.ident.database != "" {
		return nil
	}
	existingCfg, _ := configfile.Load(st.resolved.paths.beadsDir)
	if existingCfg != nil && existingCfg.DoltDatabase != "" {
		return nil
	}
	if err := dolt.ValidateDatabaseName(st.resolved.paths.dbName); err != nil {
		dirName := filepath.Base(st.resolved.paths.cwd)
		fmt.Fprintf(os.Stderr, "Error: directory name %q produces an invalid database name %q.\n", dirName, st.resolved.paths.dbName)
		fmt.Fprintf(os.Stderr, "Re-run with a valid prefix: bd init --prefix <name>\n")
		fmt.Fprintf(os.Stderr, "(Database names must start with a letter or underscore and contain only letters, digits, underscores, or hyphens.)\n")
		return &exitError{Code: 1}
	}
	return nil
}

func resolveInitBeadsDirForInit() string {
	if envBeadsDir := os.Getenv("BEADS_DIR"); envBeadsDir != "" {
		return utils.CanonicalizePath(envBeadsDir)
	}
	if beadsDir := beads.GetWorktreeFallbackBeadsDir(); beadsDir != "" {
		return beadsDir
	}
	return beads.FollowRedirect(filepath.Join(".", ".beads"))
}

func rejectInitInsideBeadsDir(cwd string) error {
	cleaned := filepath.Clean(cwd)
	sepBeads := string(filepath.Separator) + ".beads"
	if strings.Contains(cleaned, sepBeads+string(filepath.Separator)) || strings.HasSuffix(cleaned, sepBeads) {
		fmt.Fprintf(os.Stderr, "Error: cannot initialize bd inside a .beads directory\n")
		fmt.Fprintf(os.Stderr, "Current directory: %s\n", cwd)
		fmt.Fprintf(os.Stderr, "Please run 'bd init' from outside the .beads directory.\n")
		return &exitError{Code: 1}
	}
	return nil
}

func acquireInitWorkspaceGate(st *initRunContext) error {
	beadsDirAbs := absPathOrClean(st.resolved.paths.beadsDir)
	initDBPathAbs := absPathOrClean(st.resolved.paths.initDBPath)
	if fi, statErr := os.Stat(initDBPathAbs); statErr == nil && !fi.IsDir() {
		initDBPathAbs = filepath.Dir(initDBPathAbs)
	}
	handle, err := acquireExclusiveWorkspaceGates(getRootContext(), beadsDirAbs, "bd init", initDBPathAbs)
	if err != nil {
		return fmt.Errorf("bd init refuses to run over live bd activity on this workspace: %w", err)
	}
	st.gate = handle
	initDBDirAbs := absPathOrClean(filepath.Dir(st.resolved.paths.initDBPath))
	st.resolved.paths.useLocalBeads = !st.resolved.paths.hasExplicitBeadsDir || filepath.Clean(initDBDirAbs) == filepath.Clean(beadsDirAbs)
	return nil
}

func absPathOrClean(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

func createInitBeadsDir(st *initRunContext) error {
	if !st.resolved.paths.useLocalBeads {
		return nil
	}
	beadsDir := st.resolved.paths.beadsDir
	if err := mkdirInitBeadsDir(beadsDir); err != nil {
		return err
	}
	fixInitBeadsPermissions(beadsDir, st.safety.quiet)
	if err := applyNoCOW(beadsDir); err != nil && !st.safety.quiet {
		fmt.Fprintf(os.Stderr, "Warning: failed to set FS_NOCOW_FL on %s: %v\n", beadsDir, err)
	}
	if err := doctor.EnsureGitignoreForBeadsDir(beadsDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create/update .gitignore: %v\n", err)
	}
	return updateInitProjectGitignore(st)
}

func mkdirInitBeadsDir(beadsDir string) error {
	if err := os.MkdirAll(beadsDir, config.BeadsDirPerm); err != nil {
		if os.IsPermission(err) {
			return initBeadsPermissionError(beadsDir, err)
		}
		return fmt.Errorf("failed to create .beads directory: %v", err)
	}
	return nil
}

func initBeadsPermissionError(beadsDir string, err error) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("failed to create .beads directory: %v\n\n"+
			"Windows Controlled Folder Access may be blocking bd.exe.\n"+
			"To fix: Open Windows Security > Virus & threat protection >\n"+
			"Ransomware protection > Allow an app through Controlled folder access\n"+
			"and add bd.exe (typically %%USERPROFILE%%\\go\\bin\\bd.exe).", err)
	}
	return fmt.Errorf("failed to create .beads directory: %v\n\n"+
		"Permission denied. Check directory ownership and permissions:\n"+
		"  ls -la %s\n"+
		"  chmod 755 %s", err, filepath.Dir(beadsDir), filepath.Dir(beadsDir))
}

func fixInitBeadsPermissions(beadsDir string, quiet bool) {
	fixed, err := config.FixBeadsDirPermissions(beadsDir)
	if err != nil {
		if !quiet {
			fmt.Fprintf(os.Stderr, "Warning: could not fix .beads permissions: %v\n", err)
		}
		return
	}
	if fixed && !quiet {
		fmt.Fprintf(os.Stderr, "Fixed .beads permissions to %04o\n", config.BeadsDirPerm)
	}
}

func updateInitProjectGitignore(st *initRunContext) error {
	cwdAbs, _ := filepath.Abs(st.resolved.paths.cwd)
	beadsDirAbs := absPathOrClean(st.resolved.paths.beadsDir)
	beadsDirIsLocal := strings.HasPrefix(beadsDirAbs, filepath.Clean(cwdAbs)+string(filepath.Separator))
	if !beadsDirIsLocal {
		return nil
	}
	if st.safety.stealth {
		return updateInitStealthGitignore(st)
	}
	if err := doctor.EnsureProjectGitignore(st.resolved.paths.cwd); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to update project .gitignore: %v\n", err)
	}
	return nil
}

func updateInitStealthGitignore(st *initRunContext) error {
	cwd := st.resolved.paths.cwd
	if err := addProjectPatternsToGitExclude(cwd, doctor.ProjectGitignorePatterns, !st.safety.quiet); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to update git exclude: %v\n", err)
	}
	removed, err := removeBeadsProjectGitignoreSection(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to clean project .gitignore: %v\n", err)
		return nil
	}
	if removed && !st.safety.quiet {
		fmt.Printf("  %s Removed leaked beads section from tracked .gitignore\n", ui.RenderPass("✓"))
	}
	return nil
}

func ensureInitGitRepo(st *initRunContext) error {
	if isGitRepo() || st.resolved.paths.hasExplicitBeadsDir {
		return nil
	}
	gitInitCmd := exec.Command("git", "init")
	output, err := gitInitCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to initialize git repository: %v\n%s", err, output)
	}
	git.ResetCaches()
	if !st.safety.quiet {
		fmt.Printf("  %s Initialized git repository\n", ui.RenderPass("✓"))
	}
	return nil
}

func ensureInitStorageDir(st *initRunContext) error {
	if !st.server.initServerMode {
		return nil
	}
	initDBPath := st.resolved.paths.initDBPath
	if err := os.MkdirAll(initDBPath, config.BeadsDirPerm); err != nil {
		return fmt.Errorf("failed to create storage directory %s: %v", initDBPath, err)
	}
	if err := applyNoCOW(initDBPath); err != nil && !st.safety.quiet {
		fmt.Fprintf(os.Stderr, "Warning: failed to set FS_NOCOW_FL on %s: %v\n", initDBPath, err)
	}
	return nil
}

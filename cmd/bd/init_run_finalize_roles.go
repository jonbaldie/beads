package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonbaldie/beads/internal/doltserver"
)

func seedInitIdentityAndJSONL(args initFinalizeArgs, ident *initIdentityState) error {
	if shouldWriteInitStateToDB(args.doltCfg.Gateway) {
		if err := seedInitWorkspaceIdentity(args.ctx, args.store, ident.dbIdentity, ident.issuePrefix, ident.bootstrapProjectID, args.ident.beadsDir); err != nil {
			_ = args.store.Close()
			return err
		}
	}
	if shouldWriteInitStateToDB(args.doltCfg.Gateway) {
		if err := args.store.SetMetadata(args.ctx, "last_import_time", time.Now().Format(time.RFC3339)); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to initialize last_import_time: %v\n", err)
		}
	}
	return importInitFromJSONL(args)
}

func importInitFromJSONL(args initFinalizeArgs) error {
	if !args.mode.fromJSONL {
		return nil
	}
	localJSONLPath := configuredImportJSONLPath(args.ident.beadsDir)
	if _, statErr := os.Stat(localJSONLPath); os.IsNotExist(statErr) {
		_ = args.store.Close()
		return fmt.Errorf("--from-jsonl specified but %s does not exist", localJSONLPath)
	}
	issueCount, importErr := importFromLocalJSONL(args.ctx, args.store, localJSONLPath)
	if importErr != nil {
		_ = args.store.Close()
		return fmt.Errorf("failed to import from JSONL: %v", importErr)
	}
	if !args.mode.quiet {
		fmt.Printf("  Imported %d issues from %s\n", issueCount, localJSONLPath)
	}
	return nil
}

func runInitRoleSetup(args *initFinalizeArgs) error {
	if err := promptInitContributorRole(args); err != nil {
		return err
	}
	if err := runInitContributorWizard(args); err != nil {
		return err
	}
	if err := runInitTeamWizard(args); err != nil {
		return err
	}
	ensureInitBeadsRole(args)
	autoConfigureInitFork(args)
	commitInitDoltState(*args)
	return nil
}

func promptInitContributorRole(args *initFinalizeArgs) error {
	if shouldPromptInitContributor(args.mode, args.ident.roleFlag) {
		return promptInitContributorInteractive(args)
	}
	if shouldAssignInitDefaultRole(args.mode) {
		assignInitDefaultRole(args)
	}
	return nil
}

func shouldPromptInitContributor(mode initFinalizeMode, roleFlag string) bool {
	return isGitRepo() && !mode.contributor && !mode.team && roleFlag == "" && !mode.nonInteractive && shouldPromptForRole()
}

func shouldAssignInitDefaultRole(mode initFinalizeMode) bool {
	return isGitRepo() && !mode.contributor && !mode.team
}

func promptInitContributorInteractive(args *initFinalizeArgs) error {
	promptedContributor, err := promptContributorMode()
	if err != nil {
		if isCanceled(err) {
			fmt.Fprintln(os.Stderr, "Setup canceled.")
			_ = args.store.Close()
			return errCanceled()
		}
		if !args.mode.quiet {
			fmt.Fprintf(os.Stderr, "Warning: failed to prompt for role: %v\n", err)
		}
		return nil
	}
	if promptedContributor {
		args.mode.contributor = true
	}
	return nil
}

func assignInitDefaultRole(args *initFinalizeArgs) {
	role := args.ident.roleFlag
	if role == "" {
		role = "maintainer"
	}
	if _, hasRole := getBeadsRole(); !hasRole {
		if err := setBeadsRole(role); err != nil && !args.mode.quiet {
			fmt.Fprintf(os.Stderr, "Warning: failed to set default beads.role: %v\n", err)
		}
		return
	}
	if args.ident.roleFlag == "" {
		return
	}
	if err := setBeadsRole(role); err != nil && !args.mode.quiet {
		fmt.Fprintf(os.Stderr, "Warning: failed to set beads.role: %v\n", err)
	}
}

func runInitContributorWizard(args *initFinalizeArgs) error {
	if !args.mode.contributor {
		return nil
	}
	planningRepo, _ := args.cmd.Flags().GetString("planning-repo")
	if err := runContributorWizard(args.ctx, args.store, contributorWizardOpts{
		NonInteractive: args.mode.nonInteractive,
		PlanningRepo:   planningRepo,
		Quiet:          args.mode.quiet,
	}); err != nil {
		return closeInitCanceled(args, err, "running contributor wizard")
	}
	if isGitRepo() {
		if err := setBeadsRole("contributor"); err != nil && !args.mode.quiet {
			fmt.Fprintf(os.Stderr, "Warning: failed to set beads.role=contributor: %v\n", err)
		}
	}
	return nil
}

func runInitTeamWizard(args *initFinalizeArgs) error {
	if !args.mode.team {
		return nil
	}
	if err := runTeamWizard(args.ctx, args.store); err != nil {
		return closeInitCanceled(args, err, "running team wizard")
	}
	return nil
}

func closeInitCanceled(args *initFinalizeArgs, err error, action string) error {
	canceled := isCanceled(err)
	if canceled {
		fmt.Fprintln(os.Stderr, "Setup canceled.")
	}
	_ = args.store.Close()
	if canceled {
		return errCanceled()
	}
	return fmt.Errorf("%s: %v", action, err)
}

func ensureInitBeadsRole(args *initFinalizeArgs) {
	if !isGitRepo() {
		return
	}
	if _, hasRole := getBeadsRole(); hasRole {
		return
	}
	fallbackRole := "maintainer"
	if args.ident.roleFlag != "" {
		fallbackRole = args.ident.roleFlag
	}
	if err := setBeadsRole(fallbackRole); err != nil && !args.mode.quiet {
		fmt.Fprintf(os.Stderr, "Warning: failed to set beads.role=%s: %v\n", fallbackRole, err)
	}
}

func autoConfigureInitFork(args *initFinalizeArgs) {
	if args.mode.contributor || !isGitRepo() {
		return
	}
	if err := autoConfigureForkContributor(args.ctx, args.store, args.mode.quiet || args.mode.nonInteractive, args.ident.roleFlag); err != nil && !args.mode.quiet {
		fmt.Fprintf(os.Stderr, "Warning: failed to auto-configure fork contributor routing: %v\n", err)
	}
}

func commitInitDoltState(args initFinalizeArgs) {
	if !shouldWriteInitStateToDB(args.doltCfg.Gateway) {
		return
	}
	if err := commitInitState(args.ctx, args.store); err != nil {
		if !strings.Contains(err.Error(), "nothing to commit") {
			fmt.Fprintf(os.Stderr, "Warning: failed to commit initial state: %v\n", err)
		}
	}
}

func closeInitStore(args initFinalizeArgs) {
	if err := args.store.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to close database: %v\n", err)
	}
	if !args.mode.initServerMode {
		return
	}
	if err := doltserver.MarkDoltDirCompatible(args.ident.storagePath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write Dolt compatibility marker at %s: %v\n", args.ident.storagePath, err)
	}
}

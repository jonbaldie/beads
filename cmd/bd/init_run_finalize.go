package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/beads"
)

var errInitStopped = errors.New("init stopped")

func writeInitIdentity(args initFinalizeArgs) error {
	ident, err := loadInitWorkspaceIdentity(args)
	if err != nil {
		return err
	}
	writeInitTrackingMetadata(args)
	if err := writeInitLocalWorkspaceFiles(args, ident); err != nil {
		return err
	}
	if err := seedInitIdentityAndJSONL(args, ident); err != nil {
		return err
	}
	if err := runInitRoleSetup(&args); err != nil {
		return err
	}
	closeInitStore(args)
	if err := setupInitGitIntegrations(args); err != nil {
		return err
	}
	if err := installInitHooksAndAgents(args); err != nil {
		if errors.Is(err, errInitStopped) {
			return nil
		}
		return err
	}
	commitInitBeadsFiles(args)
	warnInitGitUpstream(args)
	return printInitSuccess(args, ident)
}

func writeInitTrackingMetadata(args initFinalizeArgs) {
	// Tracking metadata enhances functionality but the system works without it.
	// In gateway mode it must not be written into the shared, server-owned database.
	if !shouldWriteInitStateToDB(args.doltCfg.Gateway) {
		return
	}
	writeInitVersionMetadata(args)
	writeInitRepoFingerprint(args)
	writeInitCloneFingerprint(args)
}

func writeInitVersionMetadata(args initFinalizeArgs) {
	if err := args.store.SetLocalMetadata(args.ctx, "bd_version", Version); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write bd_version local metadata: %v\n", err)
	}
}

func writeInitRepoFingerprint(args initFinalizeArgs) {
	repoID, err := beads.ComputeRepoID()
	if err != nil {
		if !args.mode.quiet {
			fmt.Fprintf(os.Stderr, "Warning: could not compute repository ID: %v\n", err)
		}
		return
	}
	if verifyMetadata(args.ctx, args.store, "repo_id", repoID) && !args.mode.quiet {
		fmt.Printf("  Repository ID: %s\n", repoID[:8])
	}
}

func writeInitCloneFingerprint(args initFinalizeArgs) {
	cloneID, err := beads.GetCloneID()
	if err != nil {
		if !args.mode.quiet {
			fmt.Fprintf(os.Stderr, "Warning: could not compute clone ID: %v\n", err)
		}
		return
	}
	if verifyMetadata(args.ctx, args.store, "clone_id", cloneID) && !args.mode.quiet {
		fmt.Printf("  Clone ID: %s\n", cloneID)
	}
}

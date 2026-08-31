package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage/uow"
)

func runDoltRemoteRemoveProxied(ctx context.Context, name string) error {
	if getUOWProvider() == nil {
		return HandleError("proxied-server UOW provider not initialized")
	}
	if err := deleteProxiedRemote(ctx, name); err != nil {
		reportRemoteRemoveError(err)
		return SilentExit()
	}
	clearOriginRemoteConfig(name)
	return renderRemoteRemoved(name)
}

func deleteProxiedRemote(ctx context.Context, name string) error {
	return uow.RunTx(ctx, getUOWProvider(), func(ctx context.Context, uw uow.UnitOfWork) (string, error) {
		if err := uw.DoltRemoteUseCase().DeleteRemote(ctx, name); err != nil {
			return "", err
		}
		return fmt.Sprintf("bd: remove remote %s", name), nil
	})
}

func reportRemoteRemoveError(err error) {
	if isJSONOutput() {
		_ = outputJSONError(err, "remote_remove_failed")
		return
	}
	fmt.Fprintf(os.Stderr, "Error removing remote: %v\n", err)
}

func clearOriginRemoteConfig(name string) {
	if name != "origin" || config.GetYamlConfig("sync.remote") == "" {
		return
	}
	if err := config.UnsetYamlConfig("sync.remote"); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to clear sync.remote from config.yaml: %v\n", err)
	}
	if isGitRepo() {
		commitBeadsConfig("bd: clear sync.remote")
	}
}

func renderRemoteRemoved(name string) error {
	if isJSONOutput() {
		if err := outputJSON(map[string]interface{}{
			"name":    name,
			"removed": true,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
	} else {
		fmt.Printf("Removed remote %q\n", name)
	}
	return nil
}

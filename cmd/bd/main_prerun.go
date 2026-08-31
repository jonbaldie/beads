package main

import (
	"errors"

	"github.com/spf13/cobra"
)

func runPersistentPreRun(cmd *cobra.Command, args []string) error {
	if bootstrapPersistentPreRun(cmd) {
		return nil
	}
	dbNameFromDBFlag, err := applyPersistentPreRunFlags(cmd)
	if err != nil {
		return err
	}
	skipsStoreInit := classifyPersistentStoreInit(cmd)
	maybeShowMetricsFirstRunNotice(cmd)
	if skipsStoreInit {
		return preparePersistentNoDB(cmd)
	}
	return runPersistentStorePreRun(cmd, args, dbNameFromDBFlag)
}

func runPersistentStorePreRun(cmd *cobra.Command, args []string, dbNameFromDBFlag string) error {
	if err := resolvePersistentDatabasePath(cmd, args); err != nil {
		if errors.Is(err, errPersistentPreRunComplete) {
			return nil
		}
		return err
	}
	return openPersistentPreRunStore(cmd, args, dbNameFromDBFlag)
}

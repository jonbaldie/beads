package main

import (
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/spf13/cobra"
)

var sendMetricsCmd = &cobra.Command{
	Use:    metrics.SendMetricsSubcommand,
	Short:  "Internal: flush queued telemetry events (spawned by bd)",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		// Always return an exitError, including code 0, so cobra skips
		// PersistentPostRunE. The flusher child must not fall through to
		// post-command maintenance. MaybeSpawnFlusher also refuses to spawn
		// when EnvIsFlusher is set on this process.
		return &exitError{Code: metrics.RunSendMetrics()}
	},
}

func init() {
	rootCmd.AddCommand(sendMetricsCmd)
}

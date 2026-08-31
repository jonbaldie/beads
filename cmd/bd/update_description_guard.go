package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func descriptionUsesExternalInput(cmd *cobra.Command) bool {
	if stdinFlag, _ := cmd.Flags().GetBool("stdin"); stdinFlag {
		return true
	}
	if descriptionFileInputChanged(cmd) {
		return true
	}
	return descriptionDashInputChanged(cmd)
}

func descriptionFileInputChanged(cmd *cobra.Command) bool {
	return cmd.Flags().Changed("body-file") || cmd.Flags().Changed("description-file")
}

func descriptionDashInputChanged(cmd *cobra.Command) bool {
	for _, flagName := range []string{"description", "body", "message"} {
		if cmd.Flags().Changed(flagName) {
			value, _ := cmd.Flags().GetString(flagName)
			if value == "-" {
				return true
			}
		}
	}
	return false
}

func validateDescriptionUpdate(cmd *cobra.Command, description string, descChanged bool) error {
	if !descChanged || description != "" || !descriptionUsesExternalInput(cmd) {
		return nil
	}

	allowEmptyDescription, _ := cmd.Flags().GetBool("allow-empty-description")
	if allowEmptyDescription {
		return nil
	}

	return fmt.Errorf("empty description from stdin/file requires --allow-empty-description (or use an explicit inline empty value like --description \"\")")
}

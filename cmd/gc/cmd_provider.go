package main

import (
	"io"

	"github.com/spf13/cobra"
)

// newProviderCmd builds the `gc provider` command group.
func newProviderCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Provider management utilities",
		Long:  `Provider management utilities for the configured [providers.<name>] blocks in city.toml.`,
	}
	cmd.AddCommand(newProviderCredentialsCmd(stdout, stderr))
	return cmd
}

package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newAnalyzeCmd is the parent for `gc analyze` subcommands. Read-only
// analysis commands that consume events and beads to produce reports.
// Introduced by issue #1254 (1c, evals workstream).
func newAnalyzeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Read-only analysis over events and beads",
		Long: `Analyze produces correlated reports over the events log and
bead state. All subcommands are read-only and safe to run alongside a
live controller.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			fmt.Fprintf(stderr, "gc analyze: unknown subcommand %q\n", args[0]) //nolint:errcheck
			return errExit
		},
	}
	cmd.AddCommand(newAnalyzeReliabilityCmd(stdout, stderr))
	return cmd
}

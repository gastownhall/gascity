package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func newBeadsCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "beads",
		Short: "Manage the beads provider",
		Long: `Manage the beads provider (backing store for issue tracking).

Subcommands for topology operations, health checking, and diagnostics.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc beads: missing subcommand (city, health)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc beads: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newBeadsCityCmd(stdout, stderr),
		newBeadsHealthCmd(stdout, stderr),
	)
	return cmd
}

func newBeadsHealthCmd(stdout, stderr io.Writer) *cobra.Command {
	var quiet bool
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check beads provider health",
		Long: `Check beads provider health and attempt recovery on failure.

Delegates to the provider's lifecycle health operation. For exec
providers (including bd/dolt), the script handles multi-tier checking
and recovery internally. For the file provider, always succeeds (no-op).

Also used by the beads-health system order for periodic monitoring.`,
		Example: `  gc beads health
  gc beads health --quiet`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doBeadsHealth(quiet, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&quiet, "quiet", false,
		"silent on success, stderr on failure")
	return cmd
}

// doBeadsHealth runs the beads provider health check.
// Returns 0 if healthy, 1 if unhealthy/recovery-failed.
func doBeadsHealth(quiet bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads health: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if err := healthBeadsProvider(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc beads health: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !quiet {
		fmt.Fprintln(stdout, "Beads provider: healthy") //nolint:errcheck // best-effort stdout
	}
	return 0
}

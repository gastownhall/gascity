package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newRuntimeCmd creates the "gc runtime" parent command for process-intrinsic
// runtime operations. These commands are called by agent code from within
// sessions — they read/write session metadata to coordinate with the
// controller.
func newRuntimeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runtime",
		Short: "Process-intrinsic runtime operations",
		Long: `Process-intrinsic runtime operations called by agent code from within sessions.

These commands read and write session metadata to coordinate lifecycle
events (drain, restart) between agents and the controller. They are
designed to be called from within running agent sessions, not by humans.

The exception is "gc runtime check", which validates a Runtime Provider
Protocol executable — run by humans and runtime-pack CIs.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			known := []string{"drain", "undrain", "drain-check", "drain-ack", "heartbeat", "request-restart", "check", "conformance"}
			fmt.Fprintf(stderr, "gc runtime: unknown subcommand %q\nAvailable subcommands: %v\n", args[0], known) //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
	cmd.AddCommand(
		newRuntimeDrainCmd(stdout, stderr),
		newRuntimeUndrainCmd(stdout, stderr),
		newRuntimeDrainCheckCmd(stdout, stderr),
		newRuntimeDrainAckCmd(stdout, stderr),
		newRuntimeHeartbeatCmd(stdout, stderr),
		newRuntimeRequestRestartCmd(stdout, stderr),
		newRuntimeCheckCmd(stdout, stderr),
		newRuntimeConformanceCmd(stdout, stderr),
	)
	return cmd
}

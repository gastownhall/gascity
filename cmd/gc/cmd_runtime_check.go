package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/gastownhall/gascity/internal/runtime/rppcheck"
	"github.com/spf13/cobra"
)

// newRuntimeCheckCmd creates "gc runtime check" — the RPP conformance
// command (RUNTIME-RPP-010 in internal/runtime/REQUIREMENTS.md). Runtime
// pack CIs run it against their installed executable with no Go imports
// from gascity.
func newRuntimeCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		command     string
		sessionName string
	)
	cmd := &cobra.Command{
		Use:   "check <executable>",
		Short: "Validate an executable against the Runtime Provider Protocol",
		Long: `Validate an executable against the Runtime Provider Protocol (RPP v0).

Runs the protocol handshake, the required lifecycle round-trip
(start, is-running, stop, idempotent stop), exercises every capability
the handshake declares, and probes optional operations. Optional
operations that are absent (exit 2) are reported but never fail the
run; everything else that misbehaves does. Exits non-zero if any check
fails, so a runtime pack's CI can gate on it directly.

The protocol contract is docs/reference/exec-session-provider.md.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Signal-aware context so Ctrl-C cancels the run; the checker
			// stops a started conformance session on cancellation.
			ctx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stopSignals()

			res, err := rppcheck.Run(ctx, args[0], rppcheck.Options{
				Command:     command,
				SessionName: sessionName,
			})
			if err != nil {
				fmt.Fprintf(stderr, "gc runtime check: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			var pass, fail, skip int
			for _, c := range res.Checks {
				line := fmt.Sprintf("%-4s %s", c.Status, c.Name)
				if c.Detail != "" {
					line += ": " + c.Detail
				}
				fmt.Fprintln(stdout, line) //nolint:errcheck // best-effort stdout
				switch c.Status {
				case rppcheck.StatusPass:
					pass++
				case rppcheck.StatusFail:
					fail++
				case rppcheck.StatusSkip:
					skip++
				}
			}
			fmt.Fprintf(stdout, "\n%d checks: %d passed, %d failed, %d skipped\n", //nolint:errcheck // best-effort stdout
				len(res.Checks), pass, fail, skip)

			if res.Failed() {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&command, "command", "", `session command sent in the start config (default "sleep 300")`)
	cmd.Flags().StringVar(&sessionName, "session-name", "", "session name for the conformance round-trip (default: generated unique name)")
	return cmd
}

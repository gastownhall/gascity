package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
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
		Use:   "check <name|executable>",
		Short: "Validate a runtime executable against the Runtime Provider Protocol",
		Long: `Validate a runtime executable against the Runtime Provider Protocol (RPP v0).

Runs the protocol handshake, the required lifecycle round-trip
(start, is-running, stop, idempotent stop), exercises every capability
the handshake declares, and probes optional operations. Optional
operations that are absent (exit 2) are reported but never fail the
run; everything else that misbehaves does. Exits non-zero if any check
fails, so a runtime pack's CI can gate on it directly.

The argument is an executable (path or PATH name) or a pack-declared
runtime name: when it names a [runtimes.<name>] entry from the current
city's packs, the check runs against that pack's declared command.
Arguments containing a path separator, or matching an existing file,
are always treated as the executable itself.

The protocol contract is docs/reference/exec-session-provider.md.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Signal-aware context so Ctrl-C cancels the run; the checker
			// stops a started conformance session on cancellation.
			ctx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stopSignals()

			target, note := resolveRuntimeCheckTarget(args[0], stderr)
			if note != "" {
				fmt.Fprintln(stdout, note) //nolint:errcheck // best-effort stdout
			}
			res, err := rppcheck.Run(ctx, target, rppcheck.Options{
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

// resolveRuntimeCheckTarget maps a pack-declared runtime name to its
// declared command via the current city's config (RUNTIME-RPP-010), and
// returns a note announcing the resolution. Path-like arguments and
// existing files are the executable itself; without a resolvable city, or
// when the name is not declared, the argument passes through unchanged
// (the checker PATH-resolves bare names like the exec provider does).
func resolveRuntimeCheckTarget(arg string, stderr io.Writer) (target, note string) {
	if strings.Contains(arg, "/") {
		return arg, ""
	}
	if _, err := os.Stat(arg); err == nil {
		return arg, ""
	}
	cityPath, err := resolveCity()
	if err != nil {
		return arg, ""
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime check: warning: city config not loaded (%v); treating %q as an executable\n", err, arg) //nolint:errcheck // best-effort stderr
		return arg, ""
	}
	rt, ok := cfg.Runtimes[arg]
	if !ok {
		return arg, ""
	}
	return rt.Command, fmt.Sprintf("resolved runtime %q from pack %q: %s", arg, rt.PackName, rt.Command)
}

package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/config"
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

One exception: when the argument resolves to a pack-declared runtime whose
[runtimes.<name>] entry declares prompt_delivery = "nudge-fallback", the
nudge probe is reported as "required: nudge" and an absent or broken nudge
op fails the run — the declaration is smoke-tested, not trusted.

A runtime whose handshake declares carrier.ops is driven only through its
nudge, peek, send-keys, interrupt and clear-scrollback operations, so each
is reported as "capability carrier.ops: <op>" and an absent or broken one
fails the run; this naming takes precedence over "required: nudge". With
idle-boundary declared as well, "capability carrier.ops: ready-prompt
metadata" requires set-meta and get-meta to round-trip a non-default ready
prompt byte for byte, because gc keeps each session's prompt there.

Every operation runs in a city, as gc's own operations do. With
GC_EXEC_CITY_PATH set, the check runs in the city GC_EXEC_CITY_NAME and
GC_EXEC_CITY_PATH name, and never sends that city server-stop. With it
unset or empty, as in a shell outside any city, the check runs in a
private city: GC_EXEC_CITY_NAME and GC_EXEC_CITY_PATH name a fresh
temporary directory, removed when the check ends or is interrupted.

A runtime whose handshake declares server-lifecycle is sent server-stop,
which ends every session of the city it runs in, twice. The check runs
it, and the session it checks it against, in a private city of their
own, never the city the rest of the check runs in. Before it removes a
private city, the check, an interrupted one too, sends that city a final
server-stop; one that does not exit 0 is reported as a warning, as the
server it was to end may still be running. A runtime that does not scope
its server by GC_EXEC_CITY_PATH ends every session the server holds, so
check it only against a backend that no live city uses.

A runtime whose handshake un-welds start (proc.provision with
proc.launch, or with proc.exec and without carrier.ops) is sent
provision, which must leave is-running false, and with proc.launch a
launch into that box, which must leave is-running true, and a fresh_only
launch over the running agent, which must exit 73. They run in a private
city of their own, as server-stop does. Such a runtime must also keep
metadata, since gc publishes each launch's identity with set-meta: a
set-meta that exits 2 fails the metadata round-trip instead of skipping it.

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

			target, note, promptDelivery := resolveRuntimeCheckTarget(args[0], stderr)
			if note != "" {
				fmt.Fprintln(stdout, note) //nolint:errcheck // best-effort stdout
			}
			res, err := rppcheck.Run(ctx, target, rppcheck.Options{
				Command:      command,
				SessionName:  sessionName,
				RequireNudge: promptDelivery == config.PromptDeliveryNudgeFallback,
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
			printRuntimeRunWarnings(stderr, "gc runtime check", res.Warnings)

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

// printRuntimeRunWarnings writes each warning of a runtime check or
// conformance run to stderr, as gc stop reports a failed server teardown.
func printRuntimeRunWarnings(stderr io.Writer, cmdName string, warnings []string) {
	for _, w := range warnings {
		fmt.Fprintf(stderr, "%s: warning: %s\n", cmdName, w) //nolint:errcheck // best-effort stderr
	}
}

// resolveRuntimeCheckTarget maps a pack-declared runtime name to its
// declared command via the current city's config (RUNTIME-RPP-010), and
// returns a note announcing the resolution plus the runtime's declared
// PromptDelivery strategy (empty unless resolution succeeds), so the
// caller can require the nudge-fallback smoke test (FR5, ga-s5y62b.2) when
// the pack claims it. Path-like arguments and existing files are the
// executable itself; without a resolvable city, or when the name is not
// declared, the argument passes through unchanged (the checker
// PATH-resolves bare names like the exec provider does).
func resolveRuntimeCheckTarget(arg string, stderr io.Writer) (target, note, promptDelivery string) {
	if strings.Contains(arg, "/") {
		return arg, "", ""
	}
	if _, err := os.Stat(arg); err == nil {
		return arg, "", ""
	}
	cityPath, err := resolveCity()
	if err != nil {
		return arg, "", ""
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		fmt.Fprintf(stderr, "gc runtime check: warning: city config not loaded (%v); treating %q as an executable\n", err, arg) //nolint:errcheck // best-effort stderr
		return arg, "", ""
	}
	rt, ok := cfg.Runtimes[arg]
	if !ok {
		return arg, "", ""
	}
	return rt.Command, fmt.Sprintf("resolved runtime %q from pack %q: %s", arg, rt.PackName, rt.Command), rt.PromptDelivery
}

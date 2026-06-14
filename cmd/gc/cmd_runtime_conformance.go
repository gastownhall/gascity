package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/gastownhall/gascity/internal/runtime/runtimecapability"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract"
	"github.com/spf13/cobra"
)

// newRuntimeConformanceCmd creates "gc runtime conformance" — the golden
// RPP conformance suite (internal/runtime/runtimecontract). Where
// `gc runtime check` is a quick smoke test, this runs the full
// requirement-coded catalog that mirrors the in-tree provider contract:
// passing every required requirement guarantees the executable behaves like
// a gascity runtime. A runtime pack's CI runs it against its installed
// executable with no Go imports from gascity.
func newRuntimeConformanceCmd(stdout, stderr io.Writer) *cobra.Command {
	var asJSON, withEnv bool
	cmd := &cobra.Command{
		Use:   "conformance <name|executable>",
		Short: "Run the golden RPP conformance suite against a runtime executable",
		Long: `Run the golden Runtime Provider Protocol conformance suite against an
executable. Every requirement is requirement-coded (RPP-<GROUP>-NNN) and
mirrors the in-tree provider contract (RunProviderTests); a run that passes
every required requirement is guaranteed to behave like a gascity runtime.

Unlike "gc runtime check" (a lighter smoke test), each requirement is
proven to gate: the suite is kept honest by negative tests in which a
broken reference fails exactly its requirement's check.

The argument is an executable (path or PATH name) or a pack-declared
runtime name from the current city's packs. Path-like or existing-file
arguments are always the executable itself.

Use --json for a machine-readable report (CI artifacts). Exits non-zero if
any required requirement fails.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			target, note := resolveRuntimeCheckTarget(args[0], stderr)
			if note != "" && !asJSON {
				fmt.Fprintln(stdout, note) //nolint:errcheck // best-effort stdout
			}

			report, err := runtimecontract.Run(ctx, target, runtimecontract.Options{})
			if err != nil {
				fmt.Fprintf(stderr, "gc runtime conformance: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			if asJSON {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					fmt.Fprintf(stderr, "gc runtime conformance: encoding report: %v\n", err) //nolint:errcheck // best-effort stderr
					return errExit
				}
			} else {
				for _, res := range report.Results {
					line := fmt.Sprintf("%-4s %s  %s", res.Status, res.Code, res.Title)
					if res.Detail != "" {
						line += " — " + res.Detail
					}
					fmt.Fprintln(stdout, line) //nolint:errcheck // best-effort stdout
				}
				fmt.Fprintf(stdout, "\n%d requirements: %d passed, %d failed, %d skipped\n", //nolint:errcheck // best-effort stdout
					len(report.Results), report.Summary.Passed, report.Summary.Failed, report.Summary.Skipped)
			}

			failed := report.Failed()

			// --env additionally runs the environment-plane capability suite
			// (RUNTIME-RPP-012): verify the env.* guarantees the runtime
			// declares in its handshake.
			if withEnv {
				capReport, err := runtimecapability.Run(ctx, target, runtimecapability.Options{})
				if err != nil {
					fmt.Fprintf(stderr, "gc runtime conformance: capability run: %v\n", err) //nolint:errcheck
					return errExit
				}
				if asJSON {
					enc := json.NewEncoder(stdout)
					enc.SetIndent("", "  ")
					_ = enc.Encode(capReport)
				} else {
					fmt.Fprintf(stdout, "\nenvironment capabilities (declared: %v):\n", capReport.Capabilities) //nolint:errcheck
					for _, res := range capReport.Results {
						line := fmt.Sprintf("%-4s %s  %s", res.Status, res.Code, res.Title)
						if res.Detail != "" {
							line += " — " + res.Detail
						}
						fmt.Fprintln(stdout, line) //nolint:errcheck
					}
					fmt.Fprintf(stdout, "%d capabilities: %d passed, %d failed, %d skipped\n", //nolint:errcheck
						len(capReport.Results), capReport.Summary.Passed, capReport.Summary.Failed, capReport.Summary.Skipped)
				}
				failed = failed || capReport.Failed()
			}

			if failed {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable JSON report")
	cmd.Flags().BoolVar(&withEnv, "env", false, "also run the environment-plane capability suite (env.* guarantees)")
	return cmd
}

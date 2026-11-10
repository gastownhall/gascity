package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/runtime/runtimecapability"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract"
	"github.com/spf13/cobra"
)

// runtimeConformanceFailedCode is the --json error code for a run in which a
// required requirement or a declared capability failed.
const runtimeConformanceFailedCode = "runtime_conformance_failed"

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

A runtime whose handshake declares server-lifecycle is sent server-stop,
which ends every session of the city it runs in (the server group). The
suite runs it, and the sessions it checks it against, in a private city:
GC_EXEC_CITY_NAME and GC_EXEC_CITY_PATH name a fresh temporary directory,
never the city you run the suite from. A runtime that does not scope its
server by GC_EXEC_CITY_PATH ends every session the server holds, so check it
only against a backend that no live city uses.

The argument is an executable (path or PATH name) or a pack-declared
runtime name from the current city's packs. Path-like or existing-file
arguments are always the executable itself.

Use --json for a machine-readable report (CI artifacts): one JSON document
with ok:true, or ok:false and an error object when a requirement fails; the
report fields are present either way, and --env adds the capability report
under "environment". Exits non-zero if any required requirement fails.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			target, note, _ := resolveRuntimeCheckTarget(args[0], stderr)
			if note != "" && !asJSON {
				fmt.Fprintln(stdout, note) //nolint:errcheck // best-effort stdout
			}

			report, err := runtimecontract.Run(ctx, target, runtimecontract.Options{})
			if err != nil {
				fmt.Fprintf(stderr, "gc runtime conformance: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			if !asJSON {
				for _, res := range report.Results {
					line := fmt.Sprintf("%-4s %s  %s", res.Status, res.Code, res.Title)
					if res.Detail != "" {
						line += " — " + res.Detail
					}
					fmt.Fprintln(stdout, line) //nolint:errcheck // best-effort stdout
				}
				fmt.Fprintf(stdout, "\n%d requirements: %d passed, %d failed, %d skipped\n", //nolint:errcheck // best-effort stdout
					len(report.Results), report.Summary.Passed, report.Summary.Failed, report.Summary.Skipped)
				fmt.Fprintln(stdout, v2ReadyLine(report)) //nolint:errcheck // best-effort stdout
			}

			failed := report.Failed()

			// --env additionally runs the environment-plane capability suite
			// (RUNTIME-RPP-012): verify the env.* guarantees the runtime
			// declares in its handshake.
			var capReport *runtimecapability.Report
			if withEnv {
				r, err := runtimecapability.Run(ctx, target, runtimecapability.Options{})
				if err != nil {
					fmt.Fprintf(stderr, "gc runtime conformance: capability run: %v\n", err) //nolint:errcheck
					return errExit
				}
				capReport = &r
				if !asJSON {
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

			if asJSON {
				if err := writeCLIJSONLineOrErr(stdout, stderr, "gc runtime conformance", newRuntimeConformanceJSON(report, capReport, failed)); err != nil {
					return err
				}
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

// runtimeConformanceJSON is the --json payload of gc runtime conformance
// (schemas/runtime/conformance/result.schema.json): the conformance Report's
// fields at top level inside the CLI envelope. A conformant run carries
// ok:true; a failed run carries ok:false with the shared failure error object
// and still the full report, so CI sees which requirement failed. --env adds
// the capability report under environment, keeping the output one document.
type runtimeConformanceJSON struct {
	SchemaVersion string                 `json:"schema_version"`
	OK            bool                   `json:"ok"`
	Error         *jsonSchemaErrorDetail `json:"error,omitempty"`
	runtimecontract.Report
	Environment *runtimecapability.Report `json:"environment,omitempty"`
}

func newRuntimeConformanceJSON(report runtimecontract.Report, capReport *runtimecapability.Report, failed bool) runtimeConformanceJSON {
	payload := runtimeConformanceJSON{
		SchemaVersion: "1",
		OK:            !failed,
		Report:        report,
		Environment:   capReport,
	}
	if failed {
		message := fmt.Sprintf("%d required requirements failed", report.Summary.Failed)
		if capReport != nil {
			message += fmt.Sprintf(", %d declared capabilities failed", capReport.Summary.Failed)
		}
		payload.Error = &jsonSchemaErrorDetail{
			Code:     runtimeConformanceFailedCode,
			Message:  message,
			ExitCode: 1,
		}
	}
	return payload
}

// v2ReadyLine says whether the executable is fit for reconciler v2, and
// why not: the capabilities its handshake lacks, and the requirements it
// failed. A handshake that lacks nothing already has gc read the
// executable through observe.v1, whatever it failed, and the line says so.
func v2ReadyLine(report runtimecontract.Report) string {
	if report.V2Ready {
		return "v2-ready: yes"
	}
	var why []string
	if len(report.V2Missing) > 0 {
		why = append(why, "missing "+strings.Join(report.V2Missing, ", "))
	} else {
		why = append(why, "gc already reads it through observe.v1")
	}
	if len(report.V2Failed) > 0 {
		failed := make([]string, len(report.V2Failed))
		for i, c := range report.V2Failed {
			failed[i] = string(c)
		}
		why = append(why, "failed "+strings.Join(failed, ", "))
	}
	return fmt.Sprintf("v2-ready: no (%s)", strings.Join(why, "; "))
}

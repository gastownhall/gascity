package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/latency"
	"github.com/spf13/cobra"
)

// latencyCmdOptions captures the resolved CLI flags for one invocation of
// `gc analyze latency`. Extracted so the run logic is testable without
// faking the cobra binding layer — same split as beadsCmdOptions.
type latencyCmdOptions struct {
	cityPath  string
	since     string
	until     string
	pool      string
	formula   string
	jsonOut   bool
	eventPath string
}

func newAnalyzeLatencyCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := latencyCmdOptions{}
	cmd := &cobra.Command{
		Use:   "latency",
		Short: "Chain/convoy latency: claim wait, gate queue wait, gate bounce rate",
		Long: `Latency reports three chain/convoy timing questions over the same
events.jsonl stream:

  1. Claim wait per pool — the time between a bead becoming pool-routed
     (gc.routed_to set, unassigned) and being claimed (assignee set),
     grouped by pool. Diagnoses P0-pool starvation.
  2. Gate queue wait per formula/step — the gap between
     execution.step_defined and execution.step_started for the same
     physical step, grouped by formula and step id.
  3. Gate bounce rate per formula — how many times a formula's steps were
     redefined (a fresh execution.step_defined for the same run+step)
     before finally running, as a share of all definitions.

Pool is read from the bead's gc.routed_to metadata (set when a step is
routed to a pool rather than a direct session), not a dedicated claim
event — Gas City has none. Formula is read from the workflow root bead's
gc.formula/gc.formula_name metadata, resolved via each execution event's
run id; a run whose root metadata was not observed in the window groups
under "unknown".

Read-only: this command never writes events or beads.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := runAnalyzeLatency(opts, stdout, stderr); err != nil {
				if errors.Is(err, errExit) {
					return err
				}
				fmt.Fprintf(stderr, "gc analyze latency: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.cityPath, "city", "", "city directory (default: discover from cwd)")
	cmd.Flags().StringVar(&opts.since, "since", "24h",
		"start of the analysis window — duration (1h, 7d) or RFC3339 timestamp")
	cmd.Flags().StringVar(&opts.until, "until", "",
		"end of the analysis window — duration (0s = now, 30m = 30 minutes ago) or RFC3339 timestamp")
	cmd.Flags().StringVar(&opts.pool, "pool", "", "filter claim-wait to a specific pool (gc.routed_to value)")
	cmd.Flags().StringVar(&opts.formula, "formula", "", "filter gate queue-wait/bounce to a specific formula")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit JSON instead of a table")
	cmd.Flags().StringVar(&opts.eventPath, "events", "", "explicit events.jsonl path (overrides city discovery)")
	return cmd
}

// runAnalyzeLatency is the testable core: resolves inputs, loads events,
// runs the analyzer, and writes output. Returns an error so the cobra
// wrapper can decide between user-facing messages and exit codes.
func runAnalyzeLatency(opts latencyCmdOptions, stdout, _ io.Writer) error {
	now := time.Now().UTC()
	since, err := parseTimeFlag(opts.since, now)
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	until := time.Time{}
	if strings.TrimSpace(opts.until) != "" {
		until, err = parseTimeFlag(opts.until, now)
		if err != nil {
			return fmt.Errorf("--until: %w", err)
		}
	}

	eventsPath, err := resolveEventsPath(opts.cityPath, opts.eventPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(opts.eventPath) != "" {
		if err := validateExplicitEventsPath(eventsPath); err != nil {
			return err
		}
	}

	all, err := events.ReadAll(eventsPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", eventsPath, err)
	}

	report := latency.Analyze(all, latency.Window{Since: since, Until: until},
		latency.Filter{Pool: opts.pool, Formula: opts.formula})

	if opts.jsonOut {
		return writeCLIJSONLine(stdout, report)
	}
	return latency.FormatTable(stdout, report)
}

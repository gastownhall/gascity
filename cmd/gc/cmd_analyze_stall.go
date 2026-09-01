package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/stalldetect"
	"github.com/spf13/cobra"
)

// defaultStallThreshold is the "no event newer than N minutes while
// status=in_progress" window this report treats as the dispatcher-wedged
// signature when --threshold is not set.
const defaultStallThreshold = "15m"

// stallCmdOptions captures the resolved CLI flags for one invocation of
// `gc analyze stalls`. Extracted so the run logic is testable without
// faking the cobra binding layer — same split as beadsCmdOptions and
// reliabilityCmdOptions.
type stallCmdOptions struct {
	cityPath  string
	since     string
	until     string
	threshold string
	pool      string
	jsonOut   bool
	eventPath string
}

func newAnalyzeStallCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := stallCmdOptions{}
	cmd := &cobra.Command{
		Use:   "stalls",
		Short: "Last-event age per in-progress bead / pool — the dispatcher-wedged signature",
		Long: `Stalls reports every bead last known to be status=in_progress,
its last-event age (now minus the most recent event carrying that bead
id as Subject), and whether that age meets or exceeds --threshold — the
"is the dispatcher wedged" signature: a bead claimed and started with no
event since. Results are grouped per pool (derived from the bead's
assignee; a pool-instance identity like "polecat-2" folds into pool
"polecat", an in-progress bead with no assignee groups under
"unassigned") as well as listed per bead, sorted oldest-first.

--since bounds how far back events.jsonl is scanned to establish each
bead's current status and last-event time; it is not the stall
threshold. A bead that has been in_progress longer than --since with no
event in that window will not appear — widen --since to see it.

Read-only: this command never writes events or beads.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := runAnalyzeStall(opts, stdout, stderr); err != nil {
				if errors.Is(err, errExit) {
					return err
				}
				fmt.Fprintf(stderr, "gc analyze stalls: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.cityPath, "city", "", "city directory (default: discover from cwd)")
	cmd.Flags().StringVar(&opts.since, "since", "24h",
		"start of the event lookback window — duration (1h, 7d) or RFC3339 timestamp")
	cmd.Flags().StringVar(&opts.until, "until", "",
		"end of the event lookback window and evaluation instant — duration (0s = now, 30m = 30 minutes ago) or RFC3339 timestamp")
	cmd.Flags().StringVar(&opts.threshold, "threshold", defaultStallThreshold,
		"no-event age at or above which an in-progress bead is reported stalled (duration, e.g. 15m, 1h)")
	cmd.Flags().StringVar(&opts.pool, "pool", "", "filter to a specific pool (derived from bead assignee)")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit JSON instead of a table")
	cmd.Flags().StringVar(&opts.eventPath, "events", "", "explicit events.jsonl path (overrides city discovery)")
	return cmd
}

// runAnalyzeStall is the testable core: resolves inputs, loads events,
// runs the analyzer, and writes output. Returns an error so the cobra
// wrapper can decide between user-facing messages and exit codes.
func runAnalyzeStall(opts stallCmdOptions, stdout, _ io.Writer) error {
	now := time.Now().UTC()
	since, err := parseTimeFlag(opts.since, now)
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	until := now
	if strings.TrimSpace(opts.until) != "" {
		until, err = parseTimeFlag(opts.until, now)
		if err != nil {
			return fmt.Errorf("--until: %w", err)
		}
	}
	thresholdRaw := strings.TrimSpace(opts.threshold)
	if thresholdRaw == "" {
		thresholdRaw = defaultStallThreshold
	}
	threshold, err := parseDurationWithDays(thresholdRaw)
	if err != nil {
		return fmt.Errorf("--threshold: expected duration (e.g. 15m, 1h), got %q: %w", opts.threshold, err)
	}
	if threshold < 0 {
		return fmt.Errorf("--threshold: must not be negative, got %q", opts.threshold)
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

	report := stalldetect.Analyze(all, stalldetect.Window{Since: since, Until: until}, until, threshold,
		stalldetect.Filter{Pool: opts.pool})

	if opts.jsonOut {
		return writeCLIJSONLine(stdout, report)
	}
	return stalldetect.FormatTable(stdout, report)
}

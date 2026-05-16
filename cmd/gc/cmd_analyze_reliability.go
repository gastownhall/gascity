package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reliability"
	"github.com/spf13/cobra"
)

// reliabilityCmdOptions captures the resolved CLI flags for one
// invocation of `gc analyze reliability`. Extracted so the run logic
// is testable without faking the cobra binding layer.
type reliabilityCmdOptions struct {
	cityPath  string
	since     string
	until     string
	model     string
	rig       string
	jsonOut   bool
	eventPath string
}

func newAnalyzeReliabilityCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := reliabilityCmdOptions{}
	cmd := &cobra.Command{
		Use:   "reliability",
		Short: "Correlate session-lifecycle events with model/version/rig",
		Long: `Reliability reports per-(model, prompt_version, rig) counts of
the tracked session-lifecycle events:

  session.crashed
  session.quarantined (reserved; current production paths do not emit it)
  session.idle_killed
  session.draining

Worker.operation events from #1252 supply the (model, prompt_version,
agent_name) tuple per session. Lifecycle events get attributed via the
session id or producer aliases from worker.operation payloads. Sessions
with worker.operation events but no lifecycle events count toward the
per-group total — they're the denominator side of crash-rate
calculations. Model and prompt_version are best-effort dimensions; the
report warns when the source event stream is missing them.

Read-only: this command never writes events or beads.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := runAnalyzeReliability(opts, stdout, stderr); err != nil {
				if errors.Is(err, errExit) {
					return err
				}
				fmt.Fprintf(stderr, "gc analyze reliability: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.cityPath, "city", "", "city directory (default: discover from cwd)")
	cmd.Flags().StringVar(&opts.since, "since", "7d",
		"start of the analysis window — duration (1h, 7d) or RFC3339 timestamp")
	cmd.Flags().StringVar(&opts.until, "until", "",
		"end of the analysis window — duration (0s = now, 30m = 30 minutes ago) or RFC3339 timestamp")
	cmd.Flags().StringVar(&opts.model, "model", "", "filter to a specific model")
	cmd.Flags().StringVar(&opts.rig, "rig", "", "filter to a specific rig")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit JSON instead of a table")
	cmd.Flags().StringVar(&opts.eventPath, "events", "", "explicit events.jsonl path (overrides city discovery)")
	return cmd
}

// runAnalyzeReliability is the testable core: resolves inputs, loads
// events, runs the analyzer, and writes output. Returns an error so the
// cobra wrapper can decide between user-facing messages and exit codes.
func runAnalyzeReliability(opts reliabilityCmdOptions, stdout, _ io.Writer) error {
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

	eventsPath, err := resolveEventsPath(opts)
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

	report := reliability.Analyze(all, reliability.Window{Since: since, Until: until},
		reliability.Filter{Model: opts.model, Rig: opts.rig})

	if opts.jsonOut {
		return reliability.FormatJSON(stdout, report)
	}
	return reliability.FormatTable(stdout, report)
}

// resolveEventsPath returns the absolute path to events.jsonl using
// the explicit --events flag when set, --city when present, or the
// standard discovery cascade (env, cwd) otherwise.
func resolveEventsPath(opts reliabilityCmdOptions) (string, error) {
	if strings.TrimSpace(opts.eventPath) != "" {
		return opts.eventPath, nil
	}
	cityPath := strings.TrimSpace(opts.cityPath)
	if cityPath != "" {
		resolved, err := validateCityPath(cityPath)
		if err != nil {
			return "", fmt.Errorf("--city %s: %w", cityPath, err)
		}
		return filepath.Join(resolved, citylayout.RuntimeRoot, "events.jsonl"), nil
	}

	resolved, err := resolveCity()
	if err != nil {
		if rootCity := strings.TrimSpace(cityFlag); rootCity != "" {
			return "", fmt.Errorf("--city %s: %w", rootCity, err)
		}
		return "", fmt.Errorf("could not locate events.jsonl; pass --city or --events: %w", err)
	}
	return filepath.Join(resolved, citylayout.RuntimeRoot, "events.jsonl"), nil
}

func validateExplicitEventsPath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("--events %s: file does not exist", path)
		}
		return fmt.Errorf("--events %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("--events %s: expected a file, got a directory", path)
	}
	return nil
}

// parseTimeFlag accepts either a Go duration ("7d", "12h") interpreted
// as "now - duration" or an RFC3339 timestamp. The "d" suffix is
// supported as a 24-hour shorthand because Go's time.ParseDuration
// itself doesn't accept days.
func parseTimeFlag(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if d, err := parseDurationWithDays(raw); err == nil {
		if d == 0 {
			return now, nil
		}
		return now.Add(-d), nil
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts, nil
	}
	return time.Time{}, fmt.Errorf("expected duration (e.g. 7d, 12h) or RFC3339 timestamp, got %q", raw)
}

// parseDurationWithDays extends time.ParseDuration with a "d" suffix
// for whole-day durations. Examples: "7d" → 168h, "1d12h" → 36h.
// Returns an error if the input has no recognized form.
func parseDurationWithDays(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty duration")
	}
	// Find day component if present.
	dayIdx := strings.IndexByte(raw, 'd')
	var days int64
	if dayIdx >= 0 {
		// Parse leading integer for days.
		n, err := atoi64(raw[:dayIdx])
		if err != nil {
			return 0, err
		}
		days = n
		raw = raw[dayIdx+1:]
	}
	var rest time.Duration
	if raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, err
		}
		rest = d
	}
	return time.Duration(days)*24*time.Hour + rest, nil
}

// atoi64 parses a positive int64 from s. Empty s = 0.
func atoi64(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid integer %q", s)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

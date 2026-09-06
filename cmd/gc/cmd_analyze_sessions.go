package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/sessionoutcomes"
	"github.com/spf13/cobra"
)

// sessionsCmdOptions captures the resolved CLI flags for one invocation
// of `gc analyze sessions`. Extracted so the run logic is testable
// without faking the cobra binding layer — same split as
// reliabilityCmdOptions / beadsCmdOptions.
type sessionsCmdOptions struct {
	cityPath  string
	since     string
	until     string
	bucket    string
	template  string
	provider  string
	jsonOut   bool
	eventPath string
}

func newAnalyzeSessionsCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := sessionsCmdOptions{}
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Session-start outcomes by template/provider over time",
		Long: `Sessions reports worker.operation session-start attempts
(the "start" and "start_resolved" operations), bucketed by time and
grouped by template and provider, with succeeded/failed/other counts
and duration_ms statistics per bucket.

The report flags a bucket as a possible provider outage when every
attempt in it failed and duration_ms is clustered tight — the signature
of every session-start attempt dying at the same broken stage (e.g. an
auth handshake) rather than failing independently. Today that signature
only shows up if someone eyeballs worker.operation events for it; this
computes it directly.

Read-only: this command never writes events or beads.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := runAnalyzeSessions(opts, stdout, stderr); err != nil {
				if errors.Is(err, errExit) {
					return err
				}
				fmt.Fprintf(stderr, "gc analyze sessions: %v\n", err) //nolint:errcheck
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
	cmd.Flags().StringVar(&opts.bucket, "bucket", "1h", "time-bucket width for the over-time grouping (e.g. 15m, 1h, 1d)")
	cmd.Flags().StringVar(&opts.template, "template", "", "filter to a specific template")
	cmd.Flags().StringVar(&opts.provider, "provider", "", "filter to a specific provider")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit JSON instead of a table")
	cmd.Flags().StringVar(&opts.eventPath, "events", "", "explicit events.jsonl path (overrides city discovery)")
	return cmd
}

// runAnalyzeSessions is the testable core: resolves inputs, loads
// events, runs the analyzer, and writes output. Returns an error so the
// cobra wrapper can decide between user-facing messages and exit codes.
func runAnalyzeSessions(opts sessionsCmdOptions, stdout, _ io.Writer) error {
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
	bucket, err := parseDurationWithDays(strings.TrimSpace(opts.bucket))
	if err != nil {
		return fmt.Errorf("--bucket: %w", err)
	}
	if bucket <= 0 {
		return fmt.Errorf("--bucket: must be positive, got %q", opts.bucket)
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

	report := sessionoutcomes.Analyze(all, sessionoutcomes.Window{Since: since, Until: until}, bucket,
		sessionoutcomes.Filter{Template: opts.template, Provider: opts.provider})

	if opts.jsonOut {
		return writeCLIJSONLine(stdout, report)
	}
	return sessionoutcomes.FormatTable(stdout, report)
}

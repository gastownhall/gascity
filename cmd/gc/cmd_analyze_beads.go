package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadthroughput"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/spf13/cobra"
)

// beadsCmdOptions captures the resolved CLI flags for one invocation of
// `gc analyze beads`. Extracted so the run logic is testable without
// faking the cobra binding layer — same split as reliabilityCmdOptions.
type beadsCmdOptions struct {
	cityPath  string
	since     string
	until     string
	store     string
	beadType  string
	label     string
	jsonOut   bool
	eventPath string
}

func newAnalyzeBeadsCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := beadsCmdOptions{}
	cmd := &cobra.Command{
		Use:   "beads",
		Short: "Bead open/close counts by store, type, and label",
		Long: `Beads reports bead.created/bead.closed counts over a time window,
grouped by store, type, and label — the routine "how much did we ship in
this window" stats query.

Store is derived from the bead id's mint-prefix namespace (the segment
before the first "-"; ids with no hyphen group under "default"), the
same prefix convention the relocated coordination classes (graph,
messaging, sessions, orders, nudges) mint under. Type is the bead's
issue_type. A bead carrying multiple labels contributes to each label's
bucket; the TOTAL row counts distinct bead ids instead of summing
groups, so it is not inflated by multi-label fan-out.

Read-only: this command never writes events or beads.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := runAnalyzeBeads(opts, stdout, stderr); err != nil {
				if errors.Is(err, errExit) {
					return err
				}
				fmt.Fprintf(stderr, "gc analyze beads: %v\n", err) //nolint:errcheck
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
	cmd.Flags().StringVar(&opts.store, "store", "", "filter to a specific store (bead-id mint-prefix namespace)")
	cmd.Flags().StringVar(&opts.beadType, "type", "", "filter to a specific bead type (issue_type)")
	cmd.Flags().StringVar(&opts.label, "label", "", "filter to beads carrying a specific label")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit JSON instead of a table")
	cmd.Flags().StringVar(&opts.eventPath, "events", "", "explicit events.jsonl path (overrides city discovery)")
	return cmd
}

// runAnalyzeBeads is the testable core: resolves inputs, loads events,
// runs the analyzer, and writes output. Returns an error so the cobra
// wrapper can decide between user-facing messages and exit codes.
func runAnalyzeBeads(opts beadsCmdOptions, stdout, _ io.Writer) error {
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

	report := beadthroughput.Analyze(all, beadthroughput.Window{Since: since, Until: until},
		beadthroughput.Filter{Store: opts.store, Type: opts.beadType, Label: opts.label})

	if opts.jsonOut {
		return writeCLIJSONLine(stdout, report)
	}
	return beadthroughput.FormatTable(stdout, report)
}

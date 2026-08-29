package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/workrecord"
	"github.com/spf13/cobra"
)

// workRecordCmdOptions captures the resolved CLI flags for one invocation of
// `gc analyze work-record`. Extracted so the run logic is testable without
// faking the cobra binding layer.
type workRecordCmdOptions struct {
	cityPath string
	limit    int
	jsonOut  bool
}

func newAnalyzeWorkRecordCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := workRecordCmdOptions{}
	cmd := &cobra.Command{
		Use:   "work-record",
		Short: "Report work-record close-gate coverage across closed beads",
		Long: `Work-record scans closed, worker-claimable beads and reports how
many carry a valid gc.work_outcome (shipped, no-op, blocked, abandoned)
versus how many are missing one, per the work-record close gate
(ADR-0009, cmd/gc/work_record_gate.go). Control/structural beads (gc.kind
workflow roots, scope/run/check/drain steps, etc.) and non-task beads are
excluded — they are not subject to the gate.

Read-only: this command never writes beads.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := runAnalyzeWorkRecord(opts, stdout, stderr); err != nil {
				if errors.Is(err, errExit) {
					return err
				}
				fmt.Fprintf(stderr, "gc analyze work-record: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.cityPath, "city", "", "city directory (default: discover from cwd)")
	cmd.Flags().IntVar(&opts.limit, "limit", 500, "maximum number of closed beads to scan")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit JSON instead of a table")
	return cmd
}

// runAnalyzeWorkRecord is the testable core: resolves the city, opens the
// store, and delegates to analyzeWorkRecordFromStore.
func runAnalyzeWorkRecord(opts workRecordCmdOptions, stdout, _ io.Writer) error {
	cityPath, err := resolveWorkRecordCityPath(opts.cityPath)
	if err != nil {
		return err
	}
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	return analyzeWorkRecordFromStore(store, opts.limit, opts.jsonOut, stdout)
}

// resolveWorkRecordCityPath returns the absolute city path using the
// explicit --city flag when set, or the standard discovery cascade
// (env, cwd) otherwise.
func resolveWorkRecordCityPath(cityPath string) (string, error) {
	cityPath = strings.TrimSpace(cityPath)
	if cityPath != "" {
		resolved, err := validateCityPath(cityPath)
		if err != nil {
			return "", fmt.Errorf("--city %s: %w", cityPath, err)
		}
		return resolved, nil
	}

	resolved, err := resolveCity()
	if err != nil {
		if rootCity := strings.TrimSpace(cityFlag); rootCity != "" {
			return "", fmt.Errorf("--city %s: %w", rootCity, err)
		}
		return "", fmt.Errorf("could not locate a city; pass --city: %w", err)
	}
	return resolved, nil
}

// analyzeWorkRecordFromStore scans up to limit closed beads in store and
// reports work-record gate coverage to stdout, as JSON when jsonOut is set.
func analyzeWorkRecordFromStore(store beads.Store, limit int, jsonOut bool, stdout io.Writer) error {
	beadList, err := store.List(beads.ListQuery{
		Status:        "closed",
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
		Limit:         limit,
	})
	if err != nil {
		return fmt.Errorf("listing closed beads: %w", err)
	}
	report := workrecord.AnalyzeCoverage(beadList)
	if jsonOut {
		return writeCLIJSONLine(stdout, report)
	}
	return workrecord.FormatTable(stdout, report)
}

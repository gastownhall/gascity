package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/executionevent"
	"github.com/spf13/cobra"
)

var eventsReemitExecutionControllerAliveHook = controllerAlive

type eventsReemitExecutionResult struct {
	RunID      string `json:"run_id"`
	RunCount   int    `json:"run_count"`
	WorkCount  int    `json:"work_count"`
	StepCount  int    `json:"step_count"`
	EventCount int    `json:"event_count"`
	Applied    bool   `json:"applied"`
}

func newEventsReemitExecutionCmd(stdout, stderr io.Writer) *cobra.Command {
	var runID string
	var apply bool
	cmd := &cobra.Command{
		Use:   "reemit-execution --city <city> --run <run> [--apply]",
		Short: "Project one graph execution run into event facts",
		Long: `Project exactly one stopped local graph.v2 execution run into execution facts.

The default is a dry run. Pass --apply to append the projected snapshot to the
default city event log.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := runEventsReemitExecution(cmd, runID, apply, stdout); err != nil {
				fmt.Fprintf(stderr, "gc events reemit-execution: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "graph.v2 workflow root ID to project")
	cmd.Flags().BoolVar(&apply, "apply", false, "append projected facts to the default file event log")
	return cmd
}

func runEventsReemitExecution(cmd *cobra.Command, runID string, apply bool, stdout io.Writer) error {
	if !cmd.Flags().Changed("city") || strings.TrimSpace(cityFlag) == "" {
		return fmt.Errorf("--city is required")
	}
	if !cmd.Flags().Changed("run") || strings.TrimSpace(runID) == "" {
		return fmt.Errorf("--run is required")
	}
	if strings.TrimSpace(rigFlag) != "" || cmd.Flags().Changed("rig") {
		return fmt.Errorf("--rig is not supported")
	}
	if strings.TrimSpace(contextFlag) != "" || strings.TrimSpace(cityURLFlag) != "" || strings.TrimSpace(cityNameFlag) != "" || readRemoteSelection().hasExplicitRemote() {
		return fmt.Errorf("remote city selection is not supported")
	}

	cityPath, err := resolveCityFlagValue(cityFlag)
	if err != nil {
		return fmt.Errorf("resolving --city: %w", err)
	}
	if eventsReemitExecutionControllerAliveHook(cityPath) != 0 {
		return fmt.Errorf("city controller is running")
	}
	if supervisorAliveHook() != 0 {
		running, _, known := supervisorCityRunningHook(cityPath)
		if !known {
			return fmt.Errorf("could not determine supervisor city state")
		}
		if running {
			return fmt.Errorf("city is running under the supervisor")
		}
	}

	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return fmt.Errorf("loading city config: %w", err)
	}
	if apply && (cfg.Events.Provider != "" || os.Getenv("GC_EVENTS") != "") {
		return fmt.Errorf("--apply requires the default file event provider")
	}
	store, err := openStoreAtForCityWithConfig(cityPath, cityPath, cfg)
	if err != nil {
		return fmt.Errorf("opening city work store: %w", err)
	}
	projection, err := executionevent.ProjectCurrent(
		beads.GraphStore{Store: resolveGraphStore(store, cfg, cityPath, nil)},
		beads.WorkStore{Store: store},
		strings.TrimSpace(runID),
	)
	if err != nil {
		return fmt.Errorf("projecting run %q: %w", runID, err)
	}
	facts := projection.Events("execution-reemit")
	if apply {
		recorder, err := newFileEventsRecorder(filepath.Join(cityPath, ".gc", "events.jsonl"), cfg.Events, io.Discard)
		if err != nil {
			return fmt.Errorf("opening event log: %w", err)
		}
		appendErr := recorder.AppendBatch(facts)
		closeErr := recorder.Close()
		if appendErr != nil || closeErr != nil {
			return fmt.Errorf("appending execution facts: %w", errors.Join(appendErr, closeErr))
		}
	}
	return writeCLIJSONLine(stdout, eventsReemitExecutionResult{
		RunID:      strings.TrimSpace(runID),
		RunCount:   1,
		WorkCount:  len(projection.WorkAssociations),
		StepCount:  len(projection.Steps),
		EventCount: len(facts),
		Applied:    apply,
	})
}

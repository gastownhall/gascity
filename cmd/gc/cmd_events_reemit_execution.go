package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/executionevent"
	"github.com/spf13/cobra"
)

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
	if err := requireStoppedExecutionReemitCity(cityPath); err != nil {
		return err
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

func requireStoppedExecutionReemitCity(cityPath string) error {
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		return fmt.Errorf("validating city config: %w", err)
	}
	runtimeDir := filepath.Join(cityPath, ".gc")
	if info, err := os.Stat(runtimeDir); err != nil || !info.IsDir() {
		if err != nil {
			return fmt.Errorf("validating city runtime: %w", err)
		}
		return fmt.Errorf("validating city runtime: not a directory")
	}
	lock, err := os.OpenFile(filepath.Join(runtimeDir, "controller.lock"), os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening controller lock: %w", err)
	}
	defer lock.Close() //nolint:errcheck // probe cleanup
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("city controller is running")
		}
		return fmt.Errorf("probing controller lock: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck // probe cleanup
	return nil
}

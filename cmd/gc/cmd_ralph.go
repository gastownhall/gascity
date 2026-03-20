package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/ralph"
	"github.com/spf13/cobra"
)

func newRalphCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ralph",
		Short: "Drive inline Ralph run/check loops",
	}
	cmd.AddCommand(newRalphTickCmd(stdout, stderr))
	return cmd
}

func newRalphTickCmd(stdout, stderr io.Writer) *cobra.Command {
	var runTarget string
	cmd := &cobra.Command{
		Use:   "tick",
		Short: "Process ready Ralph run/check beads in the current store",
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc ralph tick: %v\n", err) //nolint:errcheck
				return errExit
			}

			readDoltPort(cityPath)
			cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
			if err != nil {
				fmt.Fprintf(stderr, "gc ralph tick: loading config: %v\n", err) //nolint:errcheck
				return errExit
			}
			resolveRigPaths(cityPath, cfg.Rigs)

			store, err := openStoreAtForCity(cityPath, cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc ralph tick: opening store: %v\n", err) //nolint:errcheck
				return errExit
			}

			cityName := cfg.Workspace.Name
			if cityName == "" {
				cityName = filepath.Base(cityPath)
			}

			summary, err := runRalphTick(cityPath, cityName, cfg, store, runTarget, stdout, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc ralph tick: %v\n", err) //nolint:errcheck
				return errExit
			}
			_, _ = fmt.Fprintf(stdout, "ralph tick: routed=%d checked=%d passed=%d retried=%d failed=%d skipped=%d\n",
				summary.Routed, summary.Checked, summary.Passed, summary.Retried, summary.Failed, summary.Skipped)
			return nil
		},
	}
	cmd.Flags().StringVar(&runTarget, "run-target", "", "fallback run target when gc.run_target metadata is absent")
	return cmd
}

type ralphTickSummary struct {
	Routed  int
	Checked int
	Passed  int
	Retried int
	Failed  int
	Skipped int
}

func runRalphTick(cityPath, cityName string, cfg *config.City, store beads.Store, fallbackRunTarget string, stdout, stderr io.Writer) (ralphTickSummary, error) {
	ready, err := store.Ready()
	if err != nil {
		return ralphTickSummary{}, fmt.Errorf("listing ready work: %w", err)
	}

	work := make([]beads.Bead, 0, len(ready))
	for _, bead := range ready {
		kind := bead.Metadata["gc.kind"]
		if kind != "run" && kind != "check" {
			continue
		}
		if kind == "check" && bead.Metadata["gc.terminal"] == "true" {
			continue
		}
		work = append(work, bead)
	}

	sort.Slice(work, func(i, j int) bool {
		ik := work[i].Metadata["gc.kind"]
		jk := work[j].Metadata["gc.kind"]
		if ik != jk {
			return ik == "check"
		}
		if work[i].CreatedAt.Equal(work[j].CreatedAt) {
			return work[i].ID < work[j].ID
		}
		return work[i].CreatedAt.Before(work[j].CreatedAt)
	})

	var summary ralphTickSummary
	var errs []string
	for _, bead := range work {
		var changed bool
		var kindErr error
		switch bead.Metadata["gc.kind"] {
		case "run":
			changed, kindErr = routeRalphRun(bead, cityPath, cityName, cfg, store, fallbackRunTarget, stdout, stderr)
			if changed {
				summary.Routed++
			} else {
				summary.Skipped++
			}
		case "check":
			var result ralph.CheckResult
			result, kindErr = ralph.ProcessCheck(bead, cityPath, store)
			if result.Processed {
				summary.Checked++
				switch result.Action {
				case "pass":
					summary.Passed++
				case "retry":
					summary.Retried++
				case "fail":
					summary.Failed++
				}
			} else {
				summary.Skipped++
			}
		}
		if kindErr != nil {
			errs = append(errs, kindErr.Error())
			fmt.Fprintf(stderr, "gc ralph tick: %v\n", kindErr) //nolint:errcheck
		}
	}

	if _, workflowErr := ralph.CloseReadyWorkflowHeads(store); workflowErr != nil {
		errs = append(errs, workflowErr.Error())
		fmt.Fprintf(stderr, "gc ralph tick: %v\n", workflowErr) //nolint:errcheck
	}

	if len(errs) > 0 {
		return summary, fmt.Errorf("%d Ralph action(s) failed", len(errs))
	}
	return summary, nil
}

func routeRalphRun(bead beads.Bead, cityPath, cityName string, cfg *config.City, store beads.Store, fallbackRunTarget string, stdout, stderr io.Writer) (bool, error) {
	if bead.Metadata["gc.routed_to"] != "" {
		return false, nil
	}

	target := ralph.ResolveInheritedMetadata(store, bead, "gc.run_target")
	if target == "" {
		target = fallbackRunTarget
	}
	if target == "" {
		return false, fmt.Errorf("%s: missing gc.run_target metadata", bead.ID)
	}

	agent, ok := resolveAgentIdentity(cfg, target, currentRigContext(cfg))
	if !ok {
		return false, fmt.Errorf("%s: unknown run target %q", bead.ID, target)
	}

	deps := slingDeps{
		CityName: cityName,
		CityPath: cityPath,
		Cfg:      cfg,
		Runner:   shellSlingRunner,
		Store:    store,
		Stdout:   stdout,
		Stderr:   stderr,
	}
	opts := slingOpts{
		Target:        agent,
		BeadOrFormula: bead.ID,
		NoFormula:     true,
		NoConvoy:      true,
	}
	if code := doSling(opts, deps, store); code != 0 {
		return false, fmt.Errorf("%s: routing to %s failed", bead.ID, agent.QualifiedName())
	}
	if err := store.SetMetadata(bead.ID, "gc.routed_to", agent.QualifiedName()); err != nil {
		return true, fmt.Errorf("%s: recording route target: %w", bead.ID, err)
	}
	return true, nil
}

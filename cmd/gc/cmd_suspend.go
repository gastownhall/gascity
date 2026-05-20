package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/citystate"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rigstate"
	"github.com/spf13/cobra"
)

// newSuspendCmd creates the "gc suspend [path]" command.
func newSuspendCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "suspend [path]",
		Short: "Suspend the city (all agents effectively suspended)",
		Long: `Suspends the city by recording an explicit "suspended" preference
in .gc/runtime/city-state.json (per-clone runtime state, not committed).

This inherits downward — when the city is suspended, all agents are
effectively suspended regardless of their individual suspended fields.
The reconciler won't spawn agents, gc hook/prime return empty.

Use "gc resume" to restore.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSuspend(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// newResumeCmd creates the "gc resume [path]" command.
func newResumeCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "resume [path]",
		Short: "Resume a suspended city",
		Long: `Resume a suspended city by recording an explicit "resumed" preference
in .gc/runtime/city-state.json. The override sticks across city restarts
even when [workspace] declares suspended_on_start = true.

Restores normal operation: the reconciler will spawn agents again and
gc hook/prime will return work. Use "gc agent resume" to resume
individual agents, or "gc rig resume" for rigs.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdResume(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdSuspend is the CLI entry point for suspending the city.
func cmdSuspend(args []string, stdout, stderr io.Writer) int {
	cityPath, err := resolveSuspendDir(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if c := apiClient(cityPath); c != nil {
		err := c.SuspendCity()
		if err == nil {
			fmt.Fprintf(stdout, "City suspended (%s)\n", cityPath) //nolint:errcheck // best-effort stdout
			return 0
		}
		if !api.ShouldFallback(err) {
			fmt.Fprintf(stderr, "gc suspend: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Connection error — fall through to direct mutation.
	}
	return doSuspendCity(fsys.OSFS{}, cityPath, true, stdout, stderr)
}

// cmdResume is the CLI entry point for resuming the city.
func cmdResume(args []string, stdout, stderr io.Writer) int {
	cityPath, err := resolveSuspendDir(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if c := apiClient(cityPath); c != nil {
		err := c.ResumeCity()
		if err == nil {
			fmt.Fprintf(stdout, "City resumed (%s)\n", cityPath) //nolint:errcheck // best-effort stdout
			return 0
		}
		if !api.ShouldFallback(err) {
			fmt.Fprintf(stderr, "gc resume: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Connection error — fall through to direct mutation.
	}
	return doSuspendCity(fsys.OSFS{}, cityPath, false, stdout, stderr)
}

// resolveSuspendDir resolves the city directory from args or the current city.
func resolveSuspendDir(args []string) (string, error) {
	return resolveCommandCity(args)
}

// doSuspendCity records the explicit city suspension preference in
// .gc/runtime/city-state.json. The committable
// workspace.suspended_on_start flag is left untouched: callers
// explicit-suspend or explicit-resume via runtime state, and that
// override beats the committed default at every read.
func doSuspendCity(fs fsys.FS, cityPath string, suspend bool, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cmd := "gc suspend"
	if !suspend {
		cmd = "gc resume"
	}
	// Validate city.toml parses so an unrelated config error surfaces
	// clearly instead of being masked by the runtime-state write.
	if _, err := loadCityConfigForEditFS(fs, tomlPath); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	want := suspend
	if err := citystate.SetCitySuspended(fs, cityPath, &want); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	rec := openCityRecorder(stderr)
	if suspend {
		rec.Record(events.Event{
			Type:  events.CitySuspended,
			Actor: eventActor(),
		})
		fmt.Fprintf(stdout, "City suspended (%s)\n", cityPath) //nolint:errcheck // best-effort stdout
	} else {
		rec.Record(events.Event{
			Type:  events.CityResumed,
			Actor: eventActor(),
		})
		fmt.Fprintf(stdout, "City resumed (%s)\n", cityPath) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// citySuspended is the canonical predicate for "is the city suspended
// right now?". It loads the runtime city state from the ambient city
// (resolveCity) and merges it with workspace.suspended_on_start. The
// deprecated [workspace] suspended field is intentionally NOT consulted
// — `gc doctor` surfaces it as a deprecated-field warning.
//
// Callers that already have a pre-loaded [citystate.State] (e.g. the
// reconciler or snapshot builder) should call [citySuspendedWithState]
// instead to avoid the extra read.
func citySuspended(cfg *config.City) bool {
	cityPath, _ := resolveCity()
	return citySuspendedWithState(cfg, loadCitySuspensionStateBestEffort(cityPath))
}

// citySuspendedWithState is the pure form for callers that already
// loaded the runtime city state.
func citySuspendedWithState(cfg *config.City, citySt citystate.State) bool {
	return effectiveCitySuspended(cfg, citySt)
}

// loadCitySuspensionStateBestEffort reads runtime city state from disk
// and silently returns a zero state on any error. Suitable for the
// suspension predicates where misclassifying as "not suspended" is no
// worse than the pre-existing behavior.
func loadCitySuspensionStateBestEffort(cityPath string) citystate.State {
	if cityPath == "" {
		return citystate.State{}
	}
	st, _ := loadCitySuspensionState(fsys.OSFS{}, cityPath)
	return st
}

// isAgentEffectivelySuspended reports whether an agent is suspended.
// True if any of: city is suspended, agent is individually suspended,
// or the agent's rig is effectively suspended (runtime override or
// SuspendedOnStart). Suspension inherits downward.
//
// Callers that already have pre-loaded runtime state should call
// [isAgentEffectivelySuspendedWith] to avoid the per-call disk read.
func isAgentEffectivelySuspended(cfg *config.City, a *config.Agent) bool {
	cityPath, _ := resolveCity()
	return isAgentEffectivelySuspendedWith(
		cfg, a,
		loadCitySuspensionStateBestEffort(cityPath),
		loadRigSuspensionStateBestEffort(cityPath),
	)
}

// loadRigSuspensionStateBestEffort mirrors loadCitySuspensionStateBestEffort.
func loadRigSuspensionStateBestEffort(cityPath string) rigstate.SuspensionState {
	if cityPath == "" {
		return rigstate.SuspensionState{}
	}
	st, _ := loadRigSuspensionState(fsys.OSFS{}, cityPath)
	return st
}

// isAgentEffectivelySuspendedWith is like isAgentEffectivelySuspended
// but takes pre-loaded runtime state so callers in hot paths don't
// re-read the same files.
func isAgentEffectivelySuspendedWith(cfg *config.City, a *config.Agent, citySt citystate.State, suspState rigstate.SuspensionState) bool {
	if effectiveCitySuspended(cfg, citySt) {
		return true
	}
	if a.Suspended {
		return true
	}
	if a.Dir == "" {
		return false
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name != a.Dir {
			continue
		}
		if rigstate.EffectiveSuspended(suspState, cfg.Rigs[i].Name, cfg.Rigs[i].SuspendedOnStart) {
			return true
		}
		break
	}
	return false
}

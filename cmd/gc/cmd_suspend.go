package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/spf13/cobra"
)

// newSuspendCmd creates the "gc suspend [path]" command.
func newSuspendCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "suspend [path]",
		Short: "Suspend the city (all agents effectively suspended)",
		Long: `Suspends the city by recording an explicit "suspended" preference
in .gc/runtime/suspension-state.json (per-clone runtime state, not
committed).

This inherits downward — when the city is suspended, all agents are
effectively suspended regardless of their individual suspended fields.
The reconciler won't spawn agents, gc hook/prime return empty.

Use "gc resume" to restore.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSuspend(args, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL summary")
	return cmd
}

// newResumeCmd creates the "gc resume [path]" command.
func newResumeCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "resume [path]",
		Short: "Resume a suspended city",
		Long: `Resume a suspended city by recording an explicit "resumed" preference
in .gc/runtime/suspension-state.json. The override sticks across city
restarts even when [workspace] declares suspended_on_start = true.

Restores normal operation: the reconciler will spawn agents again and
gc hook/prime will return work. Use "gc agent resume" to resume
individual agents, or "gc rig resume" for rigs.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdResume(args, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL summary")
	return cmd
}

// cmdSuspend is the CLI entry point for suspending the city.
func cmdSuspend(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveSuspendDir(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if c := apiClient(cityPath); c != nil {
		err := c.SuspendCity()
		if err == nil {
			return writeCitySuspensionSuccess(stdout, stderr, cityPath, true, jsonOut)
		}
		if !api.ShouldFallback(err) {
			fmt.Fprintf(stderr, "gc suspend: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Connection error — fall through to direct mutation.
	}
	return doSuspendCity(fsys.OSFS{}, cityPath, true, jsonOut, stdout, stderr)
}

// cmdResume is the CLI entry point for resuming the city.
func cmdResume(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveSuspendDir(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if c := apiClient(cityPath); c != nil {
		err := c.ResumeCity()
		if err == nil {
			return writeCitySuspensionSuccess(stdout, stderr, cityPath, false, jsonOut)
		}
		if !api.ShouldFallback(err) {
			fmt.Fprintf(stderr, "gc resume: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Connection error — fall through to direct mutation.
	}
	return doSuspendCity(fsys.OSFS{}, cityPath, false, jsonOut, stdout, stderr)
}

// resolveSuspendDir resolves the city directory from args or the current city.
func resolveSuspendDir(args []string) (string, error) {
	return resolveCommandCity(args)
}

// doSuspendCity records the explicit city suspension preference in
// .gc/runtime/suspension-state.json. The committable
// workspace.suspended_on_start flag is left untouched: callers
// explicit-suspend or explicit-resume via runtime state, and that
// override beats the committed default at every read.
func doSuspendCity(fs fsys.FS, cityPath string, suspend bool, jsonOut bool, stdout, stderr io.Writer) int {
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
	if err := suspensionstate.SetCitySuspended(fs, cityPath, &want); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	rec := openCityRecorder(stderr)
	if suspend {
		rec.Record(events.Event{
			Type:  events.CitySuspended,
			Actor: eventActor(),
		})
	} else {
		rec.Record(events.Event{
			Type:  events.CityResumed,
			Actor: eventActor(),
		})
	}
	return writeCitySuspensionSuccess(stdout, stderr, cityPath, suspend, jsonOut)
}

func writeCitySuspensionSuccess(stdout, stderr io.Writer, cityPath string, suspend bool, jsonOut bool) int {
	if jsonOut {
		action := "resume"
		message := "City resumed."
		if suspend {
			action = "suspend"
			message = "City suspended."
		}
		return writeLifecycleActionJSONOrExit(stdout, stderr, "gc "+action, lifecycleActionJSON{
			Command:  action,
			Action:   action,
			Message:  message,
			CityPath: cityPath,
		})
	}
	if suspend {
		fmt.Fprintf(stdout, "City suspended (%s)\n", cityPath) //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintf(stdout, "City resumed (%s)\n", cityPath) //nolint:errcheck // best-effort stdout
	return 0
}

// citySuspended is the canonical predicate for "is the city suspended
// right now?". It loads the runtime suspension state from the
// ambient city (resolveCity) and merges it with the workspace's
// effective suspended_on_start. The deprecated `[workspace] suspended`
// field is honored as an alias for `suspended_on_start` via
// [config.Workspace.EffectiveSuspendedOnStart], so existing city.toml
// files keep their behavior on upgrade.
//
// Callers that already have a pre-loaded [suspensionstate.State]
// (e.g. the reconciler or snapshot builder) should call
// [citySuspendedWithState] instead to avoid the extra read.
func citySuspended(cfg *config.City) bool {
	cityPath, _ := resolveCity()
	return citySuspendedWithState(cfg, loadSuspensionStateBestEffort(cityPath))
}

// citySuspendedWithState is the pure form for callers that already
// loaded the runtime suspension state.
func citySuspendedWithState(cfg *config.City, st suspensionstate.State) bool {
	return effectiveCitySuspended(cfg, st)
}

// effectiveCitySuspended is the canonical "is the city suspended
// right now" predicate. It honors the GC_SUSPENDED=1 escape hatch
// (used by integration tests and ops to override without touching
// files), then the runtime state file, then falls back to the
// workspace's effective suspended_on_start (which honors the
// deprecated `suspended` field as an alias).
func effectiveCitySuspended(cfg *config.City, st suspensionstate.State) bool {
	if os.Getenv("GC_SUSPENDED") == "1" {
		return true
	}
	if cfg == nil {
		return suspensionstate.EffectiveCitySuspended(st, false)
	}
	return suspensionstate.EffectiveCitySuspended(st, cfg.Workspace.EffectiveSuspendedOnStart())
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
	return isAgentEffectivelySuspendedWith(cfg, a, loadSuspensionStateBestEffort(cityPath))
}

// isAgentEffectivelySuspendedWith is like isAgentEffectivelySuspended
// but takes a pre-loaded runtime state so callers in hot paths don't
// re-read the file.
func isAgentEffectivelySuspendedWith(cfg *config.City, a *config.Agent, st suspensionstate.State) bool {
	if effectiveCitySuspended(cfg, st) {
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
		if suspensionstate.EffectiveRigSuspended(st, cfg.Rigs[i].Name, cfg.Rigs[i].EffectiveSuspendedOnStart()) {
			return true
		}
		break
	}
	return false
}

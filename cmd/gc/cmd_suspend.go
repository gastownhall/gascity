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
	"github.com/spf13/cobra"
)

// newSuspendCmd creates the "gc suspend [path]" command.
func newSuspendCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "suspend [path]",
		Short: "Suspend the city (all agents effectively suspended)",
		Long: `Suspends the city by setting workspace.suspended = true in city.toml.

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
		Long: `Resume a suspended city by clearing workspace.suspended in city.toml.

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

// doSuspendCity sets or clears workspace.suspended in city.toml.
// The flag inherits downward: when true, all agents are effectively
// suspended via isAgentEffectivelySuspended and computeSuspendedNames.
func doSuspendCity(fs fsys.FS, cityPath string, suspend bool, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cmd := "gc suspend"
	if !suspend {
		cmd = "gc resume"
	}
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cfg.Workspace.Suspended = suspend

	content, err := cfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := fs.WriteFile(tomlPath, content, 0o644); err != nil {
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

// citySuspended checks whether the city is suspended. Returns true if
// GC_SUSPENDED=1 is set or cfg.Workspace.Suspended is true.
func citySuspended(cfg *config.City) bool {
	if os.Getenv("GC_SUSPENDED") == "1" {
		return true
	}
	return cfg.Workspace.Suspended
}

// isAgentEffectivelySuspended reports whether an agent is suspended.
// True if any of: city is suspended, agent is individually suspended,
// or the agent's rig is suspended. Suspension inherits downward.
func isAgentEffectivelySuspended(cfg *config.City, a *config.Agent) bool {
	if cfg.Workspace.Suspended {
		return true
	}
	if a.Suspended {
		return true
	}
	if a.Dir == "" {
		return false
	}
	for _, r := range cfg.Rigs {
		if r.Name == a.Dir && r.Suspended {
			return true
		}
	}
	return false
}

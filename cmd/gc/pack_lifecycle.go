package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packlifecycle"
)

// packLifecycleHookDirs returns every pack directory composed into cfg, city
// packs first and rig packs after in rig-name order, so hook execution order
// is stable across runs.
func packLifecycleHookDirs(cfg *config.City) []string {
	if cfg == nil {
		return nil
	}
	dirs := append([]string(nil), cfg.PackDirs...)
	rigNames := make([]string, 0, len(cfg.RigPackDirs))
	for name := range cfg.RigPackDirs {
		rigNames = append(rigNames, name)
	}
	sort.Strings(rigNames)
	for _, name := range rigNames {
		dirs = append(dirs, cfg.RigPackDirs[name]...)
	}
	return dirs
}

// packLifecycleHooks collects the hooks packs composed into cfg ship for event.
func packLifecycleHooks(cfg *config.City, event string) []packlifecycle.Hook {
	discovered := config.LoadPackLifecycleHooks(fsys.OSFS{}, packLifecycleHookDirs(cfg), event)
	if len(discovered) == 0 {
		return nil
	}
	hooks := make([]packlifecycle.Hook, 0, len(discovered))
	for _, d := range discovered {
		hooks = append(hooks, packlifecycle.Hook{
			Event:    d.Event,
			Script:   d.Script,
			PackDir:  d.PackDir,
			PackName: d.PackName,
		})
	}
	return hooks
}

// runPackLifecycleHooks runs the pack hooks registered for a city lifecycle
// event. Packs use these to bring pack-owned services (a systemd unit, a
// container, an external daemon) up with the city and down with it. Execution
// is best-effort and never changes the exit status: a hook that fails or hangs
// is reported on stderr so the operator can act, but it cannot wedge start or
// stop. logPrefix identifies the caller in those warnings ("gc stop").
func runPackLifecycleHooks(cityPath string, cfg *config.City, event string, stdout, stderr io.Writer) {
	hooks := packLifecycleHooks(cfg, event)
	if len(hooks) == 0 {
		return
	}
	logPrefix := "gc " + strings.TrimPrefix(event, "city-")
	for _, result := range packlifecycle.Run(context.Background(), cityPath, hooks, packlifecycle.DefaultTimeout) {
		if result.Err != nil {
			fmt.Fprintf(stderr, "%s: pack hook %s: %v\n", logPrefix, result.Name, result.Err) //nolint:errcheck // best-effort stderr
			if result.Output != "" {
				fmt.Fprintf(stderr, "%s: pack hook %s: %s\n", logPrefix, result.Name, result.Output) //nolint:errcheck // best-effort stderr
			}
			continue
		}
		if result.Output != "" {
			fmt.Fprintf(stdout, "pack hook %s: %s\n", result.Name, result.Output) //nolint:errcheck // best-effort stdout
		}
	}
}

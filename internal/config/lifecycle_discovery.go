package config

import (
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Lifecycle events a pack may hook. A pack ships a hook by placing an
// executable script at lifecycle/<event>.sh inside the pack directory.
const (
	// LifecycleEventCityStart runs after the city has finished starting.
	LifecycleEventCityStart = "city-start"
	// LifecycleEventCityStop runs during city teardown, after agent
	// sessions have been stopped.
	LifecycleEventCityStop = "city-stop"
)

// LifecycleEvents lists every supported lifecycle event in city order.
var LifecycleEvents = []string{LifecycleEventCityStart, LifecycleEventCityStop}

// lifecycleDirName is the pack subdirectory scanned for hook scripts.
const lifecycleDirName = "lifecycle"

// IsLifecycleEvent reports whether event names a supported lifecycle event.
func IsLifecycleEvent(event string) bool {
	for _, known := range LifecycleEvents {
		if known == event {
			return true
		}
	}
	return false
}

// DiscoveredLifecycleHook is a convention-discovered pack lifecycle hook:
// a pack-shipped script the city runs when a lifecycle event fires. Packs
// use hooks to bring pack-owned services (a systemd unit, a container, an
// external daemon) up and down with the city.
type DiscoveredLifecycleHook struct {
	// Event is the lifecycle event this hook runs on — one of LifecycleEvents.
	Event string
	// Script is the path to the hook script.
	Script string
	// PackDir is the pack directory the hook was discovered in. Hooks run
	// with this as their working directory.
	PackDir string
	// PackName is the pack's [pack] name, used for the hook's display name
	// and for pack runtime env injection.
	PackName string
}

// Name returns the hook's display name, "<pack>:<event>".
func (h DiscoveredLifecycleHook) Name() string { return h.PackName + ":" + h.Event }

// DiscoverPackLifecycleHooks scans a pack's lifecycle/ directory and returns
// the hooks it ships, ordered by LifecycleEvents. A pack with no lifecycle/
// directory, or with no script for any known event, yields no hooks. Entries
// that are not regular files (directories, dangling symlinks) are ignored, so
// an unrelated lifecycle/ layout cannot be executed by accident.
func DiscoverPackLifecycleHooks(fs fsys.FS, packDir, packName string) []DiscoveredLifecycleHook {
	var hooks []DiscoveredLifecycleHook
	for _, event := range LifecycleEvents {
		script := filepath.Join(packDir, lifecycleDirName, event+".sh")
		info, err := fs.Stat(script)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		hooks = append(hooks, DiscoveredLifecycleHook{
			Event:    event,
			Script:   script,
			PackDir:  packDir,
			PackName: packName,
		})
	}
	return hooks
}

// LoadPackLifecycleHooks reads pack.toml from each pack directory and returns
// the hooks those packs ship for the given event, in pack-directory order.
// Directories are deduplicated by absolute path. Directories without a
// readable pack.toml are skipped: they are not packs, so nothing there is
// eligible to run. An unknown event yields no hooks.
func LoadPackLifecycleHooks(fs fsys.FS, packDirs []string, event string) []DiscoveredLifecycleHook {
	if !IsLifecycleEvent(event) {
		return nil
	}
	seen := make(map[string]bool, len(packDirs))
	var hooks []DiscoveredLifecycleHook
	for _, dir := range packDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			absDir = dir
		}
		if seen[absDir] {
			continue
		}
		seen[absDir] = true

		data, err := fs.ReadFile(filepath.Join(dir, packFile))
		if err != nil {
			continue
		}
		var pc PackConfig
		if _, err := toml.Decode(string(data), &pc); err != nil {
			continue
		}
		for _, hook := range DiscoverPackLifecycleHooks(fs, dir, pc.Pack.Name) {
			if hook.Event == event {
				hooks = append(hooks, hook)
			}
		}
	}
	return hooks
}

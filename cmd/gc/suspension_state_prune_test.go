package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// seedRigOverrides records an explicit resume for each named rig, the shape a
// past `gc rig resume` leaves behind.
func seedRigOverrides(t *testing.T, cityPath string, names ...string) {
	t.Helper()
	resumed := false
	for _, name := range names {
		if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, name, &resumed); err != nil {
			t.Fatalf("seeding %q: %v", name, err)
		}
	}
}

func explicitRigOverride(t *testing.T, cityPath, name string) bool {
	t.Helper()
	st, err := suspensionstate.Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load suspension state: %v", err)
	}
	_, ok := suspensionstate.ExplicitRig(st, name)
	return ok
}

func pruneTestRuntime(cityPath string, cfg *config.City) (*CityRuntime, *bytes.Buffer) {
	var stderr bytes.Buffer
	return &CityRuntime{
		cityPath:    cityPath,
		cfg:         cfg,
		configDirty: &atomic.Bool{},
		stderr:      &stderr,
		logPrefix:   "gc test",
	}, &stderr
}

// A rig deregistered by hand-editing city.toml leaves its runtime override
// behind. Once the runtime has applied a config without that rig, the
// override must go, so re-registering the rig later honors its
// suspended_on_start instead of the stale explicit resume.
func TestPruneRemovedRigSuspensionOverridesAfterSuccessfulReload(t *testing.T) {
	for _, outcome := range []reloadOutcome{reloadOutcomeApplied, reloadOutcomeNoChange} {
		t.Run(string(outcome), func(t *testing.T) {
			dir := t.TempDir()
			seedRigOverrides(t, dir, "kept", "removed")
			cr, stderr := pruneTestRuntime(dir, &config.City{Rigs: []config.Rig{{Name: "kept"}}})

			cr.pruneRemovedRigSuspensionOverrides(reloadControlReply{Outcome: outcome})

			if explicitRigOverride(t, dir, "removed") {
				t.Fatal("override for rig absent from the applied config survived")
			}
			if !explicitRigOverride(t, dir, "kept") {
				t.Fatal("override for a declared rig was pruned")
			}
			if !strings.Contains(stderr.String(), `"removed"`) {
				t.Fatalf("prune not logged; stderr = %q", stderr.String())
			}
		})
	}
}

func TestPruneRemovedRigSuspensionOverridesSkipsUnsafeViews(t *testing.T) {
	cases := map[string]func(cr *CityRuntime) reloadControlReply{
		"failed reload keeps old config": func(*CityRuntime) reloadControlReply {
			return reloadControlReply{Outcome: reloadOutcomeFailed}
		},
		"config changed during reload": func(cr *CityRuntime) reloadControlReply {
			cr.configDirty.Store(true)
			return reloadControlReply{Outcome: reloadOutcomeApplied}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			seedRigOverrides(t, dir, "removed")
			cr, _ := pruneTestRuntime(dir, &config.City{})

			cr.pruneRemovedRigSuspensionOverrides(setup(cr))

			if !explicitRigOverride(t, dir, "removed") {
				t.Fatal("override pruned from an unsafe config view")
			}
		})
	}
}

// A rig declared in city.toml but missing its .gc/site.toml path binding is
// still a declared rig: the loaded config keeps it (with an empty path), so
// its runtime override must survive the prune.
func TestPruneRemovedRigSuspensionOverridesKeepsRigWithoutSiteBinding(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test\"\n\n[[rigs]]\nname = \"unbound\"\nsuspended_on_start = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	seedRigOverrides(t, dir, "unbound")
	cr, _ := pruneTestRuntime(dir, cfg)

	cr.pruneRemovedRigSuspensionOverrides(reloadControlReply{Outcome: reloadOutcomeApplied})

	if !explicitRigOverride(t, dir, "unbound") {
		t.Fatal("override for a declared rig lacking a site binding was pruned")
	}
}

// End to end through the reload entry point: a city whose config no longer
// declares a rig drops that rig's stale override on a successful reload.
func TestReloadConfigTracedPrunesOverridesForUndeclaredRigs(t *testing.T) {
	clearInheritedBeadsEnv(t)
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_SESSION", "")

	dir := shortSocketTempDir(t, "gc-reload-susp-prune-")
	disableManagedDoltRecoveryForTest(t)
	cleanupManagedDoltTestCity(t, dir)
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := tryReloadConfig(tomlPath, "test", dir)
	if err != nil {
		t.Fatal(err)
	}
	applyFeatureFlags(result.Cfg)
	seedRigOverrides(t, dir, "hand-removed")

	var stdout, stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:    dir,
		cityName:    "test",
		configName:  "test",
		tomlPath:    tomlPath,
		configRev:   result.Revision,
		cfg:         result.Cfg,
		configDirty: &atomic.Bool{},
		sp:          runtime.NewFake(),
		dops:        newDrainOps(runtime.NewFake()),
		rec:         events.Discard,
		stdout:      &stdout,
		stderr:      &stderr,
		logPrefix:   "gc test",
	}
	lastProviderName := result.Cfg.Session.Provider

	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, dir, nil, reloadSourceManual)
	if reply.Outcome == reloadOutcomeFailed {
		t.Fatalf("reload failed: %s; stderr=%q", reply.Error, stderr.String())
	}
	if explicitRigOverride(t, dir, "hand-removed") {
		t.Fatalf("override for undeclared rig survived reload (outcome %q); stderr=%q", reply.Outcome, stderr.String())
	}
}

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// writeSplitCityConfig writes a city.toml whose five infrastructure classes
// share one SQLite binding at bindingPath. extra is appended verbatim so a
// reload case can change something OUTSIDE [storage].
func writeSplitCityConfig(t *testing.T, tomlPath, bindingPath, extra string) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	var buf strings.Builder
	buf.WriteString("[workspace]\nname = \"test-city\"\n\n")
	buf.WriteString("[beads]\nprovider = \"file\"\n\n")
	buf.WriteString("[session]\nprovider = \"fake\"\n\n")
	buf.WriteString("[storage.classes]\nwork = \"work\"\ngraph = \"infra\"\nsessions = \"infra\"\nmessaging = \"infra\"\norders = \"infra\"\nnudges = \"infra\"\n\n")
	fmt.Fprintf(&buf, "[storage.bindings.infra]\nprovider = %q\npath = %q\n", config.StorageProviderSQLiteBeads, bindingPath)
	buf.WriteString(extra)
	if err := os.WriteFile(tomlPath, []byte(buf.String()), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// bootSplitCityForReload boots a city runtime from a city.toml carrying a
// SQLite infrastructure split, and returns it with the path of the config it
// booted from.
func bootSplitCityForReload(t *testing.T) (*CityRuntime, string, *bytes.Buffer) {
	t.Helper()
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeSplitCityConfig(t, tomlPath, filepath.Join(t.TempDir(), "store"), "")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	stubInfraMigrationSource(t)

	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if cr.storageRoutes == nil {
		t.Fatal("the split city opened no storage routes, so a reload of it proves nothing")
	}
	return cr, tomlPath, &stderr
}

// TestCityRuntimeReloadRefusesAChangedStorageSection pins the claim the storage
// boot gate's header makes: [storage] is decided once, and a reload that would
// change it is refused rather than applied.
//
// The refusal has to happen before anything else in the reload runs. The engine
// this process opened is the one every relocated class resolver already points
// at, so a reload that accepted a different binding would leave a live city
// reading one database while its configuration named another.
func TestCityRuntimeReloadRefusesAChangedStorageSection(t *testing.T) {
	cr, tomlPath, stderr := bootSplitCityForReload(t)
	bootedBinding := cr.cfg.Storage.Bindings["infra"].Path
	bootedRoutes := cr.storageRoutes

	writeSplitCityConfig(t, tomlPath, filepath.Join(t.TempDir(), "moved"), "")
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cr.cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q; a live [storage] swap was accepted", reply.Outcome, reloadOutcomeFailed)
	}
	if !strings.Contains(reply.Error, "[storage]") || !strings.Contains(reply.Error, "restart") {
		t.Errorf("the refusal does not say [storage] changed and needs a restart: %q", reply.Error)
	}
	if !strings.Contains(stderr.String(), "[storage]") {
		t.Errorf("the refusal was not reported to the operator: %q", stderr.String())
	}
	if got := cr.cfg.Storage.Bindings["infra"].Path; got != bootedBinding {
		t.Errorf("the refused reload still swapped the configured binding: %q -> %q", bootedBinding, got)
	}
	if cr.storageRoutes != bootedRoutes {
		t.Error("the refused reload replaced the opened routes")
	}
}

// TestCityRuntimeReloadAcceptsAnUnchangedStorageSection is the other half: the
// guard must fire on a CHANGED [storage] and on nothing else, or every ordinary
// reload of a split city becomes a refusal.
func TestCityRuntimeReloadAcceptsAnUnchangedStorageSection(t *testing.T) {
	cr, tomlPath, stderr := bootSplitCityForReload(t)
	binding := cr.cfg.Storage.Bindings["infra"].Path

	// The same binding, a different daemon setting: a reload with real work to
	// do that leaves [storage] exactly as it was.
	writeSplitCityConfig(t, tomlPath, binding, "\n[daemon]\nshutdown_timeout = \"7s\"\n")
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cr.cityPath, nil, reloadSourceManual)

	if reply.Outcome == reloadOutcomeFailed {
		t.Fatalf("an unchanged [storage] failed the reload: %q (stderr %q)", reply.Error, stderr.String())
	}
	if strings.Contains(stderr.String(), "[storage] changed") {
		t.Errorf("an unchanged [storage] was reported as changed: %q", stderr.String())
	}
	if got := cr.cfg.Daemon.ShutdownTimeout; got != "7s" {
		t.Errorf("shutdown_timeout = %q, want 7s; the reload did not apply", got)
	}
}

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A config reload that rebuilds the wisp GC tracker must keep the configured
// cadence: a GC that ran 10 minutes ago on a 1h interval is not due again.
func TestCityRuntimeReloadKeepsWispGCCadenceRepro(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	base := "[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nwisp_gc_interval = \"1h\"\nwisp_ttl = \"1h\"\n"
	if err := os.WriteFile(tomlPath, []byte(base), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		LogPrefix: "gc reload",
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()

	prevStart := cityRuntimeStartBeadsLifecycle
	cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, cityStoreRecord, io.Writer) error { return nil }
	t.Cleanup(func() { cityRuntimeStartBeadsLifecycle = prevStart })

	cr.wg = newWispGCForCity(cfg, cr.cityName)
	m, ok := cr.wg.(*memoryWispGC)
	if !ok {
		t.Fatalf("wisp GC tracker = %T, want *memoryWispGC", cr.wg)
	}
	lastRun := time.Now()
	m.lastRun = lastRun

	oldRev := cr.configRev
	if err := os.WriteFile(tomlPath, []byte(base+"shutdown_timeout = \"1s\"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reply.Outcome = %q (err %q), want %q", reply.Outcome, reply.Error, reloadOutcomeApplied)
	}
	if cr.configRev == oldRev {
		t.Fatalf("configRev unchanged after reload")
	}
	if cr.wg == nil {
		t.Fatal("wisp GC disabled after reload; want it kept")
	}
	if cr.wg.shouldRun(lastRun.Add(10 * time.Minute)) {
		t.Fatal("wisp GC due 10m after its last run, right after a config reload; want the 1h cadence kept")
	}
}

package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// newResumeTickRuntime builds the bare CityRuntime the managed-Dolt tick
// tests use, with the resume detector reading the scripted clock fc.
func newResumeTickRuntime(t *testing.T, fc *fakeResumeClock, stderr *bytes.Buffer) *CityRuntime {
	t.Helper()
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "bd")
	stubManagedDoltStoreOpeners(t)

	cityPath := t.TempDir()
	cleanupManagedDoltTestCity(t, cityPath)
	sp := runtime.NewFake()
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: "test-city",
		cfg:      &config.City{},
		sp:       sp,
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		dops:              newDrainOps(sp),
		rec:               events.Discard,
		sessionDrains:     newDrainTracker(),
		logPrefix:         "gc test",
		stdout:            &bytes.Buffer{},
		stderr:            stderr,
		managedDoltHealth: func(context.Context, string) error { return nil },
		managedDoltOwned:  func(string) (bool, error) { return true, nil },
		managedDoltPort:   func(string) string { return "" },
		resume:            newResumeDetector(fc.read),
	}
	cs := newControllerState(context.Background(), cr.cfg, sp, events.NewFake(), "test-city", cr.cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	return cr
}

func tickResumeRuntime(cr *CityRuntime) {
	dirty := &atomic.Bool{}
	lastProviderName := ""
	prevPoolRunning := map[string]bool{}
	cr.tick(context.Background(), dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")
}

// TestCityRuntimeTick_DetectsResumeAndFloorsActivity: the tick evaluates the
// detector once, against the configured patrol interval, before any phase
// reads activity, and says so on stderr.
func TestCityRuntimeTick_DetectsResumeAndFloorsActivity(t *testing.T) {
	fc := newFakeResumeClock()
	var stderr bytes.Buffer
	cr := newResumeTickRuntime(t, fc, &stderr)

	tickResumeRuntime(cr)
	if !cr.activityFloor().at.IsZero() {
		t.Fatal("floor set before any resume")
	}
	fc.tick(30 * time.Second)
	fc.suspend(72 * time.Hour)
	tickResumeRuntime(cr)

	if got := cr.activityFloor().at; !got.Equal(fc.wall) {
		t.Fatalf("floor = %v, want the resume pass's wall time %v", got, fc.wall)
	}
	if !strings.Contains(stderr.String(), "host resume detected") {
		t.Fatalf("stderr = %q, want a host-resume line", stderr.String())
	}
}

// TestCityRuntimeTick_FloorSurvivesLaterQuietTicks: the floor stays at the
// resume instant across later quiet ticks, and the stderr line is written once.
func TestCityRuntimeTick_FloorSurvivesLaterQuietTicks(t *testing.T) {
	fc := newFakeResumeClock()
	var stderr bytes.Buffer
	cr := newResumeTickRuntime(t, fc, &stderr)

	tickResumeRuntime(cr)
	fc.suspend(time.Hour)
	tickResumeRuntime(cr)
	want := cr.activityFloor().at
	fc.tick(30 * time.Second)
	tickResumeRuntime(cr)
	if got := cr.activityFloor().at; !got.Equal(want) {
		t.Fatalf("floor moved from %v to %v on a quiet tick", want, got)
	}
	if n := strings.Count(stderr.String(), "host resume detected"); n != 1 {
		t.Fatalf("resume lines = %d, want 1", n)
	}
}

func TestCityRuntimeActivityFloor_NilDetectorIsZeroFloor(t *testing.T) {
	cr := &CityRuntime{}
	if got := cr.activityFloor(); !got.at.IsZero() {
		t.Fatalf("activityFloor() = %v on a runtime with no detector, want zero", got.at)
	}
}

// A config reload replaces the runtime's config and provider but not its
// detector: the floor a resume set survives the reload, and the detection count
// a second consumer compares against is not reset.
func TestCityRuntimeReload_KeepsResumeFloorAndDetectionCount(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
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
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()

	fc := newFakeResumeClock()
	cr.resume = newResumeDetector(fc.read)
	cr.observeResume()
	fc.suspend(72 * time.Hour)
	cr.observeResume()
	wantFloor := cr.activityFloor()
	if !wantFloor.at.Equal(fc.wall) {
		t.Fatalf("test setup: floor = %v, want the resume pass's wall time %v", wantFloor.at, fc.wall)
	}
	wantCount := cr.resume.detections()
	if wantCount != 1 {
		t.Fatalf("test setup: detections = %d, want 1", wantCount)
	}
	detector := cr.resume

	writeCityRuntimeConfig(t, tomlPath, "fail")
	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)

	if lastProviderName != "fail" {
		t.Fatalf("lastProviderName = %q, want fail: the reload did not run", lastProviderName)
	}
	if cr.resume != detector {
		t.Fatal("reload replaced the resume detector")
	}
	if got := cr.activityFloor(); !got.at.Equal(wantFloor.at) {
		t.Fatalf("floor = %v after reload, want %v", got.at, wantFloor.at)
	}
	if got := cr.resume.detections(); got != wantCount {
		t.Fatalf("detections = %d after reload, want %d", got, wantCount)
	}
}

// The control-dispatcher tick reconciles between main ticks, so it runs the
// detector itself: a resume it is the first pass to follow sets the floor.
func TestCityRuntimeControlDispatcherTick_DetectsResume(t *testing.T) {
	fc := newFakeResumeClock()
	var stderr bytes.Buffer
	cr := newResumeTickRuntime(t, fc, &stderr)

	cr.controlDispatcherTick(context.Background())
	fc.tick(30 * time.Second)
	fc.suspend(72 * time.Hour)
	cr.controlDispatcherTick(context.Background())

	if got := cr.activityFloor().at; !got.Equal(fc.wall) {
		t.Fatalf("floor = %v, want the resume pass's wall time %v", got, fc.wall)
	}
}

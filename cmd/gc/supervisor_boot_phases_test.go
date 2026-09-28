package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// fakeBootStepClock advances a fixed step on every reading, so every timed boot
// phase reports a duration over the reporting threshold without any test
// sleeping. It is read from the per-city start workers, so it is atomic.
type fakeBootStepClock struct {
	reads atomic.Int64
	step  time.Duration
	base  time.Time
}

func (c *fakeBootStepClock) now() time.Time {
	n := c.reads.Add(1)
	return c.base.Add(time.Duration(n) * c.step)
}

// TestSupervisorBootPhasesCoverEveryPostPrepareStep pins the boot-instrumentation
// contract: every step of a city start that can take real time reports under a
// named phase.
//
// The gap this closes was not theoretical. A 16:54 fleet boot spent ~18 minutes
// inside a stretch of the start path that emitted no phase at all, so the only
// thing an operator could tell from the supervisor log was that the city was
// somewhere between "opening_controller_state" and "running_pool_on_boot"
// (ga-1e78j). Three steps ran unnamed in that stretch — the stale name-claim
// sweep, the bead-event watcher plus maintenance loop, and the orphan
// rig-provision sweep — and each of them touches the city's bead store.
//
// The clock is injected rather than slept through: runPostPrepareStep only
// prints a phase whose duration clears its threshold, so a fake clock that
// advances on every read makes the log deterministic and instantaneous.
func TestSupervisorBootPhasesCoverEveryPostPrepareStep(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := shortSocketTempDir(t, "gc-boot-phases-")
	cleanupManagedDoltTestCity(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "phase-city"

[orders]
skip = ["beads-health", "cross-rig-deps", "gate-sweep", "jsonl-export", "reaper", "order-tracking-sweep", "orphan-sweep", "prune-branches", "spawn-storm-detect", "wisp-compact"]

[session]
provider = "fake"

[daemon]
shutdown_timeout = "100ms"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	script := writeSpyScript(t, filepath.Join(t.TempDir(), "ops.log"))
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	clock := &fakeBootStepClock{step: 2 * time.Second, base: time.Now()}
	prevNow := supervisorBootStepNow
	supervisorBootStepNow = clock.now
	t.Cleanup(func() { supervisorBootStepNow = prevNow })

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "phase-city"); err != nil {
		t.Fatal(err)
	}

	cr := newCityRegistry()
	var stdout, stderr bytes.Buffer
	reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)
	t.Cleanup(func() {
		if done := cr.CancelCity(canonicalTestPath(cityPath)); done != nil {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("city goroutine did not exit in time")
			}
		}
	})

	log := stderr.String()
	if !strings.Contains(log, "Launching city") && !strings.Contains(stdout.String(), "Launching city") {
		t.Fatalf("city never finished starting, so the boot phases prove nothing (stderr: %s)", log)
	}

	// The phases the start path already had, plus the three that ran unnamed.
	// Listed in boot order; the order assertion below is what makes a phase
	// attached to the wrong step visible.
	phases := []string{
		"opening_controller_state",
		"releasing_stale_name_claims",
		"starting_bead_event_watcher",
		"sweeping_rig_provisions",
		"running_pool_on_boot",
	}
	prev := -1
	for _, phase := range phases {
		want := "city 'phase-city': " + phase + " took "
		at := strings.Index(log, want)
		if at < 0 {
			t.Fatalf("boot log has no %q phase line; an unnamed boot step is an uninstrumented gap in the start path (stderr: %s)", phase, log)
		}
		if at < prev {
			t.Fatalf("phase %q was logged out of boot order (stderr: %s)", phase, log)
		}
		prev = at
	}
}

// TestReconcileCitiesRescuesACityWhoseStartPanicked drives the REAL startOneCity
// to a panic partway through its body and proves the city is retryable
// afterwards.
//
// This is the wedge the conditional marker release left open. startOneCity
// replaces the queued marker with "loading_config" and then with each phase
// name, so by the time most of its body runs the marker no longer says
// "queued". A panic from there skips recordInitFailure and publishManagedCity
// alike -- neither clear runs -- and a release that only fired on the queued
// marker found a phase name and did nothing. selectCitiesToStart skips any path
// that holds an initStatus entry, and it consults that skip before either
// backoff, so the city was never selected again for the life of the supervisor:
// no retry, no backoff, and nothing an operator could do short of restarting
// the supervisor.
//
// The panic is injected through the boot-step clock, which puts it inside
// runPostPrepareStep -- after that step has already written its phase name --
// so the marker under test is a real phase and not the queued value.
func TestReconcileCitiesRescuesACityWhoseStartPanicked(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := shortSocketTempDir(t, "gc-panic-city-")
	cleanupManagedDoltTestCity(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "panic-city"

[orders]
skip = ["beads-health", "cross-rig-deps", "gate-sweep", "jsonl-export", "reaper", "order-tracking-sweep", "orphan-sweep", "prune-branches", "spawn-storm-detect", "wisp-compact"]

[session]
provider = "fake"

[daemon]
shutdown_timeout = "100ms"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	script := writeSpyScript(t, filepath.Join(t.TempDir(), "ops.log"))
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	// Blow up on the first timed boot step. runPostPrepareStep records the phase
	// name before it reads the clock, so the city is holding a phase marker --
	// not the queued one -- at the moment it panics.
	prevNow := supervisorBootStepNow
	supervisorBootStepNow = func() time.Time { panic("boot step exploded") }
	t.Cleanup(func() { supervisorBootStepNow = prevNow })

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "panic-city"); err != nil {
		t.Fatal(err)
	}

	cr := newCityRegistry()
	var stdout, stderr bytes.Buffer
	reconcileCities(context.Background(), reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)

	if !strings.Contains(stderr.String(), "city 'panic-city': start panicked: boot step exploded") {
		t.Fatalf("the city start did not panic where this test needs it to, so nothing below is proven (stderr: %s)", stderr.String())
	}

	key := canonicalTestPath(cityPath)
	if progress, ok := initStatusEntry(cr, key); ok {
		t.Fatalf("initStatus[%s] = %+v after the start panicked, want absent; the city is wedged out of every future reconcile pass", key, progress)
	}

	// The operative consequence: the next pass selects it again.
	desired := map[string]supervisor.CityEntry{key: {Path: key, Name: "panic-city"}}
	if selected := selectCitiesToStart(cr, desired, nil); len(selected) != 1 {
		t.Fatalf("the next selection pass picked %v, want the panicked city retried", startedNames(selected))
	}
}

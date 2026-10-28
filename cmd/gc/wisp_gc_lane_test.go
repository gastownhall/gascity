package main

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// countingWispGC counts purge runs; due says whether shouldRun answers yes.
type countingWispGC struct {
	due  bool
	runs int
}

func (c *countingWispGC) shouldRun(time.Time) bool { return c.due }

func (c *countingWispGC) runGC(beads.GraphStore, beads.SessionStore, beads.MailStore, time.Time) (int, error) {
	c.runs++
	return 0, nil
}

func newWispGCLaneRuntime(t *testing.T, wg wispGC) *CityRuntime {
	t.Helper()
	return &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "test-city",
		cfg:                 &config.City{},
		sp:                  runtime.NewFake(),
		standaloneCityStore: beads.NewMemStore(),
		wg:                  wg,
		rec:                 events.Discard,
		logPrefix:           "test-city",
		stdout:              io.Discard,
		stderr:              io.Discard,
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}
}

// The purge is not a step of the tick, so a long one cannot delay the
// session decisions that wait for the next tick (#2604).
func TestTick_DoesNotRunWispGC(t *testing.T) {
	wg := &countingWispGC{due: true}
	cr := newWispGCLaneRuntime(t, wg)

	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "test")

	if wg.runs != 0 {
		t.Fatalf("tick ran wisp GC %d time(s), want 0: the purge belongs to its lane", wg.runs)
	}
}

func TestWispGCLanePass_RunsOnlyWhenDue(t *testing.T) {
	wg := &countingWispGC{}
	cr := newWispGCLaneRuntime(t, wg)
	var pressureSkips int

	cr.runWispGCLanePass(&pressureSkips)
	if wg.runs != 0 {
		t.Fatalf("pass ran wisp GC %d time(s) before it was due, want 0", wg.runs)
	}
	wg.due = true
	cr.runWispGCLanePass(&pressureSkips)
	if wg.runs != 1 {
		t.Fatalf("pass ran wisp GC %d time(s) once due, want 1", wg.runs)
	}
}

func TestWispGCLanePass_SkipsQuiescentCity(t *testing.T) {
	wg := &countingWispGC{due: true}
	cr := newWispGCLaneRuntime(t, wg)
	cr.beadsQuiescent.Store(true)
	var pressureSkips int

	cr.runWispGCLanePass(&pressureSkips)
	if wg.runs != 0 {
		t.Fatalf("pass ran wisp GC %d time(s) on a quiescent city, want 0", wg.runs)
	}
}

// blockingWispGC holds its first purge until release is closed, so a test can
// act while a pass is in flight.
type blockingWispGC struct {
	started  chan struct{}
	release  chan struct{}
	runs     atomic.Int32
	finished atomic.Bool
}

func (b *blockingWispGC) shouldRun(time.Time) bool { return b.runs.Load() == 0 }

func (b *blockingWispGC) runGC(beads.GraphStore, beads.SessionStore, beads.MailStore, time.Time) (int, error) {
	if b.runs.Add(1) == 1 {
		close(b.started)
		<-b.release
		b.finished.Store(true)
	}
	return 0, nil
}

// A tick completes while a purge is in flight on the lane, and stopping the
// lane waits for that purge, so run() closes the stores and the trace only
// after it (#2604).
func TestWispGCLane_TickCompletesWhilePurgeBlocked(t *testing.T) {
	wg := &blockingWispGC{started: make(chan struct{}), release: make(chan struct{})}
	cr := newWispGCLaneRuntime(t, wg)
	cr.cfg = &config.City{Daemon: config.DaemonConfig{PatrolInterval: "10ms"}}

	stop := cr.startWispGCLane(context.Background())
	released := false
	defer func() {
		if !released {
			close(wg.release)
			stop()
		}
	}()
	select {
	case <-wg.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the lane never started a purge")
	}

	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		var dirty atomic.Bool
		var lastProviderName string
		var prevPoolRunning map[string]bool
		cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "test")
	}()
	select {
	case <-tickDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the tick did not complete while the purge was blocked")
	}
	if wg.finished.Load() {
		t.Fatal("the purge finished before it was released")
	}

	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		stop()
	}()
	select {
	case <-stopDone:
		t.Fatal("stopping the lane returned while a purge was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	released = true
	close(wg.release)
	select {
	case <-stopDone:
	case <-time.After(10 * time.Second):
		t.Fatal("stopping the lane did not return after the purge was released")
	}
	if !wg.finished.Load() {
		t.Fatal("stopping the lane returned before the purge in flight finished")
	}
}

// The lane reads the config and the wisp GC as a pair, and a publish replaces
// both together, so a pass never pairs a new config with the old GC policy.
func TestPublishRuntimeConfig_PublishesWispGCWithConfig(t *testing.T) {
	first := &config.City{Daemon: config.DaemonConfig{WispGCInterval: "1h", WispTTL: "1h"}}
	cr := newWispGCLaneRuntime(t, newWispGCForConfig(first))
	cr.cfg = first

	next := &config.City{Daemon: config.DaemonConfig{WispGCInterval: "2h", WispTTL: "1h"}}
	if !cr.publishRuntimeConfig(next, cr.sp, cr.dops, "rev-2") {
		t.Fatal("publishRuntimeConfig rejected the config")
	}
	cfg, wg := cr.wispGCSnapshot()
	if cfg != next {
		t.Fatal("snapshot config is not the published config")
	}
	mem, ok := wg.(*memoryWispGC)
	if !ok {
		t.Fatalf("snapshot wisp GC is %T, want *memoryWispGC", wg)
	}
	if mem.interval != 2*time.Hour {
		t.Fatalf("snapshot wisp GC interval = %s, want 2h from the published config", mem.interval)
	}

	off := &config.City{}
	if !cr.publishRuntimeConfig(off, cr.sp, cr.dops, "rev-3") {
		t.Fatal("publishRuntimeConfig rejected the config")
	}
	if cfg, wg := cr.wispGCSnapshot(); cfg != off || wg != nil {
		t.Fatalf("snapshot = (%p, %v), want (%p, nil) for a config with wisp GC off", cfg, wg, off)
	}
}

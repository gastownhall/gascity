package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The start-deferral memo is one per city, and the main reconcile tick is the
// only entry point that records into it. The control-dispatcher tick runs the
// same start executor, but only over the control-dispatcher rows; a completed
// executor run replaces the memo's whole record, so a control-dispatcher tick
// that shared the memo would erase the pool creates the main tick had just
// deferred (#7170, ga-17zmyec).

// newControlDispatcherRuntime builds a CityRuntime whose only agent is the
// control dispatcher, so cr.controlDispatcherTick runs the real
// control-dispatcher-only reconcile against in-memory stores.
func newControlDispatcherRuntime(t *testing.T) *CityRuntime {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv(fsPressureThresholdEnv, "100")
	cityPath := t.TempDir()
	maxActive := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              config.ControlDispatcherAgentName,
			BindingName:       "core",
			StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
			MaxActiveSessions: &maxActive,
		}},
	}
	sp := runtime.NewFake()
	cr := &CityRuntime{
		cityPath:      cityPath,
		cityName:      "test-city",
		cfg:           cfg,
		sp:            sp,
		dops:          newDrainOps(sp),
		rec:           events.Discard,
		sessionDrains: newDrainTracker(),
		logPrefix:     "gc test",
		stdout:        io.Discard,
		stderr:        io.Discard,
	}
	cr.buildFnWithSessionBeads = supervisorBuildAgentsFnWithSessionBeads(cityPath, "test-city", io.Discard)
	cr.setControllerState(&controllerState{
		cfg:           cfg,
		sp:            sp,
		beadStores:    map[string]beads.Store{},
		cityBeadStore: beads.NewMemStore(),
		eventProv:     events.NewFake(),
		cityName:      "test-city",
		cityPath:      cityPath,
	})
	return cr
}

// memoPassStart returns the memo's lastPassStart.
func memoPassStart(m *startDeferralMemo) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastPassStart
}

func TestControlDispatcherTickLeavesTheMainTicksDeferralsAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// settle runs one tick first so the dispatcher is already running
		// and the measured tick has no start candidate at all.
		settle bool
	}{
		{"with a start candidate", false},
		{"idle, nothing to start", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := newControlDispatcherRuntime(t)
			ctx := context.Background()
			if tc.settle {
				cr.controlDispatcherTick(ctx)
				if !cr.waitForAsyncStarts() {
					t.Fatal("the dispatcher start did not settle")
				}
			}
			memo := cr.ensureStartDeferralMemo()
			mainPassStart := time.Now().Add(-time.Minute)
			commitDeferralPass(memo, mainPassStart, "gc-pool-deferred")

			cr.controlDispatcherTick(ctx)
			cr.waitForAsyncStarts()

			if got := committedDeferrals(memo); len(got) != 1 || got[0] != "gc-pool-deferred" {
				t.Errorf("a control-dispatcher tick replaced the main tick's deferrals: committed = %v, want [gc-pool-deferred]", got)
			}
			if got := memoPassStart(memo); !got.Equal(mainPassStart) {
				t.Errorf("a control-dispatcher tick moved the main tick's lastPassStart %s -> %s", mainPassStart.Format(time.RFC3339Nano), got.Format(time.RFC3339Nano))
			}
		})
	}
}

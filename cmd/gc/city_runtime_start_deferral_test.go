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
			commitSeenDeferralPass(memo, []string{"gc-pool-deferred", "gc-pool-waiting"}, "gc-pool-deferred")

			cr.controlDispatcherTick(ctx)
			cr.waitForAsyncStarts()

			if got := committedDeferrals(memo); len(got) != 1 || got[0] != "gc-pool-deferred" {
				t.Errorf("a control-dispatcher tick replaced the main tick's deferrals: committed = %v, want [gc-pool-deferred]", got)
			}
			if got := seenDeferrals(memo); len(got) != 2 || got[0] != "gc-pool-deferred" || got[1] != "gc-pool-waiting" {
				t.Errorf("a control-dispatcher tick replaced the main tick's snapshot record: seen = %v, want [gc-pool-deferred gc-pool-waiting]", got)
			}
		})
	}
}

// poolCreateRuntime builds a CityRuntime over two desired, never-started pool
// creates under max_wakes_per_tick = 1, so one main tick starts one and defers
// the other at a start gate.
func poolCreateRuntime(t *testing.T) (cr *CityRuntime, store beads.Store, result DesiredStateResult, snapshot *sessionBeadSnapshot, ids map[string]string) {
	t.Helper()
	cityPath := t.TempDir()
	store = beads.NewMemStore()
	one := 1
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon:    config.DaemonConfig{MaxWakesPerTick: &one},
		Agents: []config.Agent{
			{Name: "a", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
			{Name: "b", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
		},
	}
	sp := runtime.NewFake()
	cr = &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test-city",
		cfg:                 cfg,
		sp:                  sp,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	result = DesiredStateResult{State: map[string]TemplateParams{}, ScaleCheckCounts: map[string]int{}}
	ids = map[string]string{}
	var rows []beads.Bead
	for _, name := range []string{"a", "b"} {
		b, err := store.Create(beads.Bead{
			Title:  name,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "template:" + name},
			Metadata: map[string]string{
				"session_name":          name,
				"session_name_explicit": "true",
				"pending_create_claim":  "true",
				"template":              name,
				"state":                 "start-pending",
				"generation":            "1",
				"continuation_epoch":    "1",
				"instance_token":        "tok-" + name,
			},
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		ids[name] = b.ID
		rows = append(rows, b)
		result.State[name] = TemplateParams{Command: "true", SessionName: name, TemplateName: name}
		result.ScaleCheckCounts[name] = 1
	}
	return cr, store, result, newSessionBeadSnapshot(rows), ids
}

// The main tick passes the city's memo to the executor, so a create the start
// gate deferred is in the memo the stale-session reaper and the next tick read.
func TestCityRuntimeMainTickRecordsItsStartGateDeferralsInTheCityMemo(t *testing.T) {
	cr, _, result, snapshot, ids := poolCreateRuntime(t)

	cr.beadReconcileTick(context.Background(), newSeatWork(cr.workLegs()), result, snapshot, nil, false)
	cr.waitForAsyncStarts()

	var waiting string
	for name, id := range ids {
		if !cr.sp.IsRunning(name) {
			waiting = id
		}
	}
	if waiting == "" {
		t.Fatal("premise: the one-wake budget left no create waiting")
	}
	memo := cr.ensureStartDeferralMemo()
	if got := committedDeferrals(memo); len(got) != 1 || got[0] != waiting {
		t.Fatalf("committed = %v, want [%s]: the main tick did not record its start-gate deferral into the city's memo", got, waiting)
	}
	if again := cr.ensureStartDeferralMemo(); again != memo {
		t.Fatal("ensureStartDeferralMemo built a second memo: the tick and the reaper would not see each other's record")
	}
}

// The reaper reads the same memo: a never-started create past its lease that
// the memo holds survives, and one it does not hold is reaped.
func TestCityRuntimeReapStaleSessionBeadsReadsTheCityMemo(t *testing.T) {
	cr, store, _, _, ids := poolCreateRuntime(t)
	stale := time.Now().Add(-(pendingCreateNeverStartedTimeout + time.Minute)).UTC().Format(time.RFC3339)
	for _, id := range ids {
		if err := store.SetMetadataBatch(id, map[string]string{"state": "creating", "pending_create_started_at": stale}); err != nil {
			t.Fatalf("SetMetadataBatch(%s): %v", id, err)
		}
	}
	// A pass whose snapshot held both rows, so neither has a first chance
	// left; only the committed id is held.
	commitSeenDeferralPass(cr.ensureStartDeferralMemo(), []string{ids["a"], ids["b"]}, ids["a"])

	if n := cr.reapStaleSessionBeads(newSeatWork(cr.workLegs())); n != 1 {
		t.Fatalf("reaped %d rows, want 1 (the one the memo does not hold)", n)
	}
	for name, wantStatus := range map[string]string{"a": "open", "b": "closed"} {
		got, err := store.Get(ids[name])
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		if got.Status != wantStatus {
			t.Errorf("create %s: status = %q, want %q", name, got.Status, wantStatus)
		}
	}
}

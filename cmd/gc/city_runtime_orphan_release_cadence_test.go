package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestShouldRunOrphanRelease is a table-driven test for the pure decision
// function behind the cadence gate on release_orphaned_pool_assignments — the
// interim mitigation for ga-57er0d (skip the sweep on most reconcile ticks
// instead of running it every tick). Superseded by ga-8uf72n's convergence-lane
// fix; remove this gate then.
func TestShouldRunOrphanRelease(t *testing.T) {
	const minInterval = 5 * time.Minute
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		last time.Time
		want bool
	}{
		{"never run before (zero last) always runs", time.Time{}, true},
		{"just ran, well inside the interval", now.Add(-1 * time.Second), false},
		{"ran just under the interval ago", now.Add(-(minInterval - time.Second)), false},
		{"ran exactly at the interval boundary", now.Add(-minInterval), true},
		{"ran well over the interval ago", now.Add(-2 * minInterval), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRunOrphanRelease(now, tc.last, minInterval)
			if got != tc.want {
				t.Errorf("shouldRunOrphanRelease(now, %v, %v) = %v, want %v", tc.last, minInterval, got, tc.want)
			}
		})
	}
}

// orphanCadenceReleaseFixture creates a single genuinely orphaned pool work
// bead: in_progress, routed to the "worker" template, assigned to a session
// identity with no matching session bead anywhere — the shape
// releaseOrphanedPoolAssignments actually reopens (mirrors
// TestReleaseOrphanedPoolAssignmentsWhenSnapshotsComplete_PartialSkipsCompleteReleases
// in cmd_start_test.go and countOrphanReleaseLiveSessionLists in
// pool_session_name_orphan_release_fanout_test.go). This is deliberately NOT
// the W1/W2 shape in city_runtime_release_callsite_test.go — those are
// protected/retained by design; this fixture is meant to be released whenever
// the sweep actually runs, so a cadence-gate test can tell "gated" from
// "cured" apart.
func orphanCadenceReleaseFixture(t *testing.T) (*beads.MemStore, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:    "cadence-gate orphaned pool work",
		Assignee: "worker-dead-cadence",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatalf("create orphan work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark orphan work in_progress: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload orphan work bead: %v", err)
	}
	return store, work
}

func newOrphanCadenceTestRuntime(store beads.Store) *CityRuntime {
	cfg := &config.City{Agents: []config.Agent{{
		Name:              "worker",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(2),
	}}}
	return &CityRuntime{
		cityName:            "orphan-cadence-test-city",
		cfg:                 cfg,
		sp:                  runtime.NewFake(),
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
}

// TestBeadReconcileTick_OrphanReleaseCadenceGate_SkipsWithinMinInterval proves
// the cadence gate actually withholds the release sweep on a tick that lands
// inside orphanReleaseMinInterval of the last run: a genuinely orphaned work
// bead (no live session anywhere) must stay untouched.
func TestBeadReconcileTick_OrphanReleaseCadenceGate_SkipsWithinMinInterval(t *testing.T) {
	store, work := orphanCadenceReleaseFixture(t)
	cr := newOrphanCadenceTestRuntime(store)
	cr.orphanReleaseLast = time.Now() // "just ran" — the next tick must skip.

	result := DesiredStateResult{
		State:                 map[string]TemplateParams{},
		AssignedWorkBeads:     []beads.Bead{work},
		AssignedWorkStores:    []beads.Store{store},
		AssignedWorkStoreRefs: []string{""},
	}

	cr.beadReconcileTick(context.Background(), result, newSessionBeadSnapshot(nil), nil, false)

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work after skipped tick: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-dead-cadence" {
		t.Fatalf("orphan release ran on a tick inside the cadence's minimum interval: status=%q assignee=%q, want unchanged (in_progress, worker-dead-cadence)",
			got.Status, got.Assignee)
	}
}

// TestBeadReconcileTick_OrphanReleaseCadenceGate_RunsOnDueTick proves the gate
// still lets the real release sweep run, unchanged, once the minimum interval
// has elapsed (here: never run before, so the tick is due immediately) — the
// same genuinely orphaned work bead must be reopened exactly as it would be
// without the gate.
func TestBeadReconcileTick_OrphanReleaseCadenceGate_RunsOnDueTick(t *testing.T) {
	store, work := orphanCadenceReleaseFixture(t)
	cr := newOrphanCadenceTestRuntime(store)
	// cr.orphanReleaseLast left at its zero value: never run before, due now.

	result := DesiredStateResult{
		State:                 map[string]TemplateParams{},
		AssignedWorkBeads:     []beads.Bead{work},
		AssignedWorkStores:    []beads.Store{store},
		AssignedWorkStoreRefs: []string{""},
	}

	cr.beadReconcileTick(context.Background(), result, newSessionBeadSnapshot(nil), nil, false)

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work after due tick: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("orphan release did not run on a due tick: status=%q assignee=%q, want released (open, unassigned)",
			got.Status, got.Assignee)
	}
}

// TestBeadReconcileTick_OrphanReleaseCadenceGate_StampsCompletionNotStart
// proves the cadence gate measures orphanReleaseLast from when the sweep
// finished, not from when it started. A gate that stamps orphanReleaseLast
// with a pre-call timestamp gives no real throttling once the sweep's own
// duration approaches or exceeds orphanReleaseMinInterval — the production
// sweep this mitigates has been measured at ~328s against a 5m/300s
// interval (ga-57er0d), so the elapsed time computed from a start-time stamp
// alone already clears the gate on the very next tick, even though the
// sweep only just finished. Deploy-gate criterion-2 FAIL on ga-p69yam.
func TestBeadReconcileTick_OrphanReleaseCadenceGate_StampsCompletionNotStart(t *testing.T) {
	store, work := orphanCadenceReleaseFixture(t)
	cr := newOrphanCadenceTestRuntime(store)

	start := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	sweepDuration := 328 * time.Second // observed production sweep duration
	finish := start.Add(sweepDuration) // sweepDuration > orphanReleaseMinInterval (5m)

	// The gate consults the clock once to decide whether to run (returns
	// start — due, since orphanReleaseLast is zero) and, on a fix that stamps
	// completion, a second time after the sweep returns (returns finish).
	// This models a sweep whose own duration exceeds the interval without an
	// actual 328s sleep in the test.
	calls := 0
	cr.orphanReleaseNowFn = func() time.Time {
		calls++
		if calls == 1 {
			return start
		}
		return finish
	}

	result := DesiredStateResult{
		State:                 map[string]TemplateParams{},
		AssignedWorkBeads:     []beads.Bead{work},
		AssignedWorkStores:    []beads.Store{store},
		AssignedWorkStoreRefs: []string{""},
	}
	cr.beadReconcileTick(context.Background(), result, newSessionBeadSnapshot(nil), nil, false)

	if got, want := cr.orphanReleaseLast, finish; !got.Equal(want) {
		t.Fatalf("orphanReleaseLast = %v, want %v (completion time, not start time %v) — a start-time stamp gives the cadence gate no real throttling once a sweep's own duration exceeds orphanReleaseMinInterval",
			got, want, start)
	}

	// A second tick landing 1s after the sweep actually finished — well
	// inside the 5m interval — must be gated (skip), proving the throttle
	// survives a slow sweep instead of firing back-to-back.
	store2, work2 := orphanCadenceReleaseFixture(t)
	cr2 := newOrphanCadenceTestRuntime(store2)
	cr2.orphanReleaseLast = finish
	justAfterFinish := finish.Add(1 * time.Second)
	cr2.orphanReleaseNowFn = func() time.Time { return justAfterFinish }

	result2 := DesiredStateResult{
		State:                 map[string]TemplateParams{},
		AssignedWorkBeads:     []beads.Bead{work2},
		AssignedWorkStores:    []beads.Store{store2},
		AssignedWorkStoreRefs: []string{""},
	}
	cr2.beadReconcileTick(context.Background(), result2, newSessionBeadSnapshot(nil), nil, false)

	got2, err := store2.Get(work2.ID)
	if err != nil {
		t.Fatalf("get work after second tick: %v", err)
	}
	if got2.Status != "in_progress" || got2.Assignee != "worker-dead-cadence" {
		t.Fatalf("orphan release ran 1s after a slow sweep finished (still within the interval): status=%q assignee=%q, want unchanged (in_progress, worker-dead-cadence) — proves the gate throttles off completion time, not a stale start-time stamp",
			got2.Status, got2.Assignee)
	}
}

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The tick-level resume-floor tests below pin the wiring the reconciler- and
// backstop-level floor tests cannot: that each controller tick hands the
// floor its resume detector holds to the pass that reads wall-clock activity.
// Each has a no-resume control on the same fixture, so a green test cannot be
// the fixture failing to reach the code under test.

// resumedWallClock is a scripted resume clock whose wall time follows the real
// clock, because the ticks under test read time.Now(). suspend moves only the
// wall clock forward, as a host suspend does.
func resumedWallClock() *fakeResumeClock {
	return &fakeResumeClock{wall: time.Now().Add(-72 * time.Hour)}
}

// simulateHostSuspend runs one quiet observation, then a 72h suspend that ends
// at the real present, and observes again: the detector fires and the floor
// becomes the resume instant.
func simulateHostSuspend(t *testing.T, cr *CityRuntime, fc *fakeResumeClock) {
	t.Helper()
	cr.resume = newResumeDetector(fc.read)
	cr.observeResume()
	fc.suspend(72 * time.Hour)
	cr.observeResume()
	if cr.activityFloor().at.IsZero() {
		t.Fatal("test setup: the scripted suspend was not detected")
	}
}

// newIdleTimeoutTracker is the runtime's idle tracker with a 15m idle timeout
// on name.
func newIdleTimeoutTracker(name string) *memoryIdleTracker {
	it := newIdleTracker()
	it.setTimeout(name, 15*time.Minute)
	return it
}

// stopCalls counts the runtime Stop calls for name.
func stopCalls(sp interface{ CountCalls(string, string) int }, name string) int {
	return sp.CountCalls("Stop", name)
}

// newResumeMainTickRuntime is a one-agent pool runtime with a running, quiet
// worker session, for driving beadReconcileTick. The worker's activity stamp
// is 72h old.
func newResumeMainTickRuntime(t *testing.T) (*CityRuntime, *runtime.Fake, beads.Bead, DesiredStateResult) {
	t.Helper()
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"session_name": "worker-1", "template": "worker", "agent_name": "worker-1", "pool_managed": "true", "pool_slot": "1", "state": "awake",
		"instance_token": "tok-1", "generation": "1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	sp.SetActivity("worker-1", time.Now().Add(-72*time.Hour))
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1), StartCommand: "true"}}}
	cr := &CityRuntime{
		cityPath: t.TempDir(), cityName: "test-city", cfg: cfg, sp: sp, dops: newDrainOps(sp), standaloneCityStore: store,
		sessionDrains: newDrainTracker(), rec: events.Discard, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		it: newIdleTimeoutTracker("worker-1"),
	}
	result := DesiredStateResult{
		State:             map[string]TemplateParams{"worker-1": {TemplateName: "worker", SessionName: "worker-1", Command: "true"}},
		PoolDesiredCounts: map[string]int{"worker": 1},
	}
	return cr, sp, session, result
}

func runResumeMainTick(cr *CityRuntime, result DesiredStateResult, session beads.Bead) {
	cr.beadReconcileTick(context.Background(), newSeatWork(cr.workLegs()), result, newSessionBeadSnapshot([]beads.Bead{session}), nil, false)
}

// D1. The main bead-reconcile tick passes the resume floor to the session
// reconciler: a quiet session whose 72h-old activity stamp predates a detected
// resume is not stopped for idle_timeout on the pass after a suspend. Kills
// dropping withActivityFloor from beadReconcileTick's reconcile options.
func TestBeadReconcileTick_FloorsIdleTimeoutAfterHostResume(t *testing.T) {
	t.Run("no resume stops the quiet session", func(t *testing.T) {
		cr, sp, session, result := newResumeMainTickRuntime(t)
		runResumeMainTick(cr, result, session)
		if got := stopCalls(sp, "worker-1"); got == 0 {
			t.Fatalf("control: the quiet worker was not stopped for idle_timeout; the fixture does not reach the idle path\nstderr: %s", cr.stderr)
		}
	})
	t.Run("a detected resume spares it", func(t *testing.T) {
		cr, sp, session, result := newResumeMainTickRuntime(t)
		simulateHostSuspend(t, cr, resumedWallClock())
		runResumeMainTick(cr, result, session)
		if got := stopCalls(sp, "worker-1"); got != 0 {
			t.Fatalf("the quiet worker was stopped %d time(s) on the pass after a host resume; idle time is measured from the resume\nstderr: %s", got, cr.stderr)
		}
	})
}

// D2. The control-dispatcher tick passes the resume floor to its own
// reconcile: a running dispatcher whose activity stamp predates a detected
// resume is not stopped for idle_timeout. Kills dropping withActivityFloor
// from controlDispatcherTick.
func TestControlDispatcherTick_FloorsIdleTimeoutAfterHostResume(t *testing.T) {
	run := func(t *testing.T, suspend bool) (stops int, stderr string) {
		t.Helper()
		var errBuf bytes.Buffer
		clk := newEndpointCapacityGuard(time.Now)
		cr, sp := newCapacityRefusingRuntime(t, config.ControlDispatcherAgentName, config.ControlDispatcherStartCommandFor("{{.Agent}}"), clk)
		sp.refusing = false
		cr.stderr = &errBuf
		// A dispatcher held at its minimum is never idle-stopped, so the one
		// under test runs on demand: an open control bead routed to it.
		cr.cfg.Agents[0].MinActiveSessions = intPtr(0)
		if _, err := cr.cityBeadStore().Create(beads.Bead{Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
			beadmeta.KindMetadataKey:     beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey: config.ControlDispatcherAgentName,
		}}); err != nil {
			t.Fatal(err)
		}

		cr.controlDispatcherTick(context.Background()) // materializes and starts the dispatcher
		if !cr.waitForAsyncStarts() {
			t.Fatal("the dispatcher start did not settle")
		}
		var name string
		for _, call := range sp.SnapshotCalls() {
			if call.Method == "Start" {
				name = call.Name
			}
		}
		if name == "" {
			t.Fatalf("test setup: no dispatcher Start; stderr: %s", errBuf.String())
		}
		sp.SetActivity(name, time.Now().Add(-72*time.Hour))
		cr.it = newIdleTimeoutTracker(name)
		if suspend {
			simulateHostSuspend(t, cr, resumedWallClock())
		}
		cr.controlDispatcherTick(context.Background())
		return stopCalls(sp.Fake, name), errBuf.String()
	}
	t.Run("no resume stops the quiet dispatcher", func(t *testing.T) {
		if stops, stderr := run(t, false); stops == 0 {
			t.Fatalf("control: the quiet dispatcher was not stopped for idle_timeout; the fixture does not reach the idle path\nstderr: %s", stderr)
		}
	})
	t.Run("a detected resume spares it", func(t *testing.T) {
		if stops, stderr := run(t, true); stops != 0 {
			t.Fatalf("the quiet dispatcher was stopped %d time(s) on the pass after a host resume; idle time is measured from the resume\nstderr: %s", stops, stderr)
		}
	})
}

// D3. The main tick passes the resume floor to the execution backstop: a
// claim holder whose nudges are spent and whose activity stamp predates a
// detected resume is not drained for a stalled execution. Kills passing a
// zero floor (or the raw stamp) to nudgeStalledPoolExecution from
// beadReconcileTick.
func TestBeadReconcileTick_FloorsExecutionBackstopAfterHostResume(t *testing.T) {
	setup := func(t *testing.T) (*CityRuntime, beads.Bead, DesiredStateResult) {
		t.Helper()
		cr, _, session, result := newResumeMainTickRuntime(t)
		cr.it = nil // the idle-timeout path is D1's; this tick isolates the backstop
		store := cr.standaloneCityStore
		work, err := store.Create(beads.Bead{
			Title: "claimed step",
			Type:  "task",
			Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey:  "root-1",
				beadmeta.StepIDMetadataKey:      "step-1",
				beadmeta.SessionIDMetadataKey:   session.ID,
				beadmeta.SessionNameMetadataKey: "worker-1",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		inProgress, assignee := "in_progress", "worker-1"
		if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
			t.Fatal(err)
		}
		claimed, err := store.Get(work.ID)
		if err != nil {
			t.Fatal(err)
		}
		// The slot has spent every nudge on this claim in the past hour and
		// has been quiet since: the next quiet pass escalates.
		if err := store.SetMetadataBatch(session.ID, map[string]string{
			executionClaimNudgeWorkKey:     claimed.ID,
			executionClaimNudgeStoreRefKey: normalizeIdleClaimStoreRef("city"),
			executionClaimNudgeCountKey:    "3",
			executionClaimNudgeAtKey:       time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			executionClaimNudgeTokenKey:    "tok-1",
		}); err != nil {
			t.Fatal(err)
		}
		session, err = store.Get(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		result.AssignedWorkBeads = []beads.Bead{claimed}
		result.AssignedWorkStores = []beads.Store{store}
		result.AssignedWorkStoreRefs = []string{"city"}
		return cr, session, result
	}
	drained := func(cr *CityRuntime, session beads.Bead) bool {
		ds := cr.sessionDrains.get(session.ID)
		return ds != nil && ds.reason == executionStalledDrainReason
	}
	t.Run("no resume drains the stalled holder", func(t *testing.T) {
		cr, session, result := setup(t)
		runResumeMainTick(cr, result, session)
		if !drained(cr, session) {
			t.Fatalf("control: the stalled claim holder was not drained; the fixture does not reach the backstop\nstdout: %s\nstderr: %s", cr.stdout, cr.stderr)
		}
	})
	t.Run("a detected resume holds it", func(t *testing.T) {
		cr, session, result := setup(t)
		simulateHostSuspend(t, cr, resumedWallClock())
		runResumeMainTick(cr, result, session)
		if drained(cr, session) {
			t.Fatalf("the claim holder was drained on the pass after a host resume; quiet time is measured from the resume\nstdout: %s", strings.TrimSpace(cr.stdout.(*bytes.Buffer).String()))
		}
	})
}

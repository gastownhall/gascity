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

// startupKickoffLaneStepName is the external-reads lane step that runs the
// named/direct startup backstop under reconciler v2.
const startupKickoffLaneStepName = "startup-kickoff"

// startupKickoffWiringFixture is a running named session whose startup
// kickoff began at startedAt, bound to an open step, on a store holding both
// beads.
func startupKickoffWiringFixture(t *testing.T, startedAt time.Time) (beads.Store, beads.Bead, beads.Bead, *runtime.Fake) {
	t.Helper()
	store := beads.NewMemStore()
	step, err := store.Create(beads.Bead{Title: "step", Type: "task", Metadata: map[string]string{"gc.routed_to": "agent-a"}})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(beads.Bead{Title: "agent-a", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"session_name": "session-a", "template": "agent-a", "agent_name": "agent-a", "state": "awake",
		"instance_token": "tok-1", "generation": "1",
		beadmeta.BoundStepIDMetadataKey: step.ID,
		startupKickoffStateKey:          startupKickoffStatePending,
		startupKickoffStartedAtKey:      startedAt.UTC().Format(time.RFC3339),
		startupKickoffAttemptsKey:       "0",
	}})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "session-a", runtime.Config{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	sp.SetActivity("session-a", startedAt)
	return store, step, sess, sp
}

func startupKickoffWiringCfg() *config.City {
	return &config.City{Agents: []config.Agent{{
		Name:              "agent-a",
		Nudge:             "Run gc hook --claim --json now.",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(1),
		StartCommand:      "true",
	}}}
}

func startupKickoffTickRuntime(store beads.Store, sp runtime.Provider, cfg *config.City) *CityRuntime {
	return &CityRuntime{
		cityPath: "", cityName: "test-city", cfg: cfg, sp: sp, standaloneCityStore: store,
		sessionDrains: newDrainTracker(), rec: events.Discard, stdout: io.Discard, stderr: io.Discard,
	}
}

func startupKickoffTickResult(step beads.Bead, partial bool) DesiredStateResult {
	return DesiredStateResult{
		State:                              map[string]TemplateParams{"session-a": {TemplateName: "agent-a", SessionName: "session-a", Command: "true"}},
		ReadyUnassignedRoutedWorkBeads:     []beads.Bead{step},
		ReadyUnassignedRoutedWorkStoreRefs: []string{"city"},
		StoreQueryPartial:                  partial,
	}
}

// Reaches the beadReconcileTick call site: the controller's bead reconcile
// tick nudges a named session whose bound step is still open past the grace,
// and a partial work snapshot makes the same tick do nothing for it.
func TestBeadReconcileTickNudgesOutstandingStartupKickoff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		partial   bool
		wantNudge int
		wantState string
	}{
		{name: "complete snapshot", wantNudge: 1, wantState: startupKickoffStateNudged},
		{name: "partial snapshot", partial: true, wantNudge: 0, wantState: startupKickoffStatePending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, step, sess, sp := startupKickoffWiringFixture(t, time.Now().Add(-time.Hour))
			cr := startupKickoffTickRuntime(store, sp, startupKickoffWiringCfg())
			cr.cityPath = t.TempDir()
			cr.beadReconcileTick(context.Background(), newSeatWork(cr.workLegs()), startupKickoffTickResult(step, tc.partial), newSessionBeadSnapshot([]beads.Bead{sess}), nil, false)
			if got := sp.CountCalls("Nudge", "session-a"); got != tc.wantNudge {
				t.Fatalf("Nudge calls = %d, want %d", got, tc.wantNudge)
			}
			if got := mustGetTestBead(t, store, sess.ID).Metadata[startupKickoffStateKey]; got != tc.wantState {
				t.Fatalf("startup kickoff state = %q, want %q", got, tc.wantState)
			}
		})
	}
}

// Reconciler v2 reaches the backstop through the external-reads lane: the
// startup-kickoff step is registered with the pool steps and runs in every
// work_ladder mode, over the planner's last allocation summary. A partial or
// stale summary does nothing.
func TestStartupKickoffLaneStep(t *testing.T) {
	for _, mode := range []string{"on", "off"} {
		for _, tc := range []struct {
			name      string
			edit      func(*allocSummary)
			wantNudge int
		}{
			{name: "open bound step", edit: func(*allocSummary) {}, wantNudge: 1},
			{name: "partial summary", edit: func(s *allocSummary) { s.Partial = true }},
			{name: "stale summary", edit: func(s *allocSummary) { s.At = s.At.Add(-time.Hour) }},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				store, step, _, sp := startupKickoffWiringFixture(t, stepsTestNow.Add(-time.Hour))
				cfg := startupKickoffWiringCfg()
				cfg.Daemon.WorkLadder = mode
				sum := &allocSummary{At: stepsTestNow, ReadyRouted: []beads.Bead{step}, ReadyRoutedRefs: []string{""}}
				tc.edit(sum)
				env := externalReadsEnv{Cfg: cfg, CityStore: store, SP: sp}
				lane, _ := newPoolStepsTestLane(env, func() *allocSummary { return sum }, nil)
				lane.events = events.NewFake()
				var run func(context.Context, externalReadsEnv)
				for _, st := range lane.steps {
					if st.name == startupKickoffLaneStepName {
						run = st.run
					}
				}
				if run == nil {
					t.Fatalf("lane has no %q step: the backstop is unreachable under reconciler v2", startupKickoffLaneStepName)
				}
				run(context.Background(), env)
				if got := sp.CountCalls("Nudge", "session-a"); got != tc.wantNudge {
					t.Fatalf("Nudge calls = %d, want %d", got, tc.wantNudge)
				}
			})
		}
	}
}

package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func TestReconcileSessionBeadsReportsStartSelectionComparisons(t *testing.T) {
	tests := []struct {
		name               string
		running            bool
		mutate             func(*reconcilerTestEnv, *beads.Bead)
		wantPlan           sessionLifecycleStartSelectionOutcome
		wantReason         sessionLifecycleStartSelectionReason
		wantLegacySelected bool
	}{
		{
			name: "ready candidate selected",
			mutate: func(env *reconcilerTestEnv, sessionBead *beads.Bead) {
				env.markSessionCreating(sessionBead)
			},
			wantPlan:           sessionLifecycleStartSelectionPrepare,
			wantReason:         sessionLifecycleStartSelectionReasonReady,
			wantLegacySelected: true,
		},
		{
			name:    "already running not selected",
			running: true,
			mutate: func(env *reconcilerTestEnv, sessionBead *beads.Bead) {
				env.setSessionMetadata(sessionBead, map[string]string{"pin_awake": "true"})
			},
			wantPlan:   sessionLifecycleStartSelectionNoop,
			wantReason: sessionLifecycleStartSelectionReasonAlreadyRunning,
		},
		{
			name:       "no wake reason not selected",
			wantPlan:   sessionLifecycleStartSelectionNoop,
			wantReason: sessionLifecycleStartSelectionReasonNotNeeded,
		},
		{
			name: "pending create not selected",
			mutate: func(env *reconcilerTestEnv, sessionBead *beads.Bead) {
				env.setSessionMetadata(sessionBead, map[string]string{
					"state":                     string(session.StateCreating),
					"pending_create_claim":      "true",
					"pending_create_started_at": env.clk.Now().Format(time.RFC3339),
					"last_woke_at":              env.clk.Now().Format(time.RFC3339),
				})
			},
			wantPlan:   sessionLifecycleStartSelectionPark,
			wantReason: sessionLifecycleStartSelectionReasonStartInFlight,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", tt.running)
			sessionBead := env.createSessionBead("worker", "worker")
			if tt.mutate != nil {
				tt.mutate(env, &sessionBead)
			}
			var comparisons []sessionLifecycleStartSelectionComparison
			env.startOptions = append(env.startOptions,
				withSessionLifecycleStartSelectionComparisonObserver(func(comparison sessionLifecycleStartSelectionComparison) {
					comparisons = append(comparisons, comparison)
				}),
			)

			env.reconcile([]beads.Bead{sessionBead})

			if len(comparisons) != 1 {
				t.Fatalf("comparison count = %d, want 1; comparisons=%+v stderr=%q", len(comparisons), comparisons, env.stderr.String())
			}
			assertSingleStartSelectionComparison(
				t,
				comparisons,
				sessionBead.ID,
				tt.wantPlan,
				tt.wantReason,
				tt.wantLegacySelected,
			)
		})
	}
}

func TestReconcileSessionBeadsStartSelectionCopiesLegacyCircuitResult(t *testing.T) {
	env := newReconcilerTestEnv()
	configureAlwaysNamedSession(env)
	env.addDesired("session-a", "template-a", false)

	cb := breakerAt(30*time.Minute, 5)
	const identity = "rig-a/session-a"
	now := env.clk.Now().UTC()
	for i := 0; i < 6; i++ {
		cb.RecordRestart(identity, now.Add(-time.Duration(6-i)*time.Minute))
	}
	if !cb.IsOpen(identity, now) {
		t.Fatal("precondition: circuit breaker is closed")
	}
	defer setSessionCircuitBreakerForTest(cb)()

	sessionBead := createCircuitTestNamedSession(t, env, "creating")
	var comparisons []sessionLifecycleStartSelectionComparison
	env.startOptions = append(env.startOptions,
		withSessionLifecycleStartSelectionComparisonObserver(func(comparison sessionLifecycleStartSelectionComparison) {
			comparisons = append(comparisons, comparison)
		}),
	)

	if woken := env.reconcile([]beads.Bead{sessionBead}); woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	assertSingleStartSelectionComparison(
		t,
		comparisons,
		sessionBead.ID,
		sessionLifecycleStartSelectionPark,
		sessionLifecycleStartSelectionReasonCircuitOpen,
		false,
	)
}

func TestReconcileSessionBeadsStartSelectionCopiesLegacyProviderHealthResult(t *testing.T) {
	cityPath := t.TempDir()
	writeHealthCache(t, cityPath, "provider-red", "unhealthy", nowSecs())

	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.desiredState["worker"] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  "worker",
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			Name: "provider-red",
		},
	}
	sessionBead := env.createSessionBead("worker", "worker")
	env.markSessionCreating(&sessionBead)

	var comparisons []sessionLifecycleStartSelectionComparison
	env.startOptions = append(env.startOptions,
		withSessionLifecycleStartSelectionComparisonObserver(func(comparison sessionLifecycleStartSelectionComparison) {
			comparisons = append(comparisons, comparison)
		}),
	)
	snapshot := newSessionBeadSnapshot([]beads.Bead{sessionBead})
	configuredNames := configuredSessionNames(env.cfg, "", env.store)
	woken := reconcileSessionBeadsAtPathWithNamedDemand(
		context.Background(),
		cityPath,
		snapshot.OpenForReconcile(),
		snapshot,
		env.desiredState,
		configuredNames,
		env.cfg,
		env.sp,
		env.store,
		nil,
		nil,
		nil,
		nil,
		env.dt,
		newProviderHealthGate(),
		map[string]int{"worker": 1},
		nil,
		nil,
		false,
		nil,
		"",
		nil,
		env.clk,
		env.rec,
		0,
		0,
		&env.stdout,
		&env.stderr,
		env.startOptions...,
	)
	if woken != 0 {
		t.Fatalf("woken = %d, want 0", woken)
	}
	assertSingleStartSelectionComparison(
		t,
		comparisons,
		sessionBead.ID,
		sessionLifecycleStartSelectionPark,
		sessionLifecycleStartSelectionReasonProviderUnavailable,
		false,
	)
}

func assertSingleStartSelectionComparison(
	t *testing.T,
	comparisons []sessionLifecycleStartSelectionComparison,
	sessionID string,
	wantOutcome sessionLifecycleStartSelectionOutcome,
	wantReason sessionLifecycleStartSelectionReason,
	wantLegacySelected bool,
) {
	t.Helper()
	if len(comparisons) != 1 {
		t.Fatalf("comparison count = %d, want 1; comparisons=%+v", len(comparisons), comparisons)
	}
	got := comparisons[0]
	if got.Plan.SessionID != sessionID ||
		got.Plan.Outcome != wantOutcome ||
		got.Plan.Reason != wantReason {
		t.Fatalf("plan = %+v, want session=%q outcome=%v reason=%v", got.Plan, sessionID, wantOutcome, wantReason)
	}
	if got.LegacySelected != wantLegacySelected {
		t.Fatalf("legacy selected = %v, want %v", got.LegacySelected, wantLegacySelected)
	}
	if got.Outcome != sessionLifecycleStartSelectionComparisonMatched {
		t.Fatalf("comparison = %+v, want matched", got)
	}
}

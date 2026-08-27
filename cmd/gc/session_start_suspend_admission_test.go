package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// seedExactUserHoldSuspendRow puts the session bead in the durable shape
// `gc session suspend` leaves behind on a managed city: state=suspended,
// sleep_intent=user-hold, and a held_until far enough in the future to still be
// current.
func seedExactUserHoldSuspendRow(env *reconcilerTestEnv, bead *beads.Bead) {
	env.setSessionMetadata(bead, map[string]string{
		"state":        string(sessionpkg.StateSuspended),
		"sleep_intent": "user-hold",
		"held_until":   env.clk.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	})
}

// TestReconcileExactSuspendStopsRegardlessOfAdmissionSource pins the F2 contract
// for ga-f7v2ft.125: the suspend family dispatches on the durable row, not on how
// the admission arrived. The controller coalesces a later socket admission onto a
// pending in_process one and KEEPS source=in_process, so a source-gated family
// drops the user's suspend request into the ordinary path's held-blocker dead end
// — admission consumed, runtime still live, nothing left to re-detect.
func TestReconcileExactSuspendStopsRegardlessOfAdmissionSource(t *testing.T) {
	for _, source := range []sessionStartAdmissionSource{
		sessionStartAdmissionInProcess,
		sessionStartAdmissionSocket,
		sessionStartAdmissionAntiEntropy,
	} {
		t.Run(string(source), func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "reviewer", StartCommand: "true"}}}
			bead := env.createSessionBead("reviewer", "reviewer")
			seedExactUserHoldSuspendRow(env, &bead)

			provider := &unattendedStopProvider{Fake: runtime.NewFake()}
			if err := provider.Start(context.Background(), "reviewer", runtime.Config{Command: "test-cmd"}); err != nil {
				t.Fatalf("start runtime: %v", err)
			}
			params := exactSessionStartTestParams(t, env)
			params.Provider = provider

			owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{
				SessionID: bead.ID,
				Source:    source,
			}, params)
			if owner != exactSessionStartKeyedOwner || err != nil {
				t.Fatalf("reconcile exact suspend = (%v, %v), want keyed owner and no error", owner, err)
			}
			stops := provider.stopSnapshot()
			if len(stops) != 1 {
				t.Fatalf("token-bound stops = %#v, want exactly one", stops)
			}
			if stops[0].name != "reviewer" || stops[0].expectedToken != env.sessionInfo(bead.ID).InstanceToken {
				t.Fatalf("token-bound stop = %#v, want reviewer bound to the row's instance token", stops[0])
			}
			if provider.IsRunning("reviewer") {
				t.Fatal("suspended session runtime is still live after the keyed stop")
			}
			current := env.sessionInfo(bead.ID)
			if current.MetadataState != string(sessionpkg.StateSuspended) || current.SleepIntent != "user-hold" {
				t.Fatalf("durable row after keyed suspend stop = {state:%q sleep_intent:%q}, want the suspend marker retained",
					current.MetadataState, current.SleepIntent)
			}
		})
	}
}

// TestReconcileExactSuspendOnDeadRuntimeIsASilentNoOp is the negative: a
// user-hold-current row whose runtime is already gone needs no stop, so the
// family must consume the admission without touching the provider.
func TestReconcileExactSuspendOnDeadRuntimeIsASilentNoOp(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "reviewer", StartCommand: "true"}}}
	bead := env.createSessionBead("reviewer", "reviewer")
	seedExactUserHoldSuspendRow(env, &bead)

	provider := &unattendedStopProvider{Fake: runtime.NewFake()}
	params := exactSessionStartTestParams(t, env)
	params.Provider = provider

	owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{
		SessionID: bead.ID,
		Source:    sessionStartAdmissionInProcess,
	}, params)
	if owner != exactSessionStartKeyedOwner || err != nil {
		t.Fatalf("reconcile exact suspend on dead runtime = (%v, %v), want keyed owner and no error", owner, err)
	}
	if stops := provider.stopSnapshot(); len(stops) != 0 {
		t.Fatalf("token-bound stops on a dead runtime = %#v, want none", stops)
	}
	current := env.sessionInfo(bead.ID)
	if current.MetadataState != string(sessionpkg.StateSuspended) || current.SleepIntent != "user-hold" {
		t.Fatalf("durable row = {state:%q sleep_intent:%q}, want the suspend marker untouched",
			current.MetadataState, current.SleepIntent)
	}
}

// seedSuspendRowUnderIncompleteScan is the ga-bxa8r fixture for this arm: the
// same user-hold suspend row, observed through a provider whose alive-target
// completeness is structurally unlicensable (aliveIncompleteStopProvider).
func seedSuspendRowUnderIncompleteScan(t *testing.T, env *reconcilerTestEnv) (*aliveIncompleteStopProvider, beads.Bead, exactSessionStartParams) {
	t.Helper()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "reviewer", StartCommand: "true"}}}
	bead := env.createSessionBead("reviewer", "reviewer")
	seedExactUserHoldSuspendRow(env, &bead)
	provider := &aliveIncompleteStopProvider{unattendedStopProvider: &unattendedStopProvider{Fake: runtime.NewFake()}}
	params := exactSessionStartTestParams(t, env)
	params.Provider = provider
	return provider, bead, params
}

// TestReconcileExactSuspendStopsAliveSessionOnIncompleteScan is ga-bxa8r's
// suspend specimen and shares D-DEADLINE's adjudication: the stop is destructive
// BY INTENT — the user asked for it and the runtime is stopped precisely because
// it is still live — so demanding scan completeness first demanded a proof a
// live target can never supply. On a busy host `gc session suspend` left the
// runtime running and the admission consumed, with nothing but the park in the
// journal.
//
// The proof obligations that actually fence identity are untouched: the
// revision + instance-token + name re-read below the observation, the
// token-bound unattended stop, and the COMPLETE proven-dead confirm that only
// becomes satisfiable once the pane is gone.
func TestReconcileExactSuspendStopsAliveSessionOnIncompleteScan(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead, params := seedSuspendRowUnderIncompleteScan(t, env)
	if err := provider.Start(context.Background(), "reviewer", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}

	owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{
		SessionID: bead.ID,
		Source:    sessionStartAdmissionInProcess,
	}, params)
	if err != nil {
		t.Fatalf("an alive session's incomplete scan parked the suspend stop: %v", err)
	}
	if owner != exactSessionStartKeyedOwner {
		t.Fatalf("owner = %v, want keyed ownership", owner)
	}
	stops := provider.stopSnapshot()
	if len(stops) != 1 || stops[0].expectedToken != env.sessionInfo(bead.ID).InstanceToken {
		t.Fatalf("token-bound stops = %#v, want exactly one bound to the row's instance token", stops)
	}
	if provider.IsRunning("reviewer") {
		t.Fatal("the suspended session's runtime is still live; the suspend stop never fired")
	}
}

// TestReconcileExactSuspendUnprovenAbsenceStillParks is the fail-closed control,
// and it fails DIFFERENTLY: same fixture, runtime already gone, scan still
// unlicensable. A NEGATIVE incomplete observation cannot tell dead apart from
// unobserved, so the arm's own silent no-op — which consumes the admission and
// retires the suspend the row still owes — must not fire.
func TestReconcileExactSuspendUnprovenAbsenceStillParks(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead, params := seedSuspendRowUnderIncompleteScan(t, env)
	provider.alwaysIncomplete = true

	owner, err := reconcileExactSessionStartWithOwner(context.Background(), sessionStartAdmission{
		SessionID: bead.ID,
		Source:    sessionStartAdmissionInProcess,
	}, params)
	if err == nil || !strings.Contains(err.Error(), "liveness observation is incomplete") {
		t.Fatalf("err = %v, want the incomplete-liveness park for a negative unproven observation", err)
	}
	if owner != exactSessionStartKeyedOwner {
		t.Fatalf("owner = %v, want keyed ownership", owner)
	}
	if stops := provider.stopSnapshot(); len(stops) != 0 {
		t.Fatalf("token-bound stops on an unproven absence = %#v, want none", stops)
	}
	current := env.sessionInfo(bead.ID)
	if current.MetadataState != string(sessionpkg.StateSuspended) || current.SleepIntent != "user-hold" {
		t.Fatalf("durable row = {state:%q sleep_intent:%q}, want the suspend marker untouched",
			current.MetadataState, current.SleepIntent)
	}
}

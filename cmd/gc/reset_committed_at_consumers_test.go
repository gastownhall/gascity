package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// These tests cover the readers of reset_committed_at (gascity#4067): only a
// restart handoff stamps it, and a passive continuation-reset re-arm (a crash
// or churn reset, a fresh-wake drain) clears it, so none of the readers treats
// a passive reset as a handoff.

var resetConsumersNow = time.Date(2026, 5, 14, 13, 0, 0, 0, time.UTC)

// resetConsumerRow is an asleep, keyed on-demand row with no work.
func resetConsumerRow() sessionpkg.Info {
	return sessionpkg.Info{
		ID:                  "gc-reset",
		SessionNameMetadata: "fixture--reset-target",
		Template:            "fixture/build-agent",
		MetadataState:       string(sessionpkg.StateAsleep),
		SessionKey:          "conversation-1",
		StartedConfigHash:   "config-hash",
		ContinuationEpoch:   "2",
		CreatedAt:           resetConsumersNow.Add(-24 * time.Hour),
	}
}

// resetConsumerAwake runs info through the awake-set bridge and the awake set
// as an on-demand named session with no demand.
func resetConsumerAwake(t *testing.T, info sessionpkg.Info) (AwakeSessionBead, AwakeDecision) {
	t.Helper()
	const template, identity = "fixture/build-agent", "fixture/reset-target"
	input := newAwakeInputFromSnapshot(&config.City{}, func(*config.Agent) bool { return false },
		[]sessionpkg.Info{info}, nil, nil, nil, nil, nil, nil, nil, resetConsumersNow)
	if len(input.SessionBeads) != 1 {
		t.Fatalf("awake input rows = %+v, want the one session row", input.SessionBeads)
	}
	bead := input.SessionBeads[0]
	bead.Template = template
	bead.NamedIdentity = identity
	result := computeAwakeSetChecked(t, AwakeInput{
		Agents:           []AwakeAgent{{QualifiedName: template}},
		NamedSessions:    []AwakeNamedSession{{Identity: identity, Template: template, Mode: "on_demand"}},
		SessionBeads:     []AwakeSessionBead{bead},
		ScaleCheckCounts: map[string]int{template: 0},
		Now:              resetConsumersNow,
	})
	return bead, result[bead.SessionName]
}

func TestAwakeSetPassiveResetDesiresNoResetPendingWake(t *testing.T) {
	for name, patch := range map[string]sessionpkg.MetadataPatch{
		"a churn reset":        sessionpkg.ConversationResetPatch(false, resetConsumersNow),
		"a wake-failure reset": sessionpkg.ConversationResetPatch(true, resetConsumersNow),
	} {
		t.Run(name, func(t *testing.T) {
			bead, d := resetConsumerAwake(t, resetConsumerRow().ApplyPatch(patch))
			if bead.ContinuationResetPending {
				t.Errorf("AwakeSessionBead.ContinuationResetPending = true after a passive reset, want false")
			}
			if d.ShouldWake || d.Reason == "reset-pending" {
				t.Errorf("decision = %+v, want no wake for an idle row after a passive reset", d)
			}
		})
	}

	// The restart handoff, by contrast, is a reason to wake.
	bead, d := resetConsumerAwake(t, resetConsumerRow().ApplyPatch(sessionpkg.RestartRequestPatch("", resetConsumersNow)))
	if !bead.ContinuationResetPending || !d.ShouldWake || d.Reason != "reset-pending" {
		t.Errorf("after a restart handoff: bead pending=%v, decision %+v; want a reset-pending wake", bead.ContinuationResetPending, d)
	}
}

func TestRestartHandedOffInfoOnlyAfterHandoff(t *testing.T) {
	for name, patch := range map[string]sessionpkg.MetadataPatch{
		"a churn reset":          sessionpkg.ConversationResetPatch(false, resetConsumersNow),
		"a wake-failure reset":   sessionpkg.ConversationResetPatch(true, resetConsumersNow),
		"a fresh-wake drain":     sessionpkg.CompleteDrainPatch(resetConsumersNow, "", true),
		"a continuation wake":    sessionpkg.ContinuationResetWakePatch(resetConsumersNow),
		"an asleep drift repair": sessionpkg.ConfigDriftAsleepResetPatch("", resetConsumersNow),
	} {
		// Each passive reset lands on a row an earlier handoff stamped, so
		// the stamp must not survive it.
		info := resetConsumerRow().ApplyPatch(sessionpkg.RestartRequestPatch("", resetConsumersNow.Add(-3*time.Hour))).ApplyPatch(patch)
		if restartHandedOffInfo(info) {
			t.Errorf("%s: restartHandedOffInfo = true, want false (reset_committed_at=%q)", name, info.ResetCommittedAt)
		}
	}
	if info := resetConsumerRow().ApplyPatch(sessionpkg.RestartRequestPatch("", resetConsumersNow)); !restartHandedOffInfo(info) {
		t.Errorf("after RestartRequestPatch: restartHandedOffInfo = false, want true (%+v)", info)
	}
}

func TestHeldForPendingResetFalseAfterPassiveReset(t *testing.T) {
	wait := sessionpkg.WaitInfo{RegisteredEpoch: "2", RegisteredResetPending: true}
	for name, patch := range map[string]sessionpkg.MetadataPatch{
		"a churn reset":        sessionpkg.ConversationResetPatch(false, resetConsumersNow),
		"a wake-failure reset": sessionpkg.ConversationResetPatch(true, resetConsumersNow),
		"a fresh-wake drain":   sessionpkg.CompleteDrainPatch(resetConsumersNow, "idle", true),
	} {
		if info := resetConsumerRow().ApplyPatch(patch); wait.HeldForPendingReset(info) {
			t.Errorf("%s: HeldForPendingReset = true, want false (reset_committed_at=%q)", name, info.ResetCommittedAt)
		}
	}
	if info := resetConsumerRow().ApplyPatch(sessionpkg.RestartRequestPatch("", resetConsumersNow)); !wait.HeldForPendingReset(info) {
		t.Errorf("after RestartRequestPatch: HeldForPendingReset = false, want true")
	}
}

// TestRecordResetStallIgnoresHandoffStampAfterFreshWakeDrain is gascity#4067's
// end-to-end shape: a restart handoff at T0, then a healthy fresh-wake drain
// at T0+3h. The stall check must not measure from T0.
func TestRecordResetStallIgnoresHandoffStampAfterFreshWakeDrain(t *testing.T) {
	env := newReconcilerTestEnv(t)
	rec := events.NewFake()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
		Session:   config.SessionConfig{StartupTimeout: "60s"},
	}
	t0 := env.clk.Now().UTC().Add(-3 * time.Hour)
	drained := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&drained, sessionpkg.RestartRequestPatch("", t0))
	env.setSessionMetadata(&drained, sessionpkg.CompleteDrainPatch(env.clk.Now().UTC(), "idle", true))
	if got := env.sessionInfo(drained.ID).ResetCommittedAt; got != "" {
		t.Fatalf("reset_committed_at after the fresh-wake drain = %q, want empty", got)
	}

	recordResetStallIfDue(env.city, env.store, env.sp, env.cfg, sessiontest.SeedBead(t, drained), "worker", "worker",
		false, false, env.cfg.Session.StartupTimeoutDuration(), env.clk.Now().UTC(), env.dt, rec, &env.stderr, nil)
	if got := strings.TrimSpace(env.stderr.String()); got != "" {
		t.Errorf("stderr = %q, want no reset-stalled diagnostic", got)
	}
	if len(rec.Events) != 0 {
		t.Errorf("recorded events = %#v, want none", rec.Events)
	}

	// Control: the same T0 handoff, still waiting, is a stall.
	stalled := env.createSessionBead("worker-2", "worker")
	env.setSessionMetadata(&stalled, sessionpkg.RestartRequestPatch("", t0))
	recordResetStallIfDue(env.city, env.store, env.sp, env.cfg, sessiontest.SeedBead(t, stalled), "worker", "worker-2",
		false, false, env.cfg.Session.StartupTimeoutDuration(), env.clk.Now().UTC(), env.dt, rec, &env.stderr, nil)
	if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionResetStalled {
		t.Errorf("control: recorded events = %#v, want one %s", rec.Events, events.SessionResetStalled)
	}
}

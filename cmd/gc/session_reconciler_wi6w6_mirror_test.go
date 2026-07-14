package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// These two tests pin the WI-6 W6 red-team blocker as an Info-native invariant.
// W6 kept two transitional raw session.Metadata mirrors because deferred readers
// later in the SAME forward-pass tick read the collapsed writes RAW. WI-6 R3 typed
// every one of those readers (pendingInteractionKeepsAwakeInfo, healStateWithRollbackInfo,
// the awake-scan sleep resolvers) so they read the coherent infoByID snapshot, and
// DROPPED both mirrors. These assertions still describe the same fail-safe outcome
// (a cleared quarantine defers the max-age kill; a zombie's healed sleep_reason
// survives heal), now guaranteed by the shared Info snapshot rather than a mirror.

// TestReconcileSessionBeads_ClearedQuarantineKeepsMaxAgePendingDeferral guards the
// clearWakeFailures -> pendingInteractionKeepsAwakeInfo same-tick coupling. clearWakeFailures
// clears a still-future quarantined_until on the infoByID snapshot; the max-age kill's blocker
// check (typed Info) then sees no blocker and proceeds to the pending check, which reads
// quarantined_until off the SAME snapshot via pendingInteractionKeepsAwakeInfo (WI-6 R3). So
// the cleared quarantine reaches the pending check, and a live user interaction defers the kill.
// A regression that reads a stale quarantine would report BlockerQuarantined (not pending) and
// wrongly kill the aged session mid-interaction — a fail-safe violation.
func TestReconcileSessionBeads_ClearedQuarantineKeepsMaxAgePendingDeferral(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness", MaxSessionAge: "5h"}}}
	env.addDesired("witness", "witness", true) // running + alive
	session := env.createSessionBead("witness", "witness")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		// Aged past the configured 5h threshold so the max-age timer triggers.
		"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
		// Stable long enough (older than stabilityThreshold) so clearWakeFailures runs
		// and clears the quarantine this tick.
		"last_woke_at": env.clk.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339),
		// A STILL-FUTURE quarantine clearWakeFailures clears; without the mirror it
		// survives on the raw bead and poisons pendingInteractionKeepsAwake.
		"quarantined_until": env.clk.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339),
	})
	// A live user interaction — the max-age kill must defer to it.
	env.sp.SetPendingInteraction("witness", &runtime.PendingInteraction{RequestID: "req-1", Kind: "question", Prompt: "approve?"})

	tr := newMaxSessionAgeTracker()
	tr.setConfig("witness", 5*time.Hour, 0)
	rec := events.NewFake()
	env.rec = rec

	env.maxAgeReconcile([]beads.Bead{session}, tr)

	if !env.sp.IsRunning("witness") {
		t.Fatalf("aged witness with a pending interaction was killed; the cleared quarantine must reach pendingInteractionKeepsAwakeInfo off the shared infoByID snapshot so the kill is deferred (WI-6 R3, no mirror). stderr=%q", env.stderr.String())
	}
	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["sleep_reason"] == "max-session-age" {
		t.Fatalf("sleep_reason = %q, want not max-session-age — the pending-interaction deferral must hold once the quarantine is cleared", b.Metadata["sleep_reason"])
	}
	for _, e := range rec.Events {
		if e.Type == events.SessionMaxAgeKilled {
			t.Fatal("SessionMaxAgeKilled fired; with the quarantine cleared and a live interaction, the max-age kill must defer")
		}
	}
}

// TestReconcileSessionBeads_ZombieTerminalErrorSleepReasonSurvivesHeal guards the
// zombie markProviderTerminalError -> healStateWithRollbackInfo same-tick coupling. A dead
// pending-create zombie (state=creating, pending_create_claim=true, an expired never-started
// lease) hits a terminal provider error: the zombie block marks state=asleep + sleep_reason=
// provider-terminal-error and CLEARS the pending-create claim, folding that onto the infoByID
// snapshot, so the post-zombie rollback is suppressed and the tick falls through to heal.
// healStateWithRollbackInfo reads state / sleep_reason / pending_create_claim /
// pending_create_started_at off that SAME snapshot (WI-6 R3), sees the healed asleep +
// terminal-error state, and makes no change. A regression that read the stale state=creating +
// still-claimed lease would run heal's stale-creating rollback and overwrite sleep_reason with
// the generic runtime-missing reason — erasing the terminal-error classification the pool-slot
// reaper depends on.
func TestReconcileSessionBeads_ZombieTerminalErrorSleepReasonSurvivesHeal(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "witness"}}}
	env.desiredState["witness"] = TemplateParams{
		Command:      "true",
		SessionName:  "witness",
		TemplateName: "witness",
		Hints:        agent.StartupHints{ProcessNames: []string{"true"}},
	}
	session := env.createSessionBead("witness", "witness")
	env.setSessionMetadata(&session, map[string]string{
		"state":                     "creating",
		"pending_create_claim":      "true",
		"pending_create_started_at": env.clk.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339), // never-started lease expired
	})

	// Zombie: tmux session exists (running) but the process is dead (not alive), with
	// terminal-error scrollback so markProviderTerminalError fires in the zombie block.
	if err := env.sp.Start(context.Background(), "witness", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start zombie witness: %v", err)
	}
	env.sp.Zombies["witness"] = true
	env.sp.SetPeekOutput("witness", "model_not_found")

	env.reconcile([]beads.Bead{session})

	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Precondition: the terminal-error path actually ran (else the read-after-write is
	// vacuous).
	if b.Metadata["provider_terminal_error"] == "" {
		t.Fatalf("provider_terminal_error not recorded — the zombie terminal-error path did not run; scenario precondition unmet (metadata=%v)", b.Metadata)
	}
	if got := b.Metadata["sleep_reason"]; got != string(sessionpkg.SleepReasonProviderTerminalError) {
		t.Fatalf("sleep_reason = %q, want %q — the zombie mark's healed state/sleep_reason/pending-create lease must reach the same-tick healStateWithRollbackInfo reader via the shared infoByID snapshot so heal's stale-creating rollback does not clobber it (WI-6 R3, no mirror). stderr=%q", got, string(sessionpkg.SleepReasonProviderTerminalError), env.stderr.String())
	}
}

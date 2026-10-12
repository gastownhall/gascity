package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestPhantomReplacementSessionKeyRepro is the inverted form of the ga-o9wnd8
// characterization test (branch investigate/ga-o9wnd8-phantom-key-repro, sha
// 579e9ba1898bf5f8a1ca14b3d0b8600e081cb017). That version hand-built the
// post-teardown metadata by calling sessionpkg.RestartRequestPatch directly
// and merging it onto a hand-written "live" map, and it PASSED on origin/main
// — it characterized the bug. A literal in-place assertion-flip on that same
// construction can never go red against the ga-2fpf9z fix: the fix adds
// `state` in the CALLER (cmd/gc/session_bead_cycle.go's
// cycleAliveSessionForFreshReassign), not inside RestartRequestPatch itself
// (explicitly out of scope — see the fix spec, and
// internal/session/store.go:138's untouched third caller). Hand-merging the
// patch would never observe that caller-side write no matter what the fix
// does.
//
// So this version drives the REAL fresh-mode bead-reassign path
// (reconcileSessionBeadsWithAssignedWork -> cycleAliveSessionForFreshReassign)
// and feeds the resulting PERSISTED bead into the same heal oracle the
// original test used, to prove the phantom-key defect is actually closed:
// the post-handoff record must already say asleep, and the heal that used to
// discard the minted key must now be a genuine no-op.
func TestPhantomReplacementSessionKeyRepro(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "witness", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "witness", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "witness")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "witness",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "witness",
		namedSessionModeMetadata:     "on_demand",
		"template":                   "witness",
		"state":                      "active",
		"wake_mode":                  "fresh",
		"session_key":                "32e27ede-ffc0-40f2-ba8a-966c857f009a",
		sessionpkg.CurrentBeadIDKey:  "wb-A",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	// Patrol formula pours a fresh bead and points the witness's assignee at
	// it — the real trigger for cycleAliveSessionForFreshReassign.
	workBead := beads.Bead{ID: "wb-B", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("session should have been killed by the fresh-cycle handoff")
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}

	minted := got.Metadata["session_key"]
	if minted == "" || minted == "32e27ede-ffc0-40f2-ba8a-966c857f009a" {
		t.Fatalf("session_key = %q, want a freshly minted key", minted)
	}

	// FINDING 1 (inverted): the handoff must move state to asleep in the SAME
	// patch as the kill — the whole point of ga-2fpf9z. Before the fix,
	// RestartRequestPatch writes no "state" key at all, so this stayed
	// "active" (gc believed the dead session was alive).
	if got.Metadata["state"] != "asleep" {
		t.Fatalf("state after handoff = %q, want %q (gc must not believe the dead session is alive)", got.Metadata["state"], "asleep")
	}

	// --- The next reconciler tick observes the runtime is gone. ---
	info := seedSessionInfo(got)
	healed := healStatePatchWithRollbackInfo(info, false /*alive*/, true /*observed*/, env.clk, 5*time.Minute, true)

	// FINDING 3 (inverted) — the decisive one. Before the fix, the heal
	// cleared the just-minted key because an active session always trips
	// shouldResetContinuation. Now that the handoff already recorded asleep,
	// shouldResetContinuation no longer fires (base is Asleep, not
	// Active/Creating), so the heal must be a genuine no-op — it must not
	// touch state (already correct) and, above all, must NOT clear the
	// minted session_key.
	if healed != nil {
		t.Fatalf("heal was not a no-op for an already-asleep session with the minted key intact: %#v", healed)
	}
	t.Logf("CONFIRMED FIXED: minted key %s survived the heal untouched (state stayed %q, heal batch nil)", minted, got.Metadata["state"])
}

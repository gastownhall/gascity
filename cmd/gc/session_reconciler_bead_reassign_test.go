package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestReconcileSessionBeads_AliveFreshModeReassignCyclesConversation verifies
// the fix for gastownhall/gascity#1893: an alive on_demand named session
// running wake_mode=fresh must cycle its conversation when bd update points
// the assignee at a new bead. The session keeps the same bead identifier in
// the store (it's a named session) but its conversation lineage is reset so
// the next wake starts fresh on the newly assigned bead.
func TestReconcileSessionBeads_AliveFreshModeReassignCyclesConversation(t *testing.T) {
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
		"session_key":                "conversation-A",
		sessionpkg.CurrentBeadIDKey:  "wb-A",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	// Patrol formula poured wb-B and pointed the witness's assignee at it;
	// wb-A is gone (closed/burned), so the reconciler only sees wb-B.
	workBead := beads.Bead{ID: "wb-B", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	// The runtime cannot wait for an idle prompt, so the idle gate does
	// not hold the cycle; these rows pin the bead-state guards alone.
	reconcileSessionBeadsWithProvider(env, noIdleWaitProvider{env.sp}, []beads.Bead{session}, []beads.Bead{workBead})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("session should have been killed by fresh-cycle")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-B" {
		t.Fatalf("%s = %q, want wb-B", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want empty so the next wake takes the first-start path", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "conversation-A" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
}

// TestReconcileSessionBeads_AliveResumeModeReassignKeepsConversation verifies
// that wake_mode=resume sessions DO NOT cycle on bead reassign — the
// existing conversation is preserved and the agent picks up the new bead
// from its work query at its next prompt boundary.
func TestReconcileSessionBeads_AliveResumeModeReassignKeepsConversation(t *testing.T) {
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
		// wake_mode unset (default = resume)
		"session_key":               "conversation-A",
		sessionpkg.CurrentBeadIDKey: "wb-A",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	workBead := beads.Bead{ID: "wb-B", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("resume-mode session should still be running — divergence must not cycle non-fresh sessions")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata["session_key"] != "conversation-A" {
		t.Fatalf("session_key = %q, want conversation-A preserved", got.Metadata["session_key"])
	}
	if got.Metadata["continuation_reset_pending"] == "true" {
		t.Fatalf("continuation_reset_pending = true, want unset for resume mode (no cycle should have run)")
	}
}

// TestReconcileSessionBeads_AsleepWakeRecordsCurrentBead pins the recording
// half of the contract: when an asleep session is woken because of an
// assigned bead, the reconciler must stamp currently_processing_bead_id
// onto the session bead. Without this, the next reassign cycle would have
// no recorded current bead to compare against.
func TestReconcileSessionBeads_AsleepWakeRecordsCurrentBead(t *testing.T) {
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
		"state":                      "asleep",
		"wake_mode":                  "fresh",
	})

	workBead := beads.Bead{ID: "wb-77", Title: "witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

	got, _ := env.store.Get(session.ID)
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-77" {
		t.Fatalf("%s = %q, want wb-77 recorded at wake", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
}

// TestReconcileSessionBeads_RecoveryPrefersRecordedBead pins crash-recovery
// behavior: when a session is asleep with a recorded current bead AND
// multiple beads are assigned, the reconciler must anchor on the recorded
// bead so the agent resumes the work it was last actively processing.
func TestReconcileSessionBeads_RecoveryPrefersRecordedBead(t *testing.T) {
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
		"state":                      "asleep",
		"wake_mode":                  "fresh",
		sessionpkg.CurrentBeadIDKey:  "wb-current",
	})

	other := beads.Bead{ID: "wb-other", Title: "other wisp", Type: "task", Status: "open", Assignee: "witness"}
	current := beads.Bead{ID: "wb-current", Title: "current wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{other, current})

	got, _ := env.store.Get(session.ID)
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-current" {
		t.Fatalf("%s = %q, want wb-current preserved across restart", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
}

// TestReconcileSessionBeads_FreshCycleDefers_WhenPreviousBeadStillOpen pins
// row A of ga-3pocz7's exit contract: when the bead the session was last
// processing is still open (not yet closed by the agent), a fresh-mode
// reassign must defer rather than kill the session out from under work in
// progress. No kill, and no currently_processing_bead_id stamp either — the
// next tick must still see the same divergence and re-evaluate it.
func TestReconcileSessionBeads_FreshCycleDefers_WhenPreviousBeadStillOpen(t *testing.T) {
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

	createPrevBead(t, env, nil) // still open: no closed_at

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "witness",
		namedSessionModeMetadata:     "on_demand",
		"template":                   "witness",
		"state":                      "active",
		"wake_mode":                  "fresh",
		"session_key":                "conversation-A",
		sessionpkg.CurrentBeadIDKey:  "wb-prev",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	workBead := beads.Bead{ID: "wb-new", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("session should NOT have been killed — previous bead is still open")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-prev" {
		t.Fatalf("%s = %q, want wb-prev unchanged (no stamp while deferred)", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
	if got.Metadata["session_key"] != "conversation-A" {
		t.Fatalf("session_key = %q, want conversation-A preserved (no cycle)", got.Metadata["session_key"])
	}
	if got.Metadata["continuation_reset_pending"] == "true" {
		t.Fatal("continuation_reset_pending = true, want unset — no cycle should have run")
	}
}

// TestReconcileSessionBeads_FreshCycleFires_WhenPreviousBeadClosedWithNoTimingSignal
// pins row B of ga-3pocz7's exit contract: the previous bead is closed, but
// this session incarnation has no awake_started_at to prove it is already a
// fresh conversation. Missing timing data must fail toward the pre-existing
// behavior (cycle fires) rather than silently deferring forever.
func TestReconcileSessionBeads_FreshCycleFires_WhenPreviousBeadClosedWithNoTimingSignal(t *testing.T) {
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

	closedAt := time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC)
	createPrevBead(t, env, &closedAt)

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "witness",
		namedSessionModeMetadata:     "on_demand",
		"template":                   "witness",
		"state":                      "active",
		"wake_mode":                  "fresh",
		"session_key":                "conversation-A",
		sessionpkg.CurrentBeadIDKey:  "wb-prev",
		// awake_started_at deliberately absent.
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	workBead := beads.Bead{ID: "wb-new", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	// The runtime cannot wait for an idle prompt, so the idle gate does
	// not hold the cycle; these rows pin the bead-state guards alone.
	reconcileSessionBeadsWithProvider(env, noIdleWaitProvider{env.sp}, []beads.Bead{session}, []beads.Bead{workBead})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("session should have been killed by fresh-cycle — previous bead is closed and no timing signal defers it")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-new" {
		t.Fatalf("%s = %q, want wb-new", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

// TestReconcileSessionBeads_FreshCycleDefers_WhenIncarnationStartedAfterPreviousClose
// pins row C of ga-3pocz7's exit contract: the previous bead closed, and
// this session incarnation's awake_started_at is AFTER that close — proof
// the current conversation already began fresh for the new assignment, so
// no cycle is needed.
func TestReconcileSessionBeads_FreshCycleDefers_WhenIncarnationStartedAfterPreviousClose(t *testing.T) {
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

	closedAt := time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC)
	createPrevBead(t, env, &closedAt)

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "witness",
		namedSessionModeMetadata:     "on_demand",
		"template":                   "witness",
		"state":                      "active",
		"wake_mode":                  "fresh",
		"session_key":                "conversation-A",
		sessionpkg.CurrentBeadIDKey:  "wb-prev",
		"awake_started_at":           time.Date(2026, 3, 8, 11, 0, 0, 0, time.UTC).Format(time.RFC3339),
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	workBead := beads.Bead{ID: "wb-new", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("session should NOT have been killed — this incarnation already started after the previous bead closed")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata["session_key"] != "conversation-A" {
		t.Fatalf("session_key = %q, want conversation-A preserved (no cycle)", got.Metadata["session_key"])
	}
	if got.Metadata["continuation_reset_pending"] == "true" {
		t.Fatal("continuation_reset_pending = true, want unset — no cycle should have run")
	}
}

// TestReconcileSessionBeads_FreshCycleFires_WhenIncarnationPredatesPreviousClose
// pins row D of ga-3pocz7's exit contract: the companion to row C. This
// session incarnation started BEFORE the previous bead closed, so its
// conversation predates the close and cannot be assumed fresh for the new
// assignment — the cycle must still fire.
func TestReconcileSessionBeads_FreshCycleFires_WhenIncarnationPredatesPreviousClose(t *testing.T) {
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

	closedAt := time.Date(2026, 3, 8, 11, 0, 0, 0, time.UTC)
	createPrevBead(t, env, &closedAt)

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "witness",
		namedSessionModeMetadata:     "on_demand",
		"template":                   "witness",
		"state":                      "active",
		"wake_mode":                  "fresh",
		"session_key":                "conversation-A",
		sessionpkg.CurrentBeadIDKey:  "wb-prev",
		"awake_started_at":           time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC).Format(time.RFC3339),
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	workBead := beads.Bead{ID: "wb-new", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	// The runtime cannot wait for an idle prompt, so the idle gate does
	// not hold the cycle; these rows pin the bead-state guards alone.
	reconcileSessionBeadsWithProvider(env, noIdleWaitProvider{env.sp}, []beads.Bead{session}, []beads.Bead{workBead})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("session should have been killed by fresh-cycle — this incarnation predates the previous bead's close")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-new" {
		t.Fatalf("%s = %q, want wb-new", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

// TestReconcileSessionBeads_FreshCycleDefers_WhenSessionAlreadySelfClaimedAnchor
// pins row E of ga-3pocz7's exit contract: the session's own
// current_claim_bead_id already equals the newly-computed anchor bead — it
// claimed the work itself (via its own hook) before this reconciler tick
// caught up. currently_processing_bead_id is deliberately non-empty and
// unresolvable (no such bead exists) so RequiresFreshCycle is true (the
// untouched compute_awake_set.go gate needs a non-empty, differing prior
// bead to fire at all) — proving the outer self-claim short-circuit fires
// BEFORE any lookup of that stale pointer is attempted. No cycle is needed;
// the existing recordCurrentBeadIDOnWake backstop (not new code) then
// catches currently_processing_bead_id up to match.
func TestReconcileSessionBeads_FreshCycleDefers_WhenSessionAlreadySelfClaimedAnchor(t *testing.T) {
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
		namedSessionMetadataKey:                "true",
		namedSessionIdentityMetadata:           "witness",
		namedSessionModeMetadata:               "on_demand",
		"template":                             "witness",
		"state":                                "active",
		"wake_mode":                            "fresh",
		"session_key":                          "conversation-A",
		sessionpkg.CurrentBeadIDKey:            "wb-ancient",
		beadmeta.CurrentClaimBeadIDMetadataKey: "wb-new",
		// wb-ancient has no corresponding bead in the store — the outer
		// self-claim check must short-circuit before any lookup of it.
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	workBead := beads.Bead{ID: "wb-new", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("session should NOT have been killed — it already self-claimed the anchor bead")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata["session_key"] != "conversation-A" {
		t.Fatalf("session_key = %q, want conversation-A preserved (no cycle)", got.Metadata["session_key"])
	}
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-new" {
		t.Fatalf("%s = %q, want wb-new stamped by the existing wake backstop", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
}

// TestReconcileSessionBeads_FreshCycleFires_WhenSelfClaimMismatchesAnchor pins
// row F of ga-3pocz7's exit contract: current_claim_bead_id is set but does
// NOT match the newly-computed anchor (covers both an untouched fresh
// reassignment and a bead forced to in_progress+assigned without this
// session's own hook ever claiming it — the boolean short-circuits the same
// way in both). This is row B's setup (closed prev bead, no timing signal)
// plus a non-matching self-claim value layered on top, proving the
// mismatch does not suppress a fire that would otherwise happen. The cycle
// must still fire.
func TestReconcileSessionBeads_FreshCycleFires_WhenSelfClaimMismatchesAnchor(t *testing.T) {
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

	closedAt := time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC)
	createPrevBead(t, env, &closedAt)

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:                "true",
		namedSessionIdentityMetadata:           "witness",
		namedSessionModeMetadata:               "on_demand",
		"template":                             "witness",
		"state":                                "active",
		"wake_mode":                            "fresh",
		"session_key":                          "conversation-A",
		sessionpkg.CurrentBeadIDKey:            "wb-prev",
		beadmeta.CurrentClaimBeadIDMetadataKey: "wb-unrelated",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	workBead := beads.Bead{ID: "wb-new", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	// The runtime cannot wait for an idle prompt, so the idle gate does
	// not hold the cycle; these rows pin the bead-state guards alone.
	reconcileSessionBeadsWithProvider(env, noIdleWaitProvider{env.sp}, []beads.Bead{session}, []beads.Bead{workBead})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("session should have been killed by fresh-cycle — current_claim_bead_id does not match the anchor")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-new" {
		t.Fatalf("%s = %q, want wb-new", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

// TestReconcileSessionBeads_FreshCycleGuard_UsesCurrentClaimNotCurrentlyProcessing
// pins row G of ga-3pocz7's exit contract — a deliberate trap. The session's
// current_claim_bead_id already matches the anchor, but
// currently_processing_bead_id is left stale, pointing at a different,
// closed bead with no usable timing signal. If the guard is ever swapped to
// compare CurrentlyProcessingBeadID (CurrentBeadIDKey) instead of
// CurrentClaimBeadID for the self-claim check, it will wrongly conclude
// "not self-claimed", fall through to the previous-bead check, find it
// closed with no defer signal, and fire the cycle. A correct implementation
// must NOT kill the session here.
func TestReconcileSessionBeads_FreshCycleGuard_UsesCurrentClaimNotCurrentlyProcessing(t *testing.T) {
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

	closedAt := time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC)
	createPrevBead(t, env, &closedAt)

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:                "true",
		namedSessionIdentityMetadata:           "witness",
		namedSessionModeMetadata:               "on_demand",
		"template":                             "witness",
		"state":                                "active",
		"wake_mode":                            "fresh",
		"session_key":                          "conversation-A",
		sessionpkg.CurrentBeadIDKey:            "wb-prev",
		beadmeta.CurrentClaimBeadIDMetadataKey: "wb-new",
		// awake_started_at deliberately absent, so a wrongly-swapped
		// implementation cannot accidentally defer for the right reason.
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	workBead := beads.Bead{ID: "wb-new", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("session should NOT have been killed — current_claim_bead_id already matches the anchor; the guard must read that key, not currently_processing_bead_id")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata[sessionpkg.CurrentBeadIDKey] != "wb-new" {
		t.Fatalf("%s = %q, want wb-new stamped by the existing wake backstop", sessionpkg.CurrentBeadIDKey, got.Metadata[sessionpkg.CurrentBeadIDKey])
	}
}

// reconcileSessionBeadsWithAssignedWork is a test-only wrapper that mirrors
// restartRequestTestEnv.reconcile but threads assignedWorkBeads through so
// ComputeAwakeSet sees the work demand. Tests for assigned-work-driven
// behavior need this hook; the existing helper in
// session_reconciler_restart_request_test.go intentionally passes nil.
func reconcileSessionBeadsWithAssignedWork(env *restartRequestTestEnv, sessions []beads.Bead, assignedWork []beads.Bead) {
	poolDesired := make(map[string]int)
	for _, tp := range env.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	_ = reconcileSessionBeads(
		context.Background(),
		sessions,
		env.desiredState,
		cfgNames,
		env.cfg,
		env.sp,
		env.store,
		nil,
		assignedWork,
		nil,
		env.dt,
		poolDesired,
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
	)
}

// createPrevBead creates a work bead directly in the store (not merely as
// an assignedWork struct literal) so the terminal-status guard's own lookup
// by ID can resolve its status and closed_at. HonorExplicitIDs must be
// enabled so the store keeps the caller's chosen ID. A nil closedAt leaves
// the bead open; a non-nil closedAt closes it with that timestamp recorded
// in metadata, matching the beadToInfo read convention in
// convergence_store.go.
func createPrevBead(t *testing.T, env *restartRequestTestEnv, closedAt *time.Time) {
	t.Helper()
	const id = "wb-prev"
	mem, ok := env.store.(*beads.MemStore)
	if !ok {
		t.Fatalf("test env store is %T, want *beads.MemStore", env.store)
	}
	mem.HonorExplicitIDs = true
	if _, err := env.store.Create(beads.Bead{ID: id, Title: "previous witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}); err != nil {
		t.Fatalf("creating prev bead %s: %v", id, err)
	}
	if closedAt != nil {
		if _, err := env.store.CloseAll([]string{id}, map[string]string{"closed_at": closedAt.Format(time.RFC3339)}); err != nil {
			t.Fatalf("closing prev bead %s: %v", id, err)
		}
	}
}

// freshCycleFixture is an alive wake_mode=fresh named session whose recorded
// bead (wb-prev) closed before this incarnation's timing could prove it fresh,
// while a second bead is assigned — so #6518's and #6809's guards let the
// bead-reassign cycle through, and only the idle-prompt gate stands between
// the session and a kill.
type freshCycleFixture struct {
	env         *restartRequestTestEnv
	sp          runtime.Provider
	sessionID   string
	sessionName string
	work        []beads.Bead
}

func newFreshCycleFixture(t *testing.T) *freshCycleFixture {
	t.Helper()
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
	closedAt := time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC)
	createPrevBead(t, env, &closedAt)

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "witness",
		namedSessionModeMetadata:     "on_demand",
		"template":                   "witness",
		"state":                      "active",
		"wake_mode":                  "fresh",
		"session_key":                "conversation-A",
		sessionpkg.CurrentBeadIDKey:  "wb-prev",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	return &freshCycleFixture{
		env:         env,
		sp:          env.sp,
		sessionID:   session.ID,
		sessionName: sessionName,
		work:        []beads.Bead{{ID: "wb-new", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}},
	}
}

// pass runs one reconciler pass over the session as the store holds it now,
// then waits for any idle probe the pass launched to settle so the next pass
// observes its result deterministically.
func (f *freshCycleFixture) pass(t *testing.T) {
	t.Helper()
	session, err := f.env.store.Get(f.sessionID)
	if err != nil {
		t.Fatalf("Get(%s): %v", f.sessionID, err)
	}
	reconcileSessionBeadsWithProvider(f.env, f.sp, []beads.Bead{session}, f.work)
	waitIdleProbesSettled(t, f.env.dt)
}

func (f *freshCycleFixture) currentBead(t *testing.T) string {
	t.Helper()
	got, err := f.env.store.Get(f.sessionID)
	if err != nil {
		t.Fatalf("Get(%s): %v", f.sessionID, err)
	}
	return got.Metadata[sessionpkg.CurrentBeadIDKey]
}

// reconcileSessionBeadsWithProvider is reconcileSessionBeadsWithAssignedWork
// with the runtime provider supplied by the caller, so a test can hide the
// Fake's optional interfaces behind a narrower wrapper.
func reconcileSessionBeadsWithProvider(env *restartRequestTestEnv, sp runtime.Provider, sessions []beads.Bead, assignedWork []beads.Bead) {
	poolDesired := make(map[string]int)
	for _, tp := range env.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	_ = reconcileSessionBeads(
		context.Background(), sessions, env.desiredState, cfgNames, env.cfg, sp, env.store,
		nil, assignedWork, nil, env.dt, poolDesired, false, nil, "", nil, env.clk, env.rec,
		0, 0, &env.stdout, &env.stderr,
	)
}

// waitIdleProbesSettled blocks until no idle probe is in flight, except probes
// a test holds open on a WaitForIdleGates channel (the caller passes those
// session IDs in held).
func waitIdleProbesSettled(t *testing.T, dt *drainTracker, held ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		inFlight := 0
		dt.mu.Lock()
		for id, probe := range dt.idleProbes {
			if probe != nil && !probe.ready && !slices.Contains(held, id) {
				inFlight++
			}
		}
		dt.mu.Unlock()
		if inFlight == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d idle probe(s) still in flight after 5s", inFlight)
		}
		time.Sleep(time.Millisecond)
	}
}

func idleProbesInFlight(dt *drainTracker) int {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	n := 0
	for _, probe := range dt.idleProbes {
		if probe != nil && !probe.ready {
			n++
		}
	}
	return n
}

// noIdleWaitProvider exposes only the runtime.Provider method set, hiding the
// Fake's IdleWaitProvider and BackgroundWorkObserver implementations.
type noIdleWaitProvider struct{ runtime.Provider }

var errFreshCycleTestBusy = errors.New("agent busy")

// TestFreshCycleDefersWhileBusy: a fresh session's recorded bead closes while
// a second bead is assigned to it, and the runtime reports the agent mid-turn.
// The cycle must wait: no kill, no re-stamp of currently_processing_bead_id,
// and one deferral line for the (session, new bead) pair, repeated only after
// the 15-minute log interval.
func TestFreshCycleDefersWhileBusy(t *testing.T) {
	f := newFreshCycleFixture(t)
	f.env.sp.WaitForIdleErrors[f.sessionName] = errFreshCycleTestBusy

	for i := 0; i < 10; i++ {
		f.pass(t)
		if !f.env.sp.IsRunning(f.sessionName) {
			t.Fatalf("pass %d: session killed mid-turn by fresh-cycle; want deferred while busy", i+1)
		}
		if got := f.currentBead(t); got != "wb-prev" {
			t.Fatalf("pass %d: %s = %q, want wb-prev unchanged while deferred", i+1, sessionpkg.CurrentBeadIDKey, got)
		}
	}
	out := f.env.stdout.String()
	if n := strings.Count(out, "Deferred fresh-mode cycle for '"+f.sessionName+"' (wb-prev → wb-new): session busy"); n != 1 {
		t.Fatalf("deferral lines = %d, want 1; stdout:\n%s", n, out)
	}
	if strings.Contains(out, "Cycled fresh-mode session") {
		t.Fatalf("stdout reports a cycle while busy:\n%s", out)
	}

	// Probes alternate between starting (no verdict yet) and reporting busy,
	// so two passes cover one busy verdict.
	f.env.clk.Time = f.env.clk.Time.Add(15 * time.Minute)
	f.pass(t)
	f.pass(t)
	if n := strings.Count(f.env.stdout.String(), "Deferred fresh-mode cycle for '"); n != 2 {
		t.Fatalf("deferral lines after 15m = %d, want 2; stdout:\n%s", n, f.env.stdout.String())
	}
}

// TestFreshCycleRunsAtIdlePrompt continues the busy case: once the agent sits
// at its idle prompt with no activity since the probe, the cycle runs exactly
// once and stamps the new bead.
func TestFreshCycleRunsAtIdlePrompt(t *testing.T) {
	f := newFreshCycleFixture(t)
	f.env.sp.WaitForIdleErrors[f.sessionName] = errFreshCycleTestBusy
	for i := 0; i < 3; i++ {
		f.pass(t)
	}
	if !f.env.sp.IsRunning(f.sessionName) {
		t.Fatal("session killed while busy")
	}

	f.env.sp.WaitForIdleErrors[f.sessionName] = nil
	f.env.clk.Time = f.env.clk.Time.Add(2 * time.Minute)
	// A busy probe may still be settled from the last pass; at most one pass
	// consumes it, one starts an idle probe, and the next cycles.
	cycledOn := 0
	for i := 1; i <= 3 && cycledOn == 0; i++ {
		f.pass(t)
		if !f.env.sp.IsRunning(f.sessionName) {
			cycledOn = i
		}
	}
	if cycledOn == 0 {
		t.Fatalf("session never cycled at its idle prompt; stdout:\n%s", f.env.stdout.String())
	}
	if got := f.currentBead(t); got != "wb-new" {
		t.Fatalf("%s = %q, want wb-new after the cycle", sessionpkg.CurrentBeadIDKey, got)
	}
	out := f.env.stdout.String()
	if n := strings.Count(out, "Cycled fresh-mode session '"+f.sessionName+"' for bead reassign: wb-prev → wb-new after 2m0s deferred"); n != 1 {
		t.Fatalf("cycle lines = %d, want exactly 1 with the deferral duration; stdout:\n%s", n, out)
	}
}

// TestFreshCycleActivityAfterProbeDefers: the probe saw an idle prompt, but
// the pane moved after it completed, so the agent started a new turn. No
// cycle; the next pass starts a new probe.
func TestFreshCycleActivityAfterProbeDefers(t *testing.T) {
	f := newFreshCycleFixture(t)
	f.env.sp.WaitForIdleErrors[f.sessionName] = nil

	f.pass(t) // starts the probe
	if _, ok := f.env.dt.idleProbe(f.sessionID); !ok {
		t.Fatal("pass 1 started no idle probe")
	}
	f.env.sp.SetActivity(f.sessionName, f.env.clk.Now().Add(time.Second))

	f.pass(t) // consumes the probe, sees later activity
	if !f.env.sp.IsRunning(f.sessionName) {
		t.Fatal("session cycled although the pane moved after the idle probe")
	}
	if _, ok := f.env.dt.idleProbe(f.sessionID); ok {
		t.Fatal("probe not cleared after activity was observed")
	}

	f.pass(t)
	if _, ok := f.env.dt.idleProbe(f.sessionID); !ok {
		t.Fatal("pass 3 started no new idle probe")
	}
	if n := f.env.sp.CountCalls("WaitForIdle", f.sessionName); n != 2 {
		t.Fatalf("WaitForIdle calls = %d, want 2", n)
	}
	if got := f.currentBead(t); got != "wb-prev" {
		t.Fatalf("%s = %q, want wb-prev while deferred", sessionpkg.CurrentBeadIDKey, got)
	}
}

// TestFreshCycleBackgroundWorkDefers: an idle prompt with shells still
// running outside the turn is not a safe boundary, and neither is an
// observation error.
func TestFreshCycleBackgroundWorkDefers(t *testing.T) {
	f := newFreshCycleFixture(t)
	f.env.sp.WaitForIdleErrors[f.sessionName] = nil
	f.env.sp.BackgroundWork[f.sessionName] = true
	for i := 0; i < 4; i++ {
		f.pass(t)
		if !f.env.sp.IsRunning(f.sessionName) {
			t.Fatalf("pass %d: session cycled with background work running", i+1)
		}
	}
	if n := f.env.sp.CountCalls("HasBackgroundWork", f.sessionName); n == 0 {
		t.Fatal("HasBackgroundWork never consulted")
	}

	f.env.sp.BackgroundWork[f.sessionName] = false
	f.env.sp.BackgroundWorkErrors[f.sessionName] = errors.New("capture failed")
	for i := 0; i < 4; i++ {
		f.pass(t)
		if !f.env.sp.IsRunning(f.sessionName) {
			t.Fatalf("pass %d: session cycled on a background-work observation error", i+1)
		}
	}

	delete(f.env.sp.BackgroundWorkErrors, f.sessionName)
	for i := 0; i < 3 && f.env.sp.IsRunning(f.sessionName); i++ {
		f.pass(t)
	}
	if f.env.sp.IsRunning(f.sessionName) {
		t.Fatal("session never cycled once background work cleared")
	}
}

// TestFreshCycleWithoutIdleWaitProviderUnchanged: a runtime that cannot wait
// for an idle prompt keeps the pre-existing behavior and cycles on pass 1.
func TestFreshCycleWithoutIdleWaitProviderUnchanged(t *testing.T) {
	f := newFreshCycleFixture(t)
	f.sp = noIdleWaitProvider{f.env.sp}
	f.pass(t)
	if f.env.sp.IsRunning(f.sessionName) {
		t.Fatal("session not cycled on pass 1 by a provider without IdleWaitProvider")
	}
	if got := f.currentBead(t); got != "wb-new" {
		t.Fatalf("%s = %q, want wb-new", sessionpkg.CurrentBeadIDKey, got)
	}
}

// TestFreshCycleUnsupportedIdleWaitCycles: a composite runtime implements
// WaitForIdle for every session but answers ErrInteractionUnsupported for a
// backend that cannot observe a prompt. That answer means "cannot tell", not
// "busy", so the cycle runs as before once the probe reports it.
func TestFreshCycleUnsupportedIdleWaitCycles(t *testing.T) {
	f := newFreshCycleFixture(t)
	f.env.sp.WaitForIdleErrors[f.sessionName] = runtime.ErrInteractionUnsupported
	f.pass(t)
	f.pass(t)
	if f.env.sp.IsRunning(f.sessionName) {
		t.Fatalf("session not cycled after an unsupported idle probe; stdout:\n%s", f.env.stdout.String())
	}
	if got := f.currentBead(t); got != "wb-new" {
		t.Fatalf("%s = %q, want wb-new", sessionpkg.CurrentBeadIDKey, got)
	}
}

// TestFreshCycleSelfClaimWhileDeferredNeverCycles: while the cycle waits, the
// agent finishes its turn and claims the new bead itself. The conversation is
// already on the new bead, so no cycle runs and the stamp catches up.
func TestFreshCycleSelfClaimWhileDeferredNeverCycles(t *testing.T) {
	f := newFreshCycleFixture(t)
	f.env.sp.WaitForIdleErrors[f.sessionName] = errFreshCycleTestBusy
	f.pass(t)
	f.pass(t)
	if !f.env.sp.IsRunning(f.sessionName) {
		t.Fatal("session killed while busy")
	}

	if err := f.env.store.SetMetadata(f.sessionID, beadmeta.CurrentClaimBeadIDMetadataKey, "wb-new"); err != nil {
		t.Fatalf("SetMetadata(current_claim_bead_id): %v", err)
	}
	f.env.sp.WaitForIdleErrors[f.sessionName] = nil
	for i := 0; i < 4; i++ {
		f.pass(t)
		if !f.env.sp.IsRunning(f.sessionName) {
			t.Fatalf("pass %d: session cycled after it self-claimed the new bead", i+1)
		}
	}
	if got := f.currentBead(t); got != "wb-new" {
		t.Fatalf("%s = %q, want wb-new stamped after the self-claim", sessionpkg.CurrentBeadIDKey, got)
	}
	if strings.Contains(f.env.stdout.String(), "Cycled fresh-mode session") {
		t.Fatalf("stdout reports a cycle:\n%s", f.env.stdout.String())
	}
}

// TestFreshCycleGuardsRunBeforeProbe: #6518's open-previous-bead guard defers
// before the idle gate is reached, so no probe is started for it.
func TestFreshCycleGuardsRunBeforeProbe(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "witness", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "witness", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "witness")
	env.desiredState[sessionName] = TemplateParams{Command: "true", SessionName: sessionName, TemplateName: "witness"}
	createPrevBead(t, env, nil) // still open
	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "witness",
		namedSessionModeMetadata:     "on_demand",
		"template":                   "witness",
		"state":                      "active",
		"wake_mode":                  "fresh",
		sessionpkg.CurrentBeadIDKey:  "wb-prev",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	env.sp.WaitForIdleErrors[sessionName] = nil
	workBead := beads.Bead{ID: "wb-new", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})
	waitIdleProbesSettled(t, env.dt)

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("session killed although its previous bead is still open")
	}
	if _, ok := env.dt.idleProbe(session.ID); ok {
		t.Fatal("idle probe started for a cycle the open-previous-bead guard already deferred")
	}
	if n := env.sp.CountCalls("WaitForIdle", sessionName); n != 0 {
		t.Fatalf("WaitForIdle calls = %d, want 0", n)
	}
	if strings.Contains(env.stdout.String(), "Deferred fresh-mode cycle") {
		t.Fatalf("idle-gate deferral logged for a guard deferral:\n%s", env.stdout.String())
	}
}

// TestFreshCycleProbeBudget: five sessions waiting on a fresh cycle share the
// reconciler's idle-probe budget; never more than maxIdleSleepProbesPerTick
// probes are in flight at once, and every session is probed in turn.
func TestFreshCycleProbeBudget(t *testing.T) {
	const n = 5
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	mem := env.store.(*beads.MemStore)
	mem.HonorExplicitIDs = true
	closedAt := time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC)
	var sessions, work []beads.Bead
	var names, ids []string
	for i := 0; i < n; i++ {
		agent := fmt.Sprintf("witness%d", i)
		env.cfg.Agents = append(env.cfg.Agents, config.Agent{Name: agent, StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)})
		env.cfg.NamedSessions = append(env.cfg.NamedSessions, config.NamedSession{Template: agent, Mode: "on_demand"})
		prev := fmt.Sprintf("wb-prev-%d", i)
		if _, err := env.store.Create(beads.Bead{ID: prev, Title: "previous", Type: "task", Status: "in_progress", Assignee: agent}); err != nil {
			t.Fatalf("create %s: %v", prev, err)
		}
		if _, err := env.store.CloseAll([]string{prev}, map[string]string{"closed_at": closedAt.Format(time.RFC3339)}); err != nil {
			t.Fatalf("close %s: %v", prev, err)
		}
		work = append(work, beads.Bead{ID: fmt.Sprintf("wb-new-%d", i), Title: "next", Type: "task", Status: "in_progress", Assignee: agent})
	}
	for i := 0; i < n; i++ {
		agent := fmt.Sprintf("witness%d", i)
		name := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, agent)
		env.desiredState[name] = TemplateParams{Command: "true", SessionName: name, TemplateName: agent}
		session := env.createSessionBead(name)
		env.setSessionMetadata(&session, map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: agent,
			namedSessionModeMetadata:     "on_demand",
			"template":                   agent,
			"state":                      "active",
			"wake_mode":                  "fresh",
			sessionpkg.CurrentBeadIDKey:  fmt.Sprintf("wb-prev-%d", i),
		})
		if err := env.sp.Start(context.Background(), name, runtime.Config{Command: "true"}); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		if err := env.sp.SetMeta(name, "GC_SESSION_ID", session.ID); err != nil {
			t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
		}
		env.sp.WaitForIdleErrors[name] = errFreshCycleTestBusy
		env.sp.WaitForIdleGates[name] = make(chan struct{})
		sessions = append(sessions, session)
		names = append(names, name)
		ids = append(ids, session.ID)
	}
	reload := func() []beads.Bead {
		out := make([]beads.Bead, 0, len(sessions))
		for _, s := range sessions {
			b, err := env.store.Get(s.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", s.ID, err)
			}
			out = append(out, b)
		}
		return out
	}

	for pass := 1; pass <= 3; pass++ {
		reconcileSessionBeadsWithProvider(env, env.sp, reload(), work)
		if got := idleProbesInFlight(env.dt); got != maxIdleSleepProbesPerTick {
			t.Fatalf("pass %d: %d idle probes in flight, want %d", pass, got, maxIdleSleepProbesPerTick)
		}
	}
	for _, name := range names {
		if !env.sp.IsRunning(name) {
			t.Fatalf("%s killed while its probe was outstanding", name)
		}
	}

	// Release the held probes (still busy). Every session must be probed
	// within a few passes, and the budget must hold on each one.
	for _, name := range names {
		close(env.sp.WaitForIdleGates[name])
	}
	for pass := 1; pass <= 6; pass++ {
		reconcileSessionBeadsWithProvider(env, env.sp, reload(), work)
		if got := idleProbesInFlight(env.dt); got > maxIdleSleepProbesPerTick {
			t.Fatalf("release pass %d: %d idle probes in flight, want <= %d", pass, got, maxIdleSleepProbesPerTick)
		}
		waitIdleProbesSettled(t, env.dt)
	}
	for i, name := range names {
		if env.sp.CountCalls("WaitForIdle", name) == 0 {
			t.Fatalf("%s (%s) was never probed", name, ids[i])
		}
		if !env.sp.IsRunning(name) {
			t.Fatalf("%s cycled while busy", name)
		}
	}
}

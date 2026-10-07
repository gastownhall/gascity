package main

import (
	"context"
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

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

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

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

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

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

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

	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

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

// TestReconcileSessionBeads_FreshCycleWakesReliablyNextTick pins the second
// half of ga-2fpf9z's fresh-mode fix: it isn't enough for the handoff tick to
// record state=asleep (TestPhantomReplacementSessionKeyRepro covers that in
// isolation) — the very next reconciler tick must actually observe that
// recorded state and wake the session back up, with no separate trigger,
// stall-detector pass, or heal round-trip required. Before the fix, state
// never left "active", so ComputeAwakeSet's reset-pending desire (gated on
// continuation_reset_pending + reset_committed_at, both already set
// unconditionally by RestartRequestPatch) never got a chance to matter one
// way or the other — but the session also never looked asleep, so nothing
// woke it either.
//
// This deliberately does NOT assert the tick-2 session_key equals the
// tick-1 minted value, even though a literal reading of the bead's
// Done-when item 3 ("assert it is not regenerated") suggests it should.
// That claim does not hold for a wake_mode=fresh session specifically:
// preWakeCommit's freshWake gate (session_wake.go:64) is
// `info.WakeMode == "fresh" || pendingContinuationResetNeedsFreshStart(info)`
// — the first disjunct alone makes EVERY wake of a wake_mode=fresh session a
// fresh-wake reset, unconditionally, regardless of ga-2fpf9z's two fix
// sites (neither touches session_wake.go or PreWakePatch). That reset
// clears session_key again (lifecycle_transition.go:219), independent of
// what RestartRequestPatch minted at cycle time.
//
// Contrast the bead's own cited reference,
// TestReconcileSessionBeads_RestartRequestNamedAlwaysWakesSameTick: that
// session does not set wake_mode=fresh, so the only way freshWake could
// fire is pendingContinuationResetNeedsFreshStart — and RestartRequestPatch
// itself defeats that in the same patch by clearing started_config_hash. So
// for that session shape (fix site #2, non-fresh) the minted key genuinely
// does survive to the literal process launch, exactly as worded. For a
// wake_mode=fresh session (fix site #1, this test) the second reset is
// correct, pre-existing, out-of-scope behavior: a real agent's own
// `gc prime --hook` backfills the session_key this reconciler clears, once
// the real provider process reports its own id (cmd_prime.go:874) — which
// this reconciler-only test correctly does not simulate.
//
// What this fix actually guarantees, and what this test asserts instead:
// the session reaches a real wake commit and starts on the very next tick,
// with no dependency on the heal or the reset-stall detector. Mirroring how
// the cited reference test proves the same thing, this checks IsRunning
// plus last_woke_at (stamped unconditionally by PreWakePatch) rather than
// the "state" field: state is tick-order-dependent even within one
// synchronous reconcile pass — it moves asleep → creating → active as this
// tick's fake ("true") provider is started and then immediately observed
// alive — so last_woke_at, not "creating", is the stable witness that a
// real wake commit (not a no-op) occurred. RestartRequestPatch clears
// last_woke_at at cycle time (tick 1), so a non-empty value here can only
// have come from this tick's own preWakeCommit.
func TestReconcileSessionBeads_FreshCycleWakesReliablyNextTick(t *testing.T) {
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

	workBead := beads.Bead{ID: "wb-B", Title: "next witness wisp", Type: "task", Status: "in_progress", Assignee: "witness"}

	// Tick 1: the assignee/current-bead divergence fires the fresh-cycle
	// kill + mint.
	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{session}, []beads.Bead{workBead})

	if env.sp.IsRunning(sessionName) {
		t.Fatal("session should have been killed by the fresh-cycle handoff")
	}
	cycled, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	minted := cycled.Metadata["session_key"]
	if minted == "" || minted == "conversation-A" {
		t.Fatalf("session_key after cycle = %q, want a freshly minted key", minted)
	}
	if cycled.Metadata["state"] != "asleep" {
		t.Fatalf("state after cycle = %q, want asleep", cycled.Metadata["state"])
	}

	env.stdout.Reset()
	env.stderr.Reset()

	// Tick 2: same work demand still assigned (wb-B, already the recorded
	// current bead — no further cycling), runtime still dead. This is a
	// plain "wake an asleep named session" tick.
	reconcileSessionBeadsWithAssignedWork(env, []beads.Bead{cycled}, []beads.Bead{workBead})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q did not wake on the next tick after the fresh-cycle handoff; ComputeAwakeSet should have desired it reset-pending", sessionName)
	}
	woke, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s) after wake: %v", session.ID, err)
	}
	// preWakeCommit (session_wake.go) is what actually fired here — its
	// PreWakePatch unconditionally stamps last_woke_at, the signal that a
	// real wake commit happened rather than the session merely staying
	// parked asleep. See the doc comment above for why this checks
	// last_woke_at rather than "state".
	if woke.Metadata["last_woke_at"] == "" {
		t.Fatal("last_woke_at after wake = empty, want a timestamp from a real wake commit, not a no-op")
	}
	// session_key is cleared here, not preserved — see the doc comment
	// above. This is preWakeCommit's own unconditional wake_mode=fresh
	// reset, orthogonal to and untouched by ga-2fpf9z's two fix sites; the
	// tick-1 assertions above already pin the part this fix owns (the
	// minted key survives intact up to the point the session goes asleep).
	if woke.Metadata["session_key"] != "" {
		t.Fatalf("session_key after wake = %q, want empty (preWakeCommit's fresh-wake reset fires unconditionally for wake_mode=fresh, independent of this fix)", woke.Metadata["session_key"])
	}
}

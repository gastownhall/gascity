package main

// Regression coverage for ga-2weagw: the #6518 fresh-cycle guard
// (prevAssignedBeadStatus, session_bead_cycle.go) implements ruling
// ga-4byiyc/ga-3pocz7 rows A and C, but two defects make both rows dead code
// for every rig-scoped fresh-mode session:
//
//  1. The guard looks up the previous bead in the city/session store only.
//     Rig work beads (ga-*) live in rig stores, so the lookup fails and the
//     guard "fails toward cycling" instead of deferring.
//  2. The guard reads a metadata "closed_at" key that no real work-bead
//     close ever writes (bd keeps closed_at as a top-level column, not
//     metadata) — so the incarnation-started-after-close signal never
//     fires except in a test fixture that hand-seeds it.
//
// Each test here reproduces one of those shapes and FAILS before the fix
// (prevAssignedBeadStatus resolving via storeref.Topology + falling back to
// UpdatedAt when closed_at metadata is absent) and passes after it.
//
// Out of scope, deliberately not covered here: self-claim recognition
// (rows E/F) is the sibling architecture bead ga-pvjbx3 — see
// /var/tmp/ga-81yg38-artifacts/session_reconciler_fresh_cycle_guard_repro_test.go
// for those repros; they are not duplicated into this package.

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// freshCycleReproEnv builds the fresh-mode named "witness" session the
// existing #6518 tests use, plus a "gascity" rig (prefix "ga") so work beads
// can live in a rig store exactly as every ga-* bead does in production.
func freshCycleReproEnv(t *testing.T, sessionMeta map[string]string) (*restartRequestTestEnv, beads.Bead, string) {
	t.Helper()
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Rigs:          []config.Rig{{Name: "gascity", Prefix: "ga"}},
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
	meta := map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "witness",
		namedSessionModeMetadata:     "on_demand",
		"template":                   "witness",
		"state":                      "active",
		"wake_mode":                  "fresh",
		"session_key":                "conversation-A",
	}
	for k, v := range sessionMeta {
		meta[k] = v
	}
	env.setSessionMetadata(&session, meta)
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	return env, session, sessionName
}

func reproMemStore() *beads.MemStore {
	s := beads.NewMemStore()
	s.HonorExplicitIDs = true
	return s
}

// reproCreate creates b and then applies its status: MemStore.Create stores
// every new bead as "open" whatever Status says, so an in_progress claim must be
// written the way a real claim writes it, as an update.
func reproCreate(t *testing.T, s beads.Store, b beads.Bead) {
	t.Helper()
	if _, err := s.Create(b); err != nil {
		t.Fatalf("creating %s: %v", b.ID, err)
	}
	if b.Status != "" && b.Status != "open" {
		status := b.Status
		if err := s.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("setting %s status %s: %v", b.ID, status, err)
		}
	}
}

// reproCloseForReal closes id through the store's own Close — the path `bd
// close` takes. Unlike createPrevBead it does NOT hand-seed metadata closed_at:
// real work beads never carry that key (bd keeps closed_at as a top-level
// column, and neither bdIssue nor beads.Bead decodes it).
func reproCloseForReal(t *testing.T, s beads.Store, id string) {
	t.Helper()
	if err := s.Close(id); err != nil {
		t.Fatalf("closing %s: %v", id, err)
	}
}

// reconcileFreshCycleRepro mirrors reconcileSessionBeadsWithAssignedWork but
// threads rig stores through, the way city_runtime.go's production tick does.
func reconcileFreshCycleRepro(env *restartRequestTestEnv, sessions, assignedWork []beads.Bead, rigStores map[string]beads.Store) {
	poolDesired := make(map[string]int)
	for _, tp := range env.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	_ = reconcileSessionBeadsAtPath(
		context.Background(), "", sessions, env.desiredState, cfgNames, env.cfg, env.sp, env.store,
		nil, assignedWork, rigStores, nil, env.dt, poolDesired, false, nil, "", nil,
		env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)
}

func assertNotCycled(t *testing.T, env *restartRequestTestEnv, session beads.Bead, sessionName, why string) {
	t.Helper()
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session was killed by fresh-cycle; want it left running: %s\nstdout: %s", why, env.stdout.String())
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata["session_key"] != "conversation-A" {
		t.Fatalf("session_key = %q, want conversation-A preserved (no cycle): %s", got.Metadata["session_key"], why)
	}
}

// Case 1 (22:07:37 local, gascity--builder ga-p69yam -> ga-851h6i). The
// previous bead is OPEN — it had been re-routed to the reviewer — so ruling
// ga-4byiyc row A says defer. It lives in the gascity RIG store. The guard
// looks it up in the city/session store only (prevAssignedBeadStatus(store,
// prev), session_reconciler.go), gets not-found, and "fails toward the
// pre-existing cycle behavior".
func TestFreshCycleRepro_OpenPreviousBeadInRigStoreDefers(t *testing.T) {
	env, session, sessionName := freshCycleReproEnv(t, map[string]string{
		sessionpkg.CurrentBeadIDKey: "ga-prev",
		"awake_started_at":          "2026-03-08T09:00:00Z",
	})
	rig := reproMemStore()
	reproCreate(t, rig, beads.Bead{ID: "ga-prev", Title: "handed-off review bead", Type: "task", Status: "open", Assignee: "reviewer"})
	reproCreate(t, rig, beads.Bead{ID: "ga-new", Title: "next step the session claimed", Type: "task", Status: "in_progress", Assignee: "witness"})
	anchor, _ := rig.Get("ga-new")

	reconcileFreshCycleRepro(env, []beads.Bead{session}, []beads.Bead{anchor}, map[string]beads.Store{"gascity": rig})

	assertNotCycled(t, env, session, sessionName, "previous bead ga-prev is still open in the rig store (row A)")
}

// Row C (ga-3pocz7 01:09Z amendment) under a REAL close: this incarnation
// started after the previous bead closed, so it is already fresh. The guard
// reads closed_at from metadata, which no work-bead close ever writes, so
// prevClosedAt is always zero and the defer never fires. The existing row-C
// test passes only because createPrevBead hand-seeds metadata closed_at.
func TestFreshCycleRepro_IncarnationStartedAfterRealCloseDefers(t *testing.T) {
	env, session, sessionName := freshCycleReproEnv(t, nil)
	env.store.(*beads.MemStore).HonorExplicitIDs = true
	reproCreate(t, env.store, beads.Bead{ID: "wb-prev", Title: "previous", Type: "task", Status: "in_progress", Assignee: "witness"})
	reproCloseForReal(t, env.store, "wb-prev")
	awake := time.Now().UTC().Add(time.Minute) // started strictly after the close
	env.setSessionMetadata(&session, map[string]string{
		sessionpkg.CurrentBeadIDKey: "wb-prev",
		"awake_started_at":          awake.Format(time.RFC3339Nano),
	})
	anchor := beads.Bead{ID: "wb-new", Title: "new", Type: "task", Status: "in_progress", Assignee: "witness"}

	reconcileFreshCycleRepro(env, []beads.Bead{session}, []beads.Bead{anchor}, nil)

	assertNotCycled(t, env, session, sessionName, "this incarnation started after wb-prev closed (row C)")
}

// Row C again, but with the previous bead in the rig store — both defects at
// once, which is the production shape for every ga-* bead.
func TestFreshCycleRepro_IncarnationStartedAfterRealCloseInRigStoreDefers(t *testing.T) {
	env, session, sessionName := freshCycleReproEnv(t, nil)
	rig := reproMemStore()
	reproCreate(t, rig, beads.Bead{ID: "ga-prev", Title: "previous", Type: "task", Status: "in_progress", Assignee: "witness"})
	reproCloseForReal(t, rig, "ga-prev")
	awake := time.Now().UTC().Add(time.Minute)
	env.setSessionMetadata(&session, map[string]string{
		sessionpkg.CurrentBeadIDKey: "ga-prev",
		"awake_started_at":          awake.Format(time.RFC3339Nano),
	})
	reproCreate(t, rig, beads.Bead{ID: "ga-new", Title: "new", Type: "task", Status: "in_progress", Assignee: "witness"})
	anchor, _ := rig.Get("ga-new")

	reconcileFreshCycleRepro(env, []beads.Bead{session}, []beads.Bead{anchor}, map[string]beads.Store{"gascity": rig})

	assertNotCycled(t, env, session, sessionName, "this incarnation started after ga-prev closed (row C, rig store)")
}

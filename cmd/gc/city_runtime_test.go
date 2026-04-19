package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSweepUndesiredPoolSessionBeads_KeepsRunningSessionsOpen(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "active",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-bd-123", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		sp,
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("running pool bead was closed: %+v", got)
	}
}

func TestCityRuntimeRequestDeferredDrainFollowUpTick_PokesOnce(t *testing.T) {
	cr := &CityRuntime{
		sessionDrains: newDrainTracker(),
		pokeCh:        make(chan struct{}, 1),
	}
	cr.sessionDrains.set("bead-1", &drainState{followUp: true})

	cr.requestDeferredDrainFollowUpTick()

	select {
	case <-cr.pokeCh:
	default:
		t.Fatal("expected deferred drain follow-up to enqueue a poke")
	}

	if ds := cr.sessionDrains.get("bead-1"); ds == nil || ds.followUp {
		t.Fatal("expected deferred drain follow-up flag to be consumed")
	}

	cr.requestDeferredDrainFollowUpTick()

	select {
	case <-cr.pokeCh:
		t.Fatal("unexpected second poke without a new deferred drain follow-up")
	default:
	}
}

// Pool session beads in the "creating" window (tmux not yet up, work not yet
// assigned) must not be swept. Otherwise the sweep runs on the same tick the
// pool creates the bead, observes zero assigned work, and closes it — the
// pool re-spawns on the next tick, same fate, and the pool spins forever
// without a session reaching the ready state.
func TestSweepUndesiredPoolSessionBeads_SkipsCreatingState(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "creating",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — creating state must be preserved", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("bead in creating state was swept closed: %+v", got)
	}
}

// Age grace period: pool session beads that have moved past "creating" but
// are still younger than staleCreatingStateTimeout must not be swept. The
// tmux wake pipeline and work assignment happen across multiple ticks after
// state=creation_complete is set; sweeping in that window causes the same
// spin as sweeping during creation.
func TestSweepUndesiredPoolSessionBeads_SkipsRecentlyCreated(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-recent",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			// Post-creating: state/state_reason are advanced but last_woke
			// hasn't landed yet. The real-world state observed as being
			// swept incorrectly.
			"state":                "active",
			"state_reason":         "creation_complete",
			"creation_complete_at": time.Now().UTC().Format(time.RFC3339),
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — bead within staleCreatingStateTimeout window must survive", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("recently-created post-creating bead was swept: %+v", got)
	}
}

// Stale creating-state beads (CreatedAt older than staleCreatingStateTimeout)
// MUST be sweepable. Without this, a bead wedged in `creating` past the
// timeout would be permanently immune from this sweep path, breaking the
// symmetry with sessionStartRequested.
func TestSweepUndesiredPoolSessionBeads_SweepsStaleCreatingState(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-stale",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "creating",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bead.CreatedAt = time.Now().Add(-2 * time.Minute)
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 — stale creating bead must be sweepable", closed)
	}
}

// Stale post-creating beads (state=active, last_woke_at="",
// creation_complete_at older than staleCreatingStateTimeout) MUST be
// sweepable. Without this, the grace window would never expire.
func TestSweepUndesiredPoolSessionBeads_SweepsLongStuckActiveWithoutWake(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-stale-active",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "active",
			"state_reason":         "creation_complete",
			"creation_complete_at": time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339),
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 — bead beyond staleCreatingStateTimeout must be sweepable", closed)
	}
}

// Missing creation_complete_at (older beads predating the per-start marker,
// or beads produced by paths that don't stamp the marker) MUST be sweepable
// rather than protected indefinitely.
func TestSweepUndesiredPoolSessionBeads_SweepsActiveWithoutCreationCompleteAt(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-no-marker",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "active",
			"state_reason":         "creation_complete",
			// creation_complete_at intentionally absent.
			"continuation_epoch": "1",
			"generation":         "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 — bead without creation_complete_at must be sweepable", closed)
	}
}

// The reconciler's healStatePatch rewrites a live bead from state=active
// to state=awake (session_reconcile.go). "awake" is semantically
// equivalent to "active" in this codebase, and both must receive the
// same post-create sweep protection — otherwise the same spin loop
// reopens on the alias path.
func TestSweepUndesiredPoolSessionBeads_SkipsAwakeStateInPreWakeWindow(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-awake",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			// healStatePatch rewrote state=active → state=awake while the
			// runtime was alive; the pre-wake condition is preserved
			// because last_woke_at has not yet landed (or was cleared).
			"state":                "awake",
			"state_reason":         "creation_complete",
			"creation_complete_at": time.Now().UTC().Format(time.RFC3339),
			"last_woke_at":         "",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — state=awake in pre-wake window must receive same protection as state=active", closed)
	}
}

// Recovery of an already-active bead (recoverRunningPendingCreate path:
// state=active + pending_create_claim=true + alive runtime) must produce
// a fresh creation_complete_at so the healed bead stays protected in the
// pre-wake window on the following tick. This test asserts the sweep's
// side of that contract — a state=active bead with a fresh
// creation_complete_at and empty last_woke_at survives the sweep.
func TestSweepUndesiredPoolSessionBeads_SkipsRecoveredActiveBead(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-recovered",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			// Post-recovery shape: state was already active, recovery just
			// cleared pending_create_claim and stamped a fresh marker.
			"state":                "active",
			"state_reason":         "creation_complete",
			"creation_complete_at": time.Now().UTC().Format(time.RFC3339),
			"last_woke_at":         "",
			// Historical counters survive recovery.
			"wake_attempts":      "1",
			"continuation_epoch": "1",
			"generation":         "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — recovered active bead with fresh marker must survive pre-wake", closed)
	}
}

// Crashed-then-recently-restarted beads: wake_attempts/churn_count are
// preserved across a successful restart (CommitStartedPatch does not reset
// them), so the post-create guard CANNOT be keyed on those counters or a
// legitimate restart after a prior crash would fall into the same spin
// loop. Gating on a fresh creation_complete_at lets a just-restarted bead
// survive the pre-wake window even when its historical counters are
// non-zero.
func TestSweepUndesiredPoolSessionBeads_SkipsFreshRestartAfterPriorCrash(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-restart-after-crash",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			// Just-restarted after a prior crash: state transitioned back
			// to active with a fresh creation_complete_at, but historical
			// failure counters remain because clearWakeFailures only fires
			// after the session is stable-long-enough.
			"state":                "active",
			"state_reason":         "creation_complete",
			"creation_complete_at": time.Now().UTC().Format(time.RFC3339),
			"wake_attempts":        "2",
			"churn_count":          "1",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — fresh restart after prior crash must survive the pre-wake window", closed)
	}
}

// Crashed beads (state=active, last_woke_at="" cleared by checkStability,
// creation_complete_at stale because the last successful start was long
// ago) MUST be sweepable. checkStability/checkChurn/start-failure do not
// touch creation_complete_at, so an old marker is the signal that the
// state=active+empty-last_woke_at shape came from a crash-clear rather
// than a fresh start.
func TestSweepUndesiredPoolSessionBeads_SweepsCrashedActiveBead(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-crashed",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "active",
			"state_reason":         "creation_complete",
			"creation_complete_at": time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339),
			"last_woke_at":         "",
			"wake_attempts":        "1",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 — crashed bead with stale creation_complete_at must be swept", closed)
	}
}

func TestSweepUndesiredPoolSessionBeads_SkipsPendingCreateClaim(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"pending_create_claim": "true",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — pending_create_claim must be preserved", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("bead with pending_create_claim was swept closed: %+v", got)
	}
}

// pending_create_claim is an authoritative ownership flag for the lifecycle
// reconciler (sessionStartRequested in session_reconcile.go). The sweep must
// honor that contract regardless of age — expiring it here would let the
// sweep close a bead the reconciler still considers live.
func TestSweepUndesiredPoolSessionBeads_SkipsStalePendingCreateClaim(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-stale-claim",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"pending_create_claim": "true",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bead.CreatedAt = time.Now().Add(-2 * time.Minute)
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 — pending_create_claim must remain authoritative regardless of age", closed)
	}
}

func TestSweepUndesiredPoolSessionBeads_ClosesStoppedSessions(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "drained",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("stopped pool bead status = %q, want closed", got.Status)
	}
}

func TestSweepUndesiredPoolSessionBeads_KeepsAssignedSessionsOpen(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "asleep",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "assigned work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "worker-bd-123",
	}); err != nil {
		t.Fatalf("Create work bead: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		false,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("assigned pool bead was swept closed: %+v", got)
	}
}

func TestSweepUndesiredPoolSessionBeads_SkipsPartialAssignedSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "drained",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{bead})

	closed := sweepUndesiredPoolSessionBeads(
		store,
		nil,
		sessionBeads,
		nil,
		&config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}},
		runtime.NewFake(),
		true,
	)
	if closed != 0 {
		t.Fatalf("closed = %d, want 0", closed)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("partial assigned-work snapshot should suppress sweep: %+v", got)
	}
}

func TestCityRuntimeBeadReconcileTick_TransientStoreQueryPartialKeepsRunningPoolSessionUntilRecoveryTick(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-bd-123",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "awake",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-bd-123", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "maintainer-city",
		cfg:                 &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}},
		sp:                  sp,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	partialResult := DesiredStateResult{
		State:             map[string]TemplateParams{},
		ScaleCheckCounts:  map[string]int{"worker": 0},
		StoreQueryPartial: true,
	}
	cr.beadReconcileTick(context.Background(), partialResult, newSessionBeadSnapshot([]beads.Bead{session}), nil)

	afterPartial, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after partial tick: %v", err)
	}
	if afterPartial.Status == "closed" {
		t.Fatalf("partial tick closed running session: %+v", afterPartial)
	}
	if !sp.IsRunning("worker-bd-123") {
		t.Fatal("partial tick should not stop the running worker")
	}

	recoveredResult := DesiredStateResult{
		State:            map[string]TemplateParams{},
		ScaleCheckCounts: map[string]int{"worker": 0},
		AssignedWorkBeads: []beads.Bead{
			workBead("ga-live", "worker", "worker-bd-123", "in_progress", 5),
		},
	}
	cr.beadReconcileTick(context.Background(), recoveredResult, cr.loadSessionBeadSnapshot(), nil)

	afterRecovered, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after recovered tick: %v", err)
	}
	if afterRecovered.Status == "closed" {
		t.Fatalf("recovered tick closed running session: %+v", afterRecovered)
	}
	if state := afterRecovered.Metadata["state"]; state == "drained" || state == "asleep" {
		t.Fatalf("recovered tick state = %q, want active/awake", state)
	}
	if !sp.IsRunning("worker-bd-123") {
		t.Fatal("recovered tick should keep the worker running")
	}
}

func TestCityRuntimeBeadReconcileTick_KeepsAssignedPoolWorkerAwake(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "claude",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:gascity/claude"},
		Metadata: map[string]string{
			"session_name":         "claude-mc-live",
			"template":             "gascity/claude",
			"agent_name":           "gascity/claude",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "awake",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "claude-mc-live", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "maintainer-city",
		cfg:                 &config.City{Agents: []config.Agent{{Name: "claude", Dir: "gascity", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}},
		sp:                  sp,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	result := DesiredStateResult{
		State:            map[string]TemplateParams{},
		ScaleCheckCounts: map[string]int{"gascity/claude": 0},
		AssignedWorkBeads: []beads.Bead{
			workBead("ga-live", "gascity/claude", "claude-mc-live", "in_progress", 5),
		},
	}

	sessionBeads := newSessionBeadSnapshot([]beads.Bead{session})
	cr.beadReconcileTick(context.Background(), result, sessionBeads, nil)

	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session bead: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("assigned pool worker was closed: %+v", got)
	}
	if state := got.Metadata["state"]; state == "drained" || state == "asleep" {
		t.Fatalf("assigned pool worker state = %q, want active/awake", state)
	}
	if !sp.IsRunning("claude-mc-live") {
		t.Fatal("assigned pool worker should still be running")
	}
}

func TestCityRuntimeBeadReconcileTick_SweepRespectsLiveAssignedWork(t *testing.T) {
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":         "worker-mc-live",
			"template":             "worker",
			"agent_name":           "worker",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "asleep",
			"continuation_epoch":   "1",
			"generation":           "1",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	// Persist an open work bead assigned to the session. GCSweepSessionBeads
	// now runs a live store query via sessionHasOpenAssignedWork, so the
	// bead must live in the store itself — a pre-computed snapshot is no
	// longer consulted.
	if _, err := store.Create(beads.Bead{
		ID:       "ga-future",
		Title:    "future work",
		Type:     "task",
		Status:   "open",
		Assignee: "worker-mc-live",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	}); err != nil {
		t.Fatalf("Create work bead: %v", err)
	}

	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "maintainer-city",
		cfg:                 &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5)}}},
		sp:                  runtime.NewFake(),
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	result := DesiredStateResult{
		State:             map[string]TemplateParams{},
		ScaleCheckCounts:  map[string]int{"worker": 0},
		AssignedWorkBeads: []beads.Bead{},
	}

	sessionBeads := newSessionBeadSnapshot([]beads.Bead{session})
	cr.beadReconcileTick(context.Background(), result, sessionBeads, nil)

	got, err := store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session bead: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead was swept closed despite live assigned work: %+v", got)
	}
}

func TestCityRuntimeTick_RefreshesManualSessionOverlayAfterSync(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir prompts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "prompts", "worker.md"), []byte("# worker\n"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	store := beads.NewMemStore()
	manual, err := store.Create(beads.Bead{
		Title:  "hal",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":             "helper",
			"manual_session":       "true",
			"alias":                "hal",
			"state":                "creating",
			"pending_create_claim": "true",
		},
	})
	if err != nil {
		t.Fatalf("Create manual session bead: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{
			Name:     "my-city",
			Provider: "claude",
		},
		Providers: map[string]config.ProviderSpec{
			"claude": {
				Command:    "echo",
				PromptMode: "arg",
			},
		},
		Agents: []config.Agent{{
			Name:              "helper",
			PromptTemplate:    "prompts/worker.md",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
			ScaleCheck:        "printf 0",
		}},
	}

	sp := runtime.NewFake()
	var stderr bytes.Buffer
	var mutated bool

	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "my-city",
		cfg:                 cfg,
		sp:                  sp,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              &stderr,
	}
	cr.buildFnWithSessionBeads = func(
		c *config.City,
		currentSP runtime.Provider,
		store beads.Store,
		rigStores map[string]beads.Store,
		sessionBeads *sessionBeadSnapshot,
		trace *sessionReconcilerTraceCycle,
	) DesiredStateResult {
		result := buildDesiredStateWithSessionBeads("my-city", cityPath, time.Now(), c, currentSP, store, rigStores, sessionBeads, trace, &stderr)
		if !mutated {
			if err := store.SetMetadata(manual.ID, "session_name", sessionNameFromBeadID(manual.ID)); err != nil {
				t.Fatalf("SetMetadata(session_name): %v", err)
			}
			mutated = true
		}
		return result
	}

	var prevPoolRunning map[string]bool
	var lastProviderName string
	dirty := &atomic.Bool{}
	cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "test")

	if !mutated {
		t.Fatal("test setup did not mutate the manual session bead between build and reconcile")
	}
	got, err := store.Get(manual.ID)
	if err != nil {
		t.Fatalf("Get manual session bead: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("manual session bead was closed after refreshed overlay should have preserved it: %+v", got)
	}
	if got.Metadata["state"] == "orphaned" || got.Metadata["close_reason"] == "orphaned" {
		t.Fatalf("manual session bead was marked orphaned after refreshed overlay: %+v", got.Metadata)
	}
}

func TestCityRuntimeTickRunsOnDeathWithCanonicalRigEnv(t *testing.T) {
	cityPath, rigDir, cfg := newControllerProbeFixture(t)
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "3308",
		DoltUser:       "rig-user",
	})
	writeScopePassword(t, rigDir, "rig-secret")

	cfg.Workspace.Name = "my-city"
	outFile := filepath.Join(t.TempDir(), "on-death-env.txt")
	cfg.Agents[0] = config.Agent{
		Name:              "worker",
		Dir:               "demo",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(2),
	}

	handlers := computePoolDeathHandlers(cfg, "my-city", cityPath, runtime.NewFake(), nil)
	if len(handlers) == 0 {
		t.Fatal("computePoolDeathHandlers returned no handlers")
	}
	prevPoolRunning := map[string]bool{}
	for sessionName, info := range handlers {
		info.Command = "printf '%s|%s|%s' \"${GC_DOLT_PORT:-}\" \"${GC_DOLT_USER:-}\" \"${GC_DOLT_PASSWORD:-}\" > " + outFile
		handlers[sessionName] = info
		prevPoolRunning[sessionName] = true
		break
	}

	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "my-city",
		cfg:                 cfg,
		sp:                  runtime.NewFake(),
		standaloneCityStore: beads.NewMemStore(),
		sessionDrains:       newDrainTracker(),
		poolDeathHandlers:   handlers,
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              &stderr,
		buildFnWithSessionBeads: func(_ *config.City, _ runtime.Provider, _ beads.Store, _ map[string]beads.Store, _ *sessionBeadSnapshot, _ *sessionReconcilerTraceCycle) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}

	dirty := &atomic.Bool{}
	var lastProviderName string
	cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "test")

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v\nstderr=%s", outFile, err, stderr.String())
	}
	if got := strings.TrimSpace(string(data)); got != "3308|rig-user|rig-secret" {
		t.Fatalf("on_death env = %q, want %q", got, "3308|rig-user|rig-secret")
	}
}

func TestControlDispatcherOnlyConfig_IncludesRigScopedDispatchers(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "claude"},
			{Name: config.ControlDispatcherAgentName},
			{Name: config.ControlDispatcherAgentName, Dir: "gascity"},
		},
	}

	filtered := controlDispatcherOnlyConfig(cfg)
	if filtered == nil {
		t.Fatal("filtered config = nil")
	}
	if len(filtered.Agents) != 2 {
		t.Fatalf("len(filtered.Agents) = %d, want 2", len(filtered.Agents))
	}
	if filtered.Agents[0].QualifiedName() != "control-dispatcher" {
		t.Fatalf("filtered city dispatcher = %q, want control-dispatcher", filtered.Agents[0].QualifiedName())
	}
	if filtered.Agents[1].QualifiedName() != "gascity/control-dispatcher" {
		t.Fatalf("filtered rig dispatcher = %q, want gascity/control-dispatcher", filtered.Agents[1].QualifiedName())
	}
}

func TestCityRuntimeBuildDesiredState_StandaloneIncludesRigStores(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	var gotRigStores map[string]beads.Store

	cr := &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "maintainer-city",
		cfg:                 &config.City{Rigs: []config.Rig{{Name: "gascity"}}},
		sp:                  runtime.NewFake(),
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{"gascity": rigStore},
		buildFnWithSessionBeads: func(_ *config.City, _ runtime.Provider, store beads.Store, rigStores map[string]beads.Store, _ *sessionBeadSnapshot, _ *sessionReconcilerTraceCycle) DesiredStateResult {
			if store != cityStore {
				t.Fatalf("store = %v, want city store", store)
			}
			gotRigStores = rigStores
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}

	cr.buildDesiredState(nil, nil)

	if len(gotRigStores) != 1 {
		t.Fatalf("len(rigStores) = %d, want 1", len(gotRigStores))
	}
	if gotRigStores["gascity"] != rigStore {
		t.Fatalf("rigStores[gascity] = %v, want rig store", gotRigStores["gascity"])
	}
}

func TestCityRuntimeReloadProviderSwapPreservesDrainTracker(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	cr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: io.Discard,
	})

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	// Manually initialize drain tracker (normally done in run()).
	cr.sessionDrains = newDrainTracker()

	writeCityRuntimeConfig(t, tomlPath, "fail")
	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)

	if lastProviderName != "fail" {
		t.Fatalf("lastProviderName = %q, want fail", lastProviderName)
	}
	if cr.sessionDrains == nil {
		t.Fatal("sessionDrains = nil after provider swap, want non-nil")
	}
}

func TestCityRuntimeReloadAllowsRegistryAliasDifferentFromWorkspaceName(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfigNamed(t, tomlPath, "workspace-name", "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "machine-alias",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "machine-alias", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()

	writeCityRuntimeConfigNamed(t, tomlPath, "workspace-name", "fail")
	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)

	if lastProviderName != "fail" {
		t.Fatalf("lastProviderName = %q, want fail; stderr=%q", lastProviderName, stderr.String())
	}
	if strings.Contains(stderr.String(), "workspace.name changed") {
		t.Fatalf("reload treated registry alias as workspace drift: %s", stderr.String())
	}
}

func TestCityRuntimeReloadLifecycleFailureKeepsOldConfig(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cr := newCityRuntime(CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		LogPrefix: "gc reload",
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()

	oldCfg := cr.cfg
	oldSP := cr.sp
	oldDops := cr.dops
	oldRev := cr.configRev

	prev := cityRuntimeStartBeadsLifecycle
	cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, io.Writer) error {
		return fmt.Errorf("boom")
	}
	t.Cleanup(func() {
		cityRuntimeStartBeadsLifecycle = prev
	})

	data := []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nshutdown_timeout = \"1s\"\n")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
	}
	if !strings.Contains(reply.Error, "config reload: boom") {
		t.Fatalf("reply.Error = %q, want lifecycle error", reply.Error)
	}
	if cr.cfg != oldCfg {
		t.Fatal("cfg changed after lifecycle reload failure")
	}
	if cr.sp != oldSP {
		t.Fatal("provider changed after lifecycle reload failure")
	}
	if cr.dops != oldDops {
		t.Fatal("drain ops changed after lifecycle reload failure")
	}
	if cr.configRev != oldRev {
		t.Fatalf("configRev = %q, want %q", cr.configRev, oldRev)
	}
	if lastProviderName != "fake" {
		t.Fatalf("lastProviderName = %q, want fake", lastProviderName)
	}
	if !strings.Contains(stderr.String(), "config reload: boom (keeping old config)") {
		t.Fatalf("stderr = %q, want lifecycle failure", stderr.String())
	}
	if strings.Contains(stdout.String(), "Session provider swapped") {
		t.Fatalf("stdout = %q, want no provider swap message", stdout.String())
	}
	if strings.Contains(stdout.String(), "Config reloaded:") {
		t.Fatalf("stdout = %q, want no reload success message", stdout.String())
	}
}

func TestCityRuntimeReloadStrictWarningsReturnedOnFailure(t *testing.T) {
	oldStrict := strictMode
	strictMode = true
	t.Cleanup(func() { strictMode = oldStrict })

	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cr := newCityRuntime(CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		LogPrefix: "gc reload",
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: &stderr,
	})

	if err := os.WriteFile(tomlPath, []byte(`include = ["override.toml"]

[workspace]
name = "test-city"
install_agent_hooks = ["claude"]

[session]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "override.toml"), []byte(`[workspace]
install_agent_hooks = ["codex"]
`), 0o644); err != nil {
		t.Fatalf("write override.toml: %v", err)
	}

	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
	}
	if !strings.Contains(reply.Error, "strict mode: 1 collision warning(s)") {
		t.Fatalf("reply.Error = %q", reply.Error)
	}
	if !warningsContain(reply.Warnings, "workspace.install_agent_hooks redefined") {
		t.Fatalf("reply.Warnings = %v, want collision warning", reply.Warnings)
	}
	if !warningsContain(reply.Warnings, reloadStrictWarningHint) {
		t.Fatalf("reply.Warnings = %v, want strict recovery hint", reply.Warnings)
	}
	if !strings.Contains(stderr.String(), "gc reload: warning: workspace.install_agent_hooks redefined") {
		t.Fatalf("stderr = %q, want warning details", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc reload: warning: "+reloadStrictWarningHint) {
		t.Fatalf("stderr = %q, want strict recovery hint", stderr.String())
	}
	if strings.Contains(stderr.String(), "gc start:") {
		t.Fatalf("stderr = %q, want reload-specific prefix without gc start", stderr.String())
	}
}

func TestCityRuntimeReloadNonStrictWarningsReturnedOnValidationFailure(t *testing.T) {
	oldStrict := strictMode
	strictMode = false
	t.Cleanup(func() { strictMode = oldStrict })

	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stderr bytes.Buffer
	cr := newCityRuntime(CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		LogPrefix: "gc reload",
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: &stderr,
	})
	oldCfg := cr.cfg

	if err := os.WriteFile(tomlPath, []byte(`include = ["override.toml"]

[workspace]
name = "test-city"
install_agent_hooks = ["claude"]

[session]
provider = "fake"

[[agent]]
name = "bad name"
`), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "override.toml"), []byte(`[workspace]
install_agent_hooks = ["codex"]
`), 0o644); err != nil {
		t.Fatalf("write override.toml: %v", err)
	}

	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)

	if reply.Outcome != reloadOutcomeFailed {
		t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
	}
	if !strings.Contains(reply.Error, "validating agents") {
		t.Fatalf("reply.Error = %q, want validation failure", reply.Error)
	}
	if !warningsContain(reply.Warnings, "workspace.install_agent_hooks redefined") {
		t.Fatalf("reply.Warnings = %v, want composition warning", reply.Warnings)
	}
	if !strings.Contains(stderr.String(), "gc reload: warning: workspace.install_agent_hooks redefined") {
		t.Fatalf("stderr = %q, want warning details", stderr.String())
	}
	if cr.cfg != oldCfg {
		t.Fatal("cfg changed after validation reload failure")
	}
}

func TestCityRuntimeFailActiveReloadRepliesAndClears(t *testing.T) {
	doneCh := make(chan reloadControlReply, 1)
	cr := &CityRuntime{
		activeReload: &reloadRequest{doneCh: doneCh},
	}

	cr.failActiveReload("Reload canceled because the controller is shutting down.")

	if cr.activeReload != nil {
		t.Fatal("activeReload was not cleared")
	}
	select {
	case reply := <-doneCh:
		if reply.Outcome != reloadOutcomeFailed {
			t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeFailed)
		}
		if !strings.Contains(reply.Error, "shutting down") {
			t.Fatalf("reply.Error = %q, want shutdown reason", reply.Error)
		}
	default:
		t.Fatal("active reload did not receive cancellation reply")
	}
}

func TestCityRuntimeHandleReloadRequestInitializesConfigDirty(t *testing.T) {
	acceptedCh := make(chan reloadControlReply, 1)
	req := &reloadRequest{
		acceptedCh: acceptedCh,
		doneCh:     make(chan reloadControlReply, 1),
	}
	cr := &CityRuntime{
		pokeCh: make(chan struct{}, 1),
	}

	cr.handleReloadRequest(req)

	if cr.configDirty == nil {
		t.Fatal("configDirty was not initialized")
	}
	if !cr.configDirty.Load() {
		t.Fatal("configDirty = false, want reload request to mark dirty")
	}
	if cr.activeReload != req {
		t.Fatal("activeReload was not recorded")
	}
	select {
	case <-cr.pokeCh:
	default:
		t.Fatal("reload request did not enqueue poke")
	}
	select {
	case reply := <-acceptedCh:
		if reply.Outcome != reloadOutcomeAccepted {
			t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeAccepted)
		}
	default:
		t.Fatal("reload request did not receive accepted reply")
	}
}

func TestCityRuntimeReloadSameRevisionIsNoOp(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	configRev := config.Revision(fsys.OSFS{}, prov, cfg, cityPath)

	sp := runtime.NewFake()
	var stdout bytes.Buffer
	cr := newCityRuntime(CityRuntimeParams{
		CityPath:  cityPath,
		CityName:  "test-city",
		TomlPath:  tomlPath,
		ConfigRev: configRev,
		Cfg:       cfg,
		SP:        sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: io.Discard,
	})

	oldCfg := cr.cfg
	lastProviderName := "fake"
	cr.reloadConfig(context.Background(), &lastProviderName, cityPath)

	if cr.cfg != oldCfg {
		t.Fatal("same-revision reload should keep existing config pointer")
	}
	if cr.configRev != configRev {
		t.Fatalf("configRev = %q, want %q", cr.configRev, configRev)
	}
	if lastProviderName != "fake" {
		t.Fatalf("lastProviderName = %q, want fake", lastProviderName)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty for same-revision reload", stdout.String())
	}
}

func TestCityRuntimeManualReloadReplyWaitsForTickCompletion(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	configRev := config.Revision(fsys.OSFS{}, prov, cfg, cityPath)

	doneCh := make(chan reloadControlReply, 1)
	dirty := &atomic.Bool{}
	dirty.Store(true)
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	cr := newCityRuntime(CityRuntimeParams{
		CityPath:    cityPath,
		CityName:    "test-city",
		TomlPath:    tomlPath,
		ConfigRev:   configRev,
		ConfigDirty: dirty,
		Cfg:         cfg,
		SP:          sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			select {
			case reply := <-doneCh:
				t.Fatalf("manual reload replied before desired-state rebuild: %+v", reply)
			default:
			}
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: io.Discard,
	})
	cr.activeReload = &reloadRequest{doneCh: doneCh}
	lastProviderName := "fake"
	var prevPoolRunning map[string]bool

	cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "poke")

	select {
	case reply := <-doneCh:
		if reply.Outcome != reloadOutcomeNoChange {
			t.Fatalf("reply.Outcome = %q, want %q", reply.Outcome, reloadOutcomeNoChange)
		}
	default:
		t.Fatal("manual reload did not reply after tick completion")
	}
	if cr.activeReload != nil {
		t.Fatal("activeReload was not cleared")
	}
}

func TestCityRuntimeRunStopsBeforeStartedWhenCanceledDuringStartup(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	var stdout bytes.Buffer
	var started bool

	ctx, cancel := context.WithCancel(context.Background())
	cr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			cancel()
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:      newDrainOps(sp),
		Rec:       events.Discard,
		OnStarted: func() { started = true },
		Stdout:    &stdout,
		Stderr:    io.Discard,
	})

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	cr.run(ctx)

	if started {
		t.Fatal("OnStarted called after cancellation")
	}
	if strings.Contains(stdout.String(), "City started.") {
		t.Fatalf("stdout = %q, want no started banner after cancellation", stdout.String())
	}
}

func TestCityRuntimeRunShutsDownSessionsOnContextCancel(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Daemon.ShutdownTimeout = "20ms"

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "probe-session", runtime.Config{}); err != nil {
		t.Fatalf("start session: %v", err)
	}

	var stdout bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			cancel()
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: &stdout,
		Stderr: io.Discard,
	})

	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	cr.run(ctx)

	if sp.IsRunning("probe-session") {
		t.Fatal("probe-session still running after runtime cancellation")
	}

	var stopCalls int
	for _, call := range sp.Calls {
		if call.Method == "Stop" && call.Name == "probe-session" {
			stopCalls++
		}
	}
	if stopCalls == 0 {
		t.Fatalf("expected forced stop during shutdown, calls=%+v", sp.Calls)
	}
	if !strings.Contains(stdout.String(), "Stopped agent 'probe-session'") {
		t.Fatalf("stdout = %q, want shutdown stop message", stdout.String())
	}
}

func writeCityRuntimeConfig(t *testing.T, tomlPath, provider string) {
	t.Helper()
	writeCityRuntimeConfigNamed(t, tomlPath, "test-city", provider)
}

func writeCityRuntimeConfigNamed(t *testing.T, tomlPath, name, provider string) {
	t.Helper()
	data := []byte("[workspace]\nname = \"" + name + "\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"" + provider + "\"\n")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func warningsContain(warnings []string, substr string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, substr) {
			return true
		}
	}
	return false
}

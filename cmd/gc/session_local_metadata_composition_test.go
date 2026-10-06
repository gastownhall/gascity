package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// These tests demonstrate (not merely assert) that beads.Store's clone-local
// SetLocalString/GetLocalString compose cleanly with the durable PendingCreateLease
// and SetMarker machinery: same underlying store, same session bead id, disjoint
// storage, no shared production call sites today.

// TestReconcileSessionBeads_SetLocalStringDoesNotAffectPendingCreateLease exercises
// the actual production reconciler (not a unit-tested predicate in isolation) with
// a deliberately adversarial setup: SetLocalString is called with the exact same
// key names (last_woke_at, pending_create_claim) the durable PendingCreateLease
// logic reads out of Bead.Metadata, on the same bead id. If SetLocalString ever
// leaked into, or shadowed, durable metadata, this session would be closed as an
// orphan or lose its lease. It is not and does not: the two APIs write to
// disjoint storage, so the collision is a no-op for production behavior.
func TestReconcileSessionBeads_SetLocalStringDoesNotAffectPendingCreateLease(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	session := env.createSessionBead("s-gc-local", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state":                "creating",
		"manual_session":       "true",
		"pending_create_claim": "true",
	})
	session.CreatedAt = env.clk.Now().Add(-30 * time.Second)

	if err := env.store.SetLocalString(session.ID, "last_woke_at", "clone-local-value"); err != nil {
		t.Fatalf("SetLocalString last_woke_at: %v", err)
	}
	if err := env.store.SetLocalString(session.ID, "pending_create_claim", "false"); err != nil {
		t.Fatalf("SetLocalString pending_create_claim: %v", err)
	}

	woken := env.reconcile([]beads.Bead{session})
	if woken != 0 {
		t.Fatalf("woken = %d, want 0 without desired-state membership", woken)
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("pending-create session was closed as orphan despite colliding SetLocalString writes: %+v", got)
	}
	if got.Metadata["state"] == "orphaned" {
		t.Fatalf("pending-create session was marked orphaned despite colliding SetLocalString writes: %+v", got.Metadata)
	}
	if got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("durable pending_create_claim = %q, want unaffected by same-key SetLocalString write", got.Metadata["pending_create_claim"])
	}

	localVal, err := env.store.GetLocalString(session.ID, "last_woke_at")
	if err != nil {
		t.Fatalf("GetLocalString last_woke_at: %v", err)
	}
	if localVal != "clone-local-value" {
		t.Fatalf("GetLocalString(last_woke_at) = %q, want the clone-local value preserved independently of durable metadata", localVal)
	}
}

// TestReconcileSessionBeads_SetMarkerAndSetLocalStringCoexistOnSameSessionBead
// demonstrates the non-adversarial, realistic composition: session.Store's
// SetMarker front door (the durable path SetMarker/last_woke_at production call
// sites use) and beads.Store.SetLocalString are both called on the same session
// bead id with distinct keys. Both writes succeed independently: the durable
// write surfaces through Get/Bead.Metadata, the local write does not, and is only
// visible through GetLocalString.
func TestReconcileSessionBeads_SetMarkerAndSetLocalStringCoexistOnSameSessionBead(t *testing.T) {
	env := newReconcilerTestEnv()
	session := env.createSessionBead("s-gc-coexist", "worker")

	infoStore := sessionpkg.NewStore(beads.SessionStore{Store: env.store})
	if err := infoStore.SetMarker(session.ID, "throttle_marker", "queued"); err != nil {
		t.Fatalf("SetMarker: %v", err)
	}
	if err := env.store.SetLocalString(session.ID, "synced_at", "2026-07-14T00:00:00Z"); err != nil {
		t.Fatalf("SetLocalString: %v", err)
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["throttle_marker"] != "queued" {
		t.Fatalf("durable throttle_marker = %q, want SetMarker's durable write intact", got.Metadata["throttle_marker"])
	}
	if _, ok := got.Metadata["synced_at"]; ok {
		t.Fatalf("durable Metadata leaked the clone-local synced_at key: %+v", got.Metadata)
	}

	localVal, err := env.store.GetLocalString(session.ID, "synced_at")
	if err != nil {
		t.Fatalf("GetLocalString: %v", err)
	}
	if localVal != "2026-07-14T00:00:00Z" {
		t.Fatalf("GetLocalString(synced_at) = %q, want the clone-local value", localVal)
	}
}

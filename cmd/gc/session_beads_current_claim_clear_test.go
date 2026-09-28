package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// seedClaimingSession creates a session bead already carrying the claim
// back-channel stamp `gc hook --claim` writes, plus the work bead it names,
// assigned to that session and in progress.
func seedClaimingSession(t *testing.T, store beads.Store) (sessionBead, workBead beads.Bead) {
	t.Helper()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	workBead, err = store.Create(beads.Bead{
		Title:    "do the thing",
		Type:     "task",
		Status:   "in_progress",
		Assignee: sessionBead.ID,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "rig/worker"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := store.SetMetadata(sessionBead.ID, beadmeta.CurrentClaimBeadIDMetadataKey, workBead.ID); err != nil {
		t.Fatalf("seed claim stamp: %v", err)
	}
	return sessionBead, workBead
}

func currentClaimStamp(t *testing.T, store beads.Store, sessionID string) string {
	t.Helper()
	got, err := session.NewStore(beads.SessionStore{Store: store}).CurrentClaimBeadID(sessionID)
	if err != nil {
		t.Fatalf("reading current claim on %s: %v", sessionID, err)
	}
	return got
}

// TestReleaseWorkFromClosedSessionBeadClearsTheCurrentClaim is the release half
// of the claim back-channel contract: once a closing session's work is detached,
// the session must stop naming it. A stale stamp is not cosmetic — `gc hook
// current` is what a formula step uses to name the bead it closes, so a session
// left pointing at released work would close a bead it no longer owns.
func TestReleaseWorkFromClosedSessionBeadClearsTheCurrentClaim(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, workBead := seedClaimingSession(t, store)

	var stderr bytes.Buffer
	releaseWorkFromClosedSessionBead(store, sessionBead, &stderr)

	if got := currentClaimStamp(t, store, sessionBead.ID); got != "" {
		t.Fatalf("current claim = %q, want cleared once the work was released", got)
	}
	released, err := store.Get(workBead.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if released.Assignee != "" {
		t.Fatalf("work bead Assignee = %q, want released (the clear must not replace the release)", released.Assignee)
	}
}

// TestUnclaimWorkAssignedToRetiredSessionBeadClearsTheCurrentClaim covers the
// retirement path — `gc session close`, named-session retirement, and the
// stranded-pool-worker repair all funnel through it.
func TestUnclaimWorkAssignedToRetiredSessionBeadClearsTheCurrentClaim(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, workBead := seedClaimingSession(t, store)

	var stderr bytes.Buffer
	unclaimWorkAssignedToRetiredSessionBead("", nil, store, nil, sessionBead, "rig/worker", &stderr)

	if got := currentClaimStamp(t, store, sessionBead.ID); got != "" {
		t.Fatalf("current claim = %q, want cleared on retirement", got)
	}
	released, err := store.Get(workBead.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if released.Assignee != "" {
		t.Fatalf("work bead Assignee = %q, want released", released.Assignee)
	}
}

// TestUnclaimWorkAssignedToRetiredSessionInfoClearsTheCurrentClaim keeps the
// session.Info form byte-identical to the raw form it is proven equivalent to:
// clearing on only one of the pair would make the equivalence a lie and leave the
// reconciler's lane stamping stale claims.
func TestUnclaimWorkAssignedToRetiredSessionInfoClearsTheCurrentClaim(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, _ := seedClaimingSession(t, store)
	info, err := session.NewStore(beads.SessionStore{Store: store}).Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("projecting session info: %v", err)
	}

	var stderr bytes.Buffer
	unclaimWorkAssignedToRetiredSessionInfo("", nil, store, nil, info, "rig/worker", &stderr)

	if got := currentClaimStamp(t, store, sessionBead.ID); got != "" {
		t.Fatalf("current claim = %q, want cleared on retirement", got)
	}
}

// TestReassignWorkAssignedToRetiredSessionBeadClearsTheCurrentClaim covers the
// re-route sibling: the successor stamps its own claim on its next hook tick, but
// the retired session must stop naming the moved bead immediately or it would
// close the successor's work out from under it.
func TestReassignWorkAssignedToRetiredSessionBeadClearsTheCurrentClaim(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, workBead := seedClaimingSession(t, store)

	var stderr bytes.Buffer
	reassignWorkAssignedToRetiredSessionBead("", nil, store, nil, sessionBead, "mc-successor", &stderr)

	if got := currentClaimStamp(t, store, sessionBead.ID); got != "" {
		t.Fatalf("current claim = %q, want cleared on reassignment", got)
	}
	moved, err := store.Get(workBead.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if moved.Assignee != "mc-successor" {
		t.Fatalf("work bead Assignee = %q, want mc-successor", moved.Assignee)
	}
}

// TestCloseBeadClearsTheCurrentClaim proves the clear reaches the whole
// session-close cascade, not just the helper: closeBead is where pool retirement,
// reaping, and the reconciler's close paths converge.
func TestCloseBeadClearsTheCurrentClaim(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, _ := seedClaimingSession(t, store)

	var stderr bytes.Buffer
	if !closeBead(store, sessionBead.ID, "orphaned", time.Now(), &stderr) {
		t.Fatalf("closeBead reported no close; stderr=%s", stderr.String())
	}
	if got := currentClaimStamp(t, store, sessionBead.ID); got != "" {
		t.Fatalf("current claim = %q, want cleared by the session-close cascade", got)
	}
}

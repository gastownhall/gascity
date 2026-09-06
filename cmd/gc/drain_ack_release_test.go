package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// drainAckSessionBead is the retiring worker every case below acknowledges
// drain for.
func drainAckSessionBead() beads.Bead {
	return beads.Bead{
		ID:   "sess-1",
		Type: session.BeadType,
		Metadata: map[string]string{
			"session_name": "worker-1",
			"template":     "worker",
		},
	}
}

// mustCreateDrainAckBead seeds one bead and then puts it in the requested
// status/assignee. The two steps are separate because Create always mints an
// open, unassigned bead — a claim is a later mutation, which is exactly how a
// real claim reaches the store.
func mustCreateDrainAckBead(t *testing.T, store beads.Store, bead beads.Bead, status, assignee string) beads.Bead {
	t.Helper()
	created, err := store.Create(bead)
	if err != nil {
		t.Fatalf("creating %q: %v", bead.Title, err)
	}
	if status == "" && assignee == "" {
		return created
	}
	opts := beads.UpdateOpts{}
	if status != "" {
		opts.Status = &status
	}
	if assignee != "" {
		opts.Assignee = &assignee
	}
	if err := store.Update(created.ID, opts); err != nil {
		t.Fatalf("setting %q to status=%q assignee=%q: %v", bead.Title, status, assignee, err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("reading %q back after seeding: %v", bead.Title, err)
	}
	if got.Status != status || got.Assignee != assignee {
		t.Fatalf("seeded %q as status=%q assignee=%q, want status=%q assignee=%q", bead.Title, got.Status, got.Assignee, status, assignee)
	}
	return created
}

func drainAckBeadStatus(t *testing.T, store beads.Store, id string) (status, assignee string) {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("reading %s back: %v", id, err)
	}
	return got.Status, got.Assignee
}

// TestDrainAckResolvesRuntimeNameToSessionBeadID covers pool sessions whose
// tmux name is metadata on a ci-wisp-* session bead rather than the bead ID.
// The runtime command receives the tmux name, so the release path must resolve
// that name before it can discover the identities holding work.
func TestDrainAckResolvesRuntimeNameToSessionBeadID(t *testing.T) {
	store := beads.NewMemStore()
	const runtimeName = "city--worker-3-pool"

	sessionBead := mustCreateDrainAckBead(t, store, beads.Bead{
		ID:    "ci-wisp-session-3",
		Title: "city/worker-3",
		Type:  session.BeadType,
		Metadata: map[string]string{
			"session_name": runtimeName,
			"template":     "worker",
		},
	}, "", "")
	held := mustCreateDrainAckBead(t, store, beads.Bead{
		Title: "claimed after the session's last executable turn",
		Type:  "task",
	}, "in_progress", runtimeName)

	var stderr bytes.Buffer
	releaseUnexecutedClaimsForSessionStore("", nil, store, nil, runtimeName, drainAckReleaseBudget, &stderr)

	if sessionBead.ID == runtimeName {
		t.Fatalf("fixture session ID %q unexpectedly equals runtime name", sessionBead.ID)
	}
	status, assignee := drainAckBeadStatus(t, store, held.ID)
	if status != "open" || assignee != "" {
		t.Fatalf("held work is status=%q assignee=%q after drain-ack release, want open and unassigned; stderr=%s", status, assignee, stderr.String())
	}
}

// TestDrainAckReleasesWhenSessionBeadIDIsTheRuntimeName is the control for the
// case above: the non-pool shape, where the bead ID and the runtime name are the
// same string, must still release. The wrapper is now the only entry to both
// shapes, and resolution applies an IsSessionBeadOrRepairable acceptance check
// the old direct Get did not.
func TestDrainAckReleasesWhenSessionBeadIDIsTheRuntimeName(t *testing.T) {
	store := beads.NewMemStore()
	const runtimeName = "worker-1"

	mustCreateDrainAckBead(t, store, beads.Bead{
		ID:    runtimeName,
		Title: "city/worker-1",
		Type:  session.BeadType,
		Metadata: map[string]string{
			"session_name": runtimeName,
			"template":     "worker",
		},
	}, "", "")
	held := mustCreateDrainAckBead(t, store, beads.Bead{
		Title: "claimed after the session's last executable turn",
		Type:  "task",
	}, "in_progress", runtimeName)

	var stderr bytes.Buffer
	releaseUnexecutedClaimsForSessionStore("", nil, store, nil, runtimeName, drainAckReleaseBudget, &stderr)

	status, assignee := drainAckBeadStatus(t, store, held.ID)
	if status != "open" || assignee != "" {
		t.Fatalf("held work is status=%q assignee=%q after drain-ack release, want open and unassigned; stderr=%s", status, assignee, stderr.String())
	}
}

// TestDrainAckReleasesUnexecutedClaims pins F-D: drain-ack means "I am done and
// I hold nothing." A session that reaches it still holding an in_progress claim
// never executed that claim, so the claim is given back — across the
// residency-correct leg set, so a claim class routing left in the relocated
// binding is released by the same pass that releases a rig-store one.
//
// A correct worker releases nothing here: the pool-worker contract orders
// drain-ack strictly after `gc bd close`.
func TestDrainAckReleasesUnexecutedClaims(t *testing.T) {
	work := splittest.NewWorkStore(t, "gc")
	binding := splittest.NewClassStore(t, config.BeadClassGraph)

	rigHeld := mustCreateDrainAckBead(t, work, beads.Bead{
		Title: "held in the work store", Type: "task",
	}, "in_progress", "worker-1")
	bindingHeld := mustCreateDrainAckBead(t, binding, beads.Bead{
		Title: "held in the relocated binding", Type: "task",
	}, "in_progress", "worker-1")

	// The binding is a leg of the sweep's own plan now, not an argument: the
	// city SERVES it, so the routes are what say so.
	cityPath := t.TempDir()
	seedSplitRoutes(t, cityPath, binding)

	var stderr bytes.Buffer
	releaseUnexecutedClaimsOnDrainAck(cityPath, nil, work, nil, drainAckSessionBead(), drainAckReleaseBudget, &stderr)

	for _, tc := range []struct {
		name  string
		store beads.Store
		id    string
	}{
		{"work leg", work, rigHeld.ID},
		{"relocated binding leg", binding, bindingHeld.ID},
	} {
		status, assignee := drainAckBeadStatus(t, tc.store, tc.id)
		if status != "open" || assignee != "" {
			t.Errorf("%s: bead %s is status=%q assignee=%q after drain-ack, want open and unassigned; stderr=%s",
				tc.name, tc.id, status, assignee, stderr.String())
		}
	}
}

// TestDrainAckLeavesForeignClaimsAlone is the foreign-claim control: the release
// is compare-and-swap on THIS session's identity, so a bead another live session
// legitimately holds is untouched. A release that swept by status alone would
// destroy a working sibling's claim, and only this direction catches it.
func TestDrainAckLeavesForeignClaimsAlone(t *testing.T) {
	work := splittest.NewWorkStore(t, "gc")
	foreign := mustCreateDrainAckBead(t, work, beads.Bead{
		Title: "held by a different live session", Type: "task",
	}, "in_progress", "worker-2")

	var stderr bytes.Buffer
	releaseUnexecutedClaimsOnDrainAck("", nil, work, nil, drainAckSessionBead(), drainAckReleaseBudget, &stderr)

	status, assignee := drainAckBeadStatus(t, work, foreign.ID)
	if status != "in_progress" || assignee != "worker-2" {
		t.Fatalf("foreign claim %s became status=%q assignee=%q, want it untouched", foreign.ID, status, assignee)
	}
}

// TestDrainAckLeavesPreassignedOpenSiblingsAlone is the continuation control.
// Continuation preassignment writes an assignee onto OPEN siblings so they stay
// with the live context; F-D releases only in_progress rows. Sweeping open rows
// here would undo the preassignment the same claim just made.
func TestDrainAckLeavesPreassignedOpenSiblingsAlone(t *testing.T) {
	work := splittest.NewWorkStore(t, "gc")
	sibling := mustCreateDrainAckBead(t, work, beads.Bead{
		Title: "continuation sibling, preassigned but not started", Type: "task",
	}, "open", "worker-1")

	var stderr bytes.Buffer
	releaseUnexecutedClaimsOnDrainAck("", nil, work, nil, drainAckSessionBead(), drainAckReleaseBudget, &stderr)

	status, assignee := drainAckBeadStatus(t, work, sibling.ID)
	if status != "open" || assignee != "worker-1" {
		t.Fatalf("preassigned open sibling %s became status=%q assignee=%q, want its preassignment kept", sibling.ID, status, assignee)
	}
}

// TestDrainAckLeavesClosedWorkAlone is the already-closed control: a worker that
// finished its bead and then acknowledged drain — the CORRECT sequence the
// pool-worker prompt mandates — must have nothing reopened under it.
func TestDrainAckLeavesClosedWorkAlone(t *testing.T) {
	work := splittest.NewWorkStore(t, "gc")
	done := mustCreateDrainAckBead(t, work, beads.Bead{
		Title: "finished before drain-ack", Type: "task",
	}, "in_progress", "worker-1")
	if err := work.Close(done.ID); err != nil {
		t.Fatalf("closing %s: %v", done.ID, err)
	}

	var stderr bytes.Buffer
	releaseUnexecutedClaimsOnDrainAck("", nil, work, nil, drainAckSessionBead(), drainAckReleaseBudget, &stderr)

	if status, _ := drainAckBeadStatus(t, work, done.ID); status != "closed" {
		t.Fatalf("closed bead %s became status=%q after drain-ack, want it left closed", done.ID, status)
	}
}

// TestDrainAckLeavesSessionAndMailBeadsAlone pins that the sweep stays scoped to
// WORK. A session bead assigned to its own runtime, and a mail message addressed
// to it, both carry the same assignee-matches-identity shape as real work; a
// release would corrupt the session's own lifecycle row (and destroy a mail
// wisp's only route to an inbox, ra-59207).
func TestDrainAckLeavesSessionAndMailBeadsAlone(t *testing.T) {
	work := splittest.NewWorkStore(t, "gc")
	sessionRow := mustCreateDrainAckBead(t, work, beads.Bead{
		Title: "worker-1", Type: session.BeadType, Labels: []string{"gc:session"},
	}, "in_progress", "worker-1")
	message := mustCreateDrainAckBead(t, work, beads.Bead{
		Title: "mail for worker-1", Type: "message",
	}, "in_progress", "worker-1")

	var stderr bytes.Buffer
	releaseUnexecutedClaimsOnDrainAck("", nil, work, nil, drainAckSessionBead(), drainAckReleaseBudget, &stderr)

	for _, id := range []string{sessionRow.ID, message.ID} {
		status, assignee := drainAckBeadStatus(t, work, id)
		if status != "in_progress" || assignee != "worker-1" {
			t.Errorf("non-work bead %s became status=%q assignee=%q, want it untouched", id, status, assignee)
		}
	}
}

// TestDrainAckReleaseHonorsItsBudget pins that the release pass is bounded.
//
// It runs BEFORE the ack — the signal the controller waits on to stop this
// session — and fans out over every work leg × every identity with only
// per-command ceilings underneath. Unbounded, a slow or contended store turns a
// safety net into a stall on the exact path a draining worker needs to be fast.
// Releasing some claims and acking beats releasing all of them late; the
// remainder is the dead-assignee sweep's to collect.
func TestDrainAckReleaseHonorsItsBudget(t *testing.T) {
	work := splittest.NewWorkStore(t, "gc")
	held := mustCreateDrainAckBead(t, work, beads.Bead{
		Title: "held while the budget is already spent", Type: "task",
	}, "in_progress", "worker-1")

	var stderr bytes.Buffer
	// A budget that cannot admit even the first leg.
	releaseUnexecutedClaimsOnDrainAck("", nil, work, nil, drainAckSessionBead(), -time.Second, &stderr)

	status, assignee := drainAckBeadStatus(t, work, held.ID)
	if status != "in_progress" || assignee != "worker-1" {
		t.Fatalf("bead %s became status=%q assignee=%q; a spent budget must stop the pass, not race it", held.ID, status, assignee)
	}
	if !strings.Contains(stderr.String(), "budget") {
		t.Fatalf("stderr = %q, want the exhausted-budget diagnostic; a silent stop is indistinguishable from finding nothing", stderr.String())
	}
}

// TestDrainAckReleaseWithinBudgetStillReleases is the control: the bound must
// not disable the release. It is the same store and the same claim, with a
// budget that comfortably admits the work.
func TestDrainAckReleaseWithinBudgetStillReleases(t *testing.T) {
	work := splittest.NewWorkStore(t, "gc")
	held := mustCreateDrainAckBead(t, work, beads.Bead{
		Title: "held with budget to spare", Type: "task",
	}, "in_progress", "worker-1")

	var stderr bytes.Buffer
	releaseUnexecutedClaimsOnDrainAck("", nil, work, nil, drainAckSessionBead(), time.Minute, &stderr)

	status, assignee := drainAckBeadStatus(t, work, held.ID)
	if status != "open" || assignee != "" {
		t.Fatalf("bead %s is status=%q assignee=%q, want released; stderr=%s", held.ID, status, assignee, stderr.String())
	}
}

// TestDrainAckReleasesBeforeAcknowledging pins the ORDER. Setting the ack first
// lets the controller stop the session between the two writes, stranding exactly
// the claim this release exists to clear.
func TestDrainAckReleasesBeforeAcknowledging(t *testing.T) {
	originalRelease := drainAckReleaseHeldClaims
	originalPoke := drainAckPokeController
	t.Cleanup(func() {
		drainAckReleaseHeldClaims = originalRelease
		drainAckPokeController = originalPoke
	})
	drainAckPokeController = func(string) error { return nil }

	dops := newFakeDrainOps()
	releaseRan := false
	ackedWhenReleaseRan := true
	drainAckReleaseHeldClaims = func(string, string, io.Writer) {
		releaseRan = true
		acked, err := dops.isDrainAcked("worker-1")
		if err != nil {
			t.Errorf("isDrainAcked during release: %v", err)
		}
		ackedWhenReleaseRan = acked
	}

	var stdout, stderr bytes.Buffer
	if code := doRuntimeDrainAck(dops, t.TempDir(), "worker-1", "worker-1", false, &stdout, &stderr); code != 0 {
		t.Fatalf("doRuntimeDrainAck = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !releaseRan {
		t.Fatal("drain-ack did not release held claims")
	}
	if ackedWhenReleaseRan {
		t.Fatal("drain-ack was already set when the release ran; the controller can stop the session in that window and strand the claim")
	}
	if acked, _ := dops.isDrainAcked("worker-1"); !acked {
		t.Fatal("drain-ack was never set")
	}
}

// TestRequestRestartReleasesNothing is the continuation control at the command
// level: `gc runtime request-restart` is context-exhaustion continuation, a
// different verb that deliberately KEEPS its claims for the successor session.
// Only drain-ack means "I hold nothing."
func TestRequestRestartReleasesNothing(t *testing.T) {
	originalRelease := drainAckReleaseHeldClaims
	t.Cleanup(func() { drainAckReleaseHeldClaims = originalRelease })
	released := false
	drainAckReleaseHeldClaims = func(string, string, io.Writer) { released = true }

	var stdout, stderr bytes.Buffer
	doRuntimeRequestRestart(context.Background(), newFakeDrainOps(), runtime.NewFake(), nil, false,
		events.Discard, "worker-1", "worker-1", time.Millisecond, 50*time.Millisecond, &stdout, &stderr)

	if released {
		t.Fatal("gc runtime request-restart released the session's claims; only drain-ack may")
	}
}

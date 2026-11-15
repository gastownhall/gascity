package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// stuckCreatingBead builds a session bead in the given lifecycle state. A
// non-empty startedAgo anchors the create attempt that long ago; claim and
// lastWokeAgo shape the pending-create lease.
func stuckCreatingBead(sessionName, state string, startedAgo time.Duration, claim bool, lastWokeAgo time.Duration) beads.Bead {
	b := newSessionBead(sessionName)
	b.Metadata["state"] = state
	if startedAgo > 0 {
		b.Metadata["pending_create_started_at"] = time.Now().Add(-startedAgo).UTC().Format(time.RFC3339)
	}
	if claim {
		b.Metadata["pending_create_claim"] = "true"
	}
	if lastWokeAgo > 0 {
		b.Metadata["last_woke_at"] = time.Now().Add(-lastWokeAgo).UTC().Format(time.RFC3339)
	}
	return b
}

func createStuckBeads(t *testing.T, store beads.Store, bs ...beads.Bead) []beads.Bead {
	t.Helper()
	out := make([]beads.Bead, 0, len(bs))
	for _, b := range bs {
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("Create %q: %v", b.Metadata["session_name"], err)
		}
		out = append(out, created)
	}
	return out
}

func stuckIDs(block *StatusStuckCreating) []string {
	if block == nil {
		return nil
	}
	ids := make([]string, 0, len(block.Sessions))
	for _, s := range block.Sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

// TestStatusStuckCreatingListsEveryAbandonedCreate is the row-set test: the
// list is built from the raw session rows, so a duplicate or empty
// session_name cannot hide a stuck create the way it drops out of the snapshot
// maps, and the oldest attempt comes first.
func TestStatusStuckCreatingListsEveryAbandonedCreate(t *testing.T) {
	state := newFakeState(t)
	store := beads.NewMemStore()
	state.cityBeadStore = store
	created := createStuckBeads(t, store,
		stuckCreatingBead("sess-creating", "creating", 5*time.Minute, false, 0),                // [0] stuck, newest
		stuckCreatingBead("sess-pending", "start-pending", 30*time.Minute, false, 0),           // [1] stuck
		stuckCreatingBead("sess-wedged", "creating", 2*time.Hour, true, 2*time.Hour),           // [2] stuck: started, then wedged
		stuckCreatingBead("sess-dup", "creating", 20*time.Minute, false, 0),                    // [3] stuck
		stuckCreatingBead("sess-dup", "creating", 25*time.Minute, false, 0),                    // [4] stuck, duplicate name
		stuckCreatingBead("", "creating", 40*time.Minute, false, 0),                            // [5] stuck, empty name
		stuckCreatingBead("sess-fresh", "creating", 10*time.Second, false, 0),                  // [6] still young
		stuckCreatingBead("sess-leased", "creating", 5*time.Minute, true, 0),                   // [7] never started, inside the 10m lease
		stuckCreatingBead("sess-active", string(session.StateActive), 5*time.Minute, false, 0), // [8] not in flight
		stuckCreatingBead("sess-closed", "creating", 5*time.Minute, false, 0),                  // [9] closed
	)
	if err := store.Close(created[9].ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s := &Server{state: state}

	body := s.buildStatusBody(context.Background(), false)

	if body.StuckCreating == nil {
		t.Fatal("StuckCreating = nil, want a measured block")
	}
	if !body.StuckCreating.Complete {
		t.Error("Complete = false, want true on a clean session read")
	}
	want := []string{created[2].ID, created[1].ID, created[5].ID, created[4].ID, created[3].ID, created[0].ID}
	got := stuckIDs(body.StuckCreating)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stuck ids = %v, want %v (oldest attempt first)", got, want)
	}
	first := body.StuckCreating.Sessions[0]
	if first.State != "creating" || first.Template != "myrig/worker" || first.Since == "" {
		t.Errorf("first row = %+v, want state creating, template myrig/worker and a since", first)
	}
	if _, err := time.Parse(time.RFC3339, first.Since); err != nil {
		t.Errorf("since %q is not RFC3339: %v", first.Since, err)
	}
	if body.StuckCreating.Sessions[1].State != "start-pending" {
		t.Errorf("second row state = %q, want start-pending", body.StuckCreating.Sessions[1].State)
	}
}

// TestStatusStuckCreatingSurvivesZeroSessionCounts is the case the block
// exists for: a city whose only session is the zombie has zero active and zero
// suspended sessions, so session_counts_detail is dropped, and the stuck list
// must still be reported.
func TestStatusStuckCreatingSurvivesZeroSessionCounts(t *testing.T) {
	state := newFakeState(t)
	store := beads.NewMemStore()
	state.cityBeadStore = store
	created := createStuckBeads(t, store, stuckCreatingBead("only-zombie", "creating", 5*time.Minute, false, 0))
	s := &Server{state: state}

	body := s.buildStatusBody(context.Background(), false)

	if body.SessionCountsDetail != nil {
		t.Fatalf("SessionCountsDetail = %+v, want nil (nothing active or suspended)", body.SessionCountsDetail)
	}
	if got := stuckIDs(body.StuckCreating); len(got) != 1 || got[0] != created[0].ID {
		t.Fatalf("stuck ids = %v, want [%s]", got, created[0].ID)
	}
}

// TestStatusStuckCreatingMeasuredZeroSerializesEmptyList pins the measured
// zero: the block is present, complete, and its list is [] and never null, so
// a consumer can tell "none stuck" from "not measured".
func TestStatusStuckCreatingMeasuredZeroSerializesEmptyList(t *testing.T) {
	state := newFakeState(t)
	store := beads.NewMemStore()
	state.cityBeadStore = store
	createStuckBeads(t, store, newSessionBead("healthy"))
	s := &Server{state: state}

	body := s.buildStatusBody(context.Background(), false)

	if body.StuckCreating == nil {
		t.Fatal("StuckCreating = nil, want a measured block")
	}
	if !body.StuckCreating.Complete || len(body.StuckCreating.Sessions) != 0 {
		t.Fatalf("StuckCreating = %+v, want complete and empty", *body.StuckCreating)
	}
	raw, err := json.Marshal(body.StuckCreating)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"sessions":[],"complete":true}` {
		t.Fatalf("serialized block = %s, want {\"sessions\":[],\"complete\":true}", raw)
	}
}

// TestStatusStuckCreatingNoBeadStoreIsMeasuredZero: a city with no bead store
// at all has nothing that can be stuck, which is a measurement, not unknown.
func TestStatusStuckCreatingNoBeadStoreIsMeasuredZero(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = nil
	state.stores = map[string]beads.Store{}
	s := &Server{state: state}

	body := s.buildStatusBody(context.Background(), false)

	if body.StuckCreating == nil || !body.StuckCreating.Complete || len(body.StuckCreating.Sessions) != 0 {
		t.Fatalf("StuckCreating = %+v, want a complete, empty block", body.StuckCreating)
	}
}

// TestStatusStuckCreatingPartialReadIsALowerBound: a partial session read
// that still carried rows reports what it found with complete=false, since the
// unreadable bead may be the stuck one.
func TestStatusStuckCreatingPartialReadIsALowerBound(t *testing.T) {
	state := newFakeState(t)
	mem := beads.NewMemStore()
	created := createStuckBeads(t, mem, stuckCreatingBead("sess-stuck", "creating", 5*time.Minute, false, 0))
	rows, err := mem.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	state.cityBeadStore = &failingBeadStore{
		Store:      mem,
		listResult: rows,
		listErr:    &beads.PartialResultError{Op: "bd list", Err: errors.New("one row unreadable")},
	}
	s := &Server{state: state}

	body := s.buildStatusBody(context.Background(), false)

	if body.StuckCreating == nil {
		t.Fatal("StuckCreating = nil, want a lower-bound block (rows were read)")
	}
	if body.StuckCreating.Complete {
		t.Error("Complete = true, want false on a partial read")
	}
	if got := stuckIDs(body.StuckCreating); len(got) != 1 || got[0] != created[0].ID {
		t.Fatalf("stuck ids = %v, want [%s]", got, created[0].ID)
	}
}

// TestStatusStuckCreatingAbsentWhenNotMeasured: every way the session read can
// fail to happen leaves the block absent, which means unknown, never zero.
func TestStatusStuckCreatingAbsentWhenNotMeasured(t *testing.T) {
	t.Run("read error", func(t *testing.T) {
		state := newFakeState(t)
		state.cityBeadStore = &failingBeadStore{Store: beads.NewMemStore(), listErr: errors.New("dolt unavailable")}
		body := (&Server{state: state}).buildStatusBody(context.Background(), false)
		if body.StuckCreating != nil {
			t.Fatalf("StuckCreating = %+v, want nil after a failed session read", body.StuckCreating)
		}
	})
	t.Run("session class unavailable", func(t *testing.T) {
		state := sessionClassUnavailableState{newFakeState(t)}
		body := (&Server{state: state}).buildStatusBody(context.Background(), false)
		if body.StuckCreating != nil {
			t.Fatalf("StuckCreating = %+v, want nil when the sessions binding did not resolve", body.StuckCreating)
		}
	})
	t.Run("lite", func(t *testing.T) {
		state := newFakeState(t)
		store := beads.NewMemStore()
		state.cityBeadStore = store
		createStuckBeads(t, store, stuckCreatingBead("zombie", "creating", 5*time.Minute, false, 0))
		body := (&Server{state: state}).buildStatusBody(context.Background(), true)
		if body.StuckCreating != nil {
			t.Fatalf("StuckCreating = %+v, want nil in lite mode", body.StuckCreating)
		}
	})
	t.Run("suspended city", func(t *testing.T) {
		state := newFakeState(t)
		store := beads.NewMemStore()
		state.cityBeadStore = store
		createStuckBeads(t, store, stuckCreatingBead("zombie", "creating", 5*time.Minute, false, 0))
		suspended := true
		if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, state.cityPath, &suspended); err != nil {
			t.Fatal(err)
		}
		body := (&Server{state: state}).buildStatusBody(context.Background(), false)
		if body.StuckCreating != nil {
			t.Fatalf("StuckCreating = %+v, want nil: a suspended city reads no store", body.StuckCreating)
		}
	})
}

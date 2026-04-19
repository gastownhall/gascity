package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// humaHandleConvoyCreate applies per-item Update calls after creating
// the convoy bead. If the nth Update fails, earlier successful updates
// must be rolled back so items don't end up pointing at the deleted
// convoy ID. Round 2 (R2-9) extended the Add/Remove rollback to Create.
func TestConvoyCreateRollsBackOnLinkFailure(t *testing.T) {
	fs := newMutatorState(t)
	store := fs.stores["myrig"]

	// Seed two items with the SAME pre-existing parent so rollback has
	// something concrete to restore.
	oldParent := "legacy-parent"
	itemA, err := store.Create(beads.Bead{Type: "task", Title: "a", ParentID: oldParent})
	if err != nil {
		t.Fatalf("seed a: %v", err)
	}
	itemB, err := store.Create(beads.Bead{Type: "task", Title: "b", ParentID: oldParent})
	if err != nil {
		t.Fatalf("seed b: %v", err)
	}

	boom := errors.New("simulated update failure")
	// Wrap the store so Update fails specifically on itemB.
	fs.stores["myrig"] = &failingBeadStore{
		Store:        store,
		updateFailAt: map[string]error{itemB.ID: boom},
	}

	h := newTestCityHandler(t, fs.fakeState)
	body := []byte(`{"rig":"myrig","title":"test convoy","items":["` + itemA.ID + `","` + itemB.ID + `"]}`)
	req := newPostRequest(cityURL(fs.fakeState, "/convoys"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code < 500 {
		t.Fatalf("status = %d, want 5xx on link failure (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to link") {
		t.Errorf("body = %q, want 'failed to link' mention", rec.Body.String())
	}

	// itemA was re-parented before the failure; rollback must restore
	// its original parent, NOT leave it pointing at the deleted convoy.
	restored, err := store.Get(itemA.ID)
	if err != nil {
		t.Fatalf("get itemA: %v", err)
	}
	if restored.ParentID != oldParent {
		t.Errorf("itemA.ParentID = %q, want %q (rollback should restore original parent)", restored.ParentID, oldParent)
	}

	// The convoy bead itself must not survive as an orphan.
	ids, _ := store.List(beads.ListQuery{Type: "convoy", IncludeClosed: true})
	for _, id := range ids {
		if id.Title == "test convoy" {
			t.Errorf("convoy bead survived rollback: %+v", id)
		}
	}
	_ = json.RawMessage{}
}

// newMutatorState wraps newFakeState with the StateMutator interface so
// the handler can dispatch POST /convoys. The existing test helpers use
// fakeMutatorState for this.
func newMutatorState(t *testing.T) *fakeMutatorState {
	t.Helper()
	return newFakeMutatorState(t)
}

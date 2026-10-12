package api

import (
	"bytes"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// beadUpdate sends body to bead id's update, by POST /update or its PATCH
// alias.
func beadUpdate(t *testing.T, h http.Handler, state *fakeState, method, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	url := cityURL(state, "/bead/") + id
	if method == "POST" {
		url += "/update"
	}
	req := httptest.NewRequest(method, url, bytes.NewBufferString(body))
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestBeadUpdateRefusesASessionsDispositionKeys (mc-ojhnu): a generic update
// that sets a session row's disposition key is refused with 422, names the
// session verbs, and writes nothing, not even the body's other fields; the
// same key on a work bead, and a session row's other keys, pass.
func TestBeadUpdateRefusesASessionsDispositionKeys(t *testing.T) {
	for _, method := range []string{"POST", "PATCH"} {
		for _, key := range []string{"held_until", "sleep_intent", "wait_hold", "quarantined_until", "suspended_at"} {
			state := newFakeState(t)
			store := state.stores["myrig"]
			h := newTestCityHandler(t, state)
			row, err := store.Create(beads.Bead{Title: "s", Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"state": "asleep"}})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := store.Get(row.ID)
			rec := beadUpdate(t, h, state, method, row.ID, `{"title":"renamed","metadata":{"sleep_reason":"idle","`+key+`":""}}`)
			got, _ := store.Get(row.ID)
			switch {
			case rec.Code != http.StatusUnprocessableEntity:
				t.Errorf("%s %s on a session: status %d (%s), want 422", method, key, rec.Code, rec.Body.String())
			case !strings.Contains(rec.Body.String(), key) || !strings.Contains(rec.Body.String(), "/suspend"):
				t.Errorf("%s %s on a session: body %s, want the key and the session verbs named", method, key, rec.Body.String())
			case got.Title != before.Title || !maps.Equal(got.Metadata, before.Metadata):
				t.Errorf("%s %s on a session: row %q %v, want nothing written", method, key, got.Title, got.Metadata)
			}

			work, err := store.Create(beads.Bead{Title: "w", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			if rec := beadUpdate(t, h, state, method, work.ID, `{"metadata":{"`+key+`":"v"}}`); rec.Code != http.StatusOK {
				t.Errorf("%s %s on a work bead: status %d (%s), want 200", method, key, rec.Code, rec.Body.String())
			} else if got, _ := store.Get(work.ID); got.Metadata[key] != "v" {
				t.Errorf("%s %s on a work bead: metadata %v, want it written", method, key, got.Metadata)
			}
		}
	}
}

// TestBeadUpdatePassesASessionsOtherKeys: a session row's keys outside its
// disposition still update through the generic route.
func TestBeadUpdatePassesASessionsOtherKeys(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)
	row, err := store.Create(beads.Bead{Title: "s", Type: session.BeadType, Labels: []string{session.LabelSession}})
	if err != nil {
		t.Fatal(err)
	}
	if rec := beadUpdate(t, h, state, "PATCH", row.ID, `{"metadata":{"pin_awake":"true","sleep_reason":"idle"}}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got, _ := store.Get(row.ID); got.Metadata["pin_awake"] != "true" || got.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("metadata %v, want both keys written", got.Metadata)
	}
}

// TestBeadUpdateRefusesARepairableOrRetypedSession: a crash-damaged session
// row (no type, the session label) and an update that types a bead as a
// session are session rows too; a session label alone on a typed work bead
// is not.
func TestBeadUpdateRefusesARepairableOrRetypedSession(t *testing.T) {
	meta := beadUpdateBody{Metadata: map[string]string{"wait_hold": "true"}}
	if err := refuseSessionDispositionPatch("gc-1", beads.Bead{Labels: []string{session.LabelSession}}, meta); err == nil {
		t.Error("a repairable session row: refused nothing")
	}
	if err := refuseSessionDispositionPatch("w-1", beads.Bead{Type: "task", Labels: []string{session.LabelSession}}, meta); err != nil {
		t.Errorf("a labeled work bead: %v, want it to pass", err)
	}
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)
	work, err := store.Create(beads.Bead{Title: "w", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := beadUpdate(t, h, state, "PATCH", work.ID, `{"type":"`+session.BeadType+`","metadata":{"held_until":"x"}}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a retype to session: status %d, want 422", rec.Code)
	}
}

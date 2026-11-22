package api

import (
	"bytes"
	"encoding/json"
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
	return beadRequest(t, h, method, url, body)
}

func beadRequest(t *testing.T, h http.Handler, method, url, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, bytes.NewBufferString(body))
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sessionRow is a typed session row on store.
func sessionRow(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	row, err := store.Create(beads.Bead{Title: "s", Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"state": "asleep"}})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// problemLocations are a 422 body's errors[] locations.
func problemLocations(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var p struct {
		Errors []struct {
			Location string `json:"location"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem body %s: %v", rec.Body.String(), err)
	}
	var out []string
	for _, e := range p.Errors {
		out = append(out, e.Location)
	}
	return out
}

// TestBeadUpdateRefusesAnInvalidMetadataKey: a key a bd-backed store would
// split ("wait_hold=true" lands as wait_hold) is refused with 422 and an
// errors[] entry before anything else, on any bead, and writes nothing.
func TestBeadUpdateRefusesAnInvalidMetadataKey(t *testing.T) {
	for _, onSession := range []bool{true, false} {
		state := newFakeState(t)
		store := state.stores["myrig"]
		h := newTestCityHandler(t, state)
		var row beads.Bead
		if onSession {
			row = sessionRow(t, store)
		} else {
			var err error
			if row, err = store.Create(beads.Bead{Title: "w", Type: "task"}); err != nil {
				t.Fatal(err)
			}
		}
		before, _ := store.Get(row.ID)
		rec := beadUpdate(t, h, state, "PATCH", row.ID, `{"metadata":{"wait_hold=true":"","note":"x"}}`)
		got, _ := store.Get(row.ID)
		switch {
		case rec.Code != http.StatusUnprocessableEntity:
			t.Errorf("session %v: status %d (%s), want 422", onSession, rec.Code, rec.Body.String())
		case strings.Join(problemLocations(t, rec), ",") != "body.metadata.wait_hold=true":
			t.Errorf("session %v: errors %v, want the key's location", onSession, problemLocations(t, rec))
		case !maps.Equal(got.Metadata, before.Metadata):
			t.Errorf("session %v: metadata %v, want nothing written", onSession, got.Metadata)
		}
	}
}

// TestBeadUpdateRefusalMaterializesNothing (M15, mc-dkblm): a refused
// update, close, create or assign that names a configured named session as
// its assignee does not materialize it: the guard runs before the assignee
// is normalized.
func TestBeadUpdateRefusalMaterializesNothing(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	store := state.stores["myrig"]
	h := newTestCityHandlerWith(t, state, New(state))
	row := sessionRow(t, store)
	count := func() int {
		all, _ := state.cityBeadStore.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
		n := 0
		for _, b := range all {
			if session.IsSessionBeadOrRepairable(b) {
				n++
			}
		}
		return n
	}
	before := count()
	if rec := beadUpdate(t, h, state, "PATCH", row.ID, `{"assignee":"myrig/worker","metadata":{"held_until":"x"}}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d (%s), want 422", rec.Code, rec.Body.String())
	}
	if rec := beadRequest(t, h, "POST", cityURL(state, "/beads"), `{"title":"s","type":"`+session.BeadType+`","assignee":"myrig/worker"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create: status %d (%s), want 422", rec.Code, rec.Body.String())
	}
	if rec := beadRequest(t, h, "POST", cityURL(state, "/bead/")+row.ID+"/assign", `{"assignee":"myrig/worker"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("assign: status %d (%s), want 422", rec.Code, rec.Body.String())
	}
	if rec := beadUpdate(t, h, state, "PATCH", row.ID, `{"status":"closed","assignee":"myrig/worker"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("close: status %d (%s), want 422", rec.Code, rec.Body.String())
	}
	if after := count(); after != before {
		t.Fatalf("session rows %d -> %d: a refused request materialized one", before, after)
	}
}

// TestBeadWriteRefusesACSVLabel (mc-ojhnu rounds 2 and 3): bd reads labels
// as a CSV record, so a label holding a comma, a double quote, a newline or a
// trailing CR would add or strip the session label ("note,gc:session",
// '"gc:session"', "gc:session\n"). Create and update
// refuse one on either side with 422 at the label's index; nothing is
// written.
func TestBeadWriteRefusesACSVLabel(t *testing.T) {
	for _, c := range []struct {
		name, method, path, body string
		onSession                bool
		where                    string
	}{
		{"an added label with a comma", "PATCH", "", `{"labels":["ok","note,gc:session"]}`, false, "body.labels[1]"},
		{"an added label with a quote", "PATCH", "", `{"labels":["\"gc:session\""]}`, false, "body.labels[0]"},
		{"a removed label with a comma", "PATCH", "", `{"remove_labels":["x,gc:session"]}`, true, "body.remove_labels[0]"},
		{"a removed label with a quote", "PATCH", "", `{"remove_labels":["\"gc:session\""]}`, true, "body.remove_labels[0]"},
		{"a created label with a comma", "POST", "/beads", `{"title":"w","labels":["note,gc:session"]}`, false, "body.labels[0]"},
		{"an added label with a trailing newline", "PATCH", "", `{"labels":["gc:session\n"]}`, false, "body.labels[0]"},
		{"an added label with a leading newline", "PATCH", "", `{"labels":["\ngc:session"]}`, false, "body.labels[0]"},
		{"an added label with a trailing CR", "PATCH", "", `{"labels":["gc:session\r"]}`, false, "body.labels[0]"},
		{"a removed label with a trailing newline", "PATCH", "", `{"remove_labels":["gc:session\n"]}`, true, "body.remove_labels[0]"},
		{"a removed label with a leading newline", "PATCH", "", `{"remove_labels":["\ngc:session"]}`, true, "body.remove_labels[0]"},
		{"a removed label with a trailing CR", "PATCH", "", `{"remove_labels":["gc:session\r"]}`, true, "body.remove_labels[0]"},
		{"a created label with a trailing newline", "POST", "/beads", `{"title":"w","labels":["gc:session\n"]}`, false, "body.labels[0]"},
		{"a created label with a leading newline", "POST", "/beads", `{"title":"w","labels":["\ngc:session"]}`, false, "body.labels[0]"},
		{"a created label with a trailing CR", "POST", "/beads", `{"title":"w","labels":["gc:session\r"]}`, false, "body.labels[0]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := newFakeState(t)
			store := state.stores["myrig"]
			h := newTestCityHandler(t, state)
			var row beads.Bead
			if c.onSession {
				row = sessionRow(t, store)
			} else {
				var err error
				if row, err = store.Create(beads.Bead{Title: "w", Type: "task"}); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			want, _ := store.Get(row.ID)
			url := cityURL(state, "/bead/") + row.ID
			if c.path != "" {
				url = cityURL(state, c.path)
			}
			rec := beadRequest(t, h, c.method, url, c.body)
			after, _ := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			got, _ := store.Get(row.ID)
			switch {
			case rec.Code != http.StatusUnprocessableEntity:
				t.Fatalf("status %d (%s), want 422", rec.Code, rec.Body.String())
			case strings.Join(problemLocations(t, rec), ",") != c.where:
				t.Fatalf("errors %v, want %s", problemLocations(t, rec), c.where)
			case len(after) != len(before) || strings.Join(got.Labels, ",") != strings.Join(want.Labels, ","):
				t.Fatalf("labels %v (beads %d -> %d), want nothing written", got.Labels, len(before), len(after))
			}
		})
	}
}

// withoutSessionWriteHooks removes the test-mode session write hooks for
// the test (production installs none), so a row can carry a key written
// past the API.
func withoutSessionWriteHooks(t *testing.T) {
	t.Helper()
	beads.SetSessionWriteHook(nil)
	beads.SetSessionKeyHook(nil)
	t.Cleanup(func() {
		session.GuardSessionKeys(func(msg string) { panic(msg) })
		session.GuardDispositionWrites(func(msg string) { panic(msg) })
	})
}

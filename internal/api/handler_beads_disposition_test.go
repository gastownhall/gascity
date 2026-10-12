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

// TestBeadUpdateRefusesASessionsDispositionKeys (mc-ojhnu, CONTRACT v5.9p):
// a generic update that sets a key a session row's disposition decodes (the
// five, state, sleep_reason) is refused with 422, names the key, the session
// verbs and an errors[] entry, and writes nothing; the same key on a work
// bead passes.
func TestBeadUpdateRefusesASessionsDispositionKeys(t *testing.T) {
	for _, method := range []string{"POST", "PATCH"} {
		for _, key := range []string{"held_until", "sleep_intent", "wait_hold", "quarantined_until", "suspended_at", "state", "sleep_reason"} {
			state := newFakeState(t)
			store := state.stores["myrig"]
			h := newTestCityHandler(t, state)
			row := sessionRow(t, store)
			before, _ := store.Get(row.ID)
			rec := beadUpdate(t, h, state, method, row.ID, `{"title":"renamed","metadata":{"pin_awake":"true","`+key+`":""}}`)
			got, _ := store.Get(row.ID)
			switch {
			case rec.Code != http.StatusUnprocessableEntity:
				t.Errorf("%s %s on a session: status %d (%s), want 422", method, key, rec.Code, rec.Body.String())
			case !strings.Contains(rec.Body.String(), key) || !strings.Contains(rec.Body.String(), "/v0/city/{cityName}/session/{id}/suspend"):
				t.Errorf("%s %s on a session: body %s, want the key and the session verbs named", method, key, rec.Body.String())
			case strings.Join(problemLocations(t, rec), ",") != "body.metadata."+key:
				t.Errorf("%s %s on a session: errors %v, want the key's location", method, key, problemLocations(t, rec))
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

// TestBeadUpdatePassesASessionsOtherKeys: a session row's keys its
// disposition does not decode still update through the generic route.
func TestBeadUpdatePassesASessionsOtherKeys(t *testing.T) {
	state := newFakeState(t)
	store := state.stores["myrig"]
	h := newTestCityHandler(t, state)
	row := sessionRow(t, store)
	if rec := beadUpdate(t, h, state, "PATCH", row.ID, `{"metadata":{"pin_awake":"true","detached_at":"x"}}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if got, _ := store.Get(row.ID); got.Metadata["pin_awake"] != "true" || got.Metadata["detached_at"] != "x" {
		t.Fatalf("metadata %v, want both keys written", got.Metadata)
	}
}

// TestBeadUpdateNeverChangesWhetherARowIsASession (CONTRACT v5.9p): a
// session row retyped away, a bead retyped to a session, a bead given the
// session label (with or without a type), are refused with nothing written;
// a work bead retyped to another work type, its same-named key included,
// passes.
func TestBeadUpdateNeverChangesWhetherARowIsASession(t *testing.T) {
	for _, c := range []struct {
		name, body string
		session    bool
		want       int
	}{
		{"a session row retyped away", `{"type":"task"}`, true, http.StatusUnprocessableEntity},
		{"a session row retyped away with a key", `{"type":"task","metadata":{"wait_hold":"true"}}`, true, http.StatusUnprocessableEntity},
		{"a bead retyped to a session", `{"type":"` + session.BeadType + `"}`, false, http.StatusUnprocessableEntity},
		{"a bead given the session label", `{"labels":["` + session.LabelSession + `"]}`, false, http.StatusUnprocessableEntity},
		{"a bead untyped and labeled", `{"type":"","labels":["` + session.LabelSession + `"],"metadata":{"wait_hold":"true"}}`, false, http.StatusUnprocessableEntity},
		{"a work bead retyped to another work type with a key", `{"type":"bug","metadata":{"wait_hold":"true"}}`, false, http.StatusOK},
		{"a session row's label removed, its type kept", `{"remove_labels":["` + session.LabelSession + `"]}`, true, http.StatusUnprocessableEntity},
		{"a session row's other label added", `{"labels":["note"]}`, true, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := newFakeState(t)
			store := state.stores["myrig"]
			h := newTestCityHandler(t, state)
			var row beads.Bead
			if c.session {
				row = sessionRow(t, store)
			} else {
				var err error
				if row, err = store.Create(beads.Bead{Title: "w", Type: "task"}); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := store.Get(row.ID)
			rec := beadUpdate(t, h, state, "PATCH", row.ID, c.body)
			got, _ := store.Get(row.ID)
			switch {
			case rec.Code != c.want:
				t.Fatalf("status %d (%s), want %d", rec.Code, rec.Body.String(), c.want)
			case c.want != http.StatusOK && (got.Type != before.Type || len(got.Labels) != len(before.Labels) || !maps.Equal(got.Metadata, before.Metadata)):
				t.Fatalf("row %q %v %v, want nothing written", got.Type, got.Labels, got.Metadata)
			}
		})
	}
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

// TestBeadUpdateRefusalMaterializesNothing (M15): a refused update that
// names a configured named session as its assignee does not materialize it:
// the guard runs before the assignee is normalized.
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
	if after := count(); after != before {
		t.Fatalf("session rows %d -> %d: a refused request materialized one", before, after)
	}
}

// TestBeadCreateNeverMakesASessionRow (CONTRACT v5.9p): POST /beads refuses a
// session row, typed or labeled, its hand-written disposition keys
// included, and an invalid metadata key; nothing is created. A work bead
// with the same keys is created.
func TestBeadCreateNeverMakesASessionRow(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"a typed session row", `{"title":"s","type":"` + session.BeadType + `"}`, http.StatusUnprocessableEntity},
		{"a session row with disposition keys", `{"title":"s","type":"` + session.BeadType + `","labels":["` + session.LabelSession + `"],"metadata":{"wait_hold":"true","sleep_intent":"bogus"}}`, http.StatusUnprocessableEntity},
		{"an untyped labeled row", `{"title":"s","type":"","labels":["` + session.LabelSession + `"]}`, http.StatusUnprocessableEntity},
		{"an invalid metadata key", `{"title":"w","metadata":{"wait_hold=true":""}}`, http.StatusUnprocessableEntity},
		{"a work bead with the same keys", `{"title":"w","type":"task","metadata":{"wait_hold":"true","state":"x"}}`, http.StatusCreated},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := newFakeState(t)
			store := state.stores["myrig"]
			h := newTestCityHandler(t, state)
			before, _ := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			rec := beadRequest(t, h, "POST", cityURL(state, "/beads"), c.body)
			after, _ := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			switch {
			case rec.Code != c.want:
				t.Fatalf("status %d (%s), want %d", rec.Code, rec.Body.String(), c.want)
			case c.want != http.StatusCreated && len(after) != len(before):
				t.Fatalf("beads %d -> %d, want nothing created", len(before), len(after))
			}
		})
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

// TestBeadUpdateSessionStatusRefusals (round 2: A06, A10, A18): a
// crash-damaged session row (no type, the session label) retyped to a work
// type stops being a session, and a typed work bead carrying the label
// retyped to no type becomes one; both are refused. Each refusal points at
// the field that changed the row: the type, or the label added or removed.
func TestBeadUpdateSessionStatusRefusals(t *testing.T) {
	empty := ""
	for _, c := range []struct {
		name   string
		labels []string
		typ    *string // set after the create (MemStore types a create)
		body   string
		where  string
	}{
		{"a crash-damaged session row retyped to task", []string{session.LabelSession}, &empty, `{"type":"task"}`, "body.type"},
		{"a labeled work bead retyped to no type", []string{session.LabelSession}, nil, `{"type":""}`, "body.type"},
		{"a work bead given the session label", nil, nil, `{"labels":["note","` + session.LabelSession + `"]}`, "body.labels[1]"},
		{"a session row's label removed", nil, nil, `{"remove_labels":["` + session.LabelSession + `"]}`, "body.remove_labels[0]"},
		{"a bead retyped to a session", nil, nil, `{"type":"` + session.BeadType + `"}`, "body.type"},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := newFakeState(t)
			store := state.stores["myrig"]
			h := newTestCityHandler(t, state)
			var row beads.Bead
			var err error
			if c.name == "a session row's label removed" {
				row = sessionRow(t, store)
			} else if row, err = store.Create(beads.Bead{Title: "w", Type: "task", Labels: c.labels}); err != nil {
				t.Fatal(err)
			}
			if c.typ != nil {
				if err := store.Update(row.ID, beads.UpdateOpts{Type: c.typ}); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := store.Get(row.ID)
			rec := beadUpdate(t, h, state, "PATCH", row.ID, c.body)
			got, _ := store.Get(row.ID)
			switch {
			case rec.Code != http.StatusUnprocessableEntity:
				t.Fatalf("status %d (%s), want 422", rec.Code, rec.Body.String())
			case strings.Join(problemLocations(t, rec), ",") != c.where:
				t.Fatalf("errors %v, want %s", problemLocations(t, rec), c.where)
			case got.Type != before.Type || strings.Join(got.Labels, ",") != strings.Join(before.Labels, ","):
				t.Fatalf("row %q %v, want nothing written", got.Type, got.Labels)
			}
		})
	}
}

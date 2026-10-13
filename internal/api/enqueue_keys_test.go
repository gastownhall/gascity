package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/session"
)

// These tests pin which reconcile key each API trigger enqueues. Under the
// legacy reconciler every Enqueue is one Poke, so pokeCount must stay at the
// pre-key values; the keys are what a keyed reconciler will route on.

// waitForEnqueuedKey polls until key has been enqueued: async creates
// enqueue from a background goroutine, possibly after the success event.
func waitForEnqueuedKey(t *testing.T, fs *fakeState, key reconcilekey.Key) {
	t.Helper()
	waitFor(t, 5*time.Second, "enqueue of "+key.Encode(), func() bool {
		for _, k := range fs.enqueuedKeys() {
			if k == key {
				return true
			}
		}
		return false
	})
}

func assertEnqueued(t *testing.T, fs *fakeState, want ...reconcilekey.Key) {
	t.Helper()
	if got := fs.enqueuedKeys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("enqueued keys = %v, want %v", got, want)
	}
}

func TestAPISessionCreateAsyncEnqueuesSessionKey(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker","alias":"sky","async":true}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	waitForEnqueuedKey(t, fs, reconcilekey.Session(success.Session.ID))
	assertEnqueued(t, fs, reconcilekey.Session(success.Session.ID))
	if got := fs.enqueueCalls(); got != 1 {
		t.Fatalf("pokeCount = %d, want 1", got)
	}
}

func TestAPILegacySessionCreateEnqueuesSessionKey(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	req := newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	waitForEnqueuedKey(t, fs, reconcilekey.Session(resp.ID))
	assertEnqueued(t, fs, reconcilekey.Session(resp.ID))
}

func TestAPINamedSessionMaterializeEnqueuesSessionKey(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	id := phase0MaterializeCityScopedNamedWorker(t, srv, fs)
	assertEnqueued(t, fs, reconcilekey.Session(id))
	if got := fs.enqueueCalls(); got != 1 {
		t.Fatalf("pokeCount = %d, want 1", got)
	}
}

func TestAPIPermissionModeChangeEnqueuesSessionKey(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker","options":{"effort":"high"}}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	accepted := decodeAsyncAccepted(t, w.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	// The create enqueues from its background goroutine; let that land
	// before snapshotting, or it could be counted as the mode change's.
	waitForEnqueuedKey(t, fs, reconcilekey.Session(success.Session.ID))
	suspendSessionForPermissionModeTest(t, fs, success.Session.ID)
	before := len(fs.enqueuedKeys())

	req = newPostRequest(cityURL(fs, "/session/"+success.Session.ID+"/permission-mode"), strings.NewReader(`{"permission_mode":"plan"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("permission-mode status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	got := fs.enqueuedKeys()[before:]
	want := []reconcilekey.Key{reconcilekey.Session(success.Session.ID)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("permission-mode enqueued keys = %v, want %v", got, want)
	}
}

// TestAPISlingNotifierEnqueuesControllerAndControlDispatchKeys pins the OQ-6
// fix: the API sling path used to collapse PokeControlDispatch into a
// generic poke, so it never ran the targeted control-dispatcher reconcile
// the CLI requests. It now enqueues the control-dispatch key like the CLI.
func TestAPISlingNotifierEnqueuesControllerAndControlDispatchKeys(t *testing.T) {
	fs := newFakeState(t)
	n := &apiNotifier{state: fs}

	n.PokeController(fs.cityPath)
	n.PokeControlDispatch(fs.cityPath)

	assertEnqueued(t, fs, reconcilekey.Allocator(), reconcilekey.ControlDispatch())
}

// writeFailStore fails every bead write, standing in for a store whose
// write errors after the handler's gates passed.
type writeFailStore struct {
	beads.Store
}

var errWriteRefused = errors.New("write refused")

func (writeFailStore) Create(beads.Bead) (beads.Bead, error)    { return beads.Bead{}, errWriteRefused }
func (writeFailStore) Close(string) error                       { return errWriteRefused }
func (writeFailStore) Reopen(string) error                      { return errWriteRefused }
func (writeFailStore) Update(string, beads.UpdateOpts) error    { return errWriteRefused }
func (writeFailStore) SetMetadata(string, string, string) error { return errWriteRefused }

// beadWriteCase is one generic bead write route. closed says the target
// bead must start closed (reopen); session says the case has a session-row
// refusal to pin.
type beadWriteCase struct {
	name, method, path, body string
	closed, session          bool
}

var beadWriteCases = []beadWriteCase{
	{name: "create", method: http.MethodPost, body: `{"title":"t","rig":"myrig"}`, session: true},
	{name: "close", method: http.MethodPost, path: "/close", body: `{"reason":"done"}`, session: true},
	{name: "reopen", method: http.MethodPost, path: "/reopen", body: `{}`, closed: true, session: true},
	{name: "assign", method: http.MethodPost, path: "/assign", body: `{"assignee":"alice"}`},
	{name: "update", method: http.MethodPost, path: "/update", body: `{"title":"renamed"}`, session: true},
	{name: "patch", method: http.MethodPatch, body: `{"title":"renamed"}`, session: true},
	{name: "patch status=closed", method: http.MethodPatch, body: `{"status":"closed"}`, session: true},
	{name: "delete", method: http.MethodDelete, session: true},
}

// url is c's route for bead id; create posts to the collection.
func (c beadWriteCase) url(state *fakeState, id string) string {
	if c.name == "create" {
		return cityURL(state, "/beads")
	}
	return cityURL(state, "/bead/") + id + c.path
}

// seedBeadWriteTarget creates c's target bead on store: a work bead, or a
// session row when sessionRowTarget, closed when c needs it closed.
func seedBeadWriteTarget(t *testing.T, store beads.Store, c beadWriteCase, sessionRowTarget bool) beads.Bead {
	t.Helper()
	var b beads.Bead
	if sessionRowTarget {
		b = sessionRow(t, store)
	} else {
		var err error
		if b, err = store.Create(beads.Bead{Title: "w", Type: "task"}); err != nil {
			t.Fatal(err)
		}
	}
	if c.closed {
		if err := store.Close(b.ID); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// TestAPIBeadWritesEnqueueAllocatorAndControlDispatch (coherence slice 0b):
// a generic bead write lands through the controller's CachingStore as a
// cache-authored bead event, which never wakes the reconciler, so each
// successful write enqueues the allocator and control-dispatch keys once.
func TestAPIBeadWritesEnqueueAllocatorAndControlDispatch(t *testing.T) {
	for _, c := range beadWriteCases {
		t.Run(c.name, func(t *testing.T) {
			state := newFakeState(t)
			h := newTestCityHandler(t, state)
			b := seedBeadWriteTarget(t, state.stores["myrig"], c, false)

			rec := beadRequest(t, h, c.method, c.url(state, b.ID), c.body)
			if rec.Code >= 300 {
				t.Fatalf("%s: status %d, want success; body: %s", c.name, rec.Code, rec.Body.String())
			}
			assertEnqueued(t, state, reconcilekey.Allocator(), reconcilekey.ControlDispatch())
			if got := state.enqueueCalls(); got != 1 {
				t.Fatalf("%s: enqueue calls = %d, want 1", c.name, got)
			}
		})
	}
}

// TestAPIBeadWriteRefusedOnSessionRowEnqueuesNothing: a write the session-row
// guards refuse wrote nothing, so it wakes nothing.
func TestAPIBeadWriteRefusedOnSessionRowEnqueuesNothing(t *testing.T) {
	for _, c := range beadWriteCases {
		if !c.session {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			state := newFakeState(t)
			h := newTestCityHandler(t, state)
			b := seedBeadWriteTarget(t, state.stores["myrig"], c, true)
			body := c.body
			switch c.name {
			case "create":
				body = `{"title":"s","rig":"myrig","type":"` + session.BeadType + `"}`
			case "update", "patch":
				body = `{"metadata":{"wait_hold":"true"}}`
			}

			rec := beadRequest(t, h, c.method, c.url(state, b.ID), body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s on a session row: status %d, want 422; body: %s", c.name, rec.Code, rec.Body.String())
			}
			assertEnqueued(t, state)
			if got := state.enqueueCalls(); got != 0 {
				t.Fatalf("%s: enqueue calls = %d, want 0", c.name, got)
			}
		})
	}
}

// TestAPIBeadWriteStoreErrorEnqueuesNothing: a write the store fails wakes
// nothing.
func TestAPIBeadWriteStoreErrorEnqueuesNothing(t *testing.T) {
	for _, c := range beadWriteCases {
		t.Run(c.name, func(t *testing.T) {
			state := newFakeState(t)
			mem := state.stores["myrig"]
			b := seedBeadWriteTarget(t, mem, c, false)
			state.stores["myrig"] = writeFailStore{Store: mem}
			h := newTestCityHandler(t, state)

			rec := beadRequest(t, h, c.method, c.url(state, b.ID), c.body)
			if rec.Code < 400 {
				t.Fatalf("%s on a failing store: status %d, want an error; body: %s", c.name, rec.Code, rec.Body.String())
			}
			assertEnqueued(t, state)
			if got := state.enqueueCalls(); got != 0 {
				t.Fatalf("%s: enqueue calls = %d, want 0", c.name, got)
			}
		})
	}
}

// TestAPIBeadReadsEnqueueNothing: reads never wake the reconciler.
func TestAPIBeadReadsEnqueueNothing(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	b, err := state.stores["myrig"].Create(beads.Bead{Title: "w", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/beads", "/beads/ready", "/bead/" + b.ID, "/bead/" + b.ID + "/deps"} {
		rec := beadRequest(t, h, http.MethodGet, cityURL(state, path), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d; body: %s", path, rec.Code, rec.Body.String())
		}
	}
	assertEnqueued(t, state)
	if got := state.enqueueCalls(); got != 0 {
		t.Fatalf("reads: enqueue calls = %d, want 0", got)
	}
}

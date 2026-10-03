package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/reconcilekey"
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

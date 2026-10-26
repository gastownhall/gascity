package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// TestSessionRunningReflectsTheRuntimeInEveryState pins what `running` means to
// a client checking for quiescence: whether the session's runtime is live. It
// used to be computed only for active sessions, so a start-pending or draining
// session whose agent process was up read `running: false`. Live, after a
// client suspended every session reading `running: true`, three start-pending
// workers were still running, and the API reported none.
func TestSessionRunningReflectsTheRuntimeInEveryState(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandlerWith(t, fs, New(fs))
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "default", Title: "running-state", Command: "codex", WorkDir: "/tmp", Provider: "codex",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// running reads the session through both the single-session and the list
	// endpoints; they resolve the runtime by different paths, and a client
	// checking quiescence polls the list.
	running := func() bool {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+info.ID, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET session: status %d; body: %s", rec.Code, rec.Body.String())
		}
		var one sessionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
			t.Fatalf("decode: %v", err)
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/sessions"), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET sessions: status %d; body: %s", rec.Code, rec.Body.String())
		}
		var list struct {
			Items []sessionResponse `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		listed := false
		for _, item := range list.Items {
			if item.ID == info.ID {
				listed = item.Running
			}
		}
		if listed != one.Running {
			t.Errorf("state %q: GET /session running=%v but GET /sessions running=%v", one.State, one.Running, listed)
		}
		return one.Running && listed
	}

	if !running() {
		t.Fatal("active session with a live runtime: running = false, want true")
	}
	for _, state := range []string{"start-pending", "creating", "draining"} {
		if err := fs.cityBeadStore.SetMetadata(info.ID, "state", state); err != nil {
			t.Fatalf("SetMetadata(state=%s): %v", state, err)
		}
		if !running() {
			t.Errorf("%s session with a live runtime: running = false, want true", state)
		}
	}

	if err := fs.sp.Stop(info.SessionName); err != nil {
		t.Fatalf("stop runtime: %v", err)
	}
	if err := fs.cityBeadStore.SetMetadata(info.ID, "state", "asleep"); err != nil {
		t.Fatalf("SetMetadata(state=asleep): %v", err)
	}
	if running() {
		t.Error("asleep session with no runtime: running = true, want false")
	}
}

// TestSessionRunningIsTheRuntimeProbeNotTheLifecycleState pins that `running`
// is read from the runtime in both endpoints, never inferred from the session's
// lifecycle state. The single-session read used to take it from the worker
// phase, which a start-pending or creating session reports as "starting" (and
// an active one as "ready") without looking at the runtime, so GET /session/{id}
// said running: true while GET /sessions, which probes the runtime, said false.
// Live, a sampler reading both endpoints a few milliseconds apart saw that split
// on 14 consecutive startup reads, all before the session's agent process
// existed.
func TestSessionRunningIsTheRuntimeProbeNotTheLifecycleState(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandlerWith(t, fs, New(fs))
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp, session.WithCityPath(fs.cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "default", Title: "no-runtime-yet", Command: "codex", WorkDir: "/tmp", Provider: "codex",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// The runtime is gone (or not yet up): no agent process backs the session.
	if err := fs.sp.Stop(info.SessionName); err != nil {
		t.Fatalf("stop runtime: %v", err)
	}

	for _, state := range []string{"start-pending", "creating", "active", "draining"} {
		if err := fs.cityBeadStore.SetMetadata(info.ID, "state", state); err != nil {
			t.Fatalf("SetMetadata(state=%s): %v", state, err)
		}
		single, listed := sessionRunningFromBothEndpoints(t, h, fs, info.ID)
		if single || listed {
			t.Errorf("%s session with no runtime: GET /session running=%v, GET /sessions running=%v; want false from both", state, single, listed)
		}
	}
}

// sessionRunningFromBothEndpoints reads one session's running field from
// GET /session/{id} and from its row in GET /sessions.
func sessionRunningFromBothEndpoints(t *testing.T, h http.Handler, fs *fakeState, id string) (single, listed bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET session: status %d; body: %s", rec.Code, rec.Body.String())
	}
	var one sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/sessions"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET sessions: status %d; body: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []sessionResponse `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, item := range list.Items {
		if item.ID == id {
			listed, found = item.Running, true
		}
	}
	if !found {
		t.Fatalf("GET sessions: %s not listed", id)
	}
	return one.Running, listed
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Materialization contract
// - Wake, Suspend, and Pin
// - Close and Retirement Semantics

func TestPhase0HandleSessionSuspend_MaterializesReservedNamedIntoSuspendedState(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	rec := httptest.NewRecorder()
	req := newPostRequest("/v0/session/worker/suspend", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("suspend status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("len(session beads) = %d, want 1 canonical bead", len(all))
	}
	if got := all[0].Metadata["state"]; got != "suspended" {
		t.Fatalf("state = %q, want suspended", got)
	}
	if got := all[0].Metadata[apiNamedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session = %q, want true", got)
	}
}

func TestPhase0HandleSessionClose_AllowsConfiguredAlwaysNamedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.NamedSessions[0].Mode = "always"
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget: %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec for worker")
	}
	id, err := srv.materializeNamedSession(fs.cityBeadStore, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession: %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest("/v0/session/"+id+"/close", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if bead.Status != "closed" {
		t.Fatalf("status = %q, want closed", bead.Status)
	}
}

func TestPhase0HandleSessionClose_ClearsBeadScopedWakeAndHoldOverrides(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	id := phase0MaterializeCityScopedNamedWorker(t, srv, fs)
	if err := fs.cityBeadStore.SetMetadataBatch(id, map[string]string{
		"pin_awake":    "true",
		"held_until":   "9999-12-31T23:59:59Z",
		"sleep_intent": "user-hold",
		"state":        "suspended",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(overrides): %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest("/v0/session/"+id+"/close", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if bead.Status != "closed" {
		t.Fatalf("status = %q, want closed", bead.Status)
	}
	for _, field := range []string{"pin_awake", "held_until", "sleep_intent"} {
		if got := bead.Metadata[field]; got != "" {
			t.Fatalf("%s = %q after close, want cleared with the terminal bead", field, got)
		}
	}
}

func TestPhase0HandleSessionWake_ClosedBeadIDDoesNotCreateSuccessor(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	id := phase0MaterializeCityScopedNamedWorker(t, srv, fs)
	if err := fs.cityBeadStore.Close(id); err != nil {
		t.Fatalf("Close(%s): %v", id, err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest("/v0/session/"+id+"/wake", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("wake closed bead ID status = %d, want rejection; body: %s", rec.Code, rec.Body.String())
	}
	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("closed bead-ID wake materialized %d successor session(s), want 0", len(all))
	}
}

func TestPhase0HandleSessionWake_NamedIdentityAfterTerminalCloseUsesFreshCanonicalBead(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	oldID := phase0MaterializeCityScopedNamedWorker(t, srv, fs)

	closeRec := httptest.NewRecorder()
	srv.ServeHTTP(closeRec, newPostRequest("/v0/session/"+oldID+"/close", nil))
	if closeRec.Code != http.StatusOK {
		t.Fatalf("close status = %d, want %d; body: %s", closeRec.Code, http.StatusOK, closeRec.Body.String())
	}

	wakeRec := httptest.NewRecorder()
	srv.ServeHTTP(wakeRec, newPostRequest("/v0/session/worker/wake", nil))
	if wakeRec.Code != http.StatusOK {
		t.Fatalf("wake named identity after close status = %d, want %d; body: %s", wakeRec.Code, http.StatusOK, wakeRec.Body.String())
	}

	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("open session bead count = %d, want 1 fresh successor", len(all))
	}
	if all[0].ID == oldID {
		t.Fatalf("named-identity wake reused closed bead %s, want fresh canonical successor", oldID)
	}
	if got := all[0].Metadata["configured_named_identity"]; got != "worker" {
		t.Fatalf("successor configured_named_identity = %q, want worker", got)
	}
}

func TestPhase0HandleSessionWake_RejectsTemplateTokenOnSessionSurface(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	rec := httptest.NewRecorder()
	req := newPostRequest("/v0/session/template:worker/wake", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("wake status = %d, want non-200 session-targeting rejection; body: %s", rec.Code, rec.Body.String())
	}

	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("len(session beads) = %d, want 0", len(all))
	}
}

func TestPhase0ProviderCompatibility_CreateWritesManualOrigin(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	req := newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"provider","name":"test-agent"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	bead, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", resp.ID, err)
	}
	if got := bead.Metadata["session_origin"]; got != "manual" {
		t.Fatalf("session_origin = %q, want manual", got)
	}
}

func phase0MaterializeCityScopedNamedWorker(t *testing.T, srv *Server, fs *fakeState) string {
	t.Helper()
	fs.cfg.Agents[0].Dir = ""
	fs.cfg.NamedSessions[0].Dir = ""

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget(worker): %v", err)
	}
	if !ok {
		t.Fatal("expected city-scoped named session spec for worker")
	}
	id, err := srv.materializeNamedSession(fs.cityBeadStore, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession(worker): %v", err)
	}
	return id
}

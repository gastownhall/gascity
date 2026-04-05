package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestRigList(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/v0/rigs", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp listResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("total = %d, want 1", resp.Total)
	}
}

func TestRigGet(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/v0/rig/myrig", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var rig rigResponse
	if err := json.NewDecoder(rec.Body).Decode(&rig); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rig.Name != "myrig" {
		t.Fatalf("name = %q, want %q", rig.Name, "myrig")
	}
}

func TestRigGetNotFound(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/v0/rig/nonexistent", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRigEnrichment(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "myrig", MaxActiveSessions: intPtr(1)},
		{Name: "coder", Dir: "myrig", MaxActiveSessions: intPtr(1)},
	}
	state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}) //nolint:errcheck
	srv := New(state)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/v0/rig/myrig", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var rig rigResponse
	json.NewDecoder(rec.Body).Decode(&rig) //nolint:errcheck
	if rig.AgentCount != 2 {
		t.Errorf("AgentCount = %d, want 2", rig.AgentCount)
	}
	if rig.RunningCount != 1 {
		t.Errorf("RunningCount = %d, want 1", rig.RunningCount)
	}
}

func TestRigSuspendResume(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	// Suspend rig.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/rig/myrig/suspend", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("suspend: status = %d, want 200", rec.Code)
	}

	// Read-after-write: rig should show as suspended.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/v0/rig/myrig", nil))

	var rig rigResponse
	if err := json.NewDecoder(rec.Body).Decode(&rig); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !rig.Suspended {
		t.Fatal("rig should be suspended after suspend action")
	}

	// Resume rig.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/rig/myrig/resume", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("resume: status = %d, want 200", rec.Code)
	}

	// Read-after-write: rig should show as not suspended.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/v0/rig/myrig", nil))

	if err := json.NewDecoder(rec.Body).Decode(&rig); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rig.Suspended {
		t.Fatal("rig should not be suspended after resume action")
	}
}

func TestRigActionNotFound(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/rig/nonexistent/suspend", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRigActionUnknown(t *testing.T) {
	state := newFakeMutatorState(t)
	srv := New(state)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/rig/myrig/reboot", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

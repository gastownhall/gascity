package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestHandleStatus(t *testing.T) {
	state := newFakeState(t)
	// Start a fake session so Running > 0.
	state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}) //nolint:errcheck
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "test-city" {
		t.Errorf("Name = %q, want %q", resp.Name, "test-city")
	}
	if resp.AgentCount != 1 {
		t.Errorf("AgentCount = %d, want 1", resp.AgentCount)
	}
	if resp.RigCount != 1 {
		t.Errorf("RigCount = %d, want 1", resp.RigCount)
	}
	if resp.Running != 1 {
		t.Errorf("Running = %d, want 1", resp.Running)
	}

	// Check X-GC-Index header is present.
	if rec.Header().Get("X-GC-Index") == "" {
		t.Error("missing X-GC-Index header")
	}
}

func TestHandleStatusEnriched(t *testing.T) {
	state := newFakeState(t)
	state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}) //nolint:errcheck
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp statusResponse
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck

	// Version from fakeState.
	if resp.Version != "test" {
		t.Errorf("Version = %q, want %q", resp.Version, "test")
	}

	// Uptime should be >= 0.
	if resp.UptimeSec < 0 {
		t.Errorf("UptimeSec = %d, want >= 0", resp.UptimeSec)
	}

	// Agent counts.
	if resp.Agents.Total != 1 {
		t.Errorf("Agents.Total = %d, want 1", resp.Agents.Total)
	}
	if resp.Agents.Running != 1 {
		t.Errorf("Agents.Running = %d, want 1", resp.Agents.Running)
	}

	// Rig counts.
	if resp.Rigs.Total != 1 {
		t.Errorf("Rigs.Total = %d, want 1", resp.Rigs.Total)
	}
}

func TestHandleHealth(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/health"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp map[string]any
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck

	if resp["status"] != "ok" {
		t.Errorf("status = %v, want %q", resp["status"], "ok")
	}
	if resp["version"] != "test" {
		t.Errorf("version = %v, want %q", resp["version"], "test")
	}
	if resp["city"] != "test-city" {
		t.Errorf("city = %v, want %q", resp["city"], "test-city")
	}
	if _, ok := resp["uptime_sec"]; !ok {
		t.Error("missing uptime_sec in health response")
	}
}

func TestHandleStatus_Suspended(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Workspace.Suspended = true
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Suspended {
		t.Error("expected suspended=true in status response")
	}
}

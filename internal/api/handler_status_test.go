package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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

func TestHandleStatusPreservesPartialWorkCountSurvivors(t *testing.T) {
	state := newFakeState(t)
	store := beads.NewMemStore()
	open, err := store.Create(beads.Bead{Type: "task", Title: "open survivor", Status: "open"})
	if err != nil {
		t.Fatalf("Create(open): %v", err)
	}
	ready, err := store.Create(beads.Bead{Type: "task", Title: "ready survivor", Status: "ready"})
	if err != nil {
		t.Fatalf("Create(ready): %v", err)
	}
	readyStatus := "ready"
	if err := store.Update(ready.ID, beads.UpdateOpts{Status: &readyStatus}); err != nil {
		t.Fatalf("Update(ready): %v", err)
	}
	ready, err = store.Get(ready.ID)
	if err != nil {
		t.Fatalf("Get(ready): %v", err)
	}
	inProgress, err := store.Create(beads.Bead{Type: "task", Title: "claimed survivor", Status: "in_progress"})
	if err != nil {
		t.Fatalf("Create(in_progress): %v", err)
	}
	inProgressStatus := "in_progress"
	if err := store.Update(inProgress.ID, beads.UpdateOpts{Status: &inProgressStatus}); err != nil {
		t.Fatalf("Update(in_progress): %v", err)
	}
	inProgress, err = store.Get(inProgress.ID)
	if err != nil {
		t.Fatalf("Get(in_progress): %v", err)
	}
	state.stores["myrig"] = &failingBeadStore{
		Store:      store,
		listResult: []beads.Bead{open, ready, inProgress},
		listErr: &beads.PartialResultError{
			Op:  "bd list",
			Err: errors.New("skipped 1 corrupt bead"),
		},
	}
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
	if resp.Work.Open != 1 || resp.Work.Ready != 1 || resp.Work.InProgress != 1 {
		t.Fatalf("Work = %+v, want partial survivors counted", resp.Work)
	}
	if !resp.Partial {
		t.Fatalf("Partial = false, want true for partial work count")
	}
	if len(resp.PartialErrors) == 0 {
		t.Fatalf("PartialErrors empty")
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

func TestHandleStatusUsesCachedSessionStateForSuspendedAgents(t *testing.T) {
	state := newFakeState(t)
	store := beads.NewMemStore()
	state.cityBeadStore = store
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateSuspended),
			"template":     "myrig/worker",
			"session_name": "myrig--worker",
		},
	}); err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	if err := state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	state.sp.Calls = nil
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
	if resp.Agents.Suspended != 1 {
		t.Fatalf("Agents.Suspended = %d, want 1", resp.Agents.Suspended)
	}
	if resp.Agents.Running != 0 {
		t.Fatalf("Agents.Running = %d, want 0 for suspended session", resp.Agents.Running)
	}
	if resp.Running != 1 {
		t.Fatalf("Running = %d, want raw liveness count 1", resp.Running)
	}
}

func TestHandleStatusUsesPartialSessionRows(t *testing.T) {
	state := newFakeState(t)
	store := &partialPrimeSessionStore{MemStore: beads.NewMemStore()}
	state.cityBeadStore = store
	sessionBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateSuspended),
			"template":     "myrig/worker",
			"session_name": "myrig--worker",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	store.partialRows = []beads.Bead{sessionBead}
	if err := state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
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
	if resp.Agents.Suspended != 1 {
		t.Fatalf("Agents.Suspended = %d, want partial survivor to mark session suspended", resp.Agents.Suspended)
	}
	if resp.Agents.Running != 0 {
		t.Fatalf("Agents.Running = %d, want 0 for suspended partial survivor", resp.Agents.Running)
	}
	if resp.Running != 1 {
		t.Fatalf("Running = %d, want raw liveness count 1", resp.Running)
	}
	if !resp.Partial {
		t.Fatalf("Partial = false, want true for partial session snapshot")
	}
	if len(resp.PartialErrors) == 0 {
		t.Fatalf("PartialErrors empty")
	}
}

func TestHandleStatusUsesNewestSessionBeadForDuplicateSessionName(t *testing.T) {
	state := newFakeState(t)
	store := beads.NewMemStore()
	state.cityBeadStore = store
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateSuspended),
			"template":     "myrig/worker",
			"session_name": "myrig--worker",
		},
	}); err != nil {
		t.Fatalf("Create old session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateActive),
			"template":     "myrig/worker",
			"session_name": "myrig--worker",
		},
	}); err != nil {
		t.Fatalf("Create new session bead: %v", err)
	}
	if err := state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
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
	if resp.Agents.Suspended != 0 {
		t.Fatalf("Agents.Suspended = %d, want 0 from newest active bead", resp.Agents.Suspended)
	}
	if resp.Agents.Running != 1 {
		t.Fatalf("Agents.Running = %d, want 1", resp.Agents.Running)
	}
}

func TestHandleStatusUnlimitedPoolUsesOpenNonArchivedSessionBeads(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents[0].MaxActiveSessions = intPtr(-1)
	store := beads.NewMemStore()
	state.cityBeadStore = store
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateActive),
			"template":     "myrig/worker",
			"session_name": "myrig--worker-1",
		},
	}); err != nil {
		t.Fatalf("Create active session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateSuspended),
			"template":     "myrig/worker",
			"session_name": "myrig--worker-2",
		},
	}); err != nil {
		t.Fatalf("Create suspended session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateArchived),
			"template":     "myrig/worker",
			"session_name": "myrig--worker-3",
		},
	}); err != nil {
		t.Fatalf("Create archived session bead: %v", err)
	}
	if err := state.sp.Start(context.Background(), "myrig--worker-1", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
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
	if resp.Agents.Total != 2 {
		t.Fatalf("Agents.Total = %d, want 2 non-archived unlimited-pool slots", resp.Agents.Total)
	}
	if resp.Agents.Running != 1 {
		t.Fatalf("Agents.Running = %d, want 1", resp.Agents.Running)
	}
	if resp.Agents.Suspended != 1 {
		t.Fatalf("Agents.Suspended = %d, want 1", resp.Agents.Suspended)
	}
}

func TestHandleStatusBoundedPoolUsesCachedSessionState(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents[0].MaxActiveSessions = intPtr(2)
	store := beads.NewMemStore()
	state.cityBeadStore = store
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":        string(session.StateSuspended),
			"template":     "myrig/worker",
			"session_name": "myrig--worker-2",
		},
	}); err != nil {
		t.Fatalf("Create suspended pool session bead: %v", err)
	}
	if err := state.sp.Start(context.Background(), "myrig--worker-1", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
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
	if resp.Agents.Total != 2 {
		t.Fatalf("Agents.Total = %d, want 2 bounded pool slots", resp.Agents.Total)
	}
	if resp.Agents.Running != 1 {
		t.Fatalf("Agents.Running = %d, want 1", resp.Agents.Running)
	}
	if resp.Agents.Suspended != 1 {
		t.Fatalf("Agents.Suspended = %d, want 1", resp.Agents.Suspended)
	}
}

func TestHandleStatusOnlyUsesProviderLiveness(t *testing.T) {
	state := newFakeState(t)
	if err := state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := state.sp.SetMeta("myrig--worker", "suspended", "true"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	state.sp.SetAttached("myrig--worker", true)
	state.sp.SetActivity("myrig--worker", state.startedAt)
	state.sp.Calls = nil
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/status"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	for _, call := range state.sp.Calls {
		switch call.Method {
		case "ProcessAlive", "IsAttached", "GetLastActivity", "GetMeta", "ListRunning":
			t.Fatalf("/status called provider %s for %q; calls=%#v", call.Method, call.Name, state.sp.Calls)
		}
	}
	var resp statusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Agents.Running != 1 {
		t.Fatalf("Agents.Running = %d, want 1", resp.Agents.Running)
	}
	if resp.Running != 1 {
		t.Fatalf("Running = %d, want 1", resp.Running)
	}
}

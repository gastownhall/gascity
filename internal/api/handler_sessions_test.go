package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

func newSessionFakeState(t *testing.T) *fakeState {
	t.Helper()
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	return fs
}

func createTestSession(t *testing.T, store beads.Store, sp *runtime.Fake, title string) session.Info {
	t.Helper()
	mgr := session.NewManager(store, sp)
	info, err := mgr.Create(context.Background(), "default", title, "echo test", "/tmp", "test", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return info
}

type cancelStartProvider struct {
	*runtime.Fake
}

func (p *cancelStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.Fake.Start(ctx, name, cfg)
}

type stateWithSessionProvider struct {
	*fakeState
	provider runtime.Provider
}

func (s *stateWithSessionProvider) SessionProvider() runtime.Provider {
	return s.provider
}

func seedQueuedWaitNudge(t *testing.T, fs *fakeState, wait beads.Bead, agentName string) string {
	t.Helper()
	nudgeID := "wait-" + wait.ID
	if err := fs.cityBeadStore.SetMetadataBatch(wait.ID, map[string]string{"nudge_id": nudgeID}); err != nil {
		t.Fatalf("set wait nudge_id: %v", err)
	}
	if _, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   "nudge",
		Title:  "nudge:" + nudgeID,
		Labels: []string{"nudge:" + nudgeID},
		Metadata: map[string]string{
			"nudge_id": nudgeID,
			"state":    "queued",
		},
	}); err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	statePath := citylayout.RuntimePath(fs.cityPath, "nudges", "state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("create nudge queue dir: %v", err)
	}
	data, err := json.MarshalIndent(map[string]any{
		"pending": []map[string]any{{
			"id":    nudgeID,
			"agent": agentName,
		}},
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal nudge queue: %v", err)
	}
	if err := os.WriteFile(statePath, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("seed nudge queue: %v", err)
	}
	return nudgeID
}

func loadQueuedWaitNudgeState(t *testing.T, cityPath string) struct {
	Pending  []map[string]any `json:"pending,omitempty"`
	InFlight []map[string]any `json:"in_flight,omitempty"`
} {
	t.Helper()
	statePath := citylayout.RuntimePath(cityPath, "nudges", "state.json")
	data, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return struct {
			Pending  []map[string]any `json:"pending,omitempty"`
			InFlight []map[string]any `json:"in_flight,omitempty"`
		}{}
	}
	if err != nil {
		t.Fatalf("read nudge queue: %v", err)
	}
	var state struct {
		Pending  []map[string]any `json:"pending,omitempty"`
		InFlight []map[string]any `json:"in_flight,omitempty"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse nudge queue: %v", err)
	}
	return state
}

func writeNamedSessionJSONL(t *testing.T, searchBase, workDir, fileName string, lines ...string) {
	t.Helper()
	dir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileName)
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandleSessionList(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	// Create two sessions.
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session A")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session B")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/sessions", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp listResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Errorf("got total %d, want 2", resp.Total)
	}
}

func TestHandleSessionListFilterByState(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Suspend")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Stay Active")

	// Suspend one.
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	// List only active.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/sessions?state=active", nil)
	srv.ServeHTTP(w, r)

	var resp listResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("got total %d, want 1 (only active)", resp.Total)
	}
}

func TestHandleSessionListPagination(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	// Create 3 sessions.
	createTestSession(t, fs.cityBeadStore, fs.sp, "S1")
	createTestSession(t, fs.cityBeadStore, fs.sp, "S2")
	createTestSession(t, fs.cityBeadStore, fs.sp, "S3")

	// Limit without cursor truncates but returns no next_cursor.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/sessions?limit=2", nil)
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("limit-only: status %d", w.Code)
	}
	var resp listResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	items, _ := resp.Items.([]any)
	if len(items) != 2 {
		t.Errorf("limit-only: got %d items, want 2", len(items))
	}
	if resp.NextCursor != "" {
		t.Errorf("limit-only: got next_cursor %q, want empty (no cursor mode)", resp.NextCursor)
	}

	// Cursor mode: first page.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/v0/sessions?cursor=&limit=2", nil)
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("page1: status %d", w.Code)
	}
	var page1 listResponse
	if err := json.NewDecoder(w.Body).Decode(&page1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	items1, _ := page1.Items.([]any)
	if len(items1) != 2 {
		t.Errorf("page1: got %d items, want 2", len(items1))
	}
	if page1.Total != 3 {
		t.Errorf("page1: total = %d, want 3", page1.Total)
	}
	if page1.NextCursor == "" {
		t.Fatal("page1: expected next_cursor, got empty")
	}

	// Cursor mode: second page.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/v0/sessions?cursor="+page1.NextCursor+"&limit=2", nil)
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("page2: status %d", w.Code)
	}
	var page2 listResponse
	if err := json.NewDecoder(w.Body).Decode(&page2); err != nil {
		t.Fatalf("decode page2: %v", err)
	}
	items2, _ := page2.Items.([]any)
	if len(items2) != 1 {
		t.Errorf("page2: got %d items, want 1", len(items2))
	}
	if page2.NextCursor != "" {
		t.Errorf("page2: got next_cursor %q, want empty (last page)", page2.NextCursor)
	}
}

func TestHandleSessionGet(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID, nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != info.ID {
		t.Errorf("got ID %q, want %q", resp.ID, info.ID)
	}
	if resp.Title != "My Session" {
		t.Errorf("got title %q, want %q", resp.Title, "My Session")
	}
	if resp.State != "active" {
		t.Errorf("got state %q, want %q", resp.State, "active")
	}
	if !resp.Running {
		t.Errorf("got running=%v, want true", resp.Running)
	}
}

func TestHandleSessionGetNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/nonexistent", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleSessionSuspend(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Suspend")

	w := httptest.NewRecorder()
	r := newPostRequest("/v0/session/"+info.ID+"/suspend", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify the session is now suspended.
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != session.StateSuspended {
		t.Errorf("got state %q, want %q", got.State, session.StateSuspended)
	}
}

// TestHandleSessionSuspend_IllegalTransition covers Fix 3j: illegal state
// transitions from the manager surface as 409 Problem Details to the API.
// Drain puts the session in Draining; a subsequent Suspend is illegal
// (the state machine only allows Suspend from Active/Asleep/Quarantined).
func TestHandleSessionSuspend_IllegalTransition(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Drain")

	// Drain the session directly via the manager (the API surface for drain
	// lives elsewhere; this test isolates the transition check).
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.BeginDrain(info.ID, "shutdown"); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}

	w := httptest.NewRecorder()
	r := newPostRequest("/v0/session/"+info.ID+"/suspend", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, http.StatusConflict, w.Body.String())
	}

	// Response body should be RFC 9457 Problem Details with the
	// `illegal_transition:` semantic prefix in the detail field.
	var problem struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(w.Body).Decode(&problem); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if problem.Status != http.StatusConflict {
		t.Errorf("problem.status = %d, want %d", problem.Status, http.StatusConflict)
	}
	if !strings.Contains(problem.Detail, "illegal_transition") {
		t.Errorf("problem.detail = %q, want substring %q", problem.Detail, "illegal_transition")
	}
}

func TestHandleSessionClose(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Close")
	wait, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.WaitBeadType,
		Labels: []string{session.WaitBeadLabel, "session:" + info.ID},
		Metadata: map[string]string{
			"session_id": info.ID,
			"state":      "pending",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}
	nudgeID := seedQueuedWaitNudge(t, fs, wait, "default")

	w := httptest.NewRecorder()
	r := newPostRequest("/v0/session/"+info.ID+"/close", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Session should no longer appear in default listing (excludes closed).
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	sessions, err := mgr.List("", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("got %d sessions after close, want 0", len(sessions))
	}
	wait, err = fs.cityBeadStore.Get(wait.ID)
	if err != nil {
		t.Fatalf("get wait: %v", err)
	}
	if wait.Metadata["state"] != "canceled" {
		t.Fatalf("wait state = %q, want canceled", wait.Metadata["state"])
	}
	state := loadQueuedWaitNudgeState(t, fs.cityPath)
	for _, item := range append(state.Pending, state.InFlight...) {
		if got, _ := item["id"].(string); got == nudgeID {
			t.Fatalf("nudge %q still queued after close", nudgeID)
		}
	}
	items, err := fs.cityBeadStore.ListByLabel("nudge:"+nudgeID, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel(nudge): %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("nudge bead count = %d, want 1", len(items))
	}
	if items[0].Status != "closed" {
		t.Fatalf("nudge status = %q, want closed", items[0].Status)
	}
	if items[0].Metadata["terminal_reason"] != "wait-canceled" {
		t.Fatalf("nudge terminal_reason = %q, want wait-canceled", items[0].Metadata["terminal_reason"])
	}
}

func TestHandleSessionWake_DoesNotRewriteHistoricalWaitNudge(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Historical Wait")
	wait, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.WaitBeadType,
		Labels: []string{session.WaitBeadLabel, "session:" + info.ID},
		Metadata: map[string]string{
			"session_id": info.ID,
			"state":      "closed",
			"nudge_id":   "wait-historical",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}
	if err := fs.cityBeadStore.Close(wait.ID); err != nil {
		t.Fatalf("close wait: %v", err)
	}
	nudge, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   "nudge",
		Title:  "nudge:wait-historical",
		Labels: []string{"nudge:wait-historical"},
		Metadata: map[string]string{
			"nudge_id":        "wait-historical",
			"state":           "injected",
			"commit_boundary": "provider-nudge-return",
		},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	if err := fs.cityBeadStore.Close(nudge.ID); err != nil {
		t.Fatalf("close nudge bead: %v", err)
	}
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"wait_hold":    "true",
		"sleep_intent": "wait-hold",
		"sleep_reason": "wait-hold",
	})

	w := httptest.NewRecorder()
	r := newPostRequest("/v0/session/"+info.ID+"/wake", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	updated, err := fs.cityBeadStore.Get(nudge.ID)
	if err != nil {
		t.Fatalf("get nudge: %v", err)
	}
	if updated.Metadata["state"] != "injected" {
		t.Fatalf("nudge state = %q, want injected", updated.Metadata["state"])
	}
	if updated.Metadata["terminal_reason"] != "" {
		t.Fatalf("nudge terminal_reason = %q, want empty", updated.Metadata["terminal_reason"])
	}
	if updated.Metadata["commit_boundary"] != "provider-nudge-return" {
		t.Fatalf("nudge commit_boundary = %q, want provider-nudge-return", updated.Metadata["commit_boundary"])
	}
}

func TestHandleSessionNoCityStore(t *testing.T) {
	fs := newFakeState(t) // no cityBeadStore set
	srv := New(fs)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/sessions", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleSessionWake(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Held Session")
	wait, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.WaitBeadType,
		Labels: []string{session.WaitBeadLabel, "session:" + info.ID},
		Metadata: map[string]string{
			"session_id": info.ID,
			"state":      "pending",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}
	nudgeID := seedQueuedWaitNudge(t, fs, wait, "default")

	// Set hold metadata.
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"held_until":   "9999-12-31T23:59:59Z",
		"wait_hold":    "true",
		"sleep_intent": "wait-hold",
		"sleep_reason": "wait-hold",
	})

	w := httptest.NewRecorder()
	r := newPostRequest("/v0/session/"+info.ID+"/wake", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify hold cleared.
	b, _ := fs.cityBeadStore.Get(info.ID)
	if b.Metadata["held_until"] != "" {
		t.Errorf("held_until should be cleared, got %q", b.Metadata["held_until"])
	}
	if b.Metadata["wait_hold"] != "" {
		t.Errorf("wait_hold should be cleared, got %q", b.Metadata["wait_hold"])
	}
	if b.Metadata["sleep_intent"] != "" {
		t.Errorf("sleep_intent should be cleared, got %q", b.Metadata["sleep_intent"])
	}
	if b.Metadata["sleep_reason"] != "" {
		t.Errorf("sleep_reason should be cleared, got %q", b.Metadata["sleep_reason"])
	}
	wait, err = fs.cityBeadStore.Get(wait.ID)
	if err != nil {
		t.Fatalf("get wait: %v", err)
	}
	if wait.Metadata["state"] != "canceled" {
		t.Fatalf("wait state = %q, want canceled", wait.Metadata["state"])
	}
	state := loadQueuedWaitNudgeState(t, fs.cityPath)
	for _, item := range append(state.Pending, state.InFlight...) {
		if got, _ := item["id"].(string); got == nudgeID {
			t.Fatalf("nudge %q still queued after wake", nudgeID)
		}
	}
	items, err := fs.cityBeadStore.ListByLabel("nudge:"+nudgeID, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel(nudge): %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("nudge bead count = %d, want 1", len(items))
	}
	if items[0].Status != "closed" {
		t.Fatalf("nudge status = %q, want closed", items[0].Status)
	}
	if items[0].Metadata["terminal_reason"] != "wait-canceled" {
		t.Fatalf("nudge terminal_reason = %q, want wait-canceled", items[0].Metadata["terminal_reason"])
	}
}

func TestHandleSessionWakeClosed(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Closed Session")
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	_ = mgr.Close(info.ID)

	w := httptest.NewRecorder()
	r := newPostRequest("/v0/session/"+info.ID+"/wake", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleSessionGetByTemplateName(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Named Session")

	// Set alias metadata on the bead so public resolution works.
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"alias": "overseer",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/overseer", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != info.ID {
		t.Errorf("got ID %q, want %q", resp.ID, info.ID)
	}
}

func TestHandleSessionPatchTitle(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")

	body := `{"title":"Updated Title"}`
	req := httptest.NewRequest("PATCH", "/v0/session/"+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Title != "Updated Title" {
		t.Errorf("got title %q, want %q", resp.Title, "Updated Title")
	}
}

func TestHandleSessionPatchAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")

	body := `{"alias":"mayor"}`
	req := httptest.NewRequest("PATCH", "/v0/session/"+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Alias != "mayor" {
		t.Errorf("got alias %q, want %q", resp.Alias, "mayor")
	}
}

func TestHandleSessionPatchAliasRejectsManagedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"agent_name": "mayor",
	}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	body := `{"alias":"new-mayor"}`
	req := httptest.NewRequest("PATCH", "/v0/session/"+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
}

func TestHandleSessionPatchRejectsReservedQualifiedAliasOnFork(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(
		context.Background(),
		"myrig/worker",
		"Fork",
		"claude",
		t.TempDir(),
		"claude",
		nil,
		session.ProviderResume{},
		runtime.Config{},
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := `{"alias":"myrig/worker"}`
	req := httptest.NewRequest("PATCH", "/v0/session/"+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleSessionPatchImmutableField(t *testing.T) {
	// Fix 3f(remnant): PATCH body is now a typed struct with
	// additionalProperties:false on the schema, so unknown fields like
	// "template" are rejected by Huma's validation layer (422) rather
	// than the handler-side 403. This is a stricter error class for the
	// same underlying constraint.
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")

	body := `{"template":"hacked"}`
	req := httptest.NewRequest("PATCH", "/v0/session/"+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusUnprocessableEntity, w.Body.String())
	}
}

func TestHandleSessionListIncludesReason(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Held")

	// Set sleep reason on bead.
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"sleep_reason": "user-hold",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/sessions", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	// Parse into raw JSON to check for reason field.
	var raw struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(raw.Items))
	}
	var item sessionResponse
	if err := json.Unmarshal(raw.Items[0], &item); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	if item.Reason != "user-hold" {
		t.Errorf("got reason %q, want %q", item.Reason, "user-hold")
	}
}

func TestHandleSessionListOmitsExpiredLifecycleReason(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Expired Hold")
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"held_until": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/sessions", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var raw struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(raw.Items))
	}
	var item sessionResponse
	if err := json.Unmarshal(raw.Items[0], &item); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	if item.Reason != "" {
		t.Errorf("got reason %q, want empty after expired hold", item.Reason)
	}
}

func TestHandleSessionRename(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")

	body := `{"title":"Renamed"}`
	req := newPostRequest("/v0/session/"+info.ID+"/rename", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Title != "Renamed" {
		t.Errorf("got title %q, want %q", resp.Title, "Renamed")
	}
}

func TestHandleSessionRenameEmptyTitle(t *testing.T) {
	// Fix 3k(remnant): title now has minLength:"1"; empty-string bodies
	// are rejected by Huma's validation layer (422) rather than the
	// handler-side 400.
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")

	body := `{"title":""}`
	req := newPostRequest("/v0/session/"+info.ID+"/rename", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusUnprocessableEntity, w.Body.String())
	}
}

func TestHandleSessionAmbiguousAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	// Create two sessions with the same public alias.
	info1 := createTestSession(t, fs.cityBeadStore, fs.sp, "Worker 1")
	info2 := createTestSession(t, fs.cityBeadStore, fs.sp, "Worker 2")
	_ = fs.cityBeadStore.SetMetadataBatch(info1.ID, map[string]string{"alias": "worker"})
	_ = fs.cityBeadStore.SetMetadataBatch(info2.ID, map[string]string{"alias": "worker"})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/worker", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d (ambiguous); body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleSessionGetEnrichment(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Enriched Session")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID, nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !resp.Running {
		t.Error("running = false, want true for active session")
	}
	if resp.DisplayName != "Test" {
		t.Errorf("display_name = %q, want %q", resp.DisplayName, "Test")
	}
}

func TestHandleSessionListPeek(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	createTestSession(t, fs.cityBeadStore, fs.sp, "Peek Session")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/sessions", nil)
	srv.ServeHTTP(w, r)

	var resp struct {
		Items []sessionResponse `json:"items"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Items) == 0 {
		t.Fatal("no sessions returned")
	}
	if resp.Items[0].LastOutput != "" {
		t.Errorf("last_output = %q without peek param, want empty", resp.Items[0].LastOutput)
	}
}

func TestHandleSessionCreate(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	body := `{"kind":"agent","name":"myrig/worker"}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "sess-create-1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Template != "myrig/worker" {
		t.Errorf("Template = %q, want %q", resp.Template, "myrig/worker")
	}
	if resp.Title != "myrig/worker" {
		t.Errorf("Title = %q, want default %q", resp.Title, "myrig/worker")
	}
	// Agent sessions are always created async — not running until the
	// reconciler starts the process.
	if resp.Running {
		t.Errorf("Running = %v, want false for async create", resp.Running)
	}
	if resp.DisplayName != "Test Agent" {
		t.Errorf("DisplayName = %q, want %q", resp.DisplayName, "Test Agent")
	}
}

func TestHandleSessionCreateAsync(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	body := `{"kind":"agent","name":"myrig/worker","alias":"sky","async":true}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.State != "creating" {
		t.Fatalf("State = %q, want %q", resp.State, "creating")
	}
	if resp.Running {
		t.Fatalf("Running = true, want false for async create")
	}
	if resp.Alias != "sky" {
		t.Fatalf("Alias = %q, want %q", resp.Alias, "sky")
	}
	if fs.pokeCount != 1 {
		t.Fatalf("pokeCount = %d, want 1", fs.pokeCount)
	}
}

func TestHandleSessionCreateAsyncAcceptsInlineMessage(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	// Agent sessions are always async; messages are stored as initial_message
	// in template_overrides for the reconciler to pick up.
	body := `{"kind":"agent","name":"myrig/worker","async":true,"message":"hello"}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
}

func TestHandleSessionCreateAsync_PoolTemplateWithoutAliasUsesGeneratedWorkDirIdentity(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:              "ant",
		Dir:               "myrig",
		Provider:          "test-agent",
		WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(4),
	}}
	fs.cfg.NamedSessions = nil
	srv := New(fs)

	for i := 0; i < 2; i++ {
		req := newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"agent","name":"myrig/ant"}`))
		req.Header.Set("Idempotency-Key", "pool-create-"+string(rune('a'+i)))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("create #%d status = %d, want %d; body: %s", i+1, rec.Code, http.StatusAccepted, rec.Body.String())
		}
	}

	items, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("session bead count = %d, want 2", len(items))
	}
	seenWorkDir := make(map[string]bool, len(items))
	for _, bead := range items {
		if got := bead.Metadata["alias"]; got != "" {
			t.Fatalf("alias = %q, want empty", got)
		}
		sessionName := bead.Metadata["session_name"]
		if got := bead.Metadata["session_name_explicit"]; got != "true" {
			t.Fatalf("session_name_explicit = %q, want %q", got, "true")
		}
		if !strings.HasPrefix(sessionName, "ant-adhoc-") {
			t.Fatalf("session_name = %q, want ant-adhoc-*", sessionName)
		}
		workDir := bead.Metadata["work_dir"]
		if filepath.Dir(workDir) != filepath.Join(fs.cityPath, ".gc", "worktrees", "myrig", "ants") {
			t.Fatalf("work_dir parent = %q, want %q", filepath.Dir(workDir), filepath.Join(fs.cityPath, ".gc", "worktrees", "myrig", "ants"))
		}
		base := filepath.Base(workDir)
		if !strings.HasPrefix(base, "ant-adhoc-") {
			t.Fatalf("work_dir base = %q, want ant-adhoc-*", base)
		}
		if seenWorkDir[workDir] {
			t.Fatalf("duplicate work_dir %q", workDir)
		}
		seenWorkDir[workDir] = true
		if got := bead.Metadata["agent_name"]; got != "myrig/"+sessionName {
			t.Fatalf("agent_name(%q) = %q, want %q", sessionName, got, "myrig/"+sessionName)
		}
	}
}

func TestHandleSessionCreateAsync_PoolTemplateCanonicalizesAliasCollisions(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:              "ant",
		Dir:               "myrig",
		Provider:          "test-agent",
		WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(4),
	}}
	fs.cfg.NamedSessions = nil
	srv := New(fs)

	req := newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"agent","name":"myrig/ant","alias":"ant-fenrir"}`))
	req.Header.Set("Idempotency-Key", "pool-alias-1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first create status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Alias != "myrig/ant-fenrir" {
		t.Fatalf("Alias = %q, want canonical qualified alias", resp.Alias)
	}

	req = newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"agent","name":"myrig/ant","alias":"myrig/ant-fenrir"}`))
	req.Header.Set("Idempotency-Key", "pool-alias-2")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second create status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestHandleProviderSessionCreateRejectsAsync(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	body := `{"kind":"provider","name":"test-agent","async":true}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "async session creation is only supported for configured agent templates") {
		t.Fatalf("body = %q, want provider async guidance", w.Body.String())
	}
	if fs.pokeCount != 0 {
		t.Fatalf("pokeCount = %d, want 0", fs.pokeCount)
	}
}

func TestMaterializeNamedSession_RebrandedSingletonKeepsTemplateWorkDirIdentity(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:              "witness",
		Dir:               "myrig",
		Provider:          "test-agent",
		WorkDir:           ".gc/worktrees/{{.Rig}}/{{.AgentBase}}",
		MaxActiveSessions: intPtr(1),
	}}
	fs.cfg.NamedSessions = []config.NamedSession{{
		Name:     "boot",
		Template: "witness",
		Dir:      "myrig",
	}}
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "myrig/boot")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget: %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec")
	}
	id, err := srv.materializeNamedSession(fs.cityBeadStore, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession: %v", err)
	}
	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	wantWorkDir := filepath.Join(fs.cityPath, ".gc", "worktrees", "myrig", "witness")
	if got := bead.Metadata["work_dir"]; got != wantWorkDir {
		t.Fatalf("work_dir = %q, want %q", got, wantWorkDir)
	}
}

func TestHandleProviderSessionCreateWithMessageUsesProviderDefaultNudge(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	body := `{"kind":"provider","name":"test-agent","message":"hello"}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID == "" {
		t.Fatal("response missing id")
	}
	if resp.SessionName == "" {
		t.Fatal("response missing session_name")
	}

	nudgeCount := 0
	for _, call := range fs.sp.Calls {
		if call.Name != resp.SessionName || call.Message != "hello" {
			continue
		}
		if call.Method == "Nudge" {
			nudgeCount++
		}
	}
	if nudgeCount != 1 {
		t.Fatalf("Nudge count for %q = %d, want 1; calls=%#v", resp.SessionName, nudgeCount, fs.sp.Calls)
	}
}

func TestHandleSessionCreatePersistsAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	body := `{"kind":"agent","name":"myrig/worker","alias":"sky"}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Alias != "sky" {
		t.Fatalf("Alias = %q, want sky", resp.Alias)
	}
	if resp.SessionName == "sky" {
		t.Fatalf("SessionName = %q, want bead-derived runtime name", resp.SessionName)
	}
}

func TestHandleSessionCreateRejectsReservedQualifiedAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	body := `{"kind":"agent","name":"myrig/worker","alias":"myrig/worker"}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleProviderSessionCreateRejectsReservedQualifiedAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	body := `{"kind":"provider","name":"test-agent","alias":"myrig/worker"}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleSessionCreateRejectsInvalidAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	body := `{"kind":"agent","name":"myrig/worker","alias":"bad:name"}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleSessionCreateRejectsLegacySessionNameField(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	body := `{"kind":"agent","name":"myrig/worker","session_name":"mayor"}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "use alias") {
		t.Fatalf("body = %q, want use alias guidance", w.Body.String())
	}
}

func TestHandleSessionCreateRejectsEmptyLegacySessionNameField(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	body := `{"kind":"agent","name":"myrig/worker","session_name":""}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "use alias") {
		t.Fatalf("body = %q, want use alias guidance", w.Body.String())
	}
}

func TestHandleSessionCreateRejectsDuplicateAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	first := newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"agent","name":"myrig/worker","alias":"sky"}`))
	firstW := httptest.NewRecorder()
	srv.ServeHTTP(firstW, first)
	if firstW.Code != http.StatusAccepted {
		t.Fatalf("first create status %d, want %d; body: %s", firstW.Code, http.StatusAccepted, firstW.Body.String())
	}

	second := newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"agent","name":"myrig/worker","alias":"sky"}`))
	secondW := httptest.NewRecorder()
	srv.ServeHTTP(secondW, second)

	if secondW.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d; body: %s", secondW.Code, http.StatusConflict, secondW.Body.String())
	}
}

func TestHandleSessionCreateRejectsBareRigScopedTemplate(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	req := newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"agent","name":"worker"}`))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

// newSessionFakeStateWithOptions creates a test state where the provider has
// OptionsSchema and OptionDefaults, mimicking the builtin claude provider.
func newSessionFakeStateWithOptions(t *testing.T) *fakeState {
	t.Helper()
	fs := newFakeState(t)
	fs.cityBeadStore = beads.NewMemStore()
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"test-agent": {
			DisplayName: "Test Agent",
			Command:     "echo",
			OptionDefaults: map[string]string{
				"permission_mode": "unrestricted",
				"effort":          "max",
			},
			OptionsSchema: []config.ProviderOption{
				{
					Key: "permission_mode", Label: "Permission Mode", Type: "select",
					Default: "auto-edit",
					Choices: []config.OptionChoice{
						{Value: "auto-edit", Label: "Auto edit", FlagArgs: []string{"--permission-mode", "auto-edit"}},
						{Value: "unrestricted", Label: "Unrestricted", FlagArgs: []string{"--skip-permissions"}},
						{Value: "plan", Label: "Plan", FlagArgs: []string{"--permission-mode", "plan"}},
					},
				},
				{
					Key: "effort", Label: "Effort", Type: "select",
					Default: "",
					Choices: []config.OptionChoice{
						{Value: "", Label: "Default", FlagArgs: nil},
						{Value: "low", Label: "Low", FlagArgs: []string{"--effort", "low"}},
						{Value: "max", Label: "Max", FlagArgs: []string{"--effort", "max"}},
						{Value: "high", Label: "High", FlagArgs: []string{"--effort", "high"}},
					},
				},
			},
		},
	}
	return fs
}

func TestHandleSessionCreateAppliesProviderDefaults(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)

	body := `{"kind":"agent","name":"myrig/worker"}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	b, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	cmd := b.Metadata["command"]
	if !strings.Contains(cmd, "--skip-permissions") {
		t.Errorf("command %q should contain --skip-permissions from provider default permission_mode=unrestricted", cmd)
	}
	if !strings.Contains(cmd, "--effort max") {
		t.Errorf("command %q should contain --effort max from provider default effort=max", cmd)
	}
}

func TestHandleSessionCreateMergesPartialOptionsWithDefaults(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)

	body := `{"kind":"agent","name":"myrig/worker","options":{"effort":"high"}}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	b, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	cmd := b.Metadata["command"]
	if !strings.Contains(cmd, "--skip-permissions") {
		t.Errorf("command %q should contain --skip-permissions from unspecified default permission_mode=unrestricted", cmd)
	}
	if !strings.Contains(cmd, "--effort high") {
		t.Errorf("command %q should contain --effort high from explicit option", cmd)
	}
	if strings.Contains(cmd, "--effort max") {
		t.Errorf("command %q should NOT contain --effort max — user specified high", cmd)
	}
}

func TestHandleSessionCreateExplicitOptionsOverrideDefaults(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)

	body := `{"kind":"agent","name":"myrig/worker","options":{"permission_mode":"plan","effort":"low"}}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	b, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	cmd := b.Metadata["command"]
	if !strings.Contains(cmd, "--permission-mode plan") {
		t.Errorf("command %q should contain --permission-mode plan from explicit option", cmd)
	}
	if strings.Contains(cmd, "--skip-permissions") {
		t.Errorf("command %q should NOT contain --skip-permissions — user specified plan", cmd)
	}
	if !strings.Contains(cmd, "--effort low") {
		t.Errorf("command %q should contain --effort low from explicit option", cmd)
	}
}

func TestHandleSessionCreatePreservesInitialMessageWithOptions(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)

	// Create session with BOTH options AND a message.
	// Regression: the old code overwrote template_overrides with just the
	// options, clobbering the initial_message that was set at creation time.
	body := `{"kind":"agent","name":"myrig/worker","message":"Hello from Discord!","options":{"effort":"high"}}`
	req := newPostRequest("/v0/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	b, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	ovr := b.Metadata["template_overrides"]
	if ovr == "" {
		t.Fatal("template_overrides not set")
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(ovr), &parsed); err != nil {
		t.Fatalf("parse template_overrides: %v", err)
	}
	if parsed["initial_message"] != "Hello from Discord!" {
		t.Errorf("initial_message = %q, want %q", parsed["initial_message"], "Hello from Discord!")
	}
	if parsed["effort"] != "high" {
		t.Errorf("effort = %q, want %q", parsed["effort"], "high")
	}
}

func TestHandleSessionMessageResumesSuspendedSessionUsingProviderDefaultNudge(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Resume Me")
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	req := newPostRequest("/v0/session/"+info.ID+"/messages", strings.NewReader(`{"message":"hello"}`))
	req.Header.Set("Idempotency-Key", "sess-msg-1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if !fs.sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after POST /messages")
	}
	found := false
	for _, call := range fs.sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want provider-default nudge hello", fs.sp.Calls)
	}
}

func TestHandleSessionMessageMaterializesNamedSessionUsingProviderDefaultNudge(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	req := newPostRequest("/v0/session/myrig%2Fworker/messages", strings.NewReader(`{"message":"hello"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("message status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := resp["id"]
	if id == "" {
		t.Fatal("response missing session id")
	}
	b, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	if got := b.Metadata[apiNamedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session = %q, want true", got)
	}
	if got := b.Metadata["alias"]; got != "myrig/worker" {
		t.Fatalf("alias = %q, want myrig/worker", got)
	}
	sessionName := b.Metadata["session_name"]
	if sessionName == "" {
		t.Fatal("materialized named session missing session_name")
	}
	if !fs.sp.IsRunning(sessionName) {
		t.Fatalf("session %q should be running after POST /messages", sessionName)
	}
	nudgeCount := 0
	for _, call := range fs.sp.Calls {
		if call.Method == "Nudge" && call.Name == sessionName && call.Message == "hello" {
			nudgeCount++
		}
	}
	if nudgeCount != 1 {
		t.Fatalf("Nudge count for %q = %d, want 1; calls=%#v", sessionName, nudgeCount, fs.sp.Calls)
	}
}

func TestResolveSessionIDMaterializingNamedWithContext_RollsBackCanceledCreate(t *testing.T) {
	fs := newSessionFakeState(t)
	provider := &cancelStartProvider{Fake: runtime.NewFake()}
	srv := New(&stateWithSessionProvider{fakeState: fs, provider: provider})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := srv.resolveSessionIDMaterializingNamedWithContext(ctx, fs.cityBeadStore, "myrig/worker")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveSessionIDMaterializingNamedWithContext: %v, want context canceled", err)
	}

	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	for _, b := range all {
		if b.Status != "closed" {
			t.Fatalf("session bead %s status = %q, want closed after canceled create rollback", b.ID, b.Status)
		}
	}
}

func TestHandleSessionGetIncludesConfiguredNamedSessionFlag(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "myrig/worker")
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
	req := httptest.NewRequest("GET", "/v0/session/"+id, nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.ConfiguredNamedSession {
		t.Fatal("ConfiguredNamedSession = false, want true")
	}
}

func TestHandleSessionMessageInvalidNamedTargetDoesNotMaterialize(t *testing.T) {
	// Fix 3k(remnant): whitespace-only messages are rejected by the
	// pattern:"\\S" validation on the body; Huma returns 422 before
	// the handler runs, so no session materializes.
	fs := newSessionFakeState(t)
	srv := New(fs)

	req := newPostRequest("/v0/session/worker/messages", strings.NewReader(`{"message":"   "}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("message status = %d, want %d; body: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	items, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("invalid message materialized sessions unexpectedly: %#v", items)
	}
}

func TestHandleSessionGetReservedNamedTargetIgnoresClosedHistoricalBead(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateAliasedNamedWithTransport(
		context.Background(),
		"myrig/worker",
		"",
		"myrig/worker",
		"Historic Worker",
		"claude",
		t.TempDir(),
		"claude",
		"",
		nil,
		session.ProviderResume{},
		runtime.Config{},
	)
	if err != nil {
		t.Fatalf("CreateNamedWithTransport: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v0/session/worker", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("get status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandleSessionCloseAllowsAlwaysNamedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.NamedSessions[0].Mode = "always"
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "myrig/worker")
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
}

func TestFindNamedSessionSpecForTarget_RequiresFullyQualifiedWhenAmbiguous(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{
		{Name: "worker", Dir: "rig-a", Provider: "test-agent"},
		{Name: "worker", Dir: "rig-b", Provider: "test-agent"},
	}
	fs.cfg.NamedSessions = []config.NamedSession{
		{Template: "worker", Dir: "rig-a"},
		{Template: "worker", Dir: "rig-b"},
	}
	srv := New(fs)

	if _, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker"); err != nil || ok {
		t.Fatalf("findNamedSessionSpecForTarget(worker) = ok=%v err=%v, want not found without qualified target", ok, err)
	}

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "rig-a/worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget(rig-a/worker): %v", err)
	}
	if !ok {
		t.Fatal("expected fully qualified named session target to resolve")
	}
	if got := spec.Identity; got != "rig-a/worker" {
		t.Fatalf("Identity = %q, want rig-a/worker", got)
	}
}

func TestResolveSessionIDMaterializingNamed_QualifiedAliasBasenameDoesNotStealNamedTarget(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	ordinary, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-other-worker",
			"alias":        "other/worker",
		},
	})
	if err != nil {
		t.Fatalf("create ordinary session bead: %v", err)
	}

	id, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker): %v", err)
	}
	if id == ordinary.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker) returned qualified alias basename match %q; want canonical named session", id)
	}
	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got := bead.Metadata["alias"]; got != "myrig/worker" {
		t.Fatalf("alias = %q, want myrig/worker", got)
	}
	if got := bead.Metadata[apiNamedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session = %q, want true", got)
	}
}

func TestResolveSessionIDMaterializingNamed_AdoptsCanonicalRuntimeSessionNameBead(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget(worker): %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec for worker")
	}
	bead, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": spec.SessionName,
			"template":     spec.Identity,
			"agent_name":   spec.Identity,
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("create canonical runtime bead: %v", err)
	}

	id, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker): %v", err)
	}
	if id != bead.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker) = %q, want adopted bead %q", id, bead.ID)
	}
}

func TestResolveSessionIDMaterializingNamed_AdoptsArchivedContinuityEligibleNamedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget(worker): %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec for worker")
	}
	bead, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			apiNamedSessionMetadataKey: "true",
			apiNamedSessionIdentityKey: spec.Identity,
			apiNamedSessionModeKey:     spec.Mode,
			"session_name":             spec.SessionName,
			"alias":                    spec.Identity,
			"template":                 spec.Identity,
			"agent_name":               spec.Identity,
			"state":                    "archived",
			"continuity_eligible":      "true",
		},
	})
	if err != nil {
		t.Fatalf("create archived continuity-eligible named bead: %v", err)
	}

	id, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker): %v", err)
	}
	if id != bead.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker) = %q, want archived continuity bead %q", id, bead.ID)
	}
}

func TestResolveSessionIDMaterializingNamed_DoesNotAdoptOrdinaryPoolSessionForSameTemplate(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	ordinary, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-ordinary-worker",
			"template":     "myrig/worker",
			"agent_name":   "myrig/worker",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("create ordinary pool worker: %v", err)
	}

	id, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker): %v", err)
	}
	if id == ordinary.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker) adopted ordinary pool worker %q", ordinary.ID)
	}

	named, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got := named.Metadata[apiNamedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session = %q, want true", got)
	}
	if got := named.Metadata["alias"]; got != "myrig/worker" {
		t.Fatalf("alias = %q, want myrig/worker", got)
	}

	preserved, err := fs.cityBeadStore.Get(ordinary.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", ordinary.ID, err)
	}
	if preserved.Status != "open" {
		t.Fatalf("ordinary pool worker status = %q, want open", preserved.Status)
	}
	if got := preserved.Metadata[apiNamedSessionMetadataKey]; got != "" {
		t.Fatalf("ordinary pool worker configured_named_session = %q, want empty", got)
	}
}

func TestResolveSessionIDMaterializingNamed_RuntimeSessionNameWrongTemplateConflicts(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget(worker): %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec for worker")
	}
	if _, err := fs.cityBeadStore.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": spec.SessionName,
			"template":     "other/worker",
			"agent_name":   "other/worker",
			"state":        "asleep",
		},
	}); err != nil {
		t.Fatalf("create wrong-template runtime bead: %v", err)
	}

	_, err = srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "myrig/worker")
	if err == nil || !strings.Contains(err.Error(), "conflicts with configured named session") {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker) error = %v, want configured named session conflict", err)
	}
}

func TestHandleSessionWakeMaterializesNamedSessionAndStartsRuntime(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	rec := httptest.NewRecorder()
	req := newPostRequest("/v0/session/myrig%2Fworker/wake", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("wake status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := resp["id"]
	if id == "" {
		t.Fatal("wake response missing session id")
	}
	b, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	if got := b.Metadata[apiNamedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session = %q, want true", got)
	}
	if got := b.Metadata["alias"]; got != "myrig/worker" {
		t.Fatalf("alias = %q, want myrig/worker", got)
	}
	sessionName := b.Metadata["session_name"]
	if sessionName == "" {
		t.Fatal("materialized named session missing session_name")
	}
	if !fs.sp.IsRunning(sessionName) {
		t.Fatalf("session %q should be running after POST /wake", sessionName)
	}
}

func TestHandleSessionWakeCanceledNamedCreateRollsBack(t *testing.T) {
	fs := newSessionFakeState(t)
	provider := &cancelStartProvider{Fake: runtime.NewFake()}
	srv := New(&stateWithSessionProvider{fakeState: fs, provider: provider})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := httptest.NewRecorder()
	req := newPostRequest("/v0/session/myrig%2Fworker/wake", nil).WithContext(ctx)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("wake status = %d, want %d; body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	for _, b := range all {
		if b.Status != "closed" {
			t.Fatalf("session bead %s status = %q, want closed after canceled wake rollback", b.ID, b.Status)
		}
	}
}

func TestHandleSessionTranscriptUsesSessionKey(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	writeNamedSessionJSONL(t, searchBase, workDir, "latest.jsonl",
		`{"uuid":"9","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"wrong file\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/transcript", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionTranscriptResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "conversation" {
		t.Errorf("Format = %q, want %q", resp.Format, "conversation")
	}
	if len(resp.Turns) != 2 || resp.Turns[1].Text != "world" {
		t.Fatalf("Turns = %+v, want hello/world from session key file", resp.Turns)
	}
}

func TestHandleSessionTranscriptClosedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/transcript?tail=0", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionTranscriptResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Turns) != 2 || resp.Turns[0].Text != "hello" || resp.Turns[1].Text != "world" {
		t.Fatalf("Turns = %+v, want closed-session transcript hello/world", resp.Turns)
	}
}

func TestHandleSessionPendingAndRespond(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/pending", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("pending status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var pendingResp sessionPendingResponse
	if err := json.NewDecoder(w.Body).Decode(&pendingResp); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	if !pendingResp.Supported || pendingResp.Pending == nil || pendingResp.Pending.RequestID != "req-1" {
		t.Fatalf("pending response = %#v, want req-1", pendingResp)
	}

	respondReq := newPostRequest("/v0/session/"+info.ID+"/respond", strings.NewReader(`{"action":"approve"}`))
	respondReq.Header.Set("Idempotency-Key", "sess-respond-1")
	respondRec := httptest.NewRecorder()
	srv.ServeHTTP(respondRec, respondReq)

	if respondRec.Code != http.StatusAccepted {
		t.Fatalf("respond status = %d, want %d; body: %s", respondRec.Code, http.StatusAccepted, respondRec.Body.String())
	}
	if got := fs.sp.Responses[info.SessionName]; len(got) != 1 || got[0].Action != "approve" {
		t.Fatalf("responses = %#v, want single approve", got)
	}
}

func TestHandleSessionMessageRejectsPendingInteraction(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	rec := httptest.NewRecorder()
	req := newPostRequest("/v0/session/"+info.ID+"/messages", strings.NewReader(`{"message":"hello"}`))
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("message status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pending_interaction") {
		t.Fatalf("message body = %s, want pending_interaction error", rec.Body.String())
	}
	for _, call := range fs.sp.Calls {
		if (call.Method == "Nudge" || call.Method == "NudgeNow") && call.Name == info.SessionName {
			t.Fatalf("unexpected nudge while pending interaction is active: %#v", fs.sp.Calls)
		}
	}
}

func TestHandleSessionMessageRejectsClosedNamedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "myrig/worker", "Sky", "claude", t.TempDir(), "claude", "", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("CreateNamedWithTransport: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest("/v0/session/sky/messages", strings.NewReader(`{"message":"hello"}`))
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("message status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not_found") {
		t.Fatalf("message body = %s, want not_found error", rec.Body.String())
	}
}

func TestHandleSessionRespondMismatchedRequest(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	respondReq := newPostRequest("/v0/session/"+info.ID+"/respond", strings.NewReader(`{"request_id":"req-2","action":"approve"}`))
	respondRec := httptest.NewRecorder()
	srv.ServeHTTP(respondRec, respondReq)

	if respondRec.Code != http.StatusConflict {
		t.Fatalf("respond status = %d, want %d; body: %s", respondRec.Code, http.StatusConflict, respondRec.Body.String())
	}
}

func TestHandleSessionStreamSSEHeaders(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()
	<-done

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}
	if !strings.Contains(rec.Body.String(), "event: turn") || !strings.Contains(rec.Body.String(), "hello") {
		t.Errorf("stream body missing initial turn: %s", rec.Body.String())
	}
}

func TestHandleSessionStreamStoppedWithoutOutputReturnsNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{t.TempDir()}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(context.Background(), "default", "No Output", "echo test", t.TempDir(), "test", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandleSessionStreamClosedSessionReturnsSnapshot(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("closed session stream should return immediately")
	}

	if !strings.Contains(rec.Body.String(), "event: turn") || !strings.Contains(rec.Body.String(), "hello") || !strings.Contains(rec.Body.String(), "world") {
		t.Errorf("stream body missing closed-session snapshot: %s", rec.Body.String())
	}
}

func TestHandleSessionStreamClosedNamedSessionReturnsSnapshot(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "myrig/worker", "Chat", "claude", workDir, "claude", "", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("CreateNamedWithTransport: %v", err)
	}
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req := httptest.NewRequest("GET", "/v0/session/sky/stream", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("closed named session stream should return immediately")
	}

	if !strings.Contains(rec.Body.String(), "event: turn") || !strings.Contains(rec.Body.String(), "hello") || !strings.Contains(rec.Body.String(), "world") {
		t.Errorf("stream body missing closed-session snapshot: %s", rec.Body.String())
	}
}

func TestStreamSessionTranscriptLogDoesNotSkipTurnsAcrossCompactionBoundaries(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	searchBase := t.TempDir()
	workDir := t.TempDir()
	logDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	logPath := filepath.Join(logDir, "session.jsonl")
	initial := strings.Join([]string{
		`{"uuid":"a","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"before compaction\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"cb0","parentUuid":"a","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"b","parentUuid":"cb0","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"after first boundary\"}","timestamp":"2025-01-01T00:00:02Z"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	info := session.Info{ID: "sess-1", Template: "default"}
	ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer cancel()

	var bufMu sync.Mutex
	var buf bytes.Buffer
	send := sse.Sender(func(msg sse.Message) error {
		bufMu.Lock()
		defer bufMu.Unlock()
		data, err := json.Marshal(msg.Data)
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteString("\n")
		return nil
	})
	getBody := func() string {
		bufMu.Lock()
		defer bufMu.Unlock()
		return buf.String()
	}
	done := make(chan struct{})
	go func() {
		srv.streamSessionTranscriptLog(ctx, send, info, logPath)
		close(done)
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(getBody(), "after first boundary") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	appendFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, err = appendFile.WriteString(strings.Join([]string{
		`{"uuid":"c","parentUuid":"b","type":"user","message":"{\"role\":\"user\",\"content\":\"bridge turn\"}","timestamp":"2025-01-01T00:00:03Z"}`,
		`{"uuid":"cb1","parentUuid":"c","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:04Z"}`,
		`{"uuid":"d","parentUuid":"cb1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"after second boundary\"}","timestamp":"2025-01-01T00:00:05Z"}`,
	}, "\n") + "\n")
	if closeErr := appendFile.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("append transcript: %v", err)
	}

	<-done

	body := getBody()
	if !strings.Contains(body, "bridge turn") {
		t.Fatalf("stream body missing turn written before new compact boundary: %s", body)
	}
	if !strings.Contains(body, "after second boundary") {
		t.Fatalf("stream body missing turn written after new compact boundary: %s", body)
	}
}

func TestHandleSessionTranscriptRawIncludesAllTypes(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Write entries of different types, including tool_use and progress.
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"3","parentUuid":"2","type":"tool_use","message":"{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"name\":\"read\"}]}","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"tool_result","message":"{\"role\":\"tool\",\"content\":\"file contents\"}","timestamp":"2025-01-01T00:00:03Z"}`,
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/transcript?format=raw&tail=0", nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionRawTranscriptResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "raw" {
		t.Errorf("Format = %q, want %q", resp.Format, "raw")
	}
	// Raw format should include ALL entry types (user, assistant, tool_use, tool_result).
	if len(resp.Messages) != 4 {
		t.Fatalf("got %d raw messages, want 4 (all types included)", len(resp.Messages))
	}
}

func TestHandleSessionGetActivity(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	workDir := t.TempDir()
	info, err := mgr.Create(context.Background(), "myrig/worker", "Activity Test", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Write JSONL ending with end_turn → expect "idle".
	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"stop_reason\":\"end_turn\",\"content\":\"done\",\"model\":\"claude-opus-4-5-20251101\",\"usage\":{\"input_tokens\":1000}}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID, nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Activity != "idle" {
		t.Errorf("Activity = %q, want %q", resp.Activity, "idle")
	}
}

func TestFilterMetadataAllowlistsMCPrefix(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{
			name: "nil metadata",
			in:   nil,
			want: nil,
		},
		{
			name: "only internal keys",
			in:   map[string]string{"session_key": "abc", "command": "claude", "work_dir": "/tmp"},
			want: nil,
		},
		{
			name: "mc_ keys preserved",
			in:   map[string]string{"mc_session_kind": "agent", "mc_permission_mode": "plan", "session_key": "secret"},
			want: map[string]string{"mc_session_kind": "agent", "mc_permission_mode": "plan"},
		},
		{
			name: "mixed keys",
			in:   map[string]string{"mc_project_id": "proj-1", "quarantined_until": "2025-01-01", "held_until": "2025-01-02"},
			want: map[string]string{"mc_project_id": "proj-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterMetadata(tt.in)
			if tt.want == nil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("got %d keys, want %d: %v", len(got), len(tt.want), got)
				return
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestHandleSessionGetMetadataFiltered(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")

	// Set metadata with both mc_ and internal keys.
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"mc_project_id":  "proj-1",
		"session_key":    "secret-key",
		"command":        "claude --skip",
		"work_dir":       "/private/dir",
		"sleep_reason":   "",
		"mc_custom_mode": "plan",
	}); err != nil {
		t.Fatalf("set metadata: %v", err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID, nil)
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Only mc_ prefixed keys should be present.
	if len(resp.Metadata) != 2 {
		t.Fatalf("got %d metadata keys, want 2: %v", len(resp.Metadata), resp.Metadata)
	}
	if resp.Metadata["mc_project_id"] != "proj-1" {
		t.Errorf("mc_project_id = %q, want %q", resp.Metadata["mc_project_id"], "proj-1")
	}
	if resp.Metadata["mc_custom_mode"] != "plan" {
		t.Errorf("mc_custom_mode = %q, want %q", resp.Metadata["mc_custom_mode"], "plan")
	}
	// Internal keys must NOT be present.
	if _, ok := resp.Metadata["session_key"]; ok {
		t.Error("session_key should not be exposed in API response")
	}
	if _, ok := resp.Metadata["command"]; ok {
		t.Error("command should not be exposed in API response")
	}
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gorilla/websocket"
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

// wsSetup creates a Server, httptest.Server, and WS connection with hello drained.
func wsSetup(t *testing.T, fs *fakeState) (*Server, *httptest.Server, *websocket.Conn) {
	t.Helper()
	srv := New(fs)
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	t.Cleanup(func() { conn.Close() })
	drainWSHello(t, conn)
	return srv, ts, conn
}

// wsSetupSrv creates a Server with custom setup, then httptest.Server + WS conn.
func wsSetupSrv(t *testing.T, srv *Server) *websocket.Conn {
	t.Helper()
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	t.Cleanup(func() { conn.Close() })
	drainWSHello(t, conn)
	return conn
}

// ---- sessions.list tests ----

func TestHandleSessionList(t *testing.T) {
	fs := newSessionFakeState(t)
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session A")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session B")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "sessions.list",
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body listResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Total != 2 {
		t.Errorf("got total %d, want 2", body.Total)
	}
}

func TestHandleSessionListFilterByState(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Suspend")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Stay Active")
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "sessions.list",
		Payload: map[string]any{
			"state": "active",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	var body listResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Total != 1 {
		t.Errorf("got total %d, want 1 (only active)", body.Total)
	}
}

func TestHandleSessionListPagination(t *testing.T) {
	fs := newSessionFakeState(t)
	createTestSession(t, fs.cityBeadStore, fs.sp, "S1")
	createTestSession(t, fs.cityBeadStore, fs.sp, "S2")
	createTestSession(t, fs.cityBeadStore, fs.sp, "S3")
	_, ts, conn := wsSetup(t, fs)

	// Limit without cursor truncates but returns no next_cursor.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "sessions.list",
		Payload: map[string]any{
			"limit": 2,
		},
	})
	var resp1 wsResponseEnvelope
	readWSJSON(t, conn, &resp1)
	var page1 listResponse
	if err := json.Unmarshal(resp1.Result, &page1); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	items1, _ := page1.Items.([]any)
	if len(items1) != 2 {
		t.Errorf("limit-only: got %d items, want 2", len(items1))
	}
	if page1.NextCursor != "" {
		t.Errorf("limit-only: got next_cursor %q, want empty (no cursor mode)", page1.NextCursor)
	}

	// Use a fresh connection for cursor-mode paging.
	conn2 := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn2.Close()
	drainWSHello(t, conn2)

	// Cursor mode: first page. Use encodeCursor(0) to trigger paging mode
	// (WS requires non-empty cursor to activate paging, unlike HTTP ?cursor=).
	writeWSJSON(t, conn2, wsRequestEnvelope{
		Type:   "request",
		ID:     "t2",
		Action: "sessions.list",
		Payload: map[string]any{
			"cursor": encodeCursor(0),
			"limit":  2,
		},
	})
	var resp2 wsResponseEnvelope
	readWSJSON(t, conn2, &resp2)
	if resp2.Type != "response" || resp2.ID != "t2" {
		t.Fatalf("page1 resp = %#v, want correlated response", resp2)
	}
	var page2 listResponse
	if err := json.Unmarshal(resp2.Result, &page2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	items2, _ := page2.Items.([]any)
	if len(items2) != 2 {
		t.Errorf("page1: got %d items, want 2", len(items2))
	}
	if page2.Total != 3 {
		t.Errorf("page1: total = %d, want 3", page2.Total)
	}
	if page2.NextCursor == "" {
		t.Fatal("page1: expected next_cursor, got empty")
	}

	// Cursor mode: second page.
	writeWSJSON(t, conn2, wsRequestEnvelope{
		Type:   "request",
		ID:     "t3",
		Action: "sessions.list",
		Payload: map[string]any{
			"cursor": page2.NextCursor,
			"limit":  2,
		},
	})
	var resp3 wsResponseEnvelope
	readWSJSON(t, conn2, &resp3)
	var page3 listResponse
	if err := json.Unmarshal(resp3.Result, &page3); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	items3, _ := page3.Items.([]any)
	if len(items3) != 1 {
		t.Errorf("page2: got %d items, want 1", len(items3))
	}
	if page3.NextCursor != "" {
		t.Errorf("page2: got next_cursor %q, want empty (last page)", page3.NextCursor)
	}
}

// ---- session.get tests ----

func TestHandleSessionGet(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.get",
		Payload: map[string]any{
			"id": info.ID,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.ID != info.ID {
		t.Errorf("got ID %q, want %q", body.ID, info.ID)
	}
	if body.Title != "My Session" {
		t.Errorf("got title %q, want %q", body.Title, "My Session")
	}
	if body.State != "active" {
		t.Errorf("got state %q, want %q", body.State, "active")
	}
	if !body.Running {
		t.Errorf("got running=%v, want true", body.Running)
	}
}

func TestHandleSessionGetNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.get",
		Payload: map[string]any{
			"id": "nonexistent",
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "not_found" {
		t.Fatalf("error code = %q, want not_found", resp.Code)
	}
}

// ---- session.suspend / session.close / session.wake tests ----

func TestHandleSessionSuspend(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Suspend")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.suspend",
		Payload: map[string]any{
			"id": info.ID,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != session.StateSuspended {
		t.Errorf("got state %q, want %q", got.State, session.StateSuspended)
	}
}

func TestHandleSessionClose(t *testing.T) {
	fs := newSessionFakeState(t)
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
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.close",
		Payload: map[string]any{
			"id": info.ID,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

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
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.wake",
		Payload: map[string]any{
			"id": info.ID,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
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
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "sessions.list",
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "unavailable" {
		t.Fatalf("error code = %q, want unavailable", resp.Code)
	}
}

func TestHandleSessionWake(t *testing.T) {
	fs := newSessionFakeState(t)
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
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"held_until":   "9999-12-31T23:59:59Z",
		"wait_hold":    "true",
		"sleep_intent": "wait-hold",
		"sleep_reason": "wait-hold",
	})
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.wake",
		Payload: map[string]any{
			"id": info.ID,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}

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
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Closed Session")
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	_ = mgr.Close(info.ID)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.wake",
		Payload: map[string]any{
			"id": info.ID,
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "conflict" {
		t.Fatalf("error code = %q, want conflict", resp.Code)
	}
}

// ---- session.get by alias ----

func TestHandleSessionGetByTemplateName(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Named Session")
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"alias": "overseer",
	})
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.get",
		Payload: map[string]any{
			"id": "overseer",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.ID != info.ID {
		t.Errorf("got ID %q, want %q", body.ID, info.ID)
	}
}

// ---- session.patch tests ----

func TestHandleSessionPatchTitle(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")
	_, _, conn := wsSetup(t, fs)

	title := "Updated Title"
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.patch",
		Payload: map[string]any{
			"id":    info.ID,
			"title": title,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Title != "Updated Title" {
		t.Errorf("got title %q, want %q", body.Title, "Updated Title")
	}
}

func TestHandleSessionPatchAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")
	_, _, conn := wsSetup(t, fs)

	alias := "mayor"
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.patch",
		Payload: map[string]any{
			"id":    info.ID,
			"alias": alias,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Alias != "mayor" {
		t.Errorf("got alias %q, want %q", body.Alias, "mayor")
	}
}

func TestHandleSessionPatchAliasRejectsManagedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"agent_name": "mayor",
	}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	_, _, conn := wsSetup(t, fs)

	alias := "new-mayor"
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.patch",
		Payload: map[string]any{
			"id":    info.ID,
			"alias": alias,
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "forbidden" {
		t.Fatalf("error code = %q, want forbidden", resp.Code)
	}
}

func TestHandleSessionPatchRejectsReservedQualifiedAliasOnFork(t *testing.T) {
	fs := newSessionFakeState(t)
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
	_, _, conn := wsSetup(t, fs)

	alias := "myrig/worker"
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.patch",
		Payload: map[string]any{
			"id":    info.ID,
			"alias": alias,
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "conflict" {
		t.Fatalf("error code = %q, want conflict", resp.Code)
	}
}

func TestHandleSessionPatchImmutableField(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")
	_, _, conn := wsSetup(t, fs)

	// session.patch only accepts title and alias. "template" should be rejected.
	// The WS payload struct only has Title and Alias pointers, so "template" would
	// be ignored. However, the patch logic rejects empty patches. So we test that
	// passing neither title nor alias returns an error.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.patch",
		Payload: map[string]any{
			"id": info.ID,
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "invalid" {
		t.Fatalf("error code = %q, want invalid", resp.Code)
	}
}

// ---- sessions.list enrichment tests ----

func TestHandleSessionListIncludesReason(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Held")
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"sleep_reason": "user-hold",
	})
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "sessions.list",
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	var raw struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(resp.Result, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
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

// ---- session.rename tests ----

func TestHandleSessionRename(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.rename",
		Payload: map[string]any{
			"id":    info.ID,
			"title": "Renamed",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Title != "Renamed" {
		t.Errorf("got title %q, want %q", body.Title, "Renamed")
	}
}

func TestHandleSessionRenameEmptyTitle(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.rename",
		Payload: map[string]any{
			"id":    info.ID,
			"title": "",
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
}

// ---- session.get ambiguity tests ----

func TestHandleSessionAmbiguousAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	info1 := createTestSession(t, fs.cityBeadStore, fs.sp, "Worker 1")
	info2 := createTestSession(t, fs.cityBeadStore, fs.sp, "Worker 2")
	_ = fs.cityBeadStore.SetMetadataBatch(info1.ID, map[string]string{"alias": "worker"})
	_ = fs.cityBeadStore.SetMetadataBatch(info2.ID, map[string]string{"alias": "worker"})
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.get",
		Payload: map[string]any{
			"id": "worker",
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "ambiguous" {
		t.Fatalf("error code = %q, want ambiguous", resp.Code)
	}
}

// ---- session.get enrichment ----

func TestHandleSessionGetEnrichment(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Enriched Session")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.get",
		Payload: map[string]any{
			"id": info.ID,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.Running {
		t.Error("running = false, want true for active session")
	}
	if body.DisplayName != "Test" {
		t.Errorf("display_name = %q, want %q", body.DisplayName, "Test")
	}
}

func TestHandleSessionListPeek(t *testing.T) {
	fs := newSessionFakeState(t)
	createTestSession(t, fs.cityBeadStore, fs.sp, "Peek Session")
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "sessions.list",
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	var body struct {
		Items []sessionResponse `json:"items"`
	}
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Items) == 0 {
		t.Fatal("no sessions returned")
	}
	if body.Items[0].LastOutput != "" {
		t.Errorf("last_output = %q without peek param, want empty", body.Items[0].LastOutput)
	}
}

// ---- session.create tests ----

func TestHandleSessionCreate(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind": "agent",
			"name": "myrig/worker",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Template != "myrig/worker" {
		t.Errorf("Template = %q, want %q", body.Template, "myrig/worker")
	}
	if body.Title != "myrig/worker" {
		t.Errorf("Title = %q, want default %q", body.Title, "myrig/worker")
	}
	if body.Running {
		t.Errorf("Running = %v, want false for async create", body.Running)
	}
	if body.DisplayName != "Test Agent" {
		t.Errorf("DisplayName = %q, want %q", body.DisplayName, "Test Agent")
	}
}

func TestHandleSessionCreateAsync(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":  "agent",
			"name":  "myrig/worker",
			"alias": "sky",
			"async": true,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.State != "creating" {
		t.Fatalf("State = %q, want %q", body.State, "creating")
	}
	if body.Running {
		t.Fatalf("Running = true, want false for async create")
	}
	if body.Alias != "sky" {
		t.Fatalf("Alias = %q, want %q", body.Alias, "sky")
	}
	if fs.pokeCount != 1 {
		t.Fatalf("pokeCount = %d, want 1", fs.pokeCount)
	}
}

func TestHandleSessionCreateAsyncAcceptsInlineMessage(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":    "agent",
			"name":    "myrig/worker",
			"async":   true,
			"message": "hello",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
}

func TestHandleProviderSessionCreateRejectsAsync(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":  "provider",
			"name":  "test-agent",
			"async": true,
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if !strings.Contains(resp.Message, "async session creation is only supported for configured agent templates") {
		t.Fatalf("message = %q, want provider async guidance", resp.Message)
	}
	if fs.pokeCount != 0 {
		t.Fatalf("pokeCount = %d, want 0", fs.pokeCount)
	}
}

func TestHandleProviderSessionCreateWithMessageUsesProviderDefaultNudge(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":    "provider",
			"name":    "test-agent",
			"message": "hello",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.ID == "" {
		t.Fatal("response missing id")
	}
	if body.SessionName == "" {
		t.Fatal("response missing session_name")
	}
	nudgeCount := 0
	for _, call := range fs.sp.Calls {
		if call.Name != body.SessionName || call.Message != "hello" {
			continue
		}
		if call.Method == "Nudge" {
			nudgeCount++
		}
	}
	if nudgeCount != 1 {
		t.Fatalf("Nudge count for %q = %d, want 1; calls=%#v", body.SessionName, nudgeCount, fs.sp.Calls)
	}
}

func TestHandleSessionCreatePersistsAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":  "agent",
			"name":  "myrig/worker",
			"alias": "sky",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Alias != "sky" {
		t.Fatalf("Alias = %q, want sky", body.Alias)
	}
	if body.SessionName == "sky" {
		t.Fatalf("SessionName = %q, want bead-derived runtime name", body.SessionName)
	}
}

func TestHandleSessionCreateRejectsReservedQualifiedAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":  "agent",
			"name":  "myrig/worker",
			"alias": "myrig/worker",
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "conflict" {
		t.Fatalf("error code = %q, want conflict", resp.Code)
	}
}

func TestHandleProviderSessionCreateRejectsReservedQualifiedAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":  "provider",
			"name":  "test-agent",
			"alias": "myrig/worker",
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "conflict" {
		t.Fatalf("error code = %q, want conflict", resp.Code)
	}
}

func TestHandleSessionCreateRejectsInvalidAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":  "agent",
			"name":  "myrig/worker",
			"alias": "bad:name",
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "invalid" {
		t.Fatalf("error code = %q, want invalid", resp.Code)
	}
}

func TestHandleSessionCreateRejectsLegacySessionNameField(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	legacyName := "mayor"
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":         "agent",
			"name":         "myrig/worker",
			"session_name": &legacyName,
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if !strings.Contains(resp.Message, "use alias") {
		t.Fatalf("message = %q, want use alias guidance", resp.Message)
	}
}

func TestHandleSessionCreateRejectsEmptyLegacySessionNameField(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	emptyName := ""
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":         "agent",
			"name":         "myrig/worker",
			"session_name": &emptyName,
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if !strings.Contains(resp.Message, "use alias") {
		t.Fatalf("message = %q, want use alias guidance", resp.Message)
	}
}

func TestHandleSessionCreateRejectsDuplicateAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	// First create succeeds.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":  "agent",
			"name":  "myrig/worker",
			"alias": "sky",
		},
	})
	var resp1 wsResponseEnvelope
	readWSJSON(t, conn, &resp1)
	if resp1.Type != "response" {
		t.Fatalf("first create response type = %q, want response", resp1.Type)
	}

	// Second create with same alias conflicts.
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t2",
		Action: "session.create",
		Payload: map[string]any{
			"kind":  "agent",
			"name":  "myrig/worker",
			"alias": "sky",
		},
	})
	var resp2 wsErrorEnvelope
	readWSJSON(t, conn, &resp2)
	if resp2.Type != "error" {
		t.Fatalf("response type = %q, want error", resp2.Type)
	}
	if resp2.Code != "conflict" {
		t.Fatalf("error code = %q, want conflict", resp2.Code)
	}
}

func TestHandleSessionCreateCanonicalizesBareTemplate(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind": "agent",
			"name": "worker",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Template != "myrig/worker" {
		t.Errorf("Template = %q, want %q", body.Template, "myrig/worker")
	}
	if body.Title != "myrig/worker" {
		t.Errorf("Title = %q, want %q", body.Title, "myrig/worker")
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
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind": "agent",
			"name": "myrig/worker",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, err := fs.cityBeadStore.Get(body.ID)
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
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":    "agent",
			"name":    "myrig/worker",
			"options": map[string]string{"effort": "high"},
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, err := fs.cityBeadStore.Get(body.ID)
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
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":    "agent",
			"name":    "myrig/worker",
			"options": map[string]string{"permission_mode": "plan", "effort": "low"},
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, err := fs.cityBeadStore.Get(body.ID)
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
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.create",
		Payload: map[string]any{
			"kind":    "agent",
			"name":    "myrig/worker",
			"message": "Hello from Discord!",
			"options": map[string]string{"effort": "high"},
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, err := fs.cityBeadStore.Get(body.ID)
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

// ---- session.messages tests ----

func TestHandleSessionMessageResumesSuspendedSessionUsingProviderDefaultNudge(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Resume Me")
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.messages",
		Payload: map[string]any{
			"id":      info.ID,
			"message": "hello",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	if !fs.sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after session.messages")
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
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.messages",
		Payload: map[string]any{
			"id":      "worker",
			"message": "hello",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body map[string]string
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	id := body["id"]
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
		t.Fatalf("session %q should be running after session.messages", sessionName)
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

// ---- Internal method tests (no HTTP/WS transport) ----

func TestResolveSessionIDMaterializingNamedWithContext_RollsBackCanceledCreate(t *testing.T) {
	fs := newSessionFakeState(t)
	provider := &cancelStartProvider{Fake: runtime.NewFake()}
	srv := New(&stateWithSessionProvider{fakeState: fs, provider: provider})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := srv.resolveSessionIDMaterializingNamedWithContext(ctx, fs.cityBeadStore, "worker")
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

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.get",
		Payload: map[string]any{
			"id": id,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.ConfiguredNamedSession {
		t.Fatal("ConfiguredNamedSession = false, want true")
	}
}

func TestHandleSessionMessageInvalidNamedTargetDoesNotMaterialize(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.messages",
		Payload: map[string]any{
			"id":      "worker",
			"message": "   ",
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
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
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.get",
		Payload: map[string]any{
			"id": "worker",
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "not_found" {
		t.Fatalf("error code = %q, want not_found", resp.Code)
	}
}

func TestHandleSessionCloseRejectsAlwaysNamedSession(t *testing.T) {
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

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	conn := dialWebSocket(t, ts.URL+"/v0/ws")
	defer conn.Close()
	drainWSHello(t, conn)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.close",
		Payload: map[string]any{
			"id": id,
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "conflict" {
		t.Fatalf("error code = %q, want conflict", resp.Code)
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

	if _, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker"); err == nil || ok {
		t.Fatalf("findNamedSessionSpecForTarget(worker) = ok=%v err=%v, want ambiguous error", ok, err)
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

	id, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "worker")
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

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
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

	id, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker): %v", err)
	}
	if id != bead.ID {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker) = %q, want adopted bead %q", id, bead.ID)
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

	id, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "worker")
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

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
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

	_, err = srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "worker")
	if err == nil || !strings.Contains(err.Error(), "conflicts with configured named session") {
		t.Fatalf("resolveSessionIDMaterializingNamed(worker) error = %v, want configured named session conflict", err)
	}
}

// ---- session.wake materialization tests ----

func TestHandleSessionWakeMaterializesNamedSessionAndStartsRuntime(t *testing.T) {
	fs := newSessionFakeState(t)
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.wake",
		Payload: map[string]any{
			"id": "worker",
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body map[string]string
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	id := body["id"]
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
		t.Fatalf("session %q should be running after session.wake", sessionName)
	}
}

func TestHandleSessionWakeCanceledNamedCreateRollsBack(t *testing.T) {
	fs := newSessionFakeState(t)
	provider := &cancelStartProvider{Fake: runtime.NewFake()}
	srv := New(&stateWithSessionProvider{fakeState: fs, provider: provider})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// This test exercises internal method behavior with a canceled context.
	// The WS transport always uses context.Background(), so we test the underlying
	// method directly.
	_, err := srv.wakeSessionTarget(ctx, "worker")
	if err == nil {
		t.Fatal("wakeSessionTarget: expected error, got nil")
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

// ---- session.transcript tests ----

func TestHandleSessionTranscriptUsesSessionKey(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	searchBase := t.TempDir()
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

	conn := wsSetupSrv(t, srv)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.transcript",
		Payload: map[string]any{
			"id": info.ID,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "response" || resp.ID != "t1" {
		t.Fatalf("response = %#v, want correlated response", resp)
	}
	var body sessionTranscriptResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Format != "conversation" {
		t.Errorf("Format = %q, want %q", body.Format, "conversation")
	}
	if len(body.Turns) != 2 || body.Turns[1].Text != "world" {
		t.Fatalf("Turns = %+v, want hello/world from session key file", body.Turns)
	}
}

func TestHandleSessionTranscriptClosedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	searchBase := t.TempDir()
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

	conn := wsSetupSrv(t, srv)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.transcript",
		Payload: map[string]any{
			"id":    info.ID,
			"turns": 0,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	var body sessionTranscriptResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Turns) != 2 || body.Turns[0].Text != "hello" || body.Turns[1].Text != "world" {
		t.Fatalf("Turns = %+v, want closed-session transcript hello/world", body.Turns)
	}
}

// ---- session.pending and session.respond tests ----

func TestHandleSessionPendingAndRespond(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	_, _, conn := wsSetup(t, fs)

	// pending
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.pending",
		Payload: map[string]any{
			"id": info.ID,
		},
	})
	var pendingResp wsResponseEnvelope
	readWSJSON(t, conn, &pendingResp)
	var pendingBody sessionPendingResponse
	if err := json.Unmarshal(pendingResp.Result, &pendingBody); err != nil {
		t.Fatalf("unmarshal pending: %v", err)
	}
	if !pendingBody.Supported || pendingBody.Pending == nil || pendingBody.Pending.RequestID != "req-1" {
		t.Fatalf("pending response = %#v, want req-1", pendingBody)
	}

	// respond
	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t2",
		Action: "session.respond",
		Payload: map[string]any{
			"id":     info.ID,
			"action": "approve",
		},
	})
	var respondResp wsResponseEnvelope
	readWSJSON(t, conn, &respondResp)
	if respondResp.Type != "response" || respondResp.ID != "t2" {
		t.Fatalf("respond response = %#v, want correlated response", respondResp)
	}
	if got := fs.sp.Responses[info.SessionName]; len(got) != 1 || got[0].Action != "approve" {
		t.Fatalf("responses = %#v, want single approve", got)
	}
}

func TestHandleSessionMessageRejectsPendingInteraction(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.messages",
		Payload: map[string]any{
			"id":      info.ID,
			"message": "hello",
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "conflict" {
		t.Fatalf("error code = %q, want conflict", resp.Code)
	}
	if !strings.Contains(resp.Message, "pending interaction") {
		t.Fatalf("message = %s, want pending interaction error", resp.Message)
	}
	for _, call := range fs.sp.Calls {
		if (call.Method == "Nudge" || call.Method == "NudgeNow") && call.Name == info.SessionName {
			t.Fatalf("unexpected nudge while pending interaction is active: %#v", fs.sp.Calls)
		}
	}
}

func TestHandleSessionMessageRejectsClosedNamedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "myrig/worker", "Sky", "claude", t.TempDir(), "claude", "", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("CreateNamedWithTransport: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.messages",
		Payload: map[string]any{
			"id":      "sky",
			"message": "hello",
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "not_found" {
		t.Fatalf("error code = %q, want not_found", resp.Code)
	}
}

func TestHandleSessionRespondMismatchedRequest(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.respond",
		Payload: map[string]any{
			"id":         info.ID,
			"request_id": "req-2",
			"action":     "approve",
		},
	})
	var resp wsErrorEnvelope
	readWSJSON(t, conn, &resp)
	if resp.Type != "error" {
		t.Fatalf("response type = %q, want error", resp.Type)
	}
	if resp.Code != "conflict" {
		t.Fatalf("error code = %q, want conflict", resp.Code)
	}
}

func TestHandleSessionTranscriptRawIncludesAllTypes(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	searchBase := t.TempDir()
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
		`{"uuid":"3","parentUuid":"2","type":"tool_use","message":"{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"name\":\"read\"}]}","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"tool_result","message":"{\"role\":\"tool\",\"content\":\"file contents\"}","timestamp":"2025-01-01T00:00:03Z"}`,
	)

	conn := wsSetupSrv(t, srv)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.transcript",
		Payload: map[string]any{
			"id":     info.ID,
			"format": "raw",
			"turns":  0,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	var body sessionRawTranscriptResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Format != "raw" {
		t.Errorf("Format = %q, want %q", body.Format, "raw")
	}
	if len(body.Messages) != 4 {
		t.Fatalf("got %d raw messages, want 4 (all types included)", len(body.Messages))
	}
}

func TestHandleSessionGetActivity(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	searchBase := t.TempDir()
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

	writeNamedSessionJSONL(t, searchBase, workDir, info.SessionKey+".jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"stop_reason\":\"end_turn\",\"content\":\"done\",\"model\":\"claude-opus-4-5-20251101\",\"usage\":{\"input_tokens\":1000}}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	conn := wsSetupSrv(t, srv)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.get",
		Payload: map[string]any{
			"id": info.ID,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Activity != "idle" {
		t.Errorf("Activity = %q, want %q", body.Activity, "idle")
	}
}

// ---- Pure unit tests (no transport) ----

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
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")
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
	_, _, conn := wsSetup(t, fs)

	writeWSJSON(t, conn, wsRequestEnvelope{
		Type:   "request",
		ID:     "t1",
		Action: "session.get",
		Payload: map[string]any{
			"id": info.ID,
		},
	})
	var resp wsResponseEnvelope
	readWSJSON(t, conn, &resp)
	var body sessionResponse
	if err := json.Unmarshal(resp.Result, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Metadata) != 2 {
		t.Fatalf("got %d metadata keys, want 2: %v", len(body.Metadata), body.Metadata)
	}
	if body.Metadata["mc_project_id"] != "proj-1" {
		t.Errorf("mc_project_id = %q, want %q", body.Metadata["mc_project_id"], "proj-1")
	}
	if body.Metadata["mc_custom_mode"] != "plan" {
		t.Errorf("mc_custom_mode = %q, want %q", body.Metadata["mc_custom_mode"], "plan")
	}
	if _, ok := body.Metadata["session_key"]; ok {
		t.Error("session_key should not be exposed in API response")
	}
	if _, ok := body.Metadata["command"]; ok {
		t.Error("command should not be exposed in API response")
	}
}

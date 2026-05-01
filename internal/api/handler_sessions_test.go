package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
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

type cachedOnlyListStoreForSessionTest struct {
	*beads.MemStore
	blockList bool
	listCalls int
}

func (s *cachedOnlyListStoreForSessionTest) List(query beads.ListQuery) ([]beads.Bead, error) {
	if s.blockList {
		s.listCalls++
		return nil, errors.New("backing List should not be used")
	}
	return s.MemStore.List(query)
}

func (s *cachedOnlyListStoreForSessionTest) CachedList(query beads.ListQuery) ([]beads.Bead, bool) {
	rows, err := s.MemStore.List(query)
	if err != nil {
		return nil, false
	}
	return rows, true
}

type partialPrimeSessionStore struct {
	*beads.MemStore
	partialRows    []beads.Bead
	labelListCalls int
}

func (s *partialPrimeSessionStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(query)
	if err != nil {
		return nil, err
	}
	if query.AllowScan || query.Label == session.LabelSession {
		if query.Label == session.LabelSession {
			s.labelListCalls++
		}
		if s.partialRows != nil {
			rows = append([]beads.Bead(nil), s.partialRows...)
		}
		return rows, &beads.PartialResultError{
			Op:  "bd list",
			Err: errors.New("skipped 1 corrupt bead"),
		}
	}
	return rows, nil
}

func TestListSessionBeadsForReadModelFallsBackAfterPartialCachePrime(t *testing.T) {
	t.Parallel()

	backing := &partialPrimeSessionStore{MemStore: beads.NewMemStore()}
	survivor, err := backing.Create(beads.Bead{
		Title:  "session survivor",
		Labels: []string{session.LabelSession},
	})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	if _, err := backing.Create(beads.Bead{
		Title:  "dropped session",
		Labels: []string{session.LabelSession},
	}); err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	backing.partialRows = []beads.Bead{survivor}

	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	rows, err := listSessionBeadsForReadModel(cache)
	var partial *beads.PartialResultError
	if !errors.As(err, &partial) {
		t.Fatalf("listSessionBeadsForReadModel error = %v, want *PartialResultError", err)
	}
	if backing.labelListCalls != 1 {
		t.Fatalf("label List calls = %d, want 1 backing fallback after partial prime", backing.labelListCalls)
	}
	if len(rows) != 1 || rows[0].ID != survivor.ID {
		t.Fatalf("rows = %+v, want partial survivor %s", rows, survivor.ID)
	}
}

func TestHandleSessionListPreservesPartialRows(t *testing.T) {
	fs := newSessionFakeState(t)
	store := &partialPrimeSessionStore{MemStore: beads.NewMemStore()}
	fs.cityBeadStore = store
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, store, fs.sp, "Session survivor")
	survivor, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	store.partialRows = []beads.Bead{survivor}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Items         []sessionResponse `json:"items"`
		Total         int               `json:"total"`
		Partial       bool              `json:"partial"`
		PartialErrors []string          `json:"partial_errors"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Partial {
		t.Fatal("partial = false, want true")
	}
	if len(body.PartialErrors) == 0 {
		t.Fatal("partial_errors empty")
	}
	if body.Total != 1 || len(body.Items) != 1 || body.Items[0].ID != info.ID {
		t.Fatalf("body = %+v, want surviving session %s", body, info.ID)
	}
}

func writeGeminiHistoryFixtureForAPI(t *testing.T, path, sessionID string, messages ...string) {
	t.Helper()

	body := fmt.Sprintf("{\n  \"sessionId\": %q,\n  \"messages\": [\n    %s\n  ]\n}\n", sessionID, strings.Join(messages, ",\n    "))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write gemini transcript %s: %v", path, err)
	}
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

type failNudgeProvider struct {
	*runtime.Fake
	err error
}

func (p *failNudgeProvider) Nudge(name string, content []runtime.ContentBlock) error {
	if err := p.Fake.Nudge(name, content); err != nil {
		return err
	}
	if p.err != nil {
		return p.err
	}
	return nil
}

type transportCapableProvider struct {
	*runtime.Fake
}

func (p *transportCapableProvider) SupportsTransport(transport string) bool {
	return transport == "acp"
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

type syncResponseRecorder struct {
	*httptest.ResponseRecorder
	mu sync.Mutex
}

func newSyncResponseRecorder() *syncResponseRecorder {
	return &syncResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *syncResponseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ResponseRecorder.Write(p)
}

func (r *syncResponseRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ResponseRecorder.WriteHeader(code)
}

func (r *syncResponseRecorder) WriteString(s string) (int, error) {
	return r.Write([]byte(s))
}

func (r *syncResponseRecorder) BodyString() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Body.String()
}

func waitForRecorderSubstring(t *testing.T, rec *syncResponseRecorder, want string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		body := rec.BodyString()
		if strings.Contains(body, want) {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	return rec.BodyString()
}

func TestHandleSessionList(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	// Create two sessions.
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session A")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Session B")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(w, r)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Suspend")
	createTestSession(t, fs.cityBeadStore, fs.sp, "Stay Active")

	// Suspend one.
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	// List only active.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions?state=active"), nil)
	h.ServeHTTP(w, r)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	// Create 3 sessions.
	createTestSession(t, fs.cityBeadStore, fs.sp, "S1")
	createTestSession(t, fs.cityBeadStore, fs.sp, "S2")
	createTestSession(t, fs.cityBeadStore, fs.sp, "S3")

	// Limit without cursor truncates but returns no next_cursor.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions?limit=2"), nil)
	h.ServeHTTP(w, r)
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
	r = httptest.NewRequest("GET", cityURL(fs, "/sessions?cursor=&limit=2"), nil)
	h.ServeHTTP(w, r)
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
	r = httptest.NewRequest("GET", cityURL(fs, "/sessions?cursor=")+page1.NextCursor+"&limit=2", nil)
	h.ServeHTTP(w, r)
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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	h.ServeHTTP(w, r)

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

func TestHandleSessionListActiveBeadUsesCachedLookup(t *testing.T) {
	fs := newSessionFakeState(t)
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, nil)
	fs.stores["myrig"] = cache
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")
	work, err := backing.Create(beads.Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	assignee := info.ID
	if err := backing.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	reassigned := "other-session"
	if err := backing.Update(work.ID, beads.UpdateOpts{Assignee: &reassigned}); err != nil {
		t.Fatalf("reassign backing work: %v", err)
	}

	resp := sessionResponse{}
	srv.enrichSessionResponse(&resp, info, fs.Config(), sessionResponseCapabilityHandle{
		state: worker.State{Phase: worker.PhaseReady},
	}, false, false, false)

	if !resp.Running {
		t.Fatal("Running = false, want true")
	}
	if got := resp.ActiveBead; got != work.ID {
		t.Fatalf("active_bead = %q, want cached %q", got, work.ID)
	}
}

func TestHandleSessionListUsesCachedSessionBeadsWhenAvailable(t *testing.T) {
	fs := newSessionFakeState(t)
	store := &cachedOnlyListStoreForSessionTest{MemStore: beads.NewMemStore()}
	fs.cityBeadStore = store
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")
	store.blockList = true

	req := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []sessionResponse `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 || resp.Items[0].ID != info.ID {
		t.Fatalf("response = %#v, want one session %s", resp, info.ID)
	}
	if store.listCalls != 0 {
		t.Fatalf("backing List calls = %d, want 0", store.listCalls)
	}
}

func TestHandleSessionListSkipsWorkdirOnlyCodexTranscriptDiscovery(t *testing.T) {
	fs := newSessionFakeState(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex", "sessions"), 0o755); err != nil {
		t.Fatalf("MkdirAll default codex sessions: %v", err)
	}
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}
	h := newTestCityHandlerWith(t, fs, srv)

	workDir := t.TempDir()
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(context.Background(), "myrig/worker", "Codex Chat", "codex", workDir, "codex-max", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.SessionKey != "" {
		t.Fatalf("SessionKey = %q, want empty for codex provider without SessionIDFlag", info.SessionKey)
	}

	codexDir := filepath.Join(searchBase, "2026", "04", "18")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	codexPayload := strings.Join([]string{
		fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`, workDir),
		`{"type":"assistant","message":{"model":"gpt-5.5","usage":{"input_tokens":1000}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(codexDir, "session.jsonl"), []byte(codexPayload), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	req := httptest.NewRequest("GET", cityURL(fs, "/sessions?template=myrig%2Fworker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp struct {
		Items []sessionResponse `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != info.ID {
		t.Fatalf("items = %#v, want session %s", resp.Items, info.ID)
	}
	if resp.Items[0].Model != "" || resp.Items[0].ContextPct != nil {
		t.Fatalf("session list used workdir-only Codex transcript discovery: model=%q context=%v", resp.Items[0].Model, resp.Items[0].ContextPct)
	}
}

func TestHandleSessionGetAllowsWorkdirOnlyCodexTranscriptDiscovery(t *testing.T) {
	fs := newSessionFakeState(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex", "sessions"), 0o755); err != nil {
		t.Fatalf("MkdirAll default codex sessions: %v", err)
	}
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}
	h := newTestCityHandlerWith(t, fs, srv)

	workDir := t.TempDir()
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(context.Background(), "myrig/worker", "Codex Chat", "codex", workDir, "codex-max", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	codexDir := filepath.Join(searchBase, "2026", "04", "18")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	codexPayload := strings.Join([]string{
		fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`, workDir),
		`{"type":"assistant","message":{"model":"gpt-5.5","usage":{"input_tokens":1000}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(codexDir, "session.jsonl"), []byte(codexPayload), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != info.ID {
		t.Fatalf("ID = %q, want %q", resp.ID, info.ID)
	}
	if resp.Model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", resp.Model)
	}
}

func TestHandleSessionListActiveBeadUsesCachedListWhenAvailable(t *testing.T) {
	fs := newSessionFakeState(t)
	store := &cachedOnlyListStoreForSessionTest{MemStore: beads.NewMemStore(), blockList: true}
	fs.stores["myrig"] = store
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")
	work, err := store.Create(beads.Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	assignee := info.ID
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}

	resp := sessionResponse{}
	srv.enrichSessionResponse(&resp, info, fs.Config(), sessionResponseCapabilityHandle{
		state: worker.State{Phase: worker.PhaseReady},
	}, false, false, false)

	if got := resp.ActiveBead; got != work.ID {
		t.Fatalf("active_bead = %q, want cached %q", got, work.ID)
	}
	if store.listCalls != 0 {
		t.Fatalf("backing List calls = %d, want 0", store.listCalls)
	}
}

func TestHandleSessionGetActiveBeadUsesLiveLookup(t *testing.T) {
	fs := newSessionFakeState(t)
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, nil)
	fs.stores["myrig"] = cache
	srv := New(fs)

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "My Session")
	work, err := backing.Create(beads.Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	assignee := info.ID
	if err := backing.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	reassigned := "other-session"
	if err := backing.Update(work.ID, beads.UpdateOpts{Assignee: &reassigned}); err != nil {
		t.Fatalf("reassign backing work: %v", err)
	}

	resp := sessionResponse{}
	srv.enrichSessionResponse(&resp, info, fs.Config(), sessionResponseCapabilityHandle{
		state: worker.State{Phase: worker.PhaseReady},
	}, false, true, true)

	if !resp.Running {
		t.Fatal("Running = false, want true")
	}
	if got := resp.ActiveBead; got != "" {
		t.Fatalf("active_bead = %q, want empty after external reassignment", got)
	}
}

func TestHandleSessionGetNotFound(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/nonexistent"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleSessionSuspend(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Suspend")

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/suspend", nil)
	h.ServeHTTP(w, r)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "To Drain")

	// Drain the session directly via the manager (the API surface for drain
	// lives elsewhere; this test isolates the transition check).
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.BeginDrain(info.ID, "shutdown"); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/suspend", nil)
	h.ServeHTTP(w, r)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/close", nil)
	h.ServeHTTP(w, r)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/wake", nil)
	h.ServeHTTP(w, r)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleSessionWake(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/wake", nil)
	h.ServeHTTP(w, r)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Closed Session")
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	_ = mgr.Close(info.ID)

	w := httptest.NewRecorder()
	r := newPostRequest(cityURL(fs, "/session/")+info.ID+"/wake", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleSessionGetByTemplateName(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Named Session")

	// Set alias metadata on the bead so public resolution works.
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"alias": "overseer",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/overseer"), nil)
	h.ServeHTTP(w, r)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")

	body := `{"title":"Updated Title"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/session/")+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")

	body := `{"alias":"mayor"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/session/")+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")
	if err := fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"agent_name": "mayor",
	}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	body := `{"alias":"new-mayor"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/session/")+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusForbidden, w.Body.String())
	}
}

func TestHandleSessionPatchRejectsReservedQualifiedAliasOnFork(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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
	req := httptest.NewRequest("PATCH", cityURL(fs, "/session/")+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")

	body := `{"template":"hacked"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/session/")+info.ID, strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusUnprocessableEntity, w.Body.String())
	}
}

func TestHandleSessionListIncludesReason(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Held")

	// Set sleep reason on bead.
	_ = fs.cityBeadStore.SetMetadataBatch(info.ID, map[string]string{
		"sleep_reason": "user-hold",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(w, r)

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

func TestHandleSessionRename(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Original")

	body := `{"title":"Renamed"}`
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/rename", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Test")

	body := `{"title":""}`
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/rename", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusUnprocessableEntity, w.Body.String())
	}
}

func TestHandleSessionAmbiguousAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	// Create two sessions with the same public alias.
	info1 := createTestSession(t, fs.cityBeadStore, fs.sp, "Worker 1")
	info2 := createTestSession(t, fs.cityBeadStore, fs.sp, "Worker 2")
	_ = fs.cityBeadStore.SetMetadataBatch(info1.ID, map[string]string{"alias": "worker"})
	_ = fs.cityBeadStore.SetMetadataBatch(info2.ID, map[string]string{"alias": "worker"})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/worker"), nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d (ambiguous); body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleSessionGetEnrichment(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Enriched Session")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	h.ServeHTTP(w, r)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	createTestSession(t, fs.cityBeadStore, fs.sp, "Peek Session")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/sessions"), nil)
	h.ServeHTTP(w, r)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "sess-create-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	bead, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", resp.ID, err)
	}
	if got := bead.Metadata[session.MCPIdentityMetadataKey]; got != "" {
		t.Fatalf("mcp_identity = %q, want empty for non-ACP agent session", got)
	}
	if got := bead.Metadata[session.MCPServersSnapshotMetadataKey]; got != "" {
		t.Fatalf("mcp_servers_snapshot = %q, want empty for non-ACP agent session", got)
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

func TestHandleSessionCreateUsesACPTransportCommandForAgentTemplate(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  &transportCapableProvider{Fake: runtime.NewFake()},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	bead, err := state.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", resp.ID, err)
	}
	if got, want := bead.Metadata["command"], "/bin/echo acp"; got != want {
		t.Fatalf("command metadata = %q, want %q", got, want)
	}
	if got, want := bead.Metadata["transport"], "acp"; got != want {
		t.Fatalf("transport metadata = %q, want %q", got, want)
	}
}

func TestHumaHandleSessionCreateUsesACPTransportCommandForAgentTemplate(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  &transportCapableProvider{Fake: runtime.NewFake()},
	}
	srv := New(state)

	out, err := srv.humaHandleSessionCreate(context.Background(), &SessionCreateInput{
		Body: sessionCreateBody{
			Kind: "agent",
			Name: "myrig/worker",
		},
	})
	if err != nil {
		t.Fatalf("humaHandleSessionCreate: %v", err)
	}
	if got, want := out.Status, http.StatusAccepted; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	bead, err := state.cityBeadStore.Get(out.Body.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", out.Body.ID, err)
	}
	if got, want := bead.Metadata["command"], "/bin/echo acp"; got != want {
		t.Fatalf("command metadata = %q, want %q", got, want)
	}
	if got, want := bead.Metadata["transport"], "acp"; got != want {
		t.Fatalf("transport metadata = %q, want %q", got, want)
	}
}

func TestHandleSessionCreateRejectsACPAgentWithoutACPRouting(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "requires ACP transport") {
		t.Fatalf("body = %q, want ACP transport error", rec.Body.String())
	}
}

func TestHumaHandleSessionCreateRejectsACPAgentWithoutACPRouting(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	srv := New(fs)

	if _, err := srv.humaHandleSessionCreate(context.Background(), &SessionCreateInput{
		Body: sessionCreateBody{
			Kind: "agent",
			Name: "myrig/worker",
		},
	}); err == nil {
		t.Fatal("humaHandleSessionCreate() error = nil, want ACP routing error")
	} else if !strings.Contains(err.Error(), "requires ACP transport") {
		t.Fatalf("humaHandleSessionCreate() error = %v, want ACP transport error", err)
	}
}

func TestHandleSessionCreateRejectsACPAgentWhenProviderLacksACP(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "custom"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["custom"] = config.ProviderSpec{
		DisplayName: "Custom",
		Command:     "/bin/echo",
		PathCheck:   "true",
	}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  &transportCapableProvider{Fake: runtime.NewFake()},
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "does not support ACP transport") {
		t.Fatalf("body = %q, want provider ACP support error", rec.Body.String())
	}
}

func TestHumaHandleSessionCreatePropagatesMCPResolutionErrorForACPAgent(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "filesystem.toml"), []byte(`
name = "filesystem"
command = [broken
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  &transportCapableProvider{Fake: runtime.NewFake()},
	}
	srv := New(state)

	if _, err := srv.humaHandleSessionCreate(context.Background(), &SessionCreateInput{
		Body: sessionCreateBody{
			Kind: "agent",
			Name: "myrig/worker",
		},
	}); err == nil {
		t.Fatal("humaHandleSessionCreate() error = nil, want MCP resolution error")
	} else if !strings.Contains(err.Error(), "loading effective MCP") {
		t.Fatalf("humaHandleSessionCreate() error = %v, want MCP resolution error", err)
	}
}

func TestHandleSessionCreateIgnoresBrokenMCPWithoutACPTransport(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "filesystem.toml"), []byte(`
name = "filesystem"
command = [broken
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
}

func TestHandleSessionCreateAsync(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","alias":"sky","async":true}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	// Agent sessions are always async; messages are stored as initial_message
	// in template_overrides for the reconciler to pick up.
	body := `{"kind":"agent","name":"myrig/worker","async":true,"message":"hello"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandlerWith(t, fs, srv)

	for i := 0; i < 2; i++ {
		req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/ant"}`))
		req.Header.Set("Idempotency-Key", "pool-create-"+string(rune('a'+i)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
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

func TestResolveAgentCreateContextUsesConcreteIdentityForMCPMaterialization(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:              "ant",
		Dir:               "myrig",
		Provider:          "opencode",
		Session:           "acp",
		WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(4),
	}}
	fs.cfg.NamedSessions = nil
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}", "{{.WorkDir}}", "{{.TemplateName}}"]
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}

	srv := New(fs)
	createCtx, err := srv.resolveAgentCreateContext("myrig/ant", "")
	if err != nil {
		t.Fatalf("resolveAgentCreateContext: %v", err)
	}
	mcpServers, err := srv.sessionMCPServers("myrig/ant", "opencode", createCtx.Identity, createCtx.WorkDir, "acp", "agent")
	if err != nil {
		t.Fatalf("sessionMCPServers: %v", err)
	}
	if len(mcpServers) != 1 {
		t.Fatalf("len(mcpServers) = %d, want 1", len(mcpServers))
	}
	if got, want := mcpServers[0].Args[0], createCtx.Identity; got != want {
		t.Fatalf("Args[0] = %q, want %q", got, want)
	}
	if got, want := mcpServers[0].Args[1], createCtx.WorkDir; got != want {
		t.Fatalf("Args[1] = %q, want %q", got, want)
	}
	if got, want := mcpServers[0].Args[2], "myrig/ant"; got != want {
		t.Fatalf("Args[2] = %q, want %q", got, want)
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
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/ant","alias":"ant-fenrir"}`))
	req.Header.Set("Idempotency-Key", "pool-alias-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
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

	req = newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/ant","alias":"myrig/ant-fenrir"}`))
	req.Header.Set("Idempotency-Key", "pool-alias-2")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second create status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestHandleProviderSessionCreateRejectsAsync(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"provider","name":"test-agent","async":true}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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

func TestMaterializeNamedSessionStampsProviderFamilyMetadata(t *testing.T) {
	fs := newSessionFakeState(t)
	base := "builtin:claude"
	fs.cfg.Agents = []config.Agent{{
		Name:              "worker",
		Dir:               "myrig",
		Provider:          "claude-max",
		MaxActiveSessions: intPtr(1),
	}}
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"claude-max": {Base: &base},
	}
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "myrig/worker")
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
	if got := bead.Metadata["provider"]; got != "claude-max" {
		t.Fatalf("provider = %q, want claude-max", got)
	}
	if got := bead.Metadata["provider_kind"]; got != "claude" {
		t.Fatalf("provider_kind = %q, want claude", got)
	}
	if got := bead.Metadata["builtin_ancestor"]; got != "claude" {
		t.Fatalf("builtin_ancestor = %q, want claude", got)
	}
	cfg := fs.sp.LastStartConfig(bead.Metadata["session_name"])
	if cfg == nil {
		t.Fatalf("Start call not recorded: %#v", fs.sp.Calls)
	}
	if got := cfg.Env["GC_PROVIDER"]; got != "claude" {
		t.Fatalf("GC_PROVIDER = %q, want claude", got)
	}
}

func TestMaterializeNamedSessionRejectsACPTemplateWithoutACPRouting(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	srv := New(fs)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget: %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec")
	}
	if _, err := srv.materializeNamedSession(fs.cityBeadStore, spec); err == nil {
		t.Fatal("materializeNamedSession() error = nil, want ACP routing error")
	} else if !strings.Contains(err.Error(), "requires ACP transport") {
		t.Fatalf("materializeNamedSession() error = %v, want ACP transport error", err)
	}
	items, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("session bead count = %d, want 0", len(items))
	}
}

func TestMaterializeNamedSessionPersistsStoredMCPMetadata(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "opencode"
	fs.cfg.Agents[0].Session = "acp"
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}", "{{.WorkDir}}", "{{.TemplateName}}"]
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  &transportCapableProvider{Fake: runtime.NewFake()},
	}
	srv := New(state)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
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
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got, want := bead.Metadata[session.MCPIdentityMetadataKey], spec.Identity; got != want {
		t.Fatalf("mcp_identity = %q, want %q", got, want)
	}
	if got := bead.Metadata[session.MCPServersSnapshotMetadataKey]; got == "" {
		t.Fatal("mcp_servers_snapshot = empty, want persisted snapshot")
	}
}

func TestHandleProviderSessionCreateWithMessageUsesProviderDefaultNudge(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"provider","name":"test-agent","message":"hello"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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

func TestHandleProviderSessionCreateUsesACPTransportCommand(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	defaultSP := runtime.NewFake()
	acpSP := &transportCapableProvider{Fake: runtime.NewFake()}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  sessionauto.New(defaultSP, acpSP),
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"opencode"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	start := acpSP.LastStartConfig(resp.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil", resp.SessionName)
	}
	if got, want := start.Command, "/bin/echo acp"; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
	bead, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", resp.ID, err)
	}
	if got, want := bead.Metadata["transport"], "acp"; got != want {
		t.Fatalf("transport metadata = %q, want %q", got, want)
	}
	if defaultSP.IsRunning(resp.SessionName) {
		t.Fatalf("default backend should not own ACP session %q", resp.SessionName)
	}
}

func TestHumaCreateProviderSessionUsesACPTransportCommand(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	defaultSP := runtime.NewFake()
	acpSP := &transportCapableProvider{Fake: runtime.NewFake()}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  sessionauto.New(defaultSP, acpSP),
	}
	srv := New(state)

	out, err := srv.humaCreateProviderSession(context.Background(), fs.cityBeadStore, sessionCreateBody{
		Kind: "provider",
		Name: "opencode",
	}, "opencode")
	if err != nil {
		t.Fatalf("humaCreateProviderSession: %v", err)
	}
	if got, want := out.Status, http.StatusCreated; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	start := acpSP.LastStartConfig(out.Body.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil", out.Body.SessionName)
	}
	if got, want := start.Command, "/bin/echo acp"; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
	bead, err := fs.cityBeadStore.Get(out.Body.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", out.Body.ID, err)
	}
	if got, want := bead.Metadata["transport"], "acp"; got != want {
		t.Fatalf("transport metadata = %q, want %q", got, want)
	}
	if defaultSP.IsRunning(out.Body.SessionName) {
		t.Fatalf("default backend should not own ACP session %q", out.Body.SessionName)
	}
}

func TestHandleProviderSessionCreateUsesACPTransportCapabilityProvider(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	provider := &transportCapableProvider{Fake: runtime.NewFake()}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  provider,
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"opencode"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	start := provider.LastStartConfig(resp.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil", resp.SessionName)
	}
	if got, want := start.Command, "/bin/echo acp"; got != want {
		t.Fatalf("start command = %q, want %q", got, want)
	}
	bead, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", resp.ID, err)
	}
	if got, want := bead.Metadata["transport"], "acp"; got != want {
		t.Fatalf("transport metadata = %q, want %q", got, want)
	}
}

func TestHandleProviderSessionCreateUsesPerSessionMCPIdentity(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	fs.cfg.PackMCPDir = filepath.Join(fs.cityPath, "mcp")
	if err := os.MkdirAll(fs.cfg.PackMCPDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(mcp): %v", err)
	}
	if err := os.WriteFile(filepath.Join(fs.cfg.PackMCPDir, "identity.template.toml"), []byte(`
name = "identity"
command = "/bin/mcp"
args = ["{{.AgentName}}", "{{.WorkDir}}", "{{.TemplateName}}"]
`), 0o644); err != nil {
		t.Fatalf("WriteFile(mcp): %v", err)
	}
	provider := &transportCapableProvider{Fake: runtime.NewFake()}
	state := &stateWithSessionProvider{
		fakeState: fs,
		provider:  provider,
	}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"opencode"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	start := provider.LastStartConfig(resp.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil", resp.SessionName)
	}
	if len(start.MCPServers) != 1 {
		t.Fatalf("Start MCPServers len = %d, want 1", len(start.MCPServers))
	}
	bead, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", resp.ID, err)
	}
	if got := bead.Metadata[session.MCPIdentityMetadataKey]; got == "" {
		t.Fatal("mcp_identity metadata = empty, want per-session identity")
	}
	if got, want := start.MCPServers[0].Args[0], bead.Metadata[session.MCPIdentityMetadataKey]; got != want {
		t.Fatalf("Start MCP identity = %q, want %q", got, want)
	}
	if got := bead.Metadata[session.MCPIdentityMetadataKey]; got == "opencode" {
		t.Fatalf("mcp_identity metadata = %q, want unique per-session identity", got)
	}
	if got, want := start.MCPServers[0].Args[1], fs.cityPath; got != want {
		t.Fatalf("Start workdir arg = %q, want %q", got, want)
	}
	if got, want := start.MCPServers[0].Args[2], bead.Metadata[session.MCPIdentityMetadataKey]; got != want {
		t.Fatalf("Start template arg = %q, want %q", got, want)
	}
}

func TestHandleProviderSessionCreateRejectsACPProviderWithoutACPRouting(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"opencode"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "requires ACP transport") {
		t.Fatalf("body = %q, want ACP transport error", rec.Body.String())
	}
}

func TestHumaCreateProviderSessionRejectsACPProviderWithoutACPRouting(t *testing.T) {
	supportsACP := true
	fs := newSessionFakeState(t)
	fs.cfg.Providers["opencode"] = config.ProviderSpec{
		DisplayName: "OpenCode",
		Command:     "/bin/echo",
		PathCheck:   "true",
		SupportsACP: &supportsACP,
		ACPCommand:  "/bin/echo",
		ACPArgs:     []string{"acp"},
	}
	srv := New(fs)

	if _, err := srv.humaCreateProviderSession(context.Background(), fs.cityBeadStore, sessionCreateBody{
		Kind: "provider",
		Name: "opencode",
	}, "opencode"); err == nil {
		t.Fatal("humaCreateProviderSession() error = nil, want ACP routing error")
	}
}

func TestHandleProviderSessionCreateWithMessageRollsBackOnDeliveryFailure(t *testing.T) {
	fs := newSessionFakeState(t)
	provider := &failNudgeProvider{Fake: runtime.NewFake(), err: errors.New("nudge failed")}
	wrappedState := &stateWithSessionProvider{fakeState: fs, provider: provider}
	srv := New(wrappedState)
	h := newTestCityHandlerWith(t, wrappedState, srv)

	body := `{"kind":"provider","name":"test-agent","message":"hello","title":"Retryable"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "provider-create-rollback")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("first create status = %d, want %d; body: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "initial message delivery failed") {
		t.Fatalf("first create body = %q, want initial message delivery failure detail", rec.Body.String())
	}

	items, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("session bead count after rollback = %d, want 0", len(items))
	}
	running, err := provider.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(running) != 0 {
		t.Fatalf("running sessions after rollback = %v, want none", running)
	}

	provider.err = nil
	req = newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "provider-create-rollback")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("retry create status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	items, err = fs.cityBeadStore.ListByLabel(session.LabelSession, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel after retry: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("session bead count after retry = %d, want 1", len(items))
	}
}

func TestHandleSessionCreatePersistsAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","alias":"sky"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","alias":"myrig/worker"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleProviderSessionCreateRejectsReservedQualifiedAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"provider","name":"test-agent","alias":"myrig/worker"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}
}

func TestHandleSessionCreateRejectsInvalidAlias(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","alias":"bad:name"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleSessionCreateRejectsLegacySessionNameField(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","session_name":"mayor"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","session_name":""}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	first := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker","alias":"sky"}`))
	firstW := httptest.NewRecorder()
	h.ServeHTTP(firstW, first)
	if firstW.Code != http.StatusAccepted {
		t.Fatalf("first create status %d, want %d; body: %s", firstW.Code, http.StatusAccepted, firstW.Body.String())
	}

	second := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"myrig/worker","alias":"sky"}`))
	secondW := httptest.NewRecorder()
	h.ServeHTTP(secondW, second)

	if secondW.Code != http.StatusConflict {
		t.Fatalf("got status %d, want %d; body: %s", secondW.Code, http.StatusConflict, secondW.Body.String())
	}
}

func TestHandleSessionCreateCanonicalizesBareTemplate(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"agent","name":"worker"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
		t.Errorf("Title = %q, want %q", resp.Title, "myrig/worker")
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

func TestHandleSessionCreateDoesNotApplyProviderDefaultsToAgentCommand(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker"}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	if strings.Contains(cmd, "--skip-permissions") {
		t.Errorf("command %q should not contain provider default flags for deferred agent create", cmd)
	}
	if strings.Contains(cmd, "--effort max") {
		t.Errorf("command %q should not contain provider default effort=max for deferred agent create", cmd)
	}
}

func TestHandleSessionCreateStoresExplicitOverridesWithoutCommandRewrite(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","options":{"effort":"high"}}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	if strings.Contains(cmd, "--skip-permissions") || strings.Contains(cmd, "--effort high") || strings.Contains(cmd, "--effort max") {
		t.Errorf("command %q should not be rewritten from provider defaults or explicit overrides", cmd)
	}
	ovr := b.Metadata["template_overrides"]
	if ovr == "" {
		t.Fatal("template_overrides not set")
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(ovr), &parsed); err != nil {
		t.Fatalf("parse template_overrides: %v", err)
	}
	if parsed["effort"] != "high" {
		t.Errorf("effort = %q, want %q", parsed["effort"], "high")
	}
	if _, ok := parsed["permission_mode"]; ok {
		t.Errorf("permission_mode override unexpectedly present: %#v", parsed)
	}
}

func TestHandleSessionCreatePersistsExplicitOptionsInTemplateOverrides(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	body := `{"kind":"agent","name":"myrig/worker","options":{"permission_mode":"plan","effort":"low"}}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	if strings.Contains(cmd, "--permission-mode plan") || strings.Contains(cmd, "--skip-permissions") || strings.Contains(cmd, "--effort low") {
		t.Errorf("command %q should not be rewritten from explicit overrides", cmd)
	}
	ovr := b.Metadata["template_overrides"]
	if ovr == "" {
		t.Fatal("template_overrides not set")
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(ovr), &parsed); err != nil {
		t.Fatalf("parse template_overrides: %v", err)
	}
	if parsed["permission_mode"] != "plan" {
		t.Errorf("permission_mode = %q, want %q", parsed["permission_mode"], "plan")
	}
	if parsed["effort"] != "low" {
		t.Errorf("effort = %q, want %q", parsed["effort"], "low")
	}
}

func TestHandleSessionCreatePreservesInitialMessageWithOptions(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	// Create session with BOTH options AND a message.
	// Regression: the old code overwrote template_overrides with just the
	// options, clobbering the initial_message that was set at creation time.
	body := `{"kind":"agent","name":"myrig/worker","message":"Hello from Discord!","options":{"effort":"high"}}`
	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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

func TestHandleSessionMessageMaterializedNamedSessionUsesLaunchCommandDefaults(t *testing.T) {
	fs := newSessionFakeStateWithOptions(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/session/worker/messages"), strings.NewReader(`{"message":"hello"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := resp["id"]
	if id == "" {
		t.Fatal("response missing session id")
	}

	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	cmd := bead.Metadata["command"]
	if !strings.Contains(cmd, "--skip-permissions") {
		t.Fatalf("command %q missing permission default", cmd)
	}
	if !strings.Contains(cmd, "--effort max") {
		t.Fatalf("command %q missing effort default", cmd)
	}
}

func TestHandleSessionMessageResumesSuspendedSessionUsingProviderDefaultNudge(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Resume Me")
	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/messages", strings.NewReader(`{"message":"hello"}`))
	req.Header.Set("Idempotency-Key", "sess-msg-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	req := newPostRequest(cityURL(fs, "/session/worker/messages"), strings.NewReader(`{"message":"hello"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

func TestHandleSessionMessageMaterializesBoundNamedSessionUsingQualifiedIdentity(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents = []config.Agent{{
		Name:         "alex",
		BindingName:  "employees",
		Provider:     "test-agent",
		StartCommand: "true",
	}}
	fs.cfg.NamedSessions = []config.NamedSession{{
		Name:        "corp--alex",
		Template:    "alex",
		BindingName: "employees",
	}}
	srv := New(fs)

	req := newPostRequest(cityURL(fs, "/session/employees.corp--alex/messages"), strings.NewReader(`{"message":"hello"}`))
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
	if got := b.Metadata["alias"]; got != "employees.corp--alex" {
		t.Fatalf("alias = %q, want employees.corp--alex", got)
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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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
	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+id, nil)
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	req := newPostRequest(cityURL(fs, "/session/worker/messages"), strings.NewReader(`{"message":"   "}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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
	req := httptest.NewRequest("GET", cityURL(fs, "/session/worker"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("get status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandleSessionCloseRejectsAlwaysNamedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.NamedSessions[0].Mode = "always"
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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
	req := newPostRequest(cityURL(fs, "/session/")+id+"/close", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("close status = %d, want %d; body: %s", rec.Code, http.StatusConflict, rec.Body.String())
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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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

func TestHandleSessionWakeMaterializesNamedSessionAndStartsRuntime(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/worker/wake"), nil)
	h.ServeHTTP(rec, req)

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
	wrappedState := &stateWithSessionProvider{fakeState: fs, provider: provider}
	srv := New(wrappedState)
	h := newTestCityHandlerWith(t, wrappedState, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/worker/wake"), nil).WithContext(ctx)
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
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
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp SessionStreamMessageEvent
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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
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
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?tail=0", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp SessionStreamMessageEvent
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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/pending", nil)
	h.ServeHTTP(w, r)

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

	respondReq := newPostRequest(cityURL(fs, "/session/")+info.ID+"/respond", strings.NewReader(`{"action":"approve"}`))
	respondReq.Header.Set("Idempotency-Key", "sess-respond-1")
	respondRec := httptest.NewRecorder()
	h.ServeHTTP(respondRec, respondReq)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/")+info.ID+"/messages", strings.NewReader(`{"message":"hello"}`))
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "myrig/worker", "Sky", "claude", t.TempDir(), "claude", "", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("CreateNamedWithTransport: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/sky/messages"), strings.NewReader(`{"message":"hello"}`))
	h.ServeHTTP(rec, req)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Interactive")
	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	respondReq := newPostRequest(cityURL(fs, "/session/")+info.ID+"/respond", strings.NewReader(`{"request_id":"req-2","action":"approve"}`))
	respondRec := httptest.NewRecorder()
	h.ServeHTTP(respondRec, respondReq)

	if respondRec.Code != http.StatusConflict {
		t.Fatalf("respond status = %d, want %d; body: %s", respondRec.Code, http.StatusConflict, respondRec.Body.String())
	}
}

func TestHandleSessionStreamSSEHeaders(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
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

	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
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
	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandleSessionStreamClosedSessionReturnsSnapshot(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
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

	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream", nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
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
	for _, event := range fs.eventProv.(*events.Fake).Events {
		if event.Type == events.WorkerOperation {
			t.Fatalf("closed session stream emitted worker operation event: %#v", event)
		}
	}
}

func TestHandleSessionStreamClosedNamedSessionReturnsSnapshot(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
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

	req := httptest.NewRequest("GET", cityURL(fs, "/session/sky/stream"), nil)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
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

func TestStreamSessionTranscriptHistoryDoesNotSkipTurnsAcrossCompactionBoundaries(t *testing.T) {
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
		`{"uuid":"a","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"before compaction\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"cb0","parentUuid":"a","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"b","parentUuid":"cb0","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"after first boundary\"}","timestamp":"2025-01-01T00:00:02Z"}`,
	)

	handle, err := srv.workerHandleForSession(fs.cityBeadStore, info.ID)
	if err != nil {
		t.Fatalf("workerHandleForSession: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer cancel()

	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		initial, histErr := handle.History(ctx, worker.HistoryRequest{})
		if histErr != nil {
			t.Errorf("History(initial): %v", histErr)
			close(done)
			return
		}
		srv.streamSessionTranscriptHistory(ctx, rec, info, handle, initial)
		close(done)
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(rec.BodyString(), "after first boundary") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	logDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	logPath := filepath.Join(logDir, info.SessionKey+".jsonl")
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

	body := rec.BodyString()
	if !strings.Contains(body, "bridge turn") {
		t.Fatalf("stream body missing turn written before new compact boundary: %s", body)
	}
	if !strings.Contains(body, "after second boundary") {
		t.Fatalf("stream body missing turn written after new compact boundary: %s", body)
	}
}

func TestCityScopedSessionStreamReloadsRotatedGeminiTranscriptAcrossRestart(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	base := t.TempDir()
	workDir := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}

	searchRoot := filepath.Join(base, ".gemini", "tmp")
	srv.sessionLogSearchPaths = []string{searchRoot}
	projectDir := filepath.Join(searchRoot, "project-a")
	chatsDir := filepath.Join(projectDir, "chats")
	for _, dir := range []string{searchRoot, projectDir, chatsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o644); err != nil {
		t.Fatalf("write .project_root: %v", err)
	}

	firstTranscript := filepath.Join(chatsDir, "session-2026-04-17T03-12-before.json")
	writeGeminiHistoryFixtureForAPI(t, firstTranscript, "before-session",
		`{"id":"u1","timestamp":"2026-04-17T03:12:00Z","type":"user","content":"first-remembered-input"}`,
		`{"id":"a1","timestamp":"2026-04-17T03:12:01Z","type":"gemini","content":"first-remembered-output"}`,
	)
	firstTime := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(firstTranscript, firstTime, firstTime); err != nil {
		t.Fatalf("chtimes(first transcript): %v", err)
	}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "gemini", workDir, "gemini", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "first-remembered-output", time.Second); !strings.Contains(body, "first-remembered-output") {
		t.Fatalf("stream body missing initial transcript turn: %s", body)
	}

	secondTranscript := filepath.Join(chatsDir, "session-2026-04-17T03-15-after.json")
	writeGeminiHistoryFixtureForAPI(t, secondTranscript, "after-session",
		`{"id":"u2","timestamp":"2026-04-17T03:15:00Z","type":"user","content":"second-continued-input"}`,
		`{"id":"a2","timestamp":"2026-04-17T03:15:01Z","type":"gemini","content":"second-continued-output"}`,
	)
	secondTime := time.Now().Add(-1 * time.Minute)
	if err := os.Chtimes(secondTranscript, secondTime, secondTime); err != nil {
		t.Fatalf("chtimes(second transcript): %v", err)
	}

	fs.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: info.ID,
	})

	body := waitForRecorderSubstring(t, rec, "second-continued-output", 1500*time.Millisecond)

	cancel()
	<-done

	if !strings.Contains(body, "second-continued-input") || !strings.Contains(body, "second-continued-output") {
		t.Fatalf("city-scoped stream body missing rotated transcript turns after wake: %s", body)
	}
}

func TestCityScopedSessionStreamFollowsRotatedGeminiTranscriptAfterWake(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	base := t.TempDir()
	workDir := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}

	searchRoot := filepath.Join(base, ".gemini", "tmp")
	srv.sessionLogSearchPaths = []string{searchRoot}
	projectDir := filepath.Join(searchRoot, "project-a")
	chatsDir := filepath.Join(projectDir, "chats")
	for _, dir := range []string{searchRoot, projectDir, chatsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o644); err != nil {
		t.Fatalf("write .project_root: %v", err)
	}

	firstTranscript := filepath.Join(chatsDir, "session-2026-04-17T03-12-before.json")
	writeGeminiHistoryFixtureForAPI(t, firstTranscript, "before-session",
		`{"id":"u1","timestamp":"2026-04-17T03:12:00Z","type":"user","content":"first-input"}`,
		`{"id":"a1","timestamp":"2026-04-17T03:12:01Z","type":"gemini","content":"first-output"}`,
	)
	firstTime := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(firstTranscript, firstTime, firstTime); err != nil {
		t.Fatalf("chtimes(first transcript): %v", err)
	}

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "gemini", workDir, "gemini", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "first-output", 3*time.Second); !strings.Contains(body, "first-output") {
		t.Fatalf("stream body missing initial transcript turn: %s", body)
	}

	secondTranscript := filepath.Join(chatsDir, "session-2026-04-17T03-15-after.json")
	writeGeminiHistoryFixtureForAPI(t, secondTranscript, "after-session",
		`{"id":"u2","timestamp":"2026-04-17T03:15:00Z","type":"user","content":"second-input"}`,
		`{"id":"a2","timestamp":"2026-04-17T03:15:01Z","type":"gemini","content":"second-output"}`,
	)
	secondTime := time.Now().Add(-1 * time.Minute)
	if err := os.Chtimes(secondTranscript, secondTime, secondTime); err != nil {
		t.Fatalf("chtimes(second transcript): %v", err)
	}

	fs.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: info.ID,
	})

	if body := waitForRecorderSubstring(t, rec, "second-output", 5*time.Second); !strings.Contains(body, "second-output") {
		t.Fatalf("stream body missing rotated transcript after wake: %s", body)
	}

	writeGeminiHistoryFixtureForAPI(t, secondTranscript, "after-session",
		`{"id":"u2","timestamp":"2026-04-17T03:15:00Z","type":"user","content":"second-input"}`,
		`{"id":"a2","timestamp":"2026-04-17T03:15:01Z","type":"gemini","content":"second-output"}`,
		`{"id":"u3","timestamp":"2026-04-17T03:15:02Z","type":"user","content":"third-input"}`,
		`{"id":"a3","timestamp":"2026-04-17T03:15:03Z","type":"gemini","content":"third-output"}`,
	)
	currentTime := time.Now()
	if err := os.Chtimes(secondTranscript, currentTime, currentTime); err != nil {
		t.Fatalf("chtimes(updated second transcript): %v", err)
	}

	body := waitForRecorderSubstring(t, rec, "third-output", 5*time.Second)

	cancel()
	<-done

	if !strings.Contains(body, "third-input") || !strings.Contains(body, "third-output") {
		t.Fatalf("city-scoped stream body missing writes to rotated transcript after wake: %s", body)
	}
}

func TestHandleSessionStreamWorkerOperationEventWakesTranscriptReload(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "hello", time.Second); !strings.Contains(body, "hello") {
		t.Fatalf("stream body missing initial turn: %s", body)
	}

	logDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	logPath := filepath.Join(logDir, info.SessionKey+".jsonl")
	appendFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, err = appendFile.WriteString(strings.Join([]string{
		`{"uuid":"3","parentUuid":"2","type":"user","message":"{\"role\":\"user\",\"content\":\"wake now\"}","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"event wake turn\"}","timestamp":"2025-01-01T00:00:03Z"}`,
	}, "\n") + "\n")
	if closeErr := appendFile.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("append transcript: %v", err)
	}

	fs.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: info.ID,
	})

	body := waitForRecorderSubstring(t, rec, "event wake turn", 1500*time.Millisecond)

	cancel()
	<-done

	if !strings.Contains(body, "event wake turn") {
		t.Fatalf("stream body missing turn after worker operation wakeup: %s", body)
	}
}

func TestHandleSessionStreamRawWorkerOperationEventWakesTranscriptReload(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream?format=raw", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "hello", time.Second); !strings.Contains(body, "hello") {
		t.Fatalf("raw stream body missing initial transcript: %s", body)
	}

	logDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	logPath := filepath.Join(logDir, info.SessionKey+".jsonl")
	appendFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, err = appendFile.WriteString(`{"uuid":"3","parentUuid":"2","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"raw event wake\"}","timestamp":"2025-01-01T00:00:02Z"}` + "\n")
	if closeErr := appendFile.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("append transcript: %v", err)
	}

	fs.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: info.ID,
	})

	body := waitForRecorderSubstring(t, rec, "raw event wake", 1500*time.Millisecond)

	cancel()
	<-done

	if !strings.Contains(body, "raw event wake") {
		t.Fatalf("raw stream body missing message after worker operation wakeup: %s", body)
	}
}

func TestHandleSessionStreamRawStallEmitsPendingWithoutTranscriptGrowth(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	srv.sessionLogSearchPaths = []string{searchBase}

	prevStallTimeout := sessionStreamPendingStallTimeout
	sessionStreamPendingStallTimeout = 50 * time.Millisecond
	defer func() {
		sessionStreamPendingStallTimeout = prevStallTimeout
	}()

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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream?format=raw", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "hello", time.Second); !strings.Contains(body, "hello") {
		t.Fatalf("raw stream body missing initial transcript: %s", body)
	}

	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "Proceed?",
	})

	body := waitForRecorderSubstring(t, rec, "req-1", time.Second)

	cancel()
	<-done

	if !strings.Contains(body, "req-1") {
		t.Fatalf("raw stream body missing pending interaction after idle stall: %s", body)
	}
}

func TestHandleSessionStreamRawStallEmitsPendingEventOnCityRoute(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	srv.sessionLogSearchPaths = []string{searchBase}

	prevStallTimeout := sessionStreamPendingStallTimeout
	sessionStreamPendingStallTimeout = 50 * time.Millisecond
	defer func() {
		sessionStreamPendingStallTimeout = prevStallTimeout
	}()

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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream?format=raw", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "hello", time.Second); !strings.Contains(body, "hello") {
		t.Fatalf("raw stream body missing initial transcript: %s", body)
	}

	fs.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "Proceed?",
	})

	body := waitForRecorderSubstring(t, rec, "req-1", time.Second)

	cancel()
	<-done

	if !strings.Contains(body, "event: pending") {
		t.Fatalf("raw stream body missing pending SSE event name: %s", body)
	}
}

func TestHandleSessionStreamTranscriptWriteWakesWithoutPolling(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "hello", time.Second); !strings.Contains(body, "hello") {
		t.Fatalf("stream body missing initial turn: %s", body)
	}

	logDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	logPath := filepath.Join(logDir, info.SessionKey+".jsonl")
	appendFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, err = appendFile.WriteString(strings.Join([]string{
		`{"uuid":"3","parentUuid":"2","type":"user","message":"{\"role\":\"user\",\"content\":\"file wake\"}","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"fsnotify wake turn\"}","timestamp":"2025-01-01T00:00:03Z"}`,
	}, "\n") + "\n")
	if closeErr := appendFile.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("append transcript: %v", err)
	}

	body := waitForRecorderSubstring(t, rec, "fsnotify wake turn", 1500*time.Millisecond)

	cancel()
	<-done

	if !strings.Contains(body, "fsnotify wake turn") {
		t.Fatalf("stream body missing turn after transcript write wakeup: %s", body)
	}
}

func TestHandleSessionStreamConversationFiltersNonDisplayEntries(t *testing.T) {
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
		`{"uuid":"3","parentUuid":"2","type":"tool_use","message":"{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"name\":\"debugtool\"}]}","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"tool_result","message":"{\"role\":\"tool\",\"content\":\"internal raw detail\"}","timestamp":"2025-01-01T00:00:03Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "hello") || !strings.Contains(body, "world") {
		t.Fatalf("conversation stream body missing display turns: %s", body)
	}
	if strings.Contains(body, "debugtool") || strings.Contains(body, "internal raw detail") {
		t.Fatalf("conversation stream leaked non-display transcript entries: %s", body)
	}
}

func TestHandleSessionStreamConversationRedactsThinkingText(t *testing.T) {
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
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":[{\"type\":\"thinking\",\"thinking\":\"private chain of thought\"},{\"type\":\"text\",\"text\":\"visible answer\"}]}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "visible answer") {
		t.Fatalf("conversation stream body missing visible assistant answer: %s", body)
	}
	if strings.Contains(body, "private chain of thought") {
		t.Fatalf("conversation stream leaked thinking text: %s", body)
	}
}

func TestHandleSessionStreamRawUsesLatestCompactionTail(t *testing.T) {
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
		`{"uuid":"a","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"before compaction\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"cb0","parentUuid":"a","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"b","parentUuid":"cb0","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"after first boundary\"}","timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"cb1","parentUuid":"b","type":"system","subtype":"compact_boundary","timestamp":"2025-01-01T00:00:03Z"}`,
		`{"uuid":"c","parentUuid":"cb1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"after second boundary\"}","timestamp":"2025-01-01T00:00:04Z"}`,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream?format=raw", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "after second boundary") {
		t.Fatalf("raw stream body missing latest compaction tail: %s", body)
	}
	if strings.Contains(body, "before compaction") || strings.Contains(body, "after first boundary") {
		t.Fatalf("raw stream replayed full transcript instead of latest compaction tail: %s", body)
	}
}

func TestHandleSessionTranscriptRawIncludesAllTypes(t *testing.T) {
	fs := newSessionFakeState(t)
	searchBase := t.TempDir()
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
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
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=raw&tail=0", nil)
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp SessionStreamRawMessageEvent
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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h
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
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	h.ServeHTTP(w, r)

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
	h := newTestCityHandlerWith(t, fs, srv)
	_ = h

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
	r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID, nil)
	h.ServeHTTP(w, r)

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

// TestSessionToResponse_BaseOnlyDescendant_InheritsDisplayName mirrors
// the /v0/agents base-only test for /v0/sessions: the session response
// must pick up the builtin ancestor's DisplayName when the leaf
// provider doesn't declare one, routed through the resolved cache.
func TestSessionToResponse_BaseOnlyDescendant_InheritsDisplayName(t *testing.T) {
	baseCodex := "builtin:codex"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{
			"codex-max": {Base: &baseCodex}, // no DisplayName, no Command
		},
		ResolvedProviders: map[string]config.ResolvedProvider{
			"codex-max": {
				Name:            "codex-max",
				BuiltinAncestor: "codex",
				Command:         "codex",
			},
		},
	}

	info := session.Info{
		ID:       "sess-1",
		Template: "myrig/mayor",
		Provider: "codex-max",
	}
	resp := sessionToResponse(info, cfg)

	if resp.Provider != "codex-max" {
		t.Errorf("Provider = %q, want codex-max", resp.Provider)
	}
	// DisplayName inherited from builtin:codex via the resolved cache.
	if resp.DisplayName != "Codex CLI" {
		t.Errorf("DisplayName = %q, want %q (inherited)", resp.DisplayName, "Codex CLI")
	}
}

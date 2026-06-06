package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// writeSessionJSONL creates a JSONL session file at the slug path for
// the given workDir.
func writeSessionJSONL(t *testing.T, searchBase, workDir string, lines ...string) {
	t.Helper()
	slug := strings.ReplaceAll(workDir, "/", "-")
	slug = strings.ReplaceAll(slug, ".", "-")
	dir := filepath.Join(searchBase, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test-session.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newServerWithSearchPaths(state State, searchBase string) *Server {
	s := New(state)
	s.sessionLogSearchPaths = []string{searchBase}
	return s
}

type geminiAgentOutputStreamFixture struct {
	state *fakeState
	srv   *Server
	info  session.Info
	chats string
}

func newGeminiAgentOutputStreamFixture(t *testing.T) *geminiAgentOutputStreamFixture {
	t.Helper()

	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Provider = "gemini"
	fs.cfg.Workspace.Provider = "gemini"
	srv := New(fs)

	base := t.TempDir()
	workDir := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}
	fs.cfg.Rigs[0].Path = workDir

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

	return &geminiAgentOutputStreamFixture{
		state: fs,
		srv:   srv,
		info:  info,
		chats: chatsDir,
	}
}

func TestAgentOutputConversation(t *testing.T) {
	state := newFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}

	searchBase := t.TempDir()
	writeSessionJSONL(t, searchBase, rigDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	srv := newServerWithSearchPaths(state, searchBase)
	h := newTestCityHandlerWith(t, state, srv)
	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker/output?tail=0"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp agentOutputResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Agent != "myrig/worker" {
		t.Errorf("Agent = %q, want %q", resp.Agent, "myrig/worker")
	}
	if resp.Format != "conversation" {
		t.Errorf("Format = %q, want %q", resp.Format, "conversation")
	}
	if len(resp.Turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(resp.Turns))
	}
	if resp.Turns[0].Role != "user" || resp.Turns[0].Text != "hello" {
		t.Errorf("turn[0] = %+v, want role=user text=hello", resp.Turns[0])
	}
	if resp.Turns[1].Role != "assistant" || resp.Turns[1].Text != "world" {
		t.Errorf("turn[1] = %+v, want role=assistant text=world", resp.Turns[1])
	}
}

func TestAgentOutputConversationUsesConfiguredWorkDir(t *testing.T) {
	state := newFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}
	state.cfg.Agents[0].WorkDir = ".gc/worktrees/{{.Rig}}/{{.AgentBase}}"

	searchBase := t.TempDir()
	workDir := filepath.Join(state.cityPath, ".gc", "worktrees", "myrig", "worker")
	writeSessionJSONL(t, searchBase, workDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"from workdir\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	srv := newServerWithSearchPaths(state, searchBase)
	h := newTestCityHandlerWith(t, state, srv)
	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker/output?tail=0"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp agentOutputResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "conversation" {
		t.Fatalf("Format = %q, want conversation", resp.Format)
	}
	if len(resp.Turns) != 2 || resp.Turns[1].Text != "from workdir" {
		t.Fatalf("Turns = %+v, want configured work_dir session log", resp.Turns)
	}
}

func TestAgentOutputNotFound(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/agent/nonexistent/output"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestAgentOutputCityScoped(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = append(state.cfg.Agents, config.Agent{Name: "mayor"})

	searchBase := t.TempDir()
	writeSessionJSONL(t, searchBase, state.cityPath,
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"plan\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	srv := newServerWithSearchPaths(state, searchBase)
	h := newTestCityHandlerWith(t, state, srv)
	req := httptest.NewRequest("GET", cityURL(state, "/agent/mayor/output?tail=0"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp agentOutputResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Agent != "mayor" {
		t.Errorf("Agent = %q, want %q", resp.Agent, "mayor")
	}
	if len(resp.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(resp.Turns))
	}
	if resp.Turns[0].Text != "plan" {
		t.Errorf("text = %q, want %q", resp.Turns[0].Text, "plan")
	}
}

func TestAgentOutputPagination(t *testing.T) {
	state := newFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}
	searchBase := t.TempDir()

	var lines []string
	lines = append(lines, `{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"first\"}","timestamp":"2025-01-01T00:00:00Z"}`)
	lines = append(lines, `{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"first reply\"}","timestamp":"2025-01-01T00:00:01Z"}`)
	lines = append(lines, `{"uuid":"3","parentUuid":"2","type":"system","subtype":"compact_boundary","message":"{\"role\":\"system\",\"content\":\"compacted 1\"}","timestamp":"2025-01-01T00:00:02Z"}`)
	lines = append(lines, `{"uuid":"4","parentUuid":"3","type":"user","message":"{\"role\":\"user\",\"content\":\"second\"}","timestamp":"2025-01-01T00:00:03Z"}`)
	lines = append(lines, `{"uuid":"5","parentUuid":"4","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"second reply\"}","timestamp":"2025-01-01T00:00:04Z"}`)
	lines = append(lines, `{"uuid":"6","parentUuid":"5","type":"system","subtype":"compact_boundary","message":"{\"role\":\"system\",\"content\":\"compacted 2\"}","timestamp":"2025-01-01T00:00:05Z"}`)
	lines = append(lines, `{"uuid":"7","parentUuid":"6","type":"user","message":"{\"role\":\"user\",\"content\":\"third\"}","timestamp":"2025-01-01T00:00:06Z"}`)
	lines = append(lines, `{"uuid":"8","parentUuid":"7","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"third reply\"}","timestamp":"2025-01-01T00:00:07Z"}`)

	writeSessionJSONL(t, searchBase, rigDir, lines...)

	srv := newServerWithSearchPaths(state, searchBase)
	h := newTestCityHandlerWith(t, state, srv)

	// tail=1 should return messages from the last compact boundary onward.
	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker/output?tail=1"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp agentOutputResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Boundary text + 2 turns after = 3 turns (system entry "compacted 2" + user + assistant).
	if len(resp.Turns) != 3 {
		t.Fatalf("got %d turns, want 3", len(resp.Turns))
	}

	if resp.Pagination == nil {
		t.Fatal("pagination is nil, expected non-nil")
	}
	if !resp.Pagination.HasOlderMessages {
		t.Error("expected HasOlderMessages=true")
	}
}

func TestAgentOutputCorruptedSessionFile(t *testing.T) {
	state := newFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}

	searchBase := t.TempDir()
	// Write a corrupt JSONL file that will cause ReadFile to fail.
	// An empty file won't trigger the path (FindSessionFile needs .jsonl).
	// Write truncated/garbage content.
	writeSessionJSONL(t, searchBase, rigDir,
		`not valid json at all {{{`,
	)

	srv := newServerWithSearchPaths(state, searchBase)
	h := newTestCityHandlerWith(t, state, srv)
	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker/output"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// The corrupt file IS found by FindSessionFile, but ReadFile returns
	// an empty session (no valid entries). The handler should return a
	// conversation response with 0 turns (the entries are skipped, not errored).
	// This is correct because ReadFile skips malformed lines rather than
	// failing the whole parse.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var resp agentOutputResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "conversation" {
		t.Errorf("Format = %q, want %q", resp.Format, "conversation")
	}
}

func TestResolveAgentTranscriptUsesBeadSessionIDWhenRuntimeMetaMissing(t *testing.T) {
	state := newSessionFakeState(t)
	workDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: workDir}}
	state.cfg.Agents[0].Provider = "claude/tmux-cli"

	searchBase := t.TempDir()
	slug := strings.NewReplacer("/", "-", ".", "-").Replace(workDir)
	transcriptDir := filepath.Join(searchBase, slug)
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}

	keyedPath := filepath.Join(transcriptDir, "sess-claude.jsonl")
	if err := os.WriteFile(keyedPath, []byte(
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"right"},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-claude"}`+"\n",
	), 0o644); err != nil {
		t.Fatalf("write keyed transcript: %v", err)
	}
	otherPath := filepath.Join(transcriptDir, "different-session.jsonl")
	if err := os.WriteFile(otherPath, []byte(
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"wrong"},"timestamp":"2025-01-01T00:00:00Z","sessionId":"provider-claude"}`+"\n",
	), 0o644); err != nil {
		t.Fatalf("write fallback transcript: %v", err)
	}

	srv := newServerWithSearchPaths(state, searchBase)
	mgr := session.NewManager(state.cityBeadStore, state.sp)
	sessionName := agentSessionName(state.CityName(), "myrig/worker", state.cfg.Workspace.SessionTemplate)
	info, err := mgr.CreateAliasedNamedWithTransport(
		context.Background(),
		"",
		sessionName,
		"myrig/worker",
		"Chat",
		"claude",
		workDir,
		"claude/tmux-cli",
		"",
		nil,
		session.ProviderResume{},
		runtime.Config{},
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.PersistSessionKey(info.ID, "sess-claude"); err != nil {
		t.Fatalf("PersistSessionKey: %v", err)
	}

	resolved, err := srv.resolveAgentTranscript("myrig/worker", state.cfg.Agents[0])
	if err != nil {
		t.Fatalf("resolveAgentTranscript: %v", err)
	}
	if resolved.sessionID != info.ID {
		t.Fatalf("sessionID = %q, want %q", resolved.sessionID, info.ID)
	}
	if resolved.path != keyedPath {
		t.Fatalf("path = %q, want %q (and not %q)", resolved.path, keyedPath, otherPath)
	}
}

func TestAgentOutputStreamSSEHeaders(t *testing.T) {
	state := newFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}

	searchBase := t.TempDir()
	writeSessionJSONL(t, searchBase, rigDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	srv := newServerWithSearchPaths(state, searchBase)
	h := newTestCityHandlerWith(t, state, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker/output/stream"), nil).WithContext(ctx)
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

	body := rec.Body.String()
	if !strings.Contains(body, "event: turn") {
		t.Errorf("body should contain SSE turn event, got: %s", body)
	}
	if !strings.Contains(body, "hello") {
		t.Errorf("body should contain initial turn text, got: %s", body)
	}
}

func TestAgentOutputStreamNotFound(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/agent/nonexistent/output/stream"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestAgentOutputStreamNotRunning(t *testing.T) {
	state := newFakeState(t)
	// Agent exists in config but no session file and not running → 404.
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker/output/stream"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestAgentOutputStreamNewTurns(t *testing.T) {
	state := newFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}

	searchBase := t.TempDir()
	writeSessionJSONL(t, searchBase, rigDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"first\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	srv := newServerWithSearchPaths(state, searchBase)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/agent/myrig/worker/output/stream", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "first", time.Second); !strings.Contains(body, "first") {
		t.Fatalf("stream body missing initial turn: %s", body)
	}

	// Append a new entry to the session file.
	slug := strings.ReplaceAll(rigDir, "/", "-")
	slug = strings.ReplaceAll(slug, ".", "-")
	sessionFile := filepath.Join(searchBase, slug, "test-session.jsonl")
	f, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"second\"}","timestamp":"2025-01-01T00:00:01Z"}` + "\n")
	f.Close() //nolint:errcheck // test file
	if err != nil {
		t.Fatal(err)
	}

	// fsnotify should wake this quickly, but keep enough budget for the
	// fallback poll path in environments where file watching is unavailable.
	body := waitForRecorderSubstring(t, rec, "second", 3*time.Second)
	cancel()
	<-done

	// Should have two SSE events: initial "first" and new "second".
	if !strings.Contains(body, "first") {
		t.Errorf("body should contain initial turn, got: %s", body)
	}
	if !strings.Contains(body, "second") {
		t.Errorf("body should contain new turn after poll, got: %s", body)
	}
	// Should have two separate "event: turn" lines.
	if strings.Count(body, "event: turn") < 2 {
		t.Errorf("expected at least 2 SSE turn events, got %d in: %s", strings.Count(body, "event: turn"), body)
	}
}

func TestAgentOutputStreamStoppedAgent(t *testing.T) {
	// When a session log exists but the agent is not running, the stream
	// should still succeed (replay mode) but include GC-Agent-Status: stopped.
	state := newFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}

	searchBase := t.TempDir()
	writeSessionJSONL(t, searchBase, rigDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	srv := newServerWithSearchPaths(state, searchBase)
	h := newTestCityHandlerWith(t, state, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker/output/stream"), nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Result().Header.Get("GC-Agent-Status"); got != "stopped" {
		t.Errorf("committed GC-Agent-Status = %q, want %q", got, "stopped")
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Errorf("body should contain session data, got: %s", rec.Body.String())
	}
}

func TestAgentOutputStreamStoppedAgentCommitsStatusHeader(t *testing.T) {
	state := newFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}

	searchBase := t.TempDir()
	writeSessionJSONL(t, searchBase, rigDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	srv := newServerWithSearchPaths(state, searchBase)
	h := newTestCityHandlerWith(t, state, srv)
	ts := httptest.NewServer(h)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+cityURL(state, "/agent/myrig/worker/output/stream"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	cancel()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("GC-Agent-Status"); got != "stopped" {
		t.Fatalf("committed GC-Agent-Status = %q, want %q", got, "stopped")
	}
}

func TestAgentOutputStreamFollowsRotatedGeminiTranscriptAfterWake(t *testing.T) {
	fixture := newGeminiAgentOutputStreamFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/agent/myrig/worker/output/stream", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		fixture.srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "first-output", time.Second); !strings.Contains(body, "first-output") {
		t.Fatalf("stream body missing initial transcript turn: %s", body)
	}

	secondTranscript := filepath.Join(fixture.chats, "session-2026-04-17T03-15-after.json")
	writeGeminiHistoryFixtureForAPI(t, secondTranscript, "after-session",
		`{"id":"u1","timestamp":"2026-04-17T03:15:00Z","type":"user","content":"second-input"}`,
		`{"id":"a1","timestamp":"2026-04-17T03:15:01Z","type":"gemini","content":"second-output"}`,
	)
	secondTime := time.Now().Add(-1 * time.Minute)
	if err := os.Chtimes(secondTranscript, secondTime, secondTime); err != nil {
		t.Fatalf("chtimes(second transcript): %v", err)
	}

	sessionName := agentSessionName(fixture.state.CityName(), "myrig/worker", fixture.state.cfg.Workspace.SessionTemplate)
	fixture.state.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: sessionName,
	})

	if body := waitForRecorderSubstring(t, rec, "second-output", 1500*time.Millisecond); !strings.Contains(body, "second-output") {
		t.Fatalf("stream body missing rotated transcript after wake: %s", body)
	}

	writeGeminiHistoryFixtureForAPI(t, secondTranscript, "after-session",
		`{"id":"u1","timestamp":"2026-04-17T03:15:00Z","type":"user","content":"second-input"}`,
		`{"id":"a1","timestamp":"2026-04-17T03:15:01Z","type":"gemini","content":"second-output"}`,
		`{"id":"u2","timestamp":"2026-04-17T03:15:02Z","type":"user","content":"third-input"}`,
		`{"id":"a2","timestamp":"2026-04-17T03:15:03Z","type":"gemini","content":"third-output"}`,
	)
	if err := os.Chtimes(secondTranscript, time.Now(), time.Now()); err != nil {
		t.Fatalf("chtimes(updated second transcript): %v", err)
	}

	body := waitForRecorderSubstring(t, rec, "third-output", 1500*time.Millisecond)

	cancel()
	<-done

	if !strings.Contains(body, "third-input") || !strings.Contains(body, "third-output") {
		t.Fatalf("stream body missing writes to rotated transcript after wake: %s", body)
	}
}

func TestCityScopedAgentOutputStreamFollowsRotatedGeminiTranscriptAfterWake(t *testing.T) {
	fixture := newGeminiAgentOutputStreamFixture(t)
	h := newTestCityHandlerWith(t, fixture.state, fixture.srv)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", cityURL(fixture.state, "/agent/myrig/worker/output/stream"), nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "first-output", time.Second); !strings.Contains(body, "first-output") {
		t.Fatalf("city-scoped stream body missing initial transcript turn: %s", body)
	}

	secondTranscript := filepath.Join(fixture.chats, "session-2026-04-17T03-15-after.json")
	writeGeminiHistoryFixtureForAPI(t, secondTranscript, "after-session",
		`{"id":"u1","timestamp":"2026-04-17T03:15:00Z","type":"user","content":"second-input"}`,
		`{"id":"a1","timestamp":"2026-04-17T03:15:01Z","type":"gemini","content":"second-output"}`,
	)
	secondTime := time.Now().Add(-1 * time.Minute)
	if err := os.Chtimes(secondTranscript, secondTime, secondTime); err != nil {
		t.Fatalf("chtimes(second transcript): %v", err)
	}

	sessionName := agentSessionName(fixture.state.CityName(), "myrig/worker", fixture.state.cfg.Workspace.SessionTemplate)
	fixture.state.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: sessionName,
	})

	if body := waitForRecorderSubstring(t, rec, "second-output", 1500*time.Millisecond); !strings.Contains(body, "second-output") {
		t.Fatalf("city-scoped stream body missing rotated transcript after wake: %s", body)
	}

	writeGeminiHistoryFixtureForAPI(t, secondTranscript, "after-session",
		`{"id":"u1","timestamp":"2026-04-17T03:15:00Z","type":"user","content":"second-input"}`,
		`{"id":"a1","timestamp":"2026-04-17T03:15:01Z","type":"gemini","content":"second-output"}`,
		`{"id":"u2","timestamp":"2026-04-17T03:15:02Z","type":"user","content":"third-input"}`,
		`{"id":"a2","timestamp":"2026-04-17T03:15:03Z","type":"gemini","content":"third-output"}`,
	)
	if err := os.Chtimes(secondTranscript, time.Now(), time.Now()); err != nil {
		t.Fatalf("chtimes(updated second transcript): %v", err)
	}

	body := waitForRecorderSubstring(t, rec, "third-output", 1500*time.Millisecond)

	cancel()
	<-done

	if !strings.Contains(body, "third-input") || !strings.Contains(body, "third-output") {
		t.Fatalf("city-scoped stream body missing writes to rotated transcript after wake: %s", body)
	}
}

func TestAgentOutputStreamWorkerOperationEventWakesPeekFallback(t *testing.T) {
	state := newFakeState(t)
	if err := state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	state.sp.SetPeekOutput("myrig--worker", "first output")
	srv := New(state)
	srv.sessionLogSearchPaths = []string{t.TempDir()}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/agent/myrig/worker/output/stream", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "first output", 10*time.Second); !strings.Contains(body, "first output") {
		t.Fatalf("stream body missing initial output: %s", body)
	}

	state.sp.SetPeekOutput("myrig--worker", "wake from runtime event")
	state.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: "myrig--worker",
	})

	body := waitForRecorderSubstring(t, rec, "wake from runtime event", 10*time.Second)

	cancel()
	<-done

	if !strings.Contains(body, "wake from runtime event") {
		t.Fatalf("stream body missing output after worker operation wakeup: %s", body)
	}
}

func TestAgentOutputStreamWorkerOperationSessionIDWakesPeekFallback(t *testing.T) {
	state := newSessionFakeState(t)
	mgr := session.NewManager(state.cityBeadStore, state.sp)
	sessionName := agentSessionName(state.CityName(), "myrig/worker", state.cfg.Workspace.SessionTemplate)
	info, err := mgr.CreateAliasedNamedWithTransport(
		context.Background(),
		"",
		sessionName,
		"myrig/worker",
		"Chat",
		"claude",
		t.TempDir(),
		"claude",
		"",
		nil,
		session.ProviderResume{},
		runtime.Config{},
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	state.sp.SetPeekOutput(info.SessionName, "first output")
	srv := New(state)
	srv.sessionLogSearchPaths = []string{t.TempDir()}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/v0/agent/myrig/worker/output/stream", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()

	if body := waitForRecorderSubstring(t, rec, "first output", 10*time.Second); !strings.Contains(body, "first output") {
		t.Fatalf("stream body missing initial output: %s", body)
	}

	state.sp.SetPeekOutput(info.SessionName, "wake from session id")
	state.eventProv.(*events.Fake).Record(events.Event{
		Type:    events.WorkerOperation,
		Actor:   "worker",
		Subject: info.ID,
	})

	body := waitForRecorderSubstring(t, rec, "wake from session id", 10*time.Second)

	cancel()
	<-done

	if !strings.Contains(body, "wake from session id") {
		t.Fatalf("stream body missing output after session worker operation wakeup: %s", body)
	}
}

package t3bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gorilla/websocket"
)

// stalledWriteTimeout is the write bound the write-deadline tests install.
const stalledWriteTimeout = 200 * time.Millisecond

// shrinkWriteTimeout sets bridgeWSWriteTimeout to d and restores it on cleanup.
func shrinkWriteTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := bridgeWSWriteTimeout
	bridgeWSWriteTimeout = d
	t.Cleanup(func() { bridgeWSWriteTimeout = prev })
}

// smallRecvBufferListener shrinks the kernel receive buffer of every accepted
// connection, so a peer that stops reading backs a writer up after a few
// kilobytes instead of after the loopback autotuning ceiling.
type smallRecvBufferListener struct{ net.Listener }

// Accept takes the next connection and shrinks its receive buffer.
func (l smallRecvBufferListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(4096)
	}
	return conn, nil
}

// newLoopbackServer starts handler on a loopback listener, shrinking the
// receive buffer of each accepted connection when smallRecv is set. The caller
// closes it. Every server in this file starts here.
func newLoopbackServer(handler http.Handler, smallRecv bool) *httptest.Server {
	srv := httptest.NewUnstartedServer(handler)
	if smallRecv {
		srv.Listener = smallRecvBufferListener{srv.Listener}
	}
	srv.Start()
	return srv
}

// newStalledWSPeer starts a loopback server that completes the WebSocket
// upgrade and then never reads, and returns its ws:// URL. Closing happens on
// test cleanup.
func newStalledWSPeer(t *testing.T) string {
	t.Helper()
	release := make(chan struct{})
	upgrader := websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}
	srv := newLoopbackServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		<-release
	}), true)
	t.Cleanup(func() {
		close(release)
		srv.CloseClientConnections()
		srv.Close()
	})
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// closedLoopbackWSURL returns a ws:// URL on a loopback port that a server held
// and then released, so a dial to it is refused at once.
func closedLoopbackWSURL(t *testing.T) string {
	t.Helper()
	srv := newLoopbackServer(http.NotFoundHandler(), false)
	addr := srv.Listener.Addr().String()
	srv.Close()
	return "ws://" + addr + "/ws"
}

// callBounded runs rpcCallOnceCtx against the configured candidates and fails
// the test when it has not returned within limit.
func callBounded(t *testing.T, limit time.Duration) error {
	t.Helper()
	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}
	// Large enough to overrun the socket buffers on both sides.
	params := map[string]interface{}{"blob": strings.Repeat("x", 32<<20)}
	done := make(chan error, 1)
	go func() {
		_, err := p.rpcCallOnceCtx(context.Background(), "orchestration.dispatchCommand", params, 1, false)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("rpcCallOnceCtx still blocked after %s: a peer that stops reading holds the write forever", limit)
		return nil
	}
}

// Kills a request write with no deadline: a peer that upgrades and stops
// reading must fail the call within the write bound, not hold it open.
func TestRPCCallOnceBoundsAWriteToAPeerThatStopsReading(t *testing.T) {
	shrinkWriteTimeout(t, stalledWriteTimeout)
	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("T3_BEARER_TOKEN", "")
	limit := stalledWriteTimeout + 2*time.Second

	t.Run("single candidate", func(t *testing.T) {
		stalled := newStalledWSPeer(t)
		oldDefaults := defaultWSURLCandidates
		defaultWSURLCandidates = nil
		t.Cleanup(func() { defaultWSURLCandidates = oldDefaults })
		t.Setenv("T3_WS_URL", stalled)

		start := time.Now()
		err := callBounded(t, limit)
		if err == nil {
			t.Fatal("rpcCallOnceCtx = nil, want the write timeout")
		}
		if elapsed := time.Since(start); elapsed > limit {
			t.Fatalf("rpcCallOnceCtx took %s, want within %s", elapsed, limit)
		}
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("error = %v, want a net.Error whose Timeout() is true", err)
		}
		if !isTransientBridgeError(err) {
			t.Fatalf("isTransientBridgeError(%v) = false, want true", err)
		}
	})

	t.Run("multiple candidates", func(t *testing.T) {
		stalled := newStalledWSPeer(t)
		oldDefaults := defaultWSURLCandidates
		defaultWSURLCandidates = []string{closedLoopbackWSURL(t), closedLoopbackWSURL(t)}
		t.Cleanup(func() { defaultWSURLCandidates = oldDefaults })
		t.Setenv("T3_WS_URL", stalled)

		start := time.Now()
		err := callBounded(t, limit)
		if err == nil {
			t.Fatal("rpcCallOnceCtx = nil, want every candidate to fail")
		}
		if elapsed := time.Since(start); elapsed > limit {
			t.Fatalf("rpcCallOnceCtx took %s, want within %s", elapsed, limit)
		}
		if !strings.Contains(err.Error(), "all T3 WebSocket candidates failed") {
			t.Fatalf("error = %v, want the joined candidate failures", err)
		}
	})
}

// tokenServer answers /api/auth/bridge-ws-token with token and a parseable
// expiry, counting requests. When gate is non-nil the answer waits for it
// after signaling entered.
type tokenServer struct {
	*httptest.Server
	mu      sync.Mutex
	hits    int
	entered chan struct{}
}

// newTokenServer starts a ws-token endpoint answering token; a non-nil gate
// holds each answer until it closes.
func newTokenServer(t *testing.T, token string, gate <-chan struct{}) *tokenServer {
	t.Helper()
	ts := &tokenServer{entered: make(chan struct{}, 8)}
	ts.Server = newLoopbackServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.hits++
		ts.mu.Unlock()
		ts.entered <- struct{}{}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":     token,
			"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}), false)
	t.Cleanup(ts.Close)
	return ts
}

// requestCount is how many token requests the server has taken.
func (ts *tokenServer) requestCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.hits
}

// wsURL is the server's ws:// URL.
func (ts *tokenServer) wsURL() string { return "ws" + strings.TrimPrefix(ts.URL, "http") }

// Kills a token fetch that holds the auth lock across its HTTP round trip: a
// caller whose token is already cached must not wait behind a slow fetch for
// another base URL.
func TestIssueWebSocketTokenCacheHitDoesNotWaitForAnotherFetch(t *testing.T) {
	resetBridgeAuthCacheForTest(t)
	serverB := newTokenServer(t, "tok-b", nil)
	if tok, err := issueT3WebSocketToken(t.Context(), serverB.wsURL()); err != nil || tok != "tok-b" {
		t.Fatalf("seeding the cache: token %q, err %v", tok, err)
	}

	gate := make(chan struct{})
	serverA := newTokenServer(t, "tok-a", gate)
	type result struct {
		token string
		err   error
	}
	first := make(chan result, 1)
	go func() {
		tok, err := issueT3WebSocketToken(context.Background(), serverA.wsURL())
		first <- result{tok, err}
	}()
	released := false
	release := func() {
		if !released {
			released = true
			close(gate)
		}
	}
	t.Cleanup(release)
	select {
	case <-serverA.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first caller never reached its token request")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	tok, err := issueT3WebSocketToken(ctx, serverB.wsURL())
	if err != nil || tok != "tok-b" {
		t.Fatalf("cached caller = %q, %v after %s, want the cached token at once", tok, err, time.Since(start))
	}
	if hits := serverB.requestCount(); hits != 1 {
		t.Fatalf("server B token requests = %d, want 1 (the cache hit asks nobody)", hits)
	}

	release()
	select {
	case r := <-first:
		if r.err != nil || r.token != "tok-a" {
			t.Fatalf("first caller = %q, %v, want tok-a", r.token, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first caller never finished after its answer was released")
	}
}

// startPeer is a loopback T3 stand-in for Start: it answers every request,
// serves git.createWorktree after createDelay, and records the tags it saw
// and any plain HTTP request.
type startPeer struct {
	server      *httptest.Server
	createDelay time.Duration

	mu       sync.Mutex
	tags     []string
	payloads map[string]json.RawMessage
	paths    []string
	released chan struct{}
}

// newStartPeer starts the peer and closes it on cleanup.
func newStartPeer(t *testing.T, createDelay time.Duration) *startPeer {
	t.Helper()
	sp := &startPeer{createDelay: createDelay, payloads: make(map[string]json.RawMessage), released: make(chan struct{})}
	upgrader := websocket.Upgrader{}
	sp.server = newLoopbackServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			sp.mu.Lock()
			sp.paths = append(sp.paths, r.URL.Path)
			sp.mu.Unlock()
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var req struct {
			ID      string          `json:"id"`
			Tag     string          `json:"tag"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := conn.ReadJSON(&req); err != nil {
			return
		}
		sp.mu.Lock()
		sp.tags = append(sp.tags, req.Tag)
		sp.payloads[req.Tag] = req.Payload
		sp.mu.Unlock()
		value := map[string]interface{}{}
		if req.Tag == "git.createWorktree" {
			select {
			case <-time.After(sp.createDelay):
			case <-sp.released:
				return
			}
			value = map[string]interface{}{"worktree": map[string]interface{}{"path": "/wt/created", "branch": "feature"}}
		}
		_ = conn.WriteJSON(map[string]interface{}{
			"_tag":      "Exit",
			"requestId": req.ID,
			"exit":      map[string]interface{}{"_tag": "Success", "value": value},
		})
	}), false)
	t.Cleanup(func() {
		close(sp.released)
		sp.server.CloseClientConnections()
		sp.server.Close()
	})
	return sp
}

// wsURL is the peer's ws:// URL.
func (sp *startPeer) wsURL() string { return "ws" + strings.TrimPrefix(sp.server.URL, "http") }

// seenTags is the request tags taken so far, in order.
func (sp *startPeer) seenTags() []string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return append([]string(nil), sp.tags...)
}

// httpPaths is the plain HTTP paths requested so far.
func (sp *startPeer) httpPaths() []string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return append([]string(nil), sp.paths...)
}

// payloadFor is the payload of the last request carrying tag.
func (sp *startPeer) payloadFor(tag string) json.RawMessage {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.payloads[tag]
}

// startEnvelope is a startup envelope that makes Start create a worktree.
func startEnvelope(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{
		"gc":      map[string]interface{}{"cityPath": "/city", "rigPath": "/rig", "agent": "worker", "sessionName": "ctx-session"},
		"runtime": map[string]interface{}{"branch": "main", "provider": "codex", "model": "gpt-5.4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// assertStartCtxError checks the shared shape of a Start that ended on its
// ctx: the ctx error is reachable, the session name is named, and nothing a
// caller would mistake for a retryable or gone session, or a secret, appears.
func assertStartCtxError(t *testing.T, err, want error, name string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("Start error = %v, want %v", err, want)
	}
	if errors.Is(err, runtime.ErrSessionInitializing) {
		t.Fatalf("Start error = %v, must not be ErrSessionInitializing: the caller would retry a start whose ctx ended", err)
	}
	if runtime.IsSessionGone(err) {
		t.Fatalf("Start error = %v, must not read as a gone session", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, fmt.Sprintf("%q", name)) {
		t.Fatalf("Start error = %q, want it to name the session %q", msg, name)
	}
	for _, leak := range []string{"ws://", "wss://", "Bearer", "token"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("Start error = %q, must not carry %q", msg, leak)
		}
	}
}

// Kills a Start that discards its ctx: a ctx that ended before the call must
// stop it before it asks T3 for anything.
func TestStartStopsBeforeAnyRPCWhenCtxIsCancelled(t *testing.T) {
	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("T3_BEARER_TOKEN", "")
	peer := newStartPeer(t, 0)
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	t.Cleanup(func() { defaultWSURLCandidates = oldDefaults })
	t.Setenv("T3_WS_URL", peer.wsURL())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}
	err := p.Start(ctx, "ctx-session", runtime.Config{
		WorkDir: "/wt/want",
		Env:     map[string]string{"GC_STARTUP_ENVELOPE": startEnvelope(t)},
	})
	assertStartCtxError(t, err, context.Canceled, "ctx-session")
	if tags := peer.seenTags(); len(tags) != 0 {
		t.Fatalf("Start sent %v after its ctx was canceled, want no RPC", tags)
	}
	if paths := peer.httpPaths(); len(paths) != 0 {
		t.Fatalf("Start made HTTP requests %v after its ctx was canceled, want none", paths)
	}
}

// Kills a Start that finishes its worktree phase past its ctx deadline and
// goes on: it must remove the worktree it created and return the deadline.
func TestStartRollsBackItsWorktreeWhenCtxExpiresDuringCreate(t *testing.T) {
	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("T3_BEARER_TOKEN", "")
	peer := newStartPeer(t, 200*time.Millisecond)
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	t.Cleanup(func() { defaultWSURLCandidates = oldDefaults })
	t.Setenv("T3_WS_URL", peer.wsURL())

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}
	start := time.Now()
	err := p.Start(ctx, "ctx-session", runtime.Config{
		WorkDir: "/wt/want",
		Env:     map[string]string{"GC_STARTUP_ENVELOPE": startEnvelope(t)},
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Start took %s past a 100ms deadline", elapsed)
	}
	assertStartCtxError(t, err, context.DeadlineExceeded, "ctx-session")

	tags := peer.seenTags()
	want := []string{"git.createWorktree", "git.removeWorktree"}
	if strings.Join(tags, ",") != strings.Join(want, ",") {
		t.Fatalf("requests = %v, want %v: the created worktree is removed and nothing else is asked", tags, want)
	}
	var removed struct {
		Cwd  string `json:"cwd"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(peer.payloadFor("git.removeWorktree"), &removed); err != nil {
		t.Fatalf("decode removeWorktree payload: %v", err)
	}
	if removed.Cwd != "/rig" || removed.Path != "/wt/created" {
		t.Fatalf("removeWorktree = %+v, want cwd /rig and the path the create returned", removed)
	}
}

// countdownCtx is a ctx that reports itself live for its first live Err calls
// and context.Canceled from then on. Start asks its own ctx only at its phase
// boundaries (every RPC it makes runs on context.Background), so live is the
// number of boundaries a start may pass, and a test ends the ctx at any one of
// them without a timer. Done is never closed: Start does not select on it.
type countdownCtx struct {
	context.Context
	mu    sync.Mutex
	live  int
	asked int
}

// newCountdownCtx returns a ctx whose Err is nil live times, then context.Canceled.
func newCountdownCtx(live int) *countdownCtx {
	return &countdownCtx{Context: context.Background(), live: live}
}

// Err is nil for the first live calls and context.Canceled for every later one.
func (c *countdownCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked++
	if c.asked > c.live {
		return context.Canceled
	}
	return nil
}

// questions is how many times the ctx has been asked whether it ended.
func (c *countdownCtx) questions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked
}

// countCommands is how many of the dispatched command types are typ.
func countCommands(types []string, typ string) int {
	n := 0
	for _, got := range types {
		if got == typ {
			n++
		}
	}
	return n
}

// runPhasedStart runs a fresh Start (no worktree, no thread to reuse) with a
// startup prompt and a nudge against the shared fake T3 server, on a ctx that
// stays live for its first live boundary checks. The start has seven of them:
// entry, the snapshot read, project.create, thread.create, the event watcher,
// the startup prompt turn and the nudge turn.
func runPhasedStart(t *testing.T, live int) (*Provider, *t3BridgeTestServer, *countdownCtx, error) {
	t.Helper()
	server := newT3BridgeTestServer(t, map[string]interface{}{})
	t.Cleanup(server.Close)
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	t.Cleanup(func() { defaultWSURLCandidates = oldDefaults })
	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
	t.Setenv("T3_WS_URL", server.wsURL())
	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}
	t.Cleanup(func() { p.stopEventWatcher("phase-session") })
	ctx := newCountdownCtx(live)
	err := p.Start(ctx, "phase-session", runtime.Config{
		WorkDir:      "/tmp/phase-work",
		Command:      "codex",
		PromptSuffix: "gc prime --hook",
		Nudge:        "Check mail and hook status, then act accordingly.",
		Env: map[string]string{
			"GC_ALIAS":    "phase-agent",
			"GC_TEMPLATE": "phase-agent",
			"GC_PROVIDER": "codex",
			"GC_MODEL":    "gpt-5.4",
		},
	})
	return p, server, ctx, err
}

// Kills a Start that drops one of its phase-boundary ctx checks, or moves it:
// the ctx ends after live boundaries, and the start must stop at the next one
// with exactly the requests before that boundary sent and none after it.
// Dropping or moving any check shifts every later row.
func TestStartStopsAtEachPhaseBoundaryOnceCtxEnds(t *testing.T) {
	rows := []struct {
		name          string
		live          int
		snapshotReads int
		projects      int
		threads       int
		turns         int
		watchers      int
	}{
		{name: "entry", live: 0},
		{name: "snapshot read", live: 1},
		{name: "project create", live: 2, snapshotReads: 1},
		{name: "thread create", live: 3, snapshotReads: 1, projects: 1},
		{name: "event watcher", live: 4, snapshotReads: 1, projects: 1, threads: 1},
		{name: "startup prompt turn", live: 5, snapshotReads: 1, projects: 1, threads: 1, watchers: 1},
		{name: "nudge turn", live: 6, snapshotReads: 1, projects: 1, threads: 1, turns: 1, watchers: 1},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			p, server, ctx, err := runPhasedStart(t, row.live)
			assertStartCtxError(t, err, context.Canceled, "phase-session")
			if got := ctx.questions(); got != row.live+1 {
				t.Fatalf("Start asked its ctx %d times, want %d: it stops asking once a check fails", got, row.live+1)
			}
			types := server.commandTypes()
			if got := server.snapshotCalls(); got != row.snapshotReads {
				t.Fatalf("snapshot reads = %d, want %d (commands %v)", got, row.snapshotReads, types)
			}
			for typ, want := range map[string]int{
				"project.create":    row.projects,
				"thread.create":     row.threads,
				"thread.turn.start": row.turns,
			} {
				if got := countCommands(types, typ); got != want {
					t.Fatalf("%s commands = %d, want %d (commands %v)", typ, got, want, types)
				}
			}
			p.mu.Lock()
			watchers := len(p.watchers)
			p.mu.Unlock()
			if watchers != row.watchers {
				t.Fatalf("event watchers = %d, want %d", watchers, row.watchers)
			}
		})
	}

	t.Run("completes while every boundary is live", func(t *testing.T) {
		_, server, ctx, err := runPhasedStart(t, 7)
		if err != nil {
			t.Fatalf("Start = %v, want nil on a ctx that never ended", err)
		}
		if got := ctx.questions(); got != 7 {
			t.Fatalf("Start asked its ctx %d times, want 7 boundaries", got)
		}
		if got := countCommands(server.commandTypes(), "thread.turn.start"); got != 2 {
			t.Fatalf("thread.turn.start commands = %d, want 2 (the startup prompt and the nudge)", got)
		}
	})
}

// Kills a Start that creates its worktree without asking its ctx first: a ctx
// that ends right after the entry check must stop the start before it asks T3
// for anything.
func TestStartChecksCtxBeforeCreatingAWorktree(t *testing.T) {
	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("T3_BEARER_TOKEN", "")
	peer := newStartPeer(t, 0)
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	t.Cleanup(func() { defaultWSURLCandidates = oldDefaults })
	t.Setenv("T3_WS_URL", peer.wsURL())

	ctx := newCountdownCtx(1)
	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}
	err := p.Start(ctx, "ctx-session", runtime.Config{
		WorkDir: "/wt/want",
		Env:     map[string]string{"GC_STARTUP_ENVELOPE": startEnvelope(t)},
	})
	assertStartCtxError(t, err, context.Canceled, "ctx-session")
	if got := ctx.questions(); got != 2 {
		t.Fatalf("Start asked its ctx %d times, want 2: the entry check and the one before the worktree create", got)
	}
	if tags := peer.seenTags(); len(tags) != 0 {
		t.Fatalf("Start sent %v after its ctx ended before the worktree create, want no RPC", tags)
	}
}

// Kills a Start that checks its ctx inside the reuse branch: a thread's reuse
// is not interrupted once it begins, so a ctx that ends right after the
// snapshot read must not stop it.
func TestStartDoesNotCheckCtxInsideTheReuseBranch(t *testing.T) {
	server := newT3BridgeTestServer(t, map[string]interface{}{
		"projects": []interface{}{
			map[string]interface{}{"id": "project-1", "workspaceRoot": "/tmp/reuse-work"},
		},
		"threads": []interface{}{
			map[string]interface{}{
				"id":        "thread-1",
				"projectId": "project-1",
				"title":     "reuse-agent · reuse-session",
				"model":     "gpt-5.4",
				"customMetadata": map[string]interface{}{
					"gc.agent":           "reuse-agent",
					"gc.sessionName":     "reuse-session",
					"gc.startupTemplate": "reuse-agent",
					"gc.startupWorkDir":  "/tmp/reuse-work",
					"gc.runtimeProvider": "codex",
					"gc.startupModel":    "gpt-5.4",
				},
				"session": map[string]interface{}{"status": "ready"},
			},
		},
	})
	t.Cleanup(server.Close)
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	t.Cleanup(func() { defaultWSURLCandidates = oldDefaults })
	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
	t.Setenv("T3_WS_URL", server.wsURL())

	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}
	t.Cleanup(func() { p.stopEventWatcher("reuse-session") })
	ctx := newCountdownCtx(2) // the entry check and the one before the snapshot read pass
	err := p.Start(ctx, "reuse-session", runtime.Config{
		WorkDir:      "/tmp/reuse-work",
		Command:      "codex",
		PromptSuffix: "gc prime --hook",
		Nudge:        "Check mail and hook status, then act accordingly.",
		Env: map[string]string{
			"GC_ALIAS":    "reuse-agent",
			"GC_TEMPLATE": "reuse-agent",
			"GC_PROVIDER": "codex",
			"GC_MODEL":    "gpt-5.4",
		},
	})
	if err != nil {
		t.Fatalf("Start(reuse) = %v, want nil: a reuse in progress is not interrupted", err)
	}
	if got := ctx.questions(); got != 2 {
		t.Fatalf("Start asked its ctx %d times, want 2: no check belongs inside the reuse branch", got)
	}
	types := server.commandTypes()
	if countCommands(types, "thread.meta.update") == 0 {
		t.Fatalf("no thread.meta.update in %v: the start did not take the reuse branch", types)
	}
	for _, typ := range []string{"project.create", "thread.create", "thread.turn.start"} {
		if got := countCommands(types, typ); got != 0 {
			t.Fatalf("%s commands = %d in %v, want 0 on a reused thread", typ, got, types)
		}
	}
}

// Kills a joined error that drops the candidate whose write stalled: the call
// that fails every candidate names each of them, the stalled one included, so
// an operator reading the error can tell which URL hung.
func TestRPCCallOnceNamesTheStalledCandidateInItsJoinedError(t *testing.T) {
	shrinkWriteTimeout(t, stalledWriteTimeout)
	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("T3_BEARER_TOKEN", "")
	stalled := newStalledWSPeer(t)
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = []string{closedLoopbackWSURL(t), closedLoopbackWSURL(t)}
	t.Cleanup(func() { defaultWSURLCandidates = oldDefaults })
	t.Setenv("T3_WS_URL", stalled)

	err := callBounded(t, stalledWriteTimeout+2*time.Second)
	if err == nil {
		t.Fatal("rpcCallOnceCtx = nil, want every candidate to fail")
	}
	if !strings.Contains(err.Error(), stalled) {
		t.Fatalf("error = %v, want it to name the stalled candidate %s", err, stalled)
	}
}

// Kills a token fetch that stores its result without the auth lock: the store
// is the one step of the two-phase fetch that writes the shared cache, so it
// must wait for the lock like every reader does. The fake token server takes
// the auth lock itself once the caller has dropped it for the POST, answers,
// and keeps the lock until released; the caller's store must not finish while
// that lock is held, and must finish, and be served from the cache, after it.
func TestIssueWebSocketTokenStoreWaitsForTheAuthLock(t *testing.T) {
	resetBridgeAuthCacheForTest(t)
	held := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	var mu sync.Mutex
	requests := 0
	srv := newLoopbackServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		first := requests == 1
		mu.Unlock()
		if !first {
			http.Error(w, "unexpected second token request", http.StatusInternalServerError)
			return
		}
		if err := lockBridgeAuth(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer unlockBridgeAuth()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":     "tok-stored",
			"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(held)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}), false)
	t.Cleanup(srv.Close)
	// Registered after srv.Close, so it runs first: Close waits for the parked handler.
	t.Cleanup(letGo)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	type result struct {
		token string
		err   error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() {
		tok, err := issueT3WebSocketToken(ctx, wsURL)
		done <- result{tok, err}
	}()
	select {
	case <-held:
	case r := <-done:
		t.Fatalf("the fetch ended (%q, %v) before the token server held the auth lock: the caller kept the lock across its POST", r.token, r.err)
	case <-time.After(5 * time.Second):
		t.Fatal("the token server never took the auth lock")
	}
	select {
	case r := <-done:
		t.Fatalf("the fetch returned (%q, %v) while the auth lock was held elsewhere: its cache store did not wait for the lock", r.token, r.err)
	case <-time.After(200 * time.Millisecond):
	}

	letGo()
	select {
	case r := <-done:
		if r.err != nil || r.token != "tok-stored" {
			t.Fatalf("the fetch after the lock was released = %q, %v, want tok-stored", r.token, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fetch never finished after the auth lock was released")
	}
	tok, err := issueT3WebSocketToken(t.Context(), wsURL)
	if err != nil || tok != "tok-stored" {
		t.Fatalf("second fetch = %q, %v, want the cached tok-stored", tok, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("token requests = %d, want 1: the store left the cache empty", requests)
	}
}

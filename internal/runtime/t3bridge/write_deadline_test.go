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

package t3bridge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gorilla/websocket"
)

const turnFenceSession = "agent-a"

// turnSnapshot is a snapshot holding turnFenceSession's thread with session
// set verbatim; nil leaves the thread without a provider session.
func turnSnapshot(session map[string]interface{}) map[string]interface{} {
	thread := map[string]interface{}{
		"id":             "thread-agent-a",
		"projectId":      "project-1",
		"customMetadata": map[string]interface{}{"gc.sessionName": turnFenceSession},
		"session":        nil,
	}
	if session != nil {
		thread["session"] = session
	}
	return map[string]interface{}{"threads": []interface{}{thread}}
}

// turnRunning is a snapshot in which turnFenceSession runs turn id.
func turnRunning(id string) map[string]interface{} {
	return turnSnapshot(map[string]interface{}{"status": "running", "activeTurnId": id})
}

// turnIdle is a snapshot in which turnFenceSession's session is ready.
func turnIdle() map[string]interface{} {
	return turnSnapshot(map[string]interface{}{"status": "ready", "activeTurnId": nil})
}

// newTurnFenceTestProvider is a bridge reading server, with the turn polls
// shortened.
func newTurnFenceTestProvider(t *testing.T, server *t3BridgeTestServer) *Provider {
	t.Helper()
	resetBridgeAuthCacheForTest(t)
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	oldInitial, oldMax := turnPollInitial, turnPollMax
	turnPollInitial, turnPollMax = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() {
		defaultWSURLCandidates = oldDefaults
		turnPollInitial, turnPollMax = oldInitial, oldMax
	})
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("GC_T3BRIDGE_STATE_DIR", t.TempDir())
	t.Setenv("T3_WS_URL", server.wsURL())
	return &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}
}

func newTurnFenceServer(t *testing.T, seq ...map[string]interface{}) *t3BridgeTestServer {
	t.Helper()
	server := newT3BridgeTestServer(t, map[string]interface{}{"threads": []interface{}{}})
	t.Cleanup(server.Close)
	server.setSnapshots(seq...)
	return server
}

// Kills a turn read that drops the thread from the id, loses the turn, or
// reads a stale or absent session as running: D2's classification.
func TestObserveTurnClassifiesTheThreadSession(t *testing.T) {
	cases := []struct {
		name    string
		session map[string]interface{}
		want    string
		unavail bool
	}{
		{name: "running", session: map[string]interface{}{"status": "running", "activeTurnId": "t1"}, want: "thread-agent-a/t1"},
		{name: "ready", session: map[string]interface{}{"status": "ready", "activeTurnId": nil}},
		{name: "idle", session: map[string]interface{}{"status": "idle"}},
		{name: "starting", session: map[string]interface{}{"status": "starting", "activeTurnId": nil}},
		{name: "null session"},
		{name: "stopped with a stale id", session: map[string]interface{}{"status": "stopped", "activeTurnId": "t1"}},
		{name: "interrupted with a stale id", session: map[string]interface{}{"status": "interrupted", "activeTurnId": "t1"}},
		{name: "error with a stale id", session: map[string]interface{}{"status": "error", "activeTurnId": "t1"}},
		{name: "running without an id", session: map[string]interface{}{"status": "running", "activeTurnId": nil}, unavail: true},
		{name: "unknown status", session: map[string]interface{}{"status": "future-state", "activeTurnId": "t1"}, unavail: true},
		{name: "missing status", session: map[string]interface{}{"activeTurnId": "t1"}, unavail: true},
		{name: "malformed id", session: map[string]interface{}{"status": "running", "activeTurnId": 7}, unavail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newTurnFenceServer(t, turnSnapshot(tc.session))
			p := newTurnFenceTestProvider(t, server)
			st, err := p.ObserveTurn(t.Context(), turnFenceSession)
			if tc.unavail {
				if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("ObserveTurn = %+v, %v; want ErrRuntimeUnavailable", st, err)
				}
				return
			}
			if err != nil || st.TurnID != tc.want {
				t.Fatalf("ObserveTurn = %+v, %v; want turn %q", st, err, tc.want)
			}
		})
	}
}

// Kills a turn read served from the 10 s cache, and one that leaves the cache
// stale for the bridge's other readers.
func TestObserveTurnBypassesAndRefreshesTheSnapshotCache(t *testing.T) {
	server := newTurnFenceServer(t, turnIdle(), turnRunning("t1"), turnIdle())
	p := newTurnFenceTestProvider(t, server)
	if _, err := p.rpcSnapshot(); err != nil {
		t.Fatalf("warming the cache: %v", err)
	}
	st, err := p.ObserveTurn(t.Context(), turnFenceSession)
	if err != nil || st.TurnID != "thread-agent-a/t1" {
		t.Fatalf("ObserveTurn with a warm cache = %+v, %v; want the fresh t1", st, err)
	}
	if calls := server.snapshotCalls(); calls != 2 {
		t.Fatalf("snapshot reads = %d, want 2: the turn read must not use the cache", calls)
	}
	cached, err := p.rpcSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if calls := server.snapshotCalls(); calls != 2 {
		t.Fatalf("snapshot reads = %d after a cached read, want 2", calls)
	}
	if got := snapshotThreadSessionStatus(snapshotThreadBySessionName(cached, turnFenceSession)); got != "running" {
		t.Fatalf("cached status = %q, want the turn read's running", got)
	}
}

// Kills a turn read that calls a missing thread unavailable, or a thread not
// yet visible inside the start grace gone.
func TestObserveTurnWithoutAThread(t *testing.T) {
	server := newTurnFenceServer(t, map[string]interface{}{"threads": []interface{}{}})
	p := newTurnFenceTestProvider(t, server)
	if _, err := p.ObserveTurn(t.Context(), turnFenceSession); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("no thread = %v, want ErrSessionNotFound", err)
	}
	p.setRecentStart(turnFenceSession, time.Now())
	_, err := p.ObserveTurn(t.Context(), turnFenceSession)
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("no thread inside the start grace = %v, want only ErrRuntimeUnavailable", err)
	}
}

// Kills a turn read that reports an unreachable T3 as anything but
// unavailable.
func TestObserveTurnUnreachableIsUnavailable(t *testing.T) {
	server := newTurnFenceServer(t)
	p := newTurnFenceTestProvider(t, server)
	server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := p.ObserveTurn(ctx, turnFenceSession); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("unreachable = %v, want ErrRuntimeUnavailable", err)
	}
}

// Kills a turn read that outlives its caller's deadline on a stalled T3,
// whether the snapshot comes over HTTP (a bearer token) or the WebSocket (an
// unauthenticated loopback), or one that loses the deadline from its error.
// Production resolves two or more candidates and flattens their failures, so
// the server is reached through a second, localhost candidate too.
func TestObserveTurnStalledServerReturnsByTheDeadline(t *testing.T) {
	for _, bearer := range []string{"test-bearer", ""} {
		t.Run("bearer="+bearer, func(t *testing.T) {
			server := newTurnFenceServer(t)
			server.setStalled()
			p := newTurnFenceTestProvider(t, server)
			t.Setenv("T3_BEARER_TOKEN", bearer)
			defaultWSURLCandidates = []string{strings.Replace(server.wsURL(), "127.0.0.1", "localhost", 1)}

			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := p.ObserveTurn(ctx, turnFenceSession)
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Fatalf("ObserveTurn took %s past a 200ms deadline", elapsed)
			}
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("stalled = %v, want ErrRuntimeUnavailable wrapping the deadline", err)
			}
		})
	}
}

// Kills an InterruptTurn that dispatches without the turn, reports a fence V1
// cannot give, or leaves the cache naming the interrupted turn.
func TestInterruptTurnDispatchesForTheCurrentTurn(t *testing.T) {
	server := newTurnFenceServer(t, turnRunning("t1"), turnIdle())
	p := newTurnFenceTestProvider(t, server)
	var readsAtDispatch atomic.Int64
	readsAtDispatch.Store(-1)
	server.setCommandHook(func(map[string]interface{}) { readsAtDispatch.Store(int64(server.snapshotCalls())) })

	ack, err := p.InterruptTurn(t.Context(), turnFenceSession, "thread-agent-a/t1")
	if err != nil || ack.Fenced {
		t.Fatalf("InterruptTurn = %+v, %v; want sent, unfenced", ack, err)
	}
	if got := readsAtDispatch.Load(); got != 1 {
		t.Fatalf("snapshot reads before the dispatch = %d, want the one fence read", got)
	}
	payloads := server.commandPayloads("thread.turn.interrupt")
	if len(payloads) != 1 {
		t.Fatalf("interrupts = %+v, want one", payloads)
	}
	got := payloads[0]
	if got["threadId"] != "thread-agent-a" || got["turnId"] != "t1" {
		t.Fatalf("interrupt = %+v, want threadId thread-agent-a and the raw turnId t1", got)
	}
	if id, _ := got["commandId"].(string); !strings.HasPrefix(id, "t3bridge-interrupt-") {
		t.Fatalf("commandId = %q, want a fresh t3bridge-interrupt id", id)
	}
	if created, _ := got["createdAt"].(string); created == "" {
		t.Fatal("interrupt has no createdAt")
	}
	if _, err := p.rpcSnapshot(); err != nil {
		t.Fatal(err)
	}
	if calls := server.snapshotCalls(); calls != 2 {
		t.Fatalf("snapshot reads after the interrupt = %d, want 2: the cache must be cleared", calls)
	}
}

// Kills an InterruptTurn that interrupts a turn the caller did not name: a
// later turn, a recreated thread's turn of the same id, or no turn at all.
func TestInterruptTurnMismatchDispatchesNothing(t *testing.T) {
	cases := []struct {
		name     string
		snapshot map[string]interface{}
		turnID   string
		current  string
	}{
		{name: "later turn", snapshot: turnRunning("t2"), turnID: "thread-agent-a/t1", current: "thread-agent-a/t2"},
		{name: "recreated thread", snapshot: turnRunning("t1"), turnID: "thread-old/t1", current: "thread-agent-a/t1"},
		{name: "idle", snapshot: turnIdle(), turnID: "thread-agent-a/t1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newTurnFenceServer(t, tc.snapshot)
			p := newTurnFenceTestProvider(t, server)
			_, err := p.InterruptTurn(t.Context(), turnFenceSession, tc.turnID)
			var nc *runtime.TurnNotCurrentError
			if !errors.As(err, &nc) || nc.Current.TurnID != tc.current || nc.TurnID != tc.turnID || nc.Session != turnFenceSession {
				t.Fatalf("InterruptTurn = %v, want not-current reading %q", err, tc.current)
			}
			if cmds := server.commandTypes(); len(cmds) != 0 {
				t.Fatalf("commands = %q, want none", cmds)
			}
		})
	}
}

// Kills an InterruptTurn that reports T3's refusal as a stale turn.
func TestInterruptTurnRejectionIsNotTurnNotCurrent(t *testing.T) {
	server := newTurnFenceServer(t, turnRunning("t1"))
	server.rejectCommand("thread.turn.interrupt", "no active session")
	p := newTurnFenceTestProvider(t, server)
	_, err := p.InterruptTurn(t.Context(), turnFenceSession, "thread-agent-a/t1")
	if err == nil || errors.Is(err, runtime.ErrTurnNotCurrent) {
		t.Fatalf("rejected interrupt = %v, want an error that is not ErrTurnNotCurrent", err)
	}
}

// Kills a wait that stops before the turn ends, misses a replacement turn,
// ends on an outage, treats a removed thread as running, outlives its bound
// or its caller, or reports its own bound as the caller's deadline.
func TestWaitTurnEnded(t *testing.T) {
	const t1 = "thread-agent-a/t1"
	empty := map[string]interface{}{"threads": []interface{}{}}
	cases := []struct {
		name  string
		seq   []map[string]interface{}
		reads int
		want  string
	}{
		{name: "ends", seq: []map[string]interface{}{turnRunning("t1"), turnRunning("t1"), turnIdle()}, reads: 3},
		{name: "replaced", seq: []map[string]interface{}{turnRunning("t1"), turnRunning("t2")}, reads: 2, want: "thread-agent-a/t2"},
		{name: "outage then ends", seq: []map[string]interface{}{nil, turnRunning("t1"), turnIdle()}, reads: 3},
		{name: "thread removed", seq: []map[string]interface{}{turnRunning("t1"), empty}, reads: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newTurnFenceServer(t, tc.seq...)
			p := newTurnFenceTestProvider(t, server)
			st, err := p.WaitTurnEnded(t.Context(), turnFenceSession, t1, 10*time.Second)
			if err != nil || st.TurnID != tc.want {
				t.Fatalf("WaitTurnEnded = %+v, %v; want %q", st, err, tc.want)
			}
			if calls := server.snapshotCalls(); calls != tc.reads {
				t.Fatalf("snapshot reads = %d, want %d", calls, tc.reads)
			}
		})
	}

	t.Run("never ends", func(t *testing.T) {
		server := newTurnFenceServer(t, turnRunning("t1"))
		p := newTurnFenceTestProvider(t, server)
		start := time.Now()
		st, err := p.WaitTurnEnded(t.Context(), turnFenceSession, t1, 100*time.Millisecond)
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("WaitTurnEnded took %s past its 100ms bound", elapsed)
		}
		if !errors.Is(err, runtime.ErrTurnNotEnded) || st.TurnID != t1 {
			t.Fatalf("WaitTurnEnded = %+v, %v; want ErrTurnNotEnded with the last read", st, err)
		}
		if errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("WaitTurnEnded = %v: a wait whose reads succeeded carries no read error", err)
		}
	})

	t.Run("never read", func(t *testing.T) {
		server := newTurnFenceServer(t, nil)
		p := newTurnFenceTestProvider(t, server)
		_, err := p.WaitTurnEnded(t.Context(), turnFenceSession, t1, 100*time.Millisecond)
		if !errors.Is(err, runtime.ErrTurnNotEnded) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("WaitTurnEnded = %v; want ErrTurnNotEnded with the last read error", err)
		}
	})

	t.Run("stalled read", func(t *testing.T) {
		server := newTurnFenceServer(t, turnRunning("t1"))
		server.setStalled()
		p := newTurnFenceTestProvider(t, server)
		_, err := p.WaitTurnEnded(t.Context(), turnFenceSession, t1, 100*time.Millisecond)
		if !errors.Is(err, runtime.ErrTurnNotEnded) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("WaitTurnEnded = %v; want ErrTurnNotEnded with the last read error", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitTurnEnded = %v: the wait's own bound cut the read, not the caller's deadline", err)
		}
		if n := strings.Count(err.Error(), runtime.ErrRuntimeUnavailable.Error()); n != 1 {
			t.Fatalf("WaitTurnEnded = %v: names the outage %d times, want once", err, n)
		}
	})

	t.Run("caller canceled", func(t *testing.T) {
		server := newTurnFenceServer(t, turnRunning("t1"))
		p := newTurnFenceTestProvider(t, server)
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(30*time.Millisecond, cancel)
		_, err := p.WaitTurnEnded(ctx, turnFenceSession, t1, 10*time.Second)
		if !errors.Is(err, context.Canceled) || errors.Is(err, runtime.ErrTurnNotEnded) {
			t.Fatalf("WaitTurnEnded = %v; want the caller's context.Canceled", err)
		}
	})
}

// Kills a production provider that hides the capability behind the seam
// adapter, or a seam-backed provider that does not reach the raw bridge.
func TestSeamBackedImplementsTurnFence(t *testing.T) {
	server := newTurnFenceServer(t, turnRunning("t1"))
	_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
	tf, ok := sp.(runtime.TurnFenceProvider)
	if !ok {
		t.Fatalf("NewSeamBacked() = %T, which lacks runtime.TurnFenceProvider", sp)
	}
	if st, err := tf.ObserveTurn(t.Context(), turnFenceSession); err != nil || st.TurnID != "thread-agent-a/t1" {
		t.Fatalf("seam-backed ObserveTurn = %+v, %v; want t1", st, err)
	}
	if _, err := tf.InterruptTurn(t.Context(), turnFenceSession, "thread-agent-a/t2"); !errors.Is(err, runtime.ErrTurnNotCurrent) {
		t.Fatalf("seam-backed InterruptTurn = %v, want ErrTurnNotCurrent", err)
	}
	oldInitial := turnPollInitial
	turnPollInitial = time.Millisecond
	t.Cleanup(func() { turnPollInitial = oldInitial })
	if _, err := tf.WaitTurnEnded(t.Context(), turnFenceSession, "thread-agent-a/t1", 20*time.Millisecond); !errors.Is(err, runtime.ErrTurnNotEnded) {
		t.Fatalf("seam-backed WaitTurnEnded = %v, want ErrTurnNotEnded", err)
	}
}

// Kills an InterruptTurn or WaitTurnEnded that accepts an empty turn id: on an
// idle session the empty id equals the read and an unfenced interrupt goes
// out, and a wait on it would wait for some turn to start.
func TestTurnFenceRefusesAnEmptyTurnIDBeforeReading(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot map[string]interface{}
	}{
		{name: "idle", snapshot: turnIdle()},
		{name: "running", snapshot: turnRunning("t1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newTurnFenceServer(t, tc.snapshot)
			p := newTurnFenceTestProvider(t, server)
			_, err := p.InterruptTurn(t.Context(), turnFenceSession, "")
			if !errors.Is(err, runtime.ErrTurnUnnamed) || errors.Is(err, runtime.ErrTurnNotCurrent) {
				t.Fatalf("InterruptTurn(\"\") = %v, want only ErrTurnUnnamed", err)
			}
			if _, err := p.WaitTurnEnded(t.Context(), turnFenceSession, "", time.Second); !errors.Is(err, runtime.ErrTurnUnnamed) {
				t.Fatalf("WaitTurnEnded(\"\") = %v, want ErrTurnUnnamed", err)
			}
			if calls := server.snapshotCalls(); calls != 0 {
				t.Fatalf("snapshot reads = %d, want none", calls)
			}
			if cmds := server.commandTypes(); len(cmds) != 0 {
				t.Fatalf("commands = %q, want none", cmds)
			}
		})
	}
}

// Kills an InterruptTurn whose fence read is served from the 10 s cache: the
// cache still holds t1 while T3 runs t2, and t1 must not be interrupted.
func TestInterruptTurnFenceReadBypassesTheCache(t *testing.T) {
	server := newTurnFenceServer(t, turnRunning("t1"), turnRunning("t2"))
	p := newTurnFenceTestProvider(t, server)
	if _, err := p.rpcSnapshot(); err != nil {
		t.Fatalf("warming the cache: %v", err)
	}
	_, err := p.InterruptTurn(t.Context(), turnFenceSession, "thread-agent-a/t1")
	var nc *runtime.TurnNotCurrentError
	if !errors.As(err, &nc) || nc.Current.TurnID != "thread-agent-a/t2" {
		t.Fatalf("InterruptTurn with a warm stale cache = %v, want not-current reading t2", err)
	}
	if cmds := server.commandTypes(); len(cmds) != 0 {
		t.Fatalf("commands = %q, want none", cmds)
	}
	if calls := server.snapshotCalls(); calls != 2 {
		t.Fatalf("snapshot reads = %d, want 2: the fence read must not use the cache", calls)
	}
}

// Kills a WebSocket turn read that ignores a cancel without a deadline (it
// would block on the 30 s read deadline), and one that reports the canceled
// read as anything but unavailable.
func TestObserveTurnCanceledReadIsUnavailable(t *testing.T) {
	server := newTurnFenceServer(t)
	server.setStalled()
	p := newTurnFenceTestProvider(t, server)
	t.Setenv("T3_BEARER_TOKEN", "")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := p.ObserveTurn(ctx, turnFenceSession)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ObserveTurn took %s after a cancel at 100ms", elapsed)
	}
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v, want ErrRuntimeUnavailable wrapping context.Canceled", err)
	}
}

// Kills a websocket-ticket request or WebSocket dial that ignores ctx: either
// would hold InterruptTurn for the HTTP or handshake timeout, and then the
// transient-retry window, past its deadline. The ticket stalls on the
// dispatch; the handshake stalls on the fence read, which is a WebSocket read
// without a bearer token. The stalled ticket is reached through a second,
// localhost candidate too, as production's flattened failures lose the
// deadline unless it is wrapped again.
func TestInterruptTurnDispatchStopsAtTheDeadline(t *testing.T) {
	interrupt := func(t *testing.T, p *Provider) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := p.InterruptTurn(ctx, turnFenceSession, "thread-agent-a/t1")
		// Under bridgeWSTimeout (3 s), so a handshake left to its own timeout
		// fails the test.
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("InterruptTurn took %s past a 200ms deadline", elapsed)
		}
		return err
	}
	t.Run("stalled ticket", func(t *testing.T) {
		server := newTurnFenceServer(t, turnRunning("t1"))
		p := newTurnFenceTestProvider(t, server)
		defaultWSURLCandidates = []string{strings.Replace(server.wsURL(), "127.0.0.1", "localhost", 1)}
		if _, err := p.ObserveTurn(t.Context(), turnFenceSession); err != nil {
			t.Fatalf("fence read over HTTP: %v", err)
		}
		server.setAuthStalled()
		if err := interrupt(t, p); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stalled ticket = %v, want the deadline", err)
		}
		if cmds := server.commandTypes(); len(cmds) != 0 {
			t.Fatalf("commands = %q, want none", cmds)
		}
	})
	t.Run("stalled handshake", func(t *testing.T) {
		server := newTurnFenceServer(t, turnRunning("t1"))
		p := newTurnFenceTestProvider(t, server)
		t.Setenv("T3_BEARER_TOKEN", "")
		server.setUpgradeStalled()
		if err := interrupt(t, p); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("stalled handshake = %v, want ErrRuntimeUnavailable", err)
		}
	})
}

// Kills a fenced dispatch that retries a transient failure without reading
// the turn again: the retry would reach T3 seconds after the fence read and
// interrupt whatever turn runs then. A retry goes out only while the named
// turn still runs; otherwise the call fails as unavailable, never as
// not-current, because an earlier attempt may have reached T3.
func TestInterruptTurnRefencesBeforeARetry(t *testing.T) {
	t.Run("turn changed", func(t *testing.T) {
		server := newTurnFenceServer(t, turnRunning("t1"), turnRunning("t2"))
		p := newTurnFenceTestProvider(t, server)
		server.setAuthFailures(1, http.StatusServiceUnavailable, "warming up")
		_, err := p.InterruptTurn(t.Context(), turnFenceSession, "thread-agent-a/t1")
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotCurrent) {
			t.Fatalf("InterruptTurn = %v, want only ErrRuntimeUnavailable", err)
		}
		if cmds := server.commandTypes(); len(cmds) != 0 {
			t.Fatalf("commands = %q, want none", cmds)
		}
		if calls := server.snapshotCalls(); calls != 2 {
			t.Fatalf("snapshot reads = %d, want the fence read and one re-read", calls)
		}
	})
	t.Run("turn unchanged", func(t *testing.T) {
		server := newTurnFenceServer(t, turnRunning("t1"))
		p := newTurnFenceTestProvider(t, server)
		server.setAuthFailures(1, http.StatusServiceUnavailable, "warming up")
		var readsAtDispatch atomic.Int64
		server.setCommandHook(func(map[string]interface{}) { readsAtDispatch.Store(int64(server.snapshotCalls())) })
		if _, err := p.InterruptTurn(t.Context(), turnFenceSession, "thread-agent-a/t1"); err != nil {
			t.Fatalf("InterruptTurn = %v, want the retry sent", err)
		}
		if got := readsAtDispatch.Load(); got != 2 {
			t.Fatalf("snapshot reads before the retried dispatch = %d, want 2", got)
		}
		if payloads := server.commandPayloads("thread.turn.interrupt"); len(payloads) != 1 || payloads[0]["turnId"] != "t1" {
			t.Fatalf("interrupts = %+v, want one for t1", payloads)
		}
	})
}

// Kills a fenced dispatch that sends the interrupt on to the next T3 address
// when the first took it and the answer was lost: that send skips the
// re-read, so it can interrupt a turn that started since. The interrupt goes
// out again only on a retry the re-read allows; a turn that changed fails the
// call as unavailable, never as not-current.
func TestInterruptTurnLostAnswerWaitsForTheRefence(t *testing.T) {
	// lossy is a provider whose first address takes the first interrupt and
	// loses its answer, and whose second address answers everything.
	lossy := func(t *testing.T, seq ...map[string]interface{}) (server, other *t3BridgeTestServer, p *Provider) {
		t.Helper()
		server = newTurnFenceServer(t, seq...)
		server.dropCommands(1)
		p = newTurnFenceTestProvider(t, server)
		other = newTurnFenceServer(t, turnRunning("t1"))
		defaultWSURLCandidates = []string{other.wsURL()}
		return server, other, p
	}
	t.Run("turn changed", func(t *testing.T) {
		server, other, p := lossy(t, turnRunning("t1"), turnRunning("t2"))
		_, err := p.InterruptTurn(t.Context(), turnFenceSession, "thread-agent-a/t1")
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotCurrent) {
			t.Fatalf("InterruptTurn = %v, want only ErrRuntimeUnavailable", err)
		}
		if cmds := other.commandTypes(); len(cmds) != 0 {
			t.Fatalf("second address commands = %q, want none", cmds)
		}
		if cmds := server.commandTypes(); len(cmds) != 1 {
			t.Fatalf("first address commands = %q, want the one whose answer was lost", cmds)
		}
		if calls := server.snapshotCalls(); calls != 2 {
			t.Fatalf("snapshot reads = %d, want the fence read and one re-read", calls)
		}
	})
	t.Run("turn unchanged", func(t *testing.T) {
		server, other, p := lossy(t, turnRunning("t1"))
		var readsAtDispatch atomic.Int64
		server.setCommandHook(func(map[string]interface{}) { readsAtDispatch.Store(int64(server.snapshotCalls())) })
		if _, err := p.InterruptTurn(t.Context(), turnFenceSession, "thread-agent-a/t1"); err != nil {
			t.Fatalf("InterruptTurn = %v, want the retry sent", err)
		}
		if got := readsAtDispatch.Load(); got != 2 {
			t.Fatalf("snapshot reads before the resent dispatch = %d, want 2", got)
		}
		payloads := server.commandPayloads("thread.turn.interrupt")
		if len(payloads) != 2 || payloads[0]["commandId"] != payloads[1]["commandId"] {
			t.Fatalf("interrupts = %+v, want the lost one resent with its command id", payloads)
		}
		if cmds := other.commandTypes(); len(cmds) != 0 {
			t.Fatalf("second address commands = %q, want none", cmds)
		}
	})
}

// Kills a fenced dispatch that goes on past the T3 address that took the
// interrupt. Past a going-away close, which is not transient, it sends the
// interrupt to the next address with no re-read. Past T3's refusal, it
// flattens the refusal with an earlier unreachable address's failure, so the
// refusal loses its type, reads as transient and is sent again until the
// retry window ends.
func TestInterruptTurnStopsAtTheAddressThatTookIt(t *testing.T) {
	t.Run("closed going away", func(t *testing.T) {
		// A retry's re-read shows t2, so the interrupt goes out once whether
		// or not the close is retried.
		server := newTurnFenceServer(t, turnRunning("t1"), turnRunning("t2"))
		server.closeCommands(1, websocket.FormatCloseMessage(websocket.CloseGoingAway, "restarting"))
		p := newTurnFenceTestProvider(t, server)
		other := newTurnFenceServer(t, turnRunning("t1"))
		defaultWSURLCandidates = []string{other.wsURL()}
		_, err := p.InterruptTurn(t.Context(), turnFenceSession, "thread-agent-a/t1")
		var closed *websocket.CloseError
		if !errors.As(err, &closed) || closed.Code != websocket.CloseGoingAway || errors.Is(err, runtime.ErrTurnNotCurrent) {
			t.Fatalf("InterruptTurn = %v, want the going-away close and not ErrTurnNotCurrent", err)
		}
		if cmds := other.commandTypes(); len(cmds) != 0 {
			t.Fatalf("second address commands = %q, want none", cmds)
		}
		if cmds := server.commandTypes(); len(cmds) != 1 {
			t.Fatalf("first address commands = %q, want the one it took before closing", cmds)
		}
	})
	t.Run("rejected behind a dead address", func(t *testing.T) {
		server := newTurnFenceServer(t, turnRunning("t1"))
		server.rejectCommand("thread.turn.interrupt", "no active session")
		p := newTurnFenceTestProvider(t, server)
		dead := newTurnFenceServer(t)
		dead.Close()
		t.Setenv("T3_WS_URL", dead.wsURL())
		defaultWSURLCandidates = []string{server.wsURL()}
		_, err := p.InterruptTurn(t.Context(), turnFenceSession, "thread-agent-a/t1")
		if !isT3Rejection(err) {
			t.Fatalf("InterruptTurn = %v, want T3's rejection", err)
		}
		if payloads := server.commandPayloads("thread.turn.interrupt"); len(payloads) != 1 {
			t.Fatalf("interrupts = %+v, want the one T3 refused", payloads)
		}
	})
}

// Kills a fix that ends every RPC at the first T3 address that took it: only
// a call that re-reads before its retries stops there, and a snapshot read
// whose answer is lost is read from the next address.
func TestSnapshotReadTriesTheNextAddressAfterALostAnswer(t *testing.T) {
	server := newTurnFenceServer(t, nil)
	p := newTurnFenceTestProvider(t, server)
	t.Setenv("T3_BEARER_TOKEN", "")
	other := newTurnFenceServer(t, turnRunning("t1"))
	defaultWSURLCandidates = []string{other.wsURL()}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if st, err := p.ObserveTurn(ctx, turnFenceSession); err != nil || st.TurnID != "thread-agent-a/t1" {
		t.Fatalf("ObserveTurn = %+v, %v; want t1, read from the second address", st, err)
	}
}

// Kills a turn read that reports a snapshot failure T3 answered (HTTP 500)
// as anything but unavailable, and an InterruptTurn that sends after one.
func TestTurnFenceSnapshotFailureIsUnavailable(t *testing.T) {
	server := newTurnFenceServer(t, turnRunning("t1"))
	server.setSnapshotFailure(http.StatusInternalServerError, "boom")
	p := newTurnFenceTestProvider(t, server)
	if _, err := p.ObserveTurn(t.Context(), turnFenceSession); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("ObserveTurn = %v, want ErrRuntimeUnavailable", err)
	}
	_, err := p.InterruptTurn(t.Context(), turnFenceSession, "thread-agent-a/t1")
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotCurrent) {
		t.Fatalf("InterruptTurn = %v, want only ErrRuntimeUnavailable", err)
	}
	if cmds := server.commandTypes(); len(cmds) != 0 {
		t.Fatalf("commands = %q, want none", cmds)
	}
}

// Kills a WaitTurnEnded whose bound elapses before its first read and then
// reports a malformed "last read" clause naming no error.
func TestWaitTurnEndedBoundBeforeTheFirstRead(t *testing.T) {
	server := newTurnFenceServer(t, turnRunning("t1"))
	p := newTurnFenceTestProvider(t, server)
	turnPollInitial = 250 * time.Millisecond
	for _, timeout := range []time.Duration{0, 50 * time.Millisecond} {
		st, err := p.WaitTurnEnded(t.Context(), turnFenceSession, "thread-agent-a/t1", timeout)
		if !errors.Is(err, runtime.ErrTurnNotEnded) || st.Active() {
			t.Fatalf("timeout %s = %+v, %v; want ErrTurnNotEnded with no state read", timeout, st, err)
		}
		if strings.Contains(err.Error(), "%!") || strings.Contains(err.Error(), "last read") {
			t.Fatalf("timeout %s error = %q, want no last-read clause when nothing was read", timeout, err)
		}
	}
	if calls := server.snapshotCalls(); calls != 0 {
		t.Fatalf("snapshot reads = %d, want none", calls)
	}
}

// Kills a ws-token request that waits on the shared auth lock past its ctx:
// another caller fetching a token would hold a ctx-bounded fence read for up
// to the 12 s HTTP timeout.
func TestIssueWebSocketTokenWaitsForTheAuthLockWithinCtx(t *testing.T) {
	resetBridgeAuthCacheForTest(t)
	if err := lockBridgeAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer unlockBridgeAuth()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := issueT3WebSocketToken(ctx, "ws://t3.invalid:1")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("issueT3WebSocketToken took %s past a 100ms deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("issueT3WebSocketToken = %v, want the deadline", err)
	}
}

package subprocess

import (
	"context"
	"net"
	"os"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// stuckControlSocket binds name's control socket to a listener that accepts
// connections and never answers them, so a probe dials successfully and then
// runs out its deadline. That is the "alive but could not be observed" shape —
// a child too loaded to service its control socket inside the probe budget —
// and it is deliberately distinct from staleControlSocket below.
func stuckControlSocket(t *testing.T, p *Provider, name string) {
	t.Helper()
	writeSocketNameFile(t, p, name)
	lis, err := net.Listen("unix", p.sockPath(name))
	if err != nil {
		t.Fatalf("listen on control socket for %q: %v", name, err)
	}
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		var held []net.Conn
		defer func() {
			for _, conn := range held {
				_ = conn.Close()
			}
		}()
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()
	t.Cleanup(func() {
		_ = lis.Close()
		<-accepted
	})
}

// staleControlSocket leaves a bound-then-closed socket file behind with nothing
// listening on it — what a session whose process was killed with SIGKILL leaves,
// since the cleanup that unlinks the socket never got to run. The kernel refuses
// the connection, which is POSITIVE proof the session is gone.
func staleControlSocket(t *testing.T, p *Provider, name string) {
	t.Helper()
	writeSocketNameFile(t, p, name)
	addr, err := net.ResolveUnixAddr("unix", p.sockPath(name))
	if err != nil {
		t.Fatalf("resolve control socket for %q: %v", name, err)
	}
	lis, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("listen on control socket for %q: %v", name, err)
	}
	// Keep the socket file after Close so the next dial is refused rather than
	// reporting the file as missing; both are genuine absences, but a refused
	// connection is the one a killed process actually leaves.
	lis.SetUnlinkOnClose(false)
	if err := lis.Close(); err != nil {
		t.Fatalf("close control socket for %q: %v", name, err)
	}
}

func writeSocketNameFile(t *testing.T, p *Provider, name string) {
	t.Helper()
	if err := p.ensureSocketDir(p.socketDir(), os.Geteuid()); err != nil {
		t.Fatalf("ensure socket dir: %v", err)
	}
	if err := os.WriteFile(p.sockNamePath(name), []byte(name), 0o644); err != nil {
		t.Fatalf("write socket name file for %q: %v", name, err)
	}
}

func startLiveSession(t *testing.T, p *Provider, name string) {
	t.Helper()
	if err := p.Start(context.Background(), name, runtime.Config{Command: "sleep 3600"}); err != nil {
		t.Fatalf("Start %q: %v", name, err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
}

// TestListRunningReportsPartialOnUnobservableSocket pins the direction the
// ListRunning contract exists for: a session this provider could not observe
// must surface as an error, never as an absent name. Omitting it lets the
// pending-create rollback in cmd/gc read "I could not look" as "it is gone" and
// release a live agent's alias (gastownhall/gascity#5943, ga-6wkhl).
func TestListRunningReportsPartialOnUnobservableSocket(t *testing.T) {
	const prefix = "gc-lrpartial-a-"
	p := newTestProvider(t)
	live := prefix + "live"
	startLiveSession(t, p, live)
	stuck := prefix + "stuck"
	stuckControlSocket(t, p, stuck)

	names, err := p.ListRunning(prefix)
	if err == nil {
		t.Fatalf("ListRunning = %v, nil; an unobservable session must not be reported as an absent name", names)
	}
	if !runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning error = %T (%v), want *runtime.PartialListError", err, err)
	}
	if runtime.IsRuntimeServerAbsent(err) {
		t.Fatalf("ListRunning error asserts ServerAbsent; one stuck per-session probe says nothing about the others")
	}
	// Degraded but USABLE: the partial error does not cost best-effort callers
	// the names that did resolve.
	if !slices.Contains(names, live) {
		t.Fatalf("ListRunning = %v, want the observable session %q retained alongside the partial error", names, live)
	}
	if slices.Contains(names, stuck) {
		t.Fatalf("ListRunning = %v, must not list the unobservable session %q as running", names, stuck)
	}
}

// TestListRunningStaysCleanWhenProbesResolve is the narrowing control for the
// test above: every probe reaching a verdict must stay a clean success, so the
// fix cannot over-rotate into refusing to confirm any absence at all. A stale
// socket is the case that matters — a provider that reported it as partial
// would make pendingCreateRuntimeAbsenceConfirmed permanently inert, which is
// the opposite failure.
func TestListRunningStaysCleanWhenProbesResolve(t *testing.T) {
	const prefix = "gc-lrpartial-b-"
	p := newTestProvider(t)
	live := prefix + "live"
	startLiveSession(t, p, live)
	stale := prefix + "stale"
	staleControlSocket(t, p, stale)

	names, err := p.ListRunning(prefix)
	if err != nil {
		t.Fatalf("ListRunning = %v; a refused connection is positive proof of absence, not a failed observation", err)
	}
	if !slices.Contains(names, live) {
		t.Fatalf("ListRunning = %v, want the running session %q", names, live)
	}
	if slices.Contains(names, stale) {
		t.Fatalf("ListRunning = %v, must not list the dead session %q", names, stale)
	}
}

// TestListRunningPrefixFilterPrecedesProbe pins that the partial error is
// scoped to the sessions the caller actually asked about. The fail-closed
// consumer calls ListRunning(name) for one name, so an unrelated session's
// stuck probe must not make that narrow listing refuse.
func TestListRunningPrefixFilterPrecedesProbe(t *testing.T) {
	const prefix = "gc-lrpartial-c-"
	p := newTestProvider(t)
	live := prefix + "live"
	startLiveSession(t, p, live)
	stuckControlSocket(t, p, "gc-lrpartial-unrelated-stuck")

	names, err := p.ListRunning(prefix)
	if err != nil {
		t.Fatalf("ListRunning(%q) = %v; an out-of-prefix stuck probe must not poison a narrow listing", prefix, err)
	}
	if !slices.Contains(names, live) {
		t.Fatalf("ListRunning(%q) = %v, want the running session listed", prefix, names)
	}
}

package acp

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// sigtermIgnoringACPCommand is a minimal ACP server that ignores SIGTERM, so
// only the SIGKILL escalation after the stop grace can end it. The handler is
// installed before the handshake reply, so a completed Start proves the
// process is already deaf to SIGTERM. It also survives stdin EOF.
func sigtermIgnoringACPCommand() string {
	return `exec python3 -u -c '
import sys, json, signal
signal.signal(signal.SIGTERM, signal.SIG_IGN)
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except Exception:
        continue
    method = msg.get("method", "")
    if method == "initialize":
        print(json.dumps({"jsonrpc": "2.0", "id": msg.get("id"), "result": {"serverInfo": {"name": "fakeacp", "version": "1.0"}}}), flush=True)
    elif method == "session/new":
        print(json.dumps({"jsonrpc": "2.0", "id": msg.get("id"), "result": {"sessionId": "grace-session"}}), flush=True)
# Stop closes stdin before signaling; outlive EOF so only SIGKILL ends us.
while True:
    signal.pause()
'`
}

const testStopGrace = 300 * time.Millisecond

func newStopGraceProvider(t *testing.T, dir string) *Provider {
	t.Helper()
	return NewProviderWithDir(dir, Config{
		HandshakeTimeout:  5 * time.Second,
		NudgeBusyTimeout:  2 * time.Second,
		OutputBufferLines: 100,
		StopGrace:         testStopGrace,
	})
}

// startSigtermIgnoringSession starts the SIGTERM-ignoring fake and returns
// its tracked connection so the caller can observe process exit directly.
func startSigtermIgnoringSession(t *testing.T, p *Provider, name string) *sessionConn {
	t.Helper()
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: sigtermIgnoringACPCommand(),
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	p.mu.Lock()
	sc := p.conns[name]
	p.mu.Unlock()
	if sc == nil || sc.cmd == nil || sc.cmd.Process == nil {
		t.Fatal("started session has no tracked process")
	}
	return sc
}

func assertProcessReaped(t *testing.T, sc *sessionConn) {
	t.Helper()
	select {
	case <-sc.done:
	default:
		t.Fatal("agent process still running after Stop returned")
	}
	if err := syscall.Kill(sc.cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("kill(pid, 0) after Stop = %v, want ESRCH", err)
	}
}

func assertElapsedWithinGrace(t *testing.T, elapsed time.Duration) {
	t.Helper()
	if elapsed < testStopGrace {
		t.Fatalf("Stop returned after %v, before the %v grace: SIGTERM should have been ignored", elapsed, testStopGrace)
	}
	if elapsed >= runtime.ManagedProcessStopGrace {
		t.Fatalf("Stop took %v, want the configured %v grace rather than the %v default", elapsed, testStopGrace, runtime.ManagedProcessStopGrace)
	}
}

func TestConfigStopGraceDefaultsToManagedProcessStopGrace(t *testing.T) {
	for _, grace := range []time.Duration{0, -time.Second} {
		c := Config{StopGrace: grace}
		if got := c.stopGrace(); got != runtime.ManagedProcessStopGrace {
			t.Errorf("Config{StopGrace: %v}.stopGrace() = %v, want %v", grace, got, runtime.ManagedProcessStopGrace)
		}
	}
	c := Config{StopGrace: 42 * time.Second}
	if got := c.stopGrace(); got != 42*time.Second {
		t.Errorf("stopGrace() = %v, want 42s", got)
	}
}

func TestConfigStopSocketTimeoutOutlastsStopGrace(t *testing.T) {
	for _, c := range []Config{{}, {StopGrace: testStopGrace}, {StopGrace: 30 * time.Second}} {
		if got := c.stopSocketTimeout(); got <= c.stopGrace() {
			t.Errorf("Config{StopGrace: %v}.stopSocketTimeout() = %v, want longer than the %v grace", c.StopGrace, got, c.stopGrace())
		}
	}
	if got := (&Config{}).stopSocketTimeout(); got != 7*time.Second {
		t.Errorf("default stopSocketTimeout() = %v, want the historical 7s", got)
	}
}

func TestStopEscalatesToSIGKILLAfterConfiguredGrace(t *testing.T) {
	p := newStopGraceProvider(t, filepath.Join(shortTempDir(t), "acp"))
	name := testName()
	sc := startSigtermIgnoringSession(t, p, name)

	start := time.Now()
	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertElapsedWithinGrace(t, time.Since(start))
	assertProcessReaped(t, sc)
	if p.IsRunning(name) {
		t.Error("IsRunning = true after Stop, want false")
	}
}

func TestStopBySocketEscalatesAfterOwnersConfiguredGrace(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "acp")
	owner := newStopGraceProvider(t, dir)
	name := testName()
	sc := startSigtermIgnoringSession(t, owner, name)

	// A second provider over the same state dir has no in-process conn, so
	// Stop goes through the control socket and the owner applies its grace.
	other := newStopGraceProvider(t, dir)
	start := time.Now()
	if err := other.Stop(name); err != nil {
		t.Fatalf("Stop via socket: %v", err)
	}
	assertElapsedWithinGrace(t, time.Since(start))
	assertProcessReaped(t, sc)
}

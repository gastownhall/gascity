package acp

import (
	"bufio"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// hangGuard bounds a call that must return once the process is declared not
// confirmed dead. The reap grace is real here (the provider has no seam for it),
// so the guard allows it plus slack.
const hangGuard = runtime.ManagedProcessReapGrace + 10*time.Second

// nopWriteCloser stands in for the agent's stdin pipe.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// stuckSessionConn is a tracked session whose process never exits: done stays
// open, as for a process that outlives SIGKILL. The nil-process command makes
// the signals no-ops, so no real process is involved.
func stuckSessionConn(t *testing.T, token string) *sessionConn {
	t.Helper()
	sc := &sessionConn{
		cmd:   &exec.Cmd{},
		done:  make(chan struct{}),
		stdin: nopWriteCloser{},
		token: token,
	}
	t.Cleanup(func() { close(sc.done) })
	return sc
}

// Kills: an owner that answers "ok" (or never answers) for a stop whose
// process outlived SIGKILL: the "stop" command must reply the token.
func TestHandleControlConnStopRepliesNotConfirmedDead(t *testing.T) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	client, owner := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	go handleControlConn(owner, &exec.Cmd{}, done, 10*time.Millisecond, controlCommands{})

	if _, err := client.Write([]byte("stop\n")); err != nil {
		t.Fatalf("write stop: %v", err)
	}
	reply := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(client).ReadString('\n')
		reply <- strings.TrimSpace(line)
	}()
	select {
	case got := <-reply:
		if got != runtime.StopReplyNotConfirmedDead {
			t.Fatalf("stop reply = %q, want %q", got, runtime.StopReplyNotConfirmedDead)
		}
	case <-time.After(hangGuard):
		t.Fatalf("no stop reply after %v: the owner is still waiting on a process that outlived SIGKILL", hangGuard)
	}
}

// Kills: a stop client that reads the token as the generic "unexpected
// response" error, and a Stop that cleans up the sidecars of a process that
// may still be alive.
func TestStopBySocketNotConfirmedDeadKeepsSidecars(t *testing.T) {
	p := newProbeProvider(t)
	name := "stuck-stop"
	addSocketEntry(t, p, name)
	p.dial = fakeSocketDial(t, map[string]socketOutcome{
		p.sockPath(name): {reply: runtime.StopReplyNotConfirmedDead + "\n"},
	})

	err := p.Stop(name)
	if !errors.Is(err, runtime.ErrProcessNotConfirmedDead) {
		t.Fatalf("Stop error = %v, want ErrProcessNotConfirmedDead", err)
	}
	if runtime.IsSessionGone(err) {
		t.Errorf("IsSessionGone(%v) = true: a process that may be alive was reported gone", err)
	}
	for _, path := range []string{p.sockPath(name), p.sockNamePath(name)} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("Stat(%s) = %v, want it kept: cleanupMeta must not run for a process that may be alive", path, statErr)
		}
	}
}

// Kills: a reply other than ok or the token being mapped to the sentinel.
func TestStopBySocketKeepsUnexpectedResponseForOtherReplies(t *testing.T) {
	p := newProbeProvider(t)
	name := "odd-stop"
	addSocketEntry(t, p, name)
	p.dial = fakeSocketDial(t, map[string]socketOutcome{p.sockPath(name): {reply: "nope\n"}})

	err := p.stopBySocket(name)
	if err == nil || errors.Is(err, runtime.ErrProcessNotConfirmedDead) || !strings.Contains(err.Error(), "unexpected response") {
		t.Fatalf("stopBySocket error = %v, want the plain unexpected-response error", err)
	}
}

// Kills: terminateSessionConn-style waits in the in-memory Stop: a process that
// outlives SIGKILL must make Stop return the sentinel, and Stop must leave the
// session's identity sidecar for the retry instead of cleaning it up.
func TestStopInMemoryNotConfirmedDeadKeepsOwnMeta(t *testing.T) {
	p := newStopGraceProvider(t, t.TempDir())
	p.cfg.StopGrace = 10 * time.Millisecond
	name := "stuck-mem"
	const token = "tok-stuck"
	if err := p.SetMeta(name, "GC_INSTANCE_TOKEN", token); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	p.conns[name] = stuckSessionConn(t, token)

	res := make(chan error, 1)
	go func() { res <- p.Stop(name) }()
	var err error
	select {
	case err = <-res:
	case <-time.After(hangGuard):
		t.Fatalf("Stop still blocked after %v: it waits forever once SIGKILL has been sent", hangGuard)
	}
	if !errors.Is(err, runtime.ErrProcessNotConfirmedDead) {
		t.Fatalf("Stop error = %v, want ErrProcessNotConfirmedDead", err)
	}
	if got, getErr := p.GetMeta(name, "GC_INSTANCE_TOKEN"); getErr != nil || got != token {
		t.Errorf("GC_INSTANCE_TOKEN = %q, %v; want %q kept: cleanupOwnMeta must not run for a process that may be alive", got, getErr, token)
	}
}

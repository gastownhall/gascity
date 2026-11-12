package subprocess

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

// stuckSessionConn is a tracked session whose process never exits: done and
// reaped stay open, as for a process that outlives SIGKILL. The nil-process
// command makes the signals no-ops, so no real process is involved.
func stuckSessionConn(t *testing.T) *sessionConn {
	t.Helper()
	sc := &sessionConn{cmd: &exec.Cmd{}, done: make(chan struct{}), reaped: make(chan struct{})}
	t.Cleanup(func() {
		close(sc.done)
		close(sc.reaped)
	})
	return sc
}

func shortenSessionStopGrace(t *testing.T) {
	t.Helper()
	old := sessionStopGrace
	sessionStopGrace = 10 * time.Millisecond
	t.Cleanup(func() { sessionStopGrace = old })
}

// hangGuard bounds a call that must return once the process is declared not
// confirmed dead. The reap grace is real here (the provider has no seam for it),
// so the guard allows it plus slack.
const hangGuard = runtime.ManagedProcessReapGrace + 10*time.Second

func runGuarded(t *testing.T, what string, fn func() error) error {
	t.Helper()
	res := make(chan error, 1)
	go func() { res <- fn() }()
	select {
	case err := <-res:
		return err
	case <-time.After(hangGuard):
		t.Fatalf("%s still blocked after %v: a process that outlives SIGKILL must not hold the caller forever", what, hangGuard)
		return nil
	}
}

// Kills: a stop client that reads the not-confirmed-dead reply as the generic
// "unexpected response" error, which callers cannot tell from a transport fault.
func TestStopBySocketMapsNotConfirmedDeadReply(t *testing.T) {
	p := newTestProvider(t)
	name := "stuck-stop"
	if err := os.WriteFile(p.sockNamePath(name), []byte(name), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	gotCommand := startRecordingControlSocket(t, p.sockPath(name), runtime.StopReplyNotConfirmedDead+"\n", 1)

	err := p.stopBySocket(name)
	if !errors.Is(err, runtime.ErrProcessNotConfirmedDead) {
		t.Fatalf("stopBySocket error = %v, want ErrProcessNotConfirmedDead", err)
	}
	if runtime.IsSessionGone(err) {
		t.Errorf("IsSessionGone(%v) = true: a process that may be alive was reported gone", err)
	}
	if got := <-gotCommand; got != "stop" {
		t.Fatalf("socket command = %q, want stop", got)
	}
	if _, statErr := os.Stat(p.sockPath(name)); statErr != nil {
		t.Errorf("socket path err = %v, want the socket preserved: the process may still own it", statErr)
	}
	if _, statErr := os.Stat(p.sockNamePath(name)); statErr != nil {
		t.Errorf("socket name path err = %v, want the name file preserved", statErr)
	}
}

// Kills: a reply other than ok or the token being mapped to the sentinel.
func TestStopBySocketKeepsUnexpectedResponseForOtherReplies(t *testing.T) {
	p := newTestProvider(t)
	name := "odd-stop"
	if err := os.WriteFile(p.sockNamePath(name), []byte(name), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	startRecordingControlSocket(t, p.sockPath(name), "nope\n", 1)

	err := p.stopBySocket(name)
	if err == nil || errors.Is(err, runtime.ErrProcessNotConfirmedDead) || !strings.Contains(err.Error(), "unexpected response") {
		t.Fatalf("stopBySocket error = %v, want the plain unexpected-response error", err)
	}
}

// Kills: the owner answering "ok" (or hanging) for a stop whose process outlived
// SIGKILL: the "stop" command must reply the not-confirmed-dead token.
func TestHandleSessionConnStopRepliesNotConfirmedDead(t *testing.T) {
	shortenSessionStopGrace(t)
	sc := stuckSessionConn(t)
	client, owner := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	go handleSessionConn(owner, sc)

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

// Kills: a client timeout that expires before the owner's stop grace plus
// reap grace has run out, so the caller gives up on a stop still in progress
// (the literal 7s it replaces is below 5s grace + 3s reap).
func TestStopSocketTimeoutOutlastsStopGraceAndReapGrace(t *testing.T) {
	old := sessionStopGrace
	t.Cleanup(func() { sessionStopGrace = old })
	for _, grace := range []time.Duration{10 * time.Millisecond, runtime.ManagedProcessStopGrace, 30 * time.Second} {
		sessionStopGrace = grace
		if got, floor := stopSocketTimeout(), grace+runtime.ManagedProcessReapGrace; got <= floor {
			t.Errorf("stopSocketTimeout() = %v with a %v stop grace, want more than grace + reap grace = %v", got, grace, floor)
		}
	}
}

// Kills: Stop re-publishing the session after a not-confirmed-dead result, and
// IsRunning answering false while the process may still be alive.
func TestStopNotConfirmedDeadDropsEntryAndIsRunningFollowsSocket(t *testing.T) {
	shortenSessionStopGrace(t)
	p := newProbeProvider(t)
	name := "stuck"
	p.procs[name] = stuckSessionConn(t)
	p.ops.dial = fakeSocketDial(t, map[string]socketOutcome{p.sockPath(name): socketOK})

	err := runGuarded(t, "Stop", func() error { return p.Stop(name) })
	if !errors.Is(err, runtime.ErrProcessNotConfirmedDead) {
		t.Fatalf("Stop error = %v, want ErrProcessNotConfirmedDead", err)
	}
	if runtime.IsSessionGone(err) {
		t.Errorf("IsSessionGone(%v) = true, want false", err)
	}
	p.mu.Lock()
	_, tracked := p.procs[name]
	p.mu.Unlock()
	if tracked {
		t.Error("Stop re-published the session in p.procs; the entry must stay deleted")
	}
	if !p.IsRunning(name) {
		t.Error("IsRunning = false after a not-confirmed-dead Stop, want true while the control socket still answers")
	}
}

package herdr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// contractFakeHerdr is a herdr CLI stand-in for the runtime contract cases.
// $MODE selects the backend it plays:
//
//	unreachable         every verb fails at the transport (non-zero exit, no envelope)
//	absent              the server answers and holds no agent or pane
//	busy                `agent wait` times out; the agent reads "working"
//	busy_stderr         busy, with the timeout on stderr and a non-zero exit
//	busy_get_blocks     busy, but `agent get` signals the $DIR/get-started FIFO, then hangs
//	unknown             `agent wait` times out; herdr cannot classify the agent
//	close_fails         the bound pane exists, but `pane close` fails at the transport
//	exited_close_fails  close_fails, with the pane back at its bare shell prompt
//	close_gone          the bound pane exists, but `pane close` answers pane_not_found
//
// Whether the server is alive is not the CLI's to say: the provider's socket
// decides (newContractProvider leaves none, listenHerdrSocket plants one).
const contractFakeHerdr = `#!/bin/sh
MODE='%MODE%'
DIR='%DIR%'
shift 2
case "$MODE" in
unreachable)
  echo 'herdr: connect /run/herdr.sock: connection refused' >&2
  exit 1 ;;
esac
case "$1_$2" in
agent_get)
  case "$MODE" in
  busy|busy_stderr) printf '%s' '{"result":{"agent":{"name":"'"$3"'","pane_id":"%5","agent_status":"working"}}}' ;;
  busy_get_blocks)
    echo started > "$DIR/get-started"
    exec sleep 30 ;;
  unknown) printf '%s' '{"result":{"agent":{"name":"'"$3"'","pane_id":"%5","agent_status":"unknown"}}}' ;;
  *) printf '%s' '{"error":{"code":"agent_not_found","message":"agent target not found"}}' ;;
  esac ;;
agent_list)
  printf '%s' '{"result":{"agents":[]}}' ;;
agent_wait)
  if [ "$MODE" = busy_stderr ]; then
    printf '%s' '{"error":{"code":"timeout","message":"timed out waiting for agent status"}}' >&2
    exit 1
  fi
  printf '%s' '{"error":{"code":"timeout","message":"timed out waiting for agent status"}}' ;;
pane_process-info)
  case "$MODE" in
  exited_close_fails) printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4242,"name":"zsh"}]}}}' ;;
  *) printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4243,"name":"claude"}]}}}' ;;
  esac ;;
pane_close)
  case "$MODE" in
  close_fails|exited_close_fails)
    echo 'herdr: connect /run/herdr.sock: connection refused' >&2
    exit 1 ;;
  close_gone)
    printf '%s' '{"error":{"code":"pane_not_found","message":"pane not found"}}' ;;
  esac ;;
esac
exit 0
`

// newContractProvider builds a Provider whose client shells out to
// contractFakeHerdr in mode. Its server socket is a fresh path under /tmp
// (t.TempDir() is too deep for a unix socket on darwin) where nothing listens,
// so its server reads as dead until a test plants one with listenHerdrSocket.
func newContractProvider(t *testing.T, mode string) *Provider {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "herdr")
	body := []byte(strings.NewReplacer("%MODE%", mode, "%DIR%", dir).Replace(contractFakeHerdr))
	if err := os.WriteFile(script, body, 0o755); err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("/tmp", "hdrc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	p := New("gctest-contract", t.TempDir(), t.TempDir(), time.Second, time.Second)
	p.c.bin = script
	p.c.sockPath = filepath.Join(sockDir, "herdr.sock")
	return p
}

// newLiveContractProvider is newContractProvider with a live server socket:
// whatever its verbs answer, its server is not dead.
func newLiveContractProvider(t *testing.T, mode string) *Provider {
	t.Helper()
	p := newContractProvider(t, mode)
	listenHerdrSocket(t, p)
	return p
}

// TestHerdrStopContract runs the RUNTIME-CONTRACT-003 conformance cases
// against a fake herdr: a missing session on an answering server stops
// cleanly, a server herdr cannot reach is a Stop error StopForCleanup
// propagates rather than an absence certified on no evidence, and the same
// failing verbs with the server confirmed dead are a stop.
func TestHerdrStopContract(t *testing.T) {
	runtimetest.RunStopContractTests(t, runtimetest.StopFixtures{
		Responsive:  func(t *testing.T) runtime.Provider { return newLiveContractProvider(t, "absent") },
		Unreachable: func(t *testing.T) runtime.Provider { return newLiveContractProvider(t, "unreachable") },
		DeadServer:  func(t *testing.T) runtime.Provider { return newContractProvider(t, "unreachable") },
	})
}

// TestHerdrIdleWaitContract runs the RUNTIME-CONTRACT-016 conformance cases
// against a fake herdr whose native wait times out: an agent read working is
// an observed timeout, one herdr cannot classify is an unobserved one.
func TestHerdrIdleWaitContract(t *testing.T) {
	runtimetest.RunIdleWaitContractTests(t, runtimetest.IdleWaitFixtures{
		Busy: func(t *testing.T) (runtime.Provider, string) {
			return newContractProvider(t, "busy"), "gastown__worker"
		},
		Unobserved: func(t *testing.T) (runtime.Provider, string) {
			return newContractProvider(t, "unknown"), "gastown__worker"
		},
	})
}

// TestHerdrStopKeepsTheBindingWhenTheCloseFails pins that a pane close herdr
// could not carry out is a Stop error and leaves the sidecar binding in place,
// so a retried Stop can still find the pane. That holds for the close Stop
// issues and for the reap resolution issues on an exited agent's pane.
func TestHerdrStopKeepsTheBindingWhenTheCloseFails(t *testing.T) {
	for _, tc := range []struct {
		name, mode, bindMode, wantErr string
	}{
		{"running session's pane", "close_fails", bindModeShell, "closing pane %5"},
		{"exited agent's pane", "exited_close_fails", bindModeAgent, "reaping exited agent pane %5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newLiveContractProvider(t, tc.mode)
			const name = "gastown__worker"
			bindTestPane(t, p, name, tc.bindMode)

			err := p.Stop(name)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Stop with a failed pane close = %v, want the close error (%q)", err, tc.wantErr)
			}
			if err := runtime.StopForCleanup(p, name); err == nil {
				t.Fatal("StopForCleanup with a failed pane close = nil, want the close error propagated")
			}
			if pane, _ := p.GetMeta(name, metaBoundPane); pane != "%5" {
				t.Errorf("bound pane after a failed Stop = %q, want %%5 kept for the retry", pane)
			}
		})
	}
}

// TestHerdrStopTreatsAPaneGoneMidStopAsStopped pins the race the other way: a
// pane that vanished between resolution and close is the session gone.
func TestHerdrStopTreatsAPaneGoneMidStopAsStopped(t *testing.T) {
	p := newContractProvider(t, "close_gone")
	const name = "gastown__worker"
	bindTestPane(t, p, name, bindModeShell)

	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop on a pane gone mid-stop = %v, want nil", err)
	}
	if pane, _ := p.GetMeta(name, metaBoundPane); pane != "" {
		t.Errorf("bound pane after Stop = %q, want the binding cleared", pane)
	}
}

// TestHerdrStopOnADeadServerDropsTheBinding pins what Stop does when no verb
// reaches a server confirmed dead: the pane died with the server, so the
// session is stopped and its binding dropped, and a restarted server's
// recycled pane id cannot resolve to it.
func TestHerdrStopOnADeadServerDropsTheBinding(t *testing.T) {
	p := newContractProvider(t, "unreachable")
	const name = "gastown__worker"
	bindTestPane(t, p, name, bindModeShell)

	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop with the herdr server dead = %v, want nil", err)
	}
	if pane, _ := p.GetMeta(name, metaBoundPane); pane != "" {
		t.Errorf("bound pane after Stop = %q, want the binding dropped", pane)
	}
}

// TestHerdrStopKeepsAnUnlinkedServersBinding pins the case the kernel's
// listener table exists for: a live server whose socket file was deleted
// refuses every verb and dials as missing, yet its panes still run. Stop must
// not read that server dead.
func TestHerdrStopKeepsAnUnlinkedServersBinding(t *testing.T) {
	p := newLiveContractProvider(t, "unreachable")
	if err := os.Remove(p.c.socketPath()); err != nil {
		t.Fatal(err)
	}
	const name = "gastown__worker"
	bindTestPane(t, p, name, bindModeShell)

	if err := runtime.StopForCleanup(p, name); err == nil {
		t.Fatal("StopForCleanup with a live server whose socket was unlinked = nil, want the Stop failure propagated")
	}
	if pane, _ := p.GetMeta(name, metaBoundPane); pane != "%5" {
		t.Errorf("bound pane after a failed Stop = %q, want %%5 kept for the retry", pane)
	}
}

// TestServerConfirmedDead pins the proof Stop relies on: a missing or refusing
// socket with no listener the kernel lists is dead; a socket that accepts, an
// unlinked one a server still listens on, a dial that cannot tell and an
// unreadable listener table are not.
func TestServerConfirmedDead(t *testing.T) {
	refused := func(context.Context, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	timedOut := func(context.Context, string) (net.Conn, error) { return nil, context.DeadlineExceeded }
	unreadable := func(string) (bool, error) { return false, os.ErrPermission }
	for _, tc := range []struct {
		name      string
		server    string // "" none, "live", or "unlinked": live with its socket file deleted
		dial      func(context.Context, string) (net.Conn, error)
		listening func(string) (bool, error)
		want      bool
	}{
		{name: "missing socket", want: true},
		{name: "stale socket refusing", dial: refused, want: true},
		{name: "accepting socket", server: "live"},
		{name: "unlinked socket of a live server", server: "unlinked"},
		{name: "dial timeout", dial: timedOut},
		{name: "unreadable listener table", listening: unreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newContractProvider(t, "unreachable")
			if tc.server != "" {
				listenHerdrSocket(t, p)
			}
			if tc.server == "unlinked" {
				if err := os.Remove(p.c.socketPath()); err != nil {
					t.Fatal(err)
				}
			}
			if tc.dial != nil {
				p.c.dialUnix = tc.dial
			}
			p.c.unixListening = tc.listening
			if got := p.c.serverConfirmedDead(context.Background()); got != tc.want {
				t.Errorf("serverConfirmedDead() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHerdrWaitForIdleReadsATimeoutFromStderr pins the shape herdr reports a
// rejection in on a non-zero exit: the timeout envelope on stderr is still a
// timeout, not a herdr that could not answer.
func TestHerdrWaitForIdleReadsATimeoutFromStderr(t *testing.T) {
	p := newContractProvider(t, "busy_stderr")
	err := p.WaitForIdle(context.Background(), "gastown__worker", 200*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, runtime.ErrWaitUnobserved) {
		t.Fatalf("WaitForIdle on a busy agent with a stderr timeout = %v, want an observed timeout", err)
	}
}

// TestHerdrWaitForIdleUnreachableIsRuntimeUnavailable pins that a herdr which
// cannot answer is neither idle nor a timeout: the caller must not inject
// input, and a restart decider must not read a running turn into it.
func TestHerdrWaitForIdleUnreachableIsRuntimeUnavailable(t *testing.T) {
	p := newContractProvider(t, "unreachable")
	err := p.WaitForIdle(context.Background(), "gastown__worker", 200*time.Millisecond)
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("WaitForIdle on an unreachable herdr = %v, want runtime.ErrRuntimeUnavailable", err)
	}
}

// TestHerdrWaitForIdleCanceledDuringTheStatusReadIsCanceled pins that a
// caller canceling while the post-timeout status read is in flight gets its
// own context.Canceled, not the timeout that read was classifying.
func TestHerdrWaitForIdleCanceledDuringTheStatusReadIsCanceled(t *testing.T) {
	p := newContractProvider(t, "busy_get_blocks")
	started := filepath.Join(filepath.Dir(p.c.bin), "get-started")
	if err := syscall.Mkfifo(started, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// The read returns once the fake's `agent get` has written the FIFO,
		// so the cancel lands while that status read is in flight.
		_, _ = os.ReadFile(started)
		cancel()
	}()

	err := p.WaitForIdle(ctx, "gastown__worker", 30*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForIdle canceled during its status read = %v, want context.Canceled", err)
	}
}

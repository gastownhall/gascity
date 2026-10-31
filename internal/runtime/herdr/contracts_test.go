package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// contractFakeHerdr is a herdr CLI stand-in for the runtime contract cases.
// $MODE selects the backend it plays:
//
//	unreachable  every verb fails at the transport (non-zero exit, no envelope)
//	absent       the server answers and holds no agent or pane
//	busy         `agent wait` times out; the agent reads "working"
//	busy_stderr  busy, with the timeout on stderr and a non-zero exit
//	unknown      `agent wait` times out; herdr cannot classify the agent
//	close_fails  the bound pane exists, but `pane close` fails at the transport
//	close_gone   the bound pane exists, but `pane close` answers pane_not_found
const contractFakeHerdr = `#!/bin/sh
MODE='%MODE%'
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
  printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4243,"name":"claude"}]}}}' ;;
pane_close)
  case "$MODE" in
  close_fails)
    echo 'herdr: connect /run/herdr.sock: connection refused' >&2
    exit 1 ;;
  close_gone)
    printf '%s' '{"error":{"code":"pane_not_found","message":"pane not found"}}' ;;
  esac ;;
esac
exit 0
`

// newContractProvider builds a Provider whose client shells out to
// contractFakeHerdr in mode.
func newContractProvider(t *testing.T, mode string) *Provider {
	t.Helper()
	script := filepath.Join(t.TempDir(), "herdr")
	body := []byte(strings.ReplaceAll(contractFakeHerdr, "%MODE%", mode))
	if err := os.WriteFile(script, body, 0o755); err != nil {
		t.Fatal(err)
	}
	p := New("gctest-contract", t.TempDir(), t.TempDir(), time.Second, time.Second)
	p.c.bin = script
	return p
}

// TestHerdrStopContract runs the RUNTIME-CONTRACT-003 conformance cases
// against a fake herdr: a missing session on an answering server stops
// cleanly, and a server herdr cannot reach is a Stop error StopForCleanup
// propagates rather than an absence certified on no evidence.
func TestHerdrStopContract(t *testing.T) {
	runtimetest.RunStopContractTests(t, runtimetest.StopFixtures{
		Responsive:  func(t *testing.T) runtime.Provider { return newContractProvider(t, "absent") },
		Unreachable: func(t *testing.T) runtime.Provider { return newContractProvider(t, "unreachable") },
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
// so a retried Stop can still find the pane.
func TestHerdrStopKeepsTheBindingWhenTheCloseFails(t *testing.T) {
	p := newContractProvider(t, "close_fails")
	const name = "gastown__worker"
	bindTestPane(t, p, name, bindModeShell)

	if err := p.Stop(name); err == nil {
		t.Fatal("Stop with a failed pane close = nil, want the close error")
	}
	if err := runtime.StopForCleanup(p, name); err == nil {
		t.Fatal("StopForCleanup with a failed pane close = nil, want the close error propagated")
	}
	if pane, _ := p.GetMeta(name, metaBoundPane); pane != "%5" {
		t.Errorf("bound pane after a failed Stop = %q, want %%5 kept for the retry", pane)
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

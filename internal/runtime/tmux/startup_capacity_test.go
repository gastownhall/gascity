package tmux

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// deadPaneOps is a startOps fake whose pane has already died with the given
// exit facts, the shape startupDeadSessionError sees during the readiness wait.
func deadPaneOps(pane, status, signal string) *fakeStartOps {
	running := false
	return &fakeStartOps{
		hasSessionResult:       true,
		isSessionRunningResult: &running,
		capturePaneText:        pane,
		recordStartCrashPath:   "/city/.gc/runtime/sessions/worker/start-stderr.log",
		paneDeadStatus:         status,
		paneDeadSignal:         signal,
	}
}

func countCalls(ops *fakeStartOps, method string) int {
	n := 0
	for _, c := range ops.calls {
		if c.method == method {
			n++
		}
	}
	return n
}

// A launcher that exits EX_TEMPFAIL during startup declares an endpoint-wide
// refusal. The error must say so structurally while remaining the startup
// death it has always been, and the durable artifact must record the same exit
// status the classification used (read once, so the two cannot disagree).
func TestStartupDeadSessionError_ExitTempFailIsCapacity(t *testing.T) {
	ops := deadPaneOps("native broker tunnel 127.0.0.1:58443 is not listening", "75", "")

	err := startupDeadSessionError(ops, "worker")

	if !runtime.IsProviderCapacity(err) {
		t.Fatalf("error = %v, want a typed capacity refusal", err)
	}
	var capErr *runtime.CapacityError
	if !errors.As(err, &capErr) || capErr.ExitCode != runtime.ExitCodeTempFail || capErr.Source != runtime.CapacitySourceExitStatus {
		t.Fatalf("CapacityError = %+v, want ExitCode 75 from exit_status", capErr)
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("error = %v, want it to remain ErrSessionDiedDuringStartup", err)
	}
	if !strings.Contains(err.Error(), "is not listening") {
		t.Fatalf("error = %v, want the pane diagnostic preserved", err)
	}
	if got := countCalls(ops, "paneDeadInfo"); got != 1 {
		t.Fatalf("paneDeadInfo read %d times, want exactly once", got)
	}
	for _, c := range ops.calls {
		if c.method == "recordStartCrash" && (c.exitStatus != "75" || c.exitSignal != "") {
			t.Fatalf("recordStartCrash got status=%q signal=%q, want the classified 75", c.exitStatus, c.exitSignal)
		}
	}
	if countCalls(ops, "recordStartCrash") != 1 {
		t.Fatalf("calls = %+v, want one durable crash record", ops.calls)
	}
}

// The measured broker refusal exits 1 with an HTTP 503 in the pane. Pane text
// is never a capacity signal: only the exit status is.
func TestStartupDeadSessionError_BrokerTextWithExitOneIsNotCapacity(t *testing.T) {
	ops := deadPaneOps("native broker refused: HTTP 503 Service Unavailable", "1", "")

	err := startupDeadSessionError(ops, "worker")

	if runtime.IsProviderCapacity(err) {
		t.Fatalf("error = %v, want a session-specific startup death, not capacity", err)
	}
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
		t.Fatalf("error = %v, want ErrSessionDiedDuringStartup", err)
	}
}

// Only a clean exit 75 is a refusal. A missing status (no remain-on-exit
// corpse), a signal, or any other code stays session-specific. Signals are
// numeric, as tmux reports #{pane_dead_signal} on Linux.
func TestStartupDeadSessionError_SignalOrMissingStatusIsNotCapacity(t *testing.T) {
	cases := map[string]struct{ status, signal string }{
		"missing status":   {"", ""},
		"signal with 75":   {"75", "15"},
		"signal only":      {"", "9"},
		"killed exit code": {"137", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := startupDeadSessionError(deadPaneOps("boom", tc.status, tc.signal), "worker")
			if runtime.IsProviderCapacity(err) {
				t.Fatalf("status=%q signal=%q: error = %v, want not capacity", tc.status, tc.signal, err)
			}
			if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) {
				t.Fatalf("error = %v, want ErrSessionDiedDuringStartup", err)
			}
		})
	}
}

// Provider.Start skips the token-fenced teardown only for ErrServerDegraded.
// A capacity refusal leaves a dead pane behind and must still be torn down.
func TestCapacityErrorIsNotServerDegraded(t *testing.T) {
	err := startupDeadSessionError(deadPaneOps("tunnel down", "75", ""), "worker")
	if !runtime.IsProviderCapacity(err) {
		t.Fatalf("precondition: error = %v, want capacity", err)
	}
	if errors.Is(err, ErrServerDegraded) {
		t.Fatalf("error = %v matches ErrServerDegraded; the failed start would not be cleaned up", err)
	}
}

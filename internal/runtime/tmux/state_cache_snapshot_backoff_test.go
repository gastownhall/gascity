package tmux

import (
	"context"
	"testing"
	"time"
)

// The gate's zero value must attempt immediately: every existing construction
// site builds &tmuxFetcher{tm: tm} as a literal, so a gate that started closed
// would silently disable the process snapshot everywhere.
func TestProcessSnapshotGate_ZeroValueAllows(t *testing.T) {
	var g processSnapshotGate
	if !g.allow(time.Now()) {
		t.Fatal("allow() = false on a zero-value gate, want true")
	}
}

func TestProcessSnapshotGate_BacksOffAfterFailure(t *testing.T) {
	var g processSnapshotGate
	now := time.Now()

	window := g.failed(now)
	if window != processSnapshotBackoffBase {
		t.Errorf("first window = %v, want %v", window, processSnapshotBackoffBase)
	}
	if g.allow(now) {
		t.Error("allow() = true immediately after a failure, want false")
	}
	if g.allow(now.Add(window - time.Millisecond)) {
		t.Error("allow() = true inside the backoff window, want false")
	}
	if !g.allow(now.Add(window)) {
		t.Error("allow() = false at the end of the window, want true")
	}
}

// Doubling is what turns a saturated box's one-attempt-per-refresh into one
// attempt per cap; without it the backoff would be a fixed delay and the loop
// would merely run slower.
func TestProcessSnapshotGate_WindowDoublesToCap(t *testing.T) {
	var g processSnapshotGate
	now := time.Now()

	want := processSnapshotBackoffBase
	for i := range 12 {
		got := g.failed(now)
		if want > processSnapshotBackoffMax {
			want = processSnapshotBackoffMax
		}
		if got != want {
			t.Fatalf("window after %d failures = %v, want %v", i+1, got, want)
		}
		want *= 2
	}
	if got := g.failed(now); got != processSnapshotBackoffMax {
		t.Errorf("window stays at %v, want the cap %v", got, processSnapshotBackoffMax)
	}
}

func TestProcessSnapshotGate_SuccessClearsAndReportsRecoveryOnce(t *testing.T) {
	var g processSnapshotGate
	now := time.Now()
	g.failed(now)

	if !g.succeeded() {
		t.Error("succeeded() = false while clearing a backoff, want true so recovery is logged")
	}
	if !g.allow(now) {
		t.Error("allow() = false after success, want the gate reopened")
	}
	// A healthy fetcher must not claim recovery on every refresh forever.
	if g.succeeded() {
		t.Error("succeeded() = true on an already-clear gate, want false")
	}
}

// The integration this fix exists for: while the gate is closed, FetchState
// must not run the full-OS scan at all — it reports process detail unavailable
// without spending fetchTimeout to rediscover that.
//
// Real `ps` succeeds on the machine running this test, so ProcessesAvailable
// being false is only explicable by the skip.
func TestFetchState_SkipsProcessSnapshotWhileGated(t *testing.T) {
	const panes = "agent-1\t0\tclaude\t4242\n"

	primed := &tmuxFetcher{tm: &Tmux{cfg: DefaultConfig(), exec: &fakeExecutor{out: panes}}}
	state, err := primed.FetchState(context.Background())
	if err != nil {
		t.Fatalf("FetchState (open gate): %v", err)
	}
	if !state.ProcessesAvailable {
		t.Skip("process snapshot unavailable on this machine; the skip assertion below would not be meaningful")
	}

	gated := &tmuxFetcher{tm: &Tmux{cfg: DefaultConfig(), exec: &fakeExecutor{out: panes}}}
	gated.snapshotGate.failed(time.Now())

	state, err = gated.FetchState(context.Background())
	if err != nil {
		t.Fatalf("FetchState (closed gate): %v", err)
	}
	if state.ProcessesAvailable {
		t.Error("ProcessesAvailable = true while gated, want the scan skipped")
	}
	// Session liveness is authoritative and must survive the skip — the whole
	// reason the snapshot degrades rather than failing.
	if session, ok := state.Sessions["agent-1"]; !ok || !session.Running {
		t.Errorf("Sessions[agent-1] = %+v, want it retained and running", session)
	}
}

// A skipped snapshot degrades optimistically, exactly as a failed one does: a
// secondary probe that did not run must never be read as "the process is dead".
func TestProcessAlive_OptimisticWhileSnapshotSkipped(t *testing.T) {
	state := runtimeStateSnapshot{
		Sessions: map[string]sessionRuntimeState{
			"agent-1": {Running: true, Panes: []paneRuntimeState{{Command: "claude", PID: "4242"}}},
		},
		ProcessesAvailable: false,
	}
	if !state.processAlive("agent-1", []string{"claude"}) {
		t.Error("processAlive() = false with the snapshot unavailable, want optimistic true")
	}
}

package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// settleObserver scripts serverSocketObserver for the kill-server settle wait.
// The first live observations report a socket that still answers; every later
// one reports it safely gone. killed is read at each observation so a test can
// prove the wait starts only after kill-server was issued.
type settleObserver struct {
	live   int
	always bool
	calls  int
	paths  []string
	killed func() bool
	early  bool
}

func (o *settleObserver) observe(_ context.Context, path string) error {
	if o.killed != nil && !o.killed() {
		o.early = true
	}
	o.calls++
	o.paths = append(o.paths, path)
	if o.always || o.calls <= o.live {
		return fmt.Errorf("path=%s inode=97 peer_pid=4242 reason=live-unix-socket", path)
	}
	return nil
}

func killIssued(fe *fakeExecutor) func() bool {
	return func() bool {
		for _, call := range fe.calls {
			for _, arg := range call {
				if arg == "kill-server" {
					return true
				}
			}
		}
		return false
	}
}

// TestKillServerWaitsForSocketToStopAnswering pins the teardown-then-restart
// race: kill-server returns once tmux acknowledges, but the server closes and
// unlinks its listener afterwards. A restart in that window dials the still
// answering socket and the new-session preflight refuses it as
// reason=live-unix-socket. KillServer must not return until the socket has
// stopped answering.
func TestKillServerWaitsForSocketToStopAnswering(t *testing.T) {
	for _, teardown := range []struct {
		name string
		call func(*Tmux) error
	}{
		{name: "KillServer", call: (*Tmux).KillServer},
		{name: "TeardownServer", call: (*Tmux).TeardownServer},
	} {
		t.Run(teardown.name, func(t *testing.T) {
			t.Setenv("TMUX_TMPDIR", "/tmux-private")
			fe := &fakeExecutor{}
			obs := &settleObserver{live: 3, killed: killIssued(fe)}
			tm := &Tmux{
				cfg:                  Config{SocketName: "gc-settle"},
				exec:                 fe,
				serverSocketObserver: obs.observe,
			}

			if err := teardown.call(tm); err != nil {
				t.Fatalf("%s() = %v, want nil once the socket stops answering", teardown.name, err)
			}
			if want := obs.live + 1; obs.calls != want {
				t.Fatalf("observer calls = %d, want %d (three live observations, then the clean one)", obs.calls, want)
			}
			if obs.early {
				t.Fatalf("socket observed before kill-server was issued; calls = %v", fe.calls)
			}
			wantPath := tm.serverSocketPath()
			for _, got := range obs.paths {
				if got != wantPath {
					t.Fatalf("observed path = %q, want %q", got, wantPath)
				}
			}
		})
	}
}

// TestKillServerFailsClosedWhenSocketStaysLive is the negative control: the
// wait must not paper over a socket that genuinely keeps answering. After the
// settle budget KillServer reports ErrServerDegraded carrying the observation,
// so the socket-clobber guard keeps its strict meaning.
func TestKillServerFailsClosedWhenSocketStaysLive(t *testing.T) {
	prevTimeout, prevPoll := killServerSettleTimeout, killServerSettlePoll
	killServerSettleTimeout, killServerSettlePoll = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() { killServerSettleTimeout, killServerSettlePoll = prevTimeout, prevPoll })

	fe := &fakeExecutor{}
	obs := &settleObserver{always: true}
	tm := &Tmux{
		cfg:                  Config{SocketName: "gc-still-live"},
		exec:                 fe,
		serverSocketObserver: obs.observe,
	}

	err := tm.KillServer()
	if !errors.Is(err, ErrServerDegraded) {
		t.Fatalf("KillServer() = %v, want ErrServerDegraded for a socket that stays live", err)
	}
	for _, want := range []string{"peer_pid=4242", "reason=live-unix-socket"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q, want %q", err, want)
		}
	}
	if obs.calls < 2 {
		t.Fatalf("observer calls = %d, want the wait to poll more than once", obs.calls)
	}
}

// TestKillServerAlreadyGoneDoesNotWait keeps the idempotent-teardown contract:
// a server that is already gone is success, and a socket that is already
// absent costs one observation, not a poll loop.
func TestKillServerAlreadyGoneDoesNotWait(t *testing.T) {
	fe := &fakeExecutor{err: ErrNoServer}
	obs := &settleObserver{}
	tm := &Tmux{
		cfg:                  Config{SocketName: "gc-gone"},
		exec:                 fe,
		serverSocketObserver: obs.observe,
	}

	start := time.Now()
	if err := tm.KillServer(); err != nil {
		t.Fatalf("KillServer() = %v, want nil for an already-gone server", err)
	}
	if obs.calls > 1 {
		t.Fatalf("observer calls = %d, want at most 1", obs.calls)
	}
	if elapsed := time.Since(start); elapsed > killServerSettleTimeout {
		t.Fatalf("KillServer took %v for an already-gone server", elapsed)
	}
}

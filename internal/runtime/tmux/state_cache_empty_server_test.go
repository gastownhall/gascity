package tmux

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	gcruntime "github.com/gastownhall/gascity/internal/runtime"
)

// An alive tmux server holding zero sessions is a normal steady state for every
// gc city: ConfigureServer sets `exit-empty off`, so the server outlives its
// last session. `list-panes -a` against such a server exits non-zero with
// "no current target" (see wrapError / ErrNoCurrentTarget), which is a
// SUCCESSFUL observation of an empty server — not a failed observation of an
// unreachable one.
//
// ga-jnavd: FetchState lumped it in with ErrNoServer via isNoServerError and
// returned runtime.ErrRuntimeUnavailable, so the supervisor's state cache
// never primed: every IsRunning spawned a fresh tmux subprocess, logged
// "refresh failed ... no current target", and after staleTTL reported every
// session on that city not-running. It ran ~14x/2min for hours.
func TestTmuxFetcher_EmptyServerIsSuccessfulEmptyObservation(t *testing.T) {
	f := &tmuxFetcher{tm: &Tmux{cfg: Config{SocketName: "city-with-no-sessions"}, exec: &fakeExecutor{err: ErrNoCurrentTarget}}}

	snap, err := f.FetchState(context.Background())
	if err != nil {
		t.Fatalf("FetchState() err = %v, want nil: an alive server with zero sessions is an observation, not an outage", err)
	}
	if errors.Is(err, gcruntime.ErrRuntimeUnavailable) {
		t.Fatalf("FetchState() err = %v, must not be ErrRuntimeUnavailable for an alive empty server", err)
	}
	if snap.Sessions == nil {
		t.Fatal("FetchState() Sessions = nil, want an empty non-nil map so the cache is primed")
	}
	if len(snap.Sessions) != 0 {
		t.Fatalf("FetchState() Sessions = %v, want empty", snap.Sessions)
	}
}

// The raw tmux stderr must classify to the same empty observation, so the fix
// holds end to end from the subprocess boundary and not just from a hand-built
// sentinel.
func TestTmuxFetcher_EmptyServerFromRawStderr(t *testing.T) {
	raw := wrapError(errors.New("exit status 1"), "no current target", []string{"list-panes", "-a"})
	f := &tmuxFetcher{tm: &Tmux{cfg: Config{SocketName: "city-with-no-sessions"}, exec: &fakeExecutor{err: raw}}}

	if _, err := f.FetchState(context.Background()); err != nil {
		t.Fatalf("FetchState() err = %v, want nil for raw \"no current target\" stderr", err)
	}
}

// Control: a genuinely unreachable server must still fail differently. Without
// this the test above would also pass if FetchState swallowed every error.
func TestTmuxFetcher_UnreachableServerStillUnavailable(t *testing.T) {
	f := &tmuxFetcher{tm: &Tmux{cfg: Config{SocketName: "city-with-no-server"}, exec: &fakeExecutor{err: ErrNoServer}}}

	_, err := f.FetchState(context.Background())
	if !errors.Is(err, gcruntime.ErrRuntimeUnavailable) {
		t.Fatalf("FetchState() err = %v, want ErrRuntimeUnavailable for an unreachable server", err)
	}
}

// The deployed failure shape at the cache: a primed city empties out. The cache
// must record the empty snapshot (so subsequent reads are cache hits rather
// than a subprocess-and-log storm) and report the departed session as gone.
func TestStateCache_EmptyServerPrimesCacheAndEndsRefreshStorm(t *testing.T) {
	fe := &fakeExecutor{
		// FetchState issues exactly one executor call (list-panes) per refresh.
		outs: []string{"agent-1\t0\tclaude\t123"},
		errs: []error{nil, ErrNoCurrentTarget, ErrNoCurrentTarget, ErrNoCurrentTarget},
	}
	tm := &Tmux{cfg: Config{SocketName: "trust"}, exec: fe}
	cache := NewStateCache(&tmuxFetcher{tm: tm}, time.Minute)

	if !cache.IsRunning("agent-1") {
		t.Fatal("IsRunning(agent-1) = false after priming refresh, want true")
	}
	cache.Invalidate() // the session exited; next read re-observes

	var logs bytes.Buffer
	restore := captureLog(&logs)
	defer restore()

	if cache.IsRunning("agent-1") {
		t.Error("IsRunning(agent-1) = true after the server emptied, want false")
	}
	callsAfterEmpty := len(fe.calls)

	// Within TTL the empty observation must be a cache hit: no new subprocess.
	for range 5 {
		cache.IsRunning("agent-1")
	}
	if got := len(fe.calls); got != callsAfterEmpty {
		t.Errorf("executor calls = %d after 5 more reads, want %d: the empty snapshot must prime the cache", got, callsAfterEmpty)
	}
	if strings.Contains(logs.String(), "refresh failed") {
		t.Errorf("cache logged a refresh failure for an alive empty server:\n%s", logs.String())
	}
}

// Recovery: once a session comes back the cache must pick it up on the next
// refresh. No restart, no poisoned failure state.
func TestStateCache_RecoversAfterServerRefills(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"", "agent-2\t0\tclaude\t456"},
		errs: []error{ErrNoCurrentTarget, nil},
	}
	tm := &Tmux{cfg: Config{SocketName: "trust"}, exec: fe}
	cache := NewStateCache(&tmuxFetcher{tm: tm}, time.Minute)

	if cache.IsRunning("agent-2") {
		t.Fatal("IsRunning(agent-2) = true against an empty server, want false")
	}
	cache.Invalidate()

	if !cache.IsRunning("agent-2") {
		t.Fatal("IsRunning(agent-2) = false after the server refilled, want true: the cache must recover without a restart")
	}
}

func captureLog(buf *bytes.Buffer) func() {
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	return func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}
}

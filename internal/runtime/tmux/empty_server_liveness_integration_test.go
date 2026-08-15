//go:build integration

package tmux

import (
	"context"
	"errors"
	"testing"
	"time"

	gcruntime "github.com/gastownhall/gascity/internal/runtime"
)

// The ga-jnavd production shape, against a real tmux server: a city server
// configured with `exit-empty off` outlives its last session, and `list-panes
// -a` then exits non-zero with "no current target". The state cache must read
// that as an empty fleet, not as a runtime outage.
func TestStateCache_RealEmptyServerObservesEmptyFleet(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName + "-empty"
	tm := NewTmuxWithConfig(cfg)
	t.Cleanup(func() { _ = tm.TeardownServer() })

	const session = "gc-test-empty-server"
	if err := tm.NewSessionWithCommand(session, t.TempDir(), "sleep 300"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	// exit-empty off is what keeps the server alive with zero sessions; without
	// it the server exits and the failure degenerates into plain ErrNoServer.
	if err := tm.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}

	fetcher := &tmuxFetcher{tm: tm}
	snap, err := fetcher.FetchState(context.Background())
	if err != nil {
		t.Fatalf("FetchState with one live session: %v", err)
	}
	if !snap.Sessions[session].Running {
		t.Fatalf("FetchState did not see the live session; sessions = %v", snap.Sessions)
	}

	if err := tm.KillSession(session); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	// The server must still be up and holding zero sessions. Poll briefly: tmux
	// tears the session down asynchronously from the client's return.
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, err = fetcher.FetchState(context.Background())
		if err == nil && len(snap.Sessions) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("empty server never observed as an empty fleet: err = %v, sessions = %v", err, snap.Sessions)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if errors.Is(err, gcruntime.ErrRuntimeUnavailable) {
		t.Fatalf("FetchState = %v, want a successful empty observation for an alive empty server", err)
	}
	if snap.Sessions == nil {
		t.Fatal("FetchState Sessions = nil for an alive empty server, want an empty non-nil map")
	}

	// Recovery: a new session on the same server must be observed again with no
	// restart and no cache reset.
	if err := tm.NewSessionWithCommand(session, t.TempDir(), "sleep 300"); err != nil {
		t.Fatalf("NewSessionWithCommand after empty: %v", err)
	}
	snap, err = fetcher.FetchState(context.Background())
	if err != nil {
		t.Fatalf("FetchState after the server refilled: %v", err)
	}
	if !snap.Sessions[session].Running {
		t.Fatalf("session not observed after the server refilled; sessions = %v", snap.Sessions)
	}
}

package herdr

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestProviderLiveOccupantSwapKeepsLiveness models the herdr ≥0.7.4 breakage
// against a real herdr binary: the agent's launch shell execs into a different
// process (as claude's shell→TUI boot handoff replaces the pane occupant),
// after which herdr may clear the agent's name from its registry. Whatever
// this herdr version does to the name, the provider contract must hold: the
// session stays running, a re-issued Start refuses with ErrSessionExists
// (never a second placement — that was the spawn storm), and Stop still tears
// the pane down. Opt-in live tier: see requireLiveHerdr.
func TestProviderLiveOccupantSwapKeepsLiveness(t *testing.T) {
	requireLiveHerdr(t)

	p := New("gctest-swap", t.TempDir(), t.TempDir(), 0, 0)
	_ = p.Stop("swap") // clear any leftover from a crashed prior run
	t.Cleanup(func() { _ = p.Stop("swap"); _ = p.TeardownServer() })

	ctx := context.Background()
	cfg := runtime.Config{
		WorkDir: t.TempDir(),
		// The occupant swap: the launch shell replaces itself, mirroring the
		// boot handoff that makes herdr ≥0.7.4 clear the agent's name.
		Command: `exec sleep 120`,
		Env:     map[string]string{"GC_SESSION_ID": "gctest-swap-session"},
	}
	if err := p.Start(ctx, "swap", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Start must have persisted the pane binding — the only stable handle once
	// the name clears.
	if pane, err := p.GetMeta("swap", metaBoundPane); err != nil || pane == "" {
		t.Fatalf("bound pane after Start = %q, %v; want non-empty", pane, err)
	}

	// Give the exec swap time to land, then hold liveness across several
	// checks (the storm fired on every reconcile tick).
	time.Sleep(2 * time.Second)
	for i := 0; i < 3; i++ {
		if !p.IsRunning("swap") {
			t.Fatalf("IsRunning = false after occupant swap (check %d); this re-Start loop is the spawn storm", i)
		}
		if live := p.ObserveLiveness("swap", nil); !live.Running || !live.Alive {
			t.Fatalf("ObserveLiveness = %+v after occupant swap (check %d); want Running=true Alive=true", live, i)
		}
		if err := p.Start(ctx, "swap", cfg); !errors.Is(err, runtime.ErrSessionExists) {
			t.Fatalf("re-issued Start = %v (check %d); want ErrSessionExists", err, i)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Stop must still find and close the pane (via the binding if the name is
	// gone) — the pre-fix "sleep leak" left panes piling up here.
	if err := p.Stop("swap"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for i := 0; i < 10 && p.IsRunning("swap"); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	if p.IsRunning("swap") {
		t.Error("IsRunning = true after Stop")
	}
}

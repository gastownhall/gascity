package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// backgroundWorkLeaf is a provider leaf whose agent passes the idle wait (its
// pane shows the ready prompt and no spinner) while reporting work running
// outside its turn: a Claude Code background shell, a Codex background
// terminal. readErr makes the report fail instead.
type backgroundWorkLeaf struct {
	runtime.Provider
	work    bool
	readErr error
	asked   chan string
}

func newBackgroundWorkLeaf(work bool) *backgroundWorkLeaf {
	return &backgroundWorkLeaf{Provider: runtime.NewFake(), work: work, asked: make(chan string, 4)}
}

func (l *backgroundWorkLeaf) WaitForIdle(context.Context, string, time.Duration) error { return nil }

func (l *backgroundWorkLeaf) HasBackgroundWork(name string) (bool, error) {
	l.asked <- name
	return l.work, l.readErr
}

// probeResult waits for the asynchronous idle probe of beadID to complete and
// returns whether it proved the agent idle. No reconcile tick runs here, so
// nothing clears the ready probe before it is read.
func probeResult(t *testing.T, dt *drainTracker, beadID string) bool {
	t.Helper()
	waitForIdleProbeReady(t, dt, beadID)
	probe, _ := dt.idleProbe(beadID)
	return probe.success
}

// TestIdleProofHeldByBackgroundWork: a session whose agent sits at its ready
// prompt with a background shell still running is not idle for the idle
// sleep. Both idle-proof call sites, the reconciler's asynchronous probe
// (launchIdleProbes) and the effect pass's provedIdle, ask the leaf for
// background work after WaitForIdle succeeds and hold the proof while it
// reports some; without work, or when the leaf cannot read it, the proof
// stands. Kills: a call site that trusts WaitForIdle alone (the live defect:
// the seat was drained with its shell, whose result was lost), an inverted
// read, and a read error that pins a session awake.
func TestIdleProofHeldByBackgroundWork(t *testing.T) {
	info := sessionpkg.Info{ID: "b1", SessionNameMetadata: "worker"}
	launch := func(sp runtime.Provider) bool {
		dt := newDrainTracker()
		launchIdleProbes(context.Background(), map[string]bool{"b1": true}, []wakeTarget{{info: info}},
			dt, sp, clock.Real{}, map[string]sessionpkg.Info{"b1": info})
		return probeResult(t, dt, "b1")
	}
	asked := func(t *testing.T, leaf *backgroundWorkLeaf) {
		t.Helper()
		select {
		case name := <-leaf.asked:
			if name != "worker" {
				t.Fatalf("HasBackgroundWork asked for %q, want worker", name)
			}
		default:
			t.Fatal("HasBackgroundWork was not asked after the idle wait")
		}
	}

	t.Run("launchIdleProbes holds the proof while a shell runs", func(t *testing.T) {
		leaf := newBackgroundWorkLeaf(true)
		if launch(leaf) {
			t.Fatal("idle probe succeeded for an agent with a background shell running")
		}
		asked(t, leaf)
	})
	t.Run("provedIdle holds the proof while a shell runs", func(t *testing.T) {
		leaf := newBackgroundWorkLeaf(true)
		if provedIdle(context.Background(), leaf, "worker", time.Now()) {
			t.Fatal("provedIdle = true for an agent with a background shell running")
		}
		asked(t, leaf)
	})
	t.Run("the observer is read off the routed leaf", func(t *testing.T) {
		leaf := newBackgroundWorkLeaf(true)
		if launch(auto.New(leaf, runtime.NewFake())) {
			t.Fatal("idle probe succeeded through auto for a leaf with a background shell running")
		}
		asked(t, leaf)
	})
	t.Run("no background work proves idle", func(t *testing.T) {
		leaf := newBackgroundWorkLeaf(false)
		if !launch(leaf) {
			t.Fatal("idle probe failed for an idle agent with no background work")
		}
		if !provedIdle(context.Background(), leaf, "worker", time.Now()) {
			t.Fatal("provedIdle = false for an idle agent with no background work")
		}
	})
	t.Run("a read error holds nothing", func(t *testing.T) {
		leaf := newBackgroundWorkLeaf(true)
		leaf.readErr = errors.New("pane gone")
		if !launch(leaf) {
			t.Fatal("idle probe failed on a background-work read error")
		}
		if !provedIdle(context.Background(), leaf, "worker", time.Now()) {
			t.Fatal("provedIdle = false on a background-work read error")
		}
	})
	t.Run("a leaf without the observer proves idle as before", func(t *testing.T) {
		leaf := timeoutRecorder{Provider: runtime.NewFake(), budget: time.Second, timeouts: make(chan time.Duration, 2)}
		if !launch(leaf) {
			t.Fatal("idle probe failed for a leaf without a background-work observer")
		}
		if !provedIdle(context.Background(), leaf, "worker", time.Now()) {
			t.Fatal("provedIdle = false for a leaf without a background-work observer")
		}
	})
}

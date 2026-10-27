package main

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// interruptedCloseEnv is a one-agent, max=1 pool city (fake provider, memory
// store) whose only seat can be stamped into the half-closed shape of
// gastownhall/gascity#2085: a two-write close lost its second write, so the
// ClosePatch landed (metadata state carries this binary's own close code)
// while the status flip to closed was lost. The row then reads as an unknown
// state, and the reconciler's unknown-state skip holds it open forever — the
// canonical identity stays pinned and a max=1 pool is stuck at zero seats.
type interruptedCloseEnv struct {
	cityName string
	cityDir  string
	cfg      *config.City
	store    beads.Store
	sp       runtime.Provider
	clk      *clock.Fake
	dt       *drainTracker
	stderr   *bytes.Buffer
}

const interruptedCloseTemplate = "repo/polecat"

func newInterruptedCloseEnv(t *testing.T) *interruptedCloseEnv {
	t.Helper()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "finish-town"},
		Session:   config.SessionConfig{Provider: "fake"},
		Agents: []config.Agent{{
			Name:              "polecat",
			Dir:               "repo",
			StartCommand:      "true",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(1),
		}},
	}
	return &interruptedCloseEnv{
		cityName: "finish-town",
		cityDir:  t.TempDir(),
		cfg:      cfg,
		store:    beads.NewMemStore(),
		sp:       runtime.NewFake(),
		clk:      &clock.Fake{Time: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)},
		dt:       newDrainTracker(),
		stderr:   &bytes.Buffer{},
	}
}

// halfClose stamps the interrupted close onto the bead: the terminal state
// (and, for a passthrough code, its sleep_reason) this binary's close would
// have written, with the status flip to closed lost.
func (e *interruptedCloseEnv) halfClose(t *testing.T, id, state string, extra map[string]string) {
	t.Helper()
	meta := map[string]string{"state": state}
	for k, v := range extra {
		meta[k] = v
	}
	for k, v := range meta {
		if err := e.store.SetMetadata(id, k, v); err != nil {
			t.Fatalf("SetMetadata(%s=%s): %v", k, v, err)
		}
	}
}

// tick runs one complete non-boot controller pass in the production order:
// desired-state build, session sync (closes deferred to the reconciler), then
// the session reconciler itself.
func (e *interruptedCloseEnv) tick(t *testing.T) {
	t.Helper()
	ds := buildDesiredState(e.cityName, e.cityDir, e.clk.Now(), e.cfg, e.sp, e.store, io.Discard)
	syncSessionBeads(e.cityDir, e.store, ds.State, e.sp, allConfiguredDS(ds.State), e.cfg, e.clk, io.Discard, true)
	rows, err := loadSessionBeads(e.store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	reconcileSessionBeadsAtPath(
		context.Background(), e.cityDir, rows, ds.State, map[string]bool{interruptedCloseTemplate: true},
		e.cfg, e.sp, e.store, newDrainOps(e.sp), nil, nil, nil, e.dt, ds.PoolDesiredCounts, false, nil, e.cityName,
		nil, e.clk, events.Discard, 0, 0, io.Discard, e.stderr,
	)
}

// openSeatRunning reports whether some open session bead of the template has
// a running runtime — the pool is serving seats again.
func (e *interruptedCloseEnv) openSeatRunning(t *testing.T) bool {
	t.Helper()
	rows, err := loadSessionBeads(e.store)
	if err != nil {
		t.Fatalf("load session beads: %v", err)
	}
	for _, b := range rows {
		if b.Status != "open" || b.Metadata["template"] != interruptedCloseTemplate {
			continue
		}
		if name := b.Metadata["session_name"]; name != "" && e.sp.IsRunning(name) {
			return true
		}
	}
	return false
}

// TestReconcileSessionBeads_InterruptedCloseFreesMaxOnePoolSeat is the AC-1
// regression guard for gastownhall/gascity#2085. A max=1 pool's only seat is
// half-closed — the close's ClosePatch landed (state carries this binary's
// own close code) but the status flip to closed was lost — so the row reads
// as an unknown state. One complete non-boot controller tick must finish the
// interrupted close: the bead closes carrying its own close code and the
// canonical close reason, and with the identity freed the pool mints and
// starts a replacement seat to serve ready routed work.
func TestReconcileSessionBeads_InterruptedCloseFreesMaxOnePoolSeat(t *testing.T) {
	cases := []struct {
		name  string
		state string
		extra map[string]string
	}{
		{name: "duplicate", state: "duplicate"},
		{name: "reconfigured", state: "reconfigured"},
		{name: "gc_swept", state: "gc_swept"},
		{name: "stale-session", state: "stale-session"},
		{name: "dead-runtime", state: "dead-runtime"},
		{name: "stranded-repair", state: "stranded-repair"},
		{name: "idle-timeout passthrough", state: "idle-timeout", extra: map[string]string{"sleep_reason": "idle-timeout"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newInterruptedCloseEnv(t)
			zombie := createCanonicalPoolSession(t, env.store, &env.cfg.Agents[0], env.clk.Now(), 1)
			env.halfClose(t, zombie.ID, tc.state, tc.extra)
			createRoutedReadyBeadForReplacement(t, env.store, interruptedCloseTemplate, "queued work")

			// One complete non-boot tick finishes the interrupted close.
			env.tick(t)

			got, err := env.store.Get(zombie.ID)
			if err != nil {
				t.Fatalf("reload zombie: %v", err)
			}
			if got.Status != "closed" {
				t.Fatalf("half-closed seat (state=%q) status = %q, want closed after one tick; metadata=%v",
					tc.state, got.Status, got.Metadata)
			}
			if got.Metadata["state"] != tc.state {
				t.Fatalf("closed bead state = %q, want the interrupted close's own code %q", got.Metadata["state"], tc.state)
			}
			if got.Metadata["close_reason"] != sessionpkg.CanonicalCloseReason(tc.state) {
				t.Fatalf("closed bead close_reason = %q, want %q", got.Metadata["close_reason"], sessionpkg.CanonicalCloseReason(tc.state))
			}

			// With the canonical identity freed, the pool serves the queued
			// bead: an open seat of the template runs again within a few more
			// complete ticks.
			for i := 0; i < 5 && !env.openSeatRunning(t); i++ {
				env.tick(t)
			}
			if !env.openSeatRunning(t) {
				rows, _ := loadSessionBeads(env.store)
				t.Fatalf("no open running seat of %s after the interrupted close freed the identity; session beads: %+v",
					interruptedCloseTemplate, rows)
			}
		})
	}
}

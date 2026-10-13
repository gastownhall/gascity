package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// stopPassProvider runs a controller pass when the kill stops the runtime,
// before the teardown: the window the kill fence covers.
type stopPassProvider struct {
	runtime.Provider
	pass func()
}

func (p *stopPassProvider) Stop(name string) error {
	if p.pass != nil {
		pass := p.pass
		p.pass = nil // one pass per kill
		pass()
	}
	return p.Provider.Stop(name)
}

// An API session kill (Manager.KillFenced, the path both API kill routes
// take) is not a crash to the controller, under v2 and legacy alike
// (mc-fr77m). The seat is an idle pool seat that woke ten seconds ago, so a
// death read as a crash would accrue a rapid wake failure. A pass runs at the
// stop, inside the fence: it must not heal the asleep row back to awake or
// restart the runtime. Afterwards the controller settles: nothing accrued,
// and the idle killed seat frees its slot (it closes, SESS-623).
func TestAPIKillUnderFenceIsNotACrash(t *testing.T) {
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	f := parityFixture{
		Name: "an API kill of a fresh idle pool seat",
		Rows: []parityRow{poolRow("gc-1", "worker", 1, "awake", "instance_token", "tok-gc-1",
			"last_woke_at", at(-10*time.Second), "session_key", "conv-1")},
		Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1"}},
	}
	type world struct {
		store beads.Store
		sp    runtime.Provider
		clk   clock.Clock
		pass  func()
		// outside attributes the store's changes since the last pass to
		// the kill, an outside writer (the v2 simulator audits every
		// write against its actor).
		outside func()
		final   func() []beads.Bead
	}
	worlds := map[string]func(t *testing.T, cityPath string, rows []beads.Bead) world{
		"legacy": func(t *testing.T, cityPath string, rows []beads.Bead) world {
			w := newLegacyWorld(t, f, cityPath, rows)
			return world{
				store: w.store, sp: w.sp, clk: w.clk, pass: func() { w.tick(t, true) }, outside: func() {},
				final: func() []beads.Bead { w.settle(t, f.Name); return w.list(t) },
			}
		},
		"v2": func(t *testing.T, cityPath string, rows []beads.Bead) world {
			w := newV2World(t, f, cityPath, rows, nil, "", nil)
			return world{
				store: w.s.legs[0].backing, sp: w.sp, clk: w.s.clk, pass: func() { w.once(t) },
				outside: func() { w.s.audit("api-kill") },
				final: func() []beads.Bead {
					// The kill's writes reach v2's cache as events, so a
					// pass may decide on the row before them and its effect
					// refuse on the fresh read; the refused intent is
					// deferred, and retried as time passes.
					var rows []beads.Bead
					for step := 0; step < 10; step++ {
						w.settle(t, f.Name)
						var err error
						if rows, err = w.s.legs[0].backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true}); err != nil {
							t.Fatal(err)
						}
						if i := slices.IndexFunc(rows, func(b beads.Bead) bool { return b.ID == "gc-1" }); i >= 0 && rows[i].Status == "closed" {
							break
						}
						w.s.advance(30 * time.Second)
					}
					return rows
				},
			}
		},
	}
	for name, build := range worlds {
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			w := build(t, cityPath, f.converged(t, cityPath))
			var atStop beads.Bead
			sp := &stopPassProvider{Provider: w.sp}
			sp.pass = func() {
				w.outside()
				w.pass()
				atStop, _ = w.store.Get("gc-1")
			}
			mgr := session.NewManagerWithOptions(w.store, sp, session.WithCityPath(cityPath), session.WithClock(w.clk))
			kill := func(ctx context.Context, by session.Actor) error { return mgr.Kill(ctx, by, "gc-1") }
			if err := mgr.KillFenced(context.Background(), testActorIn(t, session.ActorOperator, cityPath), "gc-1", kill); err != nil {
				t.Fatalf("KillFenced: %v", err)
			}
			w.outside()

			// The pass inside the fence left the killed row alone.
			if got := atStop.Metadata["state"]; got != string(session.StateAsleep) {
				t.Errorf("state after the pass at the stop = %q, want asleep (a pass inside the fence healed the kill)", got)
			}
			if got := atStop.Metadata["state_reason"]; got != session.KillPendingReason {
				t.Errorf("state_reason after the pass at the stop = %q, want the kill fence", got)
			}
			if w.sp.IsRunning("s-gc-1") {
				t.Fatal("the runtime runs after the kill: a pass restarted it")
			}

			var seat beads.Bead
			for _, b := range w.final() {
				if b.ID == "gc-1" {
					seat = b
				}
			}
			for _, k := range []string{"wake_attempts", "churn_count", "continuation_reset_pending", "quarantined_until"} {
				if v := seat.Metadata[k]; v != "" {
					t.Errorf("%s = %q after the kill settled, want nothing accrued: the kill was read as a crash", k, v)
				}
			}
			if seat.Status != "closed" {
				t.Errorf("idle killed pool seat status = %q, want closed: its slot is freed\nrow %v", seat.Status, seat.Metadata)
			}
			if w.sp.IsRunning("s-gc-1") {
				t.Error("the killed seat's runtime was restarted")
			}
		})
	}
}

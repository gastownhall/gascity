package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
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

// A `gc session kill` of a seat AL1 wants awake is no wake gate (CONTRACT
// v5.10d, §12.2 #7; legacy's kill help text and kill_fence.go): a killed
// manual session and a killed named mode=always session each restart as a
// new incarnation, generation+1 under a new instance token, and the kill
// accrues no wake failure, in legacy and v2 alike. The v2 copy replays
// staging's race (lmcp): its first pass after the kill reads an inventory
// listed before it, so A4 proposes the operator-dormant stop of a runtime
// the kill already ended; that stop settles as a no-op and notes the runtime
// gone, so the next pass, on the same stale inventory a second later,
// restarts the row: no 10s row backoff to wait out, and no patrol after it.
// The simulator's invariants hold throughout.
func TestKilledWokenSeatRestartsAsANewIncarnationInBothModes(t *testing.T) {
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	chat := func() *config.City {
		cfg := workerCity(3)
		cfg.Agents = append(cfg.Agents, config.Agent{Name: "chat", StartCommand: "true"})
		cfg.NamedSessions = []config.NamedSession{{Template: "chat", Mode: "always"}}
		return cfg
	}
	for _, c := range []struct {
		name, id, sessionName string
		f                     parityFixture
	}{
		{"a manual session", "gc-1", "s-gc-1", parityFixture{
			Name: "a killed manual session",
			Rows: []parityRow{sessionRow("gc-1", "template", "worker", "agent_name", "worker", "alias", "conversation",
				"session_name", "s-gc-1", "session_origin", "manual", "state", "awake", "command", "true",
				"generation", "1", "continuation_epoch", "1", "instance_token", "tok-gc-1", "last_woke_at", at(-time.Hour))},
			Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1"}},
		}},
		{"a named mode=always session", "gc-c", "chat", parityFixture{
			Name: "a killed always-named session", City: chat,
			Rows:     []parityRow{chatRow("gc-c", "1", "state", "awake", "session_name", "chat", "instance_token", "tok-gc-c", "last_woke_at", at(-time.Hour))},
			Runtimes: []simRuntime{{id: "gc-c", epoch: "1", token: "tok-gc-c"}},
		}},
	} {
		type world struct {
			store beads.Store
			sp    runtime.Provider
			clk   clock.Clock
			// rest settles the seat before the kill; killed runs after it: v2
			// audits the outside write and replays the race.
			rest, killed func()
			// settle runs the controller to rest, time advancing until the
			// row restarts.
			settle func() beads.Bead
		}
		get := func(t *testing.T, store beads.Store) beads.Bead {
			t.Helper()
			b, err := store.Get(c.id)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		restarted := func(b beads.Bead) bool { return b.Metadata["generation"] == "2" }
		worlds := map[string]func(t *testing.T, cityPath string, rows []beads.Bead) world{
			"legacy": func(t *testing.T, cityPath string, rows []beads.Bead) world {
				w := newLegacyWorld(t, c.f, cityPath, rows)
				return world{
					store: w.store, sp: w.sp, clk: w.clk, rest: func() { w.settle(t, c.f.Name) }, killed: func() {},
					settle: func() beads.Bead {
						for step := 0; step < 10; step++ {
							w.settle(t, c.f.Name)
							if b := get(t, w.store); restarted(b) {
								return b
							}
							w.clk.Advance(simPatrol)
						}
						return get(t, w.store)
					},
				}
			},
			"v2": func(t *testing.T, cityPath string, rows []beads.Bead) world {
				w := newV2World(t, c.f, cityPath, rows, nil, "", nil)
				s := w.s
				s.p.observations = func() *ObservationCache { return s.lane.cache } // as the controller wires it (bindHost)
				store := s.legs[0].backing
				return world{
					store: store, sp: w.sp, clk: s.clk, rest: func() { w.settle(t, c.f.Name) },
					killed: func() {
						s.audit("api-kill")
						for _, l := range s.legs { // the census reads the kill, as staging's did
							for _, ev := range l.events {
								l.cache.ApplyEvent("bead.updated", ev)
							}
							l.events = nil
						}
						// Two passes on the pre-kill inventory, a second after it
						// (well inside the 10s row backoff a refusal would set).
						s.advance(time.Second)
						for range 2 {
							s.pass()
							for len(s.parked) > 0 {
								s.release(0)
							}
						}
						if b := get(t, store); !restarted(b) {
							t.Errorf("not restarted on the pass after the stale dormant stop: row %v\n%s", b.Metadata, strings.Join(s.schedule, "\n"))
						}
					},
					settle: func() beads.Bead {
						defer s.noViolations(t)
						for step := 0; step < 10; step++ {
							w.settle(t, c.f.Name)
							if b := get(t, store); restarted(b) {
								return b
							}
							s.advance(simPatrol)
						}
						return get(t, store)
					},
				}
			},
		}
		for mode, build := range worlds {
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				cityPath := t.TempDir()
				w := build(t, cityPath, c.f.converged(t, cityPath))
				w.rest()
				if b := get(t, w.store); !w.sp.IsRunning(c.sessionName) || b.Metadata["generation"] != "1" {
					t.Fatalf("premise: the seat runs at generation 1 before the kill (row %v)", b.Metadata)
				}
				mgr := session.NewManagerWithOptions(w.store, w.sp, session.WithCityPath(cityPath), session.WithClock(w.clk))
				kill := func(ctx context.Context, by session.Actor) error { return mgr.Kill(ctx, by, c.id) }
				if err := mgr.KillFenced(context.Background(), testActorIn(t, session.ActorOperator, cityPath), c.id, kill); err != nil {
					t.Fatalf("KillFenced: %v", err)
				}
				if w.sp.IsRunning(c.sessionName) {
					t.Fatal("premise: the kill left the runtime running")
				}
				w.killed()
				seat := w.settle()
				if seat.Status == "closed" || seat.Metadata["generation"] != "2" {
					t.Fatalf("status %s, generation %q, want open at generation 2 (a new incarnation)\nrow %v", seat.Status, seat.Metadata["generation"], seat.Metadata)
				}
				if tok := seat.Metadata["instance_token"]; tok == "" || tok == "tok-"+c.id {
					t.Errorf("instance_token %q, want a new one", tok)
				}
				if v := seat.Metadata["wake_attempts"]; v != "" && v != "0" {
					t.Errorf("wake_attempts = %q, want none: the kill was read as a failed wake", v)
				}
				if !w.sp.IsRunning(c.sessionName) {
					t.Error("the restarted seat's runtime is not running")
				}
			})
		}
	}
}

// fenceLiftStore runs lifted once, right after the write that lifts a kill
// fence (state_reason cleared) lands. It hides conditional writes, so every
// kill write is a plain batch write it observes.
type fenceLiftStore struct {
	beads.Store
	lifted func()
}

func (s *fenceLiftStore) SetMetadataBatch(id string, kvs map[string]string) error {
	err := s.Store.SetMetadataBatch(id, kvs)
	if v, ok := kvs["state_reason"]; err == nil && ok && v == "" && s.lifted != nil {
		lifted := s.lifted
		s.lifted = nil
		lifted()
	}
	return err
}

// The kill fence covers the whole kill (CONTRACT v5.10p; staging OPS11 J16
// A, lmc8): a controller pass that runs the instant the kill lifts its fence
// finds the kill's runtime lease released, so v2's operator-dormant stop of
// the gone runtime takes the name, settles as the no-op (v5.10d (a)), and
// the seat restarts at once: no name-busy refusal, no 10s row backoff. The
// legacy copy, ticked at the same instant, restarts it too. Before v5.10p the
// fence lifted while the lease was still held: the stop refused name-busy
// and the restart waited out the backoff.
func TestKillFenceLiftsAfterTheLeaseInBothModes(t *testing.T) {
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	f := parityFixture{
		Name: "a killed manual session, a pass at the fence lift",
		Rows: []parityRow{sessionRow("gc-1", "template", "worker", "agent_name", "worker", "alias", "conversation",
			"session_name", "s-gc-1", "session_origin", "manual", "state", "awake", "command", "true",
			"generation", "1", "continuation_epoch", "1", "instance_token", "tok-gc-1", "last_woke_at", at(-time.Hour))},
		Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1"}},
	}
	restarted := func(b beads.Bead) bool { return b.Metadata["generation"] == "2" }
	type world struct {
		store   beads.Store
		sp      runtime.Provider
		clk     clock.Clock
		atLift  func()                   // the controller's passes at the fence lift
		advance func(time.Duration) bool // settles, then advances; reports the restart
		check   func(t *testing.T)       // the copy's own assertions
		get     func() beads.Bead
	}
	worlds := map[string]func(t *testing.T, cityPath string, rows []beads.Bead) world{
		"legacy": func(t *testing.T, cityPath string, rows []beads.Bead) world {
			w := newLegacyWorld(t, f, cityPath, rows)
			w.settle(t, f.Name)
			get := func() beads.Bead { b, _ := w.store.Get("gc-1"); return b }
			return world{
				store: w.store, sp: w.sp, clk: w.clk, get: get,
				atLift: func() { w.tick(t, true) },
				advance: func(d time.Duration) bool {
					w.settle(t, f.Name)
					w.clk.Advance(d)
					return restarted(get())
				},
				check: func(*testing.T) {},
			}
		},
		"v2": func(t *testing.T, cityPath string, rows []beads.Bead) world {
			w := newV2World(t, f, cityPath, rows, nil, "", nil)
			s := w.s
			s.p.observations = func() *ObservationCache { return s.lane.cache } // as the controller wires it (bindHost)
			w.settle(t, f.Name)
			store := s.legs[0].backing
			get := func() beads.Bead { b, _ := store.Get("gc-1"); return b }
			return world{
				store: store, sp: w.sp, clk: s.clk, get: get,
				atLift: func() {
					s.audit("api-kill")
					for _, l := range s.legs { // the census reads the lift at once
						for _, ev := range l.events {
							l.cache.ApplyEvent("bead.updated", ev)
						}
						l.events = nil
					}
					for range 2 { // on the inventory listed before the kill, as staging's pass
						s.pass()
						for len(s.parked) > 0 {
							s.release(0)
						}
					}
				},
				advance: func(d time.Duration) bool {
					w.settle(t, f.Name)
					s.advance(d)
					return restarted(get())
				},
				check: func(t *testing.T) {
					s.noViolations(t)
					for _, line := range s.schedule {
						if strings.Contains(line, causeNameBusy) {
							t.Errorf("a v2 effect refused %s after the kill:\n%s", causeNameBusy, strings.Join(s.schedule, "\n"))
							break
						}
					}
				},
			}
		},
	}
	for mode, build := range worlds {
		t.Run(mode, func(t *testing.T) {
			cityPath := t.TempDir()
			w := build(t, cityPath, f.converged(t, cityPath))
			if !w.sp.IsRunning("s-gc-1") || w.get().Metadata["generation"] != "1" {
				t.Fatalf("premise: the seat runs at generation 1 before the kill (row %v)", w.get().Metadata)
			}
			store := &fenceLiftStore{Store: w.store}
			lifted := false
			store.lifted = func() { lifted = true; w.atLift() }
			mgr := session.NewManagerWithOptions(store, w.sp, session.WithCityPath(cityPath), session.WithClock(w.clk))
			kill := func(ctx context.Context, by session.Actor) error { return mgr.Kill(ctx, by, "gc-1") }
			if err := mgr.KillFenced(context.Background(), testActorIn(t, session.ActorOperator, cityPath), "gc-1", kill); err != nil {
				t.Fatalf("KillFenced: %v", err)
			}
			if !lifted {
				t.Fatal("premise: the kill never lifted its fence")
			}
			// Restarted within the backoff's 10s, a second at a time.
			var elapsed time.Duration
			for !restarted(w.get()) && elapsed < backoffBase-time.Second {
				w.advance(time.Second)
				elapsed += time.Second
			}
			seat := w.get()
			if !restarted(seat) {
				t.Fatalf("not restarted %s after the kill (within the 10s row backoff): row %v", elapsed, seat.Metadata)
			}
			if tok := seat.Metadata["instance_token"]; tok == "" || tok == "tok-gc-1" {
				t.Errorf("instance_token %q, want a new one", tok)
			}
			if v := seat.Metadata["wake_attempts"]; v != "" && v != "0" {
				t.Errorf("wake_attempts = %q, want none", v)
			}
			w.check(t)
		})
	}
}

// leaseClearRefusingStore refuses the runtime lease record's clear (it hides
// conditional writes, so the release takes the plain batch write).
type leaseClearRefusingStore struct{ beads.Store }

func (s leaseClearRefusingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if h, ok := kvs[session.RuntimeLeaseHolderKey]; ok && h == "" {
		return errors.New("store unavailable")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// A kill whose lease release fails keeps its fence (CONTRACT v5.10p), and
// the fence's grace still recovers the row: v2 leaves the killed manual
// session down while the fence is honored, then restarts it as a new
// incarnation once KillPendingGrace has passed. The uncleared record names
// the kill's dead flock, so it keeps no start out.
func TestV2KillFenceGraceRecoversAFailedLeaseRelease(t *testing.T) {
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	f := parityFixture{
		Name: "a killed manual session whose lease release failed",
		Rows: []parityRow{sessionRow("gc-1", "template", "worker", "agent_name", "worker", "alias", "conversation",
			"session_name", "s-gc-1", "session_origin", "manual", "state", "awake", "command", "true",
			"generation", "1", "continuation_epoch", "1", "instance_token", "tok-gc-1", "last_woke_at", at(-time.Hour))},
		Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1"}},
	}
	cityPath := t.TempDir()
	w := newV2World(t, f, cityPath, f.converged(t, cityPath), nil, "", nil)
	s := w.s
	w.settle(t, f.Name)
	store := s.legs[0].backing
	mgr := session.NewManagerWithOptions(leaseClearRefusingStore{store}, w.sp, session.WithCityPath(cityPath), session.WithClock(s.clk))
	kill := func(ctx context.Context, by session.Actor) error { return mgr.Kill(ctx, by, "gc-1") }
	if err := mgr.KillFenced(context.Background(), testActorIn(t, session.ActorOperator, cityPath), "gc-1", kill); err != nil {
		t.Fatalf("KillFenced: %v", err)
	}
	s.audit("api-kill")
	get := func() beads.Bead { b, _ := store.Get("gc-1"); return b }
	if b := get(); b.Metadata["state_reason"] != session.KillPendingReason || b.Metadata[session.RuntimeLeaseHolderKey] == "" {
		t.Fatalf("premise: the failed release kept the fence over its record: row %v", b.Metadata)
	}
	for elapsed := time.Duration(0); elapsed < session.KillPendingGrace-simPatrol; elapsed += simPatrol {
		w.settle(t, f.Name)
		if b := get(); b.Metadata["generation"] != "1" || w.sp.IsRunning("s-gc-1") {
			t.Fatalf("restarted %s into an honored kill fence: row %v", elapsed, b.Metadata)
		}
		s.advance(simPatrol)
	}
	for step := 0; step < 4 && get().Metadata["generation"] == "1"; step++ {
		s.advance(simPatrol)
		w.settle(t, f.Name)
	}
	seat := get()
	if seat.Metadata["generation"] != "2" || !w.sp.IsRunning("s-gc-1") {
		t.Fatalf("not restarted past the fence's grace: row %v\n%s", seat.Metadata, strings.Join(s.schedule, "\n"))
	}
	if v := seat.Metadata["wake_attempts"]; v != "" && v != "0" {
		t.Errorf("wake_attempts = %q, want none", v)
	}
	s.noViolations(t)
}

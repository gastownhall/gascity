package main

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// C6d's idle respawn in the simulator (#7688 review): its invariants, which
// every sim checks, and the directed lifecycle scenarios the corpus never
// reaches (its pool agent has no sleep_after_idle).

func init() {
	simWriteChecks = append(simWriteChecks, (*sim).checkRespawnBegin)
	simEffectChecks = append(simEffectChecks, (*sim).checkRespawnSignal)
}

// checkRespawnBegin is I25 (I-respawn), on v2's writes: at most one
// idle-respawn begin per (row, bead), so a bead whose respawn drain was
// voided or completed never begins another.
func (s *sim) checkRespawnBegin(w simWrite) {
	b, a := w.Before.Metadata, w.After.Metadata
	if a[session.DrainIntentReasonKey] != idleRespawnDrainReason || b[session.DrainIntentReasonKey] == idleRespawnDrainReason {
		return
	}
	key := w.Leg + "/" + w.After.ID + "/" + a[idleRespawnBeadIDMetadataKey]
	if s.respawnBegun == nil {
		s.respawnBegun = map[string]bool{}
	}
	if s.respawnBegun[key] {
		s.failf("I25 I-respawn", "a second idle-respawn begin for %s", key)
	}
	s.respawnBegun[key] = true
}

// checkRespawnSignal is I25 (I-respawn), on an idle-respawn signal: one
// released while the seat already held an in-progress claim never leaves the
// row stop-pending (its revalidation voids it).
func (s *sim) checkRespawnSignal(e *simEffect) func(settlement) {
	l := s.legOf(e.it.Key.Leg)
	if !respawnSignal(e.it) || l == nil {
		return func(settlement) {}
	}
	claimed, _ := l.backing.List(beads.ListQuery{Assignee: e.it.Key.ID, Status: "in_progress", AllowScan: true})
	return func(settlement) {
		if b, err := l.backing.Get(e.it.Key.ID); err == nil && len(claimed) > 0 && signaledRow(b) {
			s.failf("I25 I-respawn", "an idle-respawn signal stopped %s over its in-progress claim %s", e.it.Key.ID, claimed[0].ID)
		}
	}
}

// restartController replaces the planner, as a controller restart does: its
// in-memory state (backoff, memo, in-flight map) is gone; the stores and the
// runtimes stay. The released effects' writes are checked first, against the
// controller that wrote them (I4's zombie exception reads its
// classifications), and I9's start window begins again, as a new
// controller's token bucket is full. It models a graceful restart only: no
// crash inside an effect, and the observation cache stays warm, where a real
// restart starts cold.
func (s *sim) restartController() {
	for len(s.parked) > 0 {
		s.release(0)
	}
	s.audit("v2")
	s.admitted = nil
	s.inflight = newInflightMap()
	s.p = newPlanner(s.clk, func() time.Duration { return simPatrol }, nil, s.inflight, nil, io.Discard)
	s.p.pass = func(now time.Time) passResult { return s.p.tracePass(s.env, now) }
	s.p.effects = s.x
	s.p.seam = s.atSeam
	s.lastSeq = 0
	s.logf("controller restart")
}

// respawnSim is a sim with one live pool seat, gc-1, awake only for its
// ready, unclaimed bead gw-1, on a one-minute sleep_after_idle, its agent
// idle (WaitForIdle succeeds).
func respawnSim(t *testing.T) *sim {
	work := beads.Bead{ID: "gw-1", Title: "work", Type: "task", Status: "open", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": "worker"}}
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1"), work)
	s.cfg.Agents[0].SleepAfterIdle = "1m"
	s.sp.mu.Lock()
	s.sp.WaitForIdleErrors = map[string]error{"s-gc-1": nil}
	s.sp.mu.Unlock()
	return s
}

// tick is one patrol of the controller: a published inventory, a pass, its
// effects run out, and twenty seconds.
func (s *sim) tick() {
	s.settle()
	s.advance(20 * time.Second)
}

// tickUntil ticks until done reports true, at most n times.
func (s *sim) tickUntil(t *testing.T, n int, what string, done func() bool) {
	t.Helper()
	for range n {
		if done() {
			return
		}
		s.tick()
	}
	if !done() {
		t.Fatalf("never %s in %d ticks; violated %q; row %v", what, n, s.failures, s.gc1())
	}
}

// respawnRequested reports gc-1 holding an idle-respawn request, not yet
// signaled.
func (s *sim) respawnRequested() bool {
	m := s.gc1()
	return m[session.DrainIntentReasonKey] == idleRespawnDrainReason && m["state_reason"] != session.DrainAckStopPendingReason
}

// workerObject is gc-1's runtime object under s-gc-1, 0 when there is none.
func (s *sim) workerObject() int64 {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	if rt := s.sp.rts["s-gc-1"]; rt != nil {
		return rt.object
	}
	return 0
}

// TestSimIdleRespawnLifecycle (v5 D2, SESS-614, DRAIN-004/044; #7688
// review). Kills an idle respawn that recycles a seat mid-work or twice for
// one bead: the seat respawns once when idle, never when its idle proof
// fails, and a claim, activity, an attach or a controller restart between
// the begin and the stop each hold or void it, as does A15's stop
// pre-empting a requested respawn, with I25 (one begin per bead; no signal
// over a held claim) and every other invariant checked.
func TestSimIdleRespawnLifecycle(t *testing.T) {
	t.Run("respawns once", func(t *testing.T) {
		s := respawnSim(t)
		first := s.workerObject()
		s.tickUntil(t, 20, "respawned", func() bool { o := s.workerObject(); return o != 0 && o != first })
		for range 10 {
			s.tick()
		}
		if m := s.gc1(); m[idleRespawnAttemptsMetadataKey] != "1" || m[idleRespawnBeadIDMetadataKey] != "gw-1" || m[session.DrainIntentReasonKey] != "" {
			t.Fatalf("row %v: want the one attempt for gw-1 spent and no request", m)
		}
		s.noViolations(t)
	})
	t.Run("idle unproven", func(t *testing.T) {
		s := respawnSim(t)
		s.sp.mu.Lock()
		s.sp.WaitForIdleErrors["s-gc-1"] = errors.New("busy")
		s.sp.mu.Unlock()
		first := s.workerObject()
		for range 20 {
			s.tick()
			if s.gc1()[session.DrainIntentReasonKey] != "" || s.workerObject() != first {
				t.Fatalf("a respawn of a seat not proven idle: row %v", s.gc1())
			}
		}
		s.noViolations(t)
	})
	for _, c := range []struct {
		name    string
		between func(s *sim)
	}{
		{"claimed between begin and signal", func(s *sim) {
			l := s.legs[0]
			b, _ := l.backing.Get("gw-1")
			s.outside("claim", l, b, func(l *simLeg, b beads.Bead) {
				status := "in_progress"
				if err := l.backing.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
					s.t.Fatal(err)
				}
			})
		}},
		{"active between begin and signal", func(s *sim) {
			s.sp.mu.Lock()
			s.sp.SetActivity("s-gc-1", s.clk.Now().Add(time.Second))
			s.sp.mu.Unlock()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := respawnSim(t)
			first := s.workerObject()
			s.tickUntil(t, 20, "requested a respawn", s.respawnRequested)
			c.between(s)
			s.tickUntil(t, 10, "voided the respawn", func() bool { return s.gc1()[session.DrainIntentReasonKey] == "" })
			for range 10 {
				s.tick()
			}
			if m := s.gc1(); s.workerObject() != first || m[idleRespawnAttemptsMetadataKey] != "1" || m[session.DrainIntentReasonKey] != "" {
				t.Fatalf("object %d (was %d), row %v: want the respawn voided, its attempt kept, the seat running", s.workerObject(), first, m)
			}
			s.noViolations(t)
		})
	}
	t.Run("attached while stop-pending", func(t *testing.T) {
		s := respawnSim(t)
		first := s.workerObject()
		s.tickUntil(t, 20, "signaled the respawn", func() bool { return signaledRow(s.legs[0].mustGet(t, "gc-1")) })
		s.sp.mu.Lock()
		s.sp.rts["s-gc-1"].attached = true
		s.sp.touch("s-gc-1")
		s.sp.mu.Unlock()
		for range 6 {
			s.tick()
			if s.workerObject() != first {
				t.Fatalf("stopped the seat while a human is attached; row %v", s.gc1())
			}
		}
		s.sp.mu.Lock()
		s.sp.rts["s-gc-1"].attached = false
		s.sp.touch("s-gc-1")
		s.sp.mu.Unlock()
		s.tickUntil(t, 40, "respawned after the detach", func() bool { o := s.workerObject(); return o != 0 && o != first })
		s.noViolations(t)
	})
	t.Run("A15 pre-empts a requested respawn", func(t *testing.T) {
		s := respawnSim(t)
		s.cfg.Agents[0].IdleTimeout = "30s" // A15's idle timeout, its activity long past
		s.sp.mu.Lock()
		s.sp.SetActivity("s-gc-1", s.clk.Now().Add(-10*time.Minute))
		s.sp.mu.Unlock()
		s.tickUntil(t, 20, "requested a respawn", s.respawnRequested)
		for range 30 {
			s.tick()
		}
		if len(s.respawnBegun) != 1 || s.gc1()[session.DrainIntentReasonKey] == idleRespawnDrainReason {
			t.Fatalf("respawn begins %v, row %v: want A15's stop to end the one requested respawn, none begun again", s.respawnBegun, s.gc1())
		}
		s.noViolations(t)
	})
	t.Run("controller restart between begin and signal", func(t *testing.T) {
		s := respawnSim(t)
		first := s.workerObject()
		s.tickUntil(t, 20, "requested a respawn", s.respawnRequested)
		s.restartController()
		s.tickUntil(t, 40, "respawned", func() bool { o := s.workerObject(); return o != 0 && o != first })
		for range 10 {
			s.tick()
		}
		s.noViolations(t)
	})
}

// mustGet is the backing's row id.
func (l *simLeg) mustGet(t *testing.T, id string) beads.Bead {
	t.Helper()
	b, err := l.backing.Get(id)
	if err != nil && !strings.Contains(err.Error(), "not found") {
		t.Fatal(err)
	}
	return b
}

// TestSimRespawnChecksBite (I25). Kills an I25 that checks nothing, or too
// much: a second idle-respawn begin for one (row, bead) reports, and a begin
// for a new bead on the same row does not; an idle-respawn signal that left
// the row stop-pending over a claim the seat held when it was released
// reports, and one over a claim taken after its release does not.
func TestSimRespawnChecksBite(t *testing.T) {
	begin := func(id string) simWrite {
		return simWrite{Leg: simCityLeg, Before: sessionRow(id), After: sessionRow(id, session.DrainIntentReasonKey, idleRespawnDrainReason, idleRespawnBeadIDMetadataKey, "gw-1")}
	}
	s := respawnSim(t)
	s.checkRespawnBegin(begin("gc-1"))
	s.checkRespawnBegin(begin("gc-2")) // another row's begin for the bead
	next := begin("gc-1")
	next.After.Metadata[idleRespawnBeadIDMetadataKey] = "gw-2"
	s.checkRespawnBegin(next) // the row's begin for a new bead
	if len(s.failures) != 0 {
		t.Fatalf("one begin per (row, bead) reported %q", s.failures)
	}
	s.checkRespawnBegin(begin("gc-1"))
	if len(s.failures) != 1 || !strings.HasPrefix(s.failures[0], "I25 ") {
		t.Fatalf("a second begin for gc-1/gw-1 reported %q, want I25", s.failures)
	}

	for _, late := range []bool{false, true} {
		s = respawnSim(t)
		l := s.legs[0]
		claim := func() {
			status := "in_progress"
			if err := l.backing.Update("gw-1", beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatal(err)
			}
		}
		if !late {
			claim()
		}
		settle := s.checkRespawnSignal(&simEffect{it: intent{Kind: intentSignalFresh, Key: rowKey{Leg: rowLeg, ID: "gc-1"}, Reason: decideDrainSignal + idleRespawnDrainReason}})
		if late {
			claim() // after the release: the effect's probe could not see it
		}
		s.setMeta(l, "gc-1", "state", string(session.StateDraining), "state_reason", session.DrainAckStopPendingReason)
		settle(settlement{Outcome: settledLanded})
		switch {
		case late && len(s.failures) != 0:
			t.Fatalf("a signal over a claim taken after its release reported %q", s.failures)
		case !late && (len(s.failures) != 1 || !strings.HasPrefix(s.failures[0], "I25 ")):
			t.Fatalf("a signal over a held claim reported %q, want I25", s.failures)
		}
	}
}

// TestSimRestartChecksTheOldControllersWrites (the #7688 review's restart
// helper). Kills a restart that swaps the planner before the writes of the
// effects it released are checked, so the checks judge them against the new
// controller's empty memory, or that keeps I9's start window across it.
func TestSimRestartChecksTheOldControllersWrites(t *testing.T) {
	s := scripted(t, nil, heldRow())
	s.inventory()
	s.pass()
	if len(s.parked) != 1 {
		t.Fatalf("parked %d effects, want gc-1's heal", len(s.parked))
	}
	old := s.p
	var saw []*planner
	checks := simWriteChecks
	t.Cleanup(func() { simWriteChecks = checks })
	simWriteChecks = append(slices.Clone(checks), func(s *sim, w simWrite) {
		if w.After.ID == "gc-1" {
			saw = append(saw, s.p)
		}
	})
	s.admitted = []time.Time{s.clk.Now()}
	s.restartController()
	if len(saw) != 1 || saw[0] != old {
		t.Fatalf("the heal's write was checked %d times, under the old planner %t; want once, under it", len(saw), len(saw) > 0 && saw[0] == old)
	}
	if len(s.admitted) != 0 {
		t.Fatalf("I9's window kept %d starts across the restart", len(s.admitted))
	}
	s.noViolations(t)
}

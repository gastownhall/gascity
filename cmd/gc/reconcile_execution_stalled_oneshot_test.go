package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// A16's execution-stalled request lifecycle (C7b1; mc-uinl0 ruling): one
// request drains a seat at most once, as legacy's latch allows one
// escalation per instance token per 30 minutes, and a stall that persists is
// posted again by the backstop.

// Kills the request outliving its landed drain begin: an operator who
// resumes the seat (clearing its stop request at the same generation) has it
// drained again within a patrol. Only a new request drains it again.
func TestSimExecutionStalledRequestIsOneShot(t *testing.T) {
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1"), gc1Work())
	s.settle()
	begun := func() bool {
		for range 4 {
			if s.gc1()[session.DrainIntentReasonKey] == executionStalledDrainReason {
				return true
			}
			s.advance(simPatrol)
			s.settle()
		}
		return s.gc1()[session.DrainIntentReasonKey] == executionStalledDrainReason
	}
	s.p.postExecutionStalled(executionStalledRequest{ID: "gc-1", Generation: "1", At: s.clk.Now()})
	if !begun() {
		t.Fatalf("row %v, want the execution-stalled drain begun", s.gc1())
	}
	// The operator resumes the row before its stop: the stop request clears.
	s.operator("gc-1", session.DrainIntentReasonKey, "", session.DrainIntentAtKey, "", session.DrainIntentIncarnationKey, "")
	for elapsed := time.Duration(0); elapsed < 30*time.Minute; elapsed += simPatrol {
		s.advance(simPatrol)
		s.settle()
		if m, l := s.gc1(), s.liveness("gc-1"); m[session.DrainIntentReasonKey] != "" || m["generation"] != "1" || l != livenessAlive {
			t.Fatalf("%s after the resume: row %v liveness %v, want it left alone", elapsed+simPatrol, m, l)
		}
	}
	// The backstop's re-trigger past its latch drains it again.
	s.p.postExecutionStalled(executionStalledRequest{ID: "gc-1", Generation: "1", At: s.clk.Now()})
	if !begun() {
		t.Fatalf("row %v, want a new request to drain it again", s.gc1())
	}
	s.noViolations(t)
}

// Kills a request that never led to a landed begin outliving its runtime
// (the #7658 review's probe): the backstop posts, the operator suspends the
// seat and its runtime stops before any pass, and later resumes it at the
// same generation and token. Legacy's tracked drain completed when the
// runtime stopped; v2's request is spent once a pass reads the seat dormant,
// so the resume is not drained without a new request.
func TestSimExecutionStalledRequestSpentByADormantSeat(t *testing.T) {
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1"), gc1Work())
	s.settle()
	s.p.postExecutionStalled(executionStalledRequest{ID: "gc-1", Generation: "1", At: s.clk.Now()})
	s.operator("gc-1", "state", "suspended", "sleep_intent", "user-hold", "held_until", s.rel(time.Hour), "suspended_at", s.rel(0))
	s.sp.mu.Lock()
	s.sp.drop("s-gc-1")
	s.sp.mu.Unlock()
	for range 4 {
		s.advance(simPatrol)
		s.settle()
		if r := s.gc1()[session.DrainIntentReasonKey]; r != "" {
			t.Fatalf("a drain began on the dormant seat: %v", s.gc1())
		}
	}
	s.operator("gc-1", "state", "active", "state_reason", "creation_complete", "sleep_intent", "", "held_until", "", "suspended_at", "")
	s.sp.mu.Lock()
	s.sp.put("s-gc-1", "gc-1", "1", "tok-1")
	s.sp.mu.Unlock()
	for i := range 6 {
		s.advance(simPatrol)
		s.settle()
		if r := s.gc1()[session.DrainIntentReasonKey]; r != "" {
			t.Fatalf("pass %d after the operator's resume: drain %q begun from the request posted before the suspend: row %v", i+1, r, s.gc1())
		}
	}
	s.noViolations(t)
}

// stalledFixture is a planner holding gc-1's request at generation 1, posted
// at posted, and the pass reads over row with runtimes listed.
func stalledFixture(t *testing.T, row beads.Bead, posted time.Time, listed ...string) (*planner, func() map[string]executionStalledRequest) {
	t.Helper()
	f := newAllocFixture(t, &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}).sessions(row)
	for _, name := range listed {
		f.alive(name, InventoryAttrs{AttachedKnown: true})
	}
	in := f.inputs()
	p := newPlanner(newFakePlannerClock(plannerT0), func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, nil)
	p.postExecutionStalled(executionStalledRequest{ID: "gc-1", Generation: "1", At: posted})
	return p, func() map[string]executionStalledRequest {
		return p.executionStalled(in.Census, in.Obs, allocNow, observeMaxAge)
	}
}

// Kills each way a request is spent, or kept, at the pass's read: a live
// seat at the request's generation keeps it; the execution-stalled stop
// request on the row (a begin that landed whatever its settlement said, or a
// substituted one), a dormant seat, a runtime gone, and 30 minutes since the
// post each spend it; a seat at another generation does not spend it by its
// state.
func TestExecutionStalledRequestSpentAtThePassRead(t *testing.T) {
	stalled := []string{session.DrainIntentReasonKey, executionStalledDrainReason, session.DrainIntentAtKey, ago(time.Second), session.DrainIntentIncarnationKey, "1"}
	for _, tc := range []struct {
		name      string
		row       beads.Bead
		posted    time.Duration // before allocNow
		listed    bool
		wantSpent bool
	}{
		{name: "live seat at its generation", row: poolRow("gc-1", "worker", 1, "active"), listed: true},
		{name: "the execution-stalled stop request on the row", row: poolRow("gc-1", "worker", 1, "active", stalled...), listed: true, wantSpent: true},
		{name: "another reason's stop request", row: poolRow("gc-1", "worker", 1, "active", session.DrainIntentReasonKey, "idle", session.DrainIntentAtKey, ago(time.Second), session.DrainIntentIncarnationKey, "1"), listed: true},
		{name: "a dormant seat", row: poolRow("gc-1", "worker", 1, "suspended"), listed: true, wantSpent: true},
		{name: "its runtime gone", row: poolRow("gc-1", "worker", 1, "active"), wantSpent: true},
		{name: "29 minutes after the post", row: poolRow("gc-1", "worker", 1, "active"), posted: 29 * time.Minute, listed: true},
		{name: "30 minutes after the post", row: poolRow("gc-1", "worker", 1, "active"), posted: 30 * time.Minute, listed: true, wantSpent: true},
		{name: "a dormant seat at another generation", row: poolRow("gc-1", "worker", 1, "suspended", "generation", "2"), listed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var listed []string
			if tc.listed {
				listed = []string{"s-gc-1"}
			}
			_, read := stalledFixture(t, tc.row, allocNow.Add(-tc.posted-time.Second), listed...) // posted before the pass started
			if spent := len(read()) == 0; spent != tc.wantSpent {
				t.Fatalf("spent = %v, want %v", spent, tc.wantSpent)
			}
		})
	}
}

// Kills the request spent by a drain begin that did not land: a refused or
// failed begin keeps it for the next pass, and a landed begin of another
// reason does not spend it. A landed begin spends it, a late settlement
// included (the executor abandoned it, then it returned landed).
func TestExecutionStalledRequestSpentOnlyByItsLandedBegin(t *testing.T) {
	stalled := decideDrainBegin + executionStalledDrainReason
	for _, tc := range []struct {
		name      string
		reason    string
		outcome   settleOutcome
		late      bool
		wantSpent bool
	}{
		{"landed", stalled, settledLanded, false, true},
		{"landed late", stalled, settledLanded, true, true},
		{"refused", stalled, settledRefused, false, false},
		{"failed", stalled, settledFailed, false, false},
		{"ambiguous", stalled, settledAmbiguous, false, false},
		{"another reason's landed begin", decideDrainBegin + "idle", settledLanded, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, read := stalledFixture(t, poolRow("gc-1", "worker", 1, "active"), allocNow.Add(-time.Second), "s-gc-1")
			if got := read(); len(got) != 1 {
				t.Fatalf("before the begin: %v, want gc-1's request", got)
			}
			p.settlements.post(settlement{Key: rowKeyOf("gc-1"), Kind: intentDrainBegin, Reason: tc.reason, Outcome: tc.outcome, Late: tc.late})
			p.drainSettlements(plannerT0)
			if spent := len(read()) == 0; spent != tc.wantSpent {
				t.Fatalf("spent = %v, want %v", spent, tc.wantSpent)
			}
		})
	}
}

// Kills an ambiguous begin's request kept after its write landed: the
// settlement keeps it, and the next pass's census, showing the
// execution-stalled stop request at its generation, spends it.
func TestExecutionStalledAmbiguousBeginSpentByTheCensus(t *testing.T) {
	stalled := []string{session.DrainIntentReasonKey, executionStalledDrainReason, session.DrainIntentAtKey, ago(time.Second), session.DrainIntentIncarnationKey, "1"}
	p, before := stalledFixture(t, poolRow("gc-1", "worker", 1, "active"), allocNow.Add(-time.Second), "s-gc-1")
	if got := before(); len(got) != 1 {
		t.Fatalf("before the begin: %v", got)
	}
	p.settlements.post(settlement{Key: rowKeyOf("gc-1"), Kind: intentDrainBegin, Reason: decideDrainBegin + executionStalledDrainReason, Outcome: settledAmbiguous})
	p.drainSettlements(plannerT0)
	if got := before(); len(got) != 1 {
		t.Fatalf("after the ambiguous settlement: %v, want the request kept", got)
	}
	f := newAllocFixture(t, &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}).sessions(poolRow("gc-1", "worker", 1, "active", stalled...))
	f.alive("s-gc-1", InventoryAttrs{AttachedKnown: true})
	in := f.inputs()
	if got := p.executionStalled(in.Census, in.Obs, allocNow, observeMaxAge); len(got) != 0 {
		t.Fatalf("after the landed write's census: %v, want the request spent", got)
	}
}

// stalledReadOn reports whether a pass over a live gc-1 at generation 1,
// observed through snap (the fixture's own when nil), spends a request
// posted at posted.
func stalledReadOn(t *testing.T, posted time.Time, snap func(*allocFixture) *ObservationSnapshot) bool {
	t.Helper()
	f := newAllocFixture(t, &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}).sessions(poolRow("gc-1", "worker", 1, "active"))
	in := f.inputs()
	obs := in.Obs
	if snap != nil {
		obs = snap(f)
	}
	p := newPlanner(newFakePlannerClock(plannerT0), func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, nil)
	p.postExecutionStalled(executionStalledRequest{ID: "gc-1", Generation: "1", At: posted})
	return len(p.executionStalled(in.Census, obs, allocNow, observeMaxAge)) == 0
}

func ownStalledAttrs() map[string]InventoryAttrs {
	return map[string]InventoryAttrs{"s-gc-1": {DeadKnown: true, AttachedKnown: true, Identity: readIdentity("gc-1")}}
}

// Kills a request spent by a runtime read that does not prove the runtime
// gone (the #7658 r4 review's table): only a fresh complete pass without the
// name, or a corpse, spends it. A partial, failed or unattested listing, a
// stale or missing pass, presence carried from an earlier pass or a probe, a
// zombie, and a live runtime keep it.
func TestExecutionStalledRuntimeSpendNeedsAConfirmedRead(t *testing.T) {
	posted := allocNow.Add(-time.Second) // before the pass at allocNow started
	for _, tc := range []struct {
		name      string
		snap      func(*allocFixture) *ObservationSnapshot
		wantSpent bool
	}{
		{"complete pass without the name (gone)", func(*allocFixture) *ObservationSnapshot {
			return newObserveCache().publish(allocNow, nil, completeBackend("tmux"))
		}, true},
		{"partial pass without the name", func(*allocFixture) *ObservationSnapshot {
			return newObserveCache().publish(allocNow, nil, partialSingle())
		}, false},
		{"failed pass", func(*allocFixture) *ObservationSnapshot {
			return newObserveCache().publish(allocNow, nil, failedSingle())
		}, false},
		{"unattested backend without the name", func(*allocFixture) *ObservationSnapshot {
			return newObserveCache().publish(allocNow, nil, unattestedBackend("exec"))
		}, false},
		{"complete tmux, partial acp", func(*allocFixture) *ObservationSnapshot {
			b := partialSingle()
			b.Label = "acp"
			return newObserveCache().publish(allocNow, nil, completeBackend("tmux"), b)
		}, false},
		{"stale complete pass without the name", func(*allocFixture) *ObservationSnapshot {
			return newObserveCache().publish(allocNow.Add(-3*observeMaxAge), nil, completeBackend("tmux"))
		}, false},
		{"no pass at all", func(*allocFixture) *ObservationSnapshot {
			return newObserveCache().Snapshot()
		}, false},
		{"listed by an earlier pass, the latest partial without it", func(*allocFixture) *ObservationSnapshot {
			c := newObserveCache()
			c.publish(allocNow.Add(-20*time.Second), ownStalledAttrs(), completeBackend("tmux", "s-gc-1"))
			return c.publish(allocNow, nil, partialSingle())
		}, false},
		{"a probe listed it after a complete pass without it", func(*allocFixture) *ObservationSnapshot {
			c := newObserveCache()
			c.publish(allocNow, nil, completeBackend("tmux"))
			c.Note("s-gc-1", FactListed, ObsYes, allocNow.Add(time.Second), SourceProbe, "")
			return c.Snapshot()
		}, false},
		{"a corpse (pane dead)", func(f *allocFixture) *ObservationSnapshot {
			f.corpse("s-gc-1")
			return f.observation()
		}, true},
		{"a zombie (pane alive, agent dead)", func(f *allocFixture) *ObservationSnapshot {
			f.alive("s-gc-1", InventoryAttrs{AttachedKnown: true}).fact("s-gc-1", FactProcessAlive, ObsNo)
			return f.observation()
		}, false},
		{"alive", func(f *allocFixture) *ObservationSnapshot {
			f.alive("s-gc-1", InventoryAttrs{AttachedKnown: true})
			return f.observation()
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if spent := stalledReadOn(t, posted, tc.snap); spent != tc.wantSpent {
				t.Fatalf("spent = %v, want %v", spent, tc.wantSpent)
			}
		})
	}
}

// Kills absence or a corpse read on a pass that started before the post
// spending the request (the r4 hardening ruling): that listing may predate
// the runtime the backstop judged stalled. A pass started after the post
// spends it.
func TestExecutionStalledRuntimeSpendNeedsAPassAfterThePost(t *testing.T) {
	gone := func(*allocFixture) *ObservationSnapshot {
		return newObserveCache().publish(allocNow.Add(-10*time.Second), nil, completeBackend("tmux"))
	}
	corpse := func(*allocFixture) *ObservationSnapshot {
		attrs := ownStalledAttrs()
		a := attrs["s-gc-1"]
		a.AllPanesDead = true
		attrs["s-gc-1"] = a
		return newObserveCache().publish(allocNow.Add(-10*time.Second), attrs, completeBackend("tmux", "s-gc-1"))
	}
	for _, tc := range []struct {
		name      string
		snap      func(*allocFixture) *ObservationSnapshot
		posted    time.Duration // before allocNow; the pass started 10s before it
		wantSpent bool
	}{
		{"gone, on a pass started before the post", gone, 5 * time.Second, false},
		{"gone, on a pass started at the post", gone, 10 * time.Second, false},
		{"gone, on a pass started after the post", gone, 15 * time.Second, true},
		{"a corpse, on a pass started before the post", corpse, 5 * time.Second, false},
		{"a corpse, on a pass started after the post", corpse, 15 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if spent := stalledReadOn(t, allocNow.Add(-tc.posted), tc.snap); spent != tc.wantSpent {
				t.Fatalf("spent = %v, want %v", spent, tc.wantSpent)
			}
		})
	}
}

// Kills a request spent end to end by bad listings: a partial or failed
// listing, or a tmux server that is unreachable but not confirmed dead, for
// several passes spends no request, and A16 begins once listings recover,
// with no new post (the r4 review's sim probe).
func TestSimExecutionStalledBadListingsKeepTheRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  func(p *simProvider)
	}{
		{"partial listing", func(p *simProvider) { p.listing = 1 }},
		{"failed listing", func(p *simProvider) { p.listing = 2 }},
		{"server unreachable, not confirmed dead", func(p *simProvider) { p.serverDown, p.confirmable = true, false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1"), gc1Work())
			s.settle()
			s.sp.mu.Lock()
			tc.bad(s.sp)
			s.sp.mu.Unlock()
			s.p.postExecutionStalled(executionStalledRequest{ID: "gc-1", Generation: "1", At: s.clk.Now()})
			for range 5 {
				s.advance(simPatrol)
				s.settle()
				if s.gc1()[session.DrainIntentReasonKey] == executionStalledDrainReason {
					break // A16 may act on presence carried from the last fresh pass
				}
			}
			s.sp.mu.Lock()
			s.sp.listing, s.sp.serverDown = 0, false
			s.sp.mu.Unlock()
			for range 4 {
				if s.gc1()[session.DrainIntentReasonKey] == executionStalledDrainReason {
					return
				}
				s.advance(simPatrol)
				s.settle()
			}
			t.Errorf("row %v: the stall request was spent by the bad listings, want A16's begin once they recover", s.gc1())
		})
	}
}

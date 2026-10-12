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
			_, read := stalledFixture(t, tc.row, allocNow.Add(-tc.posted), listed...)
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
			p, read := stalledFixture(t, poolRow("gc-1", "worker", 1, "active"), allocNow, "s-gc-1")
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
	p, before := stalledFixture(t, poolRow("gc-1", "worker", 1, "active"), allocNow, "s-gc-1")
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

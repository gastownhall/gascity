package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// Kills A16's stall memory outliving its drain (mc-uinl0): the planner held
// an execution-stalled request until the row closed, so an operator who
// resumed the row (clearing its stop request at the same generation) had it
// drained again within a patrol. Legacy's latch suppresses the re-drain for
// 30 minutes per instance token (stalledLatchHolds). The request is one-shot:
// it goes once its drain begin lands, and only a new request (the backstop's
// re-trigger, which its latch holds for 30 minutes) drains the row again.
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

// Kills the request spent by a drain begin that did not land: a refused or
// failed begin keeps it for the next pass, and a landed begin of another
// reason does not spend it.
func TestExecutionStalledRequestSpentOnlyByItsLandedBegin(t *testing.T) {
	census := &sessionCensus{Legs: []censusLeg{{Ref: "sessions"}}, Rows: map[rowKey]censusRow{
		{Leg: "sessions", ID: "gc-1"}: {Key: rowKey{Leg: "sessions", ID: "gc-1"}},
	}}
	stalled := decideDrainBegin + executionStalledDrainReason
	for _, tc := range []struct {
		name      string
		reason    string
		outcome   settleOutcome
		wantSpent bool
	}{
		{"landed", stalled, settledLanded, true},
		{"refused", stalled, settledRefused, false},
		{"failed", stalled, settledFailed, false},
		{"another reason's landed begin", decideDrainBegin + "idle", settledLanded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanner(newFakePlannerClock(plannerT0), func() time.Duration { return time.Minute }, nil, newInflightMap(), nil, nil)
			p.postExecutionStalled(executionStalledRequest{ID: "gc-1", Generation: "1"})
			if got := p.executionStalled(census); len(got) != 1 {
				t.Fatalf("before the begin: %v, want gc-1's request", got)
			}
			p.settlements.post(settlement{Key: rowKey{Leg: "sessions", ID: "gc-1"}, Kind: intentDrainBegin, Reason: tc.reason, Outcome: tc.outcome})
			p.drainSettlements(plannerT0)
			if spent := len(p.executionStalled(census)) == 0; spent != tc.wantSpent {
				t.Fatalf("spent = %v, want %v", spent, tc.wantSpent)
			}
		})
	}
}

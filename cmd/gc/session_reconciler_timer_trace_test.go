package main

import (
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestTimerTraceCodesTotal drives every reachable TimerDecision from
// DecideMaxSessionAge and DecideIdleTimeout (all TimerFacts combinations,
// including both blocker kinds) and asserts that timerTraceCodes (a) maps each
// traced reason/outcome onto a NAMED constant — never falling through to the
// identity default arm — and (b) round-trips to the exact producer strings.
// When the timer ladders grow a new traced value, this test goes red instead
// of silently un-typing the vocabulary.
func TestTimerTraceCodesTotal(t *testing.T) {
	namedReasons := map[TraceReasonCode]bool{
		TraceReasonMaxSessionAge: true,
		TraceReasonIdleTimeout:   true,
		TraceReasonUserHold:      true,
		TraceReasonQuarantine:    true,
		TraceReasonPending:       true,
		TraceReasonAssignedWork:  true,
	}
	namedOutcomes := map[TraceOutcomeCode]bool{
		TraceOutcomeStop:               true,
		TraceOutcomeDeferredUserHold:   true,
		TraceOutcomeDeferredQuarantine: true,
		TraceOutcomeDeferredPending:    true,
		TraceOutcomeDeferredBusy:       true,
	}

	blockers := []string{"", "user_hold", "quarantine"}
	pendings := []sessionpkg.PendingFact{
		sessionpkg.PendingUnknown, sessionpkg.PendingNo, sessionpkg.PendingYes,
	}
	assigned := []sessionpkg.AssignedWorkFact{
		sessionpkg.AssignedWorkUnknown, sessionpkg.AssignedWorkNone, sessionpkg.AssignedWorkHas,
	}

	var decisions []sessionpkg.TimerDecision
	for _, b := range blockers {
		for _, p := range pendings {
			for _, a := range assigned {
				facts := sessionpkg.TimerFacts{Triggered: true, Blocker: b, Pending: p, AssignedWork: a}
				decisions = append(decisions, sessionpkg.DecideMaxSessionAge(facts))
				decisions = append(decisions, sessionpkg.DecideIdleTimeout(facts))
			}
		}
	}

	sawTraced := false
	for _, dec := range decisions {
		// Only Defer/Stop decisions carry trace codes and reach a
		// RecordDecision call site; gather/none actions leave them empty.
		if dec.Action != sessionpkg.TimerActionDefer && dec.Action != sessionpkg.TimerActionStop {
			continue
		}
		sawTraced = true
		reason, outcome := timerTraceCodes(dec)
		if string(reason) != dec.TraceReason {
			t.Errorf("reason round-trip: got %q, want %q", string(reason), dec.TraceReason)
		}
		if string(outcome) != dec.TraceOutcome {
			t.Errorf("outcome round-trip: got %q, want %q", string(outcome), dec.TraceOutcome)
		}
		if !namedReasons[reason] {
			t.Errorf("reason %q fell through to the identity default arm (unnamed vocabulary)", string(reason))
		}
		if !namedOutcomes[outcome] {
			t.Errorf("outcome %q fell through to the identity default arm (unnamed vocabulary)", string(outcome))
		}
	}
	if !sawTraced {
		t.Fatal("no traced TimerDecision exercised — enumeration is broken")
	}
}

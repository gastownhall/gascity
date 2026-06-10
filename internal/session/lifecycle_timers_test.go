package session

import "testing"

// These tests pin the decision ladders extracted from the reconciler's
// max-session-age and idle-timeout blocks. The precedence is a contract:
// timer blocker beats pending interaction beats assigned work beats stop.
// The caller-facing characterization tests for the same behavior live in
// cmd/gc/session_reconciler_test.go (SESSION-RECON-008, SESSION-RECON-009).

func TestDecideMaxSessionAgeNotTriggered(t *testing.T) {
	dec := DecideMaxSessionAge(TimerFacts{Triggered: false})
	if dec.Action != TimerActionNone {
		t.Fatalf("expected no action, got %v", dec.Action)
	}
}

func TestDecideMaxSessionAgeLadder(t *testing.T) {
	cases := []struct {
		name    string
		facts   TimerFacts
		action  TimerAction
		reason  string
		outcome string
	}{
		{
			name:    "user hold blocks before anything else",
			facts:   TimerFacts{Triggered: true, Blocker: "user_hold", Pending: PendingYes, AssignedWork: AssignedWorkHas},
			action:  TimerActionDefer,
			reason:  "user_hold",
			outcome: "deferred_user_hold",
		},
		{
			name:    "quarantine blocks before anything else",
			facts:   TimerFacts{Triggered: true, Blocker: "quarantine"},
			action:  TimerActionDefer,
			reason:  "quarantine",
			outcome: "deferred_quarantine",
		},
		{
			name:   "unknown pending interaction must be gathered",
			facts:  TimerFacts{Triggered: true},
			action: TimerActionGatherPending,
		},
		{
			name:    "pending interaction defers before work check",
			facts:   TimerFacts{Triggered: true, Pending: PendingYes, AssignedWork: AssignedWorkUnknown},
			action:  TimerActionDefer,
			reason:  "pending",
			outcome: "deferred_pending",
		},
		{
			name:   "unknown assigned work must be gathered",
			facts:  TimerFacts{Triggered: true, Pending: PendingNo},
			action: TimerActionGatherAssignedWork,
		},
		{
			name:    "assigned work defers the restart",
			facts:   TimerFacts{Triggered: true, Pending: PendingNo, AssignedWork: AssignedWorkHas},
			action:  TimerActionDefer,
			reason:  "assigned_work",
			outcome: "deferred_busy",
		},
		{
			name:    "free session stops",
			facts:   TimerFacts{Triggered: true, Pending: PendingNo, AssignedWork: AssignedWorkNone},
			action:  TimerActionStop,
			reason:  "max_session_age",
			outcome: "stop",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := DecideMaxSessionAge(tc.facts)
			if dec.Action != tc.action {
				t.Fatalf("action = %v, want %v", dec.Action, tc.action)
			}
			if dec.TraceReason != tc.reason {
				t.Errorf("trace reason = %q, want %q", dec.TraceReason, tc.reason)
			}
			if dec.TraceOutcome != tc.outcome {
				t.Errorf("trace outcome = %q, want %q", dec.TraceOutcome, tc.outcome)
			}
		})
	}
}

func TestDecideMaxSessionAgeStopSleepReason(t *testing.T) {
	dec := DecideMaxSessionAge(TimerFacts{Triggered: true, Pending: PendingNo, AssignedWork: AssignedWorkNone})
	if dec.SleepReason != "max-session-age" {
		t.Fatalf("sleep reason = %q, want %q", dec.SleepReason, "max-session-age")
	}
	if dec.CancelDrain || dec.SkipWakePass {
		t.Fatalf("max-age stop must not request drain cancel or wake-pass skip: %+v", dec)
	}
}

func TestDecideIdleTimeoutNotTriggered(t *testing.T) {
	dec := DecideIdleTimeout(TimerFacts{Triggered: false})
	if dec.Action != TimerActionNone {
		t.Fatalf("expected no action, got %v", dec.Action)
	}
}

func TestDecideIdleTimeoutLadder(t *testing.T) {
	cases := []struct {
		name    string
		facts   TimerFacts
		action  TimerAction
		reason  string
		outcome string
	}{
		{
			name:    "user hold blocks",
			facts:   TimerFacts{Triggered: true, Blocker: "user_hold", Pending: PendingYes},
			action:  TimerActionDefer,
			reason:  "user_hold",
			outcome: "deferred_user_hold",
		},
		{
			name:    "quarantine blocks",
			facts:   TimerFacts{Triggered: true, Blocker: "quarantine"},
			action:  TimerActionDefer,
			reason:  "quarantine",
			outcome: "deferred_quarantine",
		},
		{
			name:   "unknown pending interaction must be gathered",
			facts:  TimerFacts{Triggered: true},
			action: TimerActionGatherPending,
		},
		{
			name:    "idle session stops",
			facts:   TimerFacts{Triggered: true, Pending: PendingNo},
			action:  TimerActionStop,
			reason:  "idle_timeout",
			outcome: "stop",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := DecideIdleTimeout(tc.facts)
			if dec.Action != tc.action {
				t.Fatalf("action = %v, want %v", dec.Action, tc.action)
			}
			if dec.TraceReason != tc.reason {
				t.Errorf("trace reason = %q, want %q", dec.TraceReason, tc.reason)
			}
			if dec.TraceOutcome != tc.outcome {
				t.Errorf("trace outcome = %q, want %q", dec.TraceOutcome, tc.outcome)
			}
		})
	}
}

func TestDecideIdleTimeoutPendingCancelsDrainAndSkipsWakePass(t *testing.T) {
	dec := DecideIdleTimeout(TimerFacts{Triggered: true, Pending: PendingYes})
	if dec.Action != TimerActionDefer {
		t.Fatalf("action = %v, want defer", dec.Action)
	}
	if dec.TraceReason != "pending" || dec.TraceOutcome != "deferred_pending" {
		t.Fatalf("trace = %q/%q, want pending/deferred_pending", dec.TraceReason, dec.TraceOutcome)
	}
	if !dec.CancelDrain {
		t.Error("idle pending interaction must cancel a pending drain")
	}
	if !dec.SkipWakePass {
		t.Error("idle pending interaction must skip the wake pass for this session")
	}
}

// Max-age pending interaction does NOT cancel drains or skip the wake pass —
// that asymmetry with idle-timeout is existing reconciler behavior.
func TestDecideMaxSessionAgePendingKeepsWakePass(t *testing.T) {
	dec := DecideMaxSessionAge(TimerFacts{Triggered: true, Pending: PendingYes})
	if dec.Action != TimerActionDefer {
		t.Fatalf("action = %v, want defer", dec.Action)
	}
	if dec.CancelDrain || dec.SkipWakePass {
		t.Fatalf("max-age pending must not cancel drain or skip wake pass: %+v", dec)
	}
}

// Idle-timeout never consults assigned work; an unknown work fact must not
// trigger a gather action or change the stop decision.
func TestDecideIdleTimeoutIgnoresAssignedWork(t *testing.T) {
	dec := DecideIdleTimeout(TimerFacts{Triggered: true, Pending: PendingNo, AssignedWork: AssignedWorkUnknown})
	if dec.Action != TimerActionStop {
		t.Fatalf("action = %v, want stop", dec.Action)
	}
	if dec.SleepReason != "idle-timeout" {
		t.Fatalf("sleep reason = %q, want %q", dec.SleepReason, "idle-timeout")
	}
}

// The gather loop must terminate: once both gatherable facts are known the
// decider may only defer or stop.
func TestTimerDecisionsTerminate(t *testing.T) {
	pendings := []PendingFact{PendingNo, PendingYes}
	works := []AssignedWorkFact{AssignedWorkNone, AssignedWorkHas}
	blockers := []string{"", "user_hold", "quarantine"}
	for _, b := range blockers {
		for _, p := range pendings {
			for _, w := range works {
				facts := TimerFacts{Triggered: true, Blocker: b, Pending: p, AssignedWork: w}
				for _, dec := range []TimerDecision{DecideMaxSessionAge(facts), DecideIdleTimeout(facts)} {
					switch dec.Action {
					case TimerActionDefer, TimerActionStop:
					default:
						t.Fatalf("facts %+v produced non-terminal action %v", facts, dec.Action)
					}
				}
			}
		}
	}
}

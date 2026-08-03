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
			if dec.CancelDrain || dec.SkipWakePass {
				t.Errorf("max-age decisions never cancel drains or skip the wake pass: %+v", dec)
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
			name:   "unknown assigned work must be gathered",
			facts:  TimerFacts{Triggered: true, Pending: PendingNo},
			action: TimerActionGatherAssignedWork,
		},
		{
			name:    "assigned work defers the stop",
			facts:   TimerFacts{Triggered: true, Pending: PendingNo, AssignedWork: AssignedWorkHas},
			action:  TimerActionDefer,
			reason:  "assigned_work",
			outcome: "deferred_busy",
		},
		{
			name:    "free idle session stops",
			facts:   TimerFacts{Triggered: true, Pending: PendingNo, AssignedWork: AssignedWorkNone},
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
			if dec.CancelDrain || dec.SkipWakePass {
				t.Errorf("only pending-interaction deferrals cancel drains or skip the wake pass: %+v", dec)
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

func TestDecideIdleTimeoutStopSleepReason(t *testing.T) {
	dec := DecideIdleTimeout(TimerFacts{Triggered: true, Pending: PendingNo, AssignedWork: AssignedWorkNone})
	if dec.SleepReason != "idle-timeout" {
		t.Fatalf("sleep reason = %q, want %q", dec.SleepReason, "idle-timeout")
	}
	if dec.CancelDrain || dec.SkipWakePass {
		t.Fatalf("idle stop must not request drain cancel or wake-pass skip: %+v", dec)
	}
}

// Assigned work defers the idle-timeout stop the same way it defers
// max-session-age, so ComputeAwakeSet's assigned-work exemption and the
// idle-kill ladder agree instead of fighting (ga-3ox7rk).
func TestDecideIdleTimeoutDefersOnAssignedWork(t *testing.T) {
	dec := DecideIdleTimeout(TimerFacts{Triggered: true, Pending: PendingNo, AssignedWork: AssignedWorkHas})
	if dec.Action != TimerActionDefer {
		t.Fatalf("action = %v, want defer", dec.Action)
	}
	if dec.TraceReason != "assigned_work" || dec.TraceOutcome != "deferred_busy" {
		t.Fatalf("trace = %q/%q, want assigned_work/deferred_busy", dec.TraceReason, dec.TraceOutcome)
	}
	if dec.CancelDrain || dec.SkipWakePass {
		t.Fatalf("assigned-work deferral must not cancel drain or skip wake pass: %+v", dec)
	}
}

// DecideAssignedWorkExhausted is the caller-invoked override for a session
// that has deferred the idle-timeout stop on the same assigned-work bead more
// times than the reconciler's configured consecutive-defer limit. Unlike a
// plain idle-timeout stop it carries its own trace reason and sleep reason so
// the override is distinguishable in traces and metadata (ga-nllza6 part 2).
func TestDecideAssignedWorkExhausted(t *testing.T) {
	dec := DecideAssignedWorkExhausted()
	if dec.Action != TimerActionStop {
		t.Fatalf("action = %v, want %v", dec.Action, TimerActionStop)
	}
	if dec.TraceReason != "assigned_work_exhausted" || dec.TraceOutcome != "stop_defer_exhausted" {
		t.Fatalf("trace = %q/%q, want assigned_work_exhausted/stop_defer_exhausted", dec.TraceReason, dec.TraceOutcome)
	}
	if dec.SleepReason != string(SleepReasonAssignedWorkExhausted) {
		t.Fatalf("sleep reason = %q, want %q", dec.SleepReason, SleepReasonAssignedWorkExhausted)
	}
	if dec.CancelDrain || dec.SkipWakePass {
		t.Fatalf("defer-exhausted stop must not request drain cancel or wake-pass skip: %+v", dec)
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

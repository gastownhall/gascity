package session

// Lifecycle-timer deciders for the session reconciler's max-session-age and
// idle-timeout policies. These are pure decision ladders over caller-gathered
// facts: the reconciler owns the trackers, provider probes, store queries,
// and side effects; this package owns precedence and the decision vocabulary.
//
// Expensive facts are gathered on demand. A decider that needs a fact the
// caller has not supplied returns a gather action naming it; the caller
// fills the fact in and decides again. That keeps fact-gathering order, cost,
// and fail-open/fail-closed error mapping with the caller while the ladder
// itself stays in one testable place.

// PendingFact is the tri-state pending-interaction fact. It is Unknown until
// the caller has probed the runtime provider for an in-flight user turn.
type PendingFact int

// Pending-interaction fact states.
const (
	PendingUnknown PendingFact = iota
	PendingNo
	PendingYes
)

// AssignedWorkFact is the tri-state open-assigned-work fact. It is Unknown
// until the caller has queried the reachable stores. Callers map store errors
// to AssignedWorkHas (fail closed) so a transient blip cannot stop a session
// that may still hold in-flight work.
type AssignedWorkFact int

// Assigned-work fact states.
const (
	AssignedWorkUnknown AssignedWorkFact = iota
	AssignedWorkNone
	AssignedWorkHas
)

// TimerAction is what the caller must do next for one session and one timer.
type TimerAction int

const (
	// TimerActionNone means the timer did not trigger; nothing to do.
	TimerActionNone TimerAction = iota
	// TimerActionGatherPending means supply TimerFacts.Pending and decide
	// again.
	TimerActionGatherPending
	// TimerActionGatherAssignedWork means supply TimerFacts.AssignedWork and
	// decide again.
	TimerActionGatherAssignedWork
	// TimerActionDefer means leave the session alone this tick and record
	// the decision trace.
	TimerActionDefer
	// TimerActionStop means stop the session runtime and apply the sleep
	// patch with the decision's SleepReason.
	TimerActionStop
)

// TimerFacts are the inputs for one session's evaluation of one lifecycle
// timer on one reconciler tick.
type TimerFacts struct {
	// Triggered reports whether the timer's tracker fired (threshold elapsed
	// with a valid anchor). When false no other fact is consulted.
	Triggered bool
	// Blocker is the active lifecycle timer blocker as reported by the
	// caller (currently "user_hold" or "quarantine"), or empty when none
	// applies. Any non-empty value defers the timer.
	Blocker string
	// Pending is the pending-interaction fact, gathered on demand.
	Pending PendingFact
	// AssignedWork is the open-assigned-work fact, gathered on demand.
	// Only the max-session-age ladder consults it.
	AssignedWork AssignedWorkFact
}

// TimerDecision is the outcome of one ladder evaluation.
type TimerDecision struct {
	// Action is what the caller must do next.
	Action TimerAction
	// TraceReason and TraceOutcome are the stable vocabulary for the
	// reconciler.session.max_session_age and reconciler.session.idle_timeout
	// trace sites. Empty for gather actions and TimerActionNone.
	TraceReason  string
	TraceOutcome string
	// SleepReason is the sleep_reason recorded by SleepPatch when Action is
	// TimerActionStop.
	SleepReason string
	// CancelDrain reports that a pending drain for this session must be
	// canceled (idle-timeout pending-interaction only).
	CancelDrain bool
	// SkipWakePass reports that the session must not enter this tick's wake
	// evaluation (idle-timeout pending-interaction only).
	SkipWakePass bool
}

// DecideMaxSessionAge evaluates the preemptive max-session-age ladder:
// blocker, then pending interaction, then assigned work, then stop. A busy
// session is still subject to the age threshold, but the restart is deferred
// while the agent is mid-turn or holds open assigned work; the next tick
// retries.
func DecideMaxSessionAge(f TimerFacts) TimerDecision {
	if !f.Triggered {
		return TimerDecision{Action: TimerActionNone}
	}
	if f.Blocker != "" {
		return deferDecision(f.Blocker, "deferred_"+f.Blocker)
	}
	switch f.Pending {
	case PendingUnknown:
		return TimerDecision{Action: TimerActionGatherPending}
	case PendingYes:
		return deferDecision("pending", "deferred_pending")
	}
	switch f.AssignedWork {
	case AssignedWorkUnknown:
		return TimerDecision{Action: TimerActionGatherAssignedWork}
	case AssignedWorkHas:
		return deferDecision("assigned_work", "deferred_busy")
	}
	return TimerDecision{
		Action:       TimerActionStop,
		TraceReason:  "max_session_age",
		TraceOutcome: "stop",
		SleepReason:  "max-session-age",
	}
}

// DecideIdleTimeout evaluates the idle-timeout ladder: blocker, then pending
// interaction, then stop. Idle stops never consult assigned work. A pending
// interaction cancels any pending drain and keeps the session out of this
// tick's wake pass — asymmetries with max-session-age that are part of the
// existing reconciler contract.
func DecideIdleTimeout(f TimerFacts) TimerDecision {
	if !f.Triggered {
		return TimerDecision{Action: TimerActionNone}
	}
	if f.Blocker != "" {
		return deferDecision(f.Blocker, "deferred_"+f.Blocker)
	}
	switch f.Pending {
	case PendingUnknown:
		return TimerDecision{Action: TimerActionGatherPending}
	case PendingYes:
		dec := deferDecision("pending", "deferred_pending")
		dec.CancelDrain = true
		dec.SkipWakePass = true
		return dec
	}
	return TimerDecision{
		Action:       TimerActionStop,
		TraceReason:  "idle_timeout",
		TraceOutcome: "stop",
		SleepReason:  "idle-timeout",
	}
}

func deferDecision(reason, outcome string) TimerDecision {
	return TimerDecision{Action: TimerActionDefer, TraceReason: reason, TraceOutcome: outcome}
}

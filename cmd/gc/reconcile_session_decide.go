package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// The session key's decide (P4 spec §3.3): a pure function of one row's
// inputs. It is two-phase: decideSession may ask for probes, which the
// controller runs (bounded, at most twice per reconcile) before deciding
// again with their answers. It performs at most one action (C0.2); metadata
// folds the action depends on ride in the same write.
//
// The arms run in legacy order. This slice carries arms 0a-0d, 1 and 4; later
// slices insert theirs at their legacy positions. Every arm that depends on
// desire goes after the desire gate: a row with no entry, or with no snapshot
// yet (before S_1), takes row-local actions only (C2.9, C4.5 item 4), and an
// entry decided for an older incarnation defers them (§4.3 rule 1).

// probeKinds is a set of probes decide asks the controller to run on the
// row's routed leaf.
type probeKinds uint8

const (
	probeAttach  probeKinds = 1 << iota // the attach error probe, reporter tier
	probePending                        // the pending-interaction probe
)

// probeAnswers carries every probe answered so far this reconcile.
type probeAnswers struct {
	Asked probeKinds
	// Attached is the reporter-tier probe's answer, AttachKnown false when
	// it erred, timed out or the leaf is not a reporter.
	Attached, AttachKnown bool
	// Pending is unknown until the pending probe answers (unaskedAnswers):
	// the type's zero value is No, which no probe said.
	Pending pendingInteractionAnswer
}

// unaskedAnswers is a reconcile's answers before any probe runs.
func unaskedAnswers() probeAnswers {
	return probeAnswers{Pending: pendingInteractionUnknown}
}

// sessionInputs is everything one decide reads. It holds data only.
type sessionInputs struct {
	Key rowKey
	// Row is the session row; Found is false when no session row exists.
	Row   session.Info
	Found bool
	Now   time.Time
	// Snap is the latest published selection snapshot (nil before S_1) and
	// Entry its entry for Key (nil when the row is unranked).
	Snap  *selectionSnapshot
	Entry *selectionEntry
	// Obs is the row's runtime as I3 reads it, by allocator semantics.
	Obs rowObservation
	// InFlight is set while the executor holds an effect for Key.
	InFlight         bool
	InFlightDeadline time.Time
}

type sessionActionKind uint8

const (
	actNone sessionActionKind = iota
	actWrite
)

// sessionAction is a decision's one action. A write persists Patch under the
// row lock, fenced on the lifecycle facts the decide read.
type sessionAction struct {
	Kind  sessionActionKind
	Patch session.MetadataPatch
	// Authorize, when set, makes the action desire-dependent (§4.3 rule 1):
	// it proceeds only if it still holds against the latest entry at the
	// commit point.
	Authorize func(latest *selectionEntry) bool
}

// sessionDecision is decide's output. Event is recorded once Action's write
// lands. Reason names the arm that decided, for the trace.
type sessionDecision struct {
	Action       sessionAction
	RequeueAfter time.Duration
	Reason       string
	Event        *events.Event
}

// Decide reasons.
const (
	decideNoRow           = "no-row"
	decideKillFence       = "kill-fence"
	decideEffectInFlight  = "effect-in-flight"
	decideUnknownState    = "unknown-state"
	decideLivenessUnknown = "liveness-unknown"
	decideUnranked        = "unranked"
	decideIncarnationLag  = "incarnation-lag"
	decideNoAction        = "no-action"
)

// decideSession decides one reconcile of in's row.
func decideSession(in sessionInputs, _ probeAnswers) (sessionDecision, probeKinds) {
	// 0a: nothing to reconcile.
	if !in.Found || in.Row.Closed {
		return sessionDecision{Reason: decideNoRow}, 0
	}
	// 0b: a `gc session kill` owns the row until it lifts its fence: no
	// heal, start or close. Look again when the fence ages out.
	if session.IsKillPendingInfo(in.Row, in.Now) {
		at, _ := time.Parse(time.RFC3339, strings.TrimSpace(in.Row.SleptAt))
		return sessionDecision{Reason: decideKillFence, RequeueAfter: requeueUntil(in.Now, at.Add(session.KillPendingGrace))}, 0
	}
	// 0c: an effect is in flight; its completion enqueues the key (C5.5).
	if in.InFlight {
		return sessionDecision{Reason: decideEffectInFlight, RequeueAfter: requeueUntil(in.Now, in.InFlightDeadline)}, 0
	}
	// 0d: a state this version does not know: diagnostics only.
	if !isKnownStateInfo(in.Row) {
		return decideUnknownStateRow(in), 0
	}
	// 1: expired timers and stale unknown-state markers fold into the write.
	patch, requeue := timerHealPatch(in.Row, in.Now)
	d := sessionDecision{RequeueAfter: requeue}
	if len(patch) > 0 {
		d.Action = sessionAction{Kind: actWrite, Patch: patch}
	}
	// 4: unknown liveness blocks every action past this point (GUAR-053).
	if in.Obs.Liveness == livenessUnknown {
		d.Reason = decideLivenessUnknown
		return d, 0
	}
	// The desire gate.
	switch {
	case in.Snap == nil || in.Entry == nil:
		d.Reason = decideUnranked
	case in.Entry.Basis.Incarnation != rowIncarnation(in.Row):
		d.Reason = decideIncarnationLag
	default:
		d.Reason = decideNoAction
	}
	return d, 0
}

// timerHealPatch clears an expired hold, then an expired quarantine judged
// against the post-hold sleep_reason (SESS-009/010: the order is
// load-bearing), and clears unknown-state markers on a known-state row
// (SESS-047). requeue is the time until the earliest timer still running.
func timerHealPatch(row session.Info, now time.Time) (session.MetadataPatch, time.Duration) {
	patch := session.MetadataPatch{}
	var requeue time.Duration
	sleepReason := row.SleepReason
	for _, timer := range []struct {
		at    string
		clear func(string) session.MetadataPatch
	}{
		{row.HeldUntil, session.ClearExpiredHoldPatch},
		{row.QuarantinedUntil, session.ClearExpiredQuarantinePatch},
	} {
		t, _ := time.Parse(time.RFC3339, timer.at)
		switch {
		case t.IsZero():
		case now.After(t):
			for k, v := range timer.clear(sleepReason) {
				patch[k] = v
			}
			if v, ok := patch["sleep_reason"]; ok {
				sleepReason = v
			}
		default:
			// RFC3339 has whole seconds; a second past the timer it has expired.
			requeue = earlierRequeue(requeue, t.Sub(now)+time.Second)
		}
	}
	for key, value := range map[string]string{
		unknownStateFirstSeenKey: row.UnknownStateFirstSeen,
		unknownStateValueKey:     row.UnknownStateValue,
		unknownStateEscalatedKey: row.UnknownStateEscalatedAt,
	} {
		if strings.TrimSpace(value) != "" {
			patch[key] = ""
		}
	}
	return patch, requeue
}

// decideUnknownStateRow is SESS-044..046: skip the row; on first sight of an
// unknown value (or a change to another one) stamp the throttle markers and
// emit the event; re-emit once, escalated, after unknownStateEscalationAge.
func decideUnknownStateRow(in sessionInputs) sessionDecision {
	d := sessionDecision{Reason: decideUnknownState}
	now := in.Now.UTC()
	row := in.Row
	first, err := time.Parse(time.RFC3339, strings.TrimSpace(row.UnknownStateFirstSeen))
	switch {
	case strings.TrimSpace(row.UnknownStateFirstSeen) == "" || row.UnknownStateValue != row.MetadataState:
		patch := session.MetadataPatch{unknownStateFirstSeenKey: now.Format(time.RFC3339), unknownStateValueKey: row.MetadataState}
		if strings.TrimSpace(row.UnknownStateEscalatedAt) != "" {
			patch[unknownStateEscalatedKey] = ""
		}
		ev := sessionUnknownStateEvent(row, now, now, false)
		d.Action, d.Event, d.RequeueAfter = sessionAction{Kind: actWrite, Patch: patch}, &ev, unknownStateEscalationAge
	case strings.TrimSpace(row.UnknownStateEscalatedAt) != "" || err != nil:
		// Escalated already, or the first-seen clock is unreadable: stay
		// silent, as legacy does.
	case now.Sub(first) < unknownStateEscalationAge:
		d.RequeueAfter = first.Add(unknownStateEscalationAge).Sub(now) + time.Second
	default:
		ev := sessionUnknownStateEvent(row, now, first, true)
		d.Action = sessionAction{Kind: actWrite, Patch: session.MetadataPatch{unknownStateEscalatedKey: now.Format(time.RFC3339)}}
		d.Event = &ev
	}
	return d
}

// rowIncarnation is the row's generation, the incarnation a snapshot's
// Basis names; an unparseable one is 0.
func rowIncarnation(row session.Info) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(row.Generation), 10, 64)
	return n
}

// requeueUntil is the requeue delay to t: at least a nanosecond, so a deadline that
// has passed re-runs the key at once (zero means no requeue).
func requeueUntil(now, t time.Time) time.Duration {
	return max(t.Sub(now), time.Nanosecond)
}

// earlierRequeue is the sooner of two requeue delays, where zero means none.
func earlierRequeue(a, b time.Duration) time.Duration {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

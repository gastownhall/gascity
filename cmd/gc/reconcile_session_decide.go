package main

import (
	"maps"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// The planner's per-row decide: decideRow, CONTRACT v5 §4's arm table.

// Decide reasons.
const (
	decideNoRow           = "no-row"
	decideKillFence       = "kill-fence"
	decideUnknownState    = "unknown-state"
	decideLivenessUnknown = "liveness-unknown"
	decideUnranked        = "unranked"
	decideNoAction        = "no-action"
)

// timerHealPatch clears an expired hold, then an expired quarantine judged
// against the post-hold sleep_reason (SESS-009/010: the order is
// load-bearing), and clears unknown-state markers on a known-state row
// (SESS-047). The timers are the disposition's TimerTick cell (class), the
// lifecycle the expired-timer builders'. requeue is the time until the
// earliest timer still running.
func timerHealPatch(row session.Info, now time.Time) (session.MetadataPatch, session.DispositionPatch, time.Duration) {
	patch := session.MetadataPatch{}
	var requeue time.Duration
	expired := false
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
			expired = true
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
	var class session.DispositionPatch
	if expired {
		class = session.CellInfo(row, session.ActorController, session.DispEvent{Kind: session.EvTimerTick}, now)
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
	return patch, class, requeue
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

// decideRow is one row's decision in the pass (CONTRACT v5 §4): the first arm
// in rowArms that returns an intent or a hold wins. It is pure: no probe, no
// event, and the time is w's. A hold or no action has no Kind; Reason names
// the arm for the trace (R6). next is the row's earliest deadline, zero for
// none. The pass skips rows with an effect in flight (R5).
//
//gc:pure
func decideRow(w *World, a *allocDecision, k rowKey) (it intent, next time.Time) {
	r := &rowFacts{w: w, k: k, entry: a.Snapshot.Entries[k], partial: len(a.Snapshot.Partial.Global) > 0}
	r.row, r.found = w.Census.Rows[k]
	if f := w.fresh; f != nil && f.k == k {
		r.fresh = f.rt
	}
	for i, arm := range rowArms {
		r.rests = 0
		if it, ok := arm.decide(r); ok {
			if it.Kind != "" { // a hold has no effect to read anything
				it.Rests = r.rests
			}
			it.Key = k
			if arm.name == "A6" && it.Kind != "" {
				it.Patch, it.Class = r.fold(it.Patch, it.Class, rowArms[i+1:])
				if it.Kind == intentRowHeal || it.Reason == decideDeadRuntimeHeal {
					it = r.startHealed(it)
				}
			}
			return it, r.next
		}
	}
	return intent{Key: k, Reason: decideNoAction}, r.next
}

// fold adds to an A6 item's patch the plain row writes that A6's later items
// propose for the row in the same pass, so the row's heals and markers land in
// one CAS and none waits a pass behind another (CONTRACT v5.8 §12.3, R5). A
// key an earlier item writes keeps its value; a later fresh heal waits.
func (r *rowFacts) fold(patch session.MetadataPatch, class session.DispositionPatch, rest []rowArm) (session.MetadataPatch, session.DispositionPatch) {
	patch = maps.Clone(patch)
	for _, arm := range rest {
		if arm.name != "A6" {
			break
		}
		if it, ok := arm.decide(r); ok && it.Kind == intentRowHeal {
			for key, value := range it.Patch {
				if _, set := patch[key]; !set {
					patch[key] = value
				}
			}
			class = it.Class.With(class) // the earlier item's keys win
		}
	}
	return patch, class
}

// rowFacts is what decideRow's arms read of one row, and the earliest
// deadline the arms so far have found.
type rowFacts struct {
	w     *World
	k     rowKey
	row   censusRow
	found bool
	entry *selectionEntry
	next  time.Time
	rests rests      // what the arm deciding reads through the fresh accessors
	fresh *txRuntime // the effect's fresh runtime read (World.withRuntime), else nil
	// partial is a pass with a global partial cause (P-3): nothing closes.
	// Not the snapshot's mode, which a suspended city overwrites.
	partial bool
}

// rests are the fresh facts a decision rests on (EFFECT-STRUCTURE §2.2;
// ruling (c)). Only a fresh kind's arm records one, through a rest-recording
// accessor (freshGone, freshOwnAlive); its effect reads the fact fresh and
// decides again on it (withRuntime). A plain kind's arms read the pass's
// facts and rest on none: they are advisory, and admission refuses a plain
// intent that rests on a fact its kind does not read.
type rests uint8

const restRuntime rests = 1 << iota // the runtime's liveness and identity

// reads are the rests s reads fresh for it, its needsFor's included.
func (s effectSpec) reads(w *World, it intent) rests {
	if s.needsOf(w, it).Runtime {
		return restRuntime
	}
	return 0
}

// after records a deadline d from now, keeping the earliest; zero is none.
func (r *rowFacts) after(d time.Duration) {
	if d <= 0 {
		return
	}
	if t := r.w.Now.Add(d); r.next.IsZero() || t.Before(r.next) {
		r.next = t
	}
}

// rowArm is one row of CONTRACT v5 §4's table. ok means the arm decided:
// an intent, or a hold when the intent has no Kind.
type rowArm struct {
	name   string // the table's arm number
	decide func(r *rowFacts) (it intent, ok bool)
}

// rowArms is CONTRACT v5 §4's table in its order; an arm with several rows
// (A1, A6) takes one line per row. Later PRs insert their arms at their
// numbers: A7 row metadata (C7d), A8 the baseline
// (C7c), A11-A17 and A21.
var rowArms = []rowArm{
	{"A1", armNoRow},
	{"A1", armCensusOnly},
	{"A2", armKillFence},
	{"A3", armIdentity},
	{"A4", armStop},
	{"A5", armUnknownState},
	{"A6", armTimerHeals},
	{"A6", armClaimClear},
	{"A6", armCreatingHeal},
	{"A6", armDeadRuntimeHeal},
	{"A6", armAwakeHeal},
	{"A6", armStabilityClear},
	{"A6", armDetachedAt},
	{"A6", armStrandedClear},
	{"A6", armCurrentBead},
	{"A7", armRowMetadata},
	{"A8", armBaseline},
	{"A9", armLivenessUnknown},
	{"A10", armAdopt},
	{"A10", armRollback},
	{"A11", armAck},
	{"A12", armZombie},
	{"A14", armConfigDrift},
	{"A16", armExecutionStalled},
	{"A17", armHeartbeatRespawn},
	{"A18", armStart},
	{"A19", armDrainVoidCancel},
	{"A20", armDrainBegin},
	{"A21", armClose},
}

// decideRow's other reasons.
const (
	decideMislabelled = "mislabelled"
	decideTimerHeal   = "timer-heal"
)

// armCensusOnly is A1's second row: a row on a leg other than the sessions
// leg is AL1's None(census-only), ahead of every later arm (CONTRACT v5 §4 A6
// item 4's rule, for every arm); a mislabelled one traces mislabelled first
// (D-32). Legacy reconciles only the sessions store,
// and a shared rig store holds other cities' rows, so no arm writes, rekeys,
// drains or closes a row on another leg, and an effect admitted for one
// re-decides on the fresh row and refuses.
func armCensusOnly(r *rowFacts) (intent, bool) {
	if r.k.Leg == r.w.Census.sessionsLeg() {
		return intent{}, false
	}
	return intent{Reason: reasonCensusOnly}, true
}

// armNoRow is A1: no canonical census row, or a mislabelled one (no
// template and no session name, CONTRACT v5 AL1), is None.
func armNoRow(r *rowFacts) (intent, bool) {
	switch {
	case !r.found || r.row.DuplicateOf != "":
		return intent{Reason: decideNoRow}, true
	case r.w.Mislabelled[r.k]:
		return intent{Reason: decideMislabelled}, true
	}
	return intent{}, false
}

// armKillFence is A2: a `gc session kill` owns the row until its fence ages
// out, which is the row's deadline.
func armKillFence(r *rowFacts) (intent, bool) {
	if !session.IsKillPendingInfo(r.row.Info, r.w.Now) {
		return intent{}, false
	}
	at, _ := time.Parse(time.RFC3339, strings.TrimSpace(r.row.Info.SleptAt))
	r.after(requeueUntil(r.w.Now, at.Add(session.KillPendingGrace)))
	return intent{Reason: decideKillFence}, true
}

// armUnknownState is A5: a state this version does not know, other than
// stop-pending (the census's UnknownState), is skipped. The trace records it
// once (R6); v5 writes no markers and records no event for it.
func armUnknownState(r *rowFacts) (intent, bool) {
	if !r.row.UnknownState {
		return intent{}, false
	}
	return intent{Reason: decideUnknownState}, true
}

// armTimerHeals is A6's timer heals (SESS-009/010, and SESS-047's marker
// clear): a row write when a timer expired. A timer still running is the
// row's deadline, and the row falls through.
func armTimerHeals(r *rowFacts) (intent, bool) {
	patch, class, requeue := timerHealPatch(r.row.Info, r.w.Now)
	r.after(requeue)
	if len(patch) == 0 && class.Len() == 0 {
		return intent{}, false
	}
	basis := rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}
	return intent{Kind: intentRowHeal, Reason: decideTimerHeal, Basis: basis, Patch: patch, Class: class}, true
}

// armLivenessUnknown is A9: every arm below reads liveness and desire, so
// the row holds while its liveness is unknown (GUAR-053), or while the
// allocation has no entry for it.
func armLivenessUnknown(r *rowFacts) (intent, bool) {
	switch {
	case r.entry == nil:
		return intent{Reason: decideUnranked}, true
	case r.entry.Liveness == livenessUnknown:
		return intent{Reason: decideLivenessUnknown}, true
	}
	return intent{}, false
}

// drainKind is a drain begin's or signal's kind by CONTRACT v5 D2's fresh-legs
// column: idle, no-wake-reason and idle-respawn read fresh legs or re-prove
// idleness and claims, so they are the probing -fresh kinds; every other
// reason is a plain row write. A20 (C6a, C6b1) proposes through it.
func drainKind(reason string, signal bool) string {
	fresh := reason == drainIdle || reason == reasonNoWake || reason == idleRespawnDrainReason
	switch {
	case signal && fresh:
		return intentSignalFresh
	case signal:
		return intentSignal
	case fresh:
		return intentDrainBeginFresh
	}
	return intentDrainBegin
}

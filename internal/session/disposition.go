package session

import (
	"fmt"
	"strings"
	"time"
)

// This file is a row's disposition (ARCH-RESTRUCTURE-2 R7): the operator's
// and the agent's intent that holds a row, decoded from the five keys that
// encode it (held_until, sleep_intent, wait_hold, quarantined_until,
// suspended_at). DecodeDisposition is their one parser and Encode their one
// encoder; Apply is the one table of the moves between dispositions.

// OperatorHold is the operator dimension of a disposition.
type OperatorHold uint8

const (
	// OperatorNone is no operator suspend.
	OperatorNone OperatorHold = iota
	// OperatorSuspended is an operator's suspend (sleep_intent=user-hold),
	// until Until; it outlives a drain and keeps the row's slot.
	OperatorSuspended
	// OperatorSuspendedSoft is state=suspended with no operator hold (the chat
	// idle auto-suspend, the city stop sweep). Any wake brings it back.
	OperatorSuspendedSoft
)

// sleepIntentIdleStopPending is the idle stop's sleep_intent: a controller
// transient, never a hold.
const sleepIntentIdleStopPending SleepReason = "idle-stop-pending"

// DispositionBit names a key present but unreadable.
type DispositionBit uint8

const (
	// UnknownHeldUntil is an unparseable held_until.
	UnknownHeldUntil DispositionBit = 1 << iota
	// UnknownQuarantine is an unparseable quarantined_until.
	UnknownQuarantine
	// UnknownIntent is a sleep_intent no cell writes. It holds nothing, and
	// its text stands until a cell that clears the intent (a wake, a kill,
	// a start, a retire) or writes one of its own.
	UnknownIntent
	// UnknownSince is an unparseable suspended_at, which stands until a cell
	// that writes the stamp: every cell that sets or clears Since clears it.
	UnknownSince
)

// unreadableTimers are the bits of a timer, which a reader takes as held: no
// one can tell when it ends.
const unreadableTimers = UnknownHeldUntil | UnknownQuarantine

// dispositionKeys are the class's five keys, in the order Disposition.raw
// keeps their text.
var dispositionKeys = [...]string{"held_until", "sleep_intent", "wait_hold", "quarantined_until", "suspended_at"}

// Disposition is a row's holds as one value.
type Disposition struct {
	Operator OperatorHold
	// Until is the operator suspend's held_until (now+IndefiniteHoldDuration
	// unless the suspend was finite).
	Until time.Time
	// Since is suspended_at.
	Since time.Time
	// Wait is an agent's wait (wait_hold).
	Wait bool
	// WaitIntent is sleep_intent=wait-hold: a wait's own intent, which only
	// a wait begins and a sleep keeps (D-J 3). Legacy's drain completion
	// clears it and leaves a bare wait_hold, which holds the same.
	WaitIntent bool
	// Heartbeat is held_until when no operator suspend explains it.
	Heartbeat time.Time
	// Quarantine is quarantined_until.
	Quarantine time.Time
	// IdleStop is sleep_intent=idle-stop-pending: a controller transient,
	// never a hold.
	IdleStop bool
	// Reason is the row's sleep_reason when it is an operator's (killed,
	// user-hold, city-stop). Decoded only: the lifecycle writes it.
	Reason SleepReason
	// Unknown are keys present but unreadable.
	Unknown DispositionBit
	// raw is the decoded row's text for each class key (dispositionKeys), so
	// Encode rewrites a key only when its meaning moves: an unknown intent,
	// a bare wait_hold, a timer's exact text and an unreadable stamp survive
	// every move that does not change them.
	raw [len(dispositionKeys)]string
}

// DecodeDisposition is meta's disposition, the one parser of its keys.
func DecodeDisposition(meta map[string]string) Disposition {
	var d Disposition
	for i, k := range dispositionKeys {
		d.raw[i] = meta[k]
	}
	intent := SleepReason(strings.TrimSpace(meta["sleep_intent"]))
	switch {
	case intent == SleepReasonUserHold:
		d.Operator = OperatorSuspended
	case State(strings.TrimSpace(meta["state"])) == StateSuspended:
		d.Operator = OperatorSuspendedSoft
	}
	d.IdleStop = intent == sleepIntentIdleStopPending
	d.WaitIntent = intent == SleepReasonWaitHold
	if intent != "" && intent != SleepReasonUserHold && !d.IdleStop && !d.WaitIntent {
		d.Unknown |= UnknownIntent
	}
	d.Wait = strings.TrimSpace(meta["wait_hold"]) != ""
	if raw := strings.TrimSpace(meta["suspended_at"]); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err != nil {
			d.Unknown |= UnknownSince
		} else {
			d.Since = t
		}
	}
	if raw := strings.TrimSpace(meta["held_until"]); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err != nil {
			d.Unknown |= UnknownHeldUntil
		} else if d.Operator == OperatorSuspended {
			d.Until = t
		} else {
			d.Heartbeat = t
		}
	}
	if raw := strings.TrimSpace(meta["quarantined_until"]); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err != nil {
			d.Unknown |= UnknownQuarantine
		} else {
			d.Quarantine = t
		}
	}
	switch r := SleepReason(strings.TrimSpace(meta["sleep_reason"])); r {
	case SleepReasonKilled, SleepReasonUserHold, SleepReasonCityStop:
		d.Reason = r
	}
	return d
}

// DispositionOfInfo is DecodeDisposition over a typed row.
func DispositionOfInfo(info Info) Disposition { return DecodeDisposition(dispositionMeta(info)) }

// dispositionMeta is info's disposition keys and the lifecycle keys the
// decoder reads.
func dispositionMeta(info Info) map[string]string {
	return map[string]string{
		"state": info.MetadataState, "held_until": info.HeldUntil, "quarantined_until": info.QuarantinedUntil,
		"sleep_intent": info.SleepIntent, "wait_hold": info.WaitHold, "suspended_at": info.SuspendedAt,
		"sleep_reason": info.SleepReason,
	}
}

func formatDispositionTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// unreadableTimer is canon's value for a key d cannot read: it equals
// itself, so a move that leaves the key keeps its text.
const unreadableTimer = "\x00unreadable"

// canon is d's meaning for each class key, in today's values: an operator
// suspend is sleep_intent=user-hold with held_until its Until; a wait is
// wait_hold=true; its intent sleep_intent=wait-hold.
func (d Disposition) canon() [len(dispositionKeys)]string {
	var c [len(dispositionKeys)]string
	switch {
	case d.Unknown&UnknownHeldUntil != 0:
		c[0] = unreadableTimer
	case d.Operator == OperatorSuspended:
		c[0] = formatDispositionTime(d.Until)
	default:
		c[0] = formatDispositionTime(d.Heartbeat)
	}
	switch {
	case d.Operator == OperatorSuspended:
		c[1] = string(SleepReasonUserHold)
	case d.IdleStop:
		c[1] = string(sleepIntentIdleStopPending)
	case d.WaitIntent:
		c[1] = string(SleepReasonWaitHold)
	case d.Unknown&UnknownIntent != 0:
		c[1] = unreadableTimer
	}
	if d.Wait {
		c[2] = "true"
	}
	if d.Unknown&UnknownQuarantine != 0 {
		c[3] = unreadableTimer
	} else {
		c[3] = formatDispositionTime(d.Quarantine)
	}
	if d.Unknown&UnknownSince != 0 {
		c[4] = unreadableTimer
	} else {
		c[4] = formatDispositionTime(d.Since)
	}
	return c
}

// Encode is d as its five keys, always all of them. A key whose meaning d
// has not moved from the row it was decoded from keeps that row's text, so
// Encode(Decode(row)) is the row and a rollback build reads what it writes;
// a key that moved takes today's value (canon). The lifecycle keys (state,
// sleep_reason) are the caller's.
func (d Disposition) Encode() DispositionPatch {
	var base Disposition
	if d.raw != base.raw {
		base = DecodeDisposition(d.rawMeta())
	}
	was, now := base.canon(), d.canon()
	patch := make(MetadataPatch, len(dispositionKeys))
	for i, k := range dispositionKeys {
		if now[i] == was[i] {
			patch[k] = d.raw[i]
		} else {
			patch[k] = now[i]
		}
	}
	return DispositionPatch{patch}
}

// Changed is the part of Encode that differs from the row d was decoded
// from: what a write of d must change, and all a write over a store without
// conditional writes may touch.
func (d Disposition) Changed() DispositionPatch {
	patch := d.Encode().kv
	for i, k := range dispositionKeys {
		if patch[k] == d.raw[i] {
			delete(patch, k)
		}
	}
	return DispositionPatch{patch}
}

func (d Disposition) rawMeta() map[string]string {
	meta := make(map[string]string, len(dispositionKeys))
	for i, k := range dispositionKeys {
		meta[k] = d.raw[i]
	}
	return meta
}

// SuppressesWake reports a hold that keeps the row out of the wake and
// awake sets. An unknown timer does not, as legacy reads it (O6).
func (d Disposition) SuppressesWake(now time.Time) bool {
	return d.Operator != OperatorNone || d.Wait || d.Until.After(now) || d.Heartbeat.After(now) || d.Quarantine.After(now)
}

// BlocksConsume reports a hold that stops anything consuming one: a resume,
// a background send's queue-or-start, a wake request. An unknown timer
// blocks, since an unreadable hold may be an operator's (O6).
func (d Disposition) BlocksConsume(now time.Time) bool {
	return d.SuppressesWake(now) || d.Unknown&unreadableTimers != 0
}

// RefusesPreWake reports a hold that refuses the controller's PreWake (C5a1,
// S1): an operator suspend, soft or not, or a live timer. A wait does not:
// wait-ready launches it (POOL-077/080). Nor does an unreadable timer, which
// would never heal. It replaced C5a1's hold mask (a timer, a quarantine, a
// user hold, a suspended state) in R7d with no change. CONTRACT v5.9i
// adds A17's agent-heartbeat exemption on top; that is A17's, not this.
func (d Disposition) RefusesPreWake(now time.Time) bool {
	return d.Operator != OperatorNone || d.Until.After(now) || d.Heartbeat.After(now) || d.Quarantine.After(now)
}

// RefusesHeartbeatRespawn is RefusesPreWake for A17's respawn through an
// agent heartbeat (v5.9i), which no live timer refuses: an operator suspend,
// soft or not, or a live quarantine.
func (d Disposition) RefusesHeartbeatRespawn(now time.Time) bool {
	return d.Operator != OperatorNone || d.Quarantine.After(now)
}

// OperatorDormant reports a row its disposition holds dormant: an operator's
// suspend, kill or city stop, a wait, or a live timer. A6's awake heal skips
// it and D3 stops its runtime. A kill fence (IsKillPendingInfo) is one too:
// its sleep_reason is killed.
func (d Disposition) OperatorDormant(now time.Time) bool {
	return d.Reason != "" || d.Operator == OperatorSuspended || d.Wait || d.Heartbeat.After(now) ||
		d.Until.After(now) || d.Quarantine.After(now)
}

// MaxHeartbeatDuration bounds one agent heartbeat's hold (gc runtime
// heartbeat --duration).
const MaxHeartbeatDuration = 12 * time.Hour

// HeartbeatHeld reports an agent heartbeat keeping a live row up (A17,
// SESS-602, v5.9i): a live held_until under no sleep intent at all, within a
// heartbeat's horizon (MaxHeartbeatDuration plus an hour's skew), so an
// operator hold whose intent a blind write cleared is not one. A bare
// wait_hold beside it does not stop it.
func (d Disposition) HeartbeatHeld(now time.Time) bool {
	return d.Heartbeat.After(now) && !d.Heartbeat.After(now.Add(MaxHeartbeatDuration+time.Hour)) && strings.TrimSpace(d.Encode().Get("sleep_intent")) == ""
}

// HoldsConsume is CONTRACT v5.9 D8 7(a)'s one hold predicate (HoldVerdict),
// for resume, wake requests, controller-routed sends and queued delivery: a
// hold that blocks a consume, unless the row is not suspended (life) and its
// runtime is running (a working session; a heartbeat keeps it up, not
// queued).
func (d Disposition) HoldsConsume(running bool, life State, now time.Time) bool {
	return d.BlocksConsume(now) && (life == StateSuspended || d.Operator == OperatorSuspendedSoft || !running)
}

// KeepsSlot reports a row whose slot no free or close may take: an
// operator's suspend (the freeable, finalize and MAINT-051 hold exception,
// §12.2 row 27). A21 reads the stricter HoldsDeliberately.
func (d Disposition) KeepsSlot() bool { return d.Operator == OperatorSuspended }

// HoldsDeliberately reports a deliberate hold: an operator's suspend (not a
// soft one), a wait, or a live or unreadable timer. A21 keeps an undesired
// pool row's slot under one, stricter than KeepsSlot (§12.2 #27, v5.7 A10).
func (d Disposition) HoldsDeliberately(now time.Time) bool {
	return d.Operator == OperatorSuspended || d.Wait || d.Until.After(now) || d.Heartbeat.After(now) ||
		d.Quarantine.After(now) || d.Unknown&unreadableTimers != 0
}

// DispEventKind is what moves a disposition.
type DispEventKind uint8

const (
	// EvSuspend is an operator's suspend (CLI, managed, API), until
	// DispEvent.Until, or indefinitely.
	EvSuspend DispEventKind = iota + 1
	// EvSuspendSoft is a suspend that writes no hold: the chat idle
	// auto-suspend, the city stop sweep.
	EvSuspendSoft
	// EvKill is an operator's kill.
	EvKill
	// EvResume is a resume: an operator's consumes a suspend.
	EvResume
	// EvRestart is an interrupt's restart of the runtime it stopped.
	EvRestart
	// EvHeartbeat is an agent's heartbeat, until DispEvent.Until.
	EvHeartbeat
	// EvWaitBegin is an agent's wait beginning.
	EvWaitBegin
	// EvWaitEnd is its end.
	EvWaitEnd
	// EvSleep is a sleep or drain completion.
	EvSleep
	// EvPreWake is the controller's start of a dormant row.
	EvPreWake
	// EvIdleStopBegin marks the idle stop's transient.
	EvIdleStopBegin
	// EvIdleStopEnd clears it.
	EvIdleStopEnd
	// EvQuarantine is a quarantine until DispEvent.Until.
	EvQuarantine
	// EvTimerTick expires the timers past now.
	EvTimerTick
	// EvWake is a wake (`gc session wake`): an operator's clears every
	// blocker, an operator suspend included (D-J 1).
	EvWake
	// EvRetire is a retirement: every hold goes.
	EvRetire
)

var dispEventNames = [...]string{
	"", "Suspend", "SuspendSoft", "Kill", "Resume", "Restart", "Heartbeat", "WaitBegin",
	"WaitEnd", "Sleep", "PreWake", "IdleStopBegin", "IdleStopEnd", "Quarantine", "TimerTick", "Wake", "Retire",
}

func (k DispEventKind) String() string {
	if int(k) < len(dispEventNames) && k != 0 {
		return dispEventNames[k]
	}
	return fmt.Sprintf("DispEventKind(%d)", k)
}

// DispEvent is one move of a disposition.
type DispEvent struct {
	Kind  DispEventKind
	Until time.Time
	// Running is whether the row's runtime runs, for a resume: an agent's
	// or a background caller's resume queues by it (HoldVerdict).
	Running bool
}

// ErrDispositionRefused is a cell that refuses its move. The caller keeps
// the disposition it had: a refused resume queues, a refused PreWake does
// not start, and so on.
type ErrDispositionRefused struct {
	By    ActorKind
	Event DispEventKind
	From  OperatorHold
	Why   string
}

func (e *ErrDispositionRefused) Error() string {
	return fmt.Sprintf("disposition: %s by actor %d refused from operator hold %d: %s", e.Event, e.By, e.From, e.Why)
}

// Apply is d after ev by by, while the row's lifecycle state is life. It is
// pure: the table, nothing else.
func (d Disposition) Apply(by ActorKind, ev DispEvent, life State, now time.Time) (Disposition, error) {
	refuse := func(why string) (Disposition, error) {
		return d, &ErrDispositionRefused{By: by, Event: ev.Kind, From: d.Operator, Why: why}
	}
	switch ev.Kind {
	case EvSuspend:
		if by != ActorOperator {
			return refuse("only an operator's suspend holds")
		}
		if d.Operator == OperatorSuspended && d.Until.After(now) {
			d.Since = now // a suspend in force is stamped again: a resume that read the old stamp refuses
			d.Unknown &^= UnknownSince
			return d, nil
		}
		until := ev.Until
		if until.IsZero() {
			until = now.Add(IndefiniteHoldDuration)
		}
		d.Unknown &^= UnknownSince
		d.Operator, d.Until, d.Since, d.Heartbeat, d.IdleStop, d.WaitIntent = OperatorSuspended, until, now, time.Time{}, false, false
		d.Unknown &^= UnknownHeldUntil
	case EvSuspendSoft:
		if d.Operator == OperatorNone {
			d.Unknown &^= UnknownSince
			d.Operator, d.Since, d.IdleStop = OperatorSuspendedSoft, now, false
		}
	case EvKill:
		if by != ActorOperator {
			return refuse("only an operator kills")
		}
		if d.Operator == OperatorSuspended {
			d.Unknown &^= UnknownHeldUntil
		}
		d.Unknown &^= UnknownIntent | UnknownSince
		d.Operator, d.Until, d.Since, d.IdleStop, d.WaitIntent = OperatorNone, time.Time{}, time.Time{}, false, false
	case EvResume:
		switch {
		case by == ActorOperator && d.Operator == OperatorSuspended && life == StateDraining:
			return refuse("the drain's stop owns the runtime")
		case by == ActorOperator:
			d = d.clearOperator()
		case d.queues(by, ev.Running, life, now):
			return refuse("the caller queues on a held row (HoldVerdict)")
		}
	case EvRestart:
	case EvHeartbeat:
		if d.Operator != OperatorSuspended {
			d.Heartbeat = ev.Until // it replaces held_until, as legacy's did: a heartbeat-shaped ∞ heals
			d.Unknown &^= UnknownHeldUntil
		}
	case EvWaitBegin:
		d.Wait, d.WaitIntent, d.IdleStop = true, d.Operator != OperatorSuspended, false
	case EvWaitEnd:
		d.Wait, d.WaitIntent = false, false
	case EvSleep:
		d.IdleStop = false
		if d.Operator != OperatorSuspended {
			d.Unknown &^= UnknownSince
			d.Operator, d.Since = OperatorNone, time.Time{}
		}
	case EvPreWake:
		if d.Operator == OperatorSuspended {
			return refuse("a suspended row never starts")
		}
		d.Unknown &^= UnknownIntent | UnknownSince
		d.Operator, d.Since, d.IdleStop, d.WaitIntent = OperatorNone, time.Time{}, false, false
	case EvIdleStopBegin:
		if d.Operator != OperatorNone || d.WaitIntent {
			return refuse("an idle stop never runs over a hold")
		}
		d.IdleStop = true
	case EvIdleStopEnd:
		if d.Operator != OperatorNone {
			return refuse("an idle stop never runs over a hold")
		}
		d.IdleStop = false
	case EvQuarantine:
		d.Quarantine = ev.Until
		d.Unknown &^= UnknownQuarantine
	case EvTimerTick:
		if !d.Heartbeat.After(now) {
			d.Heartbeat = time.Time{}
		}
		if !d.Quarantine.After(now) {
			d.Quarantine = time.Time{}
		}
		if d.Operator == OperatorSuspended && !d.Until.IsZero() && !d.Until.After(now) {
			// An expired held_until on an operator's suspend goes, and the
			// suspend stands (main's heal). Every operator suspend is ∞, so an
			// expired one was shortened by a heartbeat racing it on a store
			// without CAS (D-I) or by an undo; D-J 2's release applies only to a
			// suspend the operator made finite, which needs a marker no writer
			// sets today.
			d.Until = time.Time{}
		}
	case EvWake:
		if by != ActorOperator {
			if d.SuppressesWake(now) {
				return refuse("only an operator's wake clears a hold")
			}
			return d, nil
		}
		d = Disposition{Reason: d.Reason, raw: d.raw}
	case EvRetire:
		d = Disposition{raw: d.raw}
	default:
		return refuse("no such event")
	}
	if d.Operator == OperatorSuspended || d.IdleStop || d.WaitIntent {
		d.Unknown &^= UnknownIntent // a cell's own intent replaces an unrecognized one
	}
	return d, nil
}

// queues reports whether a resume by by, not an operator, queues instead of
// starting the row: an agent's or the controller's on a held row whose
// runtime is not running or that is suspended, a background caller's also
// on any row whose runtime is not running (HoldVerdict, queueFor).
func (d Disposition) queues(by ActorKind, running bool, life State, now time.Time) bool {
	held := d.HoldsConsume(running, life, now)
	if by == ActorBackground {
		return !running || held
	}
	return held
}

// clearOperator is d with no operator suspend: its Until and Since go, and
// the held_until it owned.
func (d Disposition) clearOperator() Disposition {
	if d.Operator == OperatorSuspended {
		d.Unknown &^= UnknownHeldUntil
	}
	d.Unknown &^= UnknownSince
	d.Operator, d.Until, d.Since = OperatorNone, time.Time{}, time.Time{}
	return d
}

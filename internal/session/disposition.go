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

// DispositionBit names a timer present but unparseable.
type DispositionBit uint8

const (
	// UnknownHeldUntil is an unparseable held_until.
	UnknownHeldUntil DispositionBit = 1 << iota
	// UnknownQuarantine is an unparseable quarantined_until.
	UnknownQuarantine
)

// Disposition is a row's holds as one value.
type Disposition struct {
	Operator OperatorHold
	// Until is the operator suspend's held_until (now+IndefiniteHoldDuration
	// unless the suspend was finite).
	Until time.Time
	// Since is suspended_at.
	Since time.Time
	// Wait is an agent's wait (wait_hold, encoded with
	// sleep_intent=wait-hold under no operator suspend).
	Wait bool
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
	// Unknown are timers present but unparseable; rawUntil and rawQuarantine
	// keep their text, so an encode never rewrites a timer it cannot read.
	Unknown       DispositionBit
	rawUntil      string
	rawQuarantine string
}

// DecodeDisposition is meta's disposition, the one parser of its keys.
func DecodeDisposition(meta map[string]string) Disposition {
	var d Disposition
	intent := SleepReason(strings.TrimSpace(meta["sleep_intent"]))
	switch {
	case intent == SleepReasonUserHold:
		d.Operator = OperatorSuspended
	case State(strings.TrimSpace(meta["state"])) == StateSuspended:
		d.Operator = OperatorSuspendedSoft
	}
	d.IdleStop = intent == sleepIntentIdleStopPending
	d.Wait = strings.TrimSpace(meta["wait_hold"]) != ""
	d.Since = parseDispositionTime(meta["suspended_at"])
	if raw := strings.TrimSpace(meta["held_until"]); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err != nil {
			d.Unknown |= UnknownHeldUntil
			d.rawUntil = raw
		} else if d.Operator == OperatorSuspended {
			d.Until = t
		} else {
			d.Heartbeat = t
		}
	}
	if raw := strings.TrimSpace(meta["quarantined_until"]); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err != nil {
			d.Unknown |= UnknownQuarantine
			d.rawQuarantine = raw
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
func DispositionOfInfo(info Info) Disposition {
	return DecodeDisposition(map[string]string{
		"state": info.MetadataState, "held_until": info.HeldUntil, "quarantined_until": info.QuarantinedUntil,
		"sleep_intent": info.SleepIntent, "wait_hold": info.WaitHold, "suspended_at": info.SuspendedAt,
		"sleep_reason": info.SleepReason,
	})
}

func parseDispositionTime(raw string) time.Time {
	t, _ := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	return t
}

func formatDispositionTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Encode is d as its five keys, always all of them. It keeps today's
// values, so a rollback build reads the rows it writes: an operator suspend
// is sleep_intent=user-hold with held_until its Until; a wait under none is
// wait_hold=true with sleep_intent=wait-hold; a timer d cannot read keeps
// its text. The lifecycle keys (state, sleep_reason) are the caller's.
func (d Disposition) Encode() MetadataPatch {
	patch := MetadataPatch{
		"held_until":        formatDispositionTime(d.Heartbeat),
		"sleep_intent":      "",
		"wait_hold":         "",
		"quarantined_until": formatDispositionTime(d.Quarantine),
		"suspended_at":      formatDispositionTime(d.Since),
	}
	switch {
	case d.Operator == OperatorSuspended:
		patch["sleep_intent"] = string(SleepReasonUserHold)
		patch["held_until"] = formatDispositionTime(d.Until)
	case d.IdleStop:
		patch["sleep_intent"] = string(sleepIntentIdleStopPending)
	case d.Wait:
		patch["sleep_intent"] = string(SleepReasonWaitHold)
	}
	if d.Wait {
		patch["wait_hold"] = "true"
	}
	if d.Unknown&UnknownHeldUntil != 0 {
		patch["held_until"] = d.rawUntil
	}
	if d.Unknown&UnknownQuarantine != 0 {
		patch["quarantined_until"] = d.rawQuarantine
	}
	return patch
}

// SuppressesWake reports a hold that keeps the row out of the wake and
// awake sets. An unknown timer does not, as legacy reads it (O6). It is
// HoldSet.SuppressesWake over the same row.
func (d Disposition) SuppressesWake(now time.Time) bool {
	return d.Operator != OperatorNone || d.Wait || d.Until.After(now) || d.Heartbeat.After(now) || d.Quarantine.After(now)
}

// BlocksConsume reports a hold that stops anything consuming one: a resume,
// a background send's queue-or-start, a wake request. An unknown timer
// blocks, since an unreadable hold may be an operator's (O6). It is
// HoldSet.BlocksConsume over the same row.
func (d Disposition) BlocksConsume(now time.Time) bool {
	return d.SuppressesWake(now) || d.Unknown != 0
}

// OperatorDormant reports a row its disposition holds dormant: an operator's
// suspend, kill or city stop, a wait, or a live timer. A6's awake heal skips
// it and D3 stops its runtime; the kill fence (IsKillPendingInfo) is the
// lifecycle's half.
func (d Disposition) OperatorDormant(now time.Time) bool {
	return d.Reason != "" || d.Operator == OperatorSuspended || d.Wait || d.Heartbeat.After(now) ||
		d.Until.After(now) || d.Quarantine.After(now)
}

// HeartbeatHeld reports an agent heartbeat keeping a live row up (A17,
// SESS-602): a live held_until that neither an operator suspend, a wait nor
// an idle stop explains.
func (d Disposition) HeartbeatHeld(now time.Time) bool {
	return d.Heartbeat.After(now) && d.Operator != OperatorSuspended && !d.Wait && !d.IdleStop
}

// KeepsSlot reports a row whose slot no free or close may take: an
// operator's suspend (the freeable, finalize, MAINT-051 and A21 hold
// exception, §12.2 row 27).
func (d Disposition) KeepsSlot() bool { return d.Operator == OperatorSuspended }

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
			return d, nil // idempotent while the suspend in force lasts
		}
		until := ev.Until
		if until.IsZero() {
			until = now.Add(IndefiniteHoldDuration)
		}
		d.Operator, d.Until, d.Since, d.Heartbeat, d.IdleStop = OperatorSuspended, until, now, time.Time{}, false
		d = d.forget(UnknownHeldUntil)
	case EvSuspendSoft:
		if d.Operator == OperatorNone {
			d.Operator, d.Since, d.IdleStop = OperatorSuspendedSoft, now, false
		}
	case EvKill:
		if by != ActorOperator {
			return refuse("only an operator kills")
		}
		if d.Operator == OperatorSuspended {
			d = d.forget(UnknownHeldUntil)
		}
		d.Operator, d.Until, d.Since, d.IdleStop = OperatorNone, time.Time{}, time.Time{}, false
	case EvResume:
		switch {
		case by == ActorOperator && d.Operator == OperatorSuspended && life == StateDraining:
			return refuse("the drain's stop owns the runtime")
		case by == ActorOperator:
			d = d.clearOperator()
		case d.Operator != OperatorNone:
			return refuse("only an operator's resume consumes a suspend")
		}
	case EvRestart:
	case EvHeartbeat:
		if d.Operator != OperatorSuspended && ev.Until.After(d.Heartbeat) {
			d.Heartbeat = ev.Until
			d = d.forget(UnknownHeldUntil)
		}
	case EvWaitBegin:
		d.Wait, d.IdleStop = true, false
	case EvWaitEnd:
		d.Wait = false
	case EvSleep:
		d.IdleStop = false
		if d.Operator != OperatorSuspended {
			d.Operator, d.Since = OperatorNone, time.Time{}
		}
	case EvPreWake:
		if d.Operator == OperatorSuspended {
			return refuse("a suspended row never starts")
		}
		d.Operator, d.Since, d.IdleStop = OperatorNone, time.Time{}, false
	case EvIdleStopBegin:
		if d.Operator != OperatorNone || d.Wait {
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
		d = d.forget(UnknownQuarantine)
	case EvTimerTick:
		if !d.Heartbeat.After(now) {
			d.Heartbeat = time.Time{}
		}
		if !d.Quarantine.After(now) {
			d.Quarantine = time.Time{}
		}
		if d.Operator == OperatorSuspended && !d.Until.IsZero() && !d.Until.After(now) {
			d = d.clearOperator() // an expired finite suspend: asleep, unheld (D-J 2, mc-92clf)
		}
	case EvWake:
		if by != ActorOperator {
			if d.BlocksConsume(now) {
				return refuse("only an operator's wake clears a hold")
			}
			return d, nil
		}
		d = Disposition{Reason: d.Reason}
	case EvRetire:
		d = Disposition{}
	default:
		return refuse("no such event")
	}
	return d, nil
}

// clearOperator is d with no operator suspend: its Until, Since and the
// held_until it owned go.
func (d Disposition) clearOperator() Disposition {
	if d.Operator == OperatorSuspended {
		d = d.forget(UnknownHeldUntil)
	}
	d.Operator, d.Until, d.Since = OperatorNone, time.Time{}, time.Time{}
	return d
}

// forget is d with the unreadable timer bit replaced: a move that writes the
// timer drops the text it could not read.
func (d Disposition) forget(bit DispositionBit) Disposition {
	d.Unknown &^= bit
	if bit == UnknownHeldUntil {
		d.rawUntil = ""
	} else {
		d.rawQuarantine = ""
	}
	return d
}

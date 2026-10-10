package session

import (
	"strings"
	"time"
)

// Hold is one operator intent that holds a row (CONTRACT v5.9 D8 7(a)),
// shaped after ARCH-RESTRUCTURE R3's concept kernel.
type Hold uint8

const (
	// HoldUser is sleep_intent=user-hold.
	HoldUser Hold = 1 << iota
	// HoldTimer is a future held_until (a user hold, or a heartbeat).
	HoldTimer
	// HoldQuarantine is a future quarantined_until.
	HoldQuarantine
	// HoldWait is a set wait_hold (with or without sleep_intent=wait-hold).
	HoldWait
	// HoldSuspended is state=suspended.
	HoldSuspended
)

// HoldSet is a row's holds at a moment: In are in force; Unknown are
// timers present but unparseable.
type HoldSet struct{ In, Unknown Hold }

// Holds reads meta's holds at now. It is the one parser of the hold keys
// for D8's callers. Legacy's own intents (idle-stop-pending) hold nothing.
func Holds(meta map[string]string, now time.Time) HoldSet {
	var h HoldSet
	if State(strings.TrimSpace(meta["state"])) == StateSuspended {
		h.In |= HoldSuspended
	}
	if strings.TrimSpace(meta["wait_hold"]) != "" {
		h.In |= HoldWait
	}
	if strings.TrimSpace(meta["sleep_intent"]) == string(SleepReasonUserHold) {
		h.In |= HoldUser
	}
	for key, bit := range map[string]Hold{"held_until": HoldTimer, "quarantined_until": HoldQuarantine} {
		raw := strings.TrimSpace(meta[key])
		if raw == "" {
			continue
		}
		if until, err := time.Parse(time.RFC3339, raw); err != nil {
			h.Unknown |= bit
		} else if until.After(now) {
			h.In |= bit
		}
	}
	return h
}

// SuppressesWake reports a hold that keeps the row out of the wake and
// awake sets. An Unknown timer does not, as legacy reads it
// (ARCH-RESTRUCTURE O6).
//
// TODO(R3/K-b): no caller yet; the awake-set and wake-suppression readers
// move onto it when they migrate to the hold kernel.
func (h HoldSet) SuppressesWake() bool { return h.In != 0 }

// BlocksConsume reports a hold that stops anything consuming one: a resume,
// a background send's queue-or-start, a wake request. An Unknown timer
// blocks, since an unreadable hold may be an operator's (ARCH-RESTRUCTURE O6).
func (h HoldSet) BlocksConsume() bool { return h.In|h.Unknown != 0 }

// HoldVerdict is CONTRACT v5.9 D8 7(a)'s one hold predicate, for resume,
// wake requests, controller-routed sends and queued delivery: a hold that
// blocks a consume, unless the row is not suspended and its runtime is
// running (a working session; a heartbeat held_until keeps it up, not queued).
func HoldVerdict(meta map[string]string, runtimeRunning bool, now time.Time) bool {
	h := Holds(meta, now)
	return h.BlocksConsume() && (h.In&HoldSuspended != 0 || !runtimeRunning)
}

// HoldVerdictInfo is HoldVerdict over a typed row.
func HoldVerdictInfo(info Info, runtimeRunning bool, now time.Time) bool {
	return HoldVerdict(map[string]string{
		"state": info.MetadataState, "held_until": info.HeldUntil, "quarantined_until": info.QuarantinedUntil,
		"sleep_intent": info.SleepIntent, "wait_hold": info.WaitHold,
	}, runtimeRunning, now)
}

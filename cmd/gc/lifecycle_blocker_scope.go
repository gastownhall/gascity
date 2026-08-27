package main

import (
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Which blockers each keyed lifecycle operation takes.
//
// lifecycleTimerBlockerInfo reports three values, and only two of them mean "do
// not act on this row" in general. user_hold and quarantine are timestamp-gated:
// they self-clear once their deadline passes, and ComputeAwakeSet independently
// forces ShouldWake=false for both. "pinned" is neither. It clears only when an
// operator unpins, and it is a standing instruction to keep the session AWAKE —
// which ComputeAwakeSet implements as a durable override that SETS
// ShouldWake=true (compute_awake_set.go). Nothing in that function suppresses
// anything on account of a pin.
//
// So a pin blocks exactly one operation: the idle ladder, where it means "do not
// idle-kill what an operator pinned". Main carved it back out of the age timer
// for its own fleet arm the day it was added (maxSessionAgeBlockerInfo,
// session_reconciler.go). Both helpers return a plain string, so a keyed seam
// that simply calls the full ladder inherits the wrong scope silently and still
// compiles. These are the keyed path's per-operation spellings, so the scope is
// named at every site that needs one.

// deadlineTimerBlockerInfo reports the durable blocker for ONE fired lifecycle
// deadline. The idle timer takes the pin; the age timer does not, because a
// max-age stop is a credential refresh rather than a kill — the row is re-woken
// by the pin override on the next tick, so deferring skips the refresh without
// saving the session, and a pin would defer it forever.
func deadlineTimerBlockerInfo(info sessionpkg.Info, now time.Time, maxAge bool) string {
	if maxAge {
		return maxSessionAgeBlockerInfo(info, now)
	}
	return lifecycleTimerBlockerInfo(info, now)
}

// wakeBlockerInfo reports the durable blocker that must refuse a WAKE.
//
// It is the same narrowing maxSessionAgeBlockerInfo performs, for a different
// reason, which is why it is spelled separately instead of calling the age
// timer's helper from a wake site. There the argument is cost: deferring saves
// nothing. Here it is direction — the pin is the reason this row is being woken
// at all. ComputeAwakeSet's durable pin override is what set ShouldWake=true
// with Reason="pin" on an asleep pinned row, so refusing the wake on
// blocker=="pinned" inverts `gc session pin` from "always keep awake" into
// "never restart". TestWakeAndMaxAgeBlockerScopesAgree keeps the two in step.
func wakeBlockerInfo(info sessionpkg.Info, now time.Time) string {
	if blocker := lifecycleTimerBlockerInfo(info, now); blocker != "pinned" {
		return blocker
	}
	return ""
}

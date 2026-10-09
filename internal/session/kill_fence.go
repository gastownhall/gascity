package session

import (
	"strings"
	"time"
)

// SleepReasonKilled is the sleep_reason `gc session kill` records. The string
// predates this constant; defining it here lets the kill fence below match it
// without a raw literal.
const SleepReasonKilled SleepReason = "killed"

// KillPendingReason is the state_reason `gc session kill` stamps on the row
// while it tears the runtime down. The kill records its asleep intent BEFORE
// stopping the runtime, so for the length of the provider Stop the row reads
// asleep while the process is still alive. Without a marker the controller's
// advisory-state heal reads that as drift and flips the row back to awake, and
// the next tick then sees awake next to a dead runtime: exactly the
// kill-then-heal race the ordering exists to close.
const KillPendingReason = "kill-pending"

// KillPendingGrace bounds how long a kill fence is honored. The killing CLI
// clears the fence as soon as the provider Stop returns, so an honored fence
// normally lives for one Stop. The bound only matters when that CLI died
// mid-kill: past it the row is ordinary asleep state again and the normal
// drift heal and wake rules take over.
const KillPendingGrace = 5 * time.Minute

// KillPendingPatch is the durable kill intent: SleepPatch(now, "killed") plus
// the kill-pending state_reason. slept_at doubles as the fence timestamp, and
// together with the state_reason it identifies this particular kill, so the
// CLI clears or rolls back only its own fence. The kill supersedes any pending
// wake request (CONTRACT v5.7 D7), so the patch clears it too, and a failed
// kill's rollback restores it.
func KillPendingPatch(now time.Time) MetadataPatch {
	patch := SleepPatch(now, string(SleepReasonKilled))
	patch["state_reason"] = KillPendingReason
	for k, v := range ClearWakeRequestPatch() {
		patch[k] = v
	}
	return patch
}

// KillPendingMetadata reports whether raw session metadata carries a kill
// fence that is still within KillPendingGrace of now. A fence with an
// unparseable timestamp, or one further than the grace in the future (clock
// skew or a bogus write), is not honored: a malformed fence must never pin a
// session out of the lifecycle loop.
func KillPendingMetadata(state, stateReason, sleepReason, sleptAt string, now time.Time) bool {
	if strings.TrimSpace(state) != string(StateAsleep) ||
		strings.TrimSpace(stateReason) != KillPendingReason ||
		SleepReason(strings.TrimSpace(sleepReason)) != SleepReasonKilled {
		return false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(sleptAt))
	if err != nil {
		return false
	}
	age := now.Sub(at)
	return age < KillPendingGrace && age > -KillPendingGrace
}

// IsKillPendingInfo reports whether info carries an honored kill fence: a
// `gc session kill` is mid-teardown and owns the row until it clears the
// fence. Lifecycle passes must leave such a row alone, neither healing it back
// to awake while the runtime is still dying nor restarting or closing it once
// the runtime is gone.
func IsKillPendingInfo(info Info, now time.Time) bool {
	return KillPendingMetadata(info.MetadataState, info.StateReason, info.SleepReason, info.SleptAt, now)
}

package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

func isDrainedSessionMetadata(meta map[string]string) bool {
	state := strings.TrimSpace(meta["state"])
	if state == "drained" {
		return true
	}
	return state == "asleep" && strings.TrimSpace(meta["sleep_reason"]) == "drained"
}

func isDrainedSessionBead(session beads.Bead) bool {
	return isDrainedSessionMetadata(session.Metadata)
}

// poolSessionIsLive reports whether a pool session bead represents an
// actively running session for the runningSessions counter in
// build_desired_state. An asleep or drained bead is not live — it holds
// no active process and must not suppress the isCold cross-store wake
// probe.
func poolSessionIsLive(session beads.Bead) bool {
	return !isDormantSessionBead(session)
}

// isDormantSessionBead reports whether a session bead represents a dormant
// worker — asleep (any sleep_reason) or drained — that cannot currently claim
// or service routed demand. An asleep ephemeral pool session is replaced by a
// fresh spawn rather than restarted (see selectOrPlanPoolSessionBead), so a
// dormant bead is not live capacity.
//
// Used by the cold-pool detection in buildDesiredState: counting dormant
// sessions as "running" kept a min=0 pool from ever registering as cold
// (gc-blo) — isCold stayed false, the cold-wake demand probe never ran, and
// newly-routed work sat unclaimed while the dormant sessions masked the
// demand.
func isDormantSessionBead(session beads.Bead) bool {
	if isDrainedSessionBead(session) {
		return true
	}
	return strings.TrimSpace(session.Metadata["state"]) == "asleep"
}

// isPoolSessionSlotFreeable reports whether a session's bead is in a terminal
// state where the pool slot it occupies can be freed — either explicitly
// drained, or asleep from a normal idle transition. Sessions parked via
// `gc session wait` (sleep_reason=wait-hold), held by context-churn
// quarantine, or otherwise signaling "don't touch me" keep their slot.
//
// Distinct from `isDrainedSessionBead` because drain-ack can land pool
// workers in state=asleep+sleep_reason=idle when the pre-close ownership
// snapshot falsely reports assigned work. Freeing the slot for idle-asleep
// pool beads lets the supervisor spawn a fresh worker for ready queue work
// instead of stranding it on a ghost slot.
//
// A session parked with sleep_reason=provider-terminal-error is also freeable:
// markProviderTerminalError has classified it as a dead, non-retryable provider
// failure, so its slot must be reaped — otherwise the dead bead and its worktree
// leak indefinitely while still excluded from pool capacity.
//
// An explicit sleep_reason is required: deny-by-default for unknown or
// missing reasons so writes that land in state=asleep without a known
// reason (legacy beads, regressions, write races) cannot silently free
// their slot.
func isPoolSessionSlotFreeable(session beads.Bead) bool {
	if isDrainedSessionBead(session) {
		return true
	}
	if strings.TrimSpace(session.Metadata["state"]) != "asleep" {
		return false
	}
	reason := strings.TrimSpace(session.Metadata["sleep_reason"])
	switch reason {
	case "idle", "idle-timeout", sleepReasonCityStop, "failed-create", sleepReasonRuntimeMissing,
		sleepReasonProviderTerminalError:
		return true
	}
	return false
}

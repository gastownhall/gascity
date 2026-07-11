package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
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

// isDrainedSessionInfo is the session.Info mirror of isDrainedSessionBead. It
// reads the RAW metadata state (Info.MetadataState) and sleep reason
// (Info.SleepReason), matching the bead form's untrimmed-key reads.
func isDrainedSessionInfo(i sessionpkg.Info) bool {
	state := strings.TrimSpace(i.MetadataState)
	if state == "drained" {
		return true
	}
	return state == "asleep" && strings.TrimSpace(i.SleepReason) == "drained"
}

// poolSessionIsLiveInfo reports whether a pool session represents an actively
// running session for the runningSessions counter in build_desired_state. An
// asleep or drained session is not live — it holds no active process and must
// not suppress the isCold cross-store wake probe. It reads the RAW state
// metadata (Info.MetadataState) and delegates the drained/asleep-drained check
// to isDrainedSessionInfo, matching the untrimmed-key reads the bead carried.
func poolSessionIsLiveInfo(i sessionpkg.Info) bool {
	if strings.TrimSpace(i.MetadataState) == "asleep" {
		return false
	}
	if isDrainedSessionInfo(i) {
		return false
	}
	return true
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

// isPoolSessionSlotFreeableInfo is the session.Info mirror of isPoolSessionSlotFreeable.
func isPoolSessionSlotFreeableInfo(i sessionpkg.Info) bool {
	if isDrainedSessionInfo(i) {
		return true
	}
	if strings.TrimSpace(i.MetadataState) != "asleep" {
		return false
	}
	reason := strings.TrimSpace(i.SleepReason)
	switch reason {
	case "idle", "idle-timeout", sleepReasonCityStop, "failed-create", sleepReasonRuntimeMissing,
		sleepReasonProviderTerminalError:
		return true
	}
	return false
}

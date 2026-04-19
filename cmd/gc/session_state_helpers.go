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
	case "idle", "idle-timeout":
		return true
	}
	return false
}

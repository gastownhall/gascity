package session

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// PendingCreateLease is the typed projection of the optimistic-concurrency
// tuple a session bead carries around a create/start attempt. It is a pure
// value: constructed from a session bead, never holding a store.
// All persisted keys are unchanged on disk; this type only centralizes the
// reads and the transition decisions that were previously scattered across
// the async-start staleness helpers in cmd/gc.
type PendingCreateLease struct {
	Closed bool // bead Status == "closed" (trimmed compare)

	// Identity fence. InstanceToken is authoritative when non-empty;
	// Generation is the legacy fallback, compared as a trimmed string and
	// never parsed (preserves the pre-refactor semantics exactly).
	InstanceToken string // strings.TrimSpace(metadata["instance_token"])
	Generation    string // strings.TrimSpace(metadata["generation"])

	// Claim is the boolean the protocol keys on.
	Claim bool // strings.TrimSpace(metadata["pending_create_claim"]) == "true"

	// State is the trimmed typed state every gate uses.
	State State
}

// LeaseFromBead projects the pending-create tuple off a raw session bead.
func LeaseFromBead(b beads.Bead) PendingCreateLease {
	return PendingCreateLease{
		Closed:        strings.TrimSpace(b.Status) == "closed",
		InstanceToken: strings.TrimSpace(b.Metadata["instance_token"]),
		Generation:    strings.TrimSpace(b.Metadata["generation"]),
		Claim:         strings.TrimSpace(b.Metadata["pending_create_claim"]) == "true",
		State:         State(strings.TrimSpace(b.Metadata["state"])),
	}
}

// LeaseCommitVerdict is what the async-start commit gate returns when an
// in-flight start result meets the current bead. The two mutually-exclusive
// boolean helpers it replaces (asyncStartSessionStillCurrent /
// asyncStartStaleRuntimeCleanupAllowed) fuse into this two-outcome enum.
type LeaseCommitVerdict int

const (
	// LeaseCommit means the result is still current — commit it against the
	// current bead.
	LeaseCommit LeaseCommitVerdict = iota
	// LeaseDiscardStopRuntime means the result is stale — discard it and (subject
	// to the separate runningSessionMatchesPendingCreate runtime probe) stop
	// the spawned runtime.
	LeaseDiscardStopRuntime
)

// StateConfirmsPendingStart reports whether a session in the given state
// should transition to "active" after a successful runtime spawn. Empty,
// "start-pending", "creating", "asleep", and "drained" all indicate the
// session was pending a spawn; "awake" is treated as equivalent to "active"
// and intentionally not restamped; every other state is left alone. This is
// the single home for that frozen pending-start state set: cmd/gc's
// confirmPendingStart is a thin string adapter that delegates here.
func StateConfirmsPendingStart(s State) bool {
	switch s {
	case "", StateStartPending, StateCreating, StateAsleep, StateDrained:
		return true
	}
	return false
}

// SameIdentity reports whether the receiver (the prepared snapshot taken at
// enqueue) and current describe the same session bead. instance_token is
// authoritative when the prepared side has one; only fall back to generation
// when the prepared bead has no token (legacy pre-instance_token snapshots).
// Generation drift with a matching token is a normal consequence of
// concurrent reconciler phases and must not invalidate an in-flight start
// result (#1542).
func (l PendingCreateLease) SameIdentity(current PendingCreateLease) bool {
	if l.InstanceToken != "" {
		return current.InstanceToken == l.InstanceToken
	}
	if l.Generation == "" {
		return true
	}
	return current.Generation == l.Generation
}

// CommitVerdict decides whether an async start result should commit against
// current. The receiver is the prepared snapshot; current is a fresh read.
// This fuses asyncStartSessionStillCurrent (verdict == LeaseCommit) and
// asyncStartStaleRuntimeCleanupAllowed (verdict == LeaseDiscardStopRuntime).
func (l PendingCreateLease) CommitVerdict(current PendingCreateLease) LeaseCommitVerdict {
	if current.Closed {
		return LeaseDiscardStopRuntime
	}
	if !l.SameIdentity(current) {
		return LeaseDiscardStopRuntime
	}
	// If the bead has progressed to a live state (active or awake), the spawn
	// already succeeded and another phase cleared pending_create_claim. The
	// async result still carries useful metadata — commit it rather than
	// discarding as stale. This row fires before the claim-cleared row below,
	// and that order is load-bearing (#1542).
	if current.State == StateAwake || current.State == StateActive {
		return LeaseCommit
	}
	// For sessions still mid-flight, reject if pending_create_claim was
	// cleared from under us — a different reconciler phase already rolled the
	// create back and committing would stomp its decision (#2073).
	if l.Claim && !current.Claim {
		return LeaseDiscardStopRuntime
	}
	if StateConfirmsPendingStart(current.State) {
		return LeaseCommit
	}
	return LeaseDiscardStopRuntime
}

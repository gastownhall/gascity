package session

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// PendingCreateLease is the typed projection of the optimistic-concurrency
// tuple a session bead carries around a create/start attempt. It is a pure
// value: constructed from a bead or an Info snapshot, never holding a store.
// All persisted keys are unchanged on disk; this type only centralizes the
// reads and the transition decisions that were previously scattered across
// the async-start staleness helpers in cmd/gc.
type PendingCreateLease struct {
	SessionID string // bead ID ("" allowed: store-less callers)
	Closed    bool   // bead Status == "closed" (trimmed compare)

	// Identity fence. InstanceToken is authoritative when non-empty;
	// Generation is the legacy fallback, compared as a trimmed string and
	// never parsed (preserves the pre-refactor semantics exactly).
	InstanceToken string // strings.TrimSpace(metadata["instance_token"])
	Generation    string // strings.TrimSpace(metadata["generation"])

	// Claim is the boolean the protocol keys on; ClaimRaw preserves the raw
	// metadata value so a non-"true" garbage value round-trips observably.
	Claim    bool   // strings.TrimSpace(metadata["pending_create_claim"]) == "true"
	ClaimRaw string // metadata["pending_create_claim"] verbatim

	// Timestamps kept raw (parsed on demand by the expiry family) so
	// unparseable values keep their per-call-site behavior.
	ClaimStartedAtRaw   string // metadata["pending_create_started_at"] verbatim
	AttemptStartedAtRaw string // metadata["last_woke_at"] verbatim

	// StateRaw is the verbatim state metadata; State is the trimmed typed
	// form every gate uses.
	StateRaw string
	State    State

	CreatedAt time.Time // bead CreatedAt (legacy expiry anchor)
}

// LeaseFromBead projects the pending-create tuple off a raw session bead.
func LeaseFromBead(b beads.Bead) PendingCreateLease {
	stateRaw := b.Metadata["state"]
	claimRaw := b.Metadata["pending_create_claim"]
	return PendingCreateLease{
		SessionID:           b.ID,
		Closed:              strings.TrimSpace(b.Status) == "closed",
		InstanceToken:       strings.TrimSpace(b.Metadata["instance_token"]),
		Generation:          strings.TrimSpace(b.Metadata["generation"]),
		Claim:               strings.TrimSpace(claimRaw) == "true",
		ClaimRaw:            claimRaw,
		ClaimStartedAtRaw:   b.Metadata["pending_create_started_at"],
		AttemptStartedAtRaw: b.Metadata["last_woke_at"],
		StateRaw:            stateRaw,
		State:               State(strings.TrimSpace(stateRaw)),
		CreatedAt:           b.CreatedAt,
	}
}

// LeaseFromInfo projects the pending-create tuple off a typed Info snapshot.
// For every bead b, LeaseFromBead(b) equals
// LeaseFromInfo(InfoFromPersistedBead(b)); see TestLeaseConstructorParity.
func LeaseFromInfo(i Info) PendingCreateLease {
	return PendingCreateLease{
		SessionID:           i.ID,
		Closed:              i.Closed,
		InstanceToken:       strings.TrimSpace(i.InstanceToken),
		Generation:          strings.TrimSpace(i.Generation),
		Claim:               i.PendingCreateClaim,
		ClaimRaw:            i.PendingCreateClaimMetadata,
		ClaimStartedAtRaw:   i.PendingCreateStartedAt,
		AttemptStartedAtRaw: i.LastWokeAt,
		StateRaw:            i.MetadataState,
		State:               State(strings.TrimSpace(i.MetadataState)),
		CreatedAt:           i.CreatedAt,
	}
}

// LeaseCommitVerdict is what the async-start commit gate returns when an
// in-flight start result meets the current bead. Exactly three outcomes; the
// two mutually-exclusive boolean helpers it replaces
// (asyncStartSessionStillCurrent / asyncStartStaleRuntimeCleanupAllowed) fuse
// into this enum.
type LeaseCommitVerdict int

const (
	// LeaseCommit: the result is still current — commit it against the
	// current bead.
	LeaseCommit LeaseCommitVerdict = iota
	// LeaseDiscardStopRuntime: the result is stale — discard it and (subject
	// to the separate runningSessionMatchesPendingCreate runtime probe) stop
	// the spawned runtime.
	LeaseDiscardStopRuntime
	// LeaseDiscardKeepRuntime: the result is stale but the runtime may belong
	// to a live/committed owner — discard the result and leave the runtime
	// alone. This is the pane-safety arm (#2073). The pure state gate never
	// yields it; it exists for the refresh-level wrapper where a store Get
	// fails.
	LeaseDiscardKeepRuntime
)

// stateConfirmsPendingStart reports whether a session in the given state
// should transition to "active" after a successful runtime spawn. Empty,
// "start-pending", "creating", "asleep", and "drained" all indicate the
// session was pending a spawn; "awake" is treated as equivalent to "active"
// and intentionally not restamped; every other state is left alone. This is
// the single home for that frozen state list (invariant 16).
func stateConfirmsPendingStart(s State) bool {
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
	if stateConfirmsPendingStart(current.State) {
		return LeaseCommit
	}
	return LeaseDiscardStopRuntime
}

// ConfirmDecision carries the commit-time state decisions that
// CommitStartedPatch consumes. Returning decisions (not a patch) keeps the
// commit write a single atomic ApplyPatch batch (#3849).
type ConfirmDecision struct {
	ConfirmState            bool
	ClearPendingCreateClaim bool
	StartsAwakeInterval     bool
}

// Confirm reports the commit-time decisions for this lease's state. When
// allowAwake is set (the recover-running path), an already-awake runtime
// confirms its state but does NOT open a fresh awake interval — so awake
// contributes to ConfirmState only, never to StartsAwakeInterval (#3849).
func (l PendingCreateLease) Confirm(allowAwake bool) ConfirmDecision {
	confirms := stateConfirmsPendingStart(l.State)
	return ConfirmDecision{
		ConfirmState:            confirms || (allowAwake && l.State == StateAwake),
		ClearPendingCreateClaim: l.Claim,
		StartsAwakeInterval:     confirms,
	}
}

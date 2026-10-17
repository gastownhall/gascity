package events

import (
	"encoding/json"
	"time"
)

// Domain payload types shared across packages. Payloads specific to one
// package live with their emitter (see internal/api/event_payloads.go and
// internal/extmsg/events.go).

// BeadWorktreeReapedPayload is the typed payload for bead.worktree.reaped
// events. Emitted when the worktree reaper successfully removes a merged
// worktree and its branch after a bead is closed.
type BeadWorktreeReapedPayload struct {
	BeadID string `json:"bead_id"`
	Path   string `json:"path"`
	Rig    string `json:"rig"`
	Branch string `json:"branch"`
}

// IsEventPayload marks BeadWorktreeReapedPayload as an events.Payload variant.
func (BeadWorktreeReapedPayload) IsEventPayload() {}

// BeadWorktreeReapSkippedPayload is the typed payload for
// bead.worktree.reap_skipped events. Emitted when the worktree reaper
// decides not to remove a worktree (e.g., unmerged changes, open bead).
type BeadWorktreeReapSkippedPayload struct {
	BeadID string `json:"bead_id"`
	Path   string `json:"path"`
	Rig    string `json:"rig"`
	Reason string `json:"reason"`
}

// IsEventPayload marks BeadWorktreeReapSkippedPayload as an events.Payload variant.
func (BeadWorktreeReapSkippedPayload) IsEventPayload() {}

// BeadClaimRejectedPayload is the typed payload for bead.claim_rejected events
// (ADR-0009). Emitted when AttemptedClaimant tries to claim BeadID while it is
// already live-claimed by ExistingClaimant; the second claim is rejected as an
// idempotent no-op. The payload makes the lost-claim race observable for
// eval/audit (RCA gc-typpc: one bead concurrently claimed by four workers).
type BeadClaimRejectedPayload struct {
	BeadID            string `json:"bead_id"`
	ExistingClaimant  string `json:"existing_claimant"`
	AttemptedClaimant string `json:"attempted_claimant"`
}

// IsEventPayload marks BeadClaimRejectedPayload as an events.Payload variant.
func (BeadClaimRejectedPayload) IsEventPayload() {}

// BeadClaimReleasedPayload is the typed payload for bead.claim_released events.
// Emitted when Assignee gives back a claim it had already won on BeadID because
// the claim could not reach a live consumer. Reason names which unwind ran —
// see the BeadClaimReleased constant for the two shapes, and for why this event
// following an execution.step_started on the same subject is a compensation
// pair that a consumer must NOT read as a step still in flight.
type BeadClaimReleasedPayload struct {
	BeadID   string `json:"bead_id"`
	Assignee string `json:"assignee"`
	Reason   string `json:"reason"`
}

// IsEventPayload marks BeadClaimReleasedPayload as an events.Payload variant.
func (BeadClaimReleasedPayload) IsEventPayload() {}

// BeadClaimFailedPayload is the typed payload for bead.claim_failed events. The
// envelope's Subject is the routed bead a seat of Template could not claim.
// SessionID and RuntimeEpoch name the seat: a session bead keeps its id across
// restarts, and the epoch advances on each one, so together they tell a seat
// retrying from a new seat. Error is the claim write's own error text without a
// server request id, bounded in length: the form the claim-loop guard compares
// to tell one failure repeating from unrelated ones. The envelope's Message
// keeps the request id, an operator's handle on the server's log line.
type BeadClaimFailedPayload struct {
	Template     string `json:"template"`
	SessionID    string `json:"session_id,omitempty"`
	RuntimeEpoch int64  `json:"runtime_epoch,omitempty"`
	Error        string `json:"error"`
}

// IsEventPayload marks BeadClaimFailedPayload as an events.Payload variant.
func (BeadClaimFailedPayload) IsEventPayload() {}

// BeadClaimLoopHeldPayload is the typed payload for bead.claim_loop_held
// events. The envelope's Subject is the held bead. Failures is how many
// distinct seats of Template have failed to claim it in this streak; Error (a
// server request id removed) is the error they repeated when the hold began,
// or the probe's when a failed probe extended it. HeldUntil is when the next
// probe seat may be spawned for the bead if nothing changes before then.
type BeadClaimLoopHeldPayload struct {
	Template  string    `json:"template"`
	Error     string    `json:"error"`
	Failures  int       `json:"failures"`
	HeldUntil time.Time `json:"held_until"`
}

// IsEventPayload marks BeadClaimLoopHeldPayload as an events.Payload variant.
func (BeadClaimLoopHeldPayload) IsEventPayload() {}

// HookClaimReclaimedStalePayload is the typed payload for hook.claim.reclaimed_stale
// events (ga-7rj87d). Emitted when a scoped `bd reclaim --id BeadID` recovers a
// candidate from PreviousOwner's stale lease and the retried claim in the same
// hook cycle wins it for NewAssignee.
type HookClaimReclaimedStalePayload struct {
	BeadID        string `json:"bead_id"`
	PreviousOwner string `json:"previous_owner"`
	NewAssignee   string `json:"new_assignee"`
}

// IsEventPayload marks HookClaimReclaimedStalePayload as an events.Payload variant.
func (HookClaimReclaimedStalePayload) IsEventPayload() {}

func init() {
	RegisterPayload(BeadWorktreeReaped, BeadWorktreeReapedPayload{})
	RegisterPayload(BeadWorktreeReapSkipped, BeadWorktreeReapSkippedPayload{})
	RegisterPayload(BeadClaimRejected, BeadClaimRejectedPayload{})
	RegisterPayload(BeadClaimReleased, BeadClaimReleasedPayload{})
	RegisterPayload(BeadClaimFailed, BeadClaimFailedPayload{})
	RegisterPayload(BeadClaimLoopHeld, BeadClaimLoopHeldPayload{})
	RegisterPayload(HookClaimReclaimedStale, HookClaimReclaimedStalePayload{})
	RegisterPayload(BeadsBlockedRecomputed, BlockedRecomputedPayload{})
}

// SessionResetStalledPayload is the typed payload for
// session.reset_stalled events. It identifies the session whose reset
// completion has stalled and the reset timestamp used to compute the
// elapsed diagnostic threshold.
type SessionResetStalledPayload struct {
	SessionName      string `json:"session_name"`
	Template         string `json:"template"`
	ResetCommittedAt string `json:"reset_committed_at"`
	ElapsedSeconds   int    `json:"elapsed_s"`
}

// IsEventPayload marks SessionResetStalledPayload as an events.Payload variant.
func (SessionResetStalledPayload) IsEventPayload() {}

// SessionResetStalledPayloadJSON builds the JSON wire form for attachment to
// an Event.Payload field.
func SessionResetStalledPayloadJSON(sessionName, template, resetCommittedAt string, elapsedSeconds int) json.RawMessage {
	b, _ := json.Marshal(SessionResetStalledPayload{
		SessionName:      sessionName,
		Template:         template,
		ResetCommittedAt: resetCommittedAt,
		ElapsedSeconds:   elapsedSeconds,
	})
	return b
}

// Demand/claim divergence classifications. They are the whole point of the
// event: a demand-spawned seat that finds nothing is EITHER correct pull (a
// sibling took the row first) or a broken invariant (the row is still sitting
// there, claimable, and the two readers disagreed about it). Counting them
// together would hide the second inside the first.
const (
	// DemandClaimBenign: the trigger row is gone from the claimable set —
	// claimed by someone else or closed. Expected, healthy pull.
	DemandClaimBenign = "benign"
	// DemandClaimDivergence: the trigger row is still open, unassigned and
	// route-matching. The controller counted work the worker's own read did not
	// serve; this is the agreement invariant breaking.
	DemandClaimDivergence = "divergence"
	// DemandClaimBlocked: the trigger row is still open, unassigned and
	// route-matching, and the only thing marking it non-claimable is a blocking
	// dependency — re-derived from the row's LIVE deps, not read off bd's
	// denormalized is_blocked projection, which can lag a just-closed blocker and
	// is absent from the payloads these reads actually return. No worker could
	// have claimed the row, so it is NOT counted as a clean divergence (that
	// metric must stay the agreement signal) — but it is not folded into benign
	// either: a routed row the controller keeps counting while nobody can take it
	// is worth seeing on its own.
	DemandClaimBlocked = "blocked"
	// DemandClaimUnknown: the classification read could not be made (no trigger
	// recorded, or the row could not be read). Never counted as either.
	DemandClaimUnknown = "unknown"
)

// SessionDemandClaimDivergencePayload is the typed payload for
// session.demand_claim_divergence. It names the seat, why it drained, and what
// the row that justified spawning it looked like at that moment.
//
// TriggerBeadID is DIAGNOSTIC ONLY. The controller does not assign beads to
// seats — the pool is pull — so nothing on the claim path may read it to decide
// what to claim; it appears here so an operator can go look at the row the
// demand evidence came from.
type SessionDemandClaimDivergencePayload struct {
	SessionID string `json:"session_id"`
	Template  string `json:"template"`
	// DrainReason is the hook result's reason, so a consumer can tell this
	// counter apart from other drain shapes without joining another stream.
	DrainReason string `json:"drain_reason"`
	// TriggerBeadID is the row the controller counted, or empty when the seat
	// carried no trigger record.
	TriggerBeadID string `json:"trigger_bead_id,omitempty"`
	// TriggerStatusAtDrain is the row's observed status at drain time
	// ("open"/"in_progress"/"closed"), "unreadable" when the classification read
	// failed, or empty when there was no row to read.
	TriggerStatusAtDrain string `json:"trigger_status_at_drain,omitempty"`
	// Classification is the verdict: benign, divergence, blocked, or unknown. It
	// is carried rather than left to be re-derived, because the divergence count
	// IS the rollout metric for the agreement fix.
	Classification string `json:"classification"`
}

// IsEventPayload marks SessionDemandClaimDivergencePayload as an events.Payload variant.
func (SessionDemandClaimDivergencePayload) IsEventPayload() {}

func init() {
	RegisterPayload(SessionDemandClaimDivergence, SessionDemandClaimDivergencePayload{})
}

// BlockedRecomputedPayload is the typed payload for beads.blocked.recomputed
// events: gc start ran `bd recompute-blocked` over one scope because the scope
// had not yet been repaired under the running bd version.
type BlockedRecomputedPayload struct {
	// Scope names the repaired scope: "city" or "rig/<name>".
	Scope string `json:"scope"`
	// RowsCorrected is bd's count of is_blocked values the recompute changed.
	// Zero means the scope was already consistent.
	RowsCorrected int `json:"rows_corrected"`
	// BDVersion is the bd version that ran the recompute and is now recorded
	// as the scope's repair marker.
	BDVersion string `json:"bd_version"`
}

// IsEventPayload marks BlockedRecomputedPayload as an events.Payload variant.
func (BlockedRecomputedPayload) IsEventPayload() {}

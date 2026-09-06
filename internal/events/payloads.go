package events

import "encoding/json"

// Domain payload types shared across packages. Payloads specific to one
// package live with their emitter (see internal/api/event_payloads.go and
// internal/extmsg/events.go); this file holds payload shapes that are
// used by multiple callers — today, the supervisor's Dolt maintenance
// loop and its CLI/API projections (beads ga-e3s, ga-zn8, ga-p5n).

// StoreMaintenanceDonePayload is the typed payload for
// gc.store.maintenance.done events. Emitted after a successful
// maintenance cycle (backup snapshot + CALL DOLT_GC + smoke test).
type StoreMaintenanceDonePayload struct {
	DurationSeconds float64 `json:"duration_s"`
	BeforeBytes     int64   `json:"before_bytes"`
	AfterBytes      int64   `json:"after_bytes"`
	SnapshotPath    string  `json:"snapshot_path"`
}

// IsEventPayload marks StoreMaintenanceDonePayload as an events.Payload variant.
func (StoreMaintenanceDonePayload) IsEventPayload() {}

// StoreMaintenanceFailedPayload is the typed payload for
// gc.store.maintenance.failed events. Emitted when a maintenance stage
// returns an error. Stage names the failing phase ("backup" | "gc" |
// "smoke-test" | "prune"); ErrorMsg carries the human-readable cause;
// SnapshotPath is populated when the backup stage completed before a
// later stage failed (so operators can recover from the snapshot).
type StoreMaintenanceFailedPayload struct {
	Stage           string  `json:"stage"`
	ErrorMsg        string  `json:"error_msg"`
	SnapshotPath    string  `json:"snapshot_path,omitempty"`
	DurationSeconds float64 `json:"duration_s"`
}

// IsEventPayload marks StoreMaintenanceFailedPayload as an events.Payload variant.
func (StoreMaintenanceFailedPayload) IsEventPayload() {}

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

func init() {
	RegisterPayload(BeadWorktreeReaped, BeadWorktreeReapedPayload{})
	RegisterPayload(BeadWorktreeReapSkipped, BeadWorktreeReapSkippedPayload{})
	RegisterPayload(BeadClaimRejected, BeadClaimRejectedPayload{})
	RegisterPayload(BeadClaimReleased, BeadClaimReleasedPayload{})
}

// StoreDiskWarnPayload is the typed payload for gc.store.disk_warn events.
// Emitted before CALL DOLT_GC when free space is below GC_DOLT_WARN_FREE_BYTES
// but above GC_DOLT_MIN_FREE_BYTES; the GC proceeds.
type StoreDiskWarnPayload struct {
	FreeBytes  int64  `json:"free_bytes"`
	WarnBytes  int64  `json:"warn_bytes"`
	FloorBytes int64  `json:"floor_bytes"`
	DataDir    string `json:"data_dir"`
}

// IsEventPayload marks StoreDiskWarnPayload as an events.Payload variant.
func (StoreDiskWarnPayload) IsEventPayload() {}

// StoreDiskCriticalPayload is the typed payload for gc.store.disk_critical
// events. Emitted before CALL DOLT_GC when free space is below
// GC_DOLT_MIN_FREE_BYTES; the GC is skipped to avoid growing the store.
type StoreDiskCriticalPayload struct {
	FreeBytes  int64  `json:"free_bytes"`
	FloorBytes int64  `json:"floor_bytes"`
	DataDir    string `json:"data_dir"`
}

// IsEventPayload marks StoreDiskCriticalPayload as an events.Payload variant.
func (StoreDiskCriticalPayload) IsEventPayload() {}

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
	// Classification is the verdict: benign, divergence, or unknown. It is
	// carried rather than left to be re-derived, because the divergence count IS
	// the rollout metric for the agreement fix.
	Classification string `json:"classification"`
}

// IsEventPayload marks SessionDemandClaimDivergencePayload as an events.Payload variant.
func (SessionDemandClaimDivergencePayload) IsEventPayload() {}

func init() {
	RegisterPayload(SessionDemandClaimDivergence, SessionDemandClaimDivergencePayload{})
}

package session

import (
	"fmt"
	"time"
)

var freshWakeConversationResetKeys = []string{
	"session_key",
	"started_config_hash",
	"started_live_hash",
	"live_hash",
	startupDialogVerifiedKey,
}

// MetadataPatch is an atomic set of metadata key updates for one lifecycle
// transition. Empty values intentionally clear metadata keys in existing store
// implementations.
type MetadataPatch map[string]string

// FreshWakeConversationResetKeys returns the metadata fields that define
// provider-conversation identity and are reset when a wake or restart must
// start fresh.
func FreshWakeConversationResetKeys() []string {
	return append([]string(nil), freshWakeConversationResetKeys...)
}

// Apply returns a merged copy of meta with the patch applied.
func (p MetadataPatch) Apply(meta map[string]string) map[string]string {
	merged := make(map[string]string, len(meta)+len(p))
	for k, v := range meta {
		merged[k] = v
	}
	for k, v := range p {
		merged[k] = v
	}
	return merged
}

// applyFreshWakeConversationReset keeps all fresh-wake paths aligned on the
// same provider-identity field set. PreWakePatch remains the authoritative
// final reset immediately before command preparation; drain/config-drift paths
// reuse the same cleared fields so fresh-wake semantics do not drift.
func applyFreshWakeConversationReset(patch MetadataPatch) {
	patch["started_config_hash"] = ""
	patch["started_live_hash"] = ""
	patch["live_hash"] = ""
	patch[startupDialogVerifiedKey] = ""
}

func pendingCreateStartedAt(now time.Time) string {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.UTC().Format(time.RFC3339)
}

// RequestWakePatch records a controller-owned one-shot create claim.
func RequestWakePatch(reason string, now time.Time) MetadataPatch {
	return MetadataPatch{
		"state":                     string(StateCreating),
		"state_reason":              reason,
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAt(now),
		"held_until":                "",
		"quarantined_until":         "",
		"sleep_reason":              "",
		"wait_hold":                 "",
		"sleep_intent":              "",
		"wake_attempts":             "0",
		"churn_count":               "0",
	}
}

// PreWakePatchInput records the metadata transition for a concrete runtime
// wake attempt. The caller computes generation, token, and continuation epoch;
// the patch owns keeping all persisted lifecycle fields consistent for that
// transition.
type PreWakePatchInput struct {
	Generation        int
	InstanceToken     string
	ContinuationEpoch int
	Now               time.Time
	SleepReason       string
	FreshWake         bool
}

// PreWakePatch records the metadata transition for a concrete runtime wake
// attempt.
func PreWakePatch(input PreWakePatchInput) MetadataPatch {
	patch := MetadataPatch{
		"instance_token":             input.InstanceToken,
		"continuation_epoch":         fmt.Sprintf("%d", input.ContinuationEpoch),
		"continuation_reset_pending": "",
		"detached_at":                "",
		"last_woke_at":               input.Now.UTC().Format(time.RFC3339),
		"sleep_reason":               input.SleepReason,
		"sleep_intent":               "",
		"generation":                 fmt.Sprintf("%d", input.Generation),
	}
	if input.FreshWake {
		patch["session_key"] = ""
		applyFreshWakeConversationReset(patch)
	}
	return patch
}

// ClearWakeBlockersPatch clears advisory blockers so a dormant session may be
// selected by the normal wake path.
func ClearWakeBlockersPatch(state State, sleepReason string) MetadataPatch {
	patch := MetadataPatch{
		"held_until":        "",
		"quarantined_until": "",
		"wait_hold":         "",
		"sleep_intent":      "",
		"wake_attempts":     "0",
		"churn_count":       "0",
	}
	switch state {
	case StateSuspended, StateDrained:
		patch["state"] = string(StateAsleep)
	}
	switch sleepReason {
	case "user-hold", "wait-hold", "quarantine", "context-churn", "rate_limit", string(StateDrained):
		patch["sleep_reason"] = ""
	}
	return patch
}

// ClearExpiredHoldPatch clears an expired user hold and drops the displayed
// hold reason only when that reason came from the expired timer.
func ClearExpiredHoldPatch(sleepReason string) MetadataPatch {
	patch := MetadataPatch{
		"held_until": "",
	}
	if sleepReason == "user-hold" {
		patch["sleep_reason"] = ""
	}
	return patch
}

// ClearExpiredQuarantinePatch clears an expired quarantine-like timer and
// resets retry counters associated with that blocker.
func ClearExpiredQuarantinePatch(sleepReason string) MetadataPatch {
	patch := MetadataPatch{
		"quarantined_until": "",
		"wake_attempts":     "0",
		"churn_count":       "0",
	}
	switch sleepReason {
	case "quarantine", "context-churn", "rate_limit":
		patch["sleep_reason"] = ""
	}
	return patch
}

// ConfirmStartedPatch records a confirmed runtime start. The timestamp pins
// the "creation_complete" transition so downstream readers (e.g. the pool
// bead sweep) can distinguish a just-committed start from a long-stable
// bead whose last_woke_at was later cleared by crash/churn recovery.
func ConfirmStartedPatch(now time.Time) MetadataPatch {
	return MetadataPatch{
		"state":                     string(StateActive),
		"state_reason":              "creation_complete",
		"creation_complete_at":      now.UTC().Format(time.RFC3339),
		"pending_create_claim":      "",
		"pending_create_started_at": "",
		"sleep_reason":              "",
	}
}

// CommitStartedPatchInput describes metadata persisted after a runtime start
// has completed. Hashes describe the runtime configuration that actually
// launched; ConfirmState controls whether this start should stamp lifecycle
// state active. Now is used to stamp creation_complete_at whenever it is
// non-zero (independent of ConfirmState, so the recovery path that commits
// a fresh start on an already-active bead still refreshes the sweep's
// post-create marker). ClearPendingCreateClaim folds the
// pending_create_claim clear into the same atomic batch so downstream
// readers (e.g. the pool bead sweep) never observe a transient state
// where the claim is gone but the post-create marker hasn't landed yet.
type CommitStartedPatchInput struct {
	CoreHash                string
	LiveHash                string
	CoreBreakdown           string
	ConfirmState            bool
	ClearSleepReason        bool
	ClearPendingCreateClaim bool
	Now                     time.Time
}

// CommitStartedPatch records a successful runtime start atomically with the
// configuration hashes that future drift checks use.
func CommitStartedPatch(input CommitStartedPatchInput) MetadataPatch {
	patch := MetadataPatch{
		"started_config_hash": input.CoreHash,
		"live_hash":           input.LiveHash,
		"started_live_hash":   input.LiveHash,
	}
	if input.CoreBreakdown != "" {
		patch["core_hash_breakdown"] = input.CoreBreakdown
	}
	if input.ConfirmState {
		patch["state"] = string(StateActive)
		patch["state_reason"] = "creation_complete"
	}
	// creation_complete_at tracks when the runtime was last confirmed started.
	// Stamp it whenever Now is non-zero — the ConfirmState path marks the
	// fresh transition from creating/asleep; the recovery path (already-
	// active bead with pending_create_claim=true) re-confirms an existing
	// start, so it needs the same marker so the post-create sweep guard
	// doesn't treat the healed bead as stale on subsequent ticks.
	if !input.Now.IsZero() {
		patch["creation_complete_at"] = input.Now.UTC().Format(time.RFC3339)
	}
	if input.ClearSleepReason {
		patch["sleep_reason"] = ""
	}
	if input.ClearPendingCreateClaim {
		patch["pending_create_claim"] = ""
		patch["pending_create_started_at"] = ""
	}
	return patch
}

// BeginDrainPatch transitions a live session into draining.
func BeginDrainPatch(now time.Time, reason string) MetadataPatch {
	return MetadataPatch{
		"state":        string(StateDraining),
		"state_reason": reason,
		"drain_at":     now.UTC().Format(time.RFC3339),
	}
}

// SleepPatch records a non-terminal sleep/drain result.
func SleepPatch(now time.Time, reason string) MetadataPatch {
	return MetadataPatch{
		"state":                     string(StateAsleep),
		"sleep_reason":              reason,
		"last_woke_at":              "",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
		"sleep_intent":              "",
		"slept_at":                  now.UTC().Format(time.RFC3339),
	}
}

// AcknowledgeDrainPatch records an agent-acknowledged drain. Drained is a
// compatibility state distinct from ordinary asleep: demand alone does not
// reselect it, but explicit attach or work can.
func AcknowledgeDrainPatch(freshWake bool) MetadataPatch {
	patch := MetadataPatch{
		"state":                     string(StateDrained),
		"last_woke_at":              "",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}
	if freshWake {
		patch["session_key"] = ""
		applyFreshWakeConversationReset(patch)
		patch["continuation_reset_pending"] = "true"
	}
	return patch
}

// CompleteDrainPatch records a completed controller drain as ordinary asleep.
func CompleteDrainPatch(now time.Time, reason string, freshWake bool) MetadataPatch {
	patch := SleepPatch(now, reason)
	if freshWake {
		patch["session_key"] = ""
		applyFreshWakeConversationReset(patch)
		patch["continuation_reset_pending"] = "true"
	}
	return patch
}

// RestartRequestPatch records a controller handoff to a fresh provider
// conversation. It intentionally clears only the fields that force the next
// wake onto a first-start path; started_live_hash/live_hash remain intact until
// the next successful start rewrites them so restart-in-flight drift readers do
// not observe an empty-hash backfill state. The caller owns stopping any
// currently running runtime.
func RestartRequestPatch(sessionKey string) MetadataPatch {
	patch := MetadataPatch{
		"restart_requested":          "",
		"started_config_hash":        "",
		"continuation_reset_pending": "true",
		"last_woke_at":               "",
		"pending_create_claim":       "",
		"pending_create_started_at":  "",
	}
	if sessionKey != "" {
		patch["session_key"] = sessionKey
	}
	return patch
}

// ConfigDriftResetPatch records an in-place named-session repair after core
// config drift. Creating claims a new runtime start; asleep stays dormant
// until the next normal wake reason.
func ConfigDriftResetPatch(nextState State, sessionKey string, now time.Time) MetadataPatch {
	patch := MetadataPatch{
		"state":                      string(nextState),
		"last_woke_at":               "",
		"restart_requested":          "",
		"continuation_reset_pending": "true",
		"pending_create_claim":       "",
		"pending_create_started_at":  "",
	}
	applyFreshWakeConversationReset(patch)
	if nextState == StateCreating {
		patch["pending_create_claim"] = "true"
		patch["pending_create_started_at"] = pendingCreateStartedAt(now)
	}
	if sessionKey != "" {
		patch["session_key"] = sessionKey
	}
	return patch
}

// ArchivePatch transitions a retired session into archived history.
func ArchivePatch(now time.Time, reason string, continuityEligible bool) MetadataPatch {
	continuity := "false"
	if continuityEligible {
		continuity = "true"
	}
	return MetadataPatch{
		"state":                     string(StateArchived),
		"state_reason":              reason,
		"archived_at":               now.UTC().Format(time.RFC3339),
		"continuity_eligible":       continuity,
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}
}

// ClosePatch records terminal close metadata before the bead status is closed.
func ClosePatch(now time.Time, reason string) MetadataPatch {
	ts := now.UTC().Format(time.RFC3339)
	return MetadataPatch{
		"state":        reason,
		"close_reason": reason,
		"closed_at":    ts,
		"synced_at":    ts,
	}
}

// RetireNamedSessionPatch archives a named-session bead without closing it so
// historical references can be reassigned while canonical identifiers are freed.
func RetireNamedSessionPatch(now time.Time, reason, identity string) MetadataPatch {
	patch := ArchivePatch(now, reason, false)
	patch["alias"] = ""
	patch["session_name"] = ""
	patch["session_name_explicit"] = ""
	patch["synced_at"] = now.UTC().Format(time.RFC3339)
	patch["held_until"] = ""
	patch["quarantined_until"] = ""
	patch["wait_hold"] = ""
	patch["sleep_intent"] = ""
	patch["sleep_reason"] = ""
	if identity != "" {
		patch["retired_named_identity"] = identity
	}
	return patch
}

// QuarantinePatch records a crash-loop quarantine.
func QuarantinePatch(until time.Time, cycle int) MetadataPatch {
	return MetadataPatch{
		"state":             string(StateQuarantined),
		"state_reason":      "crash-loop",
		"quarantined_until": until.UTC().Format(time.RFC3339),
		"quarantine_cycle":  fmt.Sprintf("%d", cycle),
		"last_woke_at":      "",
	}
}

// ReactivatePatch clears quarantine/archive metadata and makes the session
// eligible for normal wake machinery when continuityEligible is true. It does
// not claim that a runtime is already alive.
func ReactivatePatch(continuityEligible bool) MetadataPatch {
	continuity := "false"
	if continuityEligible {
		continuity = "true"
	}
	return MetadataPatch{
		"state":                     string(StateAsleep),
		"state_reason":              "reactivated",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
		"continuity_eligible":       continuity,
		"quarantined_until":         "",
		"crash_count":               "0",
		"archived_at":               "",
	}
}

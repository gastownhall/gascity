package session

import (
	"fmt"
	"time"
)

// MetadataPatch is an atomic set of metadata key updates for one lifecycle
// transition. Empty values intentionally clear metadata keys in existing store
// implementations.
type MetadataPatch map[string]string

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

// RequestWakePatch records a controller-owned one-shot create claim.
func RequestWakePatch(reason string) MetadataPatch {
	return MetadataPatch{
		"state":                string(StateCreating),
		"state_reason":         reason,
		"pending_create_claim": "true",
		"held_until":           "",
		"quarantined_until":    "",
		"sleep_reason":         "",
		"wait_hold":            "",
		"sleep_intent":         "",
		"wake_attempts":        "0",
		"churn_count":          "0",
	}
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
	case "user-hold", "wait-hold", "quarantine", "context-churn", string(StateDrained):
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

// ClearExpiredQuarantinePatch clears an expired quarantine or context churn
// timer and resets retry counters associated with that blocker.
func ClearExpiredQuarantinePatch(sleepReason string) MetadataPatch {
	patch := MetadataPatch{
		"quarantined_until": "",
		"wake_attempts":     "0",
		"churn_count":       "0",
	}
	switch sleepReason {
	case "quarantine", "context-churn":
		patch["sleep_reason"] = ""
	}
	return patch
}

// ConfirmStartedPatch records a confirmed runtime start.
func ConfirmStartedPatch() MetadataPatch {
	return MetadataPatch{
		"state":                string(StateActive),
		"state_reason":         "creation_complete",
		"pending_create_claim": "",
		"sleep_reason":         "",
	}
}

// CommitStartedPatchInput describes metadata persisted after a runtime start
// has completed. Hashes describe the runtime configuration that actually
// launched; ConfirmState controls whether this start should stamp lifecycle
// state active.
type CommitStartedPatchInput struct {
	CoreHash         string
	LiveHash         string
	CoreBreakdown    string
	ConfirmState     bool
	ClearSleepReason bool
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
	if input.ClearSleepReason {
		patch["sleep_reason"] = ""
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
		"state":                string(StateAsleep),
		"sleep_reason":         reason,
		"last_woke_at":         "",
		"pending_create_claim": "",
		"sleep_intent":         "",
		"slept_at":             now.UTC().Format(time.RFC3339),
	}
}

// AcknowledgeDrainPatch records an agent-acknowledged drain. Drained is a
// compatibility state distinct from ordinary asleep: demand alone does not
// reselect it, but explicit attach or work can.
func AcknowledgeDrainPatch(freshWake bool) MetadataPatch {
	patch := MetadataPatch{
		"state":                string(StateDrained),
		"last_woke_at":         "",
		"pending_create_claim": "",
	}
	if freshWake {
		patch["session_key"] = ""
		patch["started_config_hash"] = ""
		patch["continuation_reset_pending"] = "true"
	}
	return patch
}

// CompleteDrainPatch records a completed controller drain as ordinary asleep.
func CompleteDrainPatch(now time.Time, reason string, freshWake bool) MetadataPatch {
	patch := SleepPatch(now, reason)
	if freshWake {
		patch["session_key"] = ""
		patch["started_config_hash"] = ""
		patch["continuation_reset_pending"] = "true"
	}
	return patch
}

// RestartRequestPatch records a controller handoff to a fresh provider
// conversation. The caller owns stopping any currently running runtime.
func RestartRequestPatch(sessionKey string) MetadataPatch {
	patch := MetadataPatch{
		"restart_requested":          "",
		"started_config_hash":        "",
		"continuation_reset_pending": "true",
		"last_woke_at":               "",
		"pending_create_claim":       "",
	}
	if sessionKey != "" {
		patch["session_key"] = sessionKey
	}
	return patch
}

// ConfigDriftResetPatch records an in-place named-session repair after core
// config drift. Creating claims a new runtime start; asleep stays dormant
// until the next normal wake reason.
func ConfigDriftResetPatch(nextState State, sessionKey string) MetadataPatch {
	patch := MetadataPatch{
		"state":                      string(nextState),
		"started_config_hash":        "",
		"started_live_hash":          "",
		"live_hash":                  "",
		"last_woke_at":               "",
		"restart_requested":          "",
		"continuation_reset_pending": "true",
		"pending_create_claim":       "",
	}
	if nextState == StateCreating {
		patch["pending_create_claim"] = "true"
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
		"state":                string(StateArchived),
		"state_reason":         reason,
		"archived_at":          now.UTC().Format(time.RFC3339),
		"continuity_eligible":  continuity,
		"pending_create_claim": "",
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
		"state":                string(StateAsleep),
		"state_reason":         "reactivated",
		"pending_create_claim": "",
		"continuity_eligible":  continuity,
		"quarantined_until":    "",
		"crash_count":          "0",
		"archived_at":          "",
	}
}

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

// ConfirmStartedPatch records a confirmed runtime start.
func ConfirmStartedPatch() MetadataPatch {
	return MetadataPatch{
		"state":                string(StateActive),
		"state_reason":         "creation_complete",
		"pending_create_claim": "",
		"sleep_reason":         "",
	}
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

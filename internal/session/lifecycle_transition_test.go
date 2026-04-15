package session

import (
	"reflect"
	"testing"
	"time"
)

func TestLifecycleTransitionPatchesSetCompleteMetadata(t *testing.T) {
	now := time.Date(2026, 4, 15, 13, 0, 0, 0, time.UTC)
	later := now.Add(5 * time.Minute)

	tests := []struct {
		name  string
		patch MetadataPatch
		want  MetadataPatch
	}{
		{
			name:  "request wake",
			patch: RequestWakePatch("explicit"),
			want: MetadataPatch{
				"state":                string(StateCreating),
				"state_reason":         "explicit",
				"pending_create_claim": "true",
				"held_until":           "",
				"quarantined_until":    "",
				"sleep_reason":         "",
				"wait_hold":            "",
				"sleep_intent":         "",
				"wake_attempts":        "0",
				"churn_count":          "0",
			},
		},
		{
			name:  "confirm started",
			patch: ConfirmStartedPatch(),
			want: MetadataPatch{
				"state":                string(StateActive),
				"state_reason":         "creation_complete",
				"pending_create_claim": "",
				"sleep_reason":         "",
			},
		},
		{
			name:  "begin drain",
			patch: BeginDrainPatch(now, "config-drift"),
			want: MetadataPatch{
				"state":        string(StateDraining),
				"state_reason": "config-drift",
				"drain_at":     now.Format(time.RFC3339),
			},
		},
		{
			name:  "sleep",
			patch: SleepPatch(now, "idle-timeout"),
			want: MetadataPatch{
				"state":                string(StateAsleep),
				"sleep_reason":         "idle-timeout",
				"last_woke_at":         "",
				"pending_create_claim": "",
				"sleep_intent":         "",
				"slept_at":             now.Format(time.RFC3339),
			},
		},
		{
			name:  "archive continuity eligible",
			patch: ArchivePatch(now, "drain_complete", true),
			want: MetadataPatch{
				"state":                string(StateArchived),
				"state_reason":         "drain_complete",
				"archived_at":          now.Format(time.RFC3339),
				"continuity_eligible":  "true",
				"pending_create_claim": "",
			},
		},
		{
			name:  "archive historical only",
			patch: ArchivePatch(now, "duplicate-repair", false),
			want: MetadataPatch{
				"state":                string(StateArchived),
				"state_reason":         "duplicate-repair",
				"archived_at":          now.Format(time.RFC3339),
				"continuity_eligible":  "false",
				"pending_create_claim": "",
			},
		},
		{
			name:  "quarantine",
			patch: QuarantinePatch(later, 3),
			want: MetadataPatch{
				"state":             string(StateQuarantined),
				"state_reason":      "crash-loop",
				"quarantined_until": later.Format(time.RFC3339),
				"quarantine_cycle":  "3",
				"last_woke_at":      "",
			},
		},
		{
			name:  "reactivate continuity eligible",
			patch: ReactivatePatch(true),
			want: MetadataPatch{
				"state":                string(StateAsleep),
				"state_reason":         "reactivated",
				"pending_create_claim": "",
				"continuity_eligible":  "true",
				"quarantined_until":    "",
				"crash_count":          "0",
				"archived_at":          "",
			},
		},
		{
			name:  "reactivate historical only",
			patch: ReactivatePatch(false),
			want: MetadataPatch{
				"state":                string(StateAsleep),
				"state_reason":         "reactivated",
				"pending_create_claim": "",
				"continuity_eligible":  "false",
				"quarantined_until":    "",
				"crash_count":          "0",
				"archived_at":          "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !reflect.DeepEqual(tt.patch, tt.want) {
				t.Fatalf("patch = %#v, want %#v", tt.patch, tt.want)
			}
		})
	}
}

func TestMetadataPatchApplyReturnsMergedCopy(t *testing.T) {
	original := map[string]string{
		"state":        string(StateAsleep),
		"session_name": "s-worker",
	}
	patch := RequestWakePatch("pin")

	merged := patch.Apply(original)
	if merged["state"] != string(StateCreating) {
		t.Fatalf("merged state = %q, want creating", merged["state"])
	}
	if merged["session_name"] != "s-worker" {
		t.Fatalf("merged session_name = %q, want preserved", merged["session_name"])
	}
	if original["state"] != string(StateAsleep) {
		t.Fatalf("original state = %q, want original map unchanged", original["state"])
	}
}

func TestRequestWakePatchClearsStaleWakeBlockers(t *testing.T) {
	merged := RequestWakePatch("manual").Apply(map[string]string{
		"state":             string(StateAsleep),
		"held_until":        "9999-12-31T23:59:59Z",
		"quarantined_until": "9999-12-31T23:59:59Z",
		"sleep_reason":      "wait-hold",
		"wait_hold":         "true",
		"sleep_intent":      "idle-stop-pending",
		"wake_attempts":     "4",
		"churn_count":       "2",
	})

	for _, key := range []string{"held_until", "quarantined_until", "sleep_reason", "wait_hold", "sleep_intent"} {
		if merged[key] != "" {
			t.Fatalf("%s = %q, want cleared", key, merged[key])
		}
	}
	for _, key := range []string{"wake_attempts", "churn_count"} {
		if merged[key] != "0" {
			t.Fatalf("%s = %q, want reset to 0", key, merged[key])
		}
	}
}

func TestArchivePatchClearsStaleCreateClaim(t *testing.T) {
	now := time.Date(2026, 4, 15, 13, 0, 0, 0, time.UTC)
	merged := ArchivePatch(now, "failed-create", false).Apply(map[string]string{
		"state":                string(StateCreating),
		"pending_create_claim": "true",
	})

	if merged["state"] != string(StateArchived) {
		t.Fatalf("state = %q, want archived", merged["state"])
	}
	if merged["pending_create_claim"] != "" {
		t.Fatalf("pending_create_claim = %q, want cleared", merged["pending_create_claim"])
	}
}

func TestReactivatePatchDoesNotForceHistoricalBeadEligible(t *testing.T) {
	merged := ReactivatePatch(false).Apply(map[string]string{
		"state":               string(StateArchived),
		"continuity_eligible": "false",
	})

	if merged["state"] != string(StateAsleep) {
		t.Fatalf("state = %q, want asleep", merged["state"])
	}
	if merged["continuity_eligible"] != "false" {
		t.Fatalf("continuity_eligible = %q, want false", merged["continuity_eligible"])
	}
}

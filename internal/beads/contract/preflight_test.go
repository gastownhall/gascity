package contract

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestPreflightResultRedactsBeforeSerialization(t *testing.T) {
	hasDSN := true
	hasSplit := false
	result := NewPreflightResult(PreflightResult{
		Verdict: PreflightVerdictBlocked,
		Scope:   "/tmp/gascity",
		Checks: []PreflightCheckResult{
			NewPreflightCheckResult(
				PreflightCheckMetadataBackend,
				PreflightCheckFail,
				"Metadata backend is postgres (postgres_dsn form)",
				PreflightDetails{
					MetadataBackend:     "postgres",
					HasPostgresDSN:      &hasDSN,
					HasSplitFields:      &hasSplit,
					PostgresDSNRedacted: "postgres://operator:swordfish@db.example.com/gascity",
					PostgresPassword:    "swordfish",
					AuthToken:           "token-value",
					APIKey:              "key-value",
					MetadataProjectID:   "gc-local-visible",
					DBProjectID:         "db-visible",
					AdditionalDiagnostics: []PreflightDetailField{
						{Key: "session_passwd", Value: "passwd-value"},
						{Key: "client_secret", Value: "secret-value"},
						{Key: "routing_token", Value: "routing-token-value"},
						{Key: "private_key", Value: "private-key-value"},
						{Key: "project_id", Value: "gc-local-visible-extra"},
					},
				},
			),
		},
		Fallback: PreflightFallbackBdStore,
	})

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	jsonText := string(data)
	humanFixture := fmt.Sprintf("%+v", result)

	for _, output := range []string{jsonText, humanFixture} {
		for _, secret := range []string{
			"swordfish",
			"operator:swordfish",
			"token-value",
			"key-value",
			"passwd-value",
			"secret-value",
			"routing-token-value",
			"private-key-value",
		} {
			if strings.Contains(output, secret) {
				t.Fatalf("output leaked secret %q:\n%s", secret, output)
			}
		}
		if !strings.Contains(output, "postgres://[REDACTED]") {
			t.Fatalf("output missing redacted DSN:\n%s", output)
		}
		for _, projectID := range []string{"gc-local-visible", "db-visible", "gc-local-visible-extra"} {
			if !strings.Contains(output, projectID) {
				t.Fatalf("output redacted project id %q:\n%s", projectID, output)
			}
		}
	}
}

func TestPreflightBlockedDiagnosticStableFields(t *testing.T) {
	result := NewPreflightResult(PreflightResult{
		Verdict: PreflightVerdictBlocked,
		Scope:   "/home/operator/projects/gascity",
		Checks: []PreflightCheckResult{
			NewPreflightCheckResult(
				PreflightCheckIdentityMatch,
				PreflightCheckFail,
				"project_id mismatch",
				PreflightDetails{
					MetadataProjectID: "gc-local-8ca8e43a5e2ef0b0718ca63c0c07d2df",
					DBProjectID:       "b2269d7c-b3c4-4c30-aef1-ae529c83d16f",
				},
			),
		},
		RepairSteps: []PreflightRepairStep{
			{
				CheckID:  PreflightCheckIdentityMatch,
				Priority: PreflightRepairCritical,
				Command:  "bd doctor --fix",
				Note:     "Identity mismatch is the highest-severity failure.",
			},
		},
		NativeStoreEligible: false,
		Fallback:            PreflightFallbackBdStore,
	})

	if result.Verdict != PreflightVerdictBlocked {
		t.Fatalf("Verdict = %q, want %q", result.Verdict, PreflightVerdictBlocked)
	}
	if result.NativeStoreEligible {
		t.Fatal("NativeStoreEligible = true, want false for blocked diagnostic")
	}
	if result.Fallback != PreflightFallbackBdStore {
		t.Fatalf("Fallback = %q, want %q", result.Fallback, PreflightFallbackBdStore)
	}
	if len(result.Checks) != 1 {
		t.Fatalf("Checks len = %d, want 1", len(result.Checks))
	}
	check := result.Checks[0]
	if check.ID != PreflightCheckIdentityMatch || check.State != PreflightCheckFail {
		t.Fatalf("check = %+v, want identity_match FAIL", check)
	}
	if check.Details.MetadataProjectID == "" || check.Details.DBProjectID == "" {
		t.Fatalf("project ids should remain visible in details: %+v", check.Details)
	}
	if len(result.RepairSteps) != 1 {
		t.Fatalf("RepairSteps len = %d, want 1", len(result.RepairSteps))
	}
	if got := result.RepairSteps[0].Command; got != "bd doctor --fix" {
		t.Fatalf("repair command = %q, want bd doctor --fix", got)
	}
}

func TestPreflightEnumsCoverDesignedStates(t *testing.T) {
	checkStates := []PreflightCheckState{
		PreflightCheckPass,
		PreflightCheckWarn,
		PreflightCheckFail,
	}
	verdicts := []PreflightVerdict{
		PreflightVerdictEligible,
		PreflightVerdictDegraded,
		PreflightVerdictBlocked,
	}

	for _, state := range checkStates {
		if strings.TrimSpace(string(state)) == "" {
			t.Fatalf("empty check state in %#v", checkStates)
		}
	}
	for _, verdict := range verdicts {
		if strings.TrimSpace(string(verdict)) == "" {
			t.Fatalf("empty verdict in %#v", verdicts)
		}
	}
}

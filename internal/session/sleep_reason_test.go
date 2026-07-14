package session

import "testing"

// TestSleepReasonConstantValues pins the on-store string of every sleep reason
// so a constant edit that would change the wire value is caught here rather
// than silently reclassifying live beads.
func TestSleepReasonConstantValues(t *testing.T) {
	want := map[SleepReason]string{
		SleepReasonIdle:                  "idle",
		SleepReasonIdleTimeout:           "idle-timeout",
		SleepReasonNoWakeReason:          "no-wake-reason",
		SleepReasonConfigDrift:           "config-drift",
		SleepReasonDrained:               "drained",
		SleepReasonCityStop:              "city-stop",
		SleepReasonUserHold:              "user-hold",
		SleepReasonWaitHold:              "wait-hold",
		SleepReasonRateLimit:             "rate_limit",
		SleepReasonFailedCreate:          "failed-create",
		SleepReasonProviderTerminalError: "provider-terminal-error",
		SleepReasonRuntimeMissing:        "runtime-missing",
		SleepReasonQuarantine:            "quarantine",
		SleepReasonContextChurn:          "context-churn",
		SleepReasonMaxSessionAge:         "max-session-age",
	}
	for reason, str := range want {
		if string(reason) != str {
			t.Errorf("SleepReason %q = %q, want %q", str, string(reason), str)
		}
	}
	// SleepReasonRuntimeMissing must stay pinned to the shared display-reason
	// constant so the two surfaces of the same posture never drift apart.
	if string(SleepReasonRuntimeMissing) != LifecycleReasonRuntimeMissing {
		t.Errorf("SleepReasonRuntimeMissing = %q, want %q (LifecycleReasonRuntimeMissing)",
			SleepReasonRuntimeMissing, LifecycleReasonRuntimeMissing)
	}
}

// TestSleepReasonListDivergence locks in the documented, deliberate divergence
// between the churn-suppression list (IsDeliberateSleepReason) and the
// continuation-reset list (shouldResetContinuation): the former has
// "failed-create" and lacks "runtime-missing"; the latter is the reverse.
// Merging the two lists is a bug, so this test fails if they ever converge on
// those two elements.
func TestSleepReasonListDivergence(t *testing.T) {
	// failed-create: deliberate stop (no churn) but NOT a reset-suppressor.
	if !IsDeliberateSleepReason(string(SleepReasonFailedCreate)) {
		t.Error("failed-create must be a deliberate sleep reason")
	}
	if resetSuppressed(t, SleepReasonFailedCreate) {
		t.Error("failed-create must NOT suppress continuation reset")
	}

	// runtime-missing: reset-suppressor but NOT churn-deliberate.
	if IsDeliberateSleepReason(string(SleepReasonRuntimeMissing)) {
		t.Error("runtime-missing must NOT be a deliberate sleep reason")
	}
	if !resetSuppressed(t, SleepReasonRuntimeMissing) {
		t.Error("runtime-missing must suppress continuation reset")
	}
}

// resetSuppressed reports whether shouldResetContinuation returns false (reset
// suppressed) for the given reason on an otherwise reset-eligible input.
func resetSuppressed(t *testing.T, reason SleepReason) bool {
	t.Helper()
	input := LifecycleInput{SessionKey: "sk-1"}
	return !shouldResetContinuation(BaseStateActive, input, string(reason))
}

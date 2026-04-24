package convergence

import (
	"testing"
)

func TestRequiresToken(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		// Protected convergence.* keys.
		{FieldState, true},
		{FieldIteration, true},
		{FieldMaxIterations, true},
		{FieldFormula, true},
		{FieldTarget, true},
		{FieldGateMode, true},
		{FieldGateCondition, true},
		{FieldGateTimeout, true},
		{FieldGateTimeoutAction, true},
		{FieldActiveWisp, true},
		{FieldLastProcessedWisp, true},
		{FieldGateOutcome, true},
		{FieldGateExitCode, true},
		{FieldGateOutcomeWisp, true},
		{FieldGateRetryCount, true},
		{FieldTerminalReason, true},
		{FieldTerminalActor, true},
		{FieldWaitingReason, true},
		{FieldRetrySource, true},

		// Agent-writable verdict keys — NOT protected.
		{FieldAgentVerdict, false},
		{FieldAgentVerdictWisp, false},

		// var.* keys — protected.
		{"var.doc_path", true},
		{"var.branch", true},
		{"var.", true},

		// Random keys — not protected.
		{"random_key", false},
		{"merge_strategy", false},
		{"", false},
		{"title", false},
	}
	for _, tt := range tests {
		got := RequiresToken(tt.key)
		if got != tt.want {
			t.Errorf("RequiresToken(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestScrubTokenEnv(t *testing.T) {
	env := map[string]string{
		"PATH":      "/usr/bin",
		"GC_AGENT":  "worker-1",
		TokenEnvVar: "secret-token-value",
		"GC_CITY":   "/tmp/city",
		"HOME":      "/home/user",
	}

	got := ScrubTokenEnv(env)

	// Token should be removed.
	if _, ok := got[TokenEnvVar]; ok {
		t.Errorf("ScrubTokenEnv did not remove %s", TokenEnvVar)
	}

	// Other keys should be preserved.
	for _, key := range []string{"PATH", "GC_AGENT", "GC_CITY", "HOME"} {
		if v, ok := got[key]; !ok || v != env[key] {
			t.Errorf("ScrubTokenEnv lost key %q: got %q, want %q", key, v, env[key])
		}
	}

	// Original should not be modified.
	if _, ok := env[TokenEnvVar]; !ok {
		t.Error("ScrubTokenEnv modified the original map")
	}
}

func TestScrubTokenEnvNil(t *testing.T) {
	got := ScrubTokenEnv(nil)
	if got != nil {
		t.Errorf("ScrubTokenEnv(nil) = %v, want nil", got)
	}
}

func TestScrubTokenEnvNoToken(t *testing.T) {
	env := map[string]string{
		"PATH":     "/usr/bin",
		"GC_AGENT": "worker-1",
	}

	got := ScrubTokenEnv(env)
	if len(got) != 2 {
		t.Errorf("ScrubTokenEnv returned %d keys, want 2", len(got))
	}
	if got["PATH"] != "/usr/bin" || got["GC_AGENT"] != "worker-1" {
		t.Errorf("ScrubTokenEnv modified values: %v", got)
	}
}

package convergence

import (
	"testing"

	"github.com/gastownhall/gascity/internal/processenv"
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

// Withholding the token means PRESENT AND EMPTY, not absent. The map is an
// overlay on an environment the session already inherits, so a deleted key just
// defers to the inherited value: the tmux pane falls through to the tmux
// server's global env, and the subprocess/ACP runtimes exec with os.Environ()
// underneath. An empty value is what tmux turns into `env -u GC_CONTROLLER_TOKEN`
// on the pane command, which is the only thing that makes the key genuinely
// absent from the child.
func TestScrubTokenEnvPinsTokenEmpty(t *testing.T) {
	env := map[string]string{
		"PATH":      "/usr/bin",
		"GC_AGENT":  "worker-1",
		TokenEnvVar: "secret-token-value",
		"GC_CITY":   "/tmp/city",
		"HOME":      "/home/user",
	}

	got := ScrubTokenEnv(env)

	val, ok := got[TokenEnvVar]
	if !ok {
		t.Errorf("ScrubTokenEnv omits %s; want present and empty so the session cannot inherit the controller's value", TokenEnvVar)
	} else if val != "" {
		t.Errorf("ScrubTokenEnv()[%s] = %q, want empty", TokenEnvVar, val)
	}

	// Other keys should be preserved.
	for _, key := range []string{"PATH", "GC_AGENT", "GC_CITY", "HOME"} {
		if v, ok := got[key]; !ok || v != env[key] {
			t.Errorf("ScrubTokenEnv lost key %q: got %q, want %q", key, v, env[key])
		}
	}

	// Original should not be modified.
	if v, ok := env[TokenEnvVar]; !ok || v != "secret-token-value" {
		t.Error("ScrubTokenEnv modified the original map")
	}
}

// A nil map is not a session environment — it is a caller projecting nothing —
// and every real session-env builder starts from
// processenv.ProviderProcessPassthroughEnv(), which is never empty and already
// carries the pin. Returning nil keeps "no overlay" distinguishable from "an
// overlay that sets one thing".
func TestScrubTokenEnvNil(t *testing.T) {
	got := ScrubTokenEnv(nil)
	if got != nil {
		t.Errorf("ScrubTokenEnv(nil) = %v, want nil", got)
	}
}

// The pin does not depend on the caller having supplied the token: an env map
// that never mentioned it still inherits the controller's value at the child,
// so ScrubTokenEnv has to add the empty entry rather than filter one out.
func TestScrubTokenEnvPinsTokenAbsentFromInput(t *testing.T) {
	env := map[string]string{
		"PATH":     "/usr/bin",
		"GC_AGENT": "worker-1",
	}

	got := ScrubTokenEnv(env)
	if len(got) != 3 {
		t.Errorf("ScrubTokenEnv returned %d keys, want 3 (the two inputs plus the pin)", len(got))
	}
	if val, ok := got[TokenEnvVar]; !ok || val != "" {
		t.Errorf("ScrubTokenEnv()[%s] = %q (present=%v), want present and empty", TokenEnvVar, val, ok)
	}
	if got["PATH"] != "/usr/bin" || got["GC_AGENT"] != "worker-1" {
		t.Errorf("ScrubTokenEnv modified values: %v", got)
	}
	if _, ok := env[TokenEnvVar]; ok {
		t.Error("ScrubTokenEnv modified the original map")
	}
}

// processenv pins the same key into the session-env baseline, where every
// builder picks it up whether or not it calls ScrubTokenEnv. Two hand-written
// copies of the same secret name drift silently, so bond them: dropping
// TokenEnvVar from ControllerOnlyEnvKeys would leave the worker resolvers —
// which never route through ScrubTokenEnv — unguarded.
func TestControllerOnlyEnvKeysCoverControllerToken(t *testing.T) {
	for _, key := range processenv.ControllerOnlyEnvKeys {
		if key == TokenEnvVar {
			return
		}
	}
	t.Errorf("processenv.ControllerOnlyEnvKeys = %v, missing %s", processenv.ControllerOnlyEnvKeys, TokenEnvVar)
}

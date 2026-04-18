package acceptancehelpers

import "testing"

func TestNewEnvInheritsClaudeGatewayVariables(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "synthetic-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.synthetic.new/anthropic")
	t.Setenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", "hf:zai-org/GLM-4.7-Flash")
	t.Setenv("ANTHROPIC_DEFAULT_SONNET_MODEL", "hf:moonshotai/Kimi-K2.5")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "hf:moonshotai/Kimi-K2.5")
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "hf:moonshotai/Kimi-K2.5")
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "auto")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")

	env := NewEnv("", t.TempDir(), t.TempDir())

	for key, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN":                     "synthetic-token",
		"ANTHROPIC_BASE_URL":                       "https://api.synthetic.new/anthropic",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":            "hf:zai-org/GLM-4.7-Flash",
		"ANTHROPIC_DEFAULT_SONNET_MODEL":           "hf:moonshotai/Kimi-K2.5",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":             "hf:moonshotai/Kimi-K2.5",
		"CLAUDE_CODE_SUBAGENT_MODEL":               "hf:moonshotai/Kimi-K2.5",
		"CLAUDE_CODE_EFFORT_LEVEL":                 "auto",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	} {
		if got := env.Get(key); got != want {
			t.Fatalf("NewEnv() %s = %q, want %q", key, got, want)
		}
	}
}

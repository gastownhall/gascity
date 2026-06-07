package processenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsProviderCredentialEnvMatchesCuratedAllowlist(t *testing.T) {
	for _, prefix := range providerCredentialEnvPrefixes {
		key := prefix + "TEST_VALUE"
		if !IsProviderCredentialEnv(key) {
			t.Errorf("IsProviderCredentialEnv(%q) = false, want true for prefix %q", key, prefix)
		}
	}
	for key := range providerCredentialEnvKeys {
		if !IsProviderCredentialEnv(key) {
			t.Errorf("IsProviderCredentialEnv(%q) = false, want true for exact key", key)
		}
	}
}

func TestIsProviderCredentialEnvRejectsNearMisses(t *testing.T) {
	for _, key := range []string{
		"",
		"ANTHROPIC",
		"OPENROUTER",
		"AWS_ACCESS_KEY_ID_EXTRA",
		"AWS_EXECUTION_ENV",
		"AWS_PAGER",
		"AWS_VAULT",
		"GC_RIG",
		"GC_SESSION_NAME",
		"CUSTOM_PROVIDER_TOKEN",
	} {
		if IsProviderCredentialEnv(key) {
			t.Errorf("IsProviderCredentialEnv(%q) = true, want false", key)
		}
	}
}

func TestProviderProcessPassthroughEnvIncludesProviderAndRuntimeBaseline(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("LANG", "")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "test-anthropic-token")
	t.Setenv("OLLAMA_API_KEY", "test-ollama-token")
	t.Setenv("AWS_ACCESS_KEY_ID", "test-aws-key")
	t.Setenv("AWS_PAGER", "less")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "nested")
	t.Setenv("CODEX_THREAD_ID", "thread-123")
	t.Setenv("CODEX_CI", "true")

	got := ProviderProcessPassthroughEnv()

	for key, want := range map[string]string{
		"HOME":                   homeDir,
		"PATH":                   "/usr/local/bin:/usr/bin:/bin",
		"LANG":                   "en_US.UTF-8",
		"LC_ALL":                 "",
		"LC_CTYPE":               "",
		"XDG_CONFIG_HOME":        filepath.Join(homeDir, ".config"),
		"XDG_STATE_HOME":         filepath.Join(homeDir, ".local", "state"),
		"ANTHROPIC_AUTH_TOKEN":   "test-anthropic-token",
		"OLLAMA_API_KEY":         "test-ollama-token",
		"AWS_ACCESS_KEY_ID":      "test-aws-key",
		"CLAUDECODE":             "",
		"CLAUDE_CODE_ENTRYPOINT": "",
		"CODEX_THREAD_ID":        "",
		"CODEX_CI":               "",
	} {
		if got[key] != want {
			t.Errorf("ProviderProcessPassthroughEnv()[%s] = %q, want %q", key, got[key], want)
		}
	}
	if _, ok := got["AWS_PAGER"]; ok {
		t.Errorf("ProviderProcessPassthroughEnv()[AWS_PAGER] = %q, want absent", got["AWS_PAGER"])
	}
}

func TestProviderProcessPassthroughEnvKeepsExplicitLocaleAndXDG(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("LC_CTYPE", "UTF-8")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/custom-config")
	t.Setenv("XDG_STATE_HOME", "/tmp/custom-state")

	got := ProviderProcessPassthroughEnv()

	for key, want := range map[string]string{
		"LANG":            "C.UTF-8",
		"LC_ALL":          "en_US.UTF-8",
		"LC_CTYPE":        "UTF-8",
		"XDG_CONFIG_HOME": "/tmp/custom-config",
		"XDG_STATE_HOME":  "/tmp/custom-state",
	} {
		if got[key] != want {
			t.Errorf("ProviderProcessPassthroughEnv()[%s] = %q, want %q", key, got[key], want)
		}
	}
}

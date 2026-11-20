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
	t.Setenv("XIAOMI_API_KEY", "test-xiaomi-key")
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
		"XIAOMI_API_KEY":         "test-xiaomi-key",
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

// The controller token is controller scope. Every session-env builder starts
// from this map, and the map is an OVERLAY on an environment the child already
// inherits — the tmux server's global env, or os.Environ() on the
// subprocess/ACP paths — so an omitted key is an inherited key.
// Present-and-empty is the only value that withholds it.
func TestProviderProcessPassthroughEnvPinsControllerOnlyKeysEmpty(t *testing.T) {
	for _, key := range ControllerOnlyEnvKeys {
		t.Setenv(key, "controller-scope-value")
	}

	got := ProviderProcessPassthroughEnv()

	for _, key := range ControllerOnlyEnvKeys {
		val, ok := got[key]
		if !ok {
			t.Errorf("ProviderProcessPassthroughEnv() omits %s; want present and empty so it overrides the inherited value", key)
			continue
		}
		if val != "" {
			t.Errorf("ProviderProcessPassthroughEnv()[%s] = %q, want empty", key, val)
		}
	}
}

// A controller started from an agent's pane carries that pane's demand-spawn
// keys, and they describe that pane's spawn, not the session being built. The
// keys are spelled out rather than read from DemandSpawnEnvKeys, so a key
// dropped from the list fails here (gastownhall/gascity#5738).
func TestProviderProcessPassthroughEnvWithholdsInheritedDemandSpawnKeys(t *testing.T) {
	keys := []string{
		"GC_SPAWN_ORIGIN",
		"GC_TRIGGER_BEAD_ID",
		"GC_TRIGGER_WORK_BEAD_ID",
		"GC_TRIGGER_BEAD_STORE_REF",
		"GC_TRIGGER_WORK_STORE_REF",
	}
	for _, key := range keys {
		t.Setenv(key, "ga-foreign")
	}

	got := ProviderProcessPassthroughEnv()

	for _, key := range keys {
		if val, ok := got[key]; !ok || val != "" {
			t.Errorf("ProviderProcessPassthroughEnv()[%s] = (%q, present=%v), want (\"\", true): an omitted key is an inherited key", key, val, ok)
		}
		if !IsDemandSpawnEnv(key) {
			t.Errorf("IsDemandSpawnEnv(%q) = false, want true", key)
		}
	}
	for _, key := range []string{"GC_TRIGGER", "GC_SPAWN_ORIGIN_X", "GC_BEAD_ID", "GC_SESSION_ID"} {
		if IsDemandSpawnEnv(key) {
			t.Errorf("IsDemandSpawnEnv(%q) = true, want false (exact names only)", key)
		}
	}
}

// Pinning the keys is not enough on its own: config-authored values are expanded
// against the controller process, so "$GC_CONTROLLER_TOKEN" would copy the token
// into a session variable no key-level guard is watching.
func TestExpandSessionEnvValueMasksControllerOnlyKeys(t *testing.T) {
	t.Setenv("GC_CONTROLLER_TOKEN", "super-secret-controller-token")
	t.Setenv("GC_CONTROLLER_TRACE", "on")

	for _, tc := range []struct{ in, want string }{
		{"$GC_CONTROLLER_TOKEN", ""},
		{"${GC_CONTROLLER_TOKEN}", ""},
		{"Bearer $GC_CONTROLLER_TOKEN", "Bearer "},
		{"$GC_CONTROLLER_TRACE", "on"},
		{"trace=${GC_CONTROLLER_TRACE}", "trace=on"},
	} {
		if got := ExpandSessionEnvValue(tc.in); got != tc.want {
			t.Errorf("ExpandSessionEnvValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TZ passes through to spawned provider sessions so in-session time
// reasoning (e.g. `gc order check`) agrees with the supervisor's wall clock
// instead of defaulting to UTC in the constructed env.
func TestProviderProcessPassthroughEnvIncludesTZ(t *testing.T) {
	t.Setenv("TZ", "America/New_York")
	m := ProviderProcessPassthroughEnv()
	if m["TZ"] != "America/New_York" {
		t.Errorf(`m["TZ"] = %q, want "America/New_York"`, m["TZ"])
	}
}

func TestProviderProcessPassthroughEnvOmitsUnsetTZ(t *testing.T) {
	t.Setenv("TZ", "")
	m := ProviderProcessPassthroughEnv()
	if v, ok := m["TZ"]; ok {
		t.Errorf(`m["TZ"] = %q present, want absent when host TZ is unset`, v)
	}
}

func TestIsProviderRoutingEnv(t *testing.T) {
	for _, tt := range []struct {
		key  string
		want bool
	}{
		{"ANTHROPIC_BASE_URL", true},
		{"ANTHROPIC_DEFAULT_HAIKU_MODEL", true},
		{"ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION", true},
		{"OPENAI_API_BASE", true},
		{"OPENAI_BASE_URL", true},
		{"AZURE_OPENAI_ENDPOINT", true},
		{"GOOGLE_GEMINI_BASE_URL", true},
		{"ZCODE_MODEL", true},
		{"OLLAMA_HOST", true},
		// A trailing digit run is trimmed before the routing-word match.
		{"OPENAI_BASE_URL2", true},
		{"ANTHROPIC_AUTH_TOKEN2", false},
		{"ANTHROPIC_AUTH_TOKEN_ZAI", false},
		{"ANTHROPIC_API_KEY", false},
		{"GOOGLE_APPLICATION_CREDENTIALS", false},
		{"GOOGLE_CLOUD_PROJECT", false},
		{"AZURE_CLIENT_ID", false},
		{"OPENAI_ORG_ID", false},
		{"ANTHROPIC_CUSTOM_HEADERS", false},
		// Whole words only: MODELS and HOSTNAMES are not routing words.
		{"OLLAMA_MODELS", false},
		{"OLLAMA_HOSTNAMES", false},
		// Exact-name keys and non-provider names are never routing-class.
		{"AWS_ENDPOINT_URL", false},
		{"HTTPS_PROXY", false},
		// A secret word vetoes a routing word, as written or digit-trimmed.
		{"OPENAI_PROXY_API_KEY", false},
		{"ANTHROPIC_BASE_URL_TOKEN3", false},
		{"GOOGLE_CERT_BASE64", false},
		// The bare prefix has no words.
		{"ANTHROPIC_", false},
		{"", false},
	} {
		if got := IsProviderRoutingEnv(tt.key); got != tt.want {
			t.Errorf("IsProviderRoutingEnv(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestProviderEnvPrefix(t *testing.T) {
	for _, tt := range []struct {
		key        string
		wantPrefix string
		wantOK     bool
	}{
		{"ANTHROPIC_BASE_URL", "ANTHROPIC_", true},
		{"OPENROUTER_API_KEY", "OPENROUTER_", true},
		{"OPENAI_API_KEY", "OPENAI_", true},
		{"AWS_ACCESS_KEY_ID", "", false},
		{"HTTPS_PROXY", "", false},
	} {
		prefix, ok := ProviderEnvPrefix(tt.key)
		if prefix != tt.wantPrefix || ok != tt.wantOK {
			t.Errorf("ProviderEnvPrefix(%q) = (%q, %v), want (%q, %v)", tt.key, prefix, ok, tt.wantPrefix, tt.wantOK)
		}
	}
}

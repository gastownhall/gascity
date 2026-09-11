// Package processenv centralizes process environment filters shared by CLI and
// API session launch paths.
package processenv

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/telemetry"
)

// providerCredentialEnvPrefixes lists provider-specific env-var name prefixes
// whose values are treated as agent-provider credentials and forwarded into
// the supervisor's persistent env and spawned agent processes.
//
// The list is curated, not auto-discovered: persistent supervisor env has a
// bounded size, so broad ecosystems such as AWS use exact names in
// providerCredentialEnvKeys to avoid persisting unrelated tooling state.
//
// Keep alphabetised. Documented providers:
//
//	ANTHROPIC_   Anthropic / Claude
//	AZURE_       Azure OpenAI
//	CEREBRAS_    Cerebras
//	COHERE_      Cohere
//	DEEPSEEK_    DeepSeek
//	FIREWORKS_   Fireworks AI
//	GEMINI_      Google Gemini direct API
//	GOOGLE_      Google Cloud / Vertex
//	GROQ_        Groq
//	MISTRAL_     Mistral
//	OLLAMA_      Ollama local and Ollama Cloud
//	OPENAI_      OpenAI-compatible providers
//	OPENROUTER_  OpenRouter
//	TOGETHER_    Together AI
//	VERTEX_      Vertex AI direct
//	XAI_         xAI / Grok
//	XIAOMI_      Xiaomi MiMo
//	ZCODE_       Z.ai ZCode (GLM harness)
var providerCredentialEnvPrefixes = []string{
	"ANTHROPIC_",
	"AZURE_",
	"CEREBRAS_",
	"COHERE_",
	"DEEPSEEK_",
	"FIREWORKS_",
	"GEMINI_",
	"GOOGLE_",
	"GROQ_",
	"MISTRAL_",
	"OLLAMA_",
	"OPENAI_",
	"OPENROUTER_",
	"TOGETHER_",
	"VERTEX_",
	"XAI_",
	"XIAOMI_",
	"ZCODE_",
}

// providerCredentialEnvKeys lists exact provider credential/config env vars for
// providers whose common env namespace is broader than provider auth.
var providerCredentialEnvKeys = map[string]bool{
	"AWS_ACCESS_KEY_ID":                      true,
	"AWS_BEARER_TOKEN_BEDROCK":               true,
	"AWS_CA_BUNDLE":                          true,
	"AWS_CONFIG_FILE":                        true,
	"AWS_CONTAINER_AUTHORIZATION_TOKEN":      true,
	"AWS_CONTAINER_CREDENTIALS_FULL_URI":     true,
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": true,
	"AWS_DEFAULT_REGION":                     true,
	"AWS_EC2_METADATA_DISABLED":              true,
	"AWS_ENDPOINT_URL":                       true,
	"AWS_ENDPOINT_URL_BEDROCK":               true,
	"AWS_PROFILE":                            true,
	"AWS_REGION":                             true,
	"AWS_ROLE_ARN":                           true,
	"AWS_SDK_LOAD_CONFIG":                    true,
	"AWS_SECRET_ACCESS_KEY":                  true,
	"AWS_SESSION_TOKEN":                      true,
	"AWS_SHARED_CREDENTIALS_FILE":            true,
	"AWS_USE_DUALSTACK_ENDPOINT":             true,
	"AWS_USE_FIPS_ENDPOINT":                  true,
	"AWS_WEB_IDENTITY_TOKEN_FILE":            true,
}

// ControllerOnlyEnvKeys are controller-scope variables that must never reach a
// session. Today that is the controller token (convergence.TokenEnvVar), which
// authorizes writes to the protected convergence.*/var.* metadata an agent is
// deliberately not allowed to set for itself; an agent holding it can drive its
// own convergence loop.
//
// These are SEEDED EMPTY rather than omitted, because the map this package
// returns is an overlay on an environment the child already inherits, not the
// whole environment. A tmux pane starts from the tmux server's global env, which
// holds whatever the controller exported when the server started; the
// subprocess and ACP runtimes exec with os.Environ() plus the overlay appended.
// On every one of those paths, declining to re-export a key leaves the
// controller's value visible to the child. Only an explicit KEY="" overrides it
// — and on tmux an empty value is stronger than it looks, because the adapter
// turns it into an `env -u KEY` prefix on the pane command
// (tmux.NewSessionWithCommandAndEnv).
//
// Exact names, never a prefix: a GC_ prefix match would swallow the identity
// anchors and the Dolt vars that agents legitimately need.
var ControllerOnlyEnvKeys = []string{
	"GC_CONTROLLER_TOKEN",
}

// ControllerOnlyEnvOverlay returns ControllerOnlyEnvKeys pinned to the empty
// string, for use as the last layer of a session-env merge — after every
// config-authored layer, so a city.toml entry naming one of these keys cannot
// overwrite the pin ProviderProcessPassthroughEnv established.
func ControllerOnlyEnvOverlay() map[string]string {
	m := make(map[string]string, len(ControllerOnlyEnvKeys))
	for _, key := range ControllerOnlyEnvKeys {
		m[key] = ""
	}
	return m
}

// IsControllerOnlyEnv reports whether key is controller scope, i.e. a value that
// must not survive into any process an agent runtime starts. Runtime adapters
// use it to tell a WITHHELD CREDENTIAL apart from the other empty-valued keys a
// session env carries (the Claude/Codex nesting flags), which are withheld for
// tidiness rather than containment and need no durable enforcement.
func IsControllerOnlyEnv(key string) bool {
	for _, controllerOnly := range ControllerOnlyEnvKeys {
		if key == controllerOnly {
			return true
		}
	}
	return false
}

// ExpandSessionEnvValue expands $VAR references in a config-authored env value
// against the controller process, with ControllerOnlyEnvKeys masked to empty.
// Pinning the keys themselves is not enough: a value such as
// `FOO = "$GC_CONTROLLER_TOKEN"` copies the controller's value into a
// differently-named session variable, which no key-level guard can catch.
func ExpandSessionEnvValue(value string) string {
	return os.Expand(value, func(key string) string {
		for _, controllerOnly := range ControllerOnlyEnvKeys {
			if key == controllerOnly {
				return ""
			}
		}
		return os.Getenv(key)
	})
}

// IsProviderCredentialEnv reports whether key belongs to the curated provider
// credential/config allowlist.
func IsProviderCredentialEnv(key string) bool {
	if providerCredentialEnvKeys[key] {
		return true
	}
	for _, prefix := range providerCredentialEnvPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// ProviderProcessPassthroughEnv returns non-GC process context that provider
// sessions need to start reliably: user/home, provider auth/config, locale,
// time zone, XDG, telemetry, and Claude nesting resets. It also pins
// ControllerOnlyEnvKeys empty, so every session-env builder that starts here
// withholds them without having to know they exist.
func ProviderProcessPassthroughEnv() map[string]string {
	m := make(map[string]string)
	if v := os.Getenv("PATH"); v != "" {
		m["PATH"] = v
	}
	if v := os.Getenv("HOME"); v != "" {
		m["HOME"] = v
	}
	for _, key := range []string{
		"USER",
		"LOGNAME",
		// TZ keeps spawned sessions on the host wall clock so any in-session
		// time reasoning (e.g. `gc order check`, date math in scripts) agrees
		// with the supervisor instead of defaulting to UTC.
		"TZ",
		"CLAUDE_CONFIG_DIR",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_SUBAGENT_MODEL",
		"CLAUDE_CODE_EFFORT_LEVEL",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
	} {
		if v := os.Getenv(key); v != "" {
			m[key] = v
		}
	}
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE"} {
		if v := os.Getenv(key); v != "" {
			m[key] = v
		}
	}
	if _, ok := m["LC_ALL"]; !ok {
		m["LC_ALL"] = ""
	}
	if _, ok := m["LC_CTYPE"]; !ok {
		m["LC_CTYPE"] = ""
	}
	if m["LANG"] == "" && m["LC_ALL"] == "" && m["LC_CTYPE"] == "" {
		m["LANG"] = "en_US.UTF-8"
	}
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" {
		m["XDG_CONFIG_HOME"] = v
	} else if home := os.Getenv("HOME"); home != "" {
		m["XDG_CONFIG_HOME"] = filepath.Join(home, ".config")
	}
	if v := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); v != "" {
		m["XDG_STATE_HOME"] = v
	} else if home := os.Getenv("HOME"); home != "" {
		m["XDG_STATE_HOME"] = filepath.Join(home, ".local", "state")
	}
	for _, entry := range os.Environ() {
		key, val, ok := strings.Cut(entry, "=")
		if !ok || val == "" {
			continue
		}
		if IsProviderCredentialEnv(key) {
			m[key] = val
		}
	}
	for k, v := range telemetry.OTELEnvMap() {
		m[k] = v
	}
	m["CLAUDECODE"] = ""
	m["CLAUDE_CODE_ENTRYPOINT"] = ""
	m["CODEX_THREAD_ID"] = ""
	m["CODEX_CI"] = ""
	for key, val := range ControllerOnlyEnvOverlay() {
		m[key] = val
	}
	return m
}

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
// XDG, telemetry, and Claude nesting resets.
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
	return m
}

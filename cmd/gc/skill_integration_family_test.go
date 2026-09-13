package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestEffectiveAgentProviderFamily_WrappedCodexResolvesToFamily verifies
// that a wrapped custom provider (base = "builtin:codex") resolves via
// BuiltinFamily to "codex" when consulting the vendor-sink lookup. This
// is the fix the Phase 4B audit targets: wrapped aliases previously
// missed materialize.VendorSink because the lookup used the raw name.
func TestEffectiveAgentProviderFamily_WrappedCodexResolvesToFamily(t *testing.T) {
	base := "builtin:codex"
	cityProviders := map[string]config.ProviderSpec{
		// codex uses subcommand-style resume, so a wrapper that overrides
		// Command must declare ResumeCommand (or omit Command to inherit).
		// We model a shell-wrapper alias: Command = "wrapper", resume via
		// the ancestor's binary.
		"codex-mini": {
			Base:          &base,
			Command:       "wrapper",
			ResumeCommand: "codex resume {{.SessionKey}}",
		},
	}
	agent := &config.Agent{Provider: "codex-mini"}
	got := effectiveAgentProviderFamily(agent, "", cityProviders)
	if got != "codex" {
		t.Errorf("effectiveAgentProviderFamily(codex-mini) = %q, want %q", got, "codex")
	}
}

// TestEffectiveAgentProviderFamily_WrappedGeminiResolvesToFamily mirrors
// the codex case for gemini.
func TestEffectiveAgentProviderFamily_WrappedGeminiResolvesToFamily(t *testing.T) {
	base := "builtin:gemini"
	cityProviders := map[string]config.ProviderSpec{
		"gemini-fast": {Base: &base, Command: "gemini-fast"},
	}
	agent := &config.Agent{Provider: "gemini-fast"}
	got := effectiveAgentProviderFamily(agent, "", cityProviders)
	if got != "gemini" {
		t.Errorf("effectiveAgentProviderFamily(gemini-fast) = %q, want %q", got, "gemini")
	}
}

// TestEffectiveAgentProviderFamily_BuiltinUnchanged confirms the
// identity branch: a literal built-in name returns itself.
func TestEffectiveAgentProviderFamily_BuiltinUnchanged(t *testing.T) {
	agent := &config.Agent{Provider: "codex"}
	if got := effectiveAgentProviderFamily(agent, "", nil); got != "codex" {
		t.Errorf("effectiveAgentProviderFamily(codex, nil providers) = %q, want codex", got)
	}
}

// TestEffectiveAgentProviderFamily_WorkspaceFallback verifies that an
// agent with no explicit provider inherits the workspace default, and
// that the family lookup applies to the workspace value too.
func TestEffectiveAgentProviderFamily_WorkspaceFallback(t *testing.T) {
	base := "builtin:codex"
	cityProviders := map[string]config.ProviderSpec{
		"codex-mini": {
			Base:          &base,
			Command:       "wrapper",
			ResumeCommand: "codex resume {{.SessionKey}}",
		},
	}
	agent := &config.Agent{} // no explicit provider
	got := effectiveAgentProviderFamily(agent, "codex-mini", cityProviders)
	if got != "codex" {
		t.Errorf("effectiveAgentProviderFamily(workspace=codex-mini) = %q, want codex", got)
	}
}

// TestEffectiveAgentProviderFamily_FullyCustomReturnsRawName confirms
// the fallback path: a custom provider with no built-in ancestor keeps
// its raw name so downstream lookups (VendorSink) fail closed rather
// than silently widening.
func TestEffectiveAgentProviderFamily_FullyCustomReturnsRawName(t *testing.T) {
	cityProviders := map[string]config.ProviderSpec{
		"bespoke": {Command: "my-bin"},
	}
	agent := &config.Agent{Provider: "bespoke"}
	got := effectiveAgentProviderFamily(agent, "", cityProviders)
	if got != "bespoke" {
		t.Errorf("effectiveAgentProviderFamily(fully custom) = %q, want raw name %q", got, "bespoke")
	}
}

// TestEffectiveAgentProviderFamily_NilAgentReturnsEmpty guards the
// nil-agent contract so skill-materialization callers can short-circuit
// without a nil-check at every call site.
func TestEffectiveAgentProviderFamily_NilAgentReturnsEmpty(t *testing.T) {
	if got := effectiveAgentProviderFamily(nil, "claude", nil); got != "" {
		t.Errorf("effectiveAgentProviderFamily(nil agent) = %q, want empty", got)
	}
}

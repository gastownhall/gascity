package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestUsesSoftEscapeInterrupt_WrappedCodex verifies that a session bead
// whose builtin_ancestor = "codex" (e.g. [providers.codex-mini] with
// base = "builtin:codex") triggers the same soft-escape-interrupt branch
// that a literal "codex" session does. Regression guard for Phase 4B:
// wrapped codex aliases MUST be recognized as codex-family for the
// interrupt semantics to match the ancestor.
func TestUsesSoftEscapeInterrupt_WrappedCodex(t *testing.T) {
	wrapped := beads.Bead{Metadata: map[string]string{
		"builtin_ancestor": "codex",
		"provider":         "codex-mini",
	}}
	if !usesSoftEscapeInterrupt(wrapped) {
		t.Error("wrapped codex (builtin_ancestor=codex) should use soft-escape interrupt")
	}
}

// TestUsesSoftEscapeInterrupt_WrappedGemini covers the gemini-family
// sibling of the codex case above. A wrapped gemini (e.g. a custom alias
// with base = "builtin:gemini") must also soft-escape so Ctrl-C is not
// sent to the provider.
func TestUsesSoftEscapeInterrupt_WrappedGemini(t *testing.T) {
	wrapped := beads.Bead{Metadata: map[string]string{
		"builtin_ancestor": "gemini",
		"provider":         "gemini-fast",
	}}
	if !usesSoftEscapeInterrupt(wrapped) {
		t.Error("wrapped gemini (builtin_ancestor=gemini) should use soft-escape interrupt")
	}
}

// TestUsesSoftEscapeInterrupt_WrappedClaudeDoesNot ensures we haven't
// widened the match: a claude-family bead must NOT use soft-escape
// (claude uses the hard Interrupt path).
func TestUsesSoftEscapeInterrupt_WrappedClaudeDoesNot(t *testing.T) {
	wrappedClaude := beads.Bead{Metadata: map[string]string{
		"builtin_ancestor": "claude",
		"provider":         "claude-max",
	}}
	if usesSoftEscapeInterrupt(wrappedClaude) {
		t.Error("wrapped claude (builtin_ancestor=claude) must NOT use soft-escape; it uses hard Interrupt")
	}
}

// TestUsesSoftEscapeInterrupt_ACPTransportSuppresses verifies that
// ACP transport wins over family — an ACP bead bypasses soft-escape
// regardless of family so interrupts flow through the ACP transport.
func TestUsesSoftEscapeInterrupt_ACPTransportSuppresses(t *testing.T) {
	acp := beads.Bead{Metadata: map[string]string{
		"builtin_ancestor": "codex",
		"transport":        "acp",
	}}
	if usesSoftEscapeInterrupt(acp) {
		t.Error("ACP transport must suppress soft-escape even for codex-family")
	}
}

// TestUsesImmediateDefaultSubmit_WrappedCodex verifies that a wrapped
// codex (builtin_ancestor=codex) reports immediate-default-submit. A
// raw codex uses NudgeNow on default submit; the wrapped variant must
// match.
func TestUsesImmediateDefaultSubmit_WrappedCodex(t *testing.T) {
	wrapped := beads.Bead{Metadata: map[string]string{
		"builtin_ancestor": "codex",
		"provider":         "codex-mini",
	}}
	if !usesImmediateDefaultSubmit(wrapped) {
		t.Error("wrapped codex (builtin_ancestor=codex) should use immediate default submit")
	}
}

// TestUsesImmediateDefaultSubmit_WrappedGeminiDoesNot — only codex gets
// the immediate-default treatment; gemini (even wrapped) must not.
func TestUsesImmediateDefaultSubmit_WrappedGeminiDoesNot(t *testing.T) {
	wrapped := beads.Bead{Metadata: map[string]string{
		"builtin_ancestor": "gemini",
		"provider":         "gemini-fast",
	}}
	if usesImmediateDefaultSubmit(wrapped) {
		t.Error("wrapped gemini must NOT use immediate default submit (codex-only behavior)")
	}
}

package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestSessionProviderFamily_BuiltinAncestorWins verifies that
// builtin_ancestor metadata takes precedence over provider_kind and
// provider when selecting a session bead's family. Matches the
// preference order documented on internal/session.providerKind.
func TestSessionProviderFamily_BuiltinAncestorWins(t *testing.T) {
	b := beads.Bead{Metadata: map[string]string{
		"builtin_ancestor": "codex",
		"provider_kind":    "codex-mini",
		"provider":         "codex-mini",
	}}
	if got := sessionProviderFamily(b); got != "codex" {
		t.Errorf("sessionProviderFamily wrapped codex = %q, want codex", got)
	}
}

// TestSessionProviderFamily_ProviderKindFallback covers sessions created
// before builtin_ancestor was stamped: provider_kind is used when
// builtin_ancestor is absent.
func TestSessionProviderFamily_ProviderKindFallback(t *testing.T) {
	b := beads.Bead{Metadata: map[string]string{
		"provider_kind": "codex",
		"provider":      "fast",
	}}
	if got := sessionProviderFamily(b); got != "codex" {
		t.Errorf("sessionProviderFamily with provider_kind only = %q, want codex", got)
	}
}

// TestSessionProviderFamily_RawProviderLastResort covers oldest sessions:
// neither builtin_ancestor nor provider_kind stamped, only raw provider.
func TestSessionProviderFamily_RawProviderLastResort(t *testing.T) {
	b := beads.Bead{Metadata: map[string]string{
		"provider": "codex",
	}}
	if got := sessionProviderFamily(b); got != "codex" {
		t.Errorf("sessionProviderFamily with provider only = %q, want codex", got)
	}
}

// TestSessionProviderFamily_WrappedCodexPollerGate documents the wait-
// ready-nudge site: if a session bead reports codex-family (via any
// preference), the wait-ready nudge path must start the codex poller.
// This is a structural check on the helper, not the calling site.
func TestSessionProviderFamily_WrappedCodexPollerGate(t *testing.T) {
	// Wrapped codex alias with explicit builtin_ancestor = "codex".
	wrapped := beads.Bead{Metadata: map[string]string{
		"builtin_ancestor": "codex",
		"provider":         "codex-mini",
	}}
	if sessionProviderFamily(wrapped) != "codex" {
		t.Fatal("wrapped codex must surface as codex-family so the wait poller starts")
	}
}

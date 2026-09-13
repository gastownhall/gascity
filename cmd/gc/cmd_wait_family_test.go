package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// familyInfo projects a bead's metadata into the session.Info the wait-nudge
// helpers now consume, so the family-resolution precedence stays exercised at
// this boundary (the resolution itself lives in session.ProviderFamilyFromInfo).
func familyInfo(meta map[string]string) sessionpkg.Info {
	return seedSessionInfo(beads.Bead{ID: "gc-fam", Type: "session", Labels: []string{"gc:session"}, Metadata: meta})
}

// TestSessionProviderFamily_BuiltinAncestorWins verifies that
// builtin_ancestor metadata takes precedence over provider_kind and
// provider when selecting a session's family. Matches the preference order
// documented on internal/session.providerKind.
func TestSessionProviderFamily_BuiltinAncestorWins(t *testing.T) {
	info := familyInfo(map[string]string{
		"builtin_ancestor": "codex",
		"provider_kind":    "codex-mini",
		"provider":         "codex-mini",
	})
	if got := sessionProviderFamily(info); got != "codex" {
		t.Errorf("sessionProviderFamily wrapped codex = %q, want codex", got)
	}
}

// TestSessionProviderFamily_ProviderKindFallback covers sessions created
// before builtin_ancestor was stamped: provider_kind is used when
// builtin_ancestor is absent.
func TestSessionProviderFamily_ProviderKindFallback(t *testing.T) {
	info := familyInfo(map[string]string{
		"provider_kind": "codex",
		"provider":      "fast",
	})
	if got := sessionProviderFamily(info); got != "codex" {
		t.Errorf("sessionProviderFamily with provider_kind only = %q, want codex", got)
	}
}

// TestSessionProviderFamily_RawProviderLastResort covers oldest sessions:
// neither builtin_ancestor nor provider_kind stamped, only raw provider.
func TestSessionProviderFamily_RawProviderLastResort(t *testing.T) {
	info := familyInfo(map[string]string{
		"provider": "codex",
	})
	if got := sessionProviderFamily(info); got != "codex" {
		t.Errorf("sessionProviderFamily with provider only = %q, want codex", got)
	}
}

func TestSessionProviderFamily_NormalizesProviderAliases(t *testing.T) {
	info := familyInfo(map[string]string{
		"builtin_ancestor": "my-pi/tmux",
		"provider_kind":    "codex",
		"provider":         "codex",
	})
	if got := sessionProviderFamily(info); got != "pi" {
		t.Errorf("sessionProviderFamily alias = %q, want pi", got)
	}
}

// TestSessionProviderFamily_WrappedCodexPollerGate documents the wait-
// ready-nudge site: if a session reports codex-family (via any preference),
// the wait-ready nudge path must start the codex poller.
func TestSessionProviderFamily_WrappedCodexPollerGate(t *testing.T) {
	// Wrapped codex alias with explicit builtin_ancestor = "codex".
	wrapped := familyInfo(map[string]string{
		"builtin_ancestor": "codex",
		"provider":         "codex-mini",
	})
	if sessionProviderFamily(wrapped) != "codex" {
		t.Fatal("wrapped codex must surface as codex-family so the wait poller starts")
	}
}

func TestWaitNudgeProviderNeedsPollerIncludesPi(t *testing.T) {
	for _, provider := range []string{"codex", "pi"} {
		if !waitNudgeProviderNeedsPoller(familyInfo(map[string]string{"provider": provider})) {
			t.Fatalf("%s wait nudge should start a poller", provider)
		}
	}
	if waitNudgeProviderNeedsPoller(familyInfo(map[string]string{"provider": "claude"})) {
		t.Fatal("claude wait nudge should not start a per-session poller")
	}
}

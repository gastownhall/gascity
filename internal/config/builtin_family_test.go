package config

import "testing"

// TestBuiltinFamily_DirectBuiltinReturnsItself covers the identity
// branch: any name that IS a built-in resolves to itself.
func TestBuiltinFamily_DirectBuiltinReturnsItself(t *testing.T) {
	cases := []string{"claude", "codex", "gemini", "opencode"}
	for _, name := range cases {
		if got := BuiltinFamily(name, nil); got != name {
			t.Errorf("BuiltinFamily(%q, nil) = %q, want %q", name, got, name)
		}
	}
}

func TestBuiltinFamily_ExplicitEmptyShadowedBuiltinHasNoFamily(t *testing.T) {
	empty := ""
	cityProviders := map[string]ProviderSpec{
		"codex": {Base: &empty, Command: "codex"},
	}
	if got := BuiltinFamily("codex", cityProviders); got != "" {
		t.Errorf("BuiltinFamily(shadowed codex base empty) = %q, want empty", got)
	}
}

// TestBuiltinFamily_UnknownNameReturnsEmpty ensures truly unknown names
// report "" rather than silently widening the match.
func TestBuiltinFamily_UnknownNameReturnsEmpty(t *testing.T) {
	if got := BuiltinFamily("no-such-provider", nil); got != "" {
		t.Errorf("BuiltinFamily(unknown, nil) = %q, want empty", got)
	}
}

// TestBuiltinFamily_WrappedCodexViaExplicitBase walks the chain for a
// custom provider that declares base = "builtin:codex". The helper must
// report "codex" so runtime call sites (soft-escape interrupt, nudge
// poller, etc.) treat it as codex-family.
func TestBuiltinFamily_WrappedCodexViaExplicitBase(t *testing.T) {
	base := "builtin:codex"
	cityProviders := map[string]ProviderSpec{
		"codex-mini": {
			Base:          &base,
			Command:       "codex-mini",
			ResumeCommand: "codex resume {{.SessionKey}}",
		},
	}
	if got := BuiltinFamily("codex-mini", cityProviders); got != "codex" {
		t.Errorf("BuiltinFamily(codex-mini via base=builtin:codex) = %q, want %q", got, "codex")
	}
}

// TestBuiltinFamily_WrappedGeminiViaExplicitBase mirrors the codex test
// for the gemini branch.
func TestBuiltinFamily_WrappedGeminiViaExplicitBase(t *testing.T) {
	base := "builtin:gemini"
	cityProviders := map[string]ProviderSpec{
		"gemini-fast": {Base: &base, Command: "gemini-fast"},
	}
	if got := BuiltinFamily("gemini-fast", cityProviders); got != "gemini" {
		t.Errorf("BuiltinFamily(gemini-fast via base=builtin:gemini) = %q, want %q", got, "gemini")
	}
}

// TestBuiltinFamily_LegacyCommandMatch covers the Phase A auto-
// inheritance branch: no `base` declared, but the Command field matches
// a built-in. This is the pre-inheritance v0.14 behavior and must
// continue to work so users don't need to retrofit base = "...".
func TestBuiltinFamily_LegacyCommandMatch(t *testing.T) {
	cityProviders := map[string]ProviderSpec{
		"fast": {Command: "codex"},
	}
	if got := BuiltinFamily("fast", cityProviders); got != "codex" {
		t.Errorf("BuiltinFamily(fast with command=codex, no base) = %q, want %q", got, "codex")
	}
}

// TestBuiltinFamily_FullyCustomNoAncestor verifies that a custom
// provider with no base and a non-builtin Command reports "" — the
// family is undetermined, and callers must not guess.
func TestBuiltinFamily_FullyCustomNoAncestor(t *testing.T) {
	cityProviders := map[string]ProviderSpec{
		"bespoke": {Command: "my-binary"},
	}
	if got := BuiltinFamily("bespoke", cityProviders); got != "" {
		t.Errorf("BuiltinFamily(bespoke, custom command) = %q, want empty", got)
	}
}

// TestBuiltinFamily_ProviderPrefixChainNoBuiltin verifies that a chain
// resolving entirely through provider: prefixes (never reaching a
// built-in) reports "". Guards against a common misreading of the
// helper that would return the leaf's Command name.
func TestBuiltinFamily_ProviderPrefixChainNoBuiltin(t *testing.T) {
	a := "provider:B"
	b := "" // B is a chain root with no base
	emptyBase := b
	cityProviders := map[string]ProviderSpec{
		"A": {Base: &a, Command: "my-a"},
		"B": {Base: &emptyBase, Command: "my-b"},
	}
	if got := BuiltinFamily("A", cityProviders); got != "" {
		t.Errorf("BuiltinFamily(A→provider:B→root) = %q, want empty (no built-in in chain)", got)
	}
}

// TestBuiltinFamily_CycleReturnsEmpty verifies graceful handling of
// inheritance cycles: chain resolution fails, so the helper reports ""
// rather than panicking or returning a partial result.
func TestBuiltinFamily_CycleReturnsEmpty(t *testing.T) {
	aBase := "provider:B"
	bBase := "provider:A"
	cityProviders := map[string]ProviderSpec{
		"A": {Base: &aBase},
		"B": {Base: &bBase},
	}
	if got := BuiltinFamily("A", cityProviders); got != "" {
		t.Errorf("BuiltinFamily(cycle A↔B) = %q, want empty (chain walk errors)", got)
	}
}

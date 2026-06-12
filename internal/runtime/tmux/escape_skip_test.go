package tmux

import "testing"

// TestProviderEnvSkipsEscapeBeforeEnter pins the per-provider escape-skip
// behavior for nudge submission. Providers in
// providersSkippingEscapeBeforeEnter treat Escape as a destructive input
// (dismissing menus or clearing composer state), so submits must go straight
// to Enter; dropping a family from the skip list silently reintroduces the
// Escape keystroke for that provider's panes.
func TestProviderEnvSkipsEscapeBeforeEnter(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{provider: "claude", want: true},
		{provider: "codex", want: true},
		{provider: "copilot", want: true},
		{provider: "gemini", want: true},
		{provider: "grok", want: true},
		{provider: "kimi", want: true},
		{provider: "opencode", want: true},
		{provider: "pi", want: true},
		{provider: "antigravity", want: true},
		// Derived names resolve through the session-log family mapping,
		// which matches codex/gemini/kimi/opencode/antigravity by
		// substring. GC_PROVIDER carries the builtin-ancestor family for
		// aliased providers, so this is belt-and-suspenders for those
		// families.
		{provider: "antigravity-max", want: true},
		{provider: "codex-mini", want: true},
		// claude has no substring case in sessionlog.ProviderFamily, so a
		// non-exact claude-derived value falls through to the default
		// Escape-before-Enter submit. Aliased claude providers are
		// unaffected in practice because GC_PROVIDER is already resolved
		// to the ancestor family "claude".
		{provider: "claude-mini", want: false},
		// Unknown providers keep the default Escape-before-Enter submit.
		{provider: "", want: false},
		{provider: "some-unknown-provider", want: false},
	}
	for _, tt := range tests {
		name := tt.provider
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			if got := providerEnvSkipsEscape(tt.provider); got != tt.want {
				t.Fatalf("providerEnvSkipsEscape(%q) = %v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}

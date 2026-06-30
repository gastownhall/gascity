package herdr

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestStartupDeliveryText pins the first-turn delivery decision that keeps named
// always-awake Claude sessions from booting unprimed under herdr: their prime
// rides cfg.PromptSuffix (PromptMode=arg) which herdr's exec launch has no slot
// for, so Start must recover it from PromptSuffix and deliver it. The pool/sling
// claim path (cfg.Nudge) must stay byte-for-byte unchanged.
func TestStartupDeliveryText(t *testing.T) {
	// A real prime is multiline with blank-line separators; exercise the
	// Quote→Split→parts[0] round-trip on exactly that shape.
	prime := "# Deacon Context\n\n> Recovery: Run `gc prime`\n\nYou are the heartbeat. Run."

	tests := []struct {
		name string
		cfg  runtime.Config
		want string
	}{
		{
			name: "named session: prime rides PromptSuffix (the regression)",
			cfg:  runtime.Config{PromptSuffix: shellquote.Quote(prime)},
			want: prime,
		},
		{
			name: "pool slot: claim nudge delivered unchanged",
			cfg:  runtime.Config{Nudge: "Run gc hook --claim --json now; execute the claimed formula."},
			want: "Run gc hook --claim --json now; execute the claimed formula.",
		},
		{
			name: "nudge precedence when both set keeps pool path unchanged",
			cfg:  runtime.Config{Nudge: "claim now", PromptSuffix: shellquote.Quote(prime)},
			want: "claim now",
		},
		{
			name: "nothing to deliver (deterministic worker / suppressed prompt)",
			cfg:  runtime.Config{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := startupDeliveryText(tt.cfg); got != tt.want {
				t.Errorf("startupDeliveryText() = %q, want %q", got, tt.want)
			}
		})
	}
}

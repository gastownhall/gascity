package sessionlog

import (
	"strings"
	"testing"
)

// TestHookPayloadTurnIDNoneBeforeTheLiveArm proves no family links a hook
// payload while X11's live arm has verified none: every family reads "",
// even from a payload carrying the fields the families' hooks send.
func TestHookPayloadTurnIDNoneBeforeTheLiveArm(t *testing.T) {
	payload := []byte(`{"prompt_id":"p-1","turn_id":"t-1","session_id":"s-1"}`)
	for _, provider := range []string{"claude", "codex", "gemini", "pi", "opencode"} {
		if got := HookPayloadTurnID(provider, payload); got != "" {
			t.Errorf("HookPayloadTurnID(%q) = %q before the live arm verified the family", provider, got)
		}
	}
}

// TestHookPayloadTurnIDReadsTheVerifiedField pins the read of a verified
// field: the id when it is a usable turn id, "" otherwise.
func TestHookPayloadTurnIDReadsTheVerifiedField(t *testing.T) {
	for _, tc := range []struct {
		name    string
		field   string
		payload string
		want    string
	}{
		{"id", "turn_id", `{"turn_id":"t-1"}`, "t-1"},
		{"no field named", "", `{"turn_id":"t-1"}`, ""},
		{"field absent", "turn_id", `{"prompt_id":"p-1"}`, ""},
		{"not a string", "turn_id", `{"turn_id":7}`, ""},
		{"empty", "turn_id", `{"turn_id":""}`, ""},
		{"control character", "turn_id", `{"turn_id":"t\n1"}`, ""},
		{"too long", "turn_id", `{"turn_id":"` + strings.Repeat("a", MaxTurnIDLen+1) + `"}`, ""},
		{"not an object", "turn_id", `["t-1"]`, ""},
		{"not json", "turn_id", `turn_id=t-1`, ""},
		{"no payload", "turn_id", ``, ""},
	} {
		if got := hookPayloadTurnID(tc.field, []byte(tc.payload)); got != tc.want {
			t.Errorf("%s: hookPayloadTurnID = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestHookFamily pins the family a provider's name places its hooks in: the
// canonical one, with groq and cerebras on the opencode plugin and a claude
// spelling no other family claims read as claude.
func TestHookFamily(t *testing.T) {
	for _, tc := range []struct{ provider, want string }{
		{"claude", "claude"},
		{" Claude ", "claude"},
		{"claude-eco", "claude"},
		{"claude/tmux-cli", "claude"},
		{"opencode", "opencode"},
		{"groq", "opencode"},
		{"cerebras", "opencode"},
		{"opencode-claude", "opencode"},
		{"mimocode", "mimocode"},
		{"codex", "codex"},
		{"kiro", "kiro"},
		{"omp", "pi"},
		{"", ""},
	} {
		if got := HookFamily(tc.provider); got != tc.want {
			t.Errorf("HookFamily(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}
}

package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills a replacement that drifts from what interrupt_now does
// (interruptAndSubmitLocked): a family that cannot cancel a turn refuses it,
// a hard restart is skipped on ACP transport and in protocol mode, and the
// interrupted-turn reset runs only off ACP transport on a backend that has
// one (X11 D11).
func TestTurnReplacementFollowsInterruptNow(t *testing.T) {
	const (
		keys     = runtime.TurnInterruptKeystroke
		protocol = runtime.TurnInterruptProtocol
		refused  = runtime.TurnInterruptUnsupported
	)
	cases := []struct {
		name   string
		info   Info
		mode   runtime.TurnInterruptMode
		resets bool
		want   TurnReplacement
	}{
		{"claude", Info{Provider: "claude"}, keys, true, TurnReplacementInterrupt},
		{"claude in protocol mode", Info{Provider: "claude"}, protocol, false, TurnReplacementInterrupt},
		{"claude on a runtime that cannot interrupt", Info{Provider: "claude"}, refused, true, TurnReplacementNone},
		{"codex", Info{Provider: "codex"}, keys, true, TurnReplacementInterrupt},
		{"kimi", Info{Provider: "kimi"}, keys, false, TurnReplacementInterrupt},
		{"gemini on a backend that resets", Info{Provider: "gemini"}, keys, true, TurnReplacementInterruptReset},
		{"gemini on a backend that does not reset", Info{Provider: "gemini"}, keys, false, TurnReplacementInterrupt},
		{"gemini on acp transport", Info{Provider: "gemini", TransportMetadata: "acp"}, keys, true, TurnReplacementInterrupt},
		{"wrapped gemini", Info{Provider: "my-gemini", BuiltinAncestor: "gemini"}, keys, true, TurnReplacementInterruptReset},
		{"pi", Info{Provider: "pi"}, keys, true, TurnReplacementRestart},
		{"pi on a runtime that cannot interrupt", Info{Provider: "pi"}, refused, false, TurnReplacementRestart},
		{"pi in protocol mode", Info{Provider: "pi"}, protocol, false, TurnReplacementInterrupt},
		{"pi on acp transport", Info{Provider: "pi", TransportMetadata: "acp"}, keys, false, TurnReplacementInterrupt},
		{"pi on acp transport that cannot interrupt", Info{Provider: "pi", TransportMetadata: "acp"}, refused, false, TurnReplacementNone},
		{"pi by provider kind", Info{Provider: "my-pi", ProviderKind: "pi"}, keys, false, TurnReplacementRestart},
		{"antigravity", Info{Provider: "antigravity"}, keys, true, TurnReplacementNone},
		{"antigravity in protocol mode", Info{Provider: "antigravity"}, protocol, false, TurnReplacementNone},
		{"wrapped antigravity", Info{Provider: "custom-antigravity", BuiltinAncestor: "antigravity"}, keys, true, TurnReplacementNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := turnReplacementFor(tc.info, tc.mode, tc.resets); got != tc.want {
				t.Fatalf("turnReplacementFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// Kills a replacement that disagrees with the interrupt_now affordance the API
// reports: a turn can be replaced exactly when SubmissionCapabilities offers
// interrupt_now.
func TestTurnReplacementNoneIsWithdrawnInterruptNow(t *testing.T) {
	modes := []runtime.TurnInterruptMode{runtime.TurnInterruptKeystroke, runtime.TurnInterruptProtocol, runtime.TurnInterruptUnsupported}
	for _, info := range []Info{
		{Provider: "claude"},
		{Provider: "codex"},
		{Provider: "gemini"},
		{Provider: "pi"},
		{Provider: "kimi"},
		{Provider: "antigravity"},
		{Provider: "custom-antigravity", BuiltinAncestor: "antigravity"},
		{Provider: "pi", TransportMetadata: "acp"},
		{Provider: "claude", TransportMetadata: "acp"},
	} {
		meta := map[string]string{
			"provider":         info.Provider,
			"builtin_ancestor": info.BuiltinAncestor,
			"provider_kind":    info.ProviderKind,
			"transport":        info.TransportMetadata,
		}
		for _, mode := range modes {
			for _, resets := range []bool{false, true} {
				replaced := turnReplacementFor(info, mode, resets) != TurnReplacementNone
				offered := SubmissionCapabilitiesForRuntime(meta, true, mode, runtime.SteerNone).SupportsInterruptNow
				if replaced != offered {
					t.Errorf("%+v in mode %s (resets %v): replaceable %v, interrupt_now offered %v", info, mode, resets, replaced, offered)
				}
			}
		}
	}
}

// Kills a replacement composed from anything but the session's own row and
// its routed backend: RuntimeTurnTraits reports what interrupt_now does to
// this session there.
func TestRuntimeTurnTraitsReportTheSessionsReplacement(t *testing.T) {
	cases := []struct {
		name     string
		sp       runtime.Provider
		provider string
		want     TurnReplacement
	}{
		{"gemini on tmux, which resets", runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "gemini", TurnReplacementInterruptReset},
		{"gemini on subprocess, which does not", runtime.NewFakeProfile(runtime.ProfileSubprocess, runtime.FullFake{Fake: runtime.NewFake()}), "gemini", TurnReplacementInterrupt},
		{"pi on tmux", runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "pi", TurnReplacementRestart},
		{"pi on t3bridge, in protocol mode", runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: runtime.NewFake()}), "pi", TurnReplacementInterrupt},
		{"antigravity on tmux", runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), "antigravity", TurnReplacementNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, info := newTurnObservationSessionOf(t, tc.sp, tc.provider)
			if got := mgr.RuntimeTurnTraits(info).Replace; got != tc.want {
				t.Fatalf("RuntimeTurnTraits.Replace = %q, want %q", got, tc.want)
			}
		})
	}
}

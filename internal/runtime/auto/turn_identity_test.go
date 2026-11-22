package auto

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills an auto that answers turn observation for itself, swallows a failed
// read, or falls through to the other backend: a session routed to the ACP
// profile reads its capabilities and history there, a failed read stays an
// error, and a session routed to tmux reads nothing on every axis and
// ErrInteractionUnsupported, even though the ACP leaf has a script for it.
func TestTurnObservationForwardsToTheRoutedBackend(t *testing.T) {
	full := runtime.TurnCapabilities{
		Start: runtime.TurnStartRecorded, End: runtime.TurnEndRecorded,
		Fence: runtime.TurnFenceAtomic, Link: runtime.TurnLinkExact, Lossless: true,
	}
	acpLeaf := runtime.NewFake()
	acpLeaf.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{"acp": {full}, "tmux": {full}}
	acpLeaf.TurnHistoryScripts = map[string][]runtime.TurnHistory{
		"acp":  {{Current: runtime.TurnState{TurnID: "t2"}, Recent: []runtime.EndedTurn{{TurnID: "t1"}}}},
		"tmux": {{Current: runtime.TurnState{TurnID: "wrong"}}},
	}
	acpLeaf.TurnHistoryErrors = map[string][]error{"acp": {nil, runtime.ErrRuntimeUnavailable}}
	tmuxLeaf := runtime.NewFake()
	p := New(runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: tmuxLeaf}),
		runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: acpLeaf}))
	p.RouteACP("acp")
	ctx := t.Context()

	if got := p.TurnCapabilities("acp"); got != full {
		t.Fatalf("TurnCapabilities(acp) = %+v, want %+v", got, full)
	}
	h, err := p.TurnHistory(ctx, "acp")
	if err != nil || h.Current.TurnID != "t2" || len(h.Recent) != 1 || h.Recent[0].TurnID != "t1" {
		t.Fatalf("TurnHistory(acp) = %+v, %v; want t2 current after t1", h, err)
	}
	if _, err := p.TurnHistory(ctx, "acp"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("failed TurnHistory(acp) = %v, want ErrRuntimeUnavailable", err)
	}
	if n := acpLeaf.CountCalls("TurnHistory", "acp"); n != 2 {
		t.Fatalf("acp leaf TurnHistory calls = %d, want the two forwarded reads", n)
	}

	if got := p.TurnCapabilities("tmux"); got != (runtime.TurnCapabilities{}).Normalized() {
		t.Errorf("TurnCapabilities(tmux) = %+v, want nothing on every axis", got)
	}
	if _, err := p.TurnHistory(ctx, "tmux"); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Errorf("TurnHistory(tmux) = %v, want ErrInteractionUnsupported", err)
	}
	if n := acpLeaf.CountCalls("TurnHistory", "tmux"); n != 0 {
		t.Errorf("TurnHistory(tmux) reached the acp leaf %d times, want none", n)
	}
	if calls := tmuxLeaf.SnapshotCalls(); len(calls) != 0 {
		t.Errorf("tmux leaf calls = %+v, want none", calls)
	}
}

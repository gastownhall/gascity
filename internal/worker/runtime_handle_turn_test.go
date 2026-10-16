package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func newTurnRuntimeHandle(t *testing.T, sp runtime.Provider) *RuntimeHandle {
	t.Helper()
	if err := sp.Start(context.Background(), "turn-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "turn-worker", ProviderName: "claude"})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	return h
}

func TestRuntimeHandleTurnsComeFromItsProvider(t *testing.T) {
	fake := runtime.NewFake()
	h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: fake}))
	at := time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC)
	want := runtime.TurnHistory{Current: runtime.TurnState{TurnID: "acp-1"}, CurrentStart: at, ObservedAt: at}
	fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{"turn-worker": {fullTurnCaps}}
	fake.TurnHistoryScripts = map[string][]runtime.TurnHistory{"turn-worker": {want}}

	caps, err := h.TurnCapabilities(context.Background())
	if err != nil {
		t.Fatalf("TurnCapabilities: %v", err)
	}
	if got := capsRow(caps); got != "protocol recorded/recorded/atomic/exact/true keystroke interrupt" {
		t.Fatalf("TurnCapabilities = %q, want the provider's, as a protocol source", got)
	}
	got, err := h.CurrentTurn(context.Background())
	if err != nil {
		t.Fatalf("CurrentTurn: %v", err)
	}
	assertTurnHistory(t, got, want)

	fake.TurnHistoryErrors = map[string][]error{"turn-worker": {runtime.ErrRuntimeUnavailable}}
	if _, err := h.CurrentTurn(context.Background()); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("CurrentTurn on a failed read = %v, want ErrRuntimeUnavailable", err)
	}
}

// A runtime handle has no transcript: tmux, which is a transcript source for a
// session handle, is none here.
func TestRuntimeHandleTmuxReportsNoTurnSource(t *testing.T) {
	h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}))
	caps, err := h.TurnCapabilities(context.Background())
	if err != nil {
		t.Fatalf("TurnCapabilities: %v", err)
	}
	if got := capsRow(caps); got != "none none/none/none/none/false keystroke interrupt" {
		t.Fatalf("TurnCapabilities = %q, want none", got)
	}
	if _, err := h.CurrentTurn(context.Background()); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
		t.Fatalf("CurrentTurn = %v, want ErrTurnIdentityUnsupported", err)
	}
	if _, err := h.WatchTurns(context.Background()); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
		t.Fatalf("WatchTurns = %v, want ErrTurnIdentityUnsupported", err)
	}
}

func TestRuntimeHandleT3BridgeTurnSourceIsDerived(t *testing.T) {
	fake := runtime.NewFake()
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})
	h := newTurnRuntimeHandle(t, sp)
	fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{"turn-worker": {v1TurnCaps}}
	want := "none none/none/none/none/false protocol interrupt"
	if _, ok := sp.(runtime.TurnObservationProvider); ok {
		want = "protocol recorded/recorded/checked/correlated/false protocol interrupt"
	}
	caps, err := h.TurnCapabilities(context.Background())
	if err != nil || capsRow(caps) != want {
		t.Fatalf("TurnCapabilities = %q, %v; want %q", capsRow(caps), err, want)
	}
}

func TestRuntimeHandleWatchTurns(t *testing.T) {
	backend := newTurnScriptBackend(true)
	h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileACP, backend))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := h.WatchTurns(ctx)
	if err != nil {
		t.Fatalf("WatchTurns: %v", err)
	}
	if first := receiveTurnUpdate(t, ch); first.Err != nil || first.History.Current.Active() {
		t.Fatalf("first update = %+v, want the idle history", first)
	}
	backend.set(runtime.TurnHistory{Current: runtime.TurnState{TurnID: "acp-1"}}, nil)
	backend.state.events <- runtime.TurnEvent{Kind: runtime.TurnEventStarted, Session: "turn-worker", TurnID: "acp-1"}
	if got := receiveTurnUpdate(t, ch); got.History.Current.TurnID != "acp-1" {
		t.Fatalf("update after the hint = %+v, want acp-1 running", got)
	}
	cancel()
	assertWatchCloses(t, ch)
}

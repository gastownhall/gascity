package hybrid

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills a hybrid that answers turn reads for itself, drops them, or reaches a
// backend without the capability: a session routed to the t3bridge profile
// reads and interrupts its turns there, and one routed to tmux answers
// ErrInteractionUnsupported for every method.
func TestTurnFenceForwardsToTheRoutedBackend(t *testing.T) {
	t3 := runtime.NewFake()
	t3.TurnScripts = map[string][]string{"t3": {"t1", "t1", "t1", ""}}
	tmuxLeaf := runtime.NewFake()
	p := New(runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: tmuxLeaf}),
		runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: t3}), func(name string) bool { return name == "t3" })
	ctx := t.Context()

	if st, err := p.ObserveTurn(ctx, "t3"); err != nil || st.TurnID != "t1" {
		t.Fatalf("ObserveTurn(t3) = %+v, %v; want t1", st, err)
	}
	if ack, err := p.InterruptTurn(ctx, "t3", "t1"); err != nil || ack.Fenced {
		t.Fatalf("InterruptTurn(t3) = %+v, %v; want sent, unfenced", ack, err)
	}
	if st, err := p.WaitTurnEnded(ctx, "t3", "t1", time.Second); err != nil || st.Active() {
		t.Fatalf("WaitTurnEnded(t3) = %+v, %v; want ended", st, err)
	}
	if n := len(t3.SnapshotCalls()); n != 3 {
		t.Fatalf("t3bridge leaf calls = %+v, want the three forwarded calls", t3.SnapshotCalls())
	}

	if _, err := p.ObserveTurn(ctx, "tmux"); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Errorf("ObserveTurn(tmux) = %v, want ErrInteractionUnsupported", err)
	}
	if _, err := p.InterruptTurn(ctx, "tmux", "t1"); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Errorf("InterruptTurn(tmux) = %v, want ErrInteractionUnsupported", err)
	}
	if _, err := p.WaitTurnEnded(ctx, "tmux", "t1", time.Second); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Errorf("WaitTurnEnded(tmux) = %v, want ErrInteractionUnsupported", err)
	}
	if calls := tmuxLeaf.SnapshotCalls(); len(calls) != 0 {
		t.Errorf("tmux leaf calls = %+v, want none", calls)
	}
}

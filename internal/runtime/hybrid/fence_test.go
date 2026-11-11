package hybrid

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills a hybrid that fences input itself, drops the fence, or falls through
// to the other backend: a session routed to a fencing backend gets every
// fenced call there, and one routed to a backend that cannot fence is refused
// with no call on either backend.
func TestFencedInputForwardsToTheRoutedBackend(t *testing.T) {
	fencing, unfenced := runtime.NewFake(), runtime.NewFake()
	unfenced.InputFencingValue = runtime.InputFencingNone
	p := New(fencing, unfenced, func(name string) bool { return name == "acp" })
	ctx := t.Context()
	fence := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	env := map[string]string{"GC_INSTANCE_TOKEN": "tok"}
	for _, name := range []string{"local", "acp"} {
		if err := p.Start(ctx, name, runtime.Config{Env: env}); err != nil {
			t.Fatal(err)
		}
	}
	fencing.Calls, unfenced.Calls = nil, nil

	if got := p.InputFencing("local"); got != runtime.InputFencingAtomic {
		t.Errorf("InputFencing(local) = %q, want atomic", got)
	}
	if got := p.InputFencing("acp"); got != runtime.InputFencingNone {
		t.Errorf("InputFencing(acp) = %q, want none", got)
	}
	if err := p.NudgeFenced(ctx, "local", runtime.TextContent("hi"), runtime.FencedNudge{Fence: fence, Immediate: true, DeliveryKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := p.SendKeysFenced(ctx, "local", fence, "Enter"); err != nil {
		t.Fatal(err)
	}
	if err := p.InterruptFenced(ctx, "local", fence); err != nil {
		t.Fatal(err)
	}
	calls := fencing.SnapshotCalls()
	if len(calls) != 3 || calls[0].Method != "NudgeNow" || calls[0].DeliveryKey != "k" || calls[1].Method != "SendKeys" || calls[2].Method != "Interrupt" {
		t.Fatalf("fencing leaf calls = %+v", calls)
	}
	for _, c := range calls {
		if c.Fence != fence {
			t.Errorf("%s carries %v, want the fence", c.Method, c.Fence)
		}
	}

	fencing.Calls = nil
	for _, err := range []error{
		p.NudgeFenced(ctx, "acp", runtime.TextContent("hi"), runtime.FencedNudge{Fence: fence}),
		p.SendKeysFenced(ctx, "acp", fence, "Enter"),
		p.InterruptFenced(ctx, "acp", fence),
	} {
		if !errors.Is(err, runtime.ErrInputFenceUnsupported) {
			t.Errorf("fenced call routed to a non-fencing backend = %v, want ErrInputFenceUnsupported", err)
		}
	}
	if n := len(fencing.SnapshotCalls()) + len(unfenced.SnapshotCalls()); n != 0 {
		t.Errorf("refused calls reached a backend: %+v %+v", fencing.SnapshotCalls(), unfenced.SnapshotCalls())
	}
}

// The ctx-fenced input methods (ClearInput, DismissKnownDialogs,
// ResetInterruptedTurn) routed to a backend that cannot fence are refused,
// never forwarded to run unfenced.
func TestCtxFencedInputRefusedForNonFencingBackend(t *testing.T) {
	leaf := runtime.NewFake()
	p := New(runtime.NewFake(), &unfencedInputLeaf{Fake: leaf}, func(name string) bool { return name == "acp" })
	ctx := runtime.WithInputFence(t.Context(), runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"})
	for name, err := range map[string]error{
		"ClearInput":           p.ClearInput(ctx, "acp", 0),
		"DismissKnownDialogs":  p.DismissKnownDialogs(ctx, "acp", 0),
		"ResetInterruptedTurn": p.ResetInterruptedTurn(ctx, "acp"),
	} {
		if !errors.Is(err, runtime.ErrInputFenceUnsupported) {
			t.Errorf("%s = %v, want ErrInputFenceUnsupported", name, err)
		}
	}
	if calls := leaf.SnapshotCalls(); len(calls) != 0 {
		t.Fatalf("refused calls reached the backend: %+v", calls)
	}
	// Unfenced, they forward as before.
	if err := p.ClearInput(context.Background(), "acp", 0); err != nil {
		t.Fatal(err)
	}
}

// unfencedInputLeaf is a runtime with the ctx-taking input methods but no
// FencedInputProvider, as ACP or subprocess: it would ignore a ctx fence.
type unfencedInputLeaf struct{ *runtime.Fake }

func (l *unfencedInputLeaf) InputFencing(string) runtime.InputFencing {
	return runtime.InputFencingNone
}

func (l *unfencedInputLeaf) ClearInput(_ context.Context, name string, d time.Duration) error {
	return l.Fake.ClearInput(context.Background(), name, d)
}

func (l *unfencedInputLeaf) DismissKnownDialogs(_ context.Context, name string, d time.Duration) error {
	return l.Fake.DismissKnownDialogs(context.Background(), name, d)
}

func (l *unfencedInputLeaf) ResetInterruptedTurn(_ context.Context, name string) error {
	return l.Fake.ResetInterruptedTurn(context.Background(), name)
}

package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// Kills a helper that sends a fenced request unfenced anywhere: for every
// provider shape (a fencing fake, a fake that cannot fence, a provider
// without the capability, and auto and hybrid routing to each), every op,
// delivery mode and key, a fenced call either records exactly one input
// carrying the fence (and the delivery key), or is refused with
// ErrInputFenceUnsupported and records no input at all.
func TestFencedHelpersNeverDeliverUnfenced(t *testing.T) {
	fence := runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok"}
	env := map[string]string{runtime.RuntimeEpochEnv: "2", runtime.ContinuationEpochEnv: "3", runtime.InstanceTokenEnv: "tok"}
	type shape struct {
		build    func(leaf *runtime.Fake) runtime.Provider
		canFence bool
	}
	shapes := map[string]shape{
		"fake atomic": {func(f *runtime.Fake) runtime.Provider { return f }, true},
		"fake none": {func(f *runtime.Fake) runtime.Provider {
			f.InputFencingValue = runtime.InputFencingNone
			return f
		}, false},
		"no capability": {func(f *runtime.Fake) runtime.Provider { return plainProvider{f} }, false},
		"auto to fencing": {func(f *runtime.Fake) runtime.Provider {
			other := runtime.NewFake()
			other.InputFencingValue = runtime.InputFencingNone
			return auto.New(f, other)
		}, true},
		"auto to none": {func(f *runtime.Fake) runtime.Provider {
			f.InputFencingValue = runtime.InputFencingNone
			p := auto.New(runtime.NewFake(), f)
			p.RouteACP("s")
			return p
		}, false},
		"hybrid to fencing": {func(f *runtime.Fake) runtime.Provider {
			return hybrid.New(runtime.NewFake(), f, func(string) bool { return true })
		}, true},
		"hybrid to none": {func(f *runtime.Fake) runtime.Provider {
			return hybrid.New(plainProvider{runtime.NewFake()}, plainProvider{f}, func(string) bool { return true })
		}, false},
	}
	inputMethods := map[string]bool{"Nudge": true, "NudgeNow": true, "SendKeys": true, "Interrupt": true}
	for name, sh := range shapes {
		for _, op := range []string{"nudge", "keys", "interrupt"} {
			for _, immediate := range []bool{false, true} {
				for _, key := range []string{"", "k"} {
					leaf := runtime.NewFake()
					p := sh.build(leaf)
					if err := leaf.Start(context.Background(), "s", runtime.Config{Env: env}); err != nil {
						t.Fatal(err)
					}
					leaf.Calls = nil
					ctx := runtime.WithInputFence(context.Background(), fence)
					if key != "" {
						ctx = runtime.WithNudgeDeliveryKey(ctx, key)
					}
					var err error
					switch op {
					case "nudge":
						err = runtime.NudgeWithContext(ctx, p, "s", runtime.TextContent("hi"), immediate)
					case "keys":
						err = runtime.SendKeysWithContext(ctx, p, "s", "Enter")
					case "interrupt":
						err = runtime.InterruptWithContext(ctx, p, "s")
					}
					var inputs []runtime.Call
					for _, c := range leaf.SnapshotCalls() {
						if inputMethods[c.Method] {
							inputs = append(inputs, c)
						}
					}
					label := name + "/" + op
					if !sh.canFence {
						if !errors.Is(err, runtime.ErrInputFenceUnsupported) || len(inputs) != 0 {
							t.Errorf("%s: err %v, inputs %+v; want ErrInputFenceUnsupported and none", label, err, inputs)
						}
						continue
					}
					if err != nil || len(inputs) != 1 || inputs[0].Fence != fence {
						t.Errorf("%s: err %v, inputs %+v; want one input carrying the fence", label, err, inputs)
						continue
					}
					if op == "nudge" && inputs[0].DeliveryKey != key {
						t.Errorf("%s: delivery key %q, want %q", label, inputs[0].DeliveryKey, key)
					}
				}
			}
		}
	}
}

// Kills a helper that sends under a fence that names no incarnation.
func TestFencedHelpersRefuseAnInvalidFence(t *testing.T) {
	f := runtime.NewFake()
	ctx := runtime.WithInputFence(context.Background(), runtime.InputFence{Generation: 1, Epoch: 1})
	for _, err := range []error{
		runtime.NudgeWithContext(ctx, f, "s", runtime.TextContent("hi"), false),
		runtime.SendKeysWithContext(ctx, f, "s", "Enter"),
		runtime.InterruptWithContext(ctx, f, "s"),
	} {
		if !errors.Is(err, runtime.ErrInvalidInputFence) {
			t.Errorf("err = %v, want ErrInvalidInputFence", err)
		}
	}
	if calls := f.SnapshotCalls(); len(calls) != 0 {
		t.Errorf("calls = %+v, want none", calls)
	}
}

// Kills a fake that delivers a fenced input into another incarnation, a
// missing session, a dead one or one that names none.
func TestFakeFenceRefusesEveryMiss(t *testing.T) {
	ctx := context.Background()
	fence := runtime.InputFence{Generation: 1, Epoch: 1, Token: "a"}
	f := runtime.NewFake()
	_ = f.Start(ctx, "other", runtime.Config{Env: map[string]string{"GC_INSTANCE_TOKEN": "b"}})
	_ = f.Start(ctx, "bare", runtime.Config{})
	_ = f.Start(ctx, "zombie", runtime.Config{Env: map[string]string{"GC_INSTANCE_TOKEN": "a"}})
	f.Zombies["zombie"] = true
	f.Calls = nil
	for name, want := range map[string]string{
		"other":   runtime.InputFence{Generation: 1, Epoch: 1, Token: "b"}.Digest(),
		"bare":    runtime.FenceFoundUnproven,
		"zombie":  runtime.FenceFoundDead,
		"missing": runtime.FenceFoundAbsent,
	} {
		err := f.NudgeFenced(ctx, name, runtime.TextContent("hi"), runtime.FencedNudge{Fence: fence})
		var fm *runtime.FenceMismatchError
		if !errors.As(err, &fm) || fm.Found != want || fm.Partial {
			t.Errorf("%s: %v, want a mismatch finding %s", name, err, want)
		}
		if err := f.ClearInput(runtime.WithInputFence(ctx, fence), name, 0); !errors.Is(err, runtime.ErrFenceMismatch) {
			t.Errorf("%s: ClearInput = %v, want ErrFenceMismatch", name, err)
		}
	}
	for _, c := range f.SnapshotCalls() {
		if c.Method != "FenceMismatch" {
			t.Errorf("a missed fenced call recorded %s", c.Method)
		}
	}
}

// maxUnfencedBackends bounds the production backends that cannot fence
// session input. Lower it as backends gain a fence (OJ-12a2: acp,
// subprocess, t3bridge); never raise it.
const maxUnfencedBackends = 7

// Kills a backend that loses its input fence, and a new backend that ships
// without one: tmux fences atomically, every other production backend
// reports none (the conformance cases skip it), and the set of those only
// shrinks.
func TestInputFencingByProductionBackend(t *testing.T) {
	unfenced := 0
	for p, backend := range productionBackends(t) {
		got := runtime.InputFencingFor(backend, "s")
		want := runtime.InputFencingNone
		if p == runtime.ProfileTmux {
			want = runtime.InputFencingAtomic
		}
		if got != want {
			t.Errorf("%s fences input %q, want %q", p, got, want)
		}
		if got == runtime.InputFencingNone {
			unfenced++
		}
	}
	if unfenced > maxUnfencedBackends {
		t.Errorf("%d production backends cannot fence input, more than the ratchet's %d", unfenced, maxUnfencedBackends)
	}
}

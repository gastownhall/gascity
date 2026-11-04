package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

type bootstrapRuntimeFixture struct {
	*runtime.Fake
	request runtime.BootstrapInputRequest
	name    string
	proof   *runtime.BootstrapInputProof
	err     error
}

func (p *bootstrapRuntimeFixture) ObserveBootstrapInput(_ context.Context, name string, req runtime.BootstrapInputRequest) (*runtime.BootstrapInputProof, error) {
	p.name, p.request = name, req
	return p.proof, p.err
}

func TestBootstrapInputObserverUsesExactRuntimeFamilyAndFence(t *testing.T) {
	fence := runtime.InputFence{Generation: 1, Epoch: 1, Token: "fresh-instance"}
	proof := &runtime.BootstrapInputProof{Fence: fence, Token: "opaque-credit"}
	sp := &bootstrapRuntimeFixture{Fake: runtime.NewFake(), proof: proof}
	h, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "exact-pane", ProviderName: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ObserveBootstrapInput(context.Background(), h, fence)
	if err != nil || got != proof || sp.name != "exact-pane" || sp.request.Family != "codex" || sp.request.Fence != fence {
		t.Fatalf("observation=%+v err=%v name=%q request=%+v", got, err, sp.name, sp.request)
	}
	if calls := typedCalls(sp.Fake, "exact-pane"); len(calls) != 0 {
		t.Fatalf("read caused input: %+v", calls)
	}
	sp.err = runtime.ErrRuntimeUnavailable
	if _, err := ObserveBootstrapInput(context.Background(), h, fence); !errors.Is(err, sp.err) {
		t.Fatalf("observer error=%v", err)
	}
	unsupported, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: runtime.NewFake(), SessionName: "unsupported"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ObserveBootstrapInput(context.Background(), unsupported, fence); err != nil || got != nil {
		t.Fatalf("unsupported=%+v %v", got, err)
	}
}

func TestBootstrapInputRequestOwnsItsContextAndRequiresAtomicLiveDelivery(t *testing.T) {
	fence := runtime.InputFence{Generation: 1, Epoch: 1, Token: "fresh-instance"}
	proof := &runtime.BootstrapInputProof{Fence: fence, Token: "opaque-credit"}
	base := NudgeRequest{BootstrapInput: proof, Fence: fence, Wake: NudgeWakeLiveOnly, DeliveryKey: "one-input"}
	ctx := nudgeDeliveryContext(context.Background(), base)
	if got, ok := runtime.BootstrapInputFrom(ctx); !ok || *got != *proof {
		t.Fatal("request credit did not reach context")
	}
	cleared := nudgeDeliveryContext(ctx, NudgeRequest{})
	if _, ok := runtime.BootstrapInputFrom(cleared); ok {
		t.Fatal("unrequested inherited credit reached another delivery")
	}
	for _, fencing := range []runtime.InputFencing{runtime.InputFencingNone, runtime.InputFencingChecked, runtime.InputFencingAtomic} {
		req := base
		req.AllowUnfenced = true
		got, unfenced, err := fencedNudgeContext(ctx, req, func() (runtime.InputFencing, error) { return fencing, nil })
		if fencing != runtime.InputFencingAtomic {
			if !errors.Is(err, runtime.ErrInputFenceUnsupported) || unfenced {
				t.Fatalf("non-atomic %s accepted: %v %v", fencing, unfenced, err)
			}
		} else if f, ok := runtime.InputFenceFrom(got); err != nil || unfenced || !ok || f != fence {
			t.Fatalf("atomic proof refused: %v %v", unfenced, err)
		}
	}
}

func TestBootstrapInputMalformedOrReplayRequestRefusesBeforeRuntimeProbe(t *testing.T) {
	fence := runtime.InputFence{Generation: 1, Epoch: 1, Token: "fresh-instance"}
	tests := map[string]func(*NudgeRequest){
		"zero fence":   func(r *NudgeRequest) { r.Fence = runtime.InputFence{} },
		"generation":   func(r *NudgeRequest) { r.Fence.Generation++ },
		"epoch":        func(r *NudgeRequest) { r.Fence.Epoch++ },
		"token":        func(r *NudgeRequest) { r.Fence.Token = "another-incarnation" },
		"unkeyed":      func(r *NudgeRequest) { r.DeliveryKey = "" },
		"empty credit": func(r *NudgeRequest) { r.BootstrapInput.Token = "" },
		"redelivery":   func(r *NudgeRequest) { r.Redelivery = true },
		"immediate":    func(r *NudgeRequest) { r.Delivery = NudgeDeliveryImmediate },
		"wait idle":    func(r *NudgeRequest) { r.Delivery = NudgeDeliveryWaitIdle },
		"wake":         func(r *NudgeRequest) { r.Wake = NudgeWakePolicy("start") },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			req := NudgeRequest{BootstrapInput: &runtime.BootstrapInputProof{Fence: fence, Token: "opaque-credit"}, Fence: fence, Wake: NudgeWakeLiveOnly, DeliveryKey: "one-input"}
			change(&req)
			probed := false
			_, _, err := fencedNudgeContext(context.Background(), req, func() (runtime.InputFencing, error) { probed = true; return runtime.InputFencingAtomic, nil })
			if err == nil || probed {
				t.Fatalf("malformed request reached runtime probe: probed=%v err=%v", probed, err)
			}
		})
	}
}

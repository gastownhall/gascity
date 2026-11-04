package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestBootstrapInputContextFreezesProofAndClearsExplicitly(t *testing.T) {
	proof := &BootstrapInputProof{Fence: InputFence{Generation: 1, Epoch: 1, Token: "instance"}, Token: "credit"}
	ctx := WithBootstrapInput(context.Background(), proof)
	proof.Token = "changed after queue admission"
	got, ok := BootstrapInputFrom(ctx)
	if !ok || got.Token != "credit" {
		t.Fatal("queued bootstrap requirement is mutable")
	}
	if _, ok := BootstrapInputFrom(WithBootstrapInput(ctx, nil)); ok {
		t.Fatal("explicitly cleared proof survived")
	}
}

func TestBootstrapInputUnsupportedDoesNotInferIdle(t *testing.T) {
	p := NewFake()
	got, err := ObserveBootstrapInput(t.Context(), p, "session", BootstrapInputRequest{Family: "codex"})
	if err != nil || got != nil || len(p.SnapshotCalls()) != 0 {
		t.Fatalf("unsupported runtime invented an observation: %+v %v", got, err)
	}
}

func TestBootstrapInputProofRefusesUnfencedNudgeBeforeEffects(t *testing.T) {
	p := NewFake()
	ctx := WithBootstrapInput(t.Context(), &BootstrapInputProof{Fence: InputFence{Generation: 1, Epoch: 1, Token: "instance"}, Token: "credit"})
	if _, err := NudgeWithReceipt(ctx, p, "session", TextContent("first"), false); !errors.Is(err, ErrInputNotSent) {
		t.Fatalf("first-input proof became unfenced input: %v", err)
	}
	if len(p.SnapshotCalls()) != 0 {
		t.Fatal("unfenced input reached runtime")
	}
}

func TestBootstrapInputOriginExcludedFromFingerprints(t *testing.T) {
	for name, base := range goldenFixtures() {
		fresh := base
		fresh.BootstrapInputOrigin = BootstrapInputFreshBuiltin
		for _, fp := range []func(Config) string{ConfigFingerprint, CoreFingerprint, LiveFingerprint, ProvisionFingerprint, LaunchFingerprint} {
			if fp(fresh) != fp(base) {
				t.Fatalf("%s first-input provenance changed config identity", name)
			}
		}
		if !reflect.DeepEqual(CoreFingerprintBreakdown(fresh), CoreFingerprintBreakdown(base)) {
			t.Fatal("first-input provenance changed restart breakdown")
		}
	}
}

type bootstrapRouteObserver struct {
	*Fake
	observations int
}

func (p *bootstrapRouteObserver) ObserveBootstrapInput(_ context.Context, _ string, req BootstrapInputRequest) (*BootstrapInputProof, error) {
	p.observations++
	return &BootstrapInputProof{Fence: req.Fence, Token: "only-known-route"}, nil
}

type bootstrapUnknownRouter struct{ Provider }

func (p bootstrapUnknownRouter) RouteFor(string) Route {
	return Route{Backend: Backend{Label: "unproven", Provider: p.Provider}, Known: false}
}

func TestBootstrapInputUnknownRouterDoesNotAdvertiseFreshBoundary(t *testing.T) {
	leaf := &bootstrapRouteObserver{Fake: NewFake()}
	req := BootstrapInputRequest{Family: "codex", Fence: InputFence{Generation: 1, Epoch: 1, Token: "instance"}}
	proof, err := ObserveBootstrapInput(t.Context(), bootstrapUnknownRouter{Provider: leaf}, "session", req)
	if err != nil || proof != nil || leaf.observations != 0 {
		t.Fatalf("unknown route invented a fresh boundary: %+v %v observations=%d", proof, err, leaf.observations)
	}
	proof, err = ObserveBootstrapInput(t.Context(), leaf, "session", req)
	if err != nil || proof == nil || leaf.observations != 1 {
		t.Fatal("known direct leaf lost its optional observation")
	}
}

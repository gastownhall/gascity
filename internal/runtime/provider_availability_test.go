package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

type availabilityLeaf struct {
	*runtime.Fake
	calls   []string
	request runtime.ProviderAvailabilityRequest
}

func (p *availabilityLeaf) ObserveProviderAvailability(_ context.Context, name string, req runtime.ProviderAvailabilityRequest) *runtime.ProviderAvailability {
	p.calls = append(p.calls, name)
	p.request = req
	return &runtime.ProviderAvailability{State: "unavailable", Code: "connection_restore_failed", Source: "native_ui", RuntimeGeneration: req.Fence.Digest()}
}

func TestProviderAvailabilityRoutedBackendIsTheOnlySource(t *testing.T) {
	left, right := &availabilityLeaf{Fake: runtime.NewFake()}, &availabilityLeaf{Fake: runtime.NewFake()}
	a := auto.New(left, right)
	a.SeedRoutes([]string{"acp"})
	h := hybrid.New(left, right, func(name string) bool { return strings.HasPrefix(name, "remote") })
	req := runtime.ProviderAvailabilityRequest{Family: "codex", Fence: runtime.InputFence{Generation: 2, Epoch: 3, Token: "fixture-token"}}
	for _, tc := range []struct {
		p    runtime.Provider
		name string
	}{{a, "default"}, {a, "acp"}, {h, "local"}, {h, "remote"}} {
		got := runtime.ObserveProviderAvailability(context.Background(), tc.p, tc.name, req)
		if got == nil || got.RuntimeGeneration != req.Fence.Digest() {
			t.Fatalf("routed %s availability=%+v", tc.name, got)
		}
	}
	if strings.Join(left.calls, ",") != "default,local" || strings.Join(right.calls, ",") != "acp,remote" || left.request != req || right.request != req {
		t.Fatalf("routing/fence changed: left=%v right=%v", left.calls, right.calls)
	}
}

func TestProviderAvailabilityUnsupportedOrUnknownRouteDoesNotFallBack(t *testing.T) {
	supported := &availabilityLeaf{Fake: runtime.NewFake()}
	unsupported := runtime.NewFake()
	a := auto.New(supported, unsupported)
	req := runtime.ProviderAvailabilityRequest{Family: "codex", Fence: runtime.InputFence{Generation: 2, Epoch: 3, Token: "fixture-token"}}
	if got := runtime.ObserveProviderAvailability(context.Background(), a, "not-yet-seeded", req); got != nil {
		t.Fatalf("unknown route=%+v", got)
	}
	a.SeedRoutes([]string{"acp"})
	h := hybrid.New(supported, unsupported, func(string) bool { return true })
	for _, tc := range []runtime.Provider{unsupported, a, h} {
		if got := runtime.ObserveProviderAvailability(context.Background(), tc, "acp", req); got != nil {
			t.Fatalf("unsupported leaf borrowed another backend: %+v", got)
		}
	}
	if len(supported.calls) != 0 {
		t.Fatalf("unsupported/unknown read reached another backend: %v", supported.calls)
	}
}

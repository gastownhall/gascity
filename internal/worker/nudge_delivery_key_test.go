package worker

import (
	"context"
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"

	"github.com/gastownhall/gascity/internal/runtime"
)

// A runtime-only handle carries the caller's delivery key to a provider that
// deduplicates keyed deliveries.
func TestRuntimeHandleNudgePassesTheDeliveryKey(t *testing.T) {
	fake := runtime.NewFake()
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})
	if err := sp.Start(context.Background(), "keyed-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	handle, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "keyed-worker", ProviderName: "stub"})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	ctx := runtime.WithNudgeDeliveryKey(context.Background(), "batch-1")
	result, err := handle.Nudge(ctx, handleActor(t, handle, sessionpkg.ActorAgent), NudgeRequest{Text: "hello", Wake: NudgeWakeLiveOnly})
	if err != nil || !result.Delivered {
		t.Fatalf("Nudge = %+v, %v; want delivered", result, err)
	}
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			if call.DeliveryKey != "batch-1" {
				t.Fatalf("nudge delivery key = %q, want batch-1", call.DeliveryKey)
			}
			return
		}
	}
	t.Fatalf("calls = %#v, want a Nudge", fake.Calls)
}

package runtime_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestNudgeWithDeliveryKeyPassesTheKeyToAKeyedProvider(t *testing.T) {
	fake := runtime.FullFake{Fake: runtime.NewFake()}
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, fake)
	if _, ok := sp.(runtime.KeyedNudgeProvider); !ok {
		t.Fatal("t3bridge profile is not a KeyedNudgeProvider")
	}
	ctx := runtime.WithNudgeDeliveryKey(context.Background(), "batch-1")
	if err := runtime.NudgeWithDeliveryKey(ctx, sp, "s", runtime.TextContent("hi")); err != nil {
		t.Fatalf("NudgeWithDeliveryKey: %v", err)
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Method != "Nudge" || fake.Calls[0].DeliveryKey != "batch-1" {
		t.Fatalf("calls = %+v, want one Nudge carrying delivery key batch-1", fake.Calls)
	}
}

func TestNudgeWithDeliveryKeyFallsBackToNudge(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		sp   func(*runtime.Fake) runtime.Provider
	}{
		{
			name: "no key",
			ctx:  context.Background(),
			sp: func(f *runtime.Fake) runtime.Provider {
				return runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: f})
			},
		},
		{
			name: "provider without keyed delivery",
			ctx:  runtime.WithNudgeDeliveryKey(context.Background(), "batch-1"),
			sp:   func(f *runtime.Fake) runtime.Provider { return f },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := runtime.NewFake()
			if err := runtime.NudgeWithDeliveryKey(tc.ctx, tc.sp(fake), "s", runtime.TextContent("hi")); err != nil {
				t.Fatalf("NudgeWithDeliveryKey: %v", err)
			}
			if len(fake.Calls) != 1 || fake.Calls[0].Method != "Nudge" || fake.Calls[0].DeliveryKey != "" {
				t.Fatalf("calls = %+v, want one plain Nudge", fake.Calls)
			}
		})
	}
}

func TestNudgeDeliveryKeyRoundTrip(t *testing.T) {
	if got := runtime.NudgeDeliveryKey(context.Background()); got != "" {
		t.Fatalf("NudgeDeliveryKey(empty ctx) = %q, want empty", got)
	}
	ctx := runtime.WithNudgeDeliveryKey(context.Background(), "k")
	if got := runtime.NudgeDeliveryKey(ctx); got != "k" {
		t.Fatalf("NudgeDeliveryKey = %q, want k", got)
	}
}

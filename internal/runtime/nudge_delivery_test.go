package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
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

// receiptFence is the incarnation startFenced starts session s as.
var receiptFence = runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok"}

// startFenced starts session s on f as receiptFence's incarnation and
// forgets the call.
func startFenced(t *testing.T, f *runtime.Fake) {
	t.Helper()
	env := map[string]string{runtime.RuntimeEpochEnv: "2", runtime.ContinuationEpochEnv: "3", runtime.InstanceTokenEnv: "tok"}
	if err := f.Start(context.Background(), "s", runtime.Config{Env: env}); err != nil {
		t.Fatal(err)
	}
	f.Calls = nil
}

// inputCalls are f's recorded nudges.
func inputCalls(f *runtime.Fake) []runtime.Call {
	var out []runtime.Call
	for _, c := range f.SnapshotCalls() {
		if c.Method == "Nudge" || c.Method == "NudgeNow" {
			out = append(out, c)
		}
	}
	return out
}

// Kills a funnel that drops the keyed leaf's receipt (M1).
func TestNudgeWithReceiptReturnsTheKeyedLeafsReceipt(t *testing.T) {
	fake := runtime.NewFake()
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})
	ctx := runtime.WithNudgeDeliveryKey(context.Background(), "gc:op-1:1")
	r, err := runtime.NudgeWithReceipt(ctx, sp, "s", runtime.TextContent("hi"), false)
	if err != nil {
		t.Fatalf("NudgeWithReceipt: %v", err)
	}
	held := fake.KeyedDeliveries["s"]["gc:op-1:1"]
	if r.IsZero() || r != held {
		t.Fatalf("receipt = %+v, want the held %+v", r, held)
	}
	// An unkeyed nudge reports no receipt.
	r, err = runtime.NudgeWithReceipt(context.Background(), sp, "s", runtime.TextContent("hi"), false)
	if err != nil || !r.IsZero() {
		t.Fatalf("unkeyed NudgeWithReceipt = %+v, %v; want no receipt", r, err)
	}
}

// Kills a slot handed to a leaf that does not deduplicate (M2): the tmux
// profile over the same FullFake reports no receipt and records the same
// call as before.
func TestNudgeWithReceiptFencedFillsTheSlotOnlyForAKeyedLeaf(t *testing.T) {
	for _, tc := range []struct {
		profile runtime.Profile
		want    bool
	}{
		{runtime.ProfileT3Bridge, true},
		{runtime.ProfileTmux, false},
	} {
		t.Run(tc.profile.String(), func(t *testing.T) {
			fake := runtime.NewFake()
			startFenced(t, fake)
			sp := runtime.NewFakeProfile(tc.profile, runtime.FullFake{Fake: fake})
			ctx := runtime.WithNudgeDeliveryKey(runtime.WithInputFence(context.Background(), receiptFence), "gc:op-1:1")
			r, err := runtime.NudgeWithReceipt(ctx, sp, "s", runtime.TextContent("hi"), false)
			if err != nil {
				t.Fatalf("NudgeWithReceipt: %v", err)
			}
			if got := !r.IsZero(); got != tc.want {
				t.Fatalf("receipt = %+v, want a receipt %v", r, tc.want)
			}
			calls := inputCalls(fake)
			if len(calls) != 1 || calls[0].Method != "Nudge" || calls[0].DeliveryKey != "gc:op-1:1" || calls[0].Fence != receiptFence {
				t.Fatalf("calls = %+v, want one fenced Nudge carrying the key", calls)
			}
		})
	}
}

// Kills an immediate path that still prefers NudgeNow behind a composite,
// dropping the key on its way to a keyed leaf (M3).
func TestNudgeWithReceiptKeepsTheKeyOnAnImmediateNudgeBehindAComposite(t *testing.T) {
	keyedLeaf, tmuxLeaf := runtime.NewFake(), runtime.NewFake()
	p := auto.New(runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: keyedLeaf}),
		runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: tmuxLeaf}))
	p.RouteACP("tmux")
	keyed := runtime.WithNudgeDeliveryKey(context.Background(), "gc:op-1:1")

	r, err := runtime.NudgeWithReceipt(keyed, p, "t3", runtime.TextContent("hi"), true)
	if err != nil || r.IsZero() {
		t.Fatalf("keyed immediate nudge to the t3 route = %+v, %v; want a receipt", r, err)
	}
	if calls := inputCalls(keyedLeaf); len(calls) != 1 || calls[0].Method != "Nudge" || calls[0].DeliveryKey != "gc:op-1:1" {
		t.Fatalf("t3 leaf calls = %+v, want one Nudge carrying the key", calls)
	}
	r, err = runtime.NudgeWithReceipt(keyed, p, "tmux", runtime.TextContent("hi"), true)
	if err != nil || !r.IsZero() {
		t.Fatalf("keyed immediate nudge to the tmux route = %+v, %v; want no receipt", r, err)
	}
	if calls := inputCalls(tmuxLeaf); len(calls) != 1 || calls[0].Method != "NudgeNow" || calls[0].DeliveryKey != "" {
		t.Fatalf("tmux leaf calls = %+v, want one NudgeNow as today", calls)
	}
	keyedLeaf.Calls = nil
	if _, err := runtime.NudgeWithReceipt(context.Background(), p, "t3", runtime.TextContent("hi"), true); err != nil {
		t.Fatal(err)
	}
	if calls := inputCalls(keyedLeaf); len(calls) != 1 || calls[0].Method != "Nudge" || calls[0].DeliveryKey != "" {
		t.Fatalf("unkeyed immediate t3 calls = %+v, want the composite's NudgeNow fallback, unkeyed", calls)
	}
}

// Kills a NudgeWithContext that diverges from NudgeWithReceipt (M4): both
// record the same calls for every fenced, immediate and key combination.
func TestNudgeWithContextIsNudgeWithReceipt(t *testing.T) {
	for _, profile := range []runtime.Profile{runtime.ProfileT3Bridge, runtime.ProfileTmux} {
		for _, fenced := range []bool{false, true} {
			for _, immediate := range []bool{false, true} {
				for _, key := range []string{"", "gc:op-1:1"} {
					t.Run(fmt.Sprintf("%s/fenced=%v/immediate=%v/key=%q", profile, fenced, immediate, key), func(t *testing.T) {
						run := func(withReceipt bool) ([]runtime.Call, error) {
							fake := runtime.NewFake()
							startFenced(t, fake)
							sp := runtime.NewFakeProfile(profile, runtime.FullFake{Fake: fake})
							ctx := context.Background()
							if fenced {
								ctx = runtime.WithInputFence(ctx, receiptFence)
							}
							if key != "" {
								ctx = runtime.WithNudgeDeliveryKey(ctx, key)
							}
							var err error
							if withReceipt {
								_, err = runtime.NudgeWithReceipt(ctx, sp, "s", runtime.TextContent("hi"), immediate)
							} else {
								err = runtime.NudgeWithContext(ctx, sp, "s", runtime.TextContent("hi"), immediate)
							}
							return fake.SnapshotCalls(), err
						}
						a, errA := run(true)
						b, errB := run(false)
						if (errA == nil) != (errB == nil) || fmt.Sprint(a) != fmt.Sprint(b) {
							t.Fatalf("NudgeWithReceipt = %+v, %v; NudgeWithContext = %+v, %v", a, errA, b, errB)
						}
					})
				}
			}
		}
	}
}

// lookupAnswer is a KeyedNudgeProvider whose LookupDelivery answers err.
type lookupAnswer struct {
	runtime.Provider
	err   error
	calls *int
}

func (l lookupAnswer) NudgeKeyed(string, []runtime.ContentBlock, string) (runtime.DeliveryReceipt, error) {
	return runtime.DeliveryReceipt{}, nil
}

func (l lookupAnswer) LookupDelivery(context.Context, string, string) (runtime.DeliveryReceipt, error) {
	*l.calls++
	if l.err != nil {
		return runtime.DeliveryReceipt{}, l.err
	}
	return runtime.DeliveryReceipt{MessageID: "m-1"}, nil
}

// Kills a funnel that reads an inability to tell as not found (M5, M6).
func TestLookupDeliveryFunnel(t *testing.T) {
	other := errors.New("boom")
	for _, tc := range []struct {
		name    string
		err     error
		plain   bool
		key     string
		found   bool
		wantErr bool
		calls   int
	}{
		{name: "found", key: "k", found: true, calls: 1},
		{name: "not found", key: "k", err: fmt.Errorf("x: %w", runtime.ErrDeliveryNotFound), calls: 1},
		{name: "unsupported", key: "k", err: runtime.ErrInteractionUnsupported, calls: 1},
		{name: "no session", key: "k", err: runtime.ErrSessionNotFound, calls: 1},
		{name: "no session and unavailable", key: "k", err: errors.Join(runtime.ErrSessionNotFound, runtime.ErrRuntimeUnavailable), wantErr: true, calls: 1},
		{name: "unavailable", key: "k", err: runtime.ErrRuntimeUnavailable, wantErr: true, calls: 1},
		{name: "other", key: "k", err: other, wantErr: true, calls: 1},
		{name: "non-keyed provider", key: "k", plain: true},
		{name: "empty key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var p runtime.Provider = lookupAnswer{Provider: runtime.NewFake(), err: tc.err, calls: &calls}
			if tc.plain {
				p = runtime.NewFake()
			}
			r, found, err := runtime.LookupDelivery(context.Background(), p, "s", tc.key)
			if found != tc.found || (err != nil) != tc.wantErr || calls != tc.calls {
				t.Fatalf("LookupDelivery = %+v, %v, %v after %d calls; want found %v, error %v, %d calls", r, found, err, calls, tc.found, tc.wantErr, tc.calls)
			}
			if tc.wantErr && tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("LookupDelivery error = %v, want it to wrap %v", err, tc.err)
			}
			if found != !r.IsZero() {
				t.Fatalf("receipt = %+v with found %v", r, found)
			}
		})
	}
}

// Kills a fake that mints a new receipt on a repeated key (M7) or holds a
// delivery whose send failed (M8).
func TestFullFakeKeyedDeliveries(t *testing.T) {
	fake := runtime.NewFake()
	ff := runtime.FullFake{Fake: fake}
	first, err := ff.NudgeKeyed("s", runtime.TextContent("hi"), "k")
	if err != nil || first.IsZero() {
		t.Fatalf("NudgeKeyed = %+v, %v", first, err)
	}
	again, err := ff.NudgeKeyed("s", runtime.TextContent("hi"), "k")
	if err != nil || again != first {
		t.Fatalf("repeated NudgeKeyed = %+v, %v; want the first receipt %+v", again, err, first)
	}
	if n := fake.CountCalls("Nudge", "s"); n != 2 {
		t.Fatalf("Nudge calls = %d, want both recorded", n)
	}
	if got, err := ff.LookupDelivery(context.Background(), "s", "k"); err != nil || got != first {
		t.Fatalf("LookupDelivery = %+v, %v; want %+v", got, err, first)
	}
	if _, err := ff.LookupDelivery(context.Background(), "s", "other"); !errors.Is(err, runtime.ErrDeliveryNotFound) {
		t.Fatalf("LookupDelivery(other) = %v, want ErrDeliveryNotFound", err)
	}

	seeded := runtime.DeliveryReceipt{MessageID: "m-seeded"}
	fake.KeyedDeliveries["t"] = map[string]runtime.DeliveryReceipt{"k": seeded}
	if got, err := ff.LookupDelivery(context.Background(), "t", "k"); err != nil || got != seeded {
		t.Fatalf("LookupDelivery(seeded) = %+v, %v", got, err)
	}

	fake.NudgeErrors = map[string]error{"u": runtime.ErrRuntimeUnavailable}
	if r, err := ff.NudgeKeyed("u", runtime.TextContent("hi"), "k"); err == nil || !r.IsZero() {
		t.Fatalf("failed NudgeKeyed = %+v, %v; want an error and no receipt", r, err)
	}
	if _, err := ff.LookupDelivery(context.Background(), "u", "k"); !errors.Is(err, runtime.ErrDeliveryNotFound) {
		t.Fatalf("LookupDelivery after a failed send = %v, want ErrDeliveryNotFound", err)
	}

	fake.LookupDeliveryErrors = map[string]error{"s": runtime.ErrRuntimeUnavailable}
	if _, err := ff.LookupDelivery(context.Background(), "s", "k"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("LookupDelivery with a scripted error = %v", err)
	}
	broken := runtime.FullFake{Fake: runtime.NewFailFake()}
	if _, err := broken.LookupDelivery(context.Background(), "s", "k"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("broken LookupDelivery = %v, want ErrRuntimeUnavailable", err)
	}
}

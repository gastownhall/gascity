package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills any drift in the digest format: a live pane's @gc_fence holds the
// digest, so a format change would strand every pane fenced by the old one.
func TestInputFenceDigestGoldenVectors(t *testing.T) {
	for _, c := range []struct {
		fence runtime.InputFence
		want  string
	}{
		{runtime.InputFence{Generation: 1, Epoch: 1, Token: "0123456789abcdef0123456789abcdef"}, "f158246661b07009"},
		{runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok1"}, "1172ccc4fbf9f6a6"},
		{runtime.InputFence{Generation: 12, Epoch: 3, Token: "tok"}, "ae4dd4e46b59f8b7"},
		{runtime.InputFence{Generation: 1, Epoch: 23, Token: "tok"}, "c3aa450c04bdf5eb"},
	} {
		if got := c.fence.Digest(); got != c.want {
			t.Errorf("Digest(gen=%d epoch=%d) = %q, want %q", c.fence.Generation, c.fence.Epoch, got, c.want)
		}
	}
}

// Kills a digest that drops the length prefix (or a field): two fences have
// equal digests exactly when they are equal, at field boundaries included.
func TestInputFenceDigestSeparatesFields(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	fences := []runtime.InputFence{
		{Generation: 12, Epoch: 3, Token: "tok"},
		{Generation: 1, Epoch: 23, Token: "tok"},
		{Generation: 1, Epoch: 2, Token: "3tok"},
		{Generation: 1, Epoch: 23, Token: "tok"},
	}
	for range 400 {
		fences = append(fences, runtime.InputFence{
			Generation: rng.Int63n(30) + 1,
			Epoch:      rng.Int63n(30) + 1,
			Token:      strconv.Itoa(rng.Intn(40)),
		})
	}
	for i, a := range fences {
		for _, b := range fences[i+1:] {
			if (a.Digest() == b.Digest()) != (a == b) {
				t.Fatalf("Digest(%v) == Digest(%v) is %v; fences equal is %v", a, b, a.Digest() == b.Digest(), a == b)
			}
		}
	}
}

// Kills an env reader that parses the incarnation differently from the
// session layer's starts: a missing, malformed or non-positive generation or
// epoch reads as 1, and the token is required and kept verbatim.
func TestInputFenceFromEnvDefaults(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want int64
	}{
		{"", 1},
		{"0", 1},
		{"-1", 1},
		{" 7", 1},
		{"x", 1},
		{"7", 7},
		{"9223372036854775807", 9223372036854775807},
	} {
		got, ok := runtime.InputFenceFromEnv(map[string]string{
			"GC_RUNTIME_EPOCH":      c.raw,
			"GC_CONTINUATION_EPOCH": c.raw,
			"GC_INSTANCE_TOKEN":     " tok ",
		})
		if !ok || got.Generation != c.want || got.Epoch != c.want || got.Token != " tok " {
			t.Errorf("raw %q: %v, %v; want gen=epoch=%d and the token verbatim", c.raw, got, ok, c.want)
		}
	}
	got, ok := runtime.InputFenceFromEnv(map[string]string{"GC_INSTANCE_TOKEN": "t"})
	if !ok || got != (runtime.InputFence{Generation: 1, Epoch: 1, Token: "t"}) {
		t.Errorf("no counters: %v, %v; want gen=epoch=1", got, ok)
	}
	if _, ok := runtime.InputFenceFromEnv(map[string]string{"GC_RUNTIME_EPOCH": "2"}); ok {
		t.Error("an env with no token named an incarnation")
	}
	if _, ok := runtime.InputFenceFromEnv(nil); ok {
		t.Error("a nil env named an incarnation")
	}
}

func TestInputFenceValidate(t *testing.T) {
	for _, f := range []runtime.InputFence{
		{},
		{Generation: 1, Epoch: 1},
		{Generation: 0, Epoch: 1, Token: "t"},
		{Generation: 1, Epoch: 0, Token: "t"},
		{Generation: -1, Epoch: 1, Token: "t"},
	} {
		if err := f.Validate(); !errors.Is(err, runtime.ErrInvalidInputFence) {
			t.Errorf("Validate(%v) = %v, want ErrInvalidInputFence", f, err)
		}
	}
	if err := (runtime.InputFence{Generation: 1, Epoch: 1, Token: "t"}).Validate(); err != nil {
		t.Errorf("Validate(valid) = %v", err)
	}
	if !(runtime.InputFence{}).IsZero() || (runtime.InputFence{Token: "t"}).IsZero() {
		t.Error("IsZero is wrong")
	}
}

// Kills any formatting, JSON or error path that shows the token, a
// capability that must never reach logs, argv or the SSE wire.
func TestInputFenceNeverShowsToken(t *testing.T) {
	const token = "s3cr3t-token-value"
	f := runtime.InputFence{Generation: 2, Epoch: 3, Token: token}
	type holder struct {
		Fence runtime.InputFence
		Ptr   *runtime.InputFence
		Name  string
	}
	h := holder{Fence: f, Ptr: &f, Name: "n"}
	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%T"} {
		outs = append(outs, fmt.Sprintf(verb, f), fmt.Sprintf(verb, h), fmt.Sprintf(verb, &f))
	}
	for _, v := range []any{f, h, &f, []runtime.InputFence{f}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, string(b))
	}
	outs = append(outs, f.String(), f.GoString(),
		(&runtime.FenceMismatchError{Session: "s", Found: f.Digest()}).Error(),
		(&runtime.FenceMismatchError{Session: "s", Found: runtime.FenceFoundDead, Partial: true}).Error(),
		(runtime.InputFence{Generation: 1, Epoch: 0, Token: token}).Validate().Error())
	for _, out := range outs {
		if strings.Contains(out, token) || strings.Contains(out, fmt.Sprintf("%x", token)) {
			t.Errorf("output shows the token: %s", out)
		}
	}
	if got := (runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok1"}).String(); got != "fence(gen=2 epoch=3 digest=1172ccc4fbf9f6a6)" {
		t.Errorf("String() = %q", got)
	}
	b, _ := json.Marshal(runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok1"})
	if string(b) != `{"generation":2,"epoch":3,"digest":"1172ccc4fbf9f6a6"}` {
		t.Errorf("JSON = %s", b)
	}
}

// Kills a fence mismatch that callers would read as a gone session or an
// unavailable runtime, and so retry, heal or report as absent.
func TestFenceMismatchIsNotSessionGone(t *testing.T) {
	for _, found := range []string{runtime.FenceFoundAbsent, runtime.FenceFoundDead, runtime.FenceFoundUnproven, "1172ccc4fbf9f6a6"} {
		for _, partial := range []bool{false, true} {
			// A session name a runtime allows may hold IsSessionGone's phrases.
			base := &runtime.FenceMismatchError{Session: "agent not found not running", Found: found, Partial: partial}
			for _, err := range []error{base, fmt.Errorf("delivering: %w", base), errors.Join(base)} {
				if !errors.Is(err, runtime.ErrFenceMismatch) {
					t.Errorf("%v: not ErrFenceMismatch", err)
				}
				if runtime.IsSessionGone(err) || errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrSessionNotFound) {
					t.Errorf("%v reads as gone or unavailable", err)
				}
				var fm *runtime.FenceMismatchError
				if !errors.As(err, &fm) || fm.Found != found || fm.Partial != partial {
					t.Errorf("%v: errors.As lost Found/Partial", err)
				}
			}
		}
	}
	if errors.Is(&runtime.FenceMismatchError{}, runtime.ErrInputFenceUnsupported) {
		t.Error("a mismatch matches ErrInputFenceUnsupported")
	}
	if runtime.IsSessionGone(runtime.ErrInputFenceUnsupported) || runtime.IsSessionGone(runtime.ErrInvalidInputFence) {
		t.Error("a fence sentinel reads as gone")
	}
}

func TestInputFenceContextLatestWins(t *testing.T) {
	if _, ok := runtime.InputFenceFrom(context.Background()); ok {
		t.Fatal("a bare context carries a fence")
	}
	if _, ok := runtime.InputFenceFrom(nil); ok { //nolint:staticcheck // a nil context is tolerated
		t.Fatal("a nil context carries a fence")
	}
	a := runtime.InputFence{Generation: 1, Epoch: 1, Token: "a"}
	b := runtime.InputFence{Generation: 2, Epoch: 1, Token: "b"}
	ctx := runtime.WithInputFence(runtime.WithInputFence(context.Background(), a), b)
	if got, ok := runtime.InputFenceFrom(ctx); !ok || got != b {
		t.Fatalf("InputFenceFrom = %v, %v; want the latest", got, ok)
	}
	if _, ok := runtime.InputFenceFrom(runtime.WithInputFence(context.Background(), runtime.InputFence{})); ok {
		t.Fatal("a zero fence reads as a fence")
	}
}

// plainProvider is a Provider with none of the optional interfaces.
type plainProvider struct{ runtime.Provider }

// Kills a change to the legacy dispatch NudgeWithContext replaces: NudgeNow
// when immediate on an ImmediateNudgeProvider (unless the context's key goes
// to a deduplicating leaf), else the keyed nudge when the context carries a
// key and the provider deduplicates, else Nudge.
func TestNudgeWithContextUnfencedDispatchUnchanged(t *testing.T) {
	content := runtime.TextContent("hi")
	for _, c := range []struct {
		name      string
		provider  func(*runtime.Fake) runtime.Provider
		immediate bool
		key       string
		want      string
	}{
		{"immediate", func(f *runtime.Fake) runtime.Provider { return f }, true, "", "NudgeNow"},
		// A key outranks immediacy for a leaf that deduplicates keyed
		// deliveries: NudgeNow would drop the key (OJ-7 D7).
		{"immediate keyed to a keyed leaf", func(f *runtime.Fake) runtime.Provider { return runtime.FullFake{Fake: f} }, true, "k", "Nudge/k"},
		{"immediate keyed to a plain leaf", func(f *runtime.Fake) runtime.Provider { return f }, true, "k", "NudgeNow"},
		{"default", func(f *runtime.Fake) runtime.Provider { return f }, false, "", "Nudge"},
		{"default keyed on a plain fake", func(f *runtime.Fake) runtime.Provider { return f }, false, "k", "Nudge"},
		{"default keyed", func(f *runtime.Fake) runtime.Provider { return runtime.FullFake{Fake: f} }, false, "k", "Nudge/k"},
		{"immediate without NudgeNow", func(f *runtime.Fake) runtime.Provider { return plainProvider{f} }, true, "", "Nudge"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := runtime.NewFake()
			ctx := context.Background()
			if c.key != "" {
				ctx = runtime.WithNudgeDeliveryKey(ctx, c.key)
			}
			if err := runtime.NudgeWithContext(ctx, c.provider(f), "s", content, c.immediate); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, call := range f.SnapshotCalls() {
				s := call.Method
				if call.DeliveryKey != "" {
					s += "/" + call.DeliveryKey
				}
				if !call.Fence.IsZero() {
					s += "/fenced"
				}
				got = append(got, s)
			}
			if !slices.Equal(got, []string{c.want}) {
				t.Fatalf("calls = %v, want [%s]", got, c.want)
			}
		})
	}
}

// A ctx-fenced input method routed to a runtime that cannot fence is refused
// before any input; an unfenced ctx, or a fencing runtime, passes.
func TestRefuseUnfenceableCtx(t *testing.T) {
	fence := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	fenced := runtime.WithInputFence(context.Background(), fence)
	none := runtime.NewFake()
	none.InputFencingValue = runtime.InputFencingNone
	if err := runtime.RefuseUnfenceableCtx(fenced, none, "s"); !errors.Is(err, runtime.ErrInputFenceUnsupported) {
		t.Fatalf("fenced ctx to a none runtime = %v, want ErrInputFenceUnsupported", err)
	}
	if err := runtime.RefuseUnfenceableCtx(context.Background(), none, "s"); err != nil {
		t.Fatalf("unfenced ctx = %v", err)
	}
	if err := runtime.RefuseUnfenceableCtx(fenced, runtime.NewFake(), "s"); err != nil {
		t.Fatalf("fenced ctx to a fencing runtime = %v", err)
	}
}

// A Fake that cannot fence refuses a ctx-fenced ClearInput,
// DismissKnownDialogs or ResetInterruptedTurn instead of running it
// unfenced, and records no call.
func TestFakeNoneRefusesCtxFencedInput(t *testing.T) {
	f := runtime.NewFake()
	f.InputFencingValue = runtime.InputFencingNone
	if err := f.Start(context.Background(), "s", runtime.Config{Env: map[string]string{"GC_INSTANCE_TOKEN": "tok"}}); err != nil {
		t.Fatal(err)
	}
	f.Calls = nil
	ctx := runtime.WithInputFence(context.Background(), runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"})
	for name, err := range map[string]error{
		"ClearInput":           f.ClearInput(ctx, "s", 0),
		"DismissKnownDialogs":  f.DismissKnownDialogs(ctx, "s", 0),
		"ResetInterruptedTurn": f.ResetInterruptedTurn(ctx, "s"),
	} {
		if !errors.Is(err, runtime.ErrInputFenceUnsupported) {
			t.Errorf("%s under a fence on a none fake = %v, want ErrInputFenceUnsupported", name, err)
		}
	}
	if calls := f.SnapshotCalls(); len(calls) != 0 {
		t.Fatalf("refused calls recorded: %+v", calls)
	}
}

// Kills a fenced input that reports success, or nothing sent, into an
// incarnation its fresh liveness read has ruled out: under a fence no session
// is a miss finding it absent, an exited agent or a corpse one finding it
// dead, and a read that cannot answer is ErrRuntimeUnavailable, not a miss
// (never proof the incarnation is gone). Only an alive read passes; without
// a fence every read passes, for the input's own handling.
func TestLivenessFenceMiss(t *testing.T) {
	fenced := runtime.WithInputFence(context.Background(), runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"})
	alive := runtime.Liveness{Running: true, Alive: true}
	for _, tc := range []struct {
		name        string
		live        runtime.Liveness
		err         error
		found       string // the miss's Found; "" for none
		unavailable bool
	}{
		{name: "alive", live: alive},
		{name: "absent", found: runtime.FenceFoundAbsent},
		{name: "agent exited", live: runtime.Liveness{Running: true}, found: runtime.FenceFoundDead},
		{name: "corpse", live: runtime.Liveness{Corpse: true}, found: runtime.FenceFoundDead},
		{name: "unreadable", err: errors.New("tmux state refresh failed"), unavailable: true},
		{name: "unavailable beside an alive view", live: alive, err: fmt.Errorf("probe: %w", runtime.ErrRuntimeUnavailable), unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runtime.LivenessFenceMiss(context.Background(), "s", tc.live, tc.err); err != nil {
				t.Errorf("unfenced = %v, want nil", err)
			}
			err := runtime.LivenessFenceMiss(fenced, "s", tc.live, tc.err)
			var miss *runtime.FenceMismatchError
			switch {
			case tc.unavailable:
				if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, tc.err) || errors.Is(err, runtime.ErrFenceMismatch) {
					t.Fatalf("fenced = %v, want ErrRuntimeUnavailable wrapping the read's error, not a miss", err)
				}
			case tc.found == "":
				if err != nil {
					t.Fatalf("fenced = %v, want nil", err)
				}
			case !errors.As(err, &miss) || miss.Session != "s" || miss.Found != tc.found || miss.Partial:
				t.Fatalf("fenced = %v, want a non-partial miss of %q finding it %s", err, "s", tc.found)
			}
		})
	}
}

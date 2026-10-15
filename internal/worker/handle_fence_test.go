package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// fencedSessionHandle is a started claude session handle and its fence.
func fencedSessionHandle(t *testing.T) (*SessionHandle, *runtime.Fake, string, runtime.InputFence) {
	t.Helper()
	handle, _, sp, manager := newTestSessionHandle(t, SessionSpec{Template: "probe", Title: "Probe", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := manager.Get(handle.currentSessionID())
	if err != nil {
		t.Fatal(err)
	}
	f, ok := sessionpkg.InputFenceFor(info)
	if !ok {
		t.Fatal("started session names no incarnation")
	}
	sp.Calls = nil
	return handle, sp, info.SessionName, f
}

func fencedRuntimeHandle(t *testing.T) (*RuntimeHandle, *runtime.Fake, runtime.InputFence) {
	t.Helper()
	sp := runtime.NewFake()
	f := runtime.InputFence{Generation: 4, Epoch: 2, Token: "rt-token"}
	env := map[string]string{"GC_RUNTIME_EPOCH": "4", "GC_CONTINUATION_EPOCH": "2", "GC_INSTANCE_TOKEN": "rt-token"}
	if err := sp.Start(context.Background(), "rt", runtime.Config{Env: env}); err != nil {
		t.Fatal(err)
	}
	handle, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "rt", ProviderName: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	sp.Calls = nil
	return handle, sp, f
}

func typedCalls(sp *runtime.Fake, name string) []runtime.Call {
	var out []runtime.Call
	for _, c := range sp.SnapshotCalls() {
		switch c.Method {
		case "Nudge", "NudgeNow", "SendKeys", "Interrupt", "ClearInput", "Start":
			if c.Name == name {
				out = append(out, c)
			}
		}
	}
	return out
}

type nudger interface {
	Nudge(context.Context, sessionpkg.Actor, NudgeRequest) (NudgeResult, error)
}

// Both handles: a fenced nudge must be live-only and not wait-idle, a fence
// that names nothing is refused, a match delivers carrying the fence, the
// respawn between Take and delivery is a mismatch that types nothing, and a
// runtime that cannot fence refuses or, with AllowUnfenced, delivers
// reporting Unfenced.
func TestHandlesFenceNudges(t *testing.T) {
	type built struct {
		h     nudger
		actor sessionpkg.Actor
		sp    *runtime.Fake
		name  string
		f     runtime.InputFence
	}
	builders := map[string]func(t *testing.T) built{
		"session": func(t *testing.T) built {
			h, sp, name, f := fencedSessionHandle(t)
			return built{h, handleActor(t, h, sessionpkg.ActorAgent), sp, name, f}
		},
		"runtime": func(t *testing.T) built {
			h, sp, f := fencedRuntimeHandle(t)
			return built{h, handleActor(t, h, sessionpkg.ActorAgent), sp, "rt", f}
		},
	}
	ctx := context.Background()
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			for _, req := range []NudgeRequest{
				{Wake: NudgeWakeIfNeeded},
				{Wake: NudgeWakeLiveOnly, Delivery: NudgeDeliveryWaitIdle},
			} {
				b := build(t)
				req.Text, req.Fence = "hi", b.f
				if _, err := b.h.Nudge(ctx, b.actor, req); !errors.Is(err, ErrFencedNudgeShape) {
					t.Errorf("shape %+v: %v, want ErrFencedNudgeShape", req, err)
				}
				if calls := typedCalls(b.sp, b.name); len(calls) != 0 {
					t.Errorf("shape refusal sent %+v", calls)
				}
			}

			b := build(t)
			if _, err := b.h.Nudge(ctx, b.actor, NudgeRequest{Text: "hi", Wake: NudgeWakeLiveOnly, Fence: runtime.InputFence{Generation: 1, Epoch: 1}}); !errors.Is(err, runtime.ErrInvalidInputFence) {
				t.Errorf("tokenless fence: %v", err)
			}

			for _, delivery := range []NudgeDelivery{NudgeDeliveryDefault, NudgeDeliveryImmediate} {
				b := build(t)
				res, err := b.h.Nudge(ctx, b.actor, NudgeRequest{Text: "hi", Wake: NudgeWakeLiveOnly, Delivery: delivery, Fence: b.f, DeliveryKey: "k"})
				if err != nil || !res.Delivered || res.Unfenced {
					t.Fatalf("%s matching: %+v, %v", delivery, res, err)
				}
				calls := typedCalls(b.sp, b.name)
				if len(calls) != 1 || calls[0].Fence != b.f {
					t.Fatalf("%s matching calls %+v, want one carrying the fence", delivery, calls)
				}
				if delivery == NudgeDeliveryDefault && calls[0].DeliveryKey != "k" {
					t.Errorf("default delivery lost its key: %+v", calls[0])
				}
			}

			// The incarnation the caller took is replaced before delivery.
			b = build(t)
			stale := b.f
			stale.Token = "taken-before-respawn"
			_, err := b.h.Nudge(ctx, b.actor, NudgeRequest{Text: "hi", Wake: NudgeWakeLiveOnly, Delivery: NudgeDeliveryImmediate, Fence: stale})
			var miss *runtime.FenceMismatchError
			if !errors.As(err, &miss) || miss.Partial {
				t.Fatalf("respawn between Take and delivery: %v, want a non-partial mismatch", err)
			}
			if calls := typedCalls(b.sp, b.name); len(calls) != 0 {
				t.Fatalf("a mismatched nudge typed %+v", calls)
			}

			b = build(t)
			b.sp.InputFencingValue = runtime.InputFencingNone
			if _, err := b.h.Nudge(ctx, b.actor, NudgeRequest{Text: "hi", Wake: NudgeWakeLiveOnly, Fence: b.f}); !errors.Is(err, runtime.ErrInputFenceUnsupported) {
				t.Fatalf("unfenceable runtime: %v, want ErrInputFenceUnsupported", err)
			}
			if calls := typedCalls(b.sp, b.name); len(calls) != 0 {
				t.Fatalf("a refused nudge typed %+v", calls)
			}
			res, err := b.h.Nudge(ctx, b.actor, NudgeRequest{Text: "hi", Wake: NudgeWakeLiveOnly, Fence: b.f, AllowUnfenced: true})
			if err != nil || !res.Delivered || !res.Unfenced {
				t.Fatalf("AllowUnfenced: %+v, %v; want delivered, Unfenced", res, err)
			}
			if calls := typedCalls(b.sp, b.name); len(calls) != 1 || !calls[0].Fence.IsZero() {
				t.Fatalf("AllowUnfenced calls %+v, want one unfenced", calls)
			}

			b = build(t)
			res, err = b.h.Nudge(ctx, b.actor, NudgeRequest{Text: "hi", Wake: NudgeWakeLiveOnly})
			if err != nil || !res.Delivered || res.Unfenced {
				t.Fatalf("unfenced request: %+v, %v", res, err)
			}
		})
	}
}

type interrupter interface {
	Interrupt(context.Context, InterruptRequest) error
}

// Both handles pass an interrupt's fence to every input they send, refuse a
// runtime that cannot fence, and send nothing on a mismatch.
func TestHandlesFenceInterrupts(t *testing.T) {
	ctx := context.Background()
	builders := map[string]func(t *testing.T) (interrupter, *runtime.Fake, string, runtime.InputFence){
		"session": func(t *testing.T) (interrupter, *runtime.Fake, string, runtime.InputFence) {
			h, sp, name, f := fencedSessionHandle(t)
			return h, sp, name, f
		},
		"runtime": func(t *testing.T) (interrupter, *runtime.Fake, string, runtime.InputFence) {
			h, sp, f := fencedRuntimeHandle(t)
			return h, sp, "rt", f
		},
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			h, sp, sess, f := build(t)
			if err := h.Interrupt(ctx, InterruptRequest{Fence: f}); err != nil {
				t.Fatalf("Interrupt(fence) = %v", err)
			}
			calls := typedCalls(sp, sess)
			if len(calls) == 0 {
				t.Fatal("no input sent")
			}
			for _, c := range calls {
				if c.Fence != f {
					t.Errorf("%s carries %v", c.Method, c.Fence)
				}
			}

			h, sp, sess, f = build(t)
			f.Epoch++
			if err := h.Interrupt(ctx, InterruptRequest{Fence: f}); !errors.Is(err, runtime.ErrFenceMismatch) {
				t.Fatalf("Interrupt(other) = %v", err)
			}
			if calls := typedCalls(sp, sess); len(calls) != 0 {
				t.Fatalf("a mismatched interrupt sent %+v", calls)
			}

			h, sp, sess, f = build(t)
			sp.InputFencingValue = runtime.InputFencingNone
			if err := h.Interrupt(ctx, InterruptRequest{Fence: f}); !errors.Is(err, runtime.ErrInputFenceUnsupported) {
				t.Fatalf("Interrupt on an unfenceable runtime = %v", err)
			}
			if calls := typedCalls(sp, sess); len(calls) != 0 {
				t.Fatalf("a refused interrupt sent %+v", calls)
			}
		})
	}
}

// The token is a capability: requests never serialize it.
func TestFencedRequestsNeverSerializeToken(t *testing.T) {
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "secret-capability"}
	for _, v := range []any{
		NudgeRequest{Text: "hi", Fence: f, AllowUnfenced: true},
		InterruptRequest{Fence: f},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), f.Token) || strings.Contains(string(b), f.Digest()) {
			t.Fatalf("%T serialized the fence: %s", v, b)
		}
	}
}

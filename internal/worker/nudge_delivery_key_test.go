package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func nudgeDeliveryKeys(calls []runtime.Call) []string {
	var keys []string
	for _, call := range calls {
		if call.Method == "Nudge" {
			keys = append(keys, call.DeliveryKey)
		}
	}
	return keys
}

// A runtime-only handle carries the request's delivery key to a provider
// that deduplicates keyed deliveries, and only for that request.
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
	actor := handleActor(t, handle, sessionpkg.ActorAgent)
	for _, req := range []NudgeRequest{
		{Text: "hello", Wake: NudgeWakeLiveOnly, DeliveryKey: "batch-1"},
		{Text: "hello again", Wake: NudgeWakeLiveOnly},
	} {
		result, err := handle.Nudge(context.Background(), actor, req)
		if err != nil || !result.Delivered {
			t.Fatalf("Nudge(%+v) = %+v, %v; want delivered", req, result, err)
		}
	}
	keys := nudgeDeliveryKeys(fake.Calls)
	if len(keys) != 2 || keys[0] != "batch-1" || keys[1] != "" {
		t.Fatalf("nudge delivery keys = %q, want [batch-1, \"\"]", keys)
	}
}

// A session handle carries the request's delivery key through the session
// manager to the provider.
func TestSessionHandleNudgePassesTheDeliveryKey(t *testing.T) {
	fake := runtime.NewFake()
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})
	manager := sessionpkg.NewManagerWithOptions(beads.NewMemStore(), sp, sessionpkg.WithCityPath(t.TempDir()))
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager: manager,
		Session: SessionSpec{
			Profile:  ProfileClaudeTmuxCLI,
			Template: "probe",
			Title:    "Probe",
			Command:  "claude",
			WorkDir:  t.TempDir(),
			Provider: "claude",
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := handle.Nudge(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent), NudgeRequest{
		Text:        "check deploy status",
		Wake:        NudgeWakeLiveOnly,
		DeliveryKey: "batch-1",
	})
	if err != nil || !result.Delivered {
		t.Fatalf("Nudge = %+v, %v; want delivered", result, err)
	}
	if keys := nudgeDeliveryKeys(fake.Calls); len(keys) != 1 || keys[0] != "batch-1" {
		t.Fatalf("nudge delivery keys = %q, want [batch-1]", keys)
	}
}

// keyedFence is the incarnation keyedRuntimeHandle starts its session as.
var keyedFence = runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok"}

// keyedRuntimeHandle is a runtime-only handle on a t3bridge-profiled fake
// whose session runs as keyedFence's incarnation.
func keyedRuntimeHandle(t *testing.T) (*RuntimeHandle, *runtime.Fake) {
	t.Helper()
	fake := runtime.NewFake()
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})
	env := map[string]string{runtime.RuntimeEpochEnv: "2", runtime.ContinuationEpochEnv: "3", runtime.InstanceTokenEnv: "tok"}
	if err := sp.Start(context.Background(), "keyed-worker", runtime.Config{Env: env}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	handle, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "keyed-worker", ProviderName: "stub"})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	fake.Calls = nil
	return handle, fake
}

// keyedSessionHandle is a session handle on a t3bridge-profiled fake (behind
// the auto composite when composite), with its session started.
func keyedSessionHandle(t *testing.T, composite bool) (turnSession, *runtime.Fake) {
	t.Helper()
	fake := runtime.NewFake()
	sp := runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})
	if composite {
		sp = auto.New(sp, runtime.NewFake())
	}
	s := newTurnSession(t, sp, "codex", "")
	fake.Calls = nil
	return s, fake
}

// sentCalls are the input calls fake recorded.
func sentCalls(fake *runtime.Fake) []runtime.Call {
	var out []runtime.Call
	for _, c := range fake.SnapshotCalls() {
		switch c.Method {
		case "Nudge", "NudgeNow", "Interrupt", "SendKeys", "Start", "InterruptTurn", "ObserveTurn", "WaitTurnEnded":
			out = append(out, c)
		}
	}
	return out
}

// seedHeld holds a delivery to name under key on fake.
func seedHeld(fake *runtime.Fake, name, key string) runtime.DeliveryReceipt {
	r := runtime.DeliveryReceipt{MessageID: "m-held"}
	fake.KeyedDeliveries = map[string]map[string]runtime.DeliveryReceipt{name: {key: r}}
	return r
}

// Kills a receipt left off NudgeResult (M21): default and immediate, fenced
// and unfenced, a runtime handle reports the runtime's receipt.
func TestRuntimeHandleNudgeReportsTheReceipt(t *testing.T) {
	for _, delivery := range []NudgeDelivery{NudgeDeliveryDefault, NudgeDeliveryImmediate} {
		for _, fenced := range []bool{false, true} {
			h, fake := keyedRuntimeHandle(t)
			req := NudgeRequest{Text: "hello", Delivery: delivery, Wake: NudgeWakeLiveOnly, DeliveryKey: "gc:op:1"}
			if fenced {
				req.Fence = keyedFence
			}
			res, err := h.Nudge(context.Background(), handleActor(t, h, sessionpkg.ActorAgent), req)
			if err != nil || !res.Delivered || res.Replayed {
				t.Fatalf("%s fenced=%v: Nudge = %+v, %v", delivery, fenced, res, err)
			}
			if held := fake.KeyedDeliveries["keyed-worker"]["gc:op:1"]; res.Receipt.IsZero() || res.Receipt != held {
				t.Fatalf("%s fenced=%v: receipt = %+v, want %+v", delivery, fenced, res.Receipt, held)
			}
			// A fenced fake records an immediate nudge as NudgeNow; t3bridge
			// ignores immediacy, and both carry the key.
			if got := sentCalls(fake); len(got) != 1 || got[0].DeliveryKey != "gc:op:1" {
				t.Fatalf("%s fenced=%v: sent %+v, want one nudge carrying the request's key", delivery, fenced, got)
			}
		}
	}
}

// unfencedKeyed is a keyed provider that fences no input: it forwards only
// Provider and KeyedNudgeProvider, so InputFencingFor reads it as none.
type unfencedKeyed struct {
	runtime.Provider
	keyed runtime.KeyedNudgeProvider
}

func (u unfencedKeyed) NudgeKeyed(name string, content []runtime.ContentBlock, key string) (runtime.DeliveryReceipt, error) {
	return u.keyed.NudgeKeyed(name, content, key)
}

func (u unfencedKeyed) LookupDelivery(ctx context.Context, name, key string) (runtime.DeliveryReceipt, error) {
	return u.keyed.LookupDelivery(ctx, name, key)
}

// keyedHandles are a runtime handle and a session handle on a
// t3bridge-profiled fake, fencing as the profile does or (unfenced) not at
// all, each with the fence a request to its session names.
func keyedHandles(unfenced bool) map[string]func(*testing.T) (Handle, runtime.InputFence, string, *runtime.Fake) {
	wrap := func(sp runtime.Provider) runtime.Provider {
		if !unfenced {
			return sp
		}
		return unfencedKeyed{Provider: sp, keyed: sp.(runtime.KeyedNudgeProvider)}
	}
	return map[string]func(*testing.T) (Handle, runtime.InputFence, string, *runtime.Fake){
		"runtime handle": func(t *testing.T) (Handle, runtime.InputFence, string, *runtime.Fake) {
			h, fake := keyedRuntimeHandle(t)
			h.provider = wrap(h.provider)
			return h, keyedFence, "keyed-worker", fake
		},
		"session handle": func(t *testing.T) (Handle, runtime.InputFence, string, *runtime.Fake) {
			fake := runtime.NewFake()
			s := newTurnSession(t, wrap(runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake})), "codex", "")
			fake.Calls = nil
			fence, _ := sessionpkg.InputFenceFor(s.info)
			return s.handle, fence, s.info.SessionName, fake
		},
	}
}

// Kills a redelivery that is not looked up (M22) or reports Unfenced on a
// replay (M23): a held delivery is replayed with its receipt and nothing is
// sent, on either handle. Under a runtime that fences no input, a fenced
// request that allows unfenced delivery is Unfenced when it is sent; a
// replay sent nothing, so it is not.
func TestNudgeRedeliveryHeldSendsNothing(t *testing.T) {
	for _, unfenced := range []bool{false, true} {
		for label, newHandle := range keyedHandles(unfenced) {
			t.Run(fmt.Sprintf("unfenced=%v/%s", unfenced, label), func(t *testing.T) {
				h, fence, name, fake := newHandle(t)
				held := seedHeld(fake, name, "gc:op:1")
				res, err := h.Nudge(context.Background(), handleActor(t, h, sessionpkg.ActorAgent), NudgeRequest{
					Text: "hello", Wake: NudgeWakeLiveOnly, DeliveryKey: "gc:op:1", Redelivery: true, Fence: fence, AllowUnfenced: true,
				})
				if err != nil || !res.Delivered || !res.Replayed || res.Unfenced || res.Receipt != held {
					t.Fatalf("Nudge = %+v, %v; want a replay of %+v", res, err, held)
				}
				if got := sentCalls(fake); len(got) != 0 {
					t.Fatalf("sent %+v, want nothing", got)
				}
			})
		}
	}
}

// The unfenced arm of TestNudgeRedeliveryHeldSendsNothing is live: the same
// request, not held, goes out and is reported Unfenced.
func TestNudgeRedeliveryNotHeldUnfencedIsUnfenced(t *testing.T) {
	for label, newHandle := range keyedHandles(true) {
		t.Run(label, func(t *testing.T) {
			h, fence, _, _ := newHandle(t)
			res, err := h.Nudge(context.Background(), handleActor(t, h, sessionpkg.ActorAgent), NudgeRequest{
				Text: "hello", Wake: NudgeWakeLiveOnly, DeliveryKey: "gc:op:1", Redelivery: true, Fence: fence, AllowUnfenced: true,
			})
			if err != nil || !res.Delivered || res.Replayed || !res.Unfenced {
				t.Fatalf("Nudge = %+v, %v; want delivered now, unfenced", res, err)
			}
		})
	}
}

// Kills a runtime-handle message that checks the session is running before
// it looks a redelivery up: a delivery the runtime already holds is replayed
// even when the session has since stopped, instead of reported inactive.
func TestRuntimeHandleMessageLooksUpBeforeTheRunningCheck(t *testing.T) {
	h, fake := keyedRuntimeHandle(t)
	held := seedHeld(fake, "keyed-worker", "gc:op:7")
	if err := h.provider.Stop("keyed-worker"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if h.provider.IsRunning("keyed-worker") {
		t.Fatal("session still running after Stop")
	}
	fake.Calls = nil
	res, err := h.Message(context.Background(), handleActor(t, h, sessionpkg.ActorOperator), MessageRequest{Text: "hello", DeliveryKey: "gc:op:7", Redelivery: true})
	if err != nil || !res.Replayed {
		t.Fatalf("Message to a stopped session holding %+v = %+v, %v; want a replay", held, res, err)
	}
	if got := sentCalls(fake); len(got) != 0 {
		t.Fatalf("replay sent %+v", got)
	}
}

// A redelivery the runtime does not hold goes out once, under the same key.
func TestNudgeRedeliveryNotHeldSendsUnderTheSameKey(t *testing.T) {
	h, fake := keyedRuntimeHandle(t)
	res, err := h.Nudge(context.Background(), handleActor(t, h, sessionpkg.ActorAgent), NudgeRequest{
		Text: "hello", Wake: NudgeWakeLiveOnly, DeliveryKey: "gc:op:1", Redelivery: true,
	})
	if err != nil || !res.Delivered || res.Replayed || res.Receipt.IsZero() {
		t.Fatalf("Nudge = %+v, %v; want delivered now with a receipt", res, err)
	}
	if n := fake.CountCalls("LookupDelivery", "keyed-worker"); n != 1 {
		t.Fatalf("lookups = %d, want 1", n)
	}
	if keys := nudgeDeliveryKeys(fake.Calls); len(keys) != 1 || keys[0] != "gc:op:1" {
		t.Fatalf("delivery keys = %q, want one under gc:op:1", keys)
	}
}

// Kills a lookup on every delivery (M24): without Redelivery, or without a
// key, nothing is looked up.
func TestNudgeWithoutRedeliveryLooksNothingUp(t *testing.T) {
	for _, req := range []NudgeRequest{
		{Text: "hello", Wake: NudgeWakeLiveOnly, DeliveryKey: "gc:op:1"},
		{Text: "hello", Wake: NudgeWakeLiveOnly, Redelivery: true},
	} {
		h, fake := keyedRuntimeHandle(t)
		if _, err := h.Nudge(context.Background(), handleActor(t, h, sessionpkg.ActorAgent), req); err != nil {
			t.Fatal(err)
		}
		if n := fake.CountCalls("LookupDelivery", "keyed-worker"); n != 0 {
			t.Fatalf("%+v: lookups = %d, want none", req, n)
		}
	}
}

// Kills a send after a failed lookup (M25) and an error that does not say
// nothing was sent (M26), on either handle.
func TestNudgeRedeliveryLookupFailureSendsNothing(t *testing.T) {
	check := func(t *testing.T, res NudgeResult, err error, fake *runtime.Fake) {
		t.Helper()
		if !errors.Is(err, runtime.ErrNudgeNotSent) || !errors.Is(err, runtime.ErrRuntimeUnavailable) || res.Delivered {
			t.Fatalf("Nudge = %+v, %v; want not sent and unavailable", res, err)
		}
		if got := sentCalls(fake); len(got) != 0 {
			t.Fatalf("sent %+v after a failed lookup", got)
		}
	}
	t.Run("runtime handle", func(t *testing.T) {
		h, fake := keyedRuntimeHandle(t)
		fake.LookupDeliveryErrors = map[string]error{"keyed-worker": runtime.ErrRuntimeUnavailable}
		res, err := h.Nudge(context.Background(), handleActor(t, h, sessionpkg.ActorAgent), NudgeRequest{Text: "hello", Wake: NudgeWakeLiveOnly, DeliveryKey: "gc:op:1", Redelivery: true})
		check(t, res, err, fake)
	})
	t.Run("session handle", func(t *testing.T) {
		s, fake := keyedSessionHandle(t, false)
		fake.LookupDeliveryErrors = map[string]error{s.info.SessionName: runtime.ErrRuntimeUnavailable}
		res, err := s.handle.Nudge(context.Background(), handleActor(t, s.handle, sessionpkg.ActorAgent), NudgeRequest{Text: "hello", Wake: NudgeWakeLiveOnly, DeliveryKey: "gc:op:1", Redelivery: true})
		check(t, res, err, fake)
	})
}

// A message carries its delivery key to a deduplicating runtime: a default
// intent's delivery, and an interrupt_now behind the auto composite, which
// sends immediately and must keep the key (OJ-7 D7).
func TestSessionHandleMessageCarriesTheDeliveryKey(t *testing.T) {
	for _, tc := range []struct {
		name      string
		composite bool
		intent    DeliveryIntent
	}{
		{name: "default", intent: DeliveryIntentDefault},
		{name: "interrupt_now behind auto", composite: true, intent: DeliveryIntentInterruptNow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake := keyedSessionHandle(t, tc.composite)
			res, err := s.handle.Message(context.Background(), handleActor(t, s.handle, sessionpkg.ActorOperator), MessageRequest{
				Text: "hello", Delivery: tc.intent, DeliveryKey: "gc:op:1",
			})
			if err != nil || res.Queued || res.Replayed {
				t.Fatalf("Message = %+v, %v; want delivered now", res, err)
			}
			if keys := nudgeDeliveryKeys(fake.Calls); len(keys) != 1 || keys[0] != "gc:op:1" {
				t.Fatalf("delivery keys = %q, want [gc:op:1]; calls %+v", keys, fake.SnapshotCalls())
			}
			if n := fake.CountCalls("LookupDelivery", s.info.SessionName); n != 0 {
				t.Fatalf("lookups = %d, want none on a first delivery", n)
			}
		})
	}
}

// Kills a lookup placed after the turn sighting or after Submit (M27): an
// interrupt_now redelivery the runtime holds, while the first attempt's
// turn still runs, does nothing at all.
func TestSessionHandleMessageRedeliveryHeldDoesNothing(t *testing.T) {
	s, fake := keyedSessionHandle(t, false)
	seedHeld(fake, s.info.SessionName, "gc:op:1")
	fake.TurnScripts = map[string][]string{s.info.SessionName: {"thread/t1"}}
	res, err := s.handle.Message(context.Background(), handleActor(t, s.handle, sessionpkg.ActorOperator), MessageRequest{
		Text: "hello", Delivery: DeliveryIntentInterruptNow, DeliveryKey: "gc:op:1", Redelivery: true,
	})
	if err != nil || !res.Replayed || res.Queued || res.Interrupt != nil {
		t.Fatalf("Message = %+v, %v; want a replay", res, err)
	}
	if got := sentCalls(fake); len(got) != 0 {
		t.Fatalf("calls %+v, want none: no sighting, interrupt, start or send", got)
	}
	var methods []string
	for _, c := range fake.SnapshotCalls() {
		methods = append(methods, c.Method)
	}
	if !slices.Contains(methods, "LookupDelivery") {
		t.Fatalf("calls = %v, want the lookup", methods)
	}
}

// A failed lookup fails the message with nothing sent; a runtime handle's
// message carries its key, and replays a held one.
func TestMessageRedeliveryLookupAndRuntimeHandle(t *testing.T) {
	s, fake := keyedSessionHandle(t, false)
	fake.LookupDeliveryErrors = map[string]error{s.info.SessionName: runtime.ErrRuntimeUnavailable}
	_, err := s.handle.Message(context.Background(), handleActor(t, s.handle, sessionpkg.ActorOperator), MessageRequest{
		Text: "hello", Delivery: DeliveryIntentInterruptNow, DeliveryKey: "gc:op:1", Redelivery: true,
	})
	if !errors.Is(err, runtime.ErrNudgeNotSent) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("Message after a failed lookup = %v, want not sent, unavailable", err)
	}
	if got := sentCalls(fake); len(got) != 0 {
		t.Fatalf("sent %+v after a failed lookup", got)
	}

	h, rfake := keyedRuntimeHandle(t)
	if _, err := h.Message(context.Background(), handleActor(t, h, sessionpkg.ActorOperator), MessageRequest{Text: "hello", DeliveryKey: "gc:op:1"}); err != nil {
		t.Fatal(err)
	}
	if keys := nudgeDeliveryKeys(rfake.Calls); len(keys) != 1 || keys[0] != "gc:op:1" {
		t.Fatalf("runtime handle message keys = %q", keys)
	}
	rfake.Calls = nil
	res, err := h.Message(context.Background(), handleActor(t, h, sessionpkg.ActorOperator), MessageRequest{Text: "hello", DeliveryKey: "gc:op:1", Redelivery: true})
	if err != nil || !res.Replayed {
		t.Fatalf("runtime handle redelivery = %+v, %v; want a replay", res, err)
	}
	if got := sentCalls(rfake); len(got) != 0 {
		t.Fatalf("runtime handle replay sent %+v", got)
	}
}

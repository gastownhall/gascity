package worker

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
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

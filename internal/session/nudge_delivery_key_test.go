package session

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A live-only nudge carries the caller's delivery key to a provider that
// deduplicates keyed deliveries, so a redelivered queue batch is not
// delivered twice.
func TestSendLiveOnlyPassesTheDeliveryKey(t *testing.T) {
	store := beads.NewMemStore()
	fake := runtime.NewFake()
	mgr := newTestManager(t, store, runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: fake}))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "codex", WorkDir: "/tmp", Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	ctx := runtime.WithNudgeDeliveryKey(context.Background(), "batch-1")
	delivered, err := mgr.SendLiveOnly(ctx, info.ID, "hello", nil)
	if err != nil || !delivered {
		t.Fatalf("SendLiveOnly = %v, %v; want delivered", delivered, err)
	}
	for _, call := range fake.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName {
			if call.DeliveryKey != "batch-1" {
				t.Fatalf("nudge delivery key = %q, want batch-1", call.DeliveryKey)
			}
			return
		}
	}
	t.Fatalf("calls = %#v, want a Nudge", fake.Calls)
}

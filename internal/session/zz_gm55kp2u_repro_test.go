package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// gm-55kp2u: the LiveOnly send paths skip the pendingInteractionLocked guard
// that sendLocked applies, so a live nudge types into an open interaction.
func gm55kp2uLiveOnlyCase(t *testing.T, send func(*Manager, string) (bool, error)) {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "question", Prompt: "How do you want to proceed?"})
	sp.Calls = nil

	delivered, err := send(mgr, info.ID)
	if delivered || !errors.Is(err, ErrPendingInteraction) {
		t.Errorf("delivered=%v err=%v, want delivered=false err=%v", delivered, err, ErrPendingInteraction)
	}
	for _, call := range sp.Calls {
		if (call.Method == "Nudge" || call.Method == "NudgeNow") && call.Name == info.SessionName {
			t.Fatalf("typed into a session with a pending interaction: %#v", call)
		}
	}
}

func TestGM55KP2USendLiveOnlyRejectsPendingInteraction(t *testing.T) {
	gm55kp2uLiveOnlyCase(t, func(m *Manager, id string) (bool, error) {
		return m.SendLiveOnly(context.Background(), id, "hello")
	})
}

func TestGM55KP2USendImmediateLiveOnlyRejectsPendingInteraction(t *testing.T) {
	gm55kp2uLiveOnlyCase(t, func(m *Manager, id string) (bool, error) {
		return m.SendImmediateLiveOnly(context.Background(), id, "hello")
	})
}

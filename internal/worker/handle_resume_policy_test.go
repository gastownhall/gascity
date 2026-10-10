package worker

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestNudgeToHeldSessionQueues is D8 rule 1 (CONTRACT v5.9) at the worker
// boundary: a nudge, in either live delivery mode, never resumes a held
// session. The session queues it and the result says why. Kills a nudge
// that carries the operator's policy.
func TestNudgeToHeldSessionQueues(t *testing.T) {
	for _, delivery := range []NudgeDelivery{NudgeDeliveryDefault, NudgeDeliveryImmediate, NudgeDeliveryWaitIdle} {
		t.Run(string(delivery), func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()))
			b, err := store.Create(beads.Bead{Title: "w", Type: sessionpkg.BeadType, Labels: []string{sessionpkg.LabelSession}, Metadata: map[string]string{
				"session_name": "s-held", "template": "worker", "provider": "claude", "work_dir": "/tmp", "command": "claude",
				"state": string(sessionpkg.StateSuspended), "sleep_intent": "user-hold",
			}})
			if err != nil {
				t.Fatal(err)
			}
			handle, err := NewSessionHandle(SessionHandleConfig{Manager: manager, Session: SessionSpec{ID: b.ID, Command: "claude", Provider: "claude", WorkDir: "/tmp"}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := handle.Nudge(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent), NudgeRequest{Text: "hello", Delivery: delivery})
			if err != nil {
				t.Fatalf("Nudge: %v", err)
			}
			// The session queues a sent nudge itself; a wait-idle nudge is
			// left for the caller to queue.
			want := NudgeQueuedHeld
			if delivery == NudgeDeliveryWaitIdle {
				want = NudgeUndeliveredHeld
			}
			if result.Delivered || result.Undelivered != want || sp.CountCalls("Start", "s-held") != 0 {
				t.Fatalf("Nudge = %+v (starts %d); want %s on the held session", result, sp.CountCalls("Start", "s-held"), want)
			}
		})
	}
}

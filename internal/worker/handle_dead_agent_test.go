package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// typedInto counts the calls after before that typed into name.
func typedInto(sp *runtime.Fake, name string, before int) int {
	n := 0
	for _, c := range sp.Calls[before:] {
		if c.Name == name && (c.Method == "Nudge" || c.Method == "NudgeNow" || c.Method == "SendKeys") {
			n++
		}
	}
	return n
}

// freshErrFake is a provider whose fresh liveness read fails.
type freshErrFake struct{ *runtime.Fake }

func (freshErrFake) ObserveLivenessSince(string, []string, time.Time) (runtime.Liveness, error) {
	return runtime.Liveness{}, errors.New("tmux state refresh failed")
}

// TestRuntimeHandleNudgeHoldsOffACrashedAgent (mc-48az4, mc-wdjni): the
// runtime-only handle the CLI nudge, mail notify, sling and the queued
// delivery use for a running pane reads it fresh by the target's process
// names: over a crashed agent, or when the fresh read fails, no nudge of any
// delivery is typed, and the result says so (agent_dead) for the caller to
// queue. Without process names the agent reads alive and the nudge is
// delivered.
func TestRuntimeHandleNudgeHoldsOffACrashedAgent(t *testing.T) {
	for _, delivery := range []NudgeDelivery{NudgeDeliveryDefault, NudgeDeliveryImmediate, NudgeDeliveryWaitIdle} {
		for _, wake := range []NudgeWakePolicy{"", NudgeWakeLiveOnly} {
			t.Run(string(delivery)+"/"+string(wake), func(t *testing.T) {
				sp := runtime.NewFake()
				if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
					t.Fatal(err)
				}
				sp.Zombies["worker"] = true
				handle, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "worker", ProviderName: "codex", ProcessNames: []string{"codex"}})
				if err != nil {
					t.Fatal(err)
				}
				before := len(sp.Calls)
				result, err := handle.Nudge(context.Background(), testActor(t, sessionpkg.ActorAgent), NudgeRequest{Text: "hello", Delivery: delivery, Wake: wake})
				if err != nil || result.Delivered || result.Undelivered != NudgeUndeliveredAgentDead || typedInto(sp, "worker", before) != 0 {
					t.Fatalf("Nudge = %+v, %v (typed %d); want agent_dead, nothing typed", result, err, typedInto(sp, "worker", before))
				}
			})
		}
	}
	t.Run("fresh read fails", func(t *testing.T) {
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		handle, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: freshErrFake{sp}, SessionName: "worker", ProviderName: "codex", ProcessNames: []string{"codex"}})
		if err != nil {
			t.Fatal(err)
		}
		before := len(sp.Calls)
		result, err := handle.Nudge(context.Background(), testActor(t, sessionpkg.ActorAgent), NudgeRequest{Text: "hello", Delivery: NudgeDeliveryImmediate})
		if err != nil || result.Delivered || result.Undelivered != NudgeUndeliveredAgentDead || typedInto(sp, "worker", before) != 0 {
			t.Fatalf("Nudge = %+v, %v (typed %d); want agent_dead, nothing typed", result, err, typedInto(sp, "worker", before))
		}
	})
	t.Run("no process names", func(t *testing.T) {
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		sp.Zombies["worker"] = true
		handle, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "worker", ProviderName: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		result, err := handle.Nudge(context.Background(), testActor(t, sessionpkg.ActorAgent), NudgeRequest{Text: "hello", Delivery: NudgeDeliveryImmediate})
		if err != nil || !result.Delivered {
			t.Fatalf("Nudge = %+v, %v; want delivered", result, err)
		}
	})
}

// TestSessionHandleNudgeOverACrashedAgent (mc-wdjni): the session handle
// hands the session's process names to the manager's live-only sends, so a
// live-only nudge of any delivery over a crashed agent is undelivered and
// types nothing; a sent nudge is queued by the session and says queued, not
// held.
func TestSessionHandleNudgeOverACrashedAgent(t *testing.T) {
	setup := func(t *testing.T, provider, transport string) (*SessionHandle, *runtime.Fake) {
		store := beads.NewMemStore()
		sp := runtime.NewFake()
		manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()))
		b, err := store.Create(beads.Bead{Title: "w", Type: sessionpkg.BeadType, Labels: []string{sessionpkg.LabelSession}, Metadata: map[string]string{
			"session_name": "s-dead", "template": "worker", "provider": provider, "work_dir": "/tmp", "command": provider, "state": string(sessionpkg.StateActive), "transport": transport,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := sp.Start(context.Background(), "s-dead", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		sp.Zombies["s-dead"] = true
		handle, err := NewSessionHandle(SessionHandleConfig{Manager: manager, Session: SessionSpec{ID: b.ID, Command: provider, Provider: provider, WorkDir: "/tmp", Hints: runtime.Config{ProcessNames: []string{provider}}}})
		if err != nil {
			t.Fatal(err)
		}
		return handle, sp
	}
	for _, delivery := range []NudgeDelivery{NudgeDeliveryDefault, NudgeDeliveryImmediate, NudgeDeliveryWaitIdle} {
		t.Run("live-only "+string(delivery), func(t *testing.T) {
			handle, sp := setup(t, "claude", "acp") // an ACP wait-idle nudge goes straight to the runtime
			before := len(sp.Calls)
			result, err := handle.Nudge(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent), NudgeRequest{Text: "hello", Delivery: delivery, Wake: NudgeWakeLiveOnly})
			if err != nil || result.Delivered || typedInto(sp, "s-dead", before) != 0 {
				t.Fatalf("Nudge = %+v, %v (typed %d); want undelivered, nothing typed", result, err, typedInto(sp, "s-dead", before))
			}
		})
	}
	t.Run("sent", func(t *testing.T) {
		handle, sp := setup(t, "codex", "")
		before := len(sp.Calls)
		result, err := handle.Nudge(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent), NudgeRequest{Text: "hello", Delivery: NudgeDeliveryImmediate})
		if err != nil || result.Delivered || result.Undelivered != NudgeQueued || typedInto(sp, "s-dead", before) != 0 {
			t.Fatalf("Nudge = %+v, %v (typed %d); want queued, nothing typed", result, err, typedInto(sp, "s-dead", before))
		}
	})
}

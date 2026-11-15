package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// An open AskUserQuestion dialog is a pending interaction of kind "question".
// A live-only delivery (the queued-nudge path) must report it undelivered with
// the one error every consumer classifies, and must not type into the pane.
func TestSessionHandleLiveOnlyNudgeRefusesQuestionInteraction(t *testing.T) {
	for _, delivery := range []NudgeDelivery{NudgeDeliveryDefault, NudgeDeliveryImmediate} {
		t.Run(string(delivery), func(t *testing.T) {
			handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
				Profile:  ProfileClaudeTmuxCLI,
				Template: "probe",
				Title:    "Probe",
				Command:  "claude",
				WorkDir:  t.TempDir(),
				Provider: "claude",
			})
			actor := handleActor(t, handle, sessionpkg.ActorAgent)
			if err := handle.Start(context.Background(), actor); err != nil {
				t.Fatalf("Start: %v", err)
			}
			info, err := mgr.Get(handle.sessionID)
			if err != nil {
				t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
			}
			sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
				RequestID: "tmux-question-0123456789abcdef",
				Kind:      "question",
				Prompt:    "How do you want to proceed?",
			})
			sp.Calls = nil

			result, err := handle.Nudge(context.Background(), actor, NudgeRequest{Text: "hello", Delivery: delivery, Wake: NudgeWakeLiveOnly})
			if result.Delivered {
				t.Fatalf("Nudge reported Delivered=true into an open question dialog: %+v", result)
			}
			if !errors.Is(err, sessionpkg.ErrPendingInteraction) {
				t.Fatalf("Nudge = %v, want session.ErrPendingInteraction", err)
			}
			for _, call := range sp.Calls {
				if (call.Method == "Nudge" || call.Method == "NudgeNow") && call.Name == info.SessionName {
					t.Fatalf("typed into a session with an open question dialog: %#v", call)
				}
			}
		})
	}
}

// A runtime-only handle has no manager to ask: the provider's own refusal
// (runtime.ErrPendingInteraction, the same sentinel session.ErrPendingInteraction
// aliases) is what reaches the caller, undelivered and untranslated.
func TestRuntimeHandleReportsProviderPendingInteractionRefusal(t *testing.T) {
	ctx := context.Background()
	sp := runtime.NewFake()
	if err := sp.Start(ctx, "legacy-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.NudgeErrors = map[string]error{"legacy-worker": fmt.Errorf("%w: legacy-worker is waiting on a question", runtime.ErrPendingInteraction)}
	handle, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "legacy-worker", ProviderName: "stub", Transport: "acp"})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	for _, delivery := range []NudgeDelivery{NudgeDeliveryDefault, NudgeDeliveryImmediate} {
		result, err := handle.Nudge(ctx, testOperator(t), NudgeRequest{Text: "hello", Delivery: delivery})
		if result.Delivered || !errors.Is(err, sessionpkg.ErrPendingInteraction) || !errors.Is(err, runtime.ErrPendingInteraction) {
			t.Errorf("Nudge(%q) = %+v, %v; want undelivered with ErrPendingInteraction", delivery, result, err)
		}
	}
}

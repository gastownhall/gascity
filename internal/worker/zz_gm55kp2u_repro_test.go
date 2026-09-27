package worker

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// gm-55kp2u: none of the six tmux/session/poller RED tests for this bug
// exercise the worker boundary directly. A pending AskUserQuestion dialog
// must convert to an explicit NudgeResult at both SessionHandle and
// RuntimeHandle — never a propagated Go error a caller could mistake for an
// operational failure and dead-letter (ga-38q9xh.1 acceptance criteria).

func TestGM55KP2USessionHandleNudgeReportsBlockedByDialog(t *testing.T) {
	handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("manager.Get(%q): %v", handle.sessionID, err)
	}
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "question", Prompt: "How do you want to proceed?"})
	sp.Calls = nil

	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "check deploy status",
		Delivery: NudgeDeliveryDefault,
		Wake:     NudgeWakeLiveOnly,
	})
	if err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if result.Delivered || result.Undelivered != NudgeUndeliveredBlockedByDialog {
		t.Fatalf("result = %#v, want Delivered=false Undelivered=%q", result, NudgeUndeliveredBlockedByDialog)
	}
	for _, call := range sp.Calls {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			t.Fatalf("typed into a session with a pending interaction: %#v", call)
		}
	}
}

func TestGM55KP2URuntimeHandleNudgeReportsBlockedByDialog(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "legacy-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.SetPendingInteraction("legacy-worker", &runtime.PendingInteraction{RequestID: "req-1", Kind: "question", Prompt: "How do you want to proceed?"})

	handle, err := NewRuntimeHandle(RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  "legacy-worker",
		ProviderName: "codex",
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	sp.Calls = nil

	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "check deploy status",
		Delivery: NudgeDeliveryImmediate,
	})
	if err != nil {
		t.Fatalf("Nudge(immediate): %v", err)
	}
	if result.Delivered || result.Undelivered != NudgeUndeliveredBlockedByDialog {
		t.Fatalf("result = %#v, want Delivered=false Undelivered=%q", result, NudgeUndeliveredBlockedByDialog)
	}
	for _, call := range sp.Calls {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			t.Fatalf("typed into a session with a pending interaction: %#v", call)
		}
	}
}

package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func submitWithNudgeError(t *testing.T, nudgeErr error) (SubmitOutcome, error) {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "gemini", WorkDir: t.TempDir(), Provider: "gemini", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.NudgeErrors = map[string]error{info.SessionName: nudgeErr}
	return mgr.Submit(context.Background(), info.ID, "hello", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentDefault, ResumeOperator)
}

func TestSubmitTreatsDeliveredUnobservedNudgeAsDelivered(t *testing.T) {
	nudgeErr := fmt.Errorf("%w: session %q", runtime.ErrNudgeDeliveredUnobserved, "helper")
	outcome, err := submitWithNudgeError(t, nudgeErr)
	if err != nil {
		t.Fatalf("Submit(default) = %v, want nil for a delivered-but-unobserved nudge", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(default) unexpectedly queued")
	}
}

func TestSubmitStillFailsOnUnconfirmedNudge(t *testing.T) {
	unconfirmed := errors.New("nudge: submit Enter delivered to tmux but not confirmed (busy state never observed)")
	_, err := submitWithNudgeError(t, unconfirmed)
	if !errors.Is(err, unconfirmed) {
		t.Fatalf("Submit(default) = %v, want the unconfirmed nudge error", err)
	}
}

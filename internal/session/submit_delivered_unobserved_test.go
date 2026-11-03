package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
)

// ga-bdb0ad: the tmux provider returns ErrNudgeSubmitDeliveredUnobserved when
// the submit Enter reached the pane AND the composer drained — delivery is
// proven, only the busy-indicator observation missed. The session layer must
// report that as a delivered message, not a failure: the error text says
// "submit failed", so a caller that trusts it re-sends and double-delivers.

// deliveredUnobservedErr is the exact shape tmux.nudgeSession returns.
func deliveredUnobservedErr(sessName string) error {
	return fmt.Errorf("%w: session %q", tmux.ErrNudgeSubmitDeliveredUnobserved, sessName)
}

func newDeliveredUnobservedClaudeSession(t *testing.T) (*Manager, *runtime.Fake, Info) {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.NudgeErrors = map[string]error{info.SessionName: deliveredUnobservedErr(info.SessionName)}
	return mgr, sp, info
}

func countNudges(sp *runtime.Fake, sessName string) int {
	n := 0
	for _, call := range sp.Calls {
		if (call.Method == "NudgeNow" || call.Method == "Nudge") && call.Name == sessName {
			n++
		}
	}
	return n
}

func TestSubmitInterruptNowReportsDeliveredUnobservedAsDelivered(t *testing.T) {
	mgr, sp, info := newDeliveredUnobservedClaudeSession(t)

	outcome, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "stop and do this instead", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now) = %v; want nil: the provider proved delivery (composer drained), only the busy observation missed", err)
	}
	if outcome.Queued {
		t.Fatal("Submit(interrupt_now) queued; want delivered now")
	}
	if got := countNudges(sp, info.SessionName); got != 1 {
		t.Fatalf("nudge calls = %d, want exactly 1 (no re-send of a delivered message)", got)
	}
	for _, call := range sp.Calls {
		if call.Method == "Stop" {
			t.Fatalf("unexpected Stop: a delivered-unobserved submit must not trigger the hard-restart fallback; calls = %#v", sp.Calls)
		}
	}
}

// The wait-idle paths swallow EVERY nudge error into (false, nil), so a
// delivered-unobserved submit reads as "not delivered" and callers that fall
// back to the queue (gc mail notify, gc sling --nudge) deliver it a second
// time. cmd/gc's errors.Is(nudgeErr, tmux.ErrNudgeSubmitDeliveredUnobserved)
// check in sendMailNotifyWithWorker can never fire for these handles.
func TestTryWaitIdleNudgeLiveOnlyReportsDeliveredUnobservedAsDelivered(t *testing.T) {
	mgr, sp, info := newDeliveredUnobservedClaudeSession(t)
	sp.WaitForIdleErrors[info.SessionName] = nil // pane reaches idle

	delivered, err := mgr.TryWaitIdleNudgeLiveOnly(context.Background(), info.ID, "mail", "You have mail from mayor", nil)
	if err != nil {
		t.Fatalf("TryWaitIdleNudgeLiveOnly error = %v", err)
	}
	if !delivered {
		t.Fatal("TryWaitIdleNudgeLiveOnly delivered = false for a proven delivery; the caller will queue and re-deliver it")
	}
}

func TestTryWaitIdleNudgeReportsDeliveredUnobservedAsDelivered(t *testing.T) {
	mgr, sp, info := newDeliveredUnobservedClaudeSession(t)
	sp.WaitForIdleErrors[info.SessionName] = nil

	delivered, err := mgr.TryWaitIdleNudge(context.Background(), testActor(mgr, ActorBackground), info.ID, "sling", "Work slung. Check your hook.", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
	if err != nil {
		t.Fatalf("TryWaitIdleNudge error = %v", err)
	}
	if !delivered {
		t.Fatal("TryWaitIdleNudge delivered = false for a proven delivery")
	}
}

// Control: a genuinely unconfirmed submit (composer NOT drained — the draft may
// still be sitting unsubmitted) must keep surfacing as a failure. A fix that
// swallows every nudge error would pass the tests above and fail this one.
func TestSubmitStillFailsOnGenuinelyUnconfirmedSubmit(t *testing.T) {
	mgr, sp, info := newDeliveredUnobservedClaudeSession(t)
	sp.NudgeErrors[info.SessionName] = fmt.Errorf("%w: session %q", tmux.ErrNudgeSubmitUnconfirmed, info.SessionName)

	_, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "stop and do this instead", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if !errors.Is(err, tmux.ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("Submit(interrupt_now) error = %v, want ErrNudgeSubmitUnconfirmed to propagate", err)
	}
}

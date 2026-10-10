package session

import (
	"context"
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestViaControllerNeverStarts: ResumeViaController (the API without
// resume: true) queues any message to a session whose runtime is not
// running, held or not, and marks the queue Deferred so the caller asks the
// controller to start it; a running session takes the message live. Kills
// an in-process start, a Deferred mark on a live delivery, and a dormant
// branch that reports queued without enqueueing (MC-2).
func TestViaControllerNeverStarts(t *testing.T) {
	for name, live := range map[string]bool{"asleep, unheld": false, "running": true} {
		t.Run(name, func(t *testing.T) {
			meta := policyRow(nil)
			if live {
				meta["state"] = string(StateActive)
			}
			e := newPolicyEnv(t, meta, live)
			startsBefore := e.sp.CountCalls("Start", resumeName)
			out, err := e.mgr.Submit(context.Background(), e.id, "hello", "claude --resume k", runtime.Config{}, SubmitIntentDefault, ResumeViaController)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if out.Queued == live || out.Deferred == live || e.sp.CountCalls("Start", resumeName) != startsBefore {
				t.Fatalf("Submit = %+v (starts %d); want queued+deferred=%v and no start", out, e.sp.CountCalls("Start", resumeName)-startsBefore, !live)
			}
			if got := e.queued(t); (len(got) == 1 && got[0] == "hello") == live {
				t.Fatalf("queue = %q, want the message queued only for a dormant session", got)
			}
		})
	}
}

// TestViaControllerQueuesForHeldLiveRow is the re-review's N2 (S2 reopened):
// between a managed suspend and the controller's stop the runtime is still
// live. An API message must queue, not reach the runtime and flip the row
// active. Kills a ViaController policy that reads only liveness.
func TestViaControllerQueuesForHeldLiveRow(t *testing.T) {
	e := newPolicyEnv(t, policyRow(map[string]string{"state": string(StateSuspended), "sleep_intent": "user-hold"}), true)
	before := e.row(t)
	out, err := e.mgr.Submit(context.Background(), e.id, "hello", "claude --resume k", runtime.Config{}, SubmitIntentDefault, ResumeViaController)
	if err != nil || !out.Queued {
		t.Fatalf("Submit = %+v, %v; want queued", out, err)
	}
	if got := e.row(t); !maps.Equal(got, before) || e.sp.CountCalls("Nudge", resumeName)+e.sp.CountCalls("NudgeNow", resumeName) != 0 {
		t.Fatalf("row = %v (nudges %d); want the suspended row untouched and nothing delivered", got, e.sp.CountCalls("Nudge", resumeName))
	}
}

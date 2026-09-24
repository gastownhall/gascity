package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// pendingProbeSession is the seat every case in this file drives. One name for
// the handle, the fake, and the call assertions keeps them provably about the
// same seat.
const pendingProbeSession = "legacy-worker"

// newPendingProbeHandle returns a live claude-backed RuntimeHandle over a fresh
// fake, which is the handle every LIVE seat resolves: workerHandleForNudgeTarget
// hands back a Manager-backed handle only for a seat observed NOT running, so
// the Manager's pending-interaction guard never sees the traffic that can
// actually be sitting at a prompt.
func newPendingProbeHandle(t *testing.T) (*RuntimeHandle, *runtime.Fake) {
	t.Helper()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), pendingProbeSession, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.WaitForIdleErrors[pendingProbeSession] = nil
	handle, err := NewRuntimeHandle(RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  pendingProbeSession,
		ProviderName: "claude",
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	return handle, sp
}

func assertNoKeystrokes(t *testing.T, sp *runtime.Fake) {
	t.Helper()
	for _, call := range sp.SnapshotCalls() {
		switch call.Method {
		case "Nudge", "NudgeNow", "SendKeys":
			if call.Name == pendingProbeSession {
				t.Fatalf("typed into a seat awaiting human input: %#v", sp.SnapshotCalls())
			}
		}
	}
}

// A prompted seat reads as idle to WaitForIdle — raising an approval dialog is
// what clears the busy indicator, while the ready-prompt prefix stays on the
// pane — so wait-idle delivery is the dominant producer of the #2892 mistype
// rather than a rare one. The pre-wait probe refuses it before the wait.
func TestRuntimeHandleNudgeWaitIdleRefusesPromptOpenBeforeCall(t *testing.T) {
	handle, sp := newPendingProbeHandle(t)
	sp.SetPendingInteraction(pendingProbeSession, &runtime.PendingInteraction{
		RequestID: "req-1", Kind: "approval", Prompt: "approve?",
	})

	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "check deploy status",
		Delivery: NudgeDeliveryWaitIdle,
		Source:   "mail",
	})
	if !errors.Is(err, sessionpkg.ErrPendingInteraction) {
		t.Fatalf("Nudge(wait_idle) err = %v, want %v", err, sessionpkg.ErrPendingInteraction)
	}
	if result.Delivered {
		t.Fatal("Nudge(wait_idle) Delivered = true, want false for a prompted seat")
	}
	assertNoKeystrokes(t, sp)
	// Refuse before spending the wait: the answer is already known, and the
	// mail/sling lanes queue on the error either way.
	if got := sp.CountCalls("WaitForIdle", pendingProbeSession); got != 0 {
		t.Fatalf("WaitForIdle calls = %d, want 0 (refusal precedes the wait)", got)
	}
}

// The pre-wait probe only proves no prompt was open up to
// runtimeHandleWaitIdleTimeout ago, and WaitForIdle is liable to return BECAUSE
// a dialog opened. Without the second probe, nudgeNow clears the input line and
// pastes — answering the operator's question on their behalf.
func TestRuntimeHandleNudgeWaitIdleRefusesPromptOpenedDuringWait(t *testing.T) {
	handle, sp := newPendingProbeHandle(t)
	started := make(chan struct{})
	gate := make(chan struct{})
	sp.WaitForIdleStarted[pendingProbeSession] = started
	sp.WaitForIdleGates[pendingProbeSession] = gate

	type outcome struct {
		result NudgeResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := handle.Nudge(context.Background(), NudgeRequest{
			Text:     "check deploy status",
			Delivery: NudgeDeliveryWaitIdle,
			Source:   "mail",
		})
		done <- outcome{result, err}
	}()

	<-started // the pre-wait probe has already passed: no prompt was open
	sp.SetPendingInteraction(pendingProbeSession, &runtime.PendingInteraction{
		RequestID: "req-1", Kind: "approval", Prompt: "approve?",
	})
	close(gate)

	got := <-done
	if !errors.Is(got.err, sessionpkg.ErrPendingInteraction) {
		t.Fatalf("Nudge(wait_idle) err = %v, want %v (prompt opened during the wait)", got.err, sessionpkg.ErrPendingInteraction)
	}
	if got.result.Delivered {
		t.Fatal("reported delivery into a prompt that opened during the wait")
	}
	assertNoKeystrokes(t, sp)
	if got := sp.CountCalls("WaitForIdle", pendingProbeSession); got != 1 {
		t.Fatalf("WaitForIdle calls = %d, want 1 (the wait ran, then the second probe refused)", got)
	}
}

// A broken probe is not an absent prompt. Every sibling guard fails closed on a
// probe error, and this one must too, or a pane-read failure becomes a license
// to type into whatever is on the pane.
func TestRuntimeHandleNudgeWaitIdleFailsClosedOnProbeError(t *testing.T) {
	handle, sp := newPendingProbeHandle(t)
	probeErr := errors.New("capture-pane failed")
	sp.PendingErrors[pendingProbeSession] = probeErr

	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "check deploy status",
		Delivery: NudgeDeliveryWaitIdle,
		Source:   "mail",
	})
	if !errors.Is(err, probeErr) {
		t.Fatalf("Nudge(wait_idle) err = %v, want %v", err, probeErr)
	}
	if result.Delivered {
		t.Fatal("Nudge(wait_idle) Delivered = true, want false when the probe cannot answer")
	}
	assertNoKeystrokes(t, sp)
}

// Unsupported providers keep their typed Undelivered result rather than
// acquiring a refusal: the probe sits after the early-outs, so a codex seat
// still reports "this runtime cannot take live delivery" and nothing probes it.
func TestRuntimeHandleNudgeWaitIdleUnsupportedProviderSkipsProbe(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), pendingProbeSession, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.SetPendingInteraction(pendingProbeSession, &runtime.PendingInteraction{RequestID: "req-1"})
	handle, err := NewRuntimeHandle(RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  pendingProbeSession,
		ProviderName: "codex",
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}

	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "check deploy status",
		Delivery: NudgeDeliveryWaitIdle,
		Source:   "mail",
	})
	if err != nil {
		t.Fatalf("Nudge(wait_idle) err = %v, want nil", err)
	}
	if result.Delivered || result.Undelivered != NudgeUndeliveredProviderUnsupported {
		t.Fatalf("result = %+v, want undelivered/%s", result, NudgeUndeliveredProviderUnsupported)
	}
	if got := sp.CountCalls("Pending", pendingProbeSession); got != 0 {
		t.Fatalf("Pending calls = %d, want 0 (early-outs precede the probe)", got)
	}
}

// The queued-nudge poller resolves this handle and delivers through the default
// arm. Its ErrPendingInteraction release branch — which returns claims instead
// of charging a delivery attempt — can only fire if this arm produces the
// sentinel, so the branch's reachability is this assertion.
func TestRuntimeHandleNudgeDefaultRefusesPendingInteraction(t *testing.T) {
	handle, sp := newPendingProbeHandle(t)
	sp.SetPendingInteraction(pendingProbeSession, &runtime.PendingInteraction{
		RequestID: "req-1", Kind: "approval", Prompt: "approve?",
	})

	result, err := handle.Nudge(context.Background(), NudgeRequest{Text: "queued item"})
	if !errors.Is(err, sessionpkg.ErrPendingInteraction) {
		t.Fatalf("Nudge(default) err = %v, want %v", err, sessionpkg.ErrPendingInteraction)
	}
	if result.Delivered {
		t.Fatal("Nudge(default) Delivered = true, want false for a prompted seat")
	}
	assertNoKeystrokes(t, sp)
}

// Immediate delivery is the operator's own "type this now", so it keeps its
// unguarded semantics; only the two delivery modes a controller picks on the
// operator's behalf refuse.
func TestRuntimeHandleNudgeImmediateStillDeliversToPromptedSeat(t *testing.T) {
	handle, sp := newPendingProbeHandle(t)
	sp.SetPendingInteraction(pendingProbeSession, &runtime.PendingInteraction{RequestID: "req-1"})

	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "type this now",
		Delivery: NudgeDeliveryImmediate,
	})
	if err != nil {
		t.Fatalf("Nudge(immediate) err = %v, want nil", err)
	}
	if !result.Delivered {
		t.Fatal("Nudge(immediate) Delivered = false, want true")
	}
	if got := sp.CountCalls("NudgeNow", pendingProbeSession); got != 1 {
		t.Fatalf("NudgeNow calls = %d, want 1", got)
	}
}

// Pending is consumed by status surfaces, and it used to report a broken probe
// as "nothing pending" because its pending==nil check preceded its error
// return. A status surface that says "no prompt" for a seat sitting at a dialog
// is the same fail-open the delivery guards were consolidated to end.
func TestRuntimeHandlePendingFailsClosedOnProbeError(t *testing.T) {
	handle, sp := newPendingProbeHandle(t)
	probeErr := errors.New("capture-pane failed")
	sp.PendingErrors[pendingProbeSession] = probeErr

	pending, err := handle.Pending(context.Background())
	if !errors.Is(err, probeErr) {
		t.Fatalf("Pending err = %v, want %v", err, probeErr)
	}
	if pending != nil {
		t.Fatalf("Pending = %+v, want nil alongside the error", pending)
	}

	// PendingStatus consumes Pending and inherits its behavior, so it is not
	// covered by the assertion above.
	pending, supported, err := handle.PendingStatus(context.Background())
	if !errors.Is(err, probeErr) {
		t.Fatalf("PendingStatus err = %v, want %v", err, probeErr)
	}
	if pending != nil {
		t.Fatalf("PendingStatus pending = %+v, want nil alongside the error", pending)
	}
	if !supported {
		t.Fatal("PendingStatus supported = false, want true: the provider implements the probe, it just failed")
	}
}

// The unsupported sentinel is the one probe error that still means "no prompt",
// and that normalization now lives in runtime.PendingInteractionFor rather than
// in a copy here.
func TestRuntimeHandlePendingTreatsUnsupportedAsNoPrompt(t *testing.T) {
	handle, sp := newPendingProbeHandle(t)
	sp.PendingErrors[pendingProbeSession] = runtime.ErrInteractionUnsupported

	pending, err := handle.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending err = %v, want nil for the unsupported sentinel", err)
	}
	if pending != nil {
		t.Fatalf("Pending = %+v, want nil", pending)
	}
}

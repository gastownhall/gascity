package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// waitingNudgeProvider models the shape every runtime whose Nudge is not a send
// has: a courtesy wait, then keystrokes. tmux is the production instance —
// Nudge is an up-to-NudgeIdleTimeout WaitForIdle followed by NudgeNow — and its
// NudgeUnlessPending composes the same two pieces this stub does, so the
// ordering property pinned here is the one tmux relies on.
type waitingNudgeProvider struct {
	*Fake
	waitTimeout time.Duration
}

func (p *waitingNudgeProvider) NudgeUnlessPending(name string, content []ContentBlock) error {
	_ = p.WaitForIdle(context.Background(), name, p.waitTimeout)
	return SendUnlessPending(p, name, content)
}

func newWaitingNudgeProvider(t *testing.T, name string) *waitingNudgeProvider {
	t.Helper()
	fake := NewFake()
	if err := fake.Start(context.Background(), name, Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors[name] = nil
	return &waitingNudgeProvider{Fake: fake, waitTimeout: time.Second}
}

// plainNudgeProvider exposes only the core Provider surface: no interaction
// probe to ask, and no guarded sequence of its own. Embedding the interface
// rather than the Fake is what hides those methods.
type plainNudgeProvider struct {
	Provider
}

func assertNoSend(t *testing.T, fake *Fake, name string) {
	t.Helper()
	for _, call := range fake.SnapshotCalls() {
		switch call.Method {
		case "Nudge", "NudgeNow", "SendKeys":
			if call.Name == name {
				t.Fatalf("typed into a seat awaiting human input: %#v", fake.SnapshotCalls())
			}
		}
	}
}

// The probe a caller runs before calling Nudge only proves no prompt was open
// up to a wait ago, and the wait is correlated with the hazard rather than
// incidental to it: raising an approval dialog is what clears the busy
// indicator, so WaitForIdle is liable to return BECAUSE the dialog opened.
// Only a probe on the far side of the wait catches that.
func TestNudgeUnlessPendingForRefusesPromptOpenedDuringTheWait(t *testing.T) {
	const name = "worker-1"
	sp := newWaitingNudgeProvider(t, name)
	started := make(chan struct{})
	gate := make(chan struct{})
	sp.WaitForIdleStarted[name] = started
	sp.WaitForIdleGates[name] = gate

	done := make(chan error, 1)
	go func() {
		done <- NudgeUnlessPendingFor(sp, name, TextContent("queued item"))
	}()

	<-started // no prompt was open when the wait began
	sp.SetPendingInteraction(name, &PendingInteraction{
		RequestID: "req-1", Kind: "approval", Prompt: "approve?",
	})
	close(gate)

	if err := <-done; !errors.Is(err, ErrNudgeRefusedPendingInteraction) {
		t.Fatalf("NudgeUnlessPendingFor err = %v, want %v", err, ErrNudgeRefusedPendingInteraction)
	}
	assertNoSend(t, sp.Fake, name)
	if got := sp.CountCalls("WaitForIdle", name); got != 1 {
		t.Fatalf("WaitForIdle calls = %d, want 1 (the runtime keeps its own wait)", got)
	}
}

// The guarded delivery must keep the runtime's wait, not replace it. Dropping
// the wait would stop the courtesy behavior that avoids interrupting an active
// tool call, which is a delivery-semantics regression rather than a fix.
func TestNudgeUnlessPendingForKeepsTheRuntimesOwnWait(t *testing.T) {
	const name = "worker-1"
	sp := newWaitingNudgeProvider(t, name)

	if err := NudgeUnlessPendingFor(sp, name, TextContent("queued item")); err != nil {
		t.Fatalf("NudgeUnlessPendingFor err = %v, want nil", err)
	}
	if got := sp.CountCalls("WaitForIdle", name); got != 1 {
		t.Fatalf("WaitForIdle calls = %d, want 1", got)
	}
	if got := sp.CountCalls("NudgeNow", name); got != 1 {
		t.Fatalf("NudgeNow calls = %d, want 1", got)
	}
}

func deliveryMethods(t *testing.T, fake *Fake, name string) []string {
	t.Helper()
	var methods []string
	for _, call := range fake.SnapshotCalls() {
		if call.Name != name {
			continue
		}
		switch call.Method {
		case "Pending", "Nudge", "NudgeNow":
			methods = append(methods, call.Method)
		}
	}
	return methods
}

func assertMethods(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("methods = %v, want %v", got, want)
		}
	}
}

// The generic path adds a probe in front of the runtime's own delivery; it does
// not pick a different one. Substituting NudgeNow for Nudge here would strip
// the courtesy wait from every runtime that has one and has not yet been taught
// the guarded sequence — trading a mistype for a nudge injected mid-tool-call.
func TestNudgeUnlessPendingForDoesNotSubstituteTheDeliveryCall(t *testing.T) {
	const name = "worker-1"
	sp := NewFake()
	if err := sp.Start(context.Background(), name, Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := NudgeUnlessPendingFor(sp, name, TextContent("queued item")); err != nil {
		t.Fatalf("NudgeUnlessPendingFor err = %v, want nil", err)
	}
	assertMethods(t, deliveryMethods(t, sp, name), []string{"Pending", "Nudge"})
}

// SendUnlessPending is the other half, for a runtime that has already run its
// own pre-delivery sequence: it prefers the immediate send, because going back
// through Nudge would run that sequence a second time.
func TestSendUnlessPendingPrefersTheImmediateSend(t *testing.T) {
	const name = "worker-1"
	sp := NewFake()
	if err := sp.Start(context.Background(), name, Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := SendUnlessPending(sp, name, TextContent("queued item")); err != nil {
		t.Fatalf("SendUnlessPending err = %v, want nil", err)
	}
	assertMethods(t, deliveryMethods(t, sp, name), []string{"Pending", "NudgeNow"})
}

func TestNudgeUnlessPendingForRefusesPromptOpenBeforeTheCall(t *testing.T) {
	const name = "worker-1"
	sp := NewFake()
	if err := sp.Start(context.Background(), name, Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.SetPendingInteraction(name, &PendingInteraction{RequestID: "req-1"})

	err := NudgeUnlessPendingFor(sp, name, TextContent("queued item"))
	if !errors.Is(err, ErrNudgeRefusedPendingInteraction) {
		t.Fatalf("NudgeUnlessPendingFor err = %v, want %v", err, ErrNudgeRefusedPendingInteraction)
	}
	assertNoSend(t, sp, name)
}

// A probe that will not answer is not an absent prompt. Every caller of this
// policy fails closed, so the failure is reported rather than swallowed — under
// its own sentinel, not the refusal sentinel, because the two lead to different
// handling upstream (retry the probe vs. wait for the human). Both halves are
// load-bearing: the distinct sentinel is what lets a caller with a bounded
// retry budget decline to charge a broken probe against it without collapsing
// the two conditions.
func TestNudgeUnlessPendingForFailsClosedOnProbeError(t *testing.T) {
	const name = "worker-1"
	sp := NewFake()
	if err := sp.Start(context.Background(), name, Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	probeErr := errors.New("capture-pane failed")
	sp.PendingErrors[name] = probeErr

	err := NudgeUnlessPendingFor(sp, name, TextContent("queued item"))
	if !errors.Is(err, probeErr) {
		t.Fatalf("NudgeUnlessPendingFor err = %v, want %v", err, probeErr)
	}
	if !errors.Is(err, ErrNudgePendingProbeFailed) {
		t.Fatalf("NudgeUnlessPendingFor err = %v, want it to wrap %v", err, ErrNudgePendingProbeFailed)
	}
	if errors.Is(err, ErrNudgeRefusedPendingInteraction) {
		t.Fatalf("a broken probe was reported as a pending-interaction refusal: %v", err)
	}
	assertNoSend(t, sp, name)
}

// The unsupported sentinel still means "no prompt", so a runtime without
// interaction support keeps delivering rather than refusing everything.
func TestNudgeUnlessPendingForDeliversWhenProbeUnsupported(t *testing.T) {
	const name = "worker-1"
	sp := NewFake()
	if err := sp.Start(context.Background(), name, Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.PendingErrors[name] = ErrInteractionUnsupported

	if err := NudgeUnlessPendingFor(sp, name, TextContent("queued item")); err != nil {
		t.Fatalf("NudgeUnlessPendingFor err = %v, want nil", err)
	}
	if got := sp.CountCalls("Nudge", name); got != 1 {
		t.Fatalf("Nudge calls = %d, want 1", got)
	}
}

// A runtime that cannot be probed at all still delivers, through exactly the
// Nudge it had before this guard existed. An unguarded gap on a runtime with no
// interaction surface is a known limitation; silently dropping its nudges would
// be a new outage.
func TestNudgeUnlessPendingForDeliversWhenTheRuntimeCannotBeProbed(t *testing.T) {
	const name = "worker-1"
	fake := NewFake()
	if err := fake.Start(context.Background(), name, Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.SetPendingInteraction(name, &PendingInteraction{RequestID: "req-1"})
	sp := plainNudgeProvider{Provider: fake}

	if err := NudgeUnlessPendingFor(sp, name, TextContent("queued item")); err != nil {
		t.Fatalf("NudgeUnlessPendingFor err = %v, want nil", err)
	}
	if got := fake.CountCalls("Nudge", name); got != 1 {
		t.Fatalf("Nudge calls = %d, want 1 (the pre-guard delivery is preserved)", got)
	}
	if got := fake.CountCalls("NudgeNow", name); got != 0 {
		t.Fatalf("NudgeNow calls = %d, want 0", got)
	}
}

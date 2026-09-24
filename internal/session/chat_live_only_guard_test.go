package session

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// promptsAfterFirstProbeProvider raises its approval dialog between the
// caller's own probe and the keystrokes. On a runtime whose Nudge is a wait
// followed by a send, that gap is up to the wait's whole length, and the wait
// is liable to return BECAUSE the dialog opened — so this is the ordinary case,
// not a narrow race.
type promptsAfterFirstProbeProvider struct {
	*runtime.Fake
	mu     sync.Mutex
	probes int
}

func (p *promptsAfterFirstProbeProvider) Pending(name string) (*runtime.PendingInteraction, error) {
	p.mu.Lock()
	p.probes++
	first := p.probes == 1
	p.mu.Unlock()
	if _, err := p.Fake.Pending(name); err != nil {
		return nil, err
	}
	if first {
		return nil, nil
	}
	return &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval", Prompt: "approve?"}, nil
}

func (p *promptsAfterFirstProbeProvider) probeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probes
}

func newGuardedNudgeManager(t *testing.T) (*Manager, *promptsAfterFirstProbeProvider, Info) {
	t.Helper()
	fake := runtime.NewFake()
	sp := &promptsAfterFirstProbeProvider{Fake: fake}
	mgr := NewManagerWithOptions(beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatalf("precondition: CreateSession should leave the runtime running")
	}
	return mgr, sp, info
}

// sendLiveOnly's own probe cannot cover the distance to the keystrokes: the
// delivery it calls is the runtime's Nudge, which on tmux waits up to
// NudgeIdleTimeout before sending. Routing the non-immediate arm through the
// guarded delivery is what closes that distance, and the refusal must surface
// as this package's sentinel so the mail and sling lanes keep folding it into
// their queue fallback rather than seeing a new failure mode.
func TestSendLiveOnlyRefusesPromptOpenedBeforeTheKeystrokes(t *testing.T) {
	mgr, sp, info := newGuardedNudgeManager(t)

	delivered, err := mgr.SendLiveOnly(context.Background(), info.ID, "hello")
	if !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("SendLiveOnly error = %v, want %v", err, ErrPendingInteraction)
	}
	if delivered {
		t.Fatal("SendLiveOnly reported delivery into a prompt that opened before the keystrokes")
	}
	// Two probes is the evidence: the caller's own probe passed, and the
	// delivery re-probed. One probe would mean the delivery never re-checked.
	if got := sp.probeCount(); got < 2 {
		t.Fatalf("probe count = %d, want >= 2: the delivery must re-probe after the caller's probe", got)
	}
	assertNoNudge(t, sp.Fake, info.SessionName)
}

// The immediate arm is already adjacent to its own probe — it sends without a
// wait — so it must NOT pay for a second probe. Re-probing there would be pure
// cost on the one path that does not need it.
func TestSendImmediateLiveOnlyProbesOnce(t *testing.T) {
	mgr, sp, info := newGuardedNudgeManager(t)

	delivered, err := mgr.SendImmediateLiveOnly(context.Background(), info.ID, "hello")
	if err != nil {
		t.Fatalf("SendImmediateLiveOnly error = %v, want nil", err)
	}
	if !delivered {
		t.Fatal("SendImmediateLiveOnly delivered = false, want true")
	}
	if got := sp.probeCount(); got != 1 {
		t.Fatalf("probe count = %d, want 1: the immediate path's probe is already adjacent to its send", got)
	}
}

// Send is the fourth member of the same class, reached from the HTTP API's
// background-message path and from the ordinary claude submit path, where
// usesImmediateDefaultSubmit is false. Its own probe is separated from the
// keystrokes by the runtime's whole wait exactly as sendLiveOnly's was, so it
// has to route through the same guarded delivery. Without that routing the
// fake's Nudge records a keystroke into an open approval prompt and this test
// is the only thing that says so: ensureRunning is a no-op on the live seat,
// and every other assertion in the suite stays green.
func TestSendRefusesPromptOpenedBeforeTheKeystrokes(t *testing.T) {
	mgr, sp, info := newGuardedNudgeManager(t)

	err := mgr.Send(context.Background(), info.ID, "hello", "", runtime.Config{})
	if !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("Send error = %v, want %v", err, ErrPendingInteraction)
	}
	// Two probes is the evidence: sendLocked's own probe passed, and the
	// delivery re-probed. One probe would mean the delivery never re-checked.
	if got := sp.probeCount(); got < 2 {
		t.Fatalf("probe count = %d, want >= 2: the delivery must re-probe after sendLocked's probe", got)
	}
	assertNoNudge(t, sp.Fake, info.SessionName)
}

// The immediate arm of the same entry point keeps operator semantics: its probe
// is already adjacent to its send, so SendImmediate must not pay for a second
// one. This is the control that keeps the fix above from being read as "probe
// twice everywhere".
func TestSendImmediateProbesOnce(t *testing.T) {
	mgr, sp, info := newGuardedNudgeManager(t)

	if err := mgr.SendImmediate(context.Background(), info.ID, "hello", "", runtime.Config{}); err != nil {
		t.Fatalf("SendImmediate error = %v, want nil", err)
	}
	if got := sp.probeCount(); got != 1 {
		t.Fatalf("probe count = %d, want 1: the immediate path's probe is already adjacent to its send", got)
	}
}

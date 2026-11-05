package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// SESSION-RUNTIME-024: a fenced answer reaches only the incarnation its fence
// names. Under another incarnation's fence nothing is sent; a runtime that
// cannot fence answers refuses before sending; an answer naming a request
// the runtime no longer shows is ErrInteractionMismatch.
func TestRespondFencedAnswersOnlyItsIncarnation(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	own := startedFence(t, sp, info.SessionName)
	other := own
	other.Epoch++
	answer := runtime.InteractionResponse{RequestID: "req-1", Action: "approve"}
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval"})

	if err := mgr.RespondFenced(context.Background(), info.ID, other, answer); !errors.Is(err, runtime.ErrFenceMismatch) {
		t.Fatalf("RespondFenced(other incarnation) = %v, want a fence miss", err)
	}
	if len(sp.Responses[info.SessionName]) != 0 {
		t.Fatal("a misfenced answer reached the runtime")
	}
	if err := mgr.RespondFenced(context.Background(), info.ID, runtime.InputFence{}, answer); !errors.Is(err, runtime.ErrInvalidInputFence) {
		t.Fatalf("RespondFenced(no fence) = %v, want ErrInvalidInputFence", err)
	}
	sp.InputFencingValue = runtime.InputFencingNone
	if err := mgr.RespondFenced(context.Background(), info.ID, own, answer); !errors.Is(err, runtime.ErrInputFenceUnsupported) {
		t.Fatalf("RespondFenced on a runtime that cannot fence = %v, want ErrInputFenceUnsupported", err)
	}
	sp.InputFencingValue = ""
	if err := mgr.RespondFenced(context.Background(), info.ID, own, runtime.InteractionResponse{RequestID: "req-2", Action: "approve"}); !errors.Is(err, ErrInteractionMismatch) {
		t.Fatalf("RespondFenced(another request) = %v, want ErrInteractionMismatch", err)
	}
	if err := mgr.RespondFenced(context.Background(), info.ID, own, answer); err != nil {
		t.Fatalf("RespondFenced(own incarnation) = %v", err)
	}
	if got := sp.Responses[info.SessionName]; len(got) != 1 || got[0].RequestID != "req-1" {
		t.Fatalf("runtime answers %+v, want the one fenced answer", got)
	}
	var fenced bool
	for _, c := range sp.Calls {
		if c.Method == "Respond" && c.Fence == own {
			fenced = true
		}
	}
	if !fenced {
		t.Fatal("the answer did not carry its fence to the runtime")
	}
}

// A provider answer naming a request the runtime no longer shows (the
// prompt gone or changed between the session's check and the runtime's)
// reads as ErrInteractionMismatch, not as an unexplained failure.
func TestRespondMapsAGoneInteractionToMismatch(t *testing.T) {
	store := beads.NewMemStore()
	sp := &goneRespondFake{Fake: runtime.NewFake()}
	mgr := newTestManager(t, store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval"})
	if err := mgr.Respond(info.ID, runtime.InteractionResponse{Action: "approve"}); !errors.Is(err, ErrInteractionMismatch) || !errors.Is(err, runtime.ErrInteractionGone) {
		t.Fatalf("Respond = %v, want ErrInteractionMismatch wrapping runtime.ErrInteractionGone", err)
	}
}

// goneRespondFake answers every response as the prompt having gone after
// the session's pending check.
type goneRespondFake struct{ *runtime.Fake }

func (f *goneRespondFake) Respond(string, runtime.InteractionResponse) error {
	return runtime.ErrInteractionGone
}

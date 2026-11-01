package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A fenced answer through a runtime-only handle reaches only the
// incarnation it names: a miss and an invalid fence send nothing, a runtime
// that cannot fence answers refuses, and a prompt gone reads as
// session.ErrInteractionMismatch.
func TestRuntimeHandleRespondFenced(t *testing.T) {
	sp := runtime.NewFake()
	own := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	if err := sp.Start(context.Background(), "w", runtime.Config{Env: map[string]string{
		runtime.RuntimeEpochEnv: "1", runtime.ContinuationEpochEnv: "1", runtime.InstanceTokenEnv: "tok",
	}}); err != nil {
		t.Fatal(err)
	}
	h, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "w"})
	if err != nil {
		t.Fatal(err)
	}
	sp.SetPendingInteraction("w", &runtime.PendingInteraction{RequestID: "req-1"})
	other := own
	other.Epoch = 2
	if err := h.Respond(context.Background(), InteractionResponse{RequestID: "req-1", Action: "approve", Fence: other}); !errors.Is(err, runtime.ErrFenceMismatch) {
		t.Fatalf("Respond(other incarnation) = %v, want a fence miss", err)
	}
	if err := h.Respond(context.Background(), InteractionResponse{RequestID: "req-1", Action: "approve", Fence: runtime.InputFence{Token: "tok"}}); !errors.Is(err, runtime.ErrInvalidInputFence) {
		t.Fatalf("Respond(invalid fence) = %v, want ErrInvalidInputFence", err)
	}
	sp.InputFencingValue = runtime.InputFencingNone
	if err := h.Respond(context.Background(), InteractionResponse{RequestID: "req-1", Action: "approve", Fence: own}); !errors.Is(err, runtime.ErrInputFenceUnsupported) {
		t.Fatalf("Respond on a runtime that cannot fence = %v, want ErrInputFenceUnsupported", err)
	}
	sp.InputFencingValue = ""
	if len(sp.Responses["w"]) != 0 {
		t.Fatal("a refused fenced answer reached the runtime")
	}
	if err := h.Respond(context.Background(), InteractionResponse{RequestID: "req-1", Action: "approve", Fence: own}); err != nil {
		t.Fatalf("Respond(own incarnation) = %v", err)
	}
	if got := sp.Responses["w"]; len(got) != 1 {
		t.Fatalf("runtime answers %+v, want one", got)
	}
	if err := h.Respond(context.Background(), InteractionResponse{RequestID: "req-1", Action: "approve", Fence: own}); !errors.Is(err, sessionpkg.ErrInteractionMismatch) || !errors.Is(err, runtime.ErrInteractionGone) {
		t.Fatalf("Respond to a prompt gone = %v, want session.ErrInteractionMismatch wrapping runtime.ErrInteractionGone", err)
	}
}

// A session handle routes a fenced answer through the session layer's
// fenced respond, and an unfenced one through its plain respond.
func TestSessionHandleRespondFenced(t *testing.T) {
	handle, _, sp, mgr := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
	})
	if err := handle.Start(context.Background(), handleActor(t, handle, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	own, ok := sessionpkg.InputFenceFor(info)
	if !ok {
		t.Fatal("the started session names no incarnation")
	}
	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{RequestID: "req-1", Kind: "approval"})
	other := own
	other.Epoch++
	if err := handle.Respond(context.Background(), InteractionResponse{RequestID: "req-1", Action: "approve", Fence: other}); !errors.Is(err, runtime.ErrFenceMismatch) {
		t.Fatalf("Respond(other incarnation) = %v, want a fence miss", err)
	}
	if err := handle.Respond(context.Background(), InteractionResponse{RequestID: "req-1", Action: "approve", Fence: own}); err != nil {
		t.Fatalf("Respond(own incarnation) = %v", err)
	}
	var fences []runtime.InputFence
	for _, c := range sp.Calls {
		if c.Method == "Respond" {
			fences = append(fences, c.Fence)
		}
	}
	if len(fences) != 1 || fences[0] != own {
		t.Fatalf("runtime answers carried fences %v, want exactly the own fence", fences)
	}
}

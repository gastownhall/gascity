package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type sessionResponseCapabilityHandle struct {
	state  worker.State
	output string
}

func (h sessionResponseCapabilityHandle) State(context.Context) (worker.State, error) {
	return h.state, nil
}

func (h sessionResponseCapabilityHandle) Peek(context.Context, int) (string, error) {
	return h.output, nil
}

type agentOutputCapabilityHandle struct {
	state  worker.State
	output string
}

func (h agentOutputCapabilityHandle) State(context.Context) (worker.State, error) {
	return h.state, nil
}

func (h agentOutputCapabilityHandle) LiveObservation(context.Context) (worker.LiveObservation, error) {
	return worker.LiveObservation{Running: true}, nil
}

func (h agentOutputCapabilityHandle) Peek(context.Context, int) (string, error) {
	return h.output, nil
}

func TestEnrichSessionResponseAcceptsStateAndPeekCapability(t *testing.T) {
	srv := New(newSessionFakeState(t))
	resp := &sessionResponse{}

	srv.enrichSessionResponse(resp, session.Info{
		ID:          "sess-1",
		State:       session.StateActive,
		SessionName: "sess-worker",
		Template:    "myrig/worker",
	}, nil, sessionResponseCapabilityHandle{
		state:  worker.State{Phase: worker.PhaseReady},
		output: "peek output",
	}, true, false)

	if !resp.Running {
		t.Fatal("Running = false, want true")
	}
	if got, want := resp.LastOutput, "peek output"; got != want {
		t.Fatalf("LastOutput = %q, want %q", got, want)
	}
}

func TestPeekFallbackOutputAcceptsPeekObservationCapability(t *testing.T) {
	srv := New(newSessionFakeState(t))
	rec := httptest.NewRecorder()

	srv.peekFallbackOutput(context.Background(), rec, "worker", agentOutputCapabilityHandle{
		state:  worker.State{Phase: worker.PhaseReady},
		output: "hello from peek",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello from peek") {
		t.Fatalf("body = %q, want peek output", rec.Body.String())
	}
}

var _ interface {
	worker.StateHandle
	worker.PeekHandle
} = sessionResponseCapabilityHandle{}

var _ interface {
	worker.LiveObservationHandle
	worker.StateHandle
	worker.PeekHandle
} = agentOutputCapabilityHandle{}

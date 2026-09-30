package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/session"
)

// commandableWaiterState is a fakeState whose session creates are deferred to a
// controller, the way controllerState defers them in production: the create
// handler writes the bead and waits for the reconciler to start it.
type commandableWaiterState struct {
	*fakeState
}

func (s *commandableWaiterState) WaitForSessionCommandable(_ context.Context, id string) (session.Info, error) {
	return session.Info{ID: id, State: session.StateActive}, nil
}

// assertNoSessionBeads fails if the create left any session bead behind: a
// refused create must not strand a start-pending bead the controller will
// never start (#6858).
func assertNoSessionBeads(t *testing.T, fs *fakeState) {
	t.Helper()
	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(%q): %v", session.LabelSession, err)
	}
	if len(all) != 0 {
		t.Fatalf("session beads after refused create = %d (%+v), want 0", len(all), all)
	}
}

func assertDemandOnlyRefusalMessage(t *testing.T, msg string) {
	t.Helper()
	for _, want := range []string{`"myrig/worker"`, "max_active_sessions = 1", "sling work", "[[named_session]]"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal message = %q, want substring %q", msg, want)
		}
	}
}

// #6858: with a controller owning session starts, an API create for a pool
// agent with max_active_sessions = 1 and no [[named_session]] used to return
// 202 and leave a start-pending bead the reconciler never starts. It must be
// refused up front instead.
func TestHumaHandleSessionCreateRefusesDemandOnlySingletonAgent(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].MinActiveSessions = intPtr(0)
	fs.cfg.NamedSessions = nil // the shared fixture backs myrig/worker with a named session
	srv := New(&commandableWaiterState{fakeState: fs})

	out, err := srv.humaHandleSessionCreate(context.Background(), &SessionCreateInput{
		Body: sessionCreateBody{Kind: "agent", Name: "myrig/worker"},
	})
	if err == nil {
		t.Fatalf("humaHandleSessionCreate() = %+v, nil error; want a refusal for a demand-only singleton agent", out)
	}
	var se huma.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("humaHandleSessionCreate() error = %T %v, want huma.StatusError", err, err)
	}
	if se.GetStatus() != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; err = %v", se.GetStatus(), http.StatusBadRequest, err)
	}
	assertDemandOnlyRefusalMessage(t, err.Error())
	assertNoSessionBeads(t, fs)
}

// The refusal is scoped to the demand-only singleton shape: a multi-session
// template still accepts a controller-deferred API create.
func TestHumaHandleSessionCreateAcceptsMultiSessionAgentWithController(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].MaxActiveSessions = nil
	srv := New(&commandableWaiterState{fakeState: fs})

	out, err := srv.humaHandleSessionCreate(context.Background(), &SessionCreateInput{
		Body: sessionCreateBody{Kind: "agent", Name: "myrig/worker"},
	})
	if err != nil {
		t.Fatalf("humaHandleSessionCreate(multi-session agent): %v", err)
	}
	if out.Status != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", out.Status, http.StatusAccepted)
	}
	success, failure := waitForSessionCreateResult(t, fs.eventProv, out.Body.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
}

// The compatibility REST route always defers agent creates to the reconciler,
// so it refuses the same shape with the same explanation.
func TestHandleSessionCreateRefusesDemandOnlySingletonAgent(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].MinActiveSessions = intPtr(0)
	fs.cfg.NamedSessions = nil // the shared fixture backs myrig/worker with a named session
	srv := New(fs)

	req := newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var problem problemDetails
	if err := json.NewDecoder(rec.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	assertDemandOnlyRefusalMessage(t, problem.Detail)
	assertNoSessionBeads(t, fs)
}

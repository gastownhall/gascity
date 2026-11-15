package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
)

// activeWorkStepFakeState is a city State that answers the active work step:
// it records what it was asked and answers with a fixed step or error.
type activeWorkStepFakeState struct {
	*fakeState
	step   *ActiveWorkStep
	err    error
	asked  [][]string
	scopes []string
}

func (s *activeWorkStepFakeState) ActiveWorkStep(_ context.Context, assignees []string, scopeRoot string) (*ActiveWorkStep, error) {
	s.asked = append(s.asked, slices.Clone(assignees))
	s.scopes = append(s.scopes, scopeRoot)
	return s.step, s.err
}

// TestActiveWorkStepRoundTripsThroughTheWire pins the client's half of "the
// bead the in-process read names": every field of the step survives the
// server's encode and the client's decode, the identities arrive in the order
// sent (a value may carry a comma), and the scope arrives as sent. The step is
// checked by reflection to set every field, so a field added without its
// conversion fails here.
func TestActiveWorkStepRoundTripsThroughTheWire(t *testing.T) {
	want := ActiveWorkStep{ID: "gc-7", Title: "inspect, then report", Description: "inspect the queue\nand report"}
	v := reflect.ValueOf(want)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Fatalf("fixture leaves ActiveWorkStep.%s unset", v.Type().Field(i).Name)
		}
	}
	state := &activeWorkStepFakeState{fakeState: newFakeState(t), step: &want}
	assignees := []string{"alias,with-comma", "session-name", "gcs-1"}
	got, err := workReadyServer(t, state).ActiveWorkStep(context.Background(), assignees, "/city/rigs/alpha")
	if err != nil {
		t.Fatalf("ActiveWorkStep: %v", err)
	}
	if got == nil || *got != want {
		t.Fatalf("ActiveWorkStep = %+v, want %+v", got, want)
	}
	if len(state.asked) != 1 || !reflect.DeepEqual(state.asked[0], assignees) {
		t.Fatalf("controller was asked %v, want %v in that order", state.asked, assignees)
	}
	if len(state.scopes) != 1 || state.scopes[0] != "/city/rigs/alpha" {
		t.Fatalf("controller was asked scopes %v, want the one sent", state.scopes)
	}
}

// TestActiveWorkStepAnswersNoStep pins "no current step": the step is absent
// on the wire and the client answers nil with no error, never a zero step.
func TestActiveWorkStepAnswersNoStep(t *testing.T) {
	state := &activeWorkStepFakeState{fakeState: newFakeState(t)}
	got, err := workReadyServer(t, state).ActiveWorkStep(context.Background(), []string{"gcs-1"}, "")
	if err != nil || got != nil {
		t.Fatalf("ActiveWorkStep = %+v, %v; want no step and no error", got, err)
	}
	if len(state.scopes) != 1 || state.scopes[0] != "" {
		t.Fatalf("controller was asked scopes %v, want the city (empty)", state.scopes)
	}
	sm := NewSupervisorMux(&singleStateResolver{state: state}, nil, false, "test", "", time.Now())
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/city/"+state.CityName()+"/work/active-step?assignee=gcs-1", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"step"`) {
		t.Fatalf("GET /work/active-step = %d %s, want 200 with no step", rec.Code, rec.Body.String())
	}
}

// TestWorkActiveStepStatuses pins the failure contract the hook's fallback
// rests on: the status, and for a 503 the problem code that tells a failed read
// (store-unavailable) from a controller that cannot vouch for its answer
// (service-unavailable).
func TestWorkActiveStepStatuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state State
		query string
		want  int
		code  string
	}{
		{name: "no assignee is rejected", state: &activeWorkStepFakeState{fakeState: newFakeState(t)}, query: "", want: http.StatusUnprocessableEntity},
		{name: "a blank assignee is 400", state: &activeWorkStepFakeState{fakeState: newFakeState(t)}, query: "?assignee=gcs-1&assignee=%20", want: http.StatusBadRequest},
		// Each identity is a concurrent listing on the controller; a session has
		// three at most.
		{name: "more than eight assignees are rejected", state: &activeWorkStepFakeState{fakeState: newFakeState(t)}, query: "?assignee=a&assignee=b&assignee=c&assignee=d&assignee=e&assignee=f&assignee=g&assignee=h&assignee=i", want: http.StatusUnprocessableEntity},
		{name: "unknown query parameter is rejected", state: &activeWorkStepFakeState{fakeState: newFakeState(t)}, query: "?assignee=gcs-1&bogus=1", want: http.StatusUnprocessableEntity},
		{name: "a state that cannot answer is 503", state: newFakeState(t), query: "?assignee=gcs-1", want: http.StatusServiceUnavailable, code: apierr.ServiceUnavailable.Code},
		{name: "a failed read is 503 store-unavailable", state: &activeWorkStepFakeState{fakeState: newFakeState(t), err: errors.New(`rig "alpha" store: unreachable`)}, query: "?assignee=gcs-1", want: http.StatusServiceUnavailable, code: apierr.StoreUnavailable.Code},
		{name: "a config that is not current is 503 service-unavailable", state: &activeWorkStepFakeState{fakeState: newFakeState(t), err: fmt.Errorf("refusing: %w", ErrReadyWorkConfigNotCurrent)}, query: "?assignee=gcs-1", want: http.StatusServiceUnavailable, code: apierr.ServiceUnavailable.Code},
		{name: "a scope the controller does not hold is 503 service-unavailable", state: &activeWorkStepFakeState{fakeState: newFakeState(t), err: fmt.Errorf("%w: %q", ErrActiveWorkStepScopeNotHeld, "/elsewhere")}, query: "?assignee=gcs-1&scope_root=%2Felsewhere", want: http.StatusServiceUnavailable, code: apierr.ServiceUnavailable.Code},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := NewSupervisorMux(&singleStateResolver{state: tc.state}, nil, false, "test", "", time.Now())
			rec := httptest.NewRecorder()
			sm.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/city/"+tc.state.CityName()+"/work/active-step"+tc.query, nil))
			if rec.Code != tc.want {
				t.Fatalf("GET /work/active-step%s = %d, want %d (body %s)", tc.query, rec.Code, tc.want, rec.Body.String())
			}
			if tc.code == "" {
				return
			}
			var problem struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode problem %s: %v", rec.Body.String(), err)
			}
			if problem.Code != tc.code {
				t.Fatalf("GET /work/active-step%s problem code = %q, want %q (body %s)", tc.query, problem.Code, tc.code, rec.Body.String())
			}
		})
	}
}

// TestActiveWorkStepClientReportsAFailedReadAsAnError pins that a 503 reaches
// the hook as an error, its cue to read in process; it must never decode into
// "no current step".
func TestActiveWorkStepClientReportsAFailedReadAsAnError(t *testing.T) {
	state := &activeWorkStepFakeState{fakeState: newFakeState(t), err: errors.New(`rig "alpha" store: unreachable`)}
	step, err := workReadyServer(t, state).ActiveWorkStep(context.Background(), []string{"gcs-1"}, "")
	if err == nil {
		t.Fatalf("ActiveWorkStep = %+v and no error over a failed read", step)
	}
	if !strings.Contains(err.Error(), `rig "alpha" store: unreachable`) {
		t.Fatalf("error = %v, want the failing store named", err)
	}
}

// TestActiveWorkStepClientRefusesNoAssignee pins the client's door: a request
// with no identity is refused before it is sent.
func TestActiveWorkStepClientRefusesNoAssignee(t *testing.T) {
	state := &activeWorkStepFakeState{fakeState: newFakeState(t)}
	if _, err := workReadyServer(t, state).ActiveWorkStep(context.Background(), nil, ""); err == nil {
		t.Fatal("ActiveWorkStep with no assignee = no error, want a refusal")
	}
	if len(state.asked) != 0 {
		t.Fatalf("controller was asked %v, want the query refused before it is sent", state.asked)
	}
}

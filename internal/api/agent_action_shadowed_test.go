package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/configedit"
)

// shadowedMutatorState refuses every agent suspension edit the way the
// editor does when another config layer decides the agent's suspension.
type shadowedMutatorState struct {
	*fakeMutatorState
}

func (s *shadowedMutatorState) SuspendAgent(name string) error {
	return fmt.Errorf("%w: agent %q: a later layer overrides it", configedit.ErrShadowed, name)
}

func (s *shadowedMutatorState) ResumeAgent(name string) error {
	return fmt.Errorf("%w: agent %q: a later layer overrides it", configedit.ErrShadowed, name)
}

func TestMutationErrorMapsShadowedToConflict(t *testing.T) {
	wrapped := fmt.Errorf("resuming: %w", fmt.Errorf("%w: agent %q held by fragment.toml", configedit.ErrShadowed, "myrig/worker"))
	var se huma.StatusError
	if !errors.As(mutationError(wrapped), &se) {
		t.Fatalf("mutationError(%v) is not a huma.StatusError", wrapped)
	}
	if se.GetStatus() != http.StatusConflict {
		t.Fatalf("status = %d, want %d", se.GetStatus(), http.StatusConflict)
	}
	if !strings.Contains(se.Error(), "held by fragment.toml") {
		t.Fatalf("conflict %q dropped the diagnostic", se.Error())
	}
}

func TestAgentActionShadowedReturnsConflict(t *testing.T) {
	state := &shadowedMutatorState{fakeMutatorState: newFakeMutatorState(t)}
	s := &Server{state: state}
	for _, action := range []string{"suspend", "resume"} {
		resp, err := s.agentActionByName("myrig/worker", action)
		if resp != nil {
			t.Fatalf("%s: got success response %+v for a shadowed edit", action, resp)
		}
		var se huma.StatusError
		if !errors.As(err, &se) || se.GetStatus() != http.StatusConflict {
			t.Fatalf("%s: err = %v, want HTTP 409", action, err)
		}
		if !strings.Contains(se.Error(), "myrig/worker") {
			t.Fatalf("%s: conflict %q does not name the agent", action, se.Error())
		}
	}
	if len(state.suspended) != 0 {
		t.Fatalf("shadowed edit recorded a suspension: %v", state.suspended)
	}
}

package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// Regression for ga-b2lpo6: session.Info blanks State on a closed bead and
// carries closure in Info.Closed, so a closed session read back with
// state "" and nothing on the wire said it was closed.
func TestSessionResponseReportsClosedState(t *testing.T) {
	closed := sessionResponseWithReason(session.Info{ID: "s-1", Closed: true}, session.PersistedResponse{Status: "closed"}, nil, nil, false)
	if closed.State != "closed" {
		t.Fatalf("closed session state = %q, want %q", closed.State, "closed")
	}
	open := sessionToResponse(session.Info{ID: "s-2", State: session.StateAsleep}, nil)
	if open.State != string(session.StateAsleep) {
		t.Fatalf("open session state = %q, want %q", open.State, session.StateAsleep)
	}
}

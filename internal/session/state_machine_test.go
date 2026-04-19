package session

import (
	"slices"
	"testing"
)

// TestTransitionLegalMoves enumerates every (state, command) pair that is
// allowed by the state machine and verifies each produces the expected
// resulting state. If someone adds a new command or changes the table,
// they must update this test — that's the point: the legal transitions
// are a contract, not a convention.
func TestTransitionLegalMoves(t *testing.T) {
	tests := []struct {
		from State
		cmd  TransitionCommand
		to   State
	}{
		{StateNone, CmdCreate, StateCreating},
		{StateCreating, CmdReady, StateActive},

		{StateActive, CmdSuspend, StateSuspended},
		{StateAsleep, CmdSuspend, StateSuspended},
		{StateQuarantined, CmdSuspend, StateSuspended},

		{StateAsleep, CmdWake, StateActive},
		{StateSuspended, CmdWake, StateActive},
		{StateQuarantined, CmdWake, StateActive},
		{StateArchived, CmdWake, StateActive},

		{StateActive, CmdSleep, StateAsleep},

		{StateActive, CmdQuarantine, StateQuarantined},
		{StateAsleep, CmdQuarantine, StateQuarantined},

		{StateActive, CmdDrain, StateDraining},
		{StateActive, CmdArchive, StateArchived},
		{StateAsleep, CmdArchive, StateArchived},
		{StateSuspended, CmdArchive, StateArchived},
		{StateQuarantined, CmdArchive, StateArchived},
		{StateDraining, CmdArchive, StateArchived},

		// Close is legal from any state.
		{StateActive, CmdClose, StateClosed},
		{StateAsleep, CmdClose, StateClosed},
		{StateSuspended, CmdClose, StateClosed},
		{StateDraining, CmdClose, StateClosed},
		{StateCreating, CmdClose, StateClosed},
		{StateArchived, CmdClose, StateClosed},
		{StateQuarantined, CmdClose, StateClosed},
	}

	for _, tt := range tests {
		got, err := Transition(tt.from, tt.cmd)
		if err != nil {
			t.Errorf("Transition(%q, %q) returned unexpected error: %v", tt.from, tt.cmd, err)
			continue
		}
		if got != tt.to {
			t.Errorf("Transition(%q, %q) = %q, want %q", tt.from, tt.cmd, got, tt.to)
		}
	}
}

// TestTransitionIllegalMoves verifies that common "wrong" transitions fail,
// acting as guardrails against future manager changes that would silently
// break state semantics.
func TestTransitionIllegalMoves(t *testing.T) {
	cases := []struct {
		from State
		cmd  TransitionCommand
	}{
		// Ready only valid from Creating.
		{StateActive, CmdReady},
		{StateAsleep, CmdReady},
		{StateSuspended, CmdReady},
		// Sleep only valid from Active.
		{StateAsleep, CmdSleep},
		{StateSuspended, CmdSleep},
		{StateClosed, CmdSleep},
		// Wake not valid from Active.
		{StateActive, CmdWake},
		// Drain not valid from Asleep/Suspended.
		{StateAsleep, CmdDrain},
		{StateSuspended, CmdDrain},
	}

	for _, tt := range cases {
		_, err := Transition(tt.from, tt.cmd)
		if err == nil {
			t.Errorf("Transition(%q, %q) should be illegal but was allowed", tt.from, tt.cmd)
		}
	}
}

// TestTransitionUnknownCommand verifies that made-up commands are rejected.
func TestTransitionUnknownCommand(t *testing.T) {
	_, err := Transition(StateActive, TransitionCommand("bogus"))
	if err == nil {
		t.Error("Transition with unknown command should return error")
	}
}

// TestAllowedCommandsActiveSession spot-checks AllowedCommands for a
// common state to guarantee the affordance query is working.
func TestAllowedCommandsActiveSession(t *testing.T) {
	got := AllowedCommands(StateActive)
	want := []TransitionCommand{CmdArchive, CmdClose, CmdDrain, CmdQuarantine, CmdSleep, CmdSuspend}
	if !slices.Equal(got, want) {
		t.Errorf("AllowedCommands(StateActive) = %v, want %v", got, want)
	}
}

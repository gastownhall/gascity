package session

import (
	"fmt"
	"sort"
)

// StateMachine documents the valid session state transitions as a table.
//
// Session state today is managed ad-hoc across many manager methods
// (Create, Suspend, Wake, Close, StopTurn, Kill, etc.). Each method encodes
// its own transition logic. This file is the first step toward a single
// explicit reducer: it lists the allowed transitions in one place so code
// reviews can catch illegal transitions and new handlers can check legality
// without reading the entire manager.
//
// TransitionCommand describes what triggered a state change. Naming follows
// the verb the API or reconciler invoked, not the resulting state. This is
// the language the handlers and reconciler already use, so the vocabulary
// stays consistent.
type TransitionCommand string

const (
	// CmdCreate: a new session bead is written.
	// Transitions: (nil) → StateCreating → StateActive.
	CmdCreate TransitionCommand = "create"

	// CmdReady: reconciler confirms the runtime process is alive.
	// Transitions: StateCreating → StateActive.
	CmdReady TransitionCommand = "ready"

	// CmdSuspend: operator explicitly pauses the session.
	// Transitions: StateActive, StateAsleep, StateQuarantined → StateSuspended.
	CmdSuspend TransitionCommand = "suspend"

	// CmdWake: operator explicitly resumes a paused/asleep/quarantined session.
	// Transitions: StateAsleep, StateSuspended, StateQuarantined → StateActive.
	CmdWake TransitionCommand = "wake"

	// CmdSleep: reconciler observes the runtime process exited normally.
	// Transitions: StateActive → StateAsleep.
	CmdSleep TransitionCommand = "sleep"

	// CmdQuarantine: crash-loop threshold exceeded; block waking.
	// Transitions: StateActive, StateAsleep → StateQuarantined.
	CmdQuarantine TransitionCommand = "quarantine"

	// CmdDrain: begin graceful shutdown (complete in-flight work).
	// Transitions: StateActive → StateDraining.
	CmdDrain TransitionCommand = "drain"

	// CmdArchive: drain completed; session retained for history.
	// Transitions: StateDraining → StateArchived.
	CmdArchive TransitionCommand = "archive"

	// CmdClose: hard close (no in-flight work to drain).
	// Transitions: any non-closed state → StateClosed.
	CmdClose TransitionCommand = "close"
)

// StateClosed is the terminal state for closed sessions. The bead's Status
// field is "closed" regardless of its prior state field. Adding it here as
// a named value keeps the state machine vocabulary complete.
const StateClosed State = "closed"

// StateNone is the virtual state before a session is created. Used as the
// source state for CmdCreate — transitions from StateNone can only go to
// StateCreating (via CmdCreate) and nothing else.
const StateNone State = ""

// anyState is a sentinel used in the transitions table to mean "any non-none
// state accepts this command." Currently only CmdClose uses it.
const anyState State = "*"

// transitions is the allowed (command, from-state) → to-state table.
var transitions = map[TransitionCommand]map[State]State{
	CmdCreate: {
		StateNone: StateCreating,
	},
	CmdReady: {
		StateCreating: StateActive,
	},
	CmdSuspend: {
		StateActive:      StateSuspended,
		StateAsleep:      StateSuspended,
		StateQuarantined: StateSuspended,
	},
	CmdWake: {
		StateAsleep:      StateActive,
		StateSuspended:   StateActive,
		StateQuarantined: StateActive,
	},
	CmdSleep: {
		StateActive: StateAsleep,
	},
	CmdQuarantine: {
		StateActive: StateQuarantined,
		StateAsleep: StateQuarantined,
	},
	CmdDrain: {
		StateActive: StateDraining,
	},
	CmdArchive: {
		StateDraining: StateArchived,
	},
	CmdClose: {
		anyState: StateClosed, // any non-none state can close
	},
}

// Transition validates that applying cmd to a session currently in state from
// is a legal transition, and returns the new state. Returns an error naming
// the illegal transition if not allowed.
//
// This is NOT yet called by the manager — handlers still mutate state
// directly. The goal of this first step is to document and test the allowed
// transitions so future refactors can wire the manager through Transition()
// with confidence that no legal behavior is lost.
func Transition(from State, cmd TransitionCommand) (State, error) {
	table, ok := transitions[cmd]
	if !ok {
		return "", fmt.Errorf("unknown command %q", cmd)
	}
	if to, ok := table[from]; ok {
		return to, nil
	}
	// anyState matches any non-none state (close is the only such command).
	if from != StateNone {
		if to, ok := table[anyState]; ok {
			return to, nil
		}
	}
	return "", fmt.Errorf("illegal transition: state %q does not accept command %q", from, cmd)
}

// AllowedCommands returns the set of commands legal from the given state,
// useful for rendering UI affordances ("what can I do to this session?").
func AllowedCommands(from State) []TransitionCommand {
	var out []TransitionCommand
	for cmd, table := range transitions {
		if _, ok := table[from]; ok {
			out = append(out, cmd)
			continue
		}
		if from != StateNone {
			if _, ok := table[anyState]; ok {
				out = append(out, cmd)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

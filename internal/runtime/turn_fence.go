package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TurnState is one read of a session's agent turn.
type TurnState struct {
	// TurnID identifies the turn in progress; empty when none is. It is
	// opaque and compared for equality only. A runtime keeps it unique per
	// session name across the threads or processes that host the session,
	// and uses the TurnEventProvider identity when it implements both.
	TurnID string
}

// Active reports whether a turn is in progress.
func (s TurnState) Active() bool { return s.TurnID != "" }

// TurnInterrupt is a runtime's answer to TurnFenceProvider.InterruptTurn.
type TurnInterrupt struct {
	// Fenced reports that the runtime applies the interrupt only to the named
	// turn. False means it interrupts whatever turn runs when it handles the
	// request, so a turn that started after the caller's read can be hit.
	Fenced bool
}

// ErrTurnNotCurrent reports that InterruptTurn read another turn, or none,
// in progress and sent nothing. Runtimes return a *TurnNotCurrentError.
var ErrTurnNotCurrent = errors.New("turn is no longer the session's current turn")

// TurnNotCurrentError is ErrTurnNotCurrent with what the runtime read.
type TurnNotCurrentError struct {
	Session string
	TurnID  string    // the turn the caller named
	Current TurnState // the turn the runtime read instead
}

// Error names the session, the turn the caller named and the turn read.
func (e *TurnNotCurrentError) Error() string {
	current := "no turn is in progress"
	if e.Current.Active() {
		current = fmt.Sprintf("turn %q is in progress", e.Current.TurnID)
	}
	return fmt.Sprintf("session %q: turn %q is no longer the current turn: %s", e.Session, e.TurnID, current)
}

// Is matches ErrTurnNotCurrent.
func (e *TurnNotCurrentError) Is(target error) bool { return target == ErrTurnNotCurrent }

// ErrTurnUnnamed reports that InterruptTurn or WaitTurnEnded was called with
// an empty turnID. The runtime refuses it before reading the turn and sends
// nothing: an empty id names no turn, so neither a fence nor a wait applies.
var ErrTurnUnnamed = errors.New("turn id names no turn")

// ErrTurnNotEnded reports that WaitTurnEnded's timeout elapsed while the turn
// was still in progress, or before any read succeeded (then it also wraps the
// last read error).
var ErrTurnNotEnded = errors.New("turn did not end within the wait")

// TurnFenceProvider is an optional extension for runtimes that interrupt
// turns by protocol (TurnInterruptProtocol) and can report which turn is in
// progress. A composite answers ErrInteractionUnsupported for a session
// whose backend lacks it. It is meant for the session layer to consult, and
// only for sessions in protocol mode. A runtime implementing it returns
// ErrRuntimeUnavailable from Nudge, NudgeKeyed or NudgeNow only when nothing
// was sent: deliver-now queues a message whose send failed with it, and one
// that had already left would be delivered twice.
type TurnFenceProvider interface {
	// ObserveTurn reads name's turn from the runtime, never from a cache
	// older than the call. ErrSessionNotFound: no session named name.
	// ErrRuntimeUnavailable: the runtime cannot tell which turn, if any, runs.
	ObserveTurn(ctx context.Context, name string) (TurnState, error)
	// InterruptTurn reads as ObserveTurn does and, only when the turn in
	// progress is turnID, asks the runtime to interrupt it. Otherwise it
	// returns a *TurnNotCurrentError and sends nothing. turnID must name a
	// turn: a TurnID an earlier read reported Active. An empty turnID returns
	// ErrTurnUnnamed before any read and sends nothing. It returns once the
	// runtime accepted the request, without waiting for the turn to end. Any
	// other error leaves unknown whether the request reached the runtime.
	InterruptTurn(ctx context.Context, name, turnID string) (TurnInterrupt, error)
	// WaitTurnEnded returns the first read in which turnID is no longer in
	// progress; another turn may be. A session the runtime no longer hosts
	// has ended its turn. When timeout elapses first it returns
	// ErrTurnNotEnded with the last state read; when ctx ends first, ctx's
	// error. A read showing the runtime cannot answer the wait at all
	// (an error wrapping ErrRuntimeUnavailable, such as
	// ErrTurnFenceUnavailable) may end it before timeout. timeout is a hard
	// upper bound. An empty turnID returns ErrTurnUnnamed before any read.
	WaitTurnEnded(ctx context.Context, name, turnID string, timeout time.Duration) (TurnState, error)
}

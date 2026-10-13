package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// DefaultCancelTimeout is how long Interrupt waits for session/cancel to
// settle a turn when Config.CancelTimeout is unset.
// config.DefaultACPCancelTimeout, the documented [session.acp]
// cancel_timeout default, mirrors it.
const DefaultCancelTimeout = 10 * time.Second

// interruptSIGINTAttempts is how many times Interrupt signals the agent's
// process group with SIGINT when session/cancel has not settled the turn.
const interruptSIGINTAttempts = 2

// interruptSIGINTWait is how long Interrupt waits for the turn to settle
// after each fallback SIGINT. A variable so tests can shorten it.
var interruptSIGINTWait = time.Second

// cancelTimeout returns the session/cancel settle bound.
func (c *Config) cancelTimeout() time.Duration {
	if c.CancelTimeout <= 0 {
		return DefaultCancelTimeout
	}
	return c.CancelTimeout
}

// interrupt cancels the in-flight prompt turn with the ACP prompt-turn
// cancellation protocol and waits a bounded time for it to settle:
//
//  1. Outstanding permission requests are answered canceled, then a
//     session/cancel notification is sent, in that order, unless the turn
//     has already ended by then (see cancelTurn).
//  2. Interrupt waits up to cancelTimeout for the turn's prompt response.
//  3. While the turn is still open, the process group gets SIGINT (see
//     signalTurn), up to interruptSIGINTAttempts times, each followed by
//     interruptSIGINTWait.
//  4. If it is still open after that, the result wraps
//     runtime.ErrInterruptNotSettled and the agent keeps running.
//
// With no turn in flight there is nothing to cancel: interrupt sends nothing
// and returns nil. An agent that exits while interrupt waits counts as
// settled. interrupt never sends SIGTERM or SIGKILL; ending the session is
// an explicit Stop. It must not be called with Provider.mu held.
func (sc *sessionConn) interrupt(name string, cancelTimeout time.Duration) error {
	if sc.cmd == nil {
		// Startup sentinel: no agent process yet, so no turn to cancel.
		return nil
	}
	sc.mu.Lock()
	sc.ensureIdleChannelLocked()
	busy := sc.activePromptID != 0
	// This channel closes when this turn ends, and Nudge starts the next turn
	// only after it has closed. The waits below therefore observe only this
	// turn; cancelTurn relies on it to keep session/cancel off the next one.
	idleCh := sc.idleCh
	sessionID := sc.sessionID
	sc.mu.Unlock()

	replies := sc.takeCanceledPermissionReplies()
	if !busy {
		if len(replies) > 0 {
			go sc.writePermissionReplies(replies)
		}
		return nil
	}

	// A goroutine writes the replies and the cancel, so the wait below stays
	// bounded even if the agent has stopped reading its stdin.
	go sc.cancelTurn(name, sessionID, idleCh, replies)

	if sc.waitTurnSettled(name, idleCh, cancelTimeout) {
		return nil
	}
	for attempt := 0; attempt < interruptSIGINTAttempts; attempt++ {
		if !sc.signalTurn(idleCh) {
			return nil
		}
		if sc.waitTurnSettled(name, idleCh, interruptSIGINTWait) {
			return nil
		}
	}
	return fmt.Errorf("%w: ACP session %q still in its turn %s after session/cancel and %s after each of %d SIGINTs",
		runtime.ErrInterruptNotSettled, name, cancelTimeout, interruptSIGINTWait, interruptSIGINTAttempts)
}

// cancelTurn writes the canceled permission replies and then session/cancel
// for the turn behind idleCh, in the order ACP requires. The cancel is
// written only while that turn is still open, and that check is made under
// stdinMu, together with the write. Nudge writes the next turn's prompt
// through the same lock, and only after idleCh has closed. So session/cancel
// either reaches the agent ahead of the next prompt or is not sent at all; it
// can never follow that prompt and cancel the next turn instead.
func (sc *sessionConn) cancelTurn(name, sessionID string, idleCh <-chan struct{}, replies [][]byte) {
	if !sc.writePermissionReplies(replies) {
		return
	}
	if sc.beforeTurnCancel != nil {
		sc.beforeTurnCancel()
	}
	data, err := json.Marshal(newSessionCancelNotification(sessionID))
	if err == nil {
		err = sc.writeMessageUnlessClosed(idleCh, data)
	}
	if err != nil && !isPipeWriteError(err) {
		fmt.Fprintf(os.Stderr, "acp: sending session/cancel to %q: %v\n", name, err)
	}
}

// signalTurn sends SIGINT to the agent's process group for the turn behind
// idleCh and reports whether it did. Once that turn has ended it sends
// nothing: the turn can end after a wait has given up on it, and Nudge can
// then start the next one, which the SIGINT would interrupt instead. Signal
// delivery cannot be made atomic with that check, so it narrows the window
// rather than closing it.
func (sc *sessionConn) signalTurn(idleCh <-chan struct{}) bool {
	if sc.beforeTurnSignal != nil {
		sc.beforeTurnSignal()
	}
	select {
	case <-idleCh:
		return false
	default:
	}
	// A failure means the group is already gone; the wait observes that.
	_ = runtime.SignalProcessGroup(sc.cmd, syscall.SIGINT)
	return true
}

// waitTurnSettled waits up to timeout for the turn behind idleCh to end or
// the agent to exit, and reports whether either happened. An exit it sees is
// logged to stderr so a flapping agent is visible. That log is best effort:
// an exit ends the turn through drainPending, which closes idleCh before
// readDone, so the wait usually sees only the turn ending.
func (sc *sessionConn) waitTurnSettled(name string, idleCh <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idleCh:
		// Process exit drains the connection, which also closes idleCh.
		select {
		case <-sc.readDone:
			fmt.Fprintf(os.Stderr, "acp: interrupt of %q: agent exited before its turn ended\n", name)
		default:
		}
		return true
	case <-sc.done:
		fmt.Fprintf(os.Stderr, "acp: interrupt of %q: agent exited before its turn ended\n", name)
		return true
	case <-timer.C:
		return false
	}
}

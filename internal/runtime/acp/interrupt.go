package acp

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// defaultCancelTimeout is how long Interrupt waits for session/cancel to
// settle a turn when Config.CancelTimeout is unset.
const defaultCancelTimeout = 10 * time.Second

// interruptSIGINTAttempts is how many times Interrupt signals the agent's
// process group with SIGINT when session/cancel has not settled the turn.
const interruptSIGINTAttempts = 2

// interruptSIGINTWait is how long Interrupt waits for the turn to settle
// after each fallback SIGINT. A variable so tests can shorten it.
var interruptSIGINTWait = time.Second

// cancelTimeout returns the session/cancel settle bound.
func (c *Config) cancelTimeout() time.Duration {
	if c.CancelTimeout <= 0 {
		return defaultCancelTimeout
	}
	return c.CancelTimeout
}

// interrupt cancels the in-flight prompt turn with the ACP prompt-turn
// cancellation protocol and waits a bounded time for it to settle:
//
//  1. Outstanding permission requests are answered canceled, then a
//     session/cancel notification is sent, in that order.
//  2. Interrupt waits up to cancelTimeout for the turn's prompt response.
//  3. If the turn is still open, the process group gets SIGINT, up to
//     interruptSIGINTAttempts times, each followed by interruptSIGINTWait.
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
	// This channel closes when this turn ends. Nudge cannot start another
	// turn before it closes, so the waits below observe only this turn.
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

	// One goroutine writes the permission replies and then session/cancel,
	// preserving the order ACP requires, while the wait below stays bounded
	// even if the agent has stopped reading its stdin.
	go func() {
		if !sc.writePermissionReplies(replies) {
			return
		}
		select {
		case <-idleCh:
			// The turn already ended; a late cancel could hit the next one.
			return
		default:
		}
		if err := sc.sendNotification(newSessionCancelNotification(sessionID)); err != nil && !isPipeWriteError(err) {
			fmt.Fprintf(os.Stderr, "acp: sending session/cancel to %q: %v\n", name, err)
		}
	}()

	if sc.waitTurnSettled(name, idleCh, cancelTimeout) {
		return nil
	}
	for attempt := 0; attempt < interruptSIGINTAttempts; attempt++ {
		// A failure means the group is already gone; the wait observes that.
		_ = runtime.SignalProcessGroup(sc.cmd, syscall.SIGINT)
		if sc.waitTurnSettled(name, idleCh, interruptSIGINTWait) {
			return nil
		}
	}
	return fmt.Errorf("%w: ACP session %q still in its turn %s after session/cancel and %s after each of %d SIGINTs",
		runtime.ErrInterruptNotSettled, name, cancelTimeout, interruptSIGINTWait, interruptSIGINTAttempts)
}

// waitTurnSettled waits up to timeout for the turn behind idleCh to end or
// the agent to exit, and reports whether either happened. An exit is logged
// once to stderr so a flapping agent is visible.
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

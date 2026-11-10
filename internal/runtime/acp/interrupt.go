package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
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

// interruptCancelWriteWait bounds how long a fenced interrupt that finds its
// agent exited waits for a session/cancel write still in progress to finish,
// to learn whether it reached the agent. A variable so tests can shorten it.
var interruptCancelWriteWait = time.Second

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
	_, err := sc.interruptReporting(name, cancelTimeout)
	return err
}

// interruptReporting is interrupt that also reports what reached the agent:
// session/cancel written to its stdin or a SIGINT delivered to its process
// group. Canceled permission replies do not count. It does not wait for a
// session/cancel write still in progress, so it stays bounded when the agent
// has stopped reading its stdin; interruptDelivery.reached does that wait,
// with its own bound, for a caller that needs the answer.
func (sc *sessionConn) interruptReporting(name string, cancelTimeout time.Duration) (interruptDelivery, error) {
	if sc.cmd == nil {
		// Startup sentinel: no agent process yet, so no turn to cancel.
		return interruptDelivery{}, nil
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
		return interruptDelivery{}, nil
	}

	d := interruptDelivery{sent: new(atomic.Bool)}
	cancelDone, err := sc.cancelAndSettle(name, sessionID, idleCh, replies, cancelTimeout, d.sent)
	d.cancelDone = cancelDone
	return d, err
}

// interruptDelivery is what an interrupt sent to the agent. sent records a
// written session/cancel or a delivered SIGINT; cancelDone closes once the
// goroutine writing session/cancel has finished. Both are nil when the
// interrupt started no cancel.
type interruptDelivery struct {
	sent       *atomic.Bool
	cancelDone <-chan struct{}
}

// reached reports whether session/cancel or a SIGINT reached the agent. A
// session/cancel write still in progress decides the answer, so reached
// waits for it, up to bound: a write the agent stopped reading would
// otherwise hold the caller indefinitely.
func (d interruptDelivery) reached(bound time.Duration) bool {
	if d.sent == nil {
		return false
	}
	if d.sent.Load() || d.cancelDone == nil {
		return d.sent.Load()
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-d.cancelDone:
	case <-timer.C:
	}
	return d.sent.Load()
}

// interruptTurn is interrupt aimed at turn turnID. In one critical section of
// mu it checks that turnID is the running turn and takes that turn's idle
// channel and outstanding permission requests. Its two signals are checked
// against that channel atomically with the send: session/cancel under stdinMu
// (cancelTurn) and the SIGINT fallback under mu (signalTurn), so neither
// reaches the agent once the turn has ended. The canceled permission replies
// are not fenced: they answer the turn's own request ids and may reach the
// agent after the turn ends, as replies do when a turn ends on its own.
// When turnID is not running it sends nothing, not even permission replies,
// and returns a *runtime.TurnNotCurrentError. When it is, the cancel, settle
// and SIGINT fallback run on a goroutine the connection owns, and
// interruptTurn returns at once. A turn that does not settle is logged there.
func (sc *sessionConn) interruptTurn(name, turnID string, cancelTimeout time.Duration) error {
	sc.mu.Lock()
	if sc.drained {
		sc.mu.Unlock()
		return errACPConnClosed(name)
	}
	if sc.currentTurn == nil || sc.currentTurn.ID != turnID {
		current := sc.turnStateLocked()
		sc.mu.Unlock()
		return &runtime.TurnNotCurrentError{Session: name, TurnID: turnID, Current: current}
	}
	sc.ensureIdleChannelLocked()
	// The running turn owns this channel and every held permission request:
	// the turn's end closes the channel and cancels the requests under mu.
	idleCh := sc.idleCh
	sessionID := sc.sessionID
	outstanding := sc.takePermissionsLocked()
	sc.mu.Unlock()

	replies := encodeCanceledReplies(outstanding)
	go func() {
		if _, err := sc.cancelAndSettle(name, sessionID, idleCh, replies, cancelTimeout, nil); err != nil {
			fmt.Fprintf(os.Stderr, "acp: interrupt of turn %q: %v\n", turnID, err)
		}
	}()
	return nil
}

// cancelAndSettle sends the canceled permission replies and session/cancel
// for the turn behind idleCh, then waits up to cancelTimeout for it to end and
// falls back to SIGINT, as interrupt documents. session/cancel (under
// stdinMu) and the SIGINT (under mu) are each made only while idleCh is open,
// checked atomically with the send, so neither reaches the agent after that
// turn has ended. The canceled permission replies answer the turn's own
// request ids and may arrive after the turn ends, as they do on a natural end.
// What the agent does with a SIGINT it received during the turn is up to the
// agent. sent, when set, records that session/cancel or a SIGINT reached the
// agent. The returned channel closes once the replies and session/cancel
// have been written or abandoned; cancelAndSettle itself never waits for it.
func (sc *sessionConn) cancelAndSettle(name, sessionID string, idleCh <-chan struct{}, replies [][]byte, cancelTimeout time.Duration, sent *atomic.Bool) (<-chan struct{}, error) {
	// A goroutine writes the replies and the cancel, so the wait below stays
	// bounded even if the agent has stopped reading its stdin.
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		sc.cancelTurn(name, sessionID, idleCh, replies, sent)
	}()

	if sc.waitTurnSettled(name, idleCh, cancelTimeout) {
		return cancelDone, nil
	}
	for attempt := 0; attempt < interruptSIGINTAttempts; attempt++ {
		if !sc.signalTurn(idleCh, sent) {
			return cancelDone, nil
		}
		if sc.waitTurnSettled(name, idleCh, interruptSIGINTWait) {
			return cancelDone, nil
		}
	}
	return cancelDone, fmt.Errorf("%w: ACP session %q still in its turn %s after session/cancel and %s after each of %d SIGINTs",
		runtime.ErrInterruptNotSettled, name, cancelTimeout, interruptSIGINTWait, interruptSIGINTAttempts)
}

// cancelTurn writes the canceled permission replies and then session/cancel
// for the turn behind idleCh, in the order ACP requires. The cancel is
// written only while that turn is still open, and that check is made under
// stdinMu, together with the write. Nudge writes the next turn's prompt
// through the same lock, and only after idleCh has closed. So session/cancel
// either reaches the agent ahead of the next prompt or is not sent at all; it
// can never follow that prompt and cancel the next turn instead. Nudge also
// holds stdinMu from opening a turn's record through writing its prompt, so a
// cancel for a turn whose prompt is still unwritten waits for that prompt
// rather than overtaking it. sent, when set, records a written cancel.
func (sc *sessionConn) cancelTurn(name, sessionID string, idleCh <-chan struct{}, replies [][]byte, sent *atomic.Bool) {
	if !sc.writePermissionReplies(replies) {
		return
	}
	if sc.beforeTurnCancel != nil {
		sc.beforeTurnCancel()
	}
	data, err := json.Marshal(newSessionCancelNotification(sessionID))
	if err == nil {
		err = sc.writeCancelUnlessClosed(idleCh, data, sent)
	}
	if err != nil && !isPipeWriteError(err) {
		fmt.Fprintf(os.Stderr, "acp: sending session/cancel to %q: %v\n", name, err)
	}
}

// signalTurn sends SIGINT to the agent's process group for the turn behind
// idleCh and reports whether it did. Once that turn has ended it sends
// nothing: the turn can end after a wait has given up on it, and Nudge can
// then start the next one, which the SIGINT would interrupt instead. The
// check and the signal are made under mu. The turn ends, and the next one
// opens, only under mu, and Nudge writes the next prompt only after that, so
// the signal is sent while this turn is the agent's turn and before the next
// prompt is written. kill(2) does not block, so holding mu across it keeps
// dispatch and Interrupt bounded. What remains is the agent's own latency in
// handling a signal it received during the turn. sent, when set, records a
// delivered signal.
func (sc *sessionConn) signalTurn(idleCh <-chan struct{}, sent *atomic.Bool) bool {
	if sc.beforeTurnSignal != nil {
		sc.beforeTurnSignal()
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	select {
	case <-idleCh:
		return false
	default:
	}
	if sc.onTurnSignal != nil {
		sc.onTurnSignal()
	}
	// A failure means the group is already gone; the wait observes that.
	if err := runtime.SignalProcessGroup(sc.cmd, syscall.SIGINT); err == nil && sent != nil {
		sent.Store(true)
	}
	return true
}

// writeCancelUnlessClosed is writeMessage for a session/cancel that is stale
// once closed has closed: it then writes and captures nothing and returns
// nil. closed is checked with stdinMu held, so no other stdin write can come
// between that check and this write. sent, when set, records that data was
// written.
func (sc *sessionConn) writeCancelUnlessClosed(closed <-chan struct{}, data []byte, sent *atomic.Bool) error {
	sc.stdinMu.Lock()
	defer sc.stdinMu.Unlock()
	select {
	case <-closed:
		return nil
	default:
	}
	err := sc.writeStdinLocked(nil, data, nil)
	if err == nil && sent != nil {
		sent.Store(true)
	}
	return err
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

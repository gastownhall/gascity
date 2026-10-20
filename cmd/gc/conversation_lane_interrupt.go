package main

// The live executor's interrupt (OJ-15; DES-O1 §10.1, X11 §5.4): one
// interrupt through worker.Handle.InterruptAs, turn-scoped wherever the
// session's turns allow it, so a late interrupt is refused rather than sent
// into the next turn, and session-scoped otherwise, as today. The provider
// sequences (Escape, Ctrl-C, the idle and boundary waits, the input clear)
// stay in the session layer.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// laneInterruptReadBudget bounds the interrupt's fresh read of the session's
// turns.
const laneInterruptReadBudget = 10 * time.Second

// Interrupt implements opjournal.Executor. The turn it targets is the op's
// scope turn, or, for a session-scoped op on a session whose turns carry a
// turn-scoped interrupt (worker.TurnCapabilities.InterruptsTurns), the turn
// a fresh read finds running: none running closes the op skipped{stale}.
// A session-scoped op never runs without that fresh read (X11 §5.4): a read
// that fails, or turn capabilities the probe could not read, release it,
// nothing sent. The incarnation's input fence goes with every request whose
// row names a token, with AllowUnfenced, so the worker decides where it
// cannot be carried (a protocol turn interrupt, a runtime that cannot fence)
// and reports it. The outcome is marked restarted_after_failed_interrupt
// when the op's on_failure restart replaced the runtime, else
// unfenced_interrupt unless the interrupt was confined to its turn by an
// atomic turn fence and to its incarnation by the input fence (X11 D4, the
// 2026-10-19 ruling); the restart takes precedence, as the one slot holds
// one mark. The conversation's InterruptRef is recorded for the turn
// observer before the effect and left for it to fold (laneInterrupts),
// unless the effect sent nothing.
func (e *conversationLaneExec) Interrupt(ctx context.Context, c opjournal.InterruptCall) opjournal.ControlResult {
	turn := c.Turn
	if turn == "" && e.turnCapsUnread {
		return opjournal.NotDelivered{Transient: true, Cause: fmt.Errorf("session %s's turn capabilities were unreadable; not interrupting it without a fresh read of its turns", e.info.ID)}
	}
	if turn == "" && e.turnHandle != nil && e.turnCaps.InterruptsTurns() {
		hist, err := e.interruptRead(ctx)
		switch {
		case err != nil:
			return opjournal.NotDelivered{Transient: true, Cause: fmt.Errorf("reading session %s's turns before interrupting it: %w", e.info.ID, err)}
		case !hist.Current.Active():
			return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
		default:
			turn = hist.Current.TurnID
		}
	}
	h, err := e.interruptHandle()
	if err != nil {
		return opjournal.NotDelivered{Transient: true, Cause: fmt.Errorf("resolving session %s's handle, nothing sent: %w", e.info.ID, err)}
	}
	req := worker.InterruptRequest{Turn: turn}
	if c.Op.Policy.Interrupt.OnFailure == opjournal.OnFailureRestart {
		req.OnFailure = worker.InterruptOnFailureRestart
	}
	if c.Fence.Token != "" {
		req.Fence = runtime.InputFence{Generation: c.Fence.Generation, Epoch: c.Fence.Epoch, Token: c.Fence.Token}
		req.AllowUnfenced = true
	}
	ref := opjournal.InterruptRef{Op: c.Op.ID, Turn: turn}
	e.deps.interrupts.record(c.Fence.Conversation, ref)
	res, err := h.InterruptAs(ctx, sessionActor(session.ActorController, e.deps.cityPath), req)
	exact := turn != "" && e.turnCaps.Fence == runtime.TurnFenceAtomic && !req.Fence.IsZero() && !res.Unfenced
	out := laneInterruptResult(res, err, turn, exact)
	if !laneInterruptMayHaveRun(out) {
		e.deps.interrupts.clear(c.Fence.Conversation, ref)
	}
	return out
}

// laneInterruptMayHaveRun reports whether an interrupt whose result is out
// may have reached the runtime: settled, unknown, or failed{no_boundary}.
func laneInterruptMayHaveRun(out opjournal.ControlResult) bool {
	switch r := out.(type) {
	case opjournal.Settled, opjournal.Unknown:
		return true
	case opjournal.Rejected:
		return r.Arm == opjournal.ArmFailed && r.Reason == opjournal.ReasonNoBoundary
	}
	return false
}

// interruptRead is the interrupt's fresh read of the session's turns, given
// up after laneInterruptReadBudget (worker.ObserveBounded).
func (e *conversationLaneExec) interruptRead(ctx context.Context) (runtime.TurnHistory, error) {
	rctx, cancel := context.WithTimeout(ctx, laneInterruptReadBudget)
	defer cancel()
	return worker.ObserveBounded(rctx, laneInterruptReadKey(e.deps.cityPath, e.info.ID), e.readTurns)
}

// laneInterruptReadKey keys a session's interrupt reads, apart from its
// probes' and link reads' keys.
func laneInterruptReadKey(cityPath, sessionID string) string {
	return "lane-interrupt\x00" + cityPath + "\x00" + sessionID
}

// laneInterruptResult maps one interrupt's answer to the lane's result (X11
// §5.4). turn is the turn it targeted, "" for session scope; exact says it
// was confined to that turn and incarnation (an atomic turn fence and the
// input fence). An interrupt the runtime did not settle, or a turn not seen
// to end, is failed{no_boundary}, naming turn; a replacement whose
// preparation failed after its turn settled may have reached the agent and
// is Unknown; a turn not current, or a session gone, skipped{stale}; a
// session or handle that cannot carry it failed{unsupported}; an
// unavailable runtime, a session the controller is stopping, an interrupt
// that failed before anything was sent, or an incarnation miss that sent
// nothing a transient release; a miss that sent part of the sequence, and
// any other error, may have reached the agent and is Unknown. A success is
// marked restarted_after_failed_interrupt when its restart replaced the
// runtime, which takes precedence, else unfenced_interrupt unless exact.
func laneInterruptResult(res worker.InterruptResult, err error, turn string, exact bool) opjournal.ControlResult {
	var miss *runtime.FenceMismatchError
	switch {
	case err == nil:
		degradation := ""
		switch {
		case res.Restarted:
			degradation = opjournal.DegradationRestartedAfterFailedInterrupt
		case !exact:
			degradation = opjournal.DegradationUnfencedInterrupt
		}
		return opjournal.Settled{Turn: turn, Via: laneViaLive, Degradation: degradation}
	case errors.Is(err, runtime.ErrInterruptNotSettled), errors.Is(err, runtime.ErrTurnNotEnded):
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonNoBoundary, Turn: turn}
	case errors.Is(err, session.ErrReplacementNotPrepared):
		return opjournal.Unknown{Cause: err}
	case errors.Is(err, runtime.ErrTurnNotCurrent):
		return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	case errors.Is(err, runtime.ErrTurnIdentityUnsupported), errors.Is(err, session.ErrTurnInterruptUnsupported),
		errors.Is(err, worker.ErrInterruptRestartUnsupported), errors.Is(err, runtime.ErrInputFenceUnsupported):
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
	case errors.As(err, &miss) && miss.Partial:
		return opjournal.Unknown{Cause: err}
	case errors.As(err, &miss), errors.Is(err, runtime.ErrRuntimeUnavailable), errors.Is(err, session.ErrSessionStopping):
		return opjournal.NotDelivered{Transient: true, Cause: err}
	case errors.Is(err, runtime.ErrSessionNotFound), errors.Is(err, session.ErrSessionNotFound):
		return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	case errors.Is(err, worker.ErrInterruptNotSent):
		return opjournal.NotDelivered{Transient: true, Cause: err}
	default:
		return opjournal.Unknown{Cause: err}
	}
}

// laneInterrupts is the lane host's record of the interrupt each
// conversation's executor ran last (X11 §5.4, D5, D8, DES-O1 §10.1): the
// executor records its InterruptRef before the effect, and the
// conversation's turn observer (TI-3) reads it, so its fold credits the
// turn's end to the op (opjournal.FoldTurn's interrupting). The observer
// folds turn updates between passes, after the effect returned, so the ref
// outlives the effect: the executor takes it back only when the effect sent
// nothing, and otherwise the observer clears it once it folded that turn's
// end; a newer ref replaces it. The op row carries the same credit
// (Result.Turn, also on failed{no_boundary}) for an observer that started
// after the record was lost. One effect runs at a time per conversation
// (D-1EFF), so each conversation holds at most one ref. A nil record
// records nothing.
type laneInterrupts struct {
	mu   sync.Mutex
	last map[string]opjournal.InterruptRef
}

// newLaneInterrupts returns an empty record.
func newLaneInterrupts() *laneInterrupts {
	return &laneInterrupts{last: map[string]opjournal.InterruptRef{}}
}

// record makes ref conv's interrupt, replacing any before it. A ref with no
// op or no turn names nothing for a fold to credit and is not recorded.
func (r *laneInterrupts) record(conv string, ref opjournal.InterruptRef) {
	if r == nil || ref.Op == "" || ref.Turn == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last[conv] = ref
}

// clear drops ref as conv's interrupt, if it still is: the executor's when
// its effect sent nothing, the observer's once it folded ref's turn end.
func (r *laneInterrupts) clear(conv string, ref opjournal.InterruptRef) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last[conv] == ref {
		delete(r.last, conv)
	}
}

// current is conv's interrupt, the zero ref when none is recorded.
func (r *laneInterrupts) current(conv string) opjournal.InterruptRef {
	if r == nil {
		return opjournal.InterruptRef{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last[conv]
}

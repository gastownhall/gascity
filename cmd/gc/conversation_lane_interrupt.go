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
// a fresh read finds running: none running closes the op skipped{stale},
// and a read that fails runs it session-scoped. The incarnation's input
// fence goes with it wherever the runtime fences input, except on a turn
// interrupt by protocol, which takes none (the worker refuses a fenced one).
// Its outcome is marked unfenced_interrupt unless the turn fence is atomic,
// and restarted_after_failed_interrupt when the op's on_failure restart
// replaced the runtime. While it runs, the conversation's InterruptRef is
// recorded for the turn observer.
func (e *conversationLaneExec) Interrupt(ctx context.Context, c opjournal.InterruptCall) opjournal.ControlResult {
	turn := c.Turn
	if turn == "" && e.turnHandle != nil && e.turnCaps.InterruptsTurns() {
		hist, err := e.interruptRead(ctx)
		switch {
		case err != nil:
			e.note("interrupt", fmt.Errorf("reading session %s's turns before interrupting it; interrupting it session-scoped: %w", e.info.ID, err))
		case !hist.Current.Active():
			return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
		default:
			turn = hist.Current.TurnID
		}
	}
	h, err := e.interruptHandle()
	if err != nil {
		return laneInterruptResult(worker.InterruptResult{}, fmt.Errorf("resolving session %s's handle: %w", e.info.ID, err), turn, false)
	}
	req := worker.InterruptRequest{Turn: turn}
	if c.Op.Policy.Interrupt.OnFailure == opjournal.OnFailureRestart {
		req.OnFailure = worker.InterruptOnFailureRestart
	}
	protocolTurn := turn != "" && e.turnCaps.Source == worker.TurnSourceProtocol
	if c.Fence.Token != "" && e.lastCaps.Input != opjournal.FenceNone && !protocolTurn {
		req.Fence = runtime.InputFence{Generation: c.Fence.Generation, Epoch: c.Fence.Epoch, Token: c.Fence.Token}
	}
	exact := turn != "" && e.turnCaps.Fence == runtime.TurnFenceAtomic
	defer e.deps.interrupts.begin(c.Fence.Conversation, opjournal.InterruptRef{Op: c.Op.ID, Turn: turn})()
	res, err := h.InterruptAs(ctx, sessionActor(session.ActorController, e.deps.cityPath), req)
	return laneInterruptResult(res, err, turn, exact)
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
// §5.4). turn is the turn it targeted, "" for session scope; exact says the
// turn fence confined it to that turn (atomic). A turn not seen to end is
// failed{no_boundary}; a turn not current, or a session gone, skipped{stale};
// a session or handle that cannot carry it failed{unsupported}; an
// unavailable runtime or an incarnation miss that sent nothing a transient
// release; a miss that sent part of the sequence, and any other error, may
// have reached the agent and is Unknown.
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
	case errors.Is(err, runtime.ErrTurnNotEnded):
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonNoBoundary}
	case errors.Is(err, runtime.ErrTurnNotCurrent):
		return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	case errors.Is(err, runtime.ErrTurnIdentityUnsupported), errors.Is(err, session.ErrTurnInterruptUnsupported),
		errors.Is(err, worker.ErrInterruptRestartUnsupported), errors.Is(err, runtime.ErrInputFenceUnsupported):
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
	case errors.As(err, &miss) && miss.Partial:
		return opjournal.Unknown{Cause: err}
	case errors.As(err, &miss), errors.Is(err, runtime.ErrRuntimeUnavailable):
		return opjournal.NotDelivered{Transient: true, Cause: err}
	case errors.Is(err, runtime.ErrSessionNotFound), errors.Is(err, session.ErrSessionNotFound):
		return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	default:
		return opjournal.Unknown{Cause: err}
	}
}

// laneInterrupts is the lane host's record of the interrupt each
// conversation's executor is running (X11 §5.4, DES-O1 §10.1): the executor
// hands its InterruptRef in for the effect's duration, and the conversation's
// turn observer (TI-3) reads it, so its fold credits the turn's end to the
// op (opjournal.FoldTurn's interrupting). One effect runs at a time per
// conversation (D-1EFF), so each conversation holds at most one ref. A nil
// record records nothing.
type laneInterrupts struct {
	mu      sync.Mutex
	running map[string]opjournal.InterruptRef
}

// newLaneInterrupts returns an empty record.
func newLaneInterrupts() *laneInterrupts {
	return &laneInterrupts{running: map[string]opjournal.InterruptRef{}}
}

// begin records ref as conv's running interrupt until end is called. A ref
// with no op or no turn names nothing for a fold to credit and is not
// recorded.
func (r *laneInterrupts) begin(conv string, ref opjournal.InterruptRef) (end func()) {
	if r == nil || ref.Op == "" || ref.Turn == "" {
		return func() {}
	}
	r.mu.Lock()
	r.running[conv] = ref
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.running[conv] == ref {
			delete(r.running, conv)
		}
	}
}

// current is conv's running interrupt, the zero ref when none runs.
func (r *laneInterrupts) current(conv string) opjournal.InterruptRef {
	if r == nil {
		return opjournal.InterruptRef{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[conv]
}

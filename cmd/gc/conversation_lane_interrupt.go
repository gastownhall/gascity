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
// scope turn, else the one interruptTarget resolves for a session-scoped op:
// a session-scoped op never runs without a fresh read of its turns where
// they carry a turn-scoped interrupt, and a retry never re-aims at the turn
// after the one its earlier attempt aimed at, nor runs once that turn is
// unknown. A handle that will not resolve
// releases the op, or for a session gone closes it skipped{stale}. The
// incarnation's input fence goes with every request whose row names a
// token, with AllowUnfenced, so the worker decides where it cannot be
// carried (a protocol turn interrupt, a runtime that cannot fence) and
// reports it. The outcome is marked restarted_after_failed_interrupt when
// the op's on_failure restart replaced the runtime, else unfenced_interrupt
// unless the interrupt was confined to its turn by an atomic turn fence and
// to its incarnation by the input fence (X11 D4, the 2026-10-19 ruling); the
// restart takes precedence, as the one slot holds one mark. The
// conversation's InterruptRef is recorded before the effect (laneInterrupts),
// so a retry of a session-scoped op stays on the turn this attempt aimed at,
// unless the effect definitely sent nothing, when the ref it replaced is
// restored. A deliver-now unit's replacement step (c.ForReplacement) asks
// the worker to prepare the session for the message once the turn settles,
// and reports where that left the session (replacementResult).
func (e *conversationLaneExec) Interrupt(ctx context.Context, c opjournal.InterruptCall) opjournal.ControlResult {
	turn, stop := e.interruptTarget(ctx, c)
	if stop != nil {
		return stop
	}
	h, err := e.interruptHandle()
	switch {
	case laneSessionGone(err):
		return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	case err != nil:
		return opjournal.NotDelivered{Transient: true, Cause: fmt.Errorf("resolving session %s's handle, nothing sent: %w", e.info.ID, err)}
	}
	req := worker.InterruptRequest{Turn: turn, ForReplacement: c.ForReplacement}
	if c.Op.Policy.Interrupt.OnFailure == opjournal.OnFailureRestart {
		req.OnFailure = worker.InterruptOnFailureRestart
	}
	if c.Fence.Token != "" {
		req.Fence = runtime.InputFence{Generation: c.Fence.Generation, Epoch: c.Fence.Epoch, Token: c.Fence.Token}
		req.AllowUnfenced = true
	}
	ref := opjournal.InterruptRef{Op: c.Op.ID, Turn: turn}
	prev := e.deps.interrupts.record(c.Fence.Conversation, ref)
	res, err := h.InterruptAs(ctx, sessionActor(session.ActorController, e.deps.cityPath), req)
	// A tier the probe could not read (turnCapsUnread) is not shown atomic,
	// so the outcome is marked: only an interrupt shown confined goes
	// unmarked.
	exact := turn != "" && e.turnCaps.Fence == runtime.TurnFenceAtomic && !req.Fence.IsZero() && !res.Unfenced
	out := laneInterruptResult(ctx, res, err, turn, exact)
	if c.ForReplacement {
		out = e.replacementResult(ctx, out, err, turn)
	}
	if laneInterruptSentNothing(out) {
		e.deps.interrupts.restore(c.Fence.Conversation, ref, prev)
	}
	return out
}

// interruptTarget is the turn c's interrupt targets, "" for session scope,
// or the result that ends the attempt with nothing sent. An op that names
// its turn targets it. A session-scoped op whose earlier attempt may have
// reached the runtime targets the turn that attempt aimed at while the
// record still holds that attempt's ref (laneInterrupts): content may have
// started the next turn since, and a fresh read would name that one. Turn
// capabilities the probe could not read release the op. Past those, an op
// whose earlier attempt may have reached the runtime with no ref naming it
// closes failed{ambiguous_delivery} unsent, on every session: a controller
// restart loses the record, and an attempt that ran session-scoped named
// no turn to record. The turn that attempt reached is unknown, so neither a
// fresh read nor another session-scoped interrupt can tell the next turn
// from it. Any other session-scoped op runs session-scoped on a session
// whose turns carry no turn-scoped interrupt
// (worker.TurnCapabilities.InterruptsTurns); where they carry one it
// targets the turn a fresh read finds running, and closes skipped{stale}
// when none runs, never running without that read (X11 §5.4). A read that
// cannot answer (runtime.ErrRuntimeUnavailable) releases the op, marked
// unavailable unless the read was held back on this side of the runtime
// (laneInterruptHeldBack); a read that finds the session gone closes it
// skipped{stale}; one that finds its turns carry no identity runs it
// session-scoped; any other read error releases it with a failure counted.
func (e *conversationLaneExec) interruptTarget(ctx context.Context, c opjournal.InterruptCall) (string, opjournal.ControlResult) {
	if c.Turn != "" {
		return c.Turn, nil
	}
	if ref := e.deps.interrupts.current(c.Fence.Conversation); ref.Op == c.Op.ID {
		return ref.Turn, nil
	}
	if e.turnCapsUnread {
		return "", opjournal.NotDelivered{Transient: true, Unavailable: true, Cause: fmt.Errorf("session %s's turn capabilities were unreadable; not interrupting it without a fresh read of its turns", e.info.ID)}
	}
	if c.Op.MaybeDelivered > 0 {
		return "", opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonAmbiguousDelivery}
	}
	if e.turnHandle == nil || !e.turnCaps.InterruptsTurns() {
		return "", nil
	}
	hist, err := e.interruptRead(ctx)
	switch {
	case errors.Is(err, runtime.ErrRuntimeUnavailable):
		return "", opjournal.NotDelivered{Transient: true, Unavailable: !laneInterruptHeldBack(ctx, err), Cause: fmt.Errorf("reading session %s's turns before interrupting it: %w", e.info.ID, err)}
	case laneSessionGone(err):
		return "", opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	case errors.Is(err, runtime.ErrTurnIdentityUnsupported):
		// The session's turns carry no identity (a transcript without turn
		// ids): it has none to scope the interrupt to (X11 D1, §8), so it
		// runs session-scoped, as on a session without turns.
		e.note("interrupt", fmt.Errorf("session %s's turns carry no identity; interrupting it session-scoped: %w", e.info.ID, err))
		return "", nil
	case err != nil:
		return "", opjournal.NotDelivered{Cause: fmt.Errorf("reading session %s's turns before interrupting it, nothing sent: %w", e.info.ID, err)}
	case !hist.Current.Active():
		return "", opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	}
	return hist.Current.TurnID, nil
}

// laneInterruptHeldBack reports whether err, the runtime.ErrRuntimeUnavailable
// of an interrupt that sent nothing, held the interrupt back on this side of
// the runtime rather than saying the runtime could not answer: the session
// mutation lock still held at the effect's deadline
// (session.ErrSessionMutationBusy), the restart's runtime lease held by
// another (session.ErrRuntimeLeaseBusy), a read refused at once because an
// earlier read of its key is still out (worker.ErrObservationOutstanding: one
// a budget gave up on, wedged in a store or a call that ignores its context,
// the lane's own read included), or ctx, the effect's, ended (its lease
// budget, a city stop) before the interrupt could be sent. Such a release
// says nothing of the runtime, as the attempt asked it nothing, so start_by
// closes it expired_before_start, as it does any other transient release,
// never provider_unavailable. The attempt whose read was given up is
// released by what it saw.
func laneInterruptHeldBack(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, session.ErrSessionMutationBusy) || errors.Is(err, session.ErrRuntimeLeaseBusy) ||
		errors.Is(err, worker.ErrObservationOutstanding)
}

// laneInterruptSentNothing reports whether an interrupt whose result is out
// definitely sent nothing: a release (laneInterruptResult releases only what
// sent nothing), a turn not current or a session gone (skipped{stale}), or a
// refusal (failed{unsupported}). Anything else (settled, unknown,
// failed{no_boundary}) may have reached the runtime.
func laneInterruptSentNothing(out opjournal.ControlResult) bool {
	switch r := out.(type) {
	case opjournal.NotDelivered:
		return true
	case opjournal.Rejected:
		return r.Arm == opjournal.ArmSkipped && r.Reason == opjournal.ReasonStale ||
			r.Arm == opjournal.ArmFailed && r.Reason == opjournal.ReasonUnsupported
	}
	return false
}

// interruptRead is the interrupt's fresh read of the session's turns, given
// up after laneInterruptReadBudget (worker.ObserveBounded). Its reads are its
// own (worker.TurnHandle.FreshTurn): a read of the session already in
// flight, a turn watch's or a probe's, may have begun before the turn now
// running did, and would aim the interrupt at the turn before it. The worker
// bounds its session load and runtime probes by the budget; a transcript's
// file reads it does not, so the read as a whole is given up here, under a
// key that also serializes the session's fresh reads, as the worker needs.
func (e *conversationLaneExec) interruptRead(ctx context.Context) (runtime.TurnHistory, error) {
	rctx, cancel := context.WithTimeout(ctx, laneInterruptReadBudget)
	defer cancel()
	return worker.ObserveBounded(rctx, laneInterruptReadKey(e.deps.cityPath, e.info.ID), e.freshTurns)
}

// freshTurns reads the session's turns once through the executor's turn
// handle, from runtime reads of its own (worker.TurnHandle.FreshTurn).
func (e *conversationLaneExec) freshTurns(ctx context.Context) (runtime.TurnHistory, error) {
	if e.turnHandle == nil {
		return runtime.TurnHistory{}, runtime.ErrTurnIdentityUnsupported
	}
	return e.turnHandle.FreshTurn(ctx)
}

// laneInterruptReadKey keys a session's interrupt reads, apart from its
// probes' and link reads' keys.
func laneInterruptReadKey(cityPath, sessionID string) string {
	return "lane-interrupt\x00" + cityPath + "\x00" + sessionID
}

// laneInterruptResult maps one interrupt's answer to the lane's result (X11
// §5.4); ctx is the effect's. turn is the turn it targeted, "" for session
// scope; exact says it was confined to that turn and incarnation (an atomic
// turn fence and the input fence). An interrupt the runtime did not settle,
// or a turn not seen to end, is failed{no_boundary}, naming turn. Unknown,
// since it may have reached the agent: a replacement whose preparation
// failed after its turn settled; a restart on failure that stopped the
// runtime and could not start it again (session.ErrRestartNotStarted: the
// turn ended with the runtime, and the start's error stays the op's last
// error; the controller's heal of a row whose runtime is gone, SESS-531,
// marks the row asleep, which may reset its continuation, and A18's relaunch
// of a row it wants awake, or a later wake, starts a runtime again); one
// sent whose outcome the session cannot tell
// (session.ErrInterruptOutcomeUnknown: a send that failed after its check, a
// protocol dispatch that failed, a restart on failure withheld because the
// caller's context ended, the check refused the follow-up interrupt, or
// nothing showed the turn still running); one whose runtime lost the session
// after its input was applied (runtime.ErrNudgeDeliveryUnconfirmed, though
// it says unavailable too); a miss that sent part of the sequence; and any
// error in no class below. A turn not current, or a session gone, is
// skipped{stale}; a session or handle that cannot carry it
// failed{unsupported}. An unavailable runtime is a transient release marked
// unavailable (opjournal.NotDelivered.Unavailable: start_by closes it
// provider_unavailable), unless the interrupt was held back on this side of
// the runtime (laneInterruptHeldBack); that, a session the controller is
// stopping, an interrupt that failed before anything was sent, or an
// incarnation miss that sent nothing, is a transient release. A stop or turn
// interrupt that failed before it sent anything for a cause of its own
// (session.ErrStopNotSent: its load of the session row under the lock, an
// actor refused a restart) asked the runtime nothing, so it is a transient
// release not marked unavailable, though the worker reports a row that would
// not load unavailable too, and skipped{stale} for a session gone. An
// interrupt that failed before anything was sent
// (worker.ErrInterruptNotSent) is classed by its cause first, when that says
// the runtime unavailable (a release start_by may close
// provider_unavailable) or the session gone (skipped{stale}), and is a plain
// transient release otherwise. A success is marked
// restarted_after_failed_interrupt when its restart replaced the runtime,
// which takes precedence, else unfenced_interrupt unless exact.
func laneInterruptResult(ctx context.Context, res worker.InterruptResult, err error, turn string, exact bool) opjournal.ControlResult {
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
	case errors.Is(err, session.ErrReplacementNotPrepared), errors.Is(err, session.ErrRestartNotStarted),
		errors.Is(err, session.ErrInterruptOutcomeUnknown), errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed):
		return opjournal.Unknown{Cause: err}
	case errors.Is(err, runtime.ErrTurnNotCurrent):
		return opjournal.Rejected{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale}
	case errors.Is(err, runtime.ErrTurnIdentityUnsupported), errors.Is(err, session.ErrTurnInterruptUnsupported),
		errors.Is(err, worker.ErrInterruptRestartUnsupported), errors.Is(err, runtime.ErrInputFenceUnsupported):
		return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
	case errors.As(err, &miss) && miss.Partial:
		return opjournal.Unknown{Cause: err}
	case errors.As(err, &miss), errors.Is(err, session.ErrSessionStopping),
		errors.Is(err, session.ErrStopNotSent) && !laneSessionGone(err):
		return opjournal.NotDelivered{Transient: true, Cause: err}
	case errors.Is(err, runtime.ErrRuntimeUnavailable):
		return opjournal.NotDelivered{Transient: true, Unavailable: !laneInterruptHeldBack(ctx, err), Cause: err}
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
// executor records its InterruptRef before the effect and takes it back only
// when the effect definitely sent nothing, restoring the ref it replaced; a
// newer ref replaces it. While it stands it keeps a retry of its
// session-scoped op on the turn the op's earlier attempt aimed at
// (interruptTarget). The conversation's turn observer (TI-3) does not read
// this record: it credits a turn's end to the op from the op rows
// (opjournal.Store.InterruptCredits, Result.Turn, also on
// failed{no_boundary}), which count only an op that ran, so an attempt
// released unknown that ended its turn is not credited (ga-6plx6a). A ref
// stands until a newer one replaces it. One effect runs at a time per
// conversation (D-1EFF), so each conversation holds one ref: a second op's
// effect replaces the first op's ref, and a retry of the first then finds
// none of its own and closes failed{ambiguous_delivery} unsent, as after a
// controller restart, never aimed at a turn a fresh read finds (ga-6plx6a).
// A nil record records nothing.
type laneInterrupts struct {
	mu   sync.Mutex
	last map[string]opjournal.InterruptRef
}

// newLaneInterrupts returns an empty record.
func newLaneInterrupts() *laneInterrupts {
	return &laneInterrupts{last: map[string]opjournal.InterruptRef{}}
}

// record makes ref conv's interrupt, replacing any before it, and returns
// the ref it replaced (the zero ref for none). A ref with no op or no turn
// names nothing for a fold to credit and is not recorded.
func (r *laneInterrupts) record(conv string, ref opjournal.InterruptRef) opjournal.InterruptRef {
	if r == nil || ref.Op == "" || ref.Turn == "" {
		return opjournal.InterruptRef{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.last[conv]
	r.last[conv] = ref
	return prev
}

// restore takes ref back as conv's interrupt, if it still is, and puts prev,
// the ref it replaced, back in its place: the executor's when its effect
// sent nothing, so a retry of the op prev names still finds its turn. A zero
// prev leaves none.
func (r *laneInterrupts) restore(conv string, ref, prev opjournal.InterruptRef) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last[conv] != ref {
		return
	}
	if prev == (opjournal.InterruptRef{}) {
		delete(r.last, conv)
		return
	}
	r.last[conv] = prev
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

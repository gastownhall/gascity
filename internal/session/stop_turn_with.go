package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// StopTurnOptions shape one stop of a session's turn (Manager.StopTurnWith).
// The zero value is StopTurn's stop.
type StopTurnOptions struct {
	// Still is StopTurnIf's check: it runs under the session mutation lock
	// once the stop has found a turn to interrupt and before it sends
	// anything, and again before a follow-up interrupt of the keystroke
	// sequence. An error from it before anything is sent is returned,
	// wrapped, and nothing is sent. One before the follow-up sends nothing
	// more and ends the stop's own waits: one saying the turn is no longer
	// current (runtime.ErrTurnNotCurrent) has seen the turn end, and the
	// waits pass; any other fails them (an error wrapping
	// runtime.ErrTurnNotEnded, the refusal in its text only, since the
	// first key was sent).
	Still func() error
	// Settle, when set, decides whether the interrupted turn ended, still
	// under the session mutation lock: it gets the error of the stop's own
	// waits (nil when they passed, else an error wrapping
	// runtime.ErrTurnNotEnded) and returns nil when the turn ended, else an
	// error wrapping runtime.ErrTurnNotEnded: TurnEndUnreadError's when
	// nothing showed the turn still running (no read of it answered, and the
	// waits passed or failed only for want of an answer), which withholds a
	// restart on failure. It runs only once something was sent. Unset, the
	// stop's own waits decide.
	Settle func(waitErr error) error
	// ForReplacement prepares the session for the message that replaces the
	// interrupted turn, as deliver-now does between its interrupt and its
	// message (X11 D11): once the turn settled it discards the interrupted
	// turn where the family needs that (Gemini's reset), and on a keystroke
	// runtime clears the input box for the message where the family leaves
	// input in it (Claude, Gemini). Without it a stop only clears a draft
	// Claude restores, best effort. A failed preparation is an error
	// wrapping ErrReplacementNotPrepared, the interrupt already settled. As
	// with Still, whether the runtime runs is a fresh liveness read's
	// (StopTurnWith).
	ForReplacement bool
	// Restart, when set, replaces the runtime when the interrupt was sent
	// and its turn was not seen to end: an interrupt op's on_failure:
	// restart, run inline as interrupt_now does after a failed idle wait
	// (DES-O1 §10.1). It applies to a keystroke runtime only: a protocol
	// runtime's stop may leave it running, so a restart would interrupt
	// nothing, and its stop returns the not-ended error as without Restart.
	// A row whose drain-ack stop is pending refuses it, nothing sent, with
	// ErrSessionStopping. A caller whose ctx ended by the time the turn was
	// not seen to end gets no restart, the runtime left as it is, but an
	// error wrapping ErrInterruptOutcomeUnknown and
	// runtime.ErrRuntimeUnavailable, and not ErrTurnNotEnded; so does a
	// stop whose check refused the follow-up interrupt (Still), since the
	// turn it names left Current or the check could not tell, and whatever
	// runs then may be another turn, which a restart would kill; and so does
	// one whose Settle saw nothing showing the turn still running
	// (TurnEndUnreadError), or, without Settle, whose own waits read nothing
	// showing the session busy (runtime.ErrWaitUnobserved, or a runtime
	// that could not answer), since that cannot tell either; and so does a
	// caller whose ctx deadline leaves less than stopTurnRestartBound by
	// then, since the restart could outlive it. A restart once decided runs
	// apart from ctx, within stopTurnRestartBound. As with
	// Still, whether the runtime runs is a fresh liveness read's
	// (StopTurnWith).
	Restart *StopTurnRestart
}

// StopTurnRestart is what replacing a runtime whose interrupted turn did not
// settle needs. The runtime lease is taken for By before anything is sent,
// and held through the restart (D8D-5).
type StopTurnRestart struct {
	By            Actor
	ResumeCommand string
	Hints         runtime.Config
}

// StopTurnReport says what one stop did beyond its error.
type StopTurnReport struct {
	// Restarted says the interrupt was sent, its turn was not seen to end,
	// and the runtime was stopped and started again in its place
	// (StopTurnOptions.Restart): the stop then returns nil.
	Restarted bool
}

// StopTurnWith is StopTurnIf shaped by opts (see StopTurnOptions); with the
// zero opts it is StopTurnIf(ctx, id, nil), and with only Still it is
// StopTurnIf(ctx, id, opts.Still), whose doc gives the errors. With Restart
// it refuses an actor that may not start the runtime before anything else,
// and takes the runtime lease under the session mutation lock before Still
// runs and before it sends anything: a lease it cannot take is an error
// wrapping runtime.ErrRuntimeUnavailable, nothing sent. With Restart or
// ForReplacement, as with Still, whether the runtime runs, before the lease
// and again before the first key, is a fresh liveness read's, never the
// provider's IsRunning, which the tmux state cache can answer stale: a stale
// "not running" would send nothing, skip the restart or the preparation, and
// report success. A read that does not find the agent alive is no turn, and
// the stop returns nil, nothing sent; one that cannot answer is an error
// wrapping runtime.ErrRuntimeUnavailable, nothing sent. A ctx with a
// deadline, or a Still check, bounds the wait for the session mutation lock
// by ctx: a lock still held when ctx ends is an error wrapping
// runtime.ErrRuntimeUnavailable and ErrSessionMutationBusy, nothing sent. A
// row that, loaded under the lock, is closed, gone, not a session or
// unreadable, and an actor refused a restart, is an error wrapping
// ErrStopNotSent and the cause's own sentinels, nothing sent. A restart whose
// stop fails is an error wrapping runtime.ErrTurnNotEnded and no other
// sentinel, its cause in its text: the interrupt was sent, the turn was not
// seen to end, and the runtime is as the stop left it. One whose start fails
// once the runtime stopped is an error wrapping ErrRestartNotStarted and no
// other sentinel, the start's error in its text.
func (m *Manager) StopTurnWith(ctx context.Context, id string, opts StopTurnOptions) (StopTurnReport, error) {
	if opts.Restart != nil {
		if err := m.checkStarter(opts.Restart.By); err != nil {
			return StopTurnReport{}, fmt.Errorf("stopping the turn of %s, restarting it on failure: %w: %w", id, ErrStopNotSent, err)
		}
	}
	still := opts.Still
	// A checked stop, and one with a restart on failure or a replacement,
	// decides whether the runtime runs from a fresh liveness read, never from
	// a cached IsRunning (stopFindsNoTurn).
	fresh := still != nil || opts.Restart != nil || opts.ForReplacement
	lock := withSessionMutationLock
	if _, bounded := ctx.Deadline(); still != nil || bounded {
		lock = func(id string, fn func() error) error {
			err := WithSessionMutationLockContext(ctx, id, fn)
			if errors.Is(err, ErrSessionMutationBusy) {
				return fmt.Errorf("stopping the turn of %s: %w: %w", id, runtime.ErrRuntimeUnavailable, err)
			}
			return err
		}
	}
	var report StopTurnReport
	err := lock(id, func() error {
		b, sessName, err := m.sessionBead(id)
		if err != nil {
			return fmt.Errorf("stopping the turn of %s: %w: %w", id, ErrStopNotSent, err)
		}
		if opts.Restart != nil && drainAckStopPendingMetadata(b.Metadata) {
			// The row belongs to the controller's stop: no interrupt keys,
			// no restart's stop, as interrupt_now refuses it.
			return fmt.Errorf("stopping the turn of %s, restarting it on failure: %w", id, ErrSessionStopping)
		}
		_, fenced := runtime.InputFenceFrom(ctx)
		if fenced && runtime.InputFencingFor(m.sp, sessName) == runtime.InputFencingNone {
			return fmt.Errorf("stopping the turn of %s: %w", id, runtime.ErrInputFenceUnsupported)
		}
		if none, err := m.stopFindsNoTurn(ctx, b, sessName, fresh); none {
			// No runtime means no turn to interrupt: stopTurnLocked sends
			// nothing, so there is nothing to wait for either. A fenced
			// stop, or a fresh one, reports what its fresh read found:
			// never success into none, nor no turn from a stale cache.
			return err
		}
		mode := runtime.TurnInterruptModeFor(m.sp, sessName)
		restart := opts.Restart != nil && mode == runtime.TurnInterruptKeystroke
		var by Actor
		if restart {
			// Every interrupt that may end in a stop and a restart takes the
			// runtime lease before it sends anything, and carries it to the
			// restart, as interrupt_now does (D8D-5). It takes it before the
			// check too, as the fenced deliver-now does before its turn
			// check, so the lease's store round trip never widens the window
			// between the check and the keys; a check that refuses releases
			// it.
			leased, release, err := m.leaseForRestartLocked(ctx, opts.Restart.By, id, sessName)
			if err != nil {
				return fmt.Errorf("stopping the turn of %s: taking the runtime lease for its restart on failure, nothing sent: %w: %w", id, runtime.ErrRuntimeUnavailable, err)
			}
			defer release()
			by = leased
		}
		if still != nil {
			if err := still(); err != nil {
				return fmt.Errorf("stopping the turn of %s: %w", id, err)
			}
		}
		interruptStartedAt := time.Now()
		none, err := m.sendTurnInterruptLocked(ctx, b, sessName, mode, fresh)
		switch {
		case still == nil && (none || err != nil):
			// Nothing was sent (the runtime gone at the keys, a miss), or
			// the send failed: StopTurn returns that and waits for nothing.
			return err
		case none && err == nil:
			return fmt.Errorf("stopping the turn of %s: the runtime or its agent stopped after the check, nothing sent: %w", id, runtime.ErrSessionNotFound)
		case none, errors.Is(err, runtime.ErrFenceMismatch), errors.Is(err, runtime.ErrInputFenceUnsupported),
			errors.Is(err, ErrTurnInterruptUnsupported),
			errors.Is(err, runtime.ErrRuntimeUnavailable) && !errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed):
			// Nothing was sent (a miss, a refusal), or the runtime's own
			// error says it could not tell before its key, as a fenced
			// send's liveness or fence read that cannot answer does. A
			// runtime that lost the session after its input was applied
			// says so (runtime.ErrNudgeDeliveryUnconfirmed): that key may
			// have landed, so it is unknown, below.
			return err
		case err != nil:
			return fmt.Errorf("stopping the turn of %s: %w: %w: %w", id, ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, err)
		}
		waitErr := m.waitForInterruptSettleLocked(ctx, b, sessName, mode, still, interruptStartedAt)
		refusal := followUpRefusal(waitErr)
		if errors.Is(refusal, runtime.ErrTurnNotCurrent) {
			// The follow-up's check saw the turn leave Current after the
			// first key: it ended, and the stop's own waits pass.
			waitErr = nil
		}
		if opts.Settle != nil {
			waitErr = opts.Settle(waitErr)
		}
		if waitErr != nil {
			if !restart || !errors.Is(waitErr, runtime.ErrTurnNotEnded) {
				return waitErr
			}
			if err := restartWithheld(ctx, "stopping the turn of "+id, refusal, waitErr); err != nil {
				return err
			}
			if err := m.restartAfterFailedInterruptLocked(ctx, by, id, b, sessName, opts.Restart); err != nil {
				return err
			}
			report.Restarted = true
			return nil
		}
		if opts.ForReplacement {
			return m.prepareReplacementLocked(ctx, b, id, sessName, mode)
		}
		if restoresInterruptedInput(b) && mode == runtime.TurnInterruptKeystroke {
			// The stop already succeeded. A draft that will not clear is
			// logged, not returned: the next submit clears the input box
			// again before it pastes.
			if _, err := m.clearRestoredInputLocked(ctx, sessName); err != nil {
				log.Printf("session: stopped the turn of %q but %v", sessName, err)
			}
		}
		// After the restored prompt is cleared: Claude's background shells
		// are stopped from an empty input box.
		if stopsBackgroundWork(b) {
			m.stopBackgroundWorkLocked(context.Background(), sessName)
		}
		return nil
	})
	return report, err
}

// waitForInterruptSettleLocked runs a stop's own waits after its interrupt
// was sent: the family's idle wait (with the keystroke sequence's follow-up
// interrupt) and its boundary wait. A wait that fails is an error wrapping
// runtime.ErrTurnNotEnded: the interrupt was sent.
func (m *Manager) waitForInterruptSettleLocked(ctx context.Context, b beads.Bead, sessName string, mode runtime.TurnInterruptMode, still func() error, since time.Time) error {
	idleErr := m.waitForInterruptIdleLocked(ctx, b, sessName, mode, still)
	if idleErr != nil {
		idleErr = fmt.Errorf("waiting for stopped session to become idle: %w: %w", runtime.ErrTurnNotEnded, idleErr)
		if !errors.Is(idleErr, runtime.ErrWaitUnobserved) {
			return idleErr
		}
	}
	// An idle wait that read nothing showing the session busy
	// (runtime.ErrWaitUnobserved: its pane showed neither its prompt nor a
	// busy indicator) does not decide the turn, so the boundary wait, which
	// reads the turn apart from the pane (Codex's rollout), still does: its
	// pass is the turn's end, and a failure that read the turn (a timeout
	// after reading it running) is the turn still running. A boundary wait
	// that read nothing either (waitReadNothing), or none to wait for,
	// leaves the idle wait's error.
	waited, err := m.readInterruptBoundaryLocked(ctx, b, sessName, since)
	switch {
	case idleErr != nil && (!waited || waitReadNothing(err)):
		return idleErr
	case err != nil:
		return fmt.Errorf("waiting for stopped session interrupt boundary: %w: %w", runtime.ErrTurnNotEnded, err)
	}
	return nil
}

// restartWithheld is the error of a stop or a turn interrupt with a restart
// on failure whose interrupted turn was not seen to end (waitErr) but which
// must not restart, or nil when the restart runs; what names the operation,
// as the caller's other errors do. Its caller gave up (ctx ended: a city
// stop, its lease budget), which is no reason to stop the runtime, or its
// deadline leaves less than stopTurnRestartBound, so the restart, which runs
// apart from ctx, could outlive the caller's lease and leave a runtime
// stopped under a lease another host may take; or the
// check refused the keystroke sequence's follow-up interrupt (refusal), so
// the turn it names left Current, or the check could not tell, and whatever
// runs now may be another turn, the ended one's successor, which a restart
// would kill; or nothing showed the turn still running, which cannot tell
// either: waitErr is a TurnEndUnreadError (no read of it answered), or, on
// a session-scoped stop, which has no turn identity to read, the stop's own
// waits read nothing showing the session busy (waitReadNothing) or its
// follow-up interrupt failed to send with nothing before it that read the
// session busy (errFollowUpNotSent). On the turn-scoped path Settle has
// already replaced waitErr with a text-only error, so the latter rules
// cannot fire there; there a follow-up goes only after the check read the
// turn current. Nothing more is sent, the runtime is left as it is, and
// whether the interrupt ended the turn is unknown: the error wraps
// ErrInterruptOutcomeUnknown and runtime.ErrRuntimeUnavailable, not
// runtime.ErrTurnNotEnded.
func restartWithheld(ctx context.Context, what string, refusal, waitErr error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%s: the caller's context ended before its interrupted turn was seen to end, not restarting it: %w: %w: %w (%s)", what, ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, cerr, waitErr.Error())
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < stopTurnRestartBound {
		return fmt.Errorf("%s: the caller's deadline leaves less than the %s a restart may take, not restarting it: %w: %w (%s)", what, stopTurnRestartBound, ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, waitErr.Error())
	}
	if refusal != nil {
		return fmt.Errorf("%s: the check refused the follow-up interrupt (%s), so another turn may run now, not restarting it: %w: %w (%s)", what, refusal.Error(), ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, waitErr.Error())
	}
	if errors.As(waitErr, new(*turnEndUnreadError)) || waitReadNothing(waitErr) || errors.Is(waitErr, errFollowUpNotSent) {
		return fmt.Errorf("%s: nothing showed its interrupted turn still running, not restarting it: %w: %w (%s)", what, ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, waitErr.Error())
	}
	return nil
}

// waitReadNothing reports whether err, a stop's wait's, read nothing showing
// the session busy: the wait timed out without such a reading
// (runtime.ErrWaitUnobserved, RUNTIME-CONTRACT-016), or the runtime could not
// answer (runtime.ErrRuntimeUnavailable). Such a failure is no evidence that
// the interrupted turn still runs.
func waitReadNothing(err error) bool {
	return errors.Is(err, runtime.ErrWaitUnobserved) || errors.Is(err, runtime.ErrRuntimeUnavailable)
}

// restartAfterFailedInterruptLocked replaces the runtime whose interrupted
// turn did not settle, under the lease by carries: the stop interrupt_now
// runs after a failed idle wait, then a start in its place, waiting until it
// runs. Once decided it runs to the end apart from ctx, within
// stopTurnRestartBound: a caller that gave up between the stop and the start
// would leave the runtime stopped. Like restartAndSendLocked it does not
// re-read the hold, and it sends nothing into the new runtime. A stop that
// fails is an error wrapping runtime.ErrTurnNotEnded and no other sentinel:
// the interrupt was sent, its turn was not seen to end, and the runtime is
// as the stop left it. A start that fails once the runtime stopped is an
// error wrapping ErrRestartNotStarted and no other sentinel, the start's
// error in its text.
func (m *Manager) restartAfterFailedInterruptLocked(ctx context.Context, by Actor, id string, b beads.Bead, sessName string, r *StopTurnRestart) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTurnRestartBound)
	defer cancel()
	what := fmt.Sprintf("restarting session %s after its interrupted turn was not seen to end", id)
	if err := runtime.StopForCleanup(m.sp, sessName); err != nil {
		return TurnNotEndedError(what+": stopping it", err)
	}
	if err := m.restoreAfterHardRestartFailureLocked(rctx, by, id, b, sessName, r.ResumeCommand, r.Hints); err != nil {
		return fmt.Errorf("%s: %w (%s)", what, ErrRestartNotStarted, err.Error())
	}
	return nil
}

// prepareReplacementLocked is StopTurnOptions.ForReplacement's preparation,
// the steps interruptAndSubmitLocked runs between a settled interrupt and its
// message.
// A failure is an error wrapping ErrReplacementNotPrepared: the interrupt
// was sent and its turn settled.
func (m *Manager) prepareReplacementLocked(ctx context.Context, b beads.Bead, id, sessName string, mode runtime.TurnInterruptMode) error {
	if err := m.resetInterruptedTurnLocked(ctx, b, sessName); err != nil {
		return fmt.Errorf("session %s: %w: %w", id, ErrReplacementNotPrepared, err)
	}
	if shouldClearInterruptedInputBeforeSubmit(b) && mode == runtime.TurnInterruptKeystroke {
		if err := m.clearInterruptedInputLocked(ctx, b, sessName); err != nil {
			return fmt.Errorf("session %s: %w: %w", id, ErrReplacementNotPrepared, err)
		}
	}
	return nil
}

// ErrReplacementNotPrepared reports a stop for a replacement
// (StopTurnOptions.ForReplacement) whose interrupt was sent and whose turn
// settled, but whose preparation for the replacing message failed. Whatever
// else the error wraps (a fence miss on the input clear, say) is the
// preparation's, not the interrupt's: the interrupt was not "nothing sent".
var ErrReplacementNotPrepared = errors.New("the interrupted turn settled but the session could not be prepared for its replacement")

// ErrInterruptOutcomeUnknown reports an interrupt that was sent, or may have
// been, but whose outcome cannot be told: a checked stop's
// (StopTurnOptions.Still) send that failed after the check passed, a stop or
// a protocol turn interrupt (Manager.InterruptTurn) with a restart on failure
// whose caller's context ended before the interrupted turn was seen to end,
// or whose turn nothing showed still running (TurnEndUnreadError), a stop
// whose check refused its follow-up interrupt (no restart then), or a
// protocol turn interrupt whose read or dispatch failed (InterruptTurnOn).
// The error also wraps runtime.ErrRuntimeUnavailable. Unlike that sentinel
// alone, it is never "nothing sent": the interrupt may have ended the turn.
var ErrInterruptOutcomeUnknown = errors.New("the interrupt was sent, or may have been, and its outcome is unknown")

// ErrRestartNotStarted reports a restart on failure (StopTurnOptions.Restart,
// or Manager.InterruptTurn's) that stopped the runtime and could not start it
// again: the interrupted turn ended with the runtime, and the session has
// none until a start brings one back. The controller's heal of a committed
// row whose runtime is gone (SESS-531) marks the row asleep, which may reset
// its continuation; the controller's relaunch of a row it wants awake, or a
// later wake, is that start. The start's error is in the text only, so a
// caller matching sentinels never reads one of its sentinels (a session
// stopping, a runtime unavailable) as nothing sent.
var ErrRestartNotStarted = errors.New("the runtime was stopped and could not be started again")

// ErrStopNotSent reports a stop of a session's turn (Manager.StopTurnWith),
// or a protocol interrupt of one of its turns (Manager.InterruptTurn), that
// failed before it sent anything, for a cause it names no other sentinel
// for: its load of the session row under the session mutation lock failed,
// the row closed, gone, not a session or unreadable, or the actor of its
// restart on failure may not start the runtime. The cause is wrapped
// with its own sentinels (ErrSessionClosed, ErrNotSession, beads.ErrNotFound,
// ErrSweepStarts), so a caller that tells a row gone from one it could not
// read still can.
var ErrStopNotSent = errors.New("the stop failed before it sent anything")

// followUpRefusedError is a follow-up interrupt of the keystroke sequence
// that the stop's check (StopTurnOptions.Still) refused, so nothing more was
// sent. It names the refusal in its text and wraps none of it, since the
// first key was sent: a caller matching sentinels never reads the refusal's
// as nothing sent. StopTurnWith reads the refusal itself (followUpRefusal).
type followUpRefusedError struct {
	what    string
	refusal error
}

func (e *followUpRefusedError) Error() string {
	return fmt.Sprintf("%s: not sent, the turn check refused it (%s)", e.what, e.refusal.Error())
}

// followUpRefusal is the check's refusal when err carries a refused
// follow-up interrupt (followUpRefusedError), else nil.
func followUpRefusal(err error) error {
	var refused *followUpRefusedError
	if errors.As(err, &refused) {
		return refused.refusal
	}
	return nil
}

// errFollowUpNotSent marks a keystroke sequence's follow-up interrupt (a
// soft-escape family's Ctrl-C fallback, Gemini's immediate confirm) whose
// send failed with nothing before it that read the session busy: no wait ran
// first (Gemini's confirm), the fallback wait read nothing
// (waitReadNothing), or the send found another incarnation
// (runtime.ErrFenceMismatch), of which a reading of the interrupted one says
// nothing. The failed send read nothing either, so the stop has no evidence
// the turn still runs (restartWithheld).
var errFollowUpNotSent = errors.New("the follow-up interrupt was not sent")

// followUpSendError is the error of a keystroke sequence's follow-up
// interrupt, what, whose send failed with err; waited is the error of the
// idle wait before it, nil when none ran. It wraps errFollowUpNotSent unless
// that wait read the session busy and the send found no other incarnation:
// nothing more was sent, so that reading stands as the stop's last.
func followUpSendError(what string, waited, err error) error {
	if waited != nil && !waitReadNothing(waited) && !errors.Is(err, runtime.ErrFenceMismatch) {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: %w: %w", what, errFollowUpNotSent, err)
}

// stopTurnRestartBound bounds a restart after a failed interrupt
// (StopTurnOptions.Restart) once it is decided; it runs apart from the
// caller's context.
const stopTurnRestartBound = 2 * time.Minute

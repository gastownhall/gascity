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
	// sequence; an error from it is returned, wrapped, and nothing more is
	// sent.
	Still func() error
	// Settle, when set, decides whether the interrupted turn ended, still
	// under the session mutation lock: it gets the error of the stop's own
	// waits (nil when they passed, else an error wrapping
	// runtime.ErrTurnNotEnded) and returns nil when the turn ended, else an
	// error wrapping runtime.ErrTurnNotEnded. It runs only once something
	// was sent. Unset, the stop's own waits decide.
	Settle func(waitErr error) error
	// ForReplacement prepares the session for the message that replaces the
	// interrupted turn, as deliver-now does between its interrupt and its
	// message (X11 D11): once the turn settled it discards the interrupted
	// turn where the family needs that (Gemini's reset), and on a keystroke
	// runtime clears the input box for the message where the family leaves
	// input in it (Claude, Gemini). Without it a stop only clears a draft
	// Claude restores, best effort. A failed preparation is an error
	// wrapping ErrReplacementNotPrepared, the interrupt already settled.
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
	// runtime.ErrRuntimeUnavailable, and not ErrTurnNotEnded;
	// a restart once decided runs apart from ctx, within
	// stopTurnRestartBound.
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
// and takes the runtime lease under the session mutation lock before it
// sends anything: a lease it cannot take is an error wrapping
// runtime.ErrRuntimeUnavailable, nothing sent. A ctx with a deadline, or a
// Still check, bounds the wait for the session mutation lock by ctx: a lock
// still held when ctx ends is an error wrapping
// runtime.ErrRuntimeUnavailable and ErrSessionMutationBusy, nothing sent. A
// restart that fails is an
// error wrapping runtime.ErrTurnNotEnded and no other sentinel, its cause in
// its text: the interrupt was sent and the turn was not seen to end.
func (m *Manager) StopTurnWith(ctx context.Context, id string, opts StopTurnOptions) (StopTurnReport, error) {
	if opts.Restart != nil {
		if err := m.checkStarter(opts.Restart.By); err != nil {
			return StopTurnReport{}, fmt.Errorf("stopping the turn of %s, restarting it on failure: %w", id, err)
		}
	}
	still := opts.Still
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
			return err
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
		if none, err := m.stopFindsNoTurn(ctx, b, sessName, still != nil); none {
			// No runtime means no turn to interrupt: stopTurnLocked sends
			// nothing, so there is nothing to wait for either. A fenced or
			// checked stop reports what its fresh read found: never
			// success into none, nor no turn from a stale cache.
			return err
		}
		if still != nil {
			if err := still(); err != nil {
				return fmt.Errorf("stopping the turn of %s: %w", id, err)
			}
		}
		mode := runtime.TurnInterruptModeFor(m.sp, sessName)
		restart := opts.Restart != nil && mode == runtime.TurnInterruptKeystroke
		var by Actor
		if restart {
			// Every interrupt that may end in a stop and a restart takes the
			// runtime lease before it sends anything, and carries it to the
			// restart, as interrupt_now does (D8D-5).
			leased, release, err := m.leaseForRestartLocked(ctx, opts.Restart.By, id, sessName)
			if err != nil {
				return fmt.Errorf("stopping the turn of %s: taking the runtime lease for its restart on failure, nothing sent: %w: %w", id, runtime.ErrRuntimeUnavailable, err)
			}
			defer release()
			by = leased
		}
		interruptStartedAt := time.Now()
		none, err := m.sendTurnInterruptLocked(ctx, b, sessName, mode, still != nil)
		switch {
		case still == nil && (none || err != nil):
			// Nothing was sent (the runtime gone at the keys, a miss), or
			// the send failed: StopTurn returns that and waits for nothing.
			return err
		case none && err == nil:
			return fmt.Errorf("stopping the turn of %s: the runtime or its agent stopped after the check, nothing sent: %w", id, runtime.ErrSessionNotFound)
		case none, errors.Is(err, runtime.ErrFenceMismatch), errors.Is(err, runtime.ErrInputFenceUnsupported),
			errors.Is(err, ErrTurnInterruptUnsupported), errors.Is(err, runtime.ErrRuntimeUnavailable):
			// Nothing was sent (a miss, a refusal), or the runtime's own
			// error says it could not tell, as a fenced send's liveness
			// read that cannot answer before its key does.
			return err
		case err != nil:
			return fmt.Errorf("stopping the turn of %s: %w: %w: %w", id, ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, err)
		}
		waitErr := m.waitForInterruptSettleLocked(ctx, b, sessName, mode, still, interruptStartedAt)
		if opts.Settle != nil {
			waitErr = opts.Settle(waitErr)
		}
		if waitErr != nil {
			if !restart || !errors.Is(waitErr, runtime.ErrTurnNotEnded) {
				return waitErr
			}
			if cerr := ctx.Err(); cerr != nil {
				// The caller gave up (a city stop, its lease budget): its
				// end reads as a turn not seen to end, which is no reason
				// to stop the runtime. Nothing more is sent.
				return fmt.Errorf("stopping the turn of %s: the caller's context ended before its interrupted turn was seen to end, not restarting it: %w: %w: %w (%s)", id, ErrInterruptOutcomeUnknown, runtime.ErrRuntimeUnavailable, cerr, waitErr.Error())
			}
			// A restart once decided runs to the end under its own bound: a
			// caller that gives up between the stop and the start would
			// leave the runtime stopped.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTurnRestartBound)
			defer cancel()
			if err := m.restartAfterFailedInterruptLocked(rctx, by, id, b, sessName, opts.Restart); err != nil {
				return TurnNotEndedError(fmt.Sprintf("restarting session %s after its interrupted turn was not seen to end", id), err)
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
	if err := m.waitForInterruptIdleLocked(ctx, b, sessName, mode, still); err != nil {
		return fmt.Errorf("waiting for stopped session to become idle: %w: %w", runtime.ErrTurnNotEnded, err)
	}
	if err := m.waitForInterruptBoundaryLocked(ctx, b, sessName, since); err != nil {
		return fmt.Errorf("waiting for stopped session interrupt boundary: %w: %w", runtime.ErrTurnNotEnded, err)
	}
	return nil
}

// restartAfterFailedInterruptLocked replaces the runtime whose interrupted
// turn did not settle, under the lease by carries: the stop interrupt_now
// runs after a failed idle wait, then a start in its place, waiting until it
// runs. Like restartAndSendLocked it does not re-read the hold, and it sends
// nothing into the new runtime.
func (m *Manager) restartAfterFailedInterruptLocked(ctx context.Context, by Actor, id string, b beads.Bead, sessName string, r *StopTurnRestart) error {
	if err := runtime.StopForCleanup(m.sp, sessName); err != nil {
		return fmt.Errorf("stopping session after its interrupt did not settle: %w", err)
	}
	return m.restoreAfterHardRestartFailureLocked(ctx, by, id, b, sessName, r.ResumeCommand, r.Hints)
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

// ErrInterruptOutcomeUnknown reports a stop whose interrupt was sent, or may
// have been, but whose outcome it cannot tell: a checked stop's
// (StopTurnOptions.Still) send that failed after the check passed, or a stop
// with a restart on failure whose caller's context ended before the
// interrupted turn was seen to end (no restart then). The
// error also wraps runtime.ErrRuntimeUnavailable. Unlike that sentinel
// alone, it is never "nothing sent": the interrupt may have ended the turn.
var ErrInterruptOutcomeUnknown = errors.New("the interrupt was sent but its outcome is unknown")

// stopTurnRestartBound bounds a restart after a failed interrupt
// (StopTurnOptions.Restart) once it is decided; it runs apart from the
// caller's context.
const stopTurnRestartBound = 2 * time.Minute

package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// TurnInterruptOutcome is what a turn-fenced deliver-now did about the turn
// it targeted (SESSION-RUNTIME-020).
type TurnInterruptOutcome string

const (
	// TurnInterruptSettled means the interrupt was sent and the targeted turn ended.
	TurnInterruptSettled TurnInterruptOutcome = "settled"
	// TurnInterruptNoTurn means no turn ran at the sighting or the fence read;
	// nothing was sent.
	TurnInterruptNoTurn TurnInterruptOutcome = "no_turn"
	// TurnInterruptStale means the sighted turn ended or was replaced, its agent is
	// down, or a turn the request did not see runs; nothing was sent.
	TurnInterruptStale TurnInterruptOutcome = "stale"
	// TurnInterruptUnsettled means the interrupt was sent but the turn was not
	// seen to end within the settle bound.
	TurnInterruptUnsettled TurnInterruptOutcome = "unsettled"
	// TurnInterruptUnavailable means a read or the interrupt dispatch failed;
	// nothing is known to have been sent.
	TurnInterruptUnavailable TurnInterruptOutcome = "unavailable"
)

// DegradationUnfencedInterrupt marks an interrupt the runtime accepted
// without fencing it to the named turn (runtime.TurnInterrupt.Fenced false).
const DegradationUnfencedInterrupt = "unfenced_interrupt"

// TurnInterruptReport is what a turn-fenced deliver-now did about the turn.
type TurnInterruptReport struct {
	Outcome TurnInterruptOutcome
	// TurnID is the sighted turn; empty when none ran or the sighting was
	// unusable.
	TurnID string
	// Degradation is DegradationUnfencedInterrupt exactly when InterruptTurn
	// succeeded with Fenced false; empty otherwise.
	Degradation string
}

const (
	// turnFenceReadTimeout bounds each turn read and the interrupt dispatch.
	turnFenceReadTimeout = 10 * time.Second
	// turnSettleTimeout bounds the wait for the interrupted turn to end.
	turnSettleTimeout = 15 * time.Second
)

// Message dispositions of a turn-fenced deliver-now (telemetry label values).
const (
	turnInterruptMessageDelivered = "delivered"
	turnInterruptMessageQueued    = "queued"   // follow-up queue of a live row, next boundary
	turnInterruptMessageDeferred  = "deferred" // queued for a wake: by the send (held actor, dead agent), or at the boundary on an asleep or drained row
	turnInterruptMessageFailed    = "failed"
)

// turnSighting is deliver-now's read of the session's turn, taken before the
// session mutation lock and the runtime lease, so lock waits and lease
// retries fall after it.
type turnSighting struct {
	// unsupported: a composite routes the session to a backend without the
	// capability, so the submit keeps the unfenced protocol path.
	unsupported bool
	taken       bool
	name        string
	state       runtime.TurnState
	err         error
}

// usableFor reports whether the sighting read sessName's turn successfully.
func (s turnSighting) usableFor(sessName string) bool {
	return s.taken && s.err == nil && s.name == sessName
}

// sightTurn reads session id's turn for a deliver-now when its runtime
// reports turns. A provider without the capability sees no call at all.
func (m *Manager) sightTurn(ctx context.Context, id string) turnSighting {
	tf, ok := m.sp.(runtime.TurnFenceProvider)
	if !ok {
		return turnSighting{}
	}
	b, sessName, err := m.sessionBead(id)
	if err != nil || State(b.Metadata["state"]) == StateSuspended || !supportsInterruptNowForMetadata(b.Metadata) ||
		runtime.TurnInterruptModeFor(m.sp, sessName) != runtime.TurnInterruptProtocol {
		return turnSighting{}
	}
	st, err := observeTurn(ctx, tf, sessName)
	if errors.Is(err, runtime.ErrInteractionUnsupported) {
		return turnSighting{unsupported: true}
	}
	return turnSighting{taken: true, name: sessName, state: st, err: err}
}

// turnFenceFor returns the runtime's turn fence when a deliver-now on b takes
// the fenced path: an unsuspended protocol-mode session whose runtime reports
// turns and does not answer as unsupported. Otherwise nil. A sighting that
// was skipped or read another runtime name says nothing about the
// capability, so a row the fenced path would not refuse has it read here,
// under the session mutation lock: a composite may route sessName to a
// backend without it. The fence does not reuse that read: it reads the turn
// again once it holds the runtime lease, which it may have waited for.
func (m *Manager) turnFenceFor(ctx context.Context, by Actor, id string, b beads.Bead, sessName string, seen turnSighting) runtime.TurnFenceProvider {
	if seen.unsupported || State(b.Metadata["state"]) == StateSuspended ||
		runtime.TurnInterruptModeFor(m.sp, sessName) != runtime.TurnInterruptProtocol {
		return nil
	}
	tf, ok := m.sp.(runtime.TurnFenceProvider)
	if !ok {
		return nil
	}
	if (!seen.taken || seen.name != sessName) && m.startRefusal(by, id, b) == nil {
		if _, err := observeTurn(ctx, tf, sessName); errors.Is(err, runtime.ErrInteractionUnsupported) {
			return nil
		}
	}
	return tf
}

// fencedInterruptAndSubmitLocked is deliver-now on a runtime that reports
// turns (SESSION-RUNTIME-020). Under the session mutation lock and the
// runtime lease it interrupts at most the sighted turn, waits for it to end,
// and delivers the message through the send path, or queues it for the next
// boundary when a turn may still run.
func (m *Manager) fencedInterruptAndSubmitLocked(ctx context.Context, by Actor, id string, b beads.Bead,
	sessName, message, resumeCommand string, hints runtime.Config, tf runtime.TurnFenceProvider, seen turnSighting,
) (SubmitOutcome, error) {
	if err := m.startRefusal(by, id, b); err != nil {
		return SubmitOutcome{}, err
	}
	by, release, err := m.leaseForRestartLocked(ctx, by, id, sessName)
	if err != nil {
		return SubmitOutcome{}, err
	}
	defer release()
	report, now := fenceTurn(ctx, tf, sessName, seen, func() (bool, error) {
		live, err := m.freshLiveness(sessName, hints.ProcessNames)
		return err == nil && live.Running && live.Alive, err
	})
	out := SubmitOutcome{Interrupt: &report}
	if now {
		queued, sendErr := m.sendLocked(ctx, by, id, b, sessName, message, resumeCommand, hints, true, nudgequeue.SourceSubmit)
		// A send the runtime refused as unavailable sent nothing, as
		// runtime.TurnFenceProvider requires: queue it.
		if !errors.Is(sendErr, runtime.ErrRuntimeUnavailable) {
			out.Queued, out.Deferred = queued, queued
			recordTurnInterrupt(ctx, sessName, report, out, sendErr)
			return out, sendErr
		}
	}
	if err := m.enqueueDeferredSubmitLocked(b, sessName, message, nudgequeue.SourceSubmit); err != nil {
		recordTurnInterrupt(ctx, sessName, report, out, err)
		return out, err
	}
	// The boundary queue. The follow-up poller delivers only to a live
	// session, so on a dormant row the caller must have the controller wake
	// it: Deferred. A live row's next boundary delivers it.
	out.Queued = true
	out.Deferred = dormantState(State(b.Metadata["state"]))
	recordTurnInterrupt(ctx, sessName, report, out, nil)
	return out, nil
}

// dormantState reports whether a row in state needs a wake to run: asleep or
// drained, the rows a wake request applies to.
func dormantState(state State) bool {
	return state == StateAsleep || state == StateDrained
}

// fenceTurn is deliver-now's fence. It interrupts only the sighted turn, and
// only when the fence read names it and alive finds the agent up. now
// reports that the message may be sent: the last read showed no turn in
// progress, or the agent is not live (the send queues it then).
func fenceTurn(ctx context.Context, tf runtime.TurnFenceProvider, sessName string, seen turnSighting,
	alive func() (bool, error),
) (TurnInterruptReport, bool) {
	if !seen.usableFor(sessName) { // failed, skipped or renamed: never interrupts
		cur, err := observeTurn(ctx, tf, sessName)
		return TurnInterruptReport{Outcome: TurnInterruptUnavailable}, err == nil && !cur.Active()
	}
	if !seen.state.Active() {
		cur, err := observeTurn(ctx, tf, sessName)
		switch {
		case err != nil:
			return TurnInterruptReport{Outcome: TurnInterruptUnavailable}, false
		case cur.Active():
			return TurnInterruptReport{Outcome: TurnInterruptStale}, false
		}
		return TurnInterruptReport{Outcome: TurnInterruptNoTurn}, true
	}
	r := TurnInterruptReport{TurnID: seen.state.TurnID}
	switch up, err := alive(); {
	case err != nil:
		r.Outcome = TurnInterruptUnavailable
		return r, false
	case !up: // a dead agent gets no interrupt; the send queues its message
		r.Outcome = TurnInterruptStale
		return r, true
	}
	ack, err := interruptTurn(ctx, tf, sessName, seen.state.TurnID)
	if err != nil {
		var nc *runtime.TurnNotCurrentError
		switch {
		case errors.As(err, &nc):
			r.Outcome = TurnInterruptStale
			return r, !nc.Current.Active()
		case errors.Is(err, runtime.ErrTurnNotCurrent): // the current turn is unreported
			r.Outcome = TurnInterruptStale
			return r, false
		case errors.Is(err, runtime.ErrSessionNotFound) && !errors.Is(err, runtime.ErrRuntimeUnavailable):
			// Gone before any dispatch. Gone after a failed attempt (both
			// sentinels) may have reached the runtime: unavailable below.
			r.Outcome = TurnInterruptStale
			return r, true
		}
		r.Outcome = TurnInterruptUnavailable // unknown whether it reached the runtime
		return r, false
	}
	if !ack.Fenced {
		r.Degradation = DegradationUnfencedInterrupt
	}
	final, err := tf.WaitTurnEnded(ctx, sessName, seen.state.TurnID, turnSettleTimeout)
	if err != nil {
		r.Outcome = TurnInterruptUnsettled
		return r, false
	}
	r.Outcome = TurnInterruptSettled
	return r, !final.Active()
}

// observeTurn reads sessName's turn within turnFenceReadTimeout. No session
// means no turn: the send starts it.
func observeTurn(ctx context.Context, tf runtime.TurnFenceProvider, sessName string) (runtime.TurnState, error) {
	readCtx, cancel := context.WithTimeout(ctx, turnFenceReadTimeout)
	defer cancel()
	st, err := tf.ObserveTurn(readCtx, sessName)
	if errors.Is(err, runtime.ErrSessionNotFound) {
		return runtime.TurnState{}, nil
	}
	return st, err
}

// interruptTurn is InterruptTurn, its read and dispatch within
// turnFenceReadTimeout.
func interruptTurn(ctx context.Context, tf runtime.TurnFenceProvider, sessName, turnID string) (runtime.TurnInterrupt, error) {
	readCtx, cancel := context.WithTimeout(ctx, turnFenceReadTimeout)
	defer cancel()
	return tf.InterruptTurn(readCtx, sessName, turnID)
}

// recordTurnInterrupt logs and counts one turn-fenced deliver-now. It records
// no message text.
func recordTurnInterrupt(ctx context.Context, sessName string, r TurnInterruptReport, out SubmitOutcome, err error) {
	disposition := turnInterruptDisposition(out, err)
	degraded, last := "", disposition
	if r.Degradation != "" {
		degraded = ", " + r.Degradation
	}
	if err != nil {
		last = fmt.Sprintf("%s: %v", disposition, err)
	}
	log.Printf("session: deliver now for %q: interrupt %s (turn %q)%s; message %s", sessName, r.Outcome, r.TurnID, degraded, last)
	recordCtx := ctx
	if recordCtx == nil || recordCtx.Err() != nil {
		recordCtx = context.Background()
	}
	telemetry.RecordTurnInterrupt(recordCtx, sessName, r.TurnID, string(r.Outcome), r.Degradation, disposition, err)
}

// turnInterruptDisposition names what became of a fenced deliver-now's
// message.
func turnInterruptDisposition(out SubmitOutcome, err error) string {
	switch {
	case err != nil:
		return turnInterruptMessageFailed
	case out.Deferred:
		return turnInterruptMessageDeferred
	case out.Queued:
		return turnInterruptMessageQueued
	}
	return turnInterruptMessageDelivered
}

package session

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

var (
	// ErrResumeHeld reports that the actor may not resume a held session
	// (CONTRACT v5.9 D8 rule 1). Nothing was started or written.
	ErrResumeHeld = errors.New("session is held; not resumed")
	// ErrAgentDead reports a runtime whose pane is up but whose agent is
	// dead, on a fresh read: a crash the controller has not healed yet. Text
	// delivered into it would be lost with the pane, so a send queues it for
	// the incarnation the heal brings up (mc-48az4, mc-wdjni). A fresh read
	// that fails reports it too: a delay costs less than lost text.
	ErrAgentDead = errors.New("session's agent is dead; its pane is up until the controller heals it")
	// ErrWakeRequestContended reports a wake request whose CAS lost on every
	// attempt; nothing was written. Retry.
	ErrWakeRequestContended = errors.New("wake request lost to concurrent writes; retry")
)

// WakeRequestOutcome is what RequestWakeUnlessHeld did.
type WakeRequestOutcome int

const (
	// WakeRecorded means an explicit wake is recorded on the row: written now,
	// or already pending and unserved (coalesced) so nothing was written.
	WakeRecorded WakeRequestOutcome = iota + 1
	// WakeHeld means an operator holds the row (HoldVerdict); nothing was
	// written.
	WakeHeld
	// WakeNotDormant means the row is not asleep or drained (it is
	// starting, running or closed), so it needs no wake; nothing was written.
	WakeNotDormant
)

// queueFor reports whether by may not start or resume b's row now: an
// operator always may; ActorBackground may not on any row whose runtime is not
// running (the API never starts a runtime in the controller's process) or
// that is held (a managed suspend whose runtime the controller has not
// stopped yet must not take a send that flips it active); any other actor
// may not on a held one (HoldVerdict).
func (m *Manager) queueFor(meta map[string]string, sessName string, by Actor) bool {
	return m.queueForRunning(meta, m.sp.IsRunning(sessName), by)
}

// queueForRunning is queueFor with whether the runtime runs already read
// (ensureRunningOn: a delivery's fresh read, not a caching provider's).
func (m *Manager) queueForRunning(meta map[string]string, running bool, by Actor) bool {
	switch by.Kind {
	case ActorOperator:
		return false
	case ActorBackground:
		return !running || HoldVerdict(meta, running, m.now())
	}
	return HoldVerdict(meta, running, m.now())
}

// ExplicitWakeUnserved reports whether a row's explicit wake request is still
// pending by its timestamps: wake_request is explicit and the request was not
// served by a later sleep. A request is served only when wake_requested_at and
// slept_at both parse as RFC3339 and the request is strictly before the
// sleep; a missing or unparseable timestamp, or equal ones, leave it
// unserved. It does not look at the row's state; callers that care whether
// the row is already awake (an active or awake row's wake is served) check
// that themselves.
func ExplicitWakeUnserved(info Info) bool {
	if strings.TrimSpace(info.WakeRequest) != string(WakeCauseExplicit) {
		return false
	}
	requestedAt, reqErr := time.Parse(time.RFC3339, strings.TrimSpace(info.WakeRequestedAt))
	sleptAt, sleptErr := time.Parse(time.RFC3339, strings.TrimSpace(info.SleptAt))
	return reqErr != nil || sleptErr != nil || !requestedAt.Before(sleptAt)
}

// wakeVerdict is RequestWakeUnlessHeld's decision on one read of a row: a
// lifecycle conflict, or an outcome plus whether the request coalesces onto an
// already pending wake (nothing to write).
type wakeVerdict struct {
	conflict  error
	outcome   WakeRequestOutcome
	coalesces bool
}

func decideWakeRequest(id string, info Info, persisted PersistedResponse, runtimeRunning bool, now time.Time) wakeVerdict {
	input := LifecycleInputFromMetadata(persisted.Status, persisted.Metadata)
	input.Now = now
	if conflictState, refused := lifecycleWakeConflictState(ProjectLifecycle(input)); refused {
		return wakeVerdict{conflict: &WakeConflictError{SessionID: id, State: conflictState}}
	}
	state := State(strings.TrimSpace(persisted.Metadata["state"]))
	switch {
	case HoldVerdict(persisted.Metadata, runtimeRunning, now):
		return wakeVerdict{outcome: WakeHeld}
	case info.Closed || (state != StateAsleep && state != StateDrained):
		return wakeVerdict{outcome: WakeNotDormant}
	}
	return wakeVerdict{outcome: WakeRecorded, coalesces: ExplicitWakeUnserved(info)}
}

// RequestWakeUnlessHeld records an explicit wake by CAS on an open asleep or
// drained row that nothing holds (D8 rule 1). A row that already carries an
// unserved explicit wake (ExplicitWakeUnserved) is not written again: the
// request coalesces onto the pending one and reports WakeRecorded, so many
// senders waking one session do not contend on its row. A row whose lifecycle
// refuses a wake returns *WakeConflictError, as WakeSession does; a CAS lost
// on every attempt returns ErrWakeRequestContended.
//
// Coalescing is the one WakeRecorded verdict that skips the CAS, so it is
// never taken on the front door's read alone: a cached store can lag another
// process's write (an operator suspend, or the owner consuming the wake and
// sleeping again). The verdict is confirmed on a live read of the row, and a
// failed live read is returned as an error. When the live row decides
// otherwise that decision stands; where the store fences writes, a write it
// needs goes through the CAS at the stale revision, which fails, refreshes the
// cache and retries. The held, not-dormant and conflict verdicts rest on the
// front door's read.
func (s *Store) RequestWakeUnlessHeld(id string, runtimeRunning bool, now time.Time) (WakeRequestOutcome, error) {
	var verdict wakeVerdict
	var liveErr error
	ok, err := s.UpdateMetadataFenced(id, 3, func(info Info, persisted PersistedResponse) MetadataPatch {
		liveErr = nil
		verdict = decideWakeRequest(id, info, persisted, runtimeRunning, now)
		if verdict.coalesces {
			live, err := s.freshBead(id)
			if err != nil {
				liveErr = fmt.Errorf("confirming wake request on session %q: %w", id, err)
				return nil
			}
			verdict = decideWakeRequest(id, infoFromPersistedBead(live), PersistedResponseFromBead(live), runtimeRunning, now)
		}
		if verdict.conflict != nil || verdict.outcome != WakeRecorded || verdict.coalesces {
			return nil
		}
		return RequestExplicitWakePatch(string(WakeCauseExplicit), now)
	})
	switch {
	case err != nil:
		return 0, err
	case liveErr != nil:
		return 0, liveErr
	case verdict.conflict != nil:
		return 0, verdict.conflict
	case verdict.outcome == WakeRecorded && !ok && !verdict.coalesces:
		return 0, fmt.Errorf("%w: %s", ErrWakeRequestContended, id)
	}
	return verdict.outcome, nil
}

// ResumeHeld is an operator's resume that starts nothing itself: resume:
// true on a message the conversation lane delivers, proposed at notAfter. On
// a fresh read it consumes the row's operator suspend (the disposition's
// Resume cell by an operator) and moves a suspended row asleep, as a wake
// does, and, in the same write, records an explicit wake when the moved row
// is dormant and nothing else holds it, as RequestWakeUnlessHeld would
// record it: the suspend alone kept the row, so a controller pass must never
// read it unheld and unwoken, which drains and closes a pool row. The lane's
// own wake request then coalesces onto that wake. It does nothing else: it
// cancels no wait and withdraws none of its nudges, and a wait's hold, a
// quarantine and a heartbeat stand, a row they hold getting no wake.
//
// It consumes only a suspend stamped at or before notAfter, one in force
// when the message was proposed. A suspend stamped later (an operator's,
// placed while the message waits, or one stamped again) stands: the resume
// neither consumes it nor refuses for it, so a same-key retry of the message
// cannot undo it. suspended_at is whole seconds, so a suspend stamped in
// notAfter's own second counts as before it, and one with no readable stamp
// predates the writers that stamp one.
//
// A row with no suspend it may consume is not written. That verdict skips
// the CAS, so it is confirmed on a live read of the row, as
// RequestWakeUnlessHeld confirms its coalesce: a cached store can lag
// another process's suspend. A failed live read is its error. A row whose
// lifecycle refuses a wake (closed, closing, archived) is a
// *WakeConflictError, as WakeSession returns, and a suspended row whose
// drain is stopping it wraps ErrSessionStopping; neither is written, and
// ResumeHeldRefusal answers both without a write. A CAS lost on every
// attempt wraps ErrWakeRequestContended.
func (s *Store) ResumeHeld(id string, notAfter, now time.Time) error {
	var refused error
	var decided bool
	ok, err := s.UpdateMetadataFenced(id, 3, func(info Info, persisted PersistedResponse) MetadataPatch {
		patch, refusal := resumeHeldPatch(id, info, persisted, notAfter, now)
		if len(patch) == 0 && refusal == nil {
			patch, refusal = s.liveResumeHeldPatch(id, notAfter, now)
		}
		refused, decided = refusal, len(patch) > 0
		return patch
	})
	switch {
	case err != nil:
		return err
	case refused != nil:
		return refused
	case decided && !ok:
		return fmt.Errorf("%w: %s", ErrWakeRequestContended, id)
	}
	return nil
}

// liveResumeHeldPatch is resumeHeldPatch on a live read of row id, past any
// cache. When it decides a write the cached read did not, the write goes
// through the CAS at the cached read's revision, which fails where the store
// fences writes, and the next attempt decides on a refreshed read.
func (s *Store) liveResumeHeldPatch(id string, notAfter, now time.Time) (MetadataPatch, error) {
	live, err := s.freshBead(id)
	if err != nil {
		return nil, fmt.Errorf("confirming resume on session %q: %w", id, err)
	}
	return resumeHeldPatch(id, infoFromPersistedBead(live), PersistedResponseFromBead(live), notAfter, now)
}

// resumeHeldPatch is ResumeHeld's decision on one read of row id: its
// refusal (resumeHeldRefusal), or the patch that consumes the row's suspend
// and records the wake the moved row needs, nil when the row carries no
// suspend stamped at or before notAfter for it to consume.
func resumeHeldPatch(id string, info Info, persisted PersistedResponse, notAfter, now time.Time) (MetadataPatch, error) {
	if err := resumeHeldRefusal(id, info, persisted, notAfter, now); err != nil {
		return nil, err
	}
	if suspendedAfter(DecodeDisposition(persisted.Metadata), notAfter) {
		return nil, nil
	}
	patch, _, err := MoveDisposition(persisted.Metadata, info, ActorOperator, DispEvent{Kind: EvResume}, resumeHeldLife(now), now)
	if err != nil || len(patch) == 0 {
		return nil, err
	}
	moved := PersistedResponse{Status: persisted.Status, Metadata: patch.Apply(persisted.Metadata)}
	if v := decideWakeRequest(id, info.ApplyPatch(patch), moved, false, now); v.outcome == WakeRecorded && !v.coalesces {
		maps.Copy(patch, RequestExplicitWakePatch(string(WakeCauseExplicit), now))
	}
	return patch, nil
}

// suspendedAfter reports whether d's suspend, an operator's or a soft one,
// is stamped after t.
func suspendedAfter(d Disposition, t time.Time) bool {
	return d.Operator != OperatorNone && d.Since.After(t)
}

// ResumeHeldRefusal is the refusal ResumeHeld would answer for row id now,
// for a message proposed now, decided as ResumeHeld decides it but on one
// read, writing nothing: a *WakeConflictError for a closed, closing or
// archived row, an ErrSessionStopping wrap for a suspended row whose drain
// is stopping it, else nil. A producer that writes what the resume must
// follow (the lane's message op) checks it first, so a refusal the row
// alone decides writes nothing. ResumeHeld still decides on its own fresh
// read, so a row that changes after this read is refused there. A read that
// fails is its error.
func (s *Store) ResumeHeldRefusal(id string, now time.Time) error {
	b, err := s.validatedBead(id)
	if err != nil {
		return err
	}
	return resumeHeldRefusal(id, infoFromPersistedBead(b), PersistedResponseFromBead(b), now, now)
}

// resumeHeldRefusal is ResumeHeld's refusal of one read of row id, or nil
// when the operator's Resume cell may move it or the row's suspend is
// stamped after notAfter (which the resume leaves standing): a lifecycle
// that refuses a wake is a *WakeConflictError, as WakeSession returns it,
// and the cell's own refusal (a suspended row whose drain owns the runtime)
// wraps ErrSessionStopping.
func resumeHeldRefusal(id string, info Info, persisted PersistedResponse, notAfter, now time.Time) error {
	input := LifecycleInputFromMetadata(persisted.Status, persisted.Metadata)
	input.Now = now
	if state, conflict := lifecycleWakeConflictState(ProjectLifecycle(input)); conflict {
		return &WakeConflictError{SessionID: id, State: state}
	}
	d := DecodeDisposition(persisted.Metadata)
	if suspendedAfter(d, notAfter) {
		return nil
	}
	_, err := d.Apply(ActorOperator, DispEvent{Kind: EvResume}, State(info.MetadataState), now)
	var cell *ErrDispositionRefused
	if errors.As(err, &cell) {
		return fmt.Errorf("%w: %s: %s", ErrSessionStopping, id, cell.Why)
	}
	return err
}

// resumeHeldLife is the lifecycle ResumeHeld writes beside its Resume cell: a
// suspended row goes asleep, slept now, as ClearWakeBlockersPatch moves it on
// a wake, and the operator's own hold reason goes with the hold. Any other
// row keeps its lifecycle.
func resumeHeldLife(now time.Time) DispositionLife {
	return func(_, _ Disposition, fresh Info) MetadataPatch {
		patch := ClearExpiredHoldPatch(fresh.SleepReason)
		if State(strings.TrimSpace(fresh.MetadataState)) == StateSuspended {
			patch["state"] = string(StateAsleep)
			patch["slept_at"] = now.UTC().Format(time.RFC3339)
		}
		return patch
	}
}

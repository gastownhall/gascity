package session

import (
	"errors"
	"fmt"
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

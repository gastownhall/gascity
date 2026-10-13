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
	running := m.sp.IsRunning(sessName)
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

// RequestWakeUnlessHeld records an explicit wake by CAS on an open asleep or
// drained row that nothing holds (D8 rule 1). A row that already carries an
// unserved explicit wake (ExplicitWakeUnserved) is not written again: the
// request coalesces onto the pending one and reports WakeRecorded, so many
// senders waking one session do not contend on its row. A row whose lifecycle
// refuses a wake returns *WakeConflictError, as WakeSession does; a CAS lost
// on every attempt returns ErrWakeRequestContended.
func (s *Store) RequestWakeUnlessHeld(id string, runtimeRunning bool, now time.Time) (WakeRequestOutcome, error) {
	var outcome WakeRequestOutcome
	var conflict error
	var coalesced bool
	ok, err := s.UpdateMetadataFenced(id, 3, func(info Info, persisted PersistedResponse) MetadataPatch {
		input := LifecycleInputFromMetadata(persisted.Status, persisted.Metadata)
		input.Now = now
		conflict, outcome, coalesced = nil, WakeNotDormant, false
		state := State(strings.TrimSpace(persisted.Metadata["state"]))
		if conflictState, refused := lifecycleWakeConflictState(ProjectLifecycle(input)); refused {
			conflict = &WakeConflictError{SessionID: id, State: conflictState}
			return nil
		}
		switch {
		case HoldVerdict(persisted.Metadata, runtimeRunning, now):
			outcome = WakeHeld
			return nil
		case info.Closed || (state != StateAsleep && state != StateDrained):
			return nil
		}
		outcome = WakeRecorded
		if ExplicitWakeUnserved(info) {
			coalesced = true
			return nil
		}
		return RequestExplicitWakePatch(string(WakeCauseExplicit), now)
	})
	switch {
	case err != nil:
		return 0, err
	case conflict != nil:
		return 0, conflict
	case outcome == WakeRecorded && !ok && !coalesced:
		return 0, fmt.Errorf("%w: %s", ErrWakeRequestContended, id)
	}
	return outcome, nil
}

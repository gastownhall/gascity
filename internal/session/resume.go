package session

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ResumePolicy says whether a Manager entry that can start a runtime may
// resume a row an operator holds (CONTRACT v5.9 D8 rule 1). The zero value is
// ResumeIfUnheld, so a caller that names no policy never consumes a hold.
type ResumePolicy int

const (
	// ResumeIfUnheld starts or resumes only a row nothing holds; on a held
	// row the message is queued and nothing is started.
	ResumeIfUnheld ResumePolicy = iota
	// ResumeOperator is an operator's own resume (Attach, `gc session
	// submit`, an API request carrying resume: true). It consumes the hold.
	ResumeOperator
	// ResumeViaController is a background caller beside a controller (the
	// API without resume: true): it never starts a dormant row itself, held
	// or not. The message queues, and the caller records the wake for the
	// controller (Store.RequestWakeUnlessHeld) and pokes it.
	ResumeViaController
)

var (
	// ErrResumeHeld reports that the policy did not let the call resume a
	// held session. Nothing was started or written.
	ErrResumeHeld = errors.New("session is held; not resumed")
	// ErrWakeRequestContended reports a wake request whose CAS lost on every
	// attempt; nothing was written. Retry.
	ErrWakeRequestContended = errors.New("wake request lost to concurrent writes; retry")
)

// WakeRequestOutcome is what RequestWakeUnlessHeld did.
type WakeRequestOutcome int

const (
	// WakeRecorded means an explicit wake was written.
	WakeRecorded WakeRequestOutcome = iota + 1
	// WakeHeld means an operator holds the row (HoldVerdict); nothing was
	// written.
	WakeHeld
	// WakeNotDormant means the row is not asleep or drained (it is
	// starting, running or closed), so it needs no wake; nothing was written.
	WakeNotDormant
)

// queueByPolicy reports whether policy may not start or resume b's row now:
// under ResumeViaController any row whose runtime is not running (the API
// never starts a runtime in the controller's process) or that is held (a
// managed suspend whose runtime the controller has not stopped yet must not
// take a send that flips it active); under ResumeIfUnheld a held one
// (HoldVerdict).
func (m *Manager) queueByPolicy(meta map[string]string, sessName string, policy ResumePolicy) bool {
	running := m.sp.IsRunning(sessName)
	switch policy {
	case ResumeOperator, resumeReplacing:
		return false
	case ResumeViaController:
		return !running || HoldVerdict(meta, running, m.now())
	}
	return HoldVerdict(meta, running, m.now())
}

// RequestWakeUnlessHeld records an explicit wake by CAS on an open asleep or
// drained row that nothing holds (D8 rule 1). A row whose lifecycle refuses a
// wake returns *WakeConflictError, as WakeSession does; a CAS lost on every
// attempt returns ErrWakeRequestContended.
func (s *Store) RequestWakeUnlessHeld(id string, runtimeRunning bool, now time.Time) (WakeRequestOutcome, error) {
	var outcome WakeRequestOutcome
	var conflict error
	ok, err := s.UpdateMetadataFenced(id, 3, func(info Info, persisted PersistedResponse) MetadataPatch {
		input := LifecycleInputFromMetadata(persisted.Status, persisted.Metadata)
		input.Now = now
		conflict, outcome = nil, WakeNotDormant
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
		return RequestExplicitWakePatch(string(WakeCauseExplicit), now)
	})
	switch {
	case err != nil:
		return 0, err
	case conflict != nil:
		return 0, conflict
	case outcome == WakeRecorded && !ok:
		return 0, fmt.Errorf("%w: %s", ErrWakeRequestContended, id)
	}
	return outcome, nil
}

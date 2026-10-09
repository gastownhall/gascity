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
)

var (
	// ErrResumeHeld reports that the policy did not let the call resume a
	// held session. Nothing was started or written.
	ErrResumeHeld = errors.New("session is held; not resumed")
	// ErrWakeRequestContended reports a wake request whose CAS lost on every
	// attempt; nothing was written. Retry.
	ErrWakeRequestContended = errors.New("wake request lost to concurrent writes; retry")
)

// HoldVerdict is CONTRACT v5.9 D8 7(a)'s one hold predicate, for resume,
// wake requests, controller-routed sends and queued delivery. A row is held
// by an operator's intent: state=suspended, a future held_until or
// quarantined_until (an unparseable one is not, as legacy reads it), a set
// wait_hold, or sleep_intent=user-hold. Legacy's own intents
// (idle-stop-pending) hold nothing. A row whose runtime is running and that
// is not suspended is working, not held: a heartbeat held_until keeps a live
// session up rather than queueing its sends.
func HoldVerdict(meta map[string]string, runtimeRunning bool, now time.Time) bool {
	suspended := State(strings.TrimSpace(meta["state"])) == StateSuspended
	if !suspended && runtimeRunning {
		return false
	}
	if suspended || strings.TrimSpace(meta["wait_hold"]) != "" ||
		strings.TrimSpace(meta["sleep_intent"]) == string(SleepReasonUserHold) {
		return true
	}
	for _, key := range []string{"held_until", "quarantined_until"} {
		if until, err := time.Parse(time.RFC3339, strings.TrimSpace(meta[key])); err == nil && until.After(now) {
			return true
		}
	}
	return false
}

// HoldVerdictInfo is HoldVerdict over a typed row.
func HoldVerdictInfo(info Info, runtimeRunning bool, now time.Time) bool {
	return HoldVerdict(map[string]string{
		"state": info.MetadataState, "held_until": info.HeldUntil, "quarantined_until": info.QuarantinedUntil,
		"sleep_intent": info.SleepIntent, "wait_hold": info.WaitHold,
	}, runtimeRunning, now)
}

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

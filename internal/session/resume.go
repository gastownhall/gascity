package session

import (
	"errors"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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

// ErrResumeHeld reports that the policy did not let the call resume a held
// session. Nothing was started or written.
var ErrResumeHeld = errors.New("session is held; not resumed")

// operatorSleepIntent reports whether meta's sleep_intent is an operator's
// hold: user-hold, or wait-hold while the wait hold stands. Legacy's own
// intents (idle-stop-pending) hold nothing: a send to a live session in its
// idle drain is delivered live.
func operatorSleepIntent(meta map[string]string) bool {
	switch strings.TrimSpace(meta["sleep_intent"]) {
	case string(SleepReasonUserHold):
		return true
	case string(SleepReasonWaitHold):
		return strings.TrimSpace(meta["wait_hold"]) != ""
	}
	return false
}

// isDormantRow reports whether meta's row is one a resume consumes: asleep,
// suspended, drained, or holding an operator's sleep intent.
func isDormantRow(meta map[string]string) bool {
	switch State(strings.TrimSpace(meta["state"])) {
	case StateAsleep, StateSuspended, StateDrained:
		return true
	}
	return operatorSleepIntent(meta)
}

// IsHeldForResume reports whether an operator's hold blocks a resume that is
// not the operator's own (D8 rule 1): state=suspended, a future (or
// unparseable) held_until or quarantined_until, a wait hold, or an
// operator's sleep intent.
func IsHeldForResume(meta map[string]string, now time.Time) bool {
	if State(strings.TrimSpace(meta["state"])) == StateSuspended ||
		strings.TrimSpace(meta["wait_hold"]) != "" || operatorSleepIntent(meta) {
		return true
	}
	for _, key := range []string{"held_until", "quarantined_until"} {
		if raw := strings.TrimSpace(meta[key]); raw != "" {
			if until, err := time.Parse(time.RFC3339, raw); err != nil || until.After(now) {
				return true
			}
		}
	}
	return false
}

// resumeHeld reports whether policy may not resume b's row now: a dormant,
// held row and a policy other than ResumeOperator.
func (m *Manager) resumeHeld(b beads.Bead, policy ResumePolicy) bool {
	return policy != ResumeOperator && isDormantRow(b.Metadata) && IsHeldForResume(b.Metadata, m.now())
}

// RequestWakeUnlessHeld records an explicit wake by CAS on an open, unheld
// row and reports whether it did (D8 rule 1). A row whose lifecycle refuses
// a wake returns *WakeConflictError, as WakeSession does.
func (s *Store) RequestWakeUnlessHeld(id string, now time.Time) (bool, error) {
	var conflict error
	ok, err := s.UpdateMetadataFenced(id, 3, func(info Info, persisted PersistedResponse) MetadataPatch {
		input := LifecycleInputFromMetadata(persisted.Status, persisted.Metadata)
		input.Now = now
		conflict = nil
		if state, refused := lifecycleWakeConflictState(ProjectLifecycle(input)); refused {
			conflict = &WakeConflictError{SessionID: id, State: state}
			return nil
		}
		if info.Closed || IsHeldForResume(persisted.Metadata, now) {
			return nil
		}
		return RequestExplicitWakePatch(string(WakeCauseExplicit), now)
	})
	if err != nil {
		return false, err
	}
	return ok, conflict
}

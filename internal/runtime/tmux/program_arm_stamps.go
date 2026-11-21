package tmux

import (
	"strings"
	"sync"
	"time"
)

// programArmRetention is how long programArmStamps keeps an arm's stamp. A
// stamp matters only while a process table scanned before the arm is still
// read, which the state cache's TTL and staleTTL bound to seconds; minutes
// cover any snapshot a reading still trusts.
const programArmRetention = 10 * time.Minute

// programArmStamps records when this process armed each capture nonce, keyed
// by the nonce's rand. The state cache's process table is up to its TTL old,
// so a tap this process started after that table was scanned is missing from
// it without being gone: judged by the table alone, a freshly armed pane
// would read reader_lost until the next refresh, exactly when readiness is
// decided. A stamp later than the scan makes the tap's liveness unknown
// instead (programTaps.pending), which withholds the reader's verification
// without calling the reader lost.
type programArmStamps struct {
	mu sync.Mutex
	at map[string]time.Time
	// now reads the clock; nil is time.Now. Tests inject one.
	now func() time.Time
}

func (s *programArmStamps) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// noteArmed stamps an arm under rand that has just run, and forgets stamps
// older than programArmRetention. Nil-safe for a plan that armed nothing.
func (s *programArmStamps) noteArmed(plan *capturePlan) {
	if plan == nil || plan.rand == "" {
		return
	}
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at == nil {
		s.at = make(map[string]time.Time)
	}
	for rand, at := range s.at {
		if now.Sub(at) > programArmRetention {
			delete(s.at, rand)
		}
	}
	s.at[plan.rand] = now
}

// armedSince returns the rands of the arms stamped at or after scanned: the
// taps a process table scanned at scanned may predate. A zero scanned (the
// time is unknown) returns every stamp held.
func (s *programArmStamps) armedSince(scanned time.Time) map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rands map[string]struct{}
	for rand, at := range s.at {
		if !scanned.IsZero() && at.Before(scanned) {
			continue
		}
		if rands == nil {
			rands = make(map[string]struct{})
		}
		rands[rand] = struct{}{}
	}
	return rands
}

// snapshotTaps is state's tap liveness with the taps this process armed
// after state's process table was scanned marked pending.
func (t *Tmux) snapshotTaps(state runtimeStateSnapshot) programTaps {
	taps := state.programTaps()
	if taps.known {
		taps.pending = t.programArms.armedSince(state.ProcessesAt)
	}
	return taps
}

// nonceRand is the rand of a "<rand>.<pane_id>" nonce, "" for any other value.
func nonceRand(nonce string) string {
	rand, pane, ok := strings.Cut(nonce, ".")
	if !ok || rand == "" || !wellFormedTmuxID(pane, '%') {
		return ""
	}
	return rand
}

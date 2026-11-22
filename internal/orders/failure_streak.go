package orders

import (
	"sync"
	"time"
)

const (
	// FailureAlertAfter is the consecutive-failure count at which an order's
	// failure streak alerts. Three, not two: two in a row is a plausible
	// transient for anything touching the network or a lock. Three, not five:
	// on a 6h order five failures is 30h before anything surfaces. gc doctor's
	// order-outcome check flags an order at the same count.
	FailureAlertAfter = 3

	// StartupFailureGrace covers gastownhall/gascity#3898: for ~5 minutes
	// after a controller start, exec orders fail spuriously because dispatch
	// begins before pack staging completes (2x margin on the observed window).
	// Failures inside it are skipped, neither counted nor breaking a streak.
	StartupFailureGrace = 10 * time.Minute

	// FailureAlertRepeat is the minimum gap between two alerts for one
	// unbroken streak, so a permanently failing order re-alerts hourly rather
	// than on every run.
	FailureAlertRepeat = time.Hour
)

// OrderFailingAlert describes an order whose consecutive failures reached
// FailureAlertAfter.
type OrderFailingAlert struct {
	OrderName   string
	Consecutive int
	// FirstFailed is the streak anchor: the failure that opened it.
	FirstFailed time.Time
	// FailingFor is the gap from FirstFailed to the failure that alerted.
	FailingFor time.Duration
	// LastError is the message of the failure that alerted.
	LastError string
}

// FailureStreaks counts each order's consecutive failures and says when a
// streak should alert. It decides nothing about dispatch; callers report
// outcomes and publish the alerts it returns. Safe for concurrent use.
type FailureStreaks struct {
	mu        sync.Mutex
	startedAt time.Time
	streaks   map[string]*failureStreak
}

type failureStreak struct {
	count     int
	first     time.Time
	alerted   bool
	lastAlert time.Time
}

// NewFailureStreaks returns an empty table for a controller started at
// startedAt; failures within StartupFailureGrace of it are not counted.
func NewFailureStreaks(startedAt time.Time) *FailureStreaks {
	return &FailureStreaks{startedAt: startedAt, streaks: make(map[string]*failureStreak)}
}

// ObserveFailure records a failure of the named order at at, and returns the
// alert to publish when the streak has reached FailureAlertAfter and has not
// alerted within the last FailureAlertRepeat.
func (s *FailureStreaks) ObserveFailure(name, message string, at time.Time) (OrderFailingAlert, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.Before(s.startedAt.Add(StartupFailureGrace)) {
		return OrderFailingAlert{}, false
	}
	st := s.streaks[name]
	if st == nil {
		st = &failureStreak{first: at}
		s.streaks[name] = st
	}
	st.count++
	if st.count < FailureAlertAfter {
		return OrderFailingAlert{}, false
	}
	if st.alerted && at.Sub(st.lastAlert) < FailureAlertRepeat {
		return OrderFailingAlert{}, false
	}
	st.alerted = true
	st.lastAlert = at
	return OrderFailingAlert{
		OrderName:   name,
		Consecutive: st.count,
		FirstFailed: st.first,
		FailingFor:  at.Sub(st.first),
		LastError:   message,
	}, true
}

// OrderRecovery describes the success that ended an order's alerted failure
// streak.
type OrderRecovery struct {
	OrderName string
	// Consecutive is how many failures the streak counted.
	Consecutive int
	// FirstFailed is the streak anchor, matching the alerts it raised.
	FirstFailed time.Time
	// FailedFor is the gap from FirstFailed to the success.
	FailedFor time.Duration
}

// ObserveSuccess records a successful run of the named order at at, ending
// its streak so the next failure starts a new one at 1. It returns the
// recovery to publish when the streak it ended had alerted.
func (s *FailureStreaks) ObserveSuccess(name string, at time.Time) (OrderRecovery, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.streaks[name]
	if st == nil {
		return OrderRecovery{}, false
	}
	delete(s.streaks, name)
	if !st.alerted {
		return OrderRecovery{}, false
	}
	return OrderRecovery{
		OrderName:   name,
		Consecutive: st.count,
		FirstFailed: st.first,
		FailedFor:   at.Sub(st.first),
	}, true
}

// Inherit takes over prev's controller start and the streaks of the orders
// keep accepts, so a table rebuilt on a config reload neither restarts the
// startup grace nor forgets a running streak. A streak keep rejects, an order
// no longer configured, is dropped: no later outcome could reach it.
func (s *FailureStreaks) Inherit(prev *FailureStreaks, keep func(name string) bool) {
	if s == nil || prev == nil || s == prev {
		return
	}
	prev.mu.Lock()
	defer prev.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startedAt = prev.startedAt
	for name, st := range prev.streaks {
		if !keep(name) {
			continue
		}
		if _, ok := s.streaks[name]; ok {
			continue
		}
		cp := *st
		s.streaks[name] = &cp
	}
}

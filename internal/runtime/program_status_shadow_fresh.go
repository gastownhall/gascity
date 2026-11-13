package runtime

import (
	"errors"
	"sync"
	"time"
)

// Fresh decision readings under the shadow switch. The points outside the
// leaf whose decision the snapshot [ProgramStatusProvider] answers from can
// lag (wait_idle_preread, queued_delivery, starved_bypass) judge the record
// read fresh from the runtime with the decision they compare: that snapshot
// lags the record by up to its TTL, and a reading that old judges a record
// the program has already left (a turn ended, a dialog answered) as the
// program's verdict. A decision's points share its one reading
// ([ShadowReading]); the rule judges it with the runtime's last activity for
// the hold bound, at the clock read after the reads, so no state the reading
// shows can have been entered after the time it is judged at. The other hold
// points (execution_backstop, claim_nudge_hold, work_ladder, drain_reminder)
// still judge the snapshot ([ShadowCompareHold]): the shadow traced no
// misjudged reading to them. Observation judges the reading its view took
// ([ShadowObserveIdle]): under the on switch the view decides from that
// reading, lag included, so judging a fresh read would measure a view the on
// switch never serves.

// ShadowReading is one decision's record reading under the shadow switch:
// the session's record read fresh from the runtime
// ([ProgramStatusShadowProvider.ProgramStatusShadowRead]), the runtime's last
// activity, and the clock read after both. It is taken the first time one of
// the decision's points compares on it and shared by the rest, so a decision
// several points record takes one read. A decision is one synchronous look
// with nothing typed between its points: a reading kept past it would judge
// a later decision on a record the program may have left. A leaf that is not
// shadowing reads nothing and records nothing. Safe for concurrent use; the
// nil reading records nothing.
type ShadowReading struct {
	p    Provider
	name string

	once     sync.Once
	ok       bool
	status   ProgramStatus
	sink     ProgramStatusComparisonSink
	activity time.Time
	at       time.Time
}

// NewShadowReading returns the reading of session name on p one decision
// shares. It reads nothing until a point compares on it.
func NewShadowReading(p Provider, name string) *ShadowReading {
	return &ShadowReading{p: p, name: name}
}

// take takes the reading once, and reports whether it was taken: the leaf
// is shadowing and its fresh read succeeded.
func (r *ShadowReading) take() bool {
	if r == nil {
		return false
	}
	r.once.Do(func() {
		s, sink, ok := programStatusShadowFresh(r.p, r.name)
		if !ok {
			return
		}
		last, err := r.p.GetLastActivity(r.name)
		if err != nil {
			last = time.Time{}
		}
		r.status, r.sink, r.activity, r.at, r.ok = s, sink, last, time.Now(), true
	})
	return r.ok
}

// Compare records one comparison of the reading's session at point: scrape
// is the verdict today's code decided with, rule maps the reading to the
// record's verdict, judged with the runtime's last activity at the later of
// now (the time the caller's decision was made) and the clock read after
// the reading. It takes the reading when no point has yet. It records
// nothing unless the leaf is shadowing, and never feeds the decision.
func (r *ShadowReading) Compare(sessionID, family string, point ProgramStatusPoint, scrape ProgramVerdict, now time.Time, rule func(s ProgramStatus, lastActivity, now time.Time) ProgramVerdict) {
	if !r.take() {
		return
	}
	judged := laterTime(now, r.at)
	r.sink(ProgramStatusComparison{
		Session: r.name, SessionID: sessionID, Family: family, Point: point,
		Scrape: scrape, Program: rule(r.status, r.activity, judged), Status: r.status,
		HoldActivity: r.activity, JudgedAt: judged,
	})
}

// ShadowIdleBoundaryNow is [IdleBoundaryNow], the pre-read of a wait-idle
// delivery, recorded under the shadow switch (wait_idle_preread): the idle
// snapshot the boundary is classified from (idle, busy, or indeterminate)
// beside the record's idle verdict ([ProgramIdleVerdict]) on a reading taken
// fresh right after the snapshot's capture. A snapshot that fails otherwise
// decided nothing and records nothing. The boundary returned is
// IdleBoundaryNow's: the snapshot is the decision's own, so the shadow adds
// no look at the screen, and no reading feeds it.
func ShadowIdleBoundaryNow(p Provider, name, sessionID, family string) IdleBoundary {
	snap, ok := p.(IdleSnapshotProvider)
	if !ok {
		return IdleBoundaryUnknown
	}
	idle, err := snap.SnapshotIdle(name)
	if scrape, decided := idleSnapshotVerdict(idle, err); decided && name != "" {
		NewShadowReading(p, name).Compare(sessionID, family, ProgramPointWaitIdlePreread, scrape, time.Now(), ProgramIdleVerdict)
	}
	return idleBoundaryOf(p, name, idle, err)
}

// idleSnapshotVerdict is an idle snapshot's scrape verdict: idle, busy, or
// indeterminate for a reading the runtime does not trust. ok is false for a
// snapshot that failed otherwise: it decided nothing.
func idleSnapshotVerdict(idle bool, err error) (ProgramVerdict, bool) {
	switch {
	case errors.Is(err, ErrIdleIndeterminate):
		return ProgramVerdictIndeterminate, true
	case err != nil:
		return "", false
	case idle:
		return ProgramVerdictIdle, true
	default:
		return ProgramVerdictBusy, true
	}
}

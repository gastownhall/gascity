package workladder

import (
	"slices"
	"time"
)

// Decide is one ladder pass for one seat (§6.3). It is pure: the same prev,
// obs and p give the same Decision (WL7).
//
// The steps, in order: reconcile the entries with obs.Outstanding (rungs only
// rise; a rise resets the count); reset counts on a new generation, and on
// progress the seat made since its last nudge, whether or not the ladder saw
// it; settle a pending release; then, only with the gate open, the runtime
// alive and the seat quiet, act on the due entries, earliest first: at most
// one Nudge per pass, and none within Base of the seat's last (WL3). An entry
// whose live read could not tell (obs.Hold) waits, kept as it is.
func Decide(prev Record, obs Observation, p Policy) Decision {
	p = p.withDefaults()
	now := obs.Now
	var acts []Action

	// 1-2. Reconcile entries; a new generation restarts every count.
	newGen := prev.V != 0 && prev.Gen != obs.Gen
	old := make(map[ClaimKey]Entry, len(prev.Entries))
	for _, e := range prev.Entries {
		old[e.Key] = clampEntry(e, now)
	}
	var entries []Entry
	for _, w := range outstanding(obs.Outstanding) {
		src, started := startedBy(obs, w)
		init := initialRung(w.Kind, started)
		e, ok := old[w.Key]
		switch {
		case !ok || e.Kind != w.Kind:
			ne := Entry{Key: w.Key, Kind: w.Kind, Rung: init, Since: now}
			if ok { // the same work under another kind keeps its recycle history
				ne.Recycles, ne.RecycleAt = e.Recycles, e.RecycleAt
			}
			if init == RungStarted {
				ne.Seed = src
			}
			e = ne
		default:
			if newGen {
				e.Attempts, e.Paused, e.Since, e.Unreleased, e.Refused = 0, 0, now, false, 0
			}
			if init > e.Rung {
				e.Rung, e.Attempts, e.Paused, e.Since, e.Escalated, e.Unreleased, e.Refused = init, 0, 0, now, false, false, 0
				if init == RungStarted {
					e.Seed = src
				}
			}
		}
		e = p.progressReset(e, obs)
		entries = append(entries, e)
	}
	next := Record{V: Version, Gen: obs.Gen, Entries: entries, LastNudge: minTime(prev.LastNudge, now), RecycledGen: prev.RecycledGen}
	idx := func(k ClaimKey) int { return slices.IndexFunc(next.Entries, func(e Entry) bool { return e.Key == k }) }

	// 3. A pending release: re-emit while the claim is still outstanding and
	// unstarted, clear it once it is gone, alarm if it raced a start.
	if r := prev.Release; r != nil {
		i := idx(r.Key)
		_, named := obs.Started[r.Key]
		switch {
		case obs.StartedRead && (named || (i >= 0 && next.Entries[i].Rung >= RungStarted)):
			acts = append(acts, Alert{Key: r.Key, Reason: AlertReleaseRacedStart})
		case i < 0 || next.Entries[i].Kind != KindClaim:
		case !obs.StartedRead: // cannot re-prove unstarted; hold the mark, so the hook still refuses it
			next.Release = r
		default:
			next.Release = r
			acts = append(acts, Release{Key: r.Key})
		}
	}

	d := Decision{Next: next, Actions: acts}
	// 4-6. Held, not running, or not quiet: nothing more.
	if len(next.Entries) == 0 || obs.Gate.Held || obs.Runtime != TriTrue || obs.Quiet.State != Quiescent {
		return p.settle(d, prev)
	}

	// 7-8. Quiet: act on the due entries, earliest first.
	degraded := obs.Provider.Degraded
	order := make([]int, 0, len(d.Next.Entries))
	for i, e := range d.Next.Entries {
		if obs.Hold[e.Key] || (d.Next.Release != nil && d.Next.Release.Key == e.Key) {
			continue // a read that could not tell, or being released
		}
		if !p.dueAt(e, degraded).After(now) {
			order = append(order, i)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return p.dueAt(d.Next.Entries[a], degraded).Compare(p.dueAt(d.Next.Entries[b], degraded))
	})
	// The seat's nudge slot: one per pass, and none within Base of its last.
	nudged := !d.Next.LastNudge.IsZero() && now.Sub(d.Next.LastNudge) < p.Base
	for _, i := range order {
		e := &d.Next.Entries[i]
		nudge := func(paused bool) {
			if paused {
				e.Paused++
			} else {
				e.Attempts++
			}
			e.Last, d.Next.LastNudge = now, now
			nudged = true
			d.Actions = append(d.Actions, Nudge{Key: e.Key, Kind: e.Kind, Rung: e.Rung, Attempt: e.Attempts, Paused: paused})
		}
		t := p.threshold(e.Kind, e.Rung)
		switch {
		case degraded: // WL4: deliver, never count, never escalate
			if !nudged {
				nudge(true)
			}
		case e.Rung == RungEscalated || e.Attempts < t:
			if !nudged {
				nudge(false)
			}
		case e.Kind == KindTrigger:
			if nudged {
				continue
			}
			if e.RecycleAt.IsZero() || now.Sub(e.RecycleAt) >= p.RecycleRepost {
				e.RecycleAt, d.Next.RecycledGen = now, obs.Gen
				d.Actions = append(d.Actions, Recycle{Key: e.Key})
			}
			nudge(false)
		case e.Kind == KindContinuation:
			if nudged {
				continue
			}
			if !e.Escalated {
				e.Escalated = true
				d.Actions = append(d.Actions, Alert{Key: e.Key, Reason: AlertContinuationUnanswered})
			}
			nudge(false)
		case e.Rung == RungClaimed:
			// WL2: the rung is claimed only while nothing names the key
			// started; the caller re-proves it by CAS before releasing.
			canRelease := d.Next.Release == nil && obs.StartedRead
			if _, named := obs.Started[e.Key]; named {
				canRelease = false
			}
			if !e.Unreleased {
				if !canRelease {
					continue
				}
				e.Last = now // a release that cannot finish is retried a backoff later
				d.Next.Release = &ReleaseMark{Key: e.Key, Gen: obs.Gen, At: now}
				d.Actions = append(d.Actions, Release{Key: e.Key})
				continue
			}
			// A release ran and did not finish: keep nudging at Cap, alert
			// once, and retry the release at the same pace.
			if nudged {
				continue
			}
			if canRelease {
				d.Next.Release = &ReleaseMark{Key: e.Key, Gen: obs.Gen, At: now}
				d.Actions = append(d.Actions, Release{Key: e.Key})
			}
			if !e.Escalated {
				e.Escalated = true
				d.Actions = append(d.Actions, Alert{Key: e.Key, Reason: AlertReleaseUnfinished})
			}
			nudge(false)
		case e.Recycles < p.MaxRecycles:
			e.Recycles++
			e.RecycleAt, e.Last, d.Next.RecycledGen = now, now, obs.Gen
			d.Actions = append(d.Actions, Recycle{Key: e.Key, KeepClaim: true}, Alert{Key: e.Key, Reason: AlertStartedRecycled})
		default:
			if nudged {
				continue
			}
			e.Rung, e.Escalated = RungEscalated, true
			d.Actions = append(d.Actions, Alert{Key: e.Key, Reason: AlertStartedUnanswered})
			nudge(false)
		}
	}
	return p.settle(d, prev)
}

// progressReset restarts e's count when the seat made progress at least
// ProgressReset after e's last nudge, observed now or not (an outage, a
// hold, the ladder off): its activity clock is the evidence, read on any
// pass where the runtime is alive. The entry's grace restarts from that
// activity.
func (p Policy) progressReset(e Entry, obs Observation) Entry {
	if obs.Runtime != TriTrue || e.Last.IsZero() || (e.Attempts == 0 && e.Paused == 0 && !e.Unreleased) ||
		obs.LastActivity.Before(e.Last.Add(p.ProgressReset)) {
		return e
	}
	e.Attempts, e.Paused, e.Unreleased, e.Refused, e.Since = 0, 0, false, 0, minTime(obs.LastActivity, obs.Now)
	return e
}

// clampEntry pulls e's times from a clock that ran ahead back to now, so a
// skewed stamp cannot hold the entry back by its skew.
func clampEntry(e Entry, now time.Time) Entry {
	e.Since, e.Last, e.RecycleAt = minTime(e.Since, now), minTime(e.Last, now), minTime(e.RecycleAt, now)
	return e
}

func minTime(t, now time.Time) time.Time {
	if t.After(now) {
		return now
	}
	return t
}

// dueAt is when a quiet seat's entry is next due: a grace after the entry
// (or its generation, or its last progress) started, else the backoff after
// its last nudge. While the provider is degraded the backoff is PausedCap,
// both floor and cap: delivery goes on, at a steady pace. While the seat
// refuses nudges (Refused) PausedCap is the floor.
func (p Policy) dueAt(e Entry, degraded bool) time.Time {
	if e.Attempts == 0 && e.Paused == 0 && !e.Unreleased {
		return e.Since.Add(p.Grace)
	}
	iv := p.backoff(e)
	switch {
	case degraded:
		iv = p.PausedCap
	case e.Refused > 0:
		iv = max(iv, p.PausedCap)
	}
	return e.Last.Add(iv)
}

// settle stamps each entry's Next and decides whether the cell changed. Two
// empty cells are equal whatever their generations, so a seat with nothing
// outstanding never writes.
func (p Policy) settle(d Decision, prev Record) Decision {
	for i := range d.Next.Entries {
		d.Next.Entries[i].Next = p.dueAt(d.Next.Entries[i], false)
	}
	empty := func(r Record) bool { return len(r.Entries) == 0 && r.Release == nil }
	switch {
	case empty(d.Next) && (prev.V == 0 || empty(prev)):
		d.Changed = false
	default:
		d.Changed = !sameRecord(prev, d.Next)
	}
	return d
}

// sameRecord reports whether a and b store the same cell text.
func sameRecord(a, b Record) bool {
	if a.V != b.V || a.Gen != b.Gen || a.RecycledGen != b.RecycledGen || !a.LastNudge.Equal(b.LastNudge) || len(a.Entries) != len(b.Entries) || (a.Release == nil) != (b.Release == nil) {
		return false
	}
	if a.Release != nil && (a.Release.Key != b.Release.Key || a.Release.Gen != b.Release.Gen || !a.Release.At.Equal(b.Release.At)) {
		return false
	}
	for i, x := range a.Entries {
		y := b.Entries[i]
		if x.Key != y.Key || x.Kind != y.Kind || x.Rung != y.Rung || x.Attempts != y.Attempts || x.Paused != y.Paused ||
			x.Recycles != y.Recycles || x.Escalated != y.Escalated || x.Seed != y.Seed || x.Unreleased != y.Unreleased ||
			x.Refused != y.Refused || !x.Since.Equal(y.Since) || !x.Last.Equal(y.Last) || !x.Next.Equal(y.Next) || !x.RecycleAt.Equal(y.RecycleAt) {
			return false
		}
	}
	return true
}

// outstanding is ws deduplicated by key (the first wins) in key order.
func outstanding(ws []Work) []Work {
	out := make([]Work, 0, len(ws))
	seen := make(map[ClaimKey]bool, len(ws))
	for _, w := range ws {
		if w.Key.Bead == "" || seen[w.Key] {
			continue
		}
		seen[w.Key] = true
		out = append(out, w)
	}
	slices.SortStableFunc(out, func(a, b Work) int { return a.Key.compare(b.Key) })
	return out
}

// startedBy reports whether a claim is started and by what. An unreadable
// agent cell names every claim (fail safe). Triggers and continuations are
// never started: nothing has been handed out.
func startedBy(obs Observation, w Work) (StartSource, bool) {
	if w.Kind != KindClaim {
		return 0, false
	}
	if !obs.StartedRead {
		return SrcAgent, true
	}
	src, ok := obs.Started[w.Key]
	return src, ok
}

func initialRung(k Kind, started bool) Rung {
	switch {
	case k != KindClaim:
		return RungAssigned
	case started:
		return RungStarted
	default:
		return RungClaimed
	}
}

// ClearRelease is r without its pending release, when that release names key
// at generation gen: the lane's last step of a release that released the
// claim (§6.5 step 3). ok is false when r holds no such release.
func ClearRelease(r Record, key ClaimKey, gen int64) (Record, bool) {
	if r.Release == nil || r.Release.Key != key || r.Release.Gen != gen {
		return r, false
	}
	r.Release = nil
	return r, true
}

// ReleaseUnfinished is r after a release of key at gen ran to an outcome
// that did not release the claim (kept: no lane serves its route, or no
// single live hit; or failed): the release cleared, and the entry marked so
// it is nudged at Cap, with one alert, and the release retried at Cap.
func ReleaseUnfinished(r Record, key ClaimKey, gen int64, now time.Time) (Record, bool) {
	r, ok := ClearRelease(r, key, gen)
	if !ok {
		return r, false
	}
	r.Entries = slices.Clone(r.Entries)
	if i := slices.IndexFunc(r.Entries, func(e Entry) bool { return e.Key == key }); i >= 0 {
		r.Entries[i].Unreleased, r.Entries[i].Last = true, now
	}
	return r, true
}

// ReleaseFoundStarted is r after the release of key at gen found, on its
// live read, that the claim was handed to a turn of the seat (by src): the
// release cleared, and the entry started, with a fresh count.
func ReleaseFoundStarted(r Record, key ClaimKey, gen int64, src StartSource, now time.Time) (Record, bool) {
	r, ok := ClearRelease(r, key, gen)
	if !ok {
		return r, false
	}
	r.Entries = slices.Clone(r.Entries)
	if i := slices.IndexFunc(r.Entries, func(e Entry) bool { return e.Key == key }); i >= 0 && r.Entries[i].Rung < RungStarted {
		e := &r.Entries[i]
		e.Rung, e.Attempts, e.Paused, e.Since, e.Escalated, e.Unreleased, e.Seed = RungStarted, 0, 0, now, false, false, src
	}
	return r, true
}

// refusedAlertAt is how many nudges in a row a seat refuses before the one
// nudge_refused alert.
const refusedAlertAt = 3

// NudgeRefused is r after the seat could not take the nudge of attempt
// about key that landed at now (a permission prompt, the runtime
// unavailable): the attempt is re-counted as paused, so it moves nothing
// toward escalation, and the entry backs off to PausedCap until a nudge is
// taken. alert is true exactly when the refusals reach refusedAlertAt: the
// caller raises one nudge_refused alert. ok is false when r no longer holds
// that attempt.
func NudgeRefused(r Record, key ClaimKey, attempt int, now time.Time) (out Record, alert, ok bool) {
	i := slices.IndexFunc(r.Entries, func(e Entry) bool { return e.Key == key })
	if i < 0 || !r.Entries[i].Last.Equal(now) || r.Entries[i].Attempts != attempt {
		return r, false, false
	}
	r.Entries = slices.Clone(r.Entries)
	e := &r.Entries[i]
	if e.Attempts > 0 {
		e.Attempts--
	}
	e.Paused++
	e.Refused++
	return r, e.Refused == refusedAlertAt, true
}

// NudgeTaken is r after the seat took a nudge about key: its refusals end.
// ok is false when there were none (no write needed).
func NudgeTaken(r Record, key ClaimKey) (Record, bool) {
	i := slices.IndexFunc(r.Entries, func(e Entry) bool { return e.Key == key })
	if i < 0 || r.Entries[i].Refused == 0 {
		return r, false
	}
	r.Entries = slices.Clone(r.Entries)
	r.Entries[i].Refused = 0
	return r, true
}

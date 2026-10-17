package workladder

import (
	"slices"
	"time"
)

// Decide is one ladder pass for one seat (§6.3). It is pure: the same prev,
// obs and p give the same Decision (WL7).
//
// The steps, in order: reconcile the entries with obs.Outstanding (rungs only
// rise; a rise resets the count); reset counts on a new generation; settle a
// pending release; then, only with the gate open, the runtime alive and the
// seat quiet, act on the due entries: at most one Nudge per pass, for the
// earliest-due entry. Due is never zero while anything is outstanding, and
// never later than now+Cap with the gate open (WL1).
func Decide(prev Record, obs Observation, p Policy) Decision {
	p = p.withDefaults()
	now := obs.Now
	var acts []Action
	note := func(k ClaimKey, ev string) { acts = append(acts, Note{Key: k, Event: ev}) }

	// 1-2. Reconcile entries; a new generation restarts every count.
	newGen := prev.V != 0 && prev.Gen != obs.Gen
	old := make(map[ClaimKey]Entry, len(prev.Entries))
	for _, e := range prev.Entries {
		old[e.Key] = e
	}
	var entries []Entry
	cur := make(map[ClaimKey]bool, len(obs.Outstanding))
	for _, w := range outstanding(obs.Outstanding) {
		cur[w.Key] = true
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
				note(w.Key, NoteSeeded)
			} else {
				note(w.Key, NoteNew)
			}
			e = ne
		default:
			if newGen {
				e.Attempts, e.Paused, e.Since = 0, 0, now
			}
			if init > e.Rung {
				e.Rung, e.Attempts, e.Paused, e.Since, e.Escalated = init, 0, 0, now, false
				if init == RungStarted {
					e.Seed = src
				}
				note(w.Key, NoteRung)
			}
		}
		entries = append(entries, e)
	}
	for _, e := range prev.Entries {
		if !cur[e.Key] {
			note(e.Key, NoteDropped)
		}
	}
	next := Record{V: Version, Gen: obs.Gen, Entries: entries}
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
	if len(next.Entries) == 0 && next.Release == nil {
		return p.settle(d, prev)
	}

	// 4-6. Held, not running, or not quiet: no nudge.
	switch {
	case obs.Gate.Held:
		d.Due = now.Add(p.Grace)
		if obs.Gate.Until.After(now) {
			d.Due = obs.Gate.Until
		}
		return p.settle(d, prev)
	case obs.Runtime != TriTrue:
		d.Due = now.Add(p.Grace)
		return p.settle(d, prev)
	case obs.Quiet.State != Quiescent:
		for i := range d.Next.Entries {
			e := &d.Next.Entries[i]
			if e.Attempts > 0 && !e.Last.IsZero() && !obs.LastActivity.Before(e.Last.Add(p.ProgressReset)) {
				e.Attempts, e.Paused = 0, 0
				d.Actions = append(d.Actions, Note{Key: e.Key, Event: NoteProgressReset})
			}
		}
		d.Due = now.Add(p.Grace)
		return p.settle(d, prev)
	}

	// 7-8. Quiet: act on the due entries, earliest first.
	degraded := obs.Provider.Degraded
	order := make([]int, 0, len(d.Next.Entries))
	for i, e := range d.Next.Entries {
		if d.Next.Release != nil && d.Next.Release.Key == e.Key {
			continue // being released
		}
		if !p.dueAt(e, degraded).After(now) {
			order = append(order, i)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return p.dueAt(d.Next.Entries[a], degraded).Compare(p.dueAt(d.Next.Entries[b], degraded))
	})
	nudged := false
	for _, i := range order {
		e := &d.Next.Entries[i]
		nudge := func(paused bool) {
			if paused {
				e.Paused++
			} else {
				e.Attempts++
			}
			e.Last = now
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
				e.RecycleAt = now
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
			if d.Next.Release != nil || !obs.StartedRead {
				continue
			}
			if _, named := obs.Started[e.Key]; named {
				continue
			}
			d.Next.Release = &ReleaseMark{Key: e.Key, Gen: obs.Gen, At: now}
			d.Actions = append(d.Actions, Release{Key: e.Key})
		case e.Recycles < p.MaxRecycles:
			e.Recycles++
			e.RecycleAt, e.Last = now, now
			d.Actions = append(d.Actions, Recycle{Key: e.Key, KeepClaim: true})
		default:
			if nudged {
				continue
			}
			e.Rung, e.Escalated = RungEscalated, true
			d.Actions = append(d.Actions, Alert{Key: e.Key, Reason: AlertStartedUnanswered})
			nudge(false)
		}
	}

	// 9. Due: the earliest entry, never later than now+Cap.
	d.Due = now.Add(p.Cap)
	for _, e := range d.Next.Entries {
		if d.Next.Release != nil && d.Next.Release.Key == e.Key {
			continue
		}
		if at := p.dueAt(e, degraded); at.Before(d.Due) {
			d.Due = at
		}
	}
	if d.Next.Release != nil && now.Add(p.Grace).Before(d.Due) {
		d.Due = now.Add(p.Grace) // retry a release that did not finish
	}
	if d.Due.Before(now) {
		d.Due = now // an entry this pass deferred is due on the next
	}
	return p.settle(d, prev)
}

// dueAt is when a quiet seat's entry is next due: a grace after the entry
// (or its generation) started, else the backoff after its last nudge, capped
// at PausedCap while the provider is degraded.
func (p Policy) dueAt(e Entry, degraded bool) time.Time {
	if e.Attempts == 0 && e.Paused == 0 {
		return e.Since.Add(p.Grace)
	}
	iv := p.backoff(e)
	if degraded {
		iv = min(iv, p.PausedCap)
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
	if a.V != b.V || a.Gen != b.Gen || len(a.Entries) != len(b.Entries) || (a.Release == nil) != (b.Release == nil) {
		return false
	}
	if a.Release != nil && (a.Release.Key != b.Release.Key || a.Release.Gen != b.Release.Gen || !a.Release.At.Equal(b.Release.At)) {
		return false
	}
	for i, x := range a.Entries {
		y := b.Entries[i]
		if x.Key != y.Key || x.Kind != y.Kind || x.Rung != y.Rung || x.Attempts != y.Attempts || x.Paused != y.Paused ||
			x.Recycles != y.Recycles || x.Escalated != y.Escalated || x.Seed != y.Seed ||
			!x.Since.Equal(y.Since) || !x.Last.Equal(y.Last) || !x.Next.Equal(y.Next) || !x.RecycleAt.Equal(y.RecycleAt) {
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

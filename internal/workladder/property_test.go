package workladder

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

// The invariants of §6.8, checked over random observation sequences from a
// fixed seed: 100k sequences (10k under -short).

func sequences() int {
	if testing.Short() {
		return 10_000
	}
	return 100_000
}

// forEachSequence runs check on seeds 0..sequences()-1 across a few
// workers, failing on the lowest failing seed.
func forEachSequence(t *testing.T, check func(seed uint64) error) {
	t.Helper()
	n := sequences()
	workers := min(runtime.GOMAXPROCS(0), 8)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := w; seq < n; seq += workers {
				errs[seq] = check(uint64(seq))
			}
		}()
	}
	wg.Wait()
	for seq, err := range errs {
		if err != nil {
			t.Fatalf("sequence %d: %v", seq, err)
		}
	}
}

// sim is one seat over time: the work it holds, the evidence for it, and the
// lane's part (a release removes the work, mostly).
type sim struct {
	rng      *rand.Rand
	now      time.Time
	gen      int64
	rec      Record
	kinds    map[ClaimKey]Kind
	agent    map[ClaimKey]bool // the agent cell
	seeds    map[ClaimKey]StartSource
	started  map[ClaimKey]bool // ever started while continuously outstanding as a claim
	obs      Observation
	activity time.Time // the seat's last activity: it works now and then
	lastStep time.Duration
	churn    int // 0: a calm seat that only gains work; 1: everything flips
	// refusing: a calm seat its provider refuses throughout, under a hold
	// that does not end (FreshHoldStanding): it answers every nudge with a
	// refused turn a second later.
	refusing bool
}

var simKeys = []ClaimKey{key("a"), key("b"), key("c")}

const maxStep = 20 * time.Minute

// livenessBound is the longest a started claim may go unheard (no alert, no
// recycle) while it could be heard: the started rung's schedule from its
// grace (Grace, then Base doubling for the threshold's nudges, then Cap),
// with each of its seven acts late by up to three passes (the seat's one
// nudge slot shared by three entries, at the sim's longest step). It
// catches a claim silenced for good, which the sim's calm seats outlive.
const livenessBound = DefaultGrace + DefaultBase*(1+2+4+8+16) + DefaultCap + 7*3*maxStep

func newSim(seed uint64) *sim {
	s := &sim{
		rng: rand.New(rand.NewPCG(seed, 0x6c6164646572)), now: t0, gen: 1,
		kinds: map[ClaimKey]Kind{}, agent: map[ClaimKey]bool{}, seeds: map[ClaimKey]StartSource{}, started: map[ClaimKey]bool{},
	}
	s.obs = Observation{StartedRead: true, Runtime: TriTrue, Quiet: Quiet{State: Quiescent}}
	if s.chance(70) {
		s.churn = 1
	} else if s.chance(40) {
		s.refusing = true
		s.obs.RateLimited, s.obs.RateLimitedAt, s.obs.FreshHold = 3, t0.Add(-time.Hour), FreshHoldStanding
	}
	return s
}

func (s *sim) chance(n int) bool { return s.rng.IntN(100) < n }

// event is a change a calm seat never sees.
func (s *sim) event(n int) bool { return s.chance(n * s.churn) }

func (s *sim) drop(k ClaimKey) {
	delete(s.kinds, k)
	delete(s.agent, k)
	delete(s.seeds, k)
	delete(s.started, k)
}

// step advances the world and builds the next observation.
func (s *sim) step() Observation {
	steps := []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 3 * time.Minute, 7 * time.Minute, maxStep}
	if s.churn == 0 { // a calm seat runs for hours
		steps = steps[3:]
	}
	s.lastStep = steps[s.rng.IntN(len(steps))]
	s.now = s.now.Add(s.lastStep)
	for _, k := range simKeys {
		kind, ok := s.kinds[k]
		switch {
		case !ok && s.chance(15):
			s.kinds[k] = Kind(1 + s.rng.IntN(3))
			if s.kinds[k] == KindClaim && s.chance(30) {
				s.seeds[k] = StartSource(2 + s.rng.IntN(3))
			}
		case ok && s.event(4):
			s.drop(k)
		case ok && kind == KindTrigger && s.event(5): // the agent claims it
			s.kinds[k] = KindClaim
			delete(s.started, k)
			if s.chance(70) {
				s.agent[k] = true
			}
		case ok && kind == KindClaim && s.event(5):
			s.agent[k] = true
		}
	}
	if s.event(3) {
		s.gen++
	}
	flip := s.event
	if flip(10) {
		s.obs.Gate = Gate{Held: !s.obs.Gate.Held, Reason: "draining"}
	}
	if flip(5) {
		s.obs.Runtime = Tri(s.rng.IntN(3))
	}
	if flip(15) {
		s.obs.Quiet = Quiet{State: QuietState(s.rng.IntN(3))}
	}
	if flip(5) {
		s.obs.Provider = ProviderState{Degraded: !s.obs.Provider.Degraded}
	}
	if flip(8) { // the provider refuses the seat's turns now and then
		s.obs.RateLimited = s.rng.IntN(4)
		s.obs.RateLimitedAt = s.now.Add(-time.Duration(s.rng.IntN(int(maxStep))))
	}
	if flip(5) {
		s.obs.FreshHold = FreshHold(s.rng.IntN(3))
	}
	if flip(6) { // now and then the provider answers a turn
		s.obs.ServedAt = s.now.Add(-time.Duration(s.rng.IntN(int(maxStep))))
	}
	s.obs.StartedRead = !s.event(3)
	if s.event(4) { // the seat did some work
		s.activity = s.now.Add(-time.Duration(s.rng.IntN(120)) * time.Second)
	}
	s.obs.LastActivity = s.activity
	s.obs.Hold = nil
	for _, k := range simKeys {
		if s.event(5) {
			if s.obs.Hold == nil {
				s.obs.Hold = map[ClaimKey]bool{}
			}
			s.obs.Hold[k] = true
		}
	}

	o := s.obs
	o.Now, o.Gen, o.Outstanding, o.Started = s.now, s.gen, nil, map[ClaimKey]StartSource{}
	seeding := s.rec.V == 0 || s.rec.Gen != s.gen
	for _, k := range simKeys {
		kind, ok := s.kinds[k]
		if !ok {
			continue
		}
		o.Outstanding = append(o.Outstanding, Work{Key: k, Kind: kind})
		if s.agent[k] {
			o.Started[k] = SrcAgent
		} else if src, ok := s.seeds[k]; ok && seeding {
			o.Started[k] = src
		}
		if kind == KindClaim && (o.Started[k] != 0 || !o.StartedRead) {
			s.started[k] = true
		}
	}
	return o
}

func countNudges(d Decision) int {
	n := 0
	for _, a := range d.Actions {
		if _, ok := a.(Nudge); ok {
			n++
		}
	}
	return n
}

func entryOf(r Record, k ClaimKey) (Entry, bool) {
	for _, e := range r.Entries {
		if e.Key == k {
			return e, true
		}
	}
	return Entry{}, false
}

func TestLadderInvariants(t *testing.T) {
	forEachSequence(t, runSequence)
}

func runSequence(seed uint64) error {
	s := newSim(seed)
	var lastSeatNudge time.Time
	lastNudge := map[ClaimKey]time.Time{}   // per entry life
	lastTrigger := map[ClaimKey]time.Time{} // trigger recycles, per entry life
	recycles := map[ClaimKey]int{}          // started recycles while the entry lives
	lastHeard := map[ClaimKey]time.Time{}   // a nudge or an alert, while the entry is actionable
	type counted struct {
		at      time.Time
		attempt int
		gen     int64
	}
	lastCounted := map[ClaimKey]counted{}  // the entry's last counted nudge
	unfinished := map[ClaimKey]time.Time{} // when the lane last reported a release kept
	poolAlerts := map[ClaimKey]int{}       // rate-limit alerts while the entry lives
	quietSince := map[ClaimKey]time.Time{} // liveness: since when a started claim has gone unheard while it could be heard
	for i := range 90 {
		prev := s.rec
		prevCopy := prev
		prevCopy.Entries = slices.Clone(prev.Entries)
		o := s.step()
		d := Decide(prev, o, p)
		fail := func(format string, a ...any) error {
			return fmt.Errorf("step %d at %v: %s\nprev %s\nobs %+v\nnext %s\nacts %+v", i, o.Now.Sub(t0), fmt.Sprintf(format, a...), EncodeLadder(prevCopy), o, EncodeLadder(d.Next), d.Actions)
		}

		// queuedFor: k is a started claim the rule would recycle fresh but
		// for a queued hold: it must be paced paused (never counted, never
		// recycled) until its turn comes.
		queuedFor := func(k ClaimKey) bool {
			pe, ok := entryOf(prev, k)
			e, now := entryOf(d.Next, k)
			if ok {
				pe = p.progressReset(pe, o) // progress since its last nudge restarts its count first, as Decide does
			}
			return ok && now && o.FreshHold == FreshHoldQueued && !o.Provider.Degraded && o.RateLimited >= DefaultRateLimitThreshold &&
				prev.Gen == o.Gen && pe.Kind == KindClaim && e.Kind == KindClaim && pe.Rung >= RungStarted && pe.Recycles < DefaultMaxRecycles &&
				pe.Attempts+pe.Paused > 0 && o.RateLimitedAt.After(pe.Last) && !o.Hold[k] && (prev.Release == nil || prev.Release.Key != k)
		}

		// WL7: pure and deterministic.
		if again := Decide(prev, o, p); !reflect.DeepEqual(again, d) {
			return fail("WL7: a second call differs: %+v", again)
		}
		if !reflect.DeepEqual(prev, prevCopy) {
			return fail("WL7: prev mutated")
		}
		// Changed is exactly "the stored text differs".
		if seed%97 == 0 && (len(prev.Entries) > 0 || prev.Release != nil || len(d.Next.Entries) > 0 || d.Next.Release != nil) &&
			d.Changed != (EncodeLadder(prev) != EncodeLadder(d.Next)) {
			return fail("Changed=%v disagrees with the cell text", d.Changed)
		}

		// An entry that is new, of a new kind, or gone starts a new life.
		for k := range lastNudge {
			if _, ok := entryOf(d.Next, k); !ok {
				delete(lastNudge, k)
				delete(lastCounted, k)
			}
		}
		for _, e := range d.Next.Entries {
			if pe, ok := entryOf(prev, e.Key); !ok || pe.Kind != e.Kind {
				delete(lastNudge, e.Key)
				delete(lastCounted, e.Key)
				delete(lastTrigger, e.Key)
				if !ok {
					delete(recycles, e.Key)
					delete(poolAlerts, e.Key)
				}
			}
		}

		nudges, fresh := 0, false
		for _, a := range d.Actions {
			switch a := a.(type) {
			case Nudge:
				nudges++
				// WL5: no nudge under a hold.
				if o.Gate.Held || o.Hold[a.Key] {
					return fail("WL5: nudge under a hold")
				}
				// WL4: no counted nudge while degraded, nor for a refused seat
				// whose fresh recycle is held.
				if o.Provider.Degraded && !a.Paused {
					return fail("WL4: counted nudge while degraded")
				}
				if queuedFor(a.Key) && !a.Paused {
					return fail("a counted nudge of %v, a started claim queued for its fresh recycle", a.Key)
				}
				// WL3: one nudge per seat per Base, whatever the work.
				if !lastSeatNudge.IsZero() && o.Now.Sub(lastSeatNudge) < DefaultBase {
					return fail("WL3: seat nudged %v after its last nudge", o.Now.Sub(lastSeatNudge))
				}
				lastSeatNudge = o.Now
				lastNudge[a.Key] = o.Now
				// A seat refusing nudges is retried no more often than PausedCap.
				if pe, ok := entryOf(prev, a.Key); ok && pe.Refused > 0 && pe.Kind == entryKind(d.Next, a.Key) && prev.Gen == o.Gen && o.Now.Sub(pe.Last) < DefaultPausedCap {
					return fail("WL3: %v nudged %v after a refused nudge", a.Key, o.Now.Sub(pe.Last))
				}
				// WL3: a counted nudge waits out its backoff: Base doubling
				// from the last counted nudge, never less.
				if !a.Paused {
					if c, ok := lastCounted[a.Key]; ok && c.gen == o.Gen && a.Attempt == c.attempt+1 {
						if want := min(DefaultBase<<max(c.attempt-1, 0), DefaultCap); o.Now.Sub(c.at) < want {
							return fail("WL3: attempt %d of %v %v after attempt %d, want >= %v", a.Attempt, a.Key, o.Now.Sub(c.at), c.attempt, want)
						}
					}
					lastCounted[a.Key] = counted{at: o.Now, attempt: a.Attempt, gen: o.Gen}
				}
				lastHeard[a.Key] = o.Now
			case Recycle:
				if o.Provider.Degraded {
					return fail("WL4: recycle while degraded")
				}
				if queuedFor(a.Key) {
					return fail("a recycle of %v, a started claim queued for its fresh recycle (fresh %v)", a.Key, a.Fresh)
				}
				if a.Fresh {
					fresh = true
					// The rate-limit rule recycles only a started claim of a
					// seat refused at least RateLimitThreshold times in a row,
					// the newest answering the ladder's own nudge, and never
					// while fresh recycles are held.
					if o.RateLimited < DefaultRateLimitThreshold || !a.KeepClaim || o.FreshHold != FreshHoldNone {
						return fail("a fresh recycle of %v with %d refusals (hold %v)", a.Key, o.RateLimited, o.FreshHold)
					}
					if e, ok := entryOf(d.Next, a.Key); !ok || e.Kind != KindClaim || e.Rung < RungStarted || !e.FreshRecycled {
						return fail("a fresh recycle of %v, not a started claim", a.Key)
					}
					if pe, ok := entryOf(prev, a.Key); !ok || pe.Attempts+pe.Paused == 0 || !o.RateLimitedAt.After(pe.Last) {
						return fail("a fresh recycle of %v on a refusal that answered no nudge (prev %+v, refused at %v)", a.Key, pe, o.RateLimitedAt.Sub(t0))
					}
				}
				if a.KeepClaim {
					if recycles[a.Key]++; recycles[a.Key] > DefaultMaxRecycles {
						return fail("WL3: %d recycles of started %v", recycles[a.Key], a.Key)
					}
				} else {
					if t := lastTrigger[a.Key]; !t.IsZero() && o.Now.Sub(t) < DefaultRecycleRepost {
						return fail("WL3: trigger recycles %v apart", o.Now.Sub(t))
					}
					lastTrigger[a.Key] = o.Now
				}
			case Release:
				// WL2: never release started work.
				if !o.StartedRead || o.Started[a.Key] != 0 || s.started[a.Key] {
					return fail("WL2: release of started %v", a.Key)
				}
				// WL3: a release that did not finish is retried at Cap.
				if pe, ok := entryOf(prev, a.Key); ok && pe.Unreleased && prev.Release == nil {
					if at := unfinished[a.Key]; o.Now.Sub(at) < DefaultCap {
						return fail("WL3: release of %v retried %v after it was kept", a.Key, o.Now.Sub(at))
					}
				}
			case Alert:
				if o.Provider.Degraded && a.Reason != AlertReleaseRacedStart {
					return fail("WL4: escalation while degraded")
				}
				if a.Reason == AlertProviderRateLimited || a.Reason == AlertRateLimitedUnrecycled || a.Reason == AlertRateLimitedAgain {
					if poolAlerts[a.Key]++; poolAlerts[a.Key] > 1 {
						return fail("a rate-limit alert for %v raised %d times in its life", a.Key, poolAlerts[a.Key])
					}
					e, _ := entryOf(d.Next, a.Key)
					recent := e.FreshRecycled && !e.FreshServed && o.Now.Sub(e.RecycleAt) <= DefaultCap
					if (a.Reason == AlertProviderRateLimited) != recent || (a.Reason == AlertRateLimitedUnrecycled) == e.FreshRecycled {
						return fail("%s for %v whose last recycle was fresh=%v %v ago", a.Reason, a.Key, e.FreshRecycled, o.Now.Sub(e.RecycleAt))
					}
				}
				lastHeard[a.Key] = o.Now
			}
		}
		if nudges > 1 {
			return fail("WL3: %d nudges in one pass", nudges)
		}
		if fresh && nudges > 0 {
			return fail("a nudge to a seat the pass recycled fresh")
		}
		if o.Provider.Degraded {
			if d.Next.Release != nil && (prev.Release == nil || *d.Next.Release != *prev.Release) {
				return fail("WL4: a release decided while degraded")
			}
			for _, e := range d.Next.Entries {
				if pe, ok := entryOf(prev, e.Key); ok && pe.Kind == e.Kind && prev.Gen == o.Gen && e.Attempts > pe.Attempts {
					return fail("WL4: %v attempts rose while degraded", e.Key)
				}
			}
		}

		// WL1: every entry of a nudgeable seat hears from the ladder (a
		// nudge or an alert) within Cap, give or take the passes it waits
		// for the seat's nudge slot; a release, kept, never silences it.
		eligible := !o.Gate.Held && o.Runtime == TriTrue && o.Quiet.State == Quiescent
		for _, e := range d.Next.Entries {
			since, ok := lastHeard[e.Key]
			switch {
			case !eligible || !ok || o.Hold[e.Key] || prev.Gen != o.Gen || (d.Next.Release != nil && d.Next.Release.Key == e.Key):
				lastHeard[e.Key] = o.Now
			case o.Now.Sub(since) > DefaultCap+3*maxStep+2*DefaultBase:
				return fail("WL1: %v of a quiet seat has not heard from the ladder for %v", e.Key, o.Now.Sub(since))
			}
		}
		for k := range lastHeard {
			if _, ok := entryOf(d.Next, k); !ok {
				delete(lastHeard, k)
			}
		}

		// Liveness: a started claim of an open, live, quiet seat, on a
		// provider that is not degraded and not queued for a fresh recycle,
		// hears an alert or a recycle within the ladder's own schedule (the
		// started rung's nudges, then Cap), until it is escalated: no hold
		// that has no end of its own silences it.
		for _, e := range d.Next.Entries {
			k := e.Key
			open := !o.Gate.Held && o.Runtime == TriTrue && o.Quiet.State == Quiescent && !o.Provider.Degraded &&
				!queuedFor(k) && !o.Hold[k] && prev.Gen == o.Gen && (d.Next.Release == nil || d.Next.Release.Key != k)
			switch {
			case e.Kind != KindClaim || e.Rung < RungStarted || e.Escalated || e.RateLimitAlerted || !open:
				delete(quietSince, k)
				continue
			case quietSince[k].IsZero() || o.LastActivity.After(quietSince[k]):
				quietSince[k] = o.Now
			}
			for _, a := range d.Actions {
				switch a := a.(type) {
				case Recycle:
					if a.Key == k {
						quietSince[k] = o.Now
					}
				case Alert:
					if a.Key == k {
						quietSince[k] = o.Now
					}
				}
			}
			if o.Now.Sub(quietSince[k]) > livenessBound {
				return fail("liveness: started claim %v quiet for %v with no alert or recycle (hold %v, refused %d)", k, o.Now.Sub(quietSince[k]), o.FreshHold, o.RateLimited)
			}
		}

		// The lane: commit through the cell's text, then finish releases:
		// mostly released, else kept (ReleaseUnfinished), else (a crash)
		// left pending; and some nudges are refused (NudgeRefused).
		if d.Changed {
			r, err := DecodeLadder(EncodeLadder(d.Next))
			if err != nil {
				return fail("the cell does not decode: %v", err)
			}
			s.rec = r
		}
		if s.refusing {
			for _, a := range d.Actions {
				if _, ok := a.(Nudge); ok {
					s.obs.RateLimitedAt = o.Now.Add(time.Second)
				}
			}
		}
		for _, a := range d.Actions {
			if n, ok := a.(Nudge); ok && !n.Paused && s.event(10) { // only a counted nudge is refused (the lane's rule)
				if r, _, ok := NudgeRefused(s.rec, n.Key, n.Attempt, o.Now); ok {
					s.rec = r
				}
			}
		}
		for _, a := range d.Actions {
			switch a := a.(type) {
			case Release:
				switch n := s.rng.IntN(10); {
				case n < 6:
					s.drop(a.Key)
				case n < 9:
					if r, ok := ReleaseUnfinished(s.rec, a.Key, s.gen, o.Now); ok {
						s.rec = r
						unfinished[a.Key] = o.Now
					}
				}
			case Recycle: // A16 drains the seat; it wakes at a new generation
				if s.chance(70) {
					s.gen++
				}
			}
		}
	}
	return nil
}

// WL8: whatever a row carried before the ladder first sees it (no cell, or
// a cell for another generation with any counts), no recycle or release
// lands within Grace + Base + 2·Base of continuous quiet.
func TestNoDrainWave(t *testing.T) {
	window := DefaultGrace + DefaultBase + 2*DefaultBase
	forEachSequence(t, func(seq uint64) error {
		rng := rand.New(rand.NewPCG(seq, 0x7761766521))
		var prev Record
		var work []Work
		started := map[ClaimKey]StartSource{}
		for _, k := range simKeys {
			kind := Kind(1 + rng.IntN(3))
			work = append(work, Work{Key: k, Kind: kind})
			if kind == KindClaim && rng.IntN(2) == 0 {
				started[k] = StartSource(1 + rng.IntN(4))
			}
		}
		if rng.IntN(2) == 0 { // a cell from an earlier generation, at its limits
			prev = Record{V: 1, Gen: 41}
			for _, w := range work {
				kind := w.Kind
				if rng.IntN(3) == 0 {
					kind = Kind(1 + rng.IntN(3))
				}
				prev.Entries = append(prev.Entries, Entry{
					Key: w.Key, Kind: kind, Rung: initialRung(kind, started[w.Key] != 0),
					Since: t0.Add(-10 * time.Hour), Attempts: 6 + rng.IntN(50), Paused: rng.IntN(9),
					Last: t0.Add(-time.Duration(rng.IntN(120)) * time.Minute), RecycleAt: t0.Add(-5 * time.Hour),
				})
			}
		}
		r := prev
		refused := rng.IntN(5) // refusals already in the transcript at deploy
		for now := t0; now.Sub(t0) < window; now = now.Add(time.Duration(1+rng.IntN(60)) * time.Second) {
			o := quiet(now, 42, work...)
			o.RateLimited, o.RateLimitedAt = refused, t0.Add(-time.Duration(1+rng.IntN(600))*time.Minute)
			if r.V == 0 || r.Gen != 42 {
				o.Started = started
			} else {
				o.Started = map[ClaimKey]StartSource{}
				for k, src := range started {
					if src == SrcAgent {
						o.Started[k] = src
					}
				}
			}
			d := Decide(r, o, p)
			for _, a := range d.Actions {
				switch a.(type) {
				case Recycle, Release:
					return fmt.Errorf("%T at %v after deploy (prev %s)", a, now.Sub(t0), EncodeLadder(prev))
				}
			}
			if d.Changed {
				r = d.Next
			}
		}
		return nil
	})
}

func entryKind(r Record, k ClaimKey) Kind {
	e, _ := entryOf(r, k)
	return e.Kind
}

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
	workers := min(runtime.NumCPU(), 16)
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
	lastStep time.Duration
	churn    int // 0: a calm seat that only gains work; 1: everything flips
}

var simKeys = []ClaimKey{key("a"), key("b"), key("c")}

const maxStep = 20 * time.Minute

func newSim(seed uint64) *sim {
	s := &sim{
		rng: rand.New(rand.NewPCG(seed, 0x6c6164646572)), now: t0, gen: 1,
		kinds: map[ClaimKey]Kind{}, agent: map[ClaimKey]bool{}, seeds: map[ClaimKey]StartSource{}, started: map[ClaimKey]bool{},
	}
	s.obs = Observation{StartedRead: true, Runtime: TriTrue, Quiet: Quiet{State: Quiescent}}
	if s.chance(70) {
		s.churn = 1
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
		if s.obs.Gate.Held && s.chance(50) {
			s.obs.Gate.Until = s.now.Add(time.Duration(s.rng.IntN(13*60)) * time.Minute)
		}
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
	s.obs.StartedRead = !s.event(3)
	s.obs.LastActivity = s.now.Add(-time.Duration(s.rng.IntN(60)) * time.Minute)

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
	type keyHist struct {
		lastNudge, lastTriggerRecycle time.Time
		gen                           int64
	}
	hist := map[ClaimKey]*keyHist{}
	recycles := map[ClaimKey]int{}      // started recycles while the entry lives
	lastAct := map[ClaimKey]time.Time{} // per live entry, while the seat is nudgeable
	for i := range 90 {
		prev := s.rec
		prevCopy := prev
		prevCopy.Entries = slices.Clone(prev.Entries)
		o := s.step()
		d := Decide(prev, o, p)
		fail := func(format string, a ...any) error {
			return fmt.Errorf("step %d at %v: %s\nprev %s\nobs %+v\nnext %s\nacts %+v", i, o.Now.Sub(t0), fmt.Sprintf(format, a...), EncodeLadder(prevCopy), o, EncodeLadder(d.Next), d.Actions)
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

		live := 0
		for _, e := range d.Next.Entries {
			if d.Next.Release == nil || d.Next.Release.Key != e.Key {
				live++
			}
		}
		// WL1: never zero.
		if (live > 0 || d.Next.Release != nil) && d.Due.IsZero() {
			return fail("WL1: zero Due with work outstanding")
		}
		if live > 0 && !o.Gate.Held && d.Due.After(o.Now.Add(DefaultCap)) {
			return fail("WL1: Due %v past now+Cap", d.Due.Sub(o.Now))
		}
		// WL1: every entry of a nudgeable seat is acted on within Cap, give
		// or take the passes it waits for the one nudge slot.
		eligible := !o.Gate.Held && o.Runtime == TriTrue && o.Quiet.State == Quiescent
		for _, a := range d.Actions {
			lastAct[keyOf(a)] = o.Now
		}
		for _, e := range d.Next.Entries {
			since, ok := lastAct[e.Key]
			switch {
			case !eligible || !ok || prev.Gen != o.Gen || (d.Next.Release != nil && d.Next.Release.Key == e.Key):
				// not nudgeable, new, restarted by a new generation, or
				// being released: its clock starts now
				lastAct[e.Key] = o.Now
			case o.Now.Sub(since) > DefaultCap+3*maxStep:
				return fail("WL1: %v of a quiet seat has had no action for %v", e.Key, o.Now.Sub(since))
			}
		}
		for k := range lastAct {
			if _, ok := entryOf(d.Next, k); !ok {
				delete(lastAct, k)
			}
		}

		// WL3: one nudge per pass.
		if countNudges(d) > 1 {
			return fail("WL3: %d nudges in one pass", countNudges(d))
		}
		for _, a := range d.Actions {
			if n, ok := a.(Note); ok {
				delete(hist, n.Key) // the entry restarted: a new pacing history
				if n.Event == NoteDropped {
					delete(recycles, n.Key)
				}
			}
		}
		for _, a := range d.Actions {
			h := hist[keyOf(a)]
			if h == nil || h.gen != o.Gen {
				h = &keyHist{gen: o.Gen}
				hist[keyOf(a)] = h
			}
			switch a := a.(type) {
			case Nudge:
				// WL5: no nudge under a hold.
				if o.Gate.Held {
					return fail("WL5: nudge under a held gate")
				}
				// WL4: no counted nudge while degraded.
				if o.Provider.Degraded && !a.Paused {
					return fail("WL4: counted nudge while degraded")
				}
				if !h.lastNudge.IsZero() && o.Now.Sub(h.lastNudge) < DefaultBase {
					return fail("WL3: nudges of %v %v apart", a.Key, o.Now.Sub(h.lastNudge))
				}
				h.lastNudge = o.Now
			case Recycle:
				if o.Provider.Degraded {
					return fail("WL4: recycle while degraded")
				}
				if a.KeepClaim {
					if recycles[a.Key]++; recycles[a.Key] > DefaultMaxRecycles {
						return fail("WL3: %d recycles of started %v", recycles[a.Key], a.Key)
					}
				} else {
					if !h.lastTriggerRecycle.IsZero() && o.Now.Sub(h.lastTriggerRecycle) < DefaultRecycleRepost {
						return fail("WL3: trigger recycles %v apart", o.Now.Sub(h.lastTriggerRecycle))
					}
					h.lastTriggerRecycle = o.Now
				}
			case Release:
				// WL2: never release started work.
				if !o.StartedRead || o.Started[a.Key] != 0 || s.started[a.Key] {
					return fail("WL2: release of started %v", a.Key)
				}
			case Alert:
				if o.Provider.Degraded && a.Reason != AlertReleaseRacedStart {
					return fail("WL4: escalation while degraded")
				}
			}
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

		// The lane: commit, then mostly finish releases.
		if d.Changed {
			s.rec = d.Next
		}
		for _, a := range d.Actions {
			switch a := a.(type) {
			case Release:
				if s.chance(70) {
					s.drop(a.Key)
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

func keyOf(a Action) ClaimKey {
	switch a := a.(type) {
	case Nudge:
		return a.Key
	case Recycle:
		return a.Key
	case Release:
		return a.Key
	case Alert:
		return a.Key
	case Note:
		return a.Key
	}
	panic("unknown action")
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
		for now := t0; now.Sub(t0) < window; now = now.Add(time.Duration(1+rng.IntN(60)) * time.Second) {
			o := quiet(now, 42, work...)
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

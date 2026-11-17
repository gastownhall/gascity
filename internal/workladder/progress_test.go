package workladder

import (
	"testing"
	"time"
)

// A failed turn is never progress (#8132 split C1). On a pass whose reading
// of the seat's turns shows the newest ended turn failed, only a turn the
// provider completed at least ProgressReset after the entry's last nudge
// restarts its count; on any other pass, as before, the activity clock
// does. And progress is judged only on a pass that may act on the entry:
// gate open, runtime alive, seat quiet, its live read told.
func TestDecideProgressTable(t *testing.T) {
	last := t0.Add(10 * time.Minute)
	now := last.Add(40 * time.Minute)
	act := last.Add(DefaultProgressReset + 9*time.Minute) // the seat's newest activity: the failed turn's end
	before := last.Add(-time.Hour)                        // a turn served before the nudge
	after := last.Add(DefaultProgressReset + 2*time.Minute)
	prev := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 3, Last: last})
	served := func(at time.Time) TurnReading { return TurnReading{Read: true, Conv: 1, ServedAt: at} }
	failed := func(at time.Time) TurnReading {
		return TurnReading{Read: true, Conv: 1, LastFailed: true, ServedAt: at}
	}

	for _, tc := range []struct {
		name  string
		turns TurnReading
		pass  func(*Observation)
		reset time.Time // the reset's new Since; zero: the count stands
	}{
		{"newest completed", served(act), nil, act},
		{"newest failed (a 429 or a 524), served only before the nudge", failed(before), nil, time.Time{}},
		{"newest failed, never served", failed(time.Time{}), nil, time.Time{}},
		{"newest failed, served past the reset behind it", failed(after), nil, after},
		{"newest failed, served under the reset", failed(last.Add(DefaultProgressReset - time.Second)), nil, time.Time{}},
		{"no status (Gemini): the activity clock", served(time.Time{}), nil, act},
		{"a turn open: the activity clock", TurnReading{Read: true, Conv: 1}, nil, act},
		{"the read failed: the activity clock", TurnReading{}, nil, act},
		{"a failed reading of another conversation is no reading", TurnReading{Read: true, Conv: 2, LastFailed: true}, nil, act},
		{"degraded acts: newest completed", served(act), func(o *Observation) { o.Provider.Degraded = true }, act},
		{"degraded acts: newest failed", failed(before), func(o *Observation) { o.Provider.Degraded = true }, time.Time{}},
		{"busy judges nothing", served(act), func(o *Observation) { o.Quiet = Quiet{State: NotQuiet} }, time.Time{}},
		{"quiet unread judges nothing", served(act), func(o *Observation) { o.Quiet = Quiet{} }, time.Time{}},
		{"gated judges nothing", served(act), func(o *Observation) { o.Gate = Gate{Held: true} }, time.Time{}},
		{"runtime unknown judges nothing", served(act), func(o *Observation) { o.Runtime = TriUnknown }, time.Time{}},
		{"its live read could not tell: judges nothing", served(act), func(o *Observation) { o.Hold = map[ClaimKey]bool{key("s"): true} }, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := startedBy1(quiet(now, 1, claim("s")), "s", SrcAgent)
			o.Conv, o.LastActivity, o.Turns = 1, act, tc.turns
			if tc.pass != nil {
				tc.pass(&o)
			}
			e := entry(t, Decide(prev, o, p), "s")
			switch {
			case tc.reset.IsZero() && (!e.Since.Equal(t0) || e.Attempts+e.Paused < 3):
				t.Fatalf("entry %+v: want the count standing", e)
			case !tc.reset.IsZero() && (!e.Since.Equal(tc.reset) || e.Attempts+e.Paused > 1):
				t.Fatalf("entry %+v: want the count reset from %v", e, tc.reset.Sub(t0))
			}
		})
	}
}

// On main a seat whose every answer is a turn its provider failed, ending
// ProgressReset or more after the nudge it answers, never escalates: each
// failure is activity that restarts the count. A failed turn is not
// progress, so the seat climbs the ladder to its recycle.
func TestDecideSlowFailuresClimbTheLadder(t *testing.T) {
	for _, after := range []time.Duration{2 * time.Second, 9*time.Minute + 59*time.Second, 10*time.Minute + 30*time.Second, 19 * time.Minute} {
		t.Run(after.String(), func(t *testing.T) {
			var r Record
			var nudges []time.Time
			recycled := false
			for now := t0; now.Before(t0.Add(4*time.Hour)) && !recycled; now = now.Add(30 * time.Second) {
				o := startedBy1(quiet(now, 1, claim("s")), "s", SrcAgent)
				o.Conv = 1
				var lastFail time.Time
				for _, n := range nudges {
					if end := n.Add(after); !end.After(now) {
						lastFail = end
					}
				}
				if !lastFail.IsZero() {
					o.LastActivity = lastFail
					o.Turns = TurnReading{Read: true, Conv: 1, LastFailed: true}
				}
				d := Decide(r, o, p)
				for _, a := range d.Actions {
					switch a.(type) {
					case Nudge:
						nudges = append(nudges, now)
					case Recycle:
						recycled = true
					}
				}
				r = d.Next
			}
			if !recycled {
				t.Fatalf("nudged %d times over 4h and never recycled: a failed turn counted as progress", len(nudges))
			}
		})
	}
}

// The same seat answered by completed turns is making progress: its count
// restarts, and it is never recycled.
func TestDecideSlowServiceIsProgress(t *testing.T) {
	var r Record
	var nudges []time.Time
	for now := t0; now.Before(t0.Add(4 * time.Hour)); now = now.Add(30 * time.Second) {
		o := startedBy1(quiet(now, 1, claim("s")), "s", SrcAgent)
		o.Conv = 1
		var lastServed time.Time
		for _, n := range nudges {
			if end := n.Add(19 * time.Minute); !end.After(now) {
				lastServed = end
			}
		}
		if !lastServed.IsZero() {
			o.LastActivity = lastServed
			o.Turns = TurnReading{Read: true, Conv: 1, ServedAt: lastServed}
		}
		d := Decide(r, o, p)
		for _, a := range d.Actions {
			switch a.(type) {
			case Nudge:
				nudges = append(nudges, now)
			case Recycle, Alert:
				t.Fatalf("%v at %v: a served seat is making progress", a, now.Sub(t0))
			}
		}
		r = d.Next
	}
}

// The ladder's invariants hold, and progress is judged as above, when the
// caller reads the seat's turns on some passes and hands some nudges to an
// asynchronous delivery.
func TestPropertyProgressNeedsService(t *testing.T) {
	forEachSequence(t, func(seed uint64) error {
		s := newSim(seed)
		s.turns, s.marks = true, seed%2 == 0
		return s.run()
	})
}

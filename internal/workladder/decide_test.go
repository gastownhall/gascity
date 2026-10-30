package workladder

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func claim(b string) Work { return Work{Key: key(b), Kind: KindClaim} }
func trig(b string) Work  { return Work{Key: key(b), Kind: KindTrigger} }
func cont(b string) Work  { return Work{Key: key(b), Kind: KindContinuation} }

// quiet is an observation of an open, alive, quiet, healthy seat.
func quiet(now time.Time, gen int64, work ...Work) Observation {
	return Observation{
		Now: now, Gen: gen, Outstanding: work, Started: map[ClaimKey]StartSource{}, StartedRead: true,
		Runtime: TriTrue, Quiet: Quiet{State: Quiescent},
	}
}

func startedBy1(o Observation, b string, src StartSource) Observation {
	o.Started = map[ClaimKey]StartSource{key(b): src}
	return o
}

// rec is a stored cell: entries in key order, each Next stamped the way
// Decide stamps it.
func rec(gen int64, es ...Entry) Record {
	for i := range es {
		es[i].Next = p.dueAt(es[i], false)
	}
	return Record{V: 1, Gen: gen, Entries: sortedEntries(es)}
}

// effects is d's actions.
func effects(d Decision) []Action { return d.Actions }

// due is when d's cell is next due on a quiet, healthy seat: its earliest
// entry not being released; zero for none.
func due(d Decision) time.Time {
	var at time.Time
	for _, e := range d.Next.Entries {
		if d.Next.Release != nil && d.Next.Release.Key == e.Key {
			continue
		}
		if n := p.dueAt(e, false); at.IsZero() || n.Before(at) {
			at = n
		}
	}
	return at
}

func entry(t *testing.T, d Decision, b string) Entry {
	t.Helper()
	for _, e := range d.Next.Entries {
		if e.Key == key(b) {
			return e
		}
	}
	t.Fatalf("no entry %s in %+v", b, d.Next)
	return Entry{}
}

func wantEffects(t *testing.T, d Decision, want ...Action) {
	t.Helper()
	if got := effects(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("effects = %#v\nwant %#v", got, want)
	}
}

var p = DefaultPolicy()

// Step 1: entries follow Outstanding; rungs start where the evidence puts
// them and only rise.
func TestDecideReconcile(t *testing.T) {
	t.Run("new entries start at their initial rung, uncounted", func(t *testing.T) {
		o := startedBy1(quiet(t0, 3, trig("t"), cont("c"), claim("u"), claim("s")), "s", SrcCurrentClaim)
		d := Decide(Record{}, o, p)
		for b, want := range map[string]Rung{"t": RungAssigned, "c": RungAssigned, "u": RungClaimed, "s": RungStarted} {
			e := entry(t, d, b)
			if e.Rung != want || e.Attempts != 0 || e.Paused != 0 || !e.Since.Equal(t0) {
				t.Errorf("%s: %+v, want rung %v uncounted since t0", b, e, want)
			}
		}
		if e := entry(t, d, "s"); e.Seed != SrcCurrentClaim {
			t.Errorf("seed = %v, want current_claim", e.Seed)
		}
		if !d.Changed || d.Next.Gen != 3 {
			t.Errorf("Changed=%v gen=%d, want a write at gen 3", d.Changed, d.Next.Gen)
		}
		wantEffects(t, d) // within grace: nothing
		if !due(d).Equal(t0.Add(DefaultGrace)) {
			t.Errorf("due = %v, want t0+grace", due(d))
		}
	})
	t.Run("an unreadable agent cell names every claim started", func(t *testing.T) {
		o := quiet(t0, 1, claim("u"), trig("t"))
		o.StartedRead = false
		d := Decide(Record{}, o, p)
		if e := entry(t, d, "u"); e.Rung != RungStarted {
			t.Errorf("claim rung %v, want started", e.Rung)
		}
		if e := entry(t, d, "t"); e.Rung != RungAssigned {
			t.Errorf("trigger rung %v, want assigned", e.Rung)
		}
	})
	t.Run("a rung rise resets the count, the rung clock and the alert", func(t *testing.T) {
		prev := rec(1, Entry{Key: key("u"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 2, Paused: 1, Last: t0.Add(5 * time.Minute), Escalated: true, Recycles: 1})
		now := t0.Add(6 * time.Minute)
		d := Decide(prev, startedBy1(quiet(now, 1, claim("u")), "u", SrcAgent), p)
		e := entry(t, d, "u")
		if e.Rung != RungStarted || e.Attempts != 0 || e.Paused != 0 || !e.Since.Equal(now) || e.Escalated || e.Seed != SrcAgent || e.Recycles != 1 {
			t.Fatalf("after rise: %+v", e)
		}
	})
	t.Run("a rung never falls when the evidence goes away", func(t *testing.T) {
		prev := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 3, Last: t0, Seed: SrcCurrentClaim})
		d := Decide(prev, quiet(t0.Add(time.Minute), 1, claim("s")), p)
		if e := entry(t, d, "s"); e.Rung != RungStarted || e.Attempts != 3 {
			t.Fatalf("entry %+v, want started with its count", e)
		}
		prev = rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungEscalated, Since: t0, Attempts: 9, Last: t0})
		d = Decide(prev, startedBy1(quiet(t0.Add(time.Minute), 1, claim("s")), "s", SrcAgent), p)
		if e := entry(t, d, "s"); e.Rung != RungEscalated || e.Attempts != 9 {
			t.Fatalf("entry %+v, want escalated kept", e)
		}
	})
	t.Run("a finished entry is dropped", func(t *testing.T) {
		prev := rec(1, Entry{Key: key("a"), Kind: KindClaim, Rung: RungClaimed, Since: t0}, Entry{Key: key("b"), Kind: KindTrigger, Rung: RungAssigned, Since: t0})
		d := Decide(prev, quiet(t0, 1, trig("b")), p)
		if len(d.Next.Entries) != 1 || d.Next.Entries[0].Key != key("b") {
			t.Fatalf("entries %+v, want only b", d.Next.Entries)
		}
	})
	t.Run("work that changes kind starts over but keeps its recycles", func(t *testing.T) {
		prev := rec(1, Entry{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 3, Last: t0, Recycles: 1, RecycleAt: t0})
		now := t0.Add(time.Minute)
		d := Decide(prev, quiet(now, 1, claim("x")), p)
		e := entry(t, d, "x")
		if e.Kind != KindClaim || e.Rung != RungClaimed || e.Attempts != 0 || !e.Since.Equal(now) || e.Recycles != 1 || !e.RecycleAt.Equal(t0) {
			t.Fatalf("entry %+v", e)
		}
	})
	t.Run("nothing outstanding and no cell writes nothing", func(t *testing.T) {
		d := Decide(Record{}, quiet(t0, 4), p)
		if d.Changed || !due(d).IsZero() || len(d.Actions) != 0 {
			t.Fatalf("decision %+v, want no write, no due", d)
		}
		d = Decide(rec(2), quiet(t0, 4), p)
		if d.Changed {
			t.Fatal("an empty cell at another generation must not be rewritten")
		}
		d = Decide(rec(4, Entry{Key: key("a"), Kind: KindClaim, Rung: RungClaimed, Since: t0}), quiet(t0, 4), p)
		if !d.Changed || len(d.Next.Entries) != 0 {
			t.Fatalf("emptying a cell must write it: %+v", d)
		}
	})
}

// Step 2: a new generation restarts every count, keeping recycles, the
// alert and the rung.
func TestDecideGenerationReset(t *testing.T) {
	prev := rec(5, Entry{Key: key("s"), Kind: KindClaim, Rung: RungEscalated, Since: t0, Attempts: 8, Paused: 2, Last: t0, Recycles: 1, Escalated: true, Seed: SrcAgent})
	now := t0.Add(3 * time.Hour)
	d := Decide(prev, quiet(now, 6, claim("s")), p)
	e := entry(t, d, "s")
	if e.Attempts != 0 || e.Paused != 0 || !e.Since.Equal(now) || e.Recycles != 1 || !e.Escalated || e.Rung != RungEscalated || d.Next.Gen != 6 {
		t.Fatalf("after generation change: %+v gen %d", e, d.Next.Gen)
	}
	wantEffects(t, d) // a fresh grace
	if !due(d).Equal(now.Add(DefaultGrace)) {
		t.Fatalf("due = %v, want a fresh grace", due(d))
	}
}

// Step 3: a pending release is retried, cleared, or alarmed.
func TestDecidePendingRelease(t *testing.T) {
	mark := &ReleaseMark{Key: key("u"), Gen: 1, At: t0}
	prev := rec(1, Entry{Key: key("u"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Last: t0})
	prev.Release = mark
	now := t0.Add(time.Minute)

	t.Run("still outstanding and unstarted: re-emit", func(t *testing.T) {
		o := quiet(now, 1, claim("u"))
		o.Gate = Gate{Held: true, Reason: "draining"} // not gated: the release is already decided
		d := Decide(prev, o, p)
		wantEffects(t, d, Release{Key: key("u")})
		if d.Next.Release == nil || *d.Next.Release != *mark {
			t.Fatalf("mark %+v, want kept", d.Next.Release)
		}
	})
	t.Run("gone: clear", func(t *testing.T) {
		d := Decide(prev, quiet(now, 1), p)
		wantEffects(t, d)
		if d.Next.Release != nil || !d.Changed {
			t.Fatalf("mark %+v changed %v, want cleared", d.Next.Release, d.Changed)
		}
	})
	t.Run("started since: clear and alarm", func(t *testing.T) {
		d := Decide(prev, startedBy1(quiet(now, 1, claim("u")), "u", SrcAgent), p)
		wantEffects(t, d, Alert{Key: key("u"), Reason: AlertReleaseRacedStart})
		if d.Next.Release != nil {
			t.Fatal("mark kept after a start")
		}
	})
	t.Run("agent cell unreadable: hold the mark, do not act", func(t *testing.T) {
		o := quiet(now, 1, claim("u"))
		o.StartedRead = false
		d := Decide(prev, o, p)
		for _, a := range effects(d) {
			if _, ok := a.(Release); ok {
				t.Fatalf("released with an unreadable agent cell: %+v", d.Actions)
			}
		}
		if d.Next.Release == nil {
			t.Fatal("mark dropped while the agent cell is unreadable")
		}
	})
	t.Run("a second unstarted claim waits for the first release", func(t *testing.T) {
		prev := prev
		prev.Entries = append(prev.Entries, Entry{Key: key("v"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Last: t0})
		d := Decide(prev, quiet(t0.Add(time.Hour), 1, claim("u"), claim("v")), p)
		wantEffects(t, d, Release{Key: key("u")})
		if e := entry(t, d, "v"); e.Attempts != 3 || !e.Last.Equal(t0) {
			t.Fatalf("v %+v, want it waiting untouched", e)
		}
	})
}

// Steps 4-6: a held gate, a runtime not proven alive, a seat not proven
// quiet, or work whose live read could not tell get no action.
func TestDecideHolds(t *testing.T) {
	prev := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: t0})
	now := t0.Add(2 * time.Hour) // long past due
	cases := map[string]func(*Observation){
		"draining":            func(o *Observation) { o.Gate = Gate{Held: true, Reason: "draining"} },
		"heartbeat hold":      func(o *Observation) { o.Gate = Gate{Held: true, Reason: "heartbeat"} },
		"runtime dead":        func(o *Observation) { o.Runtime = TriFalse },
		"runtime unknown":     func(o *Observation) { o.Runtime = TriUnknown },
		"not quiet":           func(o *Observation) { o.Quiet = Quiet{State: NotQuiet, Reason: "snapshot_not_idle"} },
		"quiet unknown":       func(o *Observation) { o.Quiet = Quiet{} },
		"held while degraded": func(o *Observation) { o.Gate = Gate{Held: true}; o.Provider.Degraded = true },
		"live read held":      func(o *Observation) { o.Hold = map[ClaimKey]bool{key("s"): true} },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			o := quiet(now, 1, claim("s"))
			mut(&o)
			d := Decide(prev, o, p)
			wantEffects(t, d)
			if d.Changed {
				t.Fatalf("a hold must not write: %+v", d.Next)
			}
		})
	}
}

// Activity ProgressReset past the last nudge restarts the count, on any
// pass where the runtime is alive, whether the ladder saw the seat busy or
// not (a gate, an outage, the ladder off): its grace restarts from the
// activity. A runtime not proven alive proves no progress.
func TestDecideProgressReset(t *testing.T) {
	last := t0.Add(10 * time.Minute)
	prev := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Paused: 1, Last: last, Unreleased: true})
	now := last.Add(40 * time.Minute)
	act := last.Add(DefaultProgressReset)
	for name, mut := range map[string]func(*Observation){
		"busy":         func(o *Observation) { o.Quiet = Quiet{State: NotQuiet} },
		"quiet now":    func(*Observation) {},
		"behind gate":  func(o *Observation) { o.Gate = Gate{Held: true} },
		"degraded now": func(o *Observation) { o.Provider.Degraded = true },
	} {
		o := quiet(now, 1, claim("s"))
		o.LastActivity = act
		mut(&o)
		d := Decide(prev, o, p)
		e := entry(t, d, "s")
		// Reset, and on a quiet seat nudged afresh at once: its grace ran
		// from the activity.
		if e.Attempts+e.Paused > 1 || e.Unreleased || !e.Since.Equal(act) || !d.Changed {
			t.Fatalf("%s: worked %v past the nudge: %+v, want the count reset from the activity", name, DefaultProgressReset, e)
		}
		for _, a := range effects(d) {
			if _, ok := a.(Release); ok {
				t.Fatalf("%s: released on a count the seat's progress reset", name)
			}
		}
	}
	o := quiet(now, 1, claim("s"))
	o.LastActivity = last.Add(DefaultProgressReset - time.Second)
	o.Gate = Gate{Held: true}
	if d := Decide(prev, o, p); entry(t, d, "s").Attempts != 3 || d.Changed {
		t.Fatalf("worked under %v: want the count kept", DefaultProgressReset)
	}
	o = quiet(now, 1, claim("s"))
	o.LastActivity, o.Runtime = act, TriUnknown
	if d := Decide(prev, o, p); entry(t, d, "s").Attempts != 3 {
		t.Fatal("a runtime not proven alive reset the count")
	}
}

// Step 7: when a quiet seat's entry is due.
func TestDueAt(t *testing.T) {
	last := t0.Add(time.Hour)
	cases := []struct {
		name     string
		e        Entry
		degraded bool
		want     time.Time
	}{
		{"uncounted: a grace after since", Entry{Kind: KindClaim, Rung: RungStarted, Since: t0}, false, t0.Add(90 * time.Second)},
		{"1 attempt: base", Entry{Kind: KindClaim, Rung: RungStarted, Attempts: 1, Last: last}, false, last.Add(3 * time.Minute)},
		{"2: doubled", Entry{Kind: KindClaim, Rung: RungStarted, Attempts: 2, Last: last}, false, last.Add(6 * time.Minute)},
		{"3", Entry{Kind: KindClaim, Rung: RungStarted, Attempts: 3, Last: last}, false, last.Add(12 * time.Minute)},
		{"4", Entry{Kind: KindClaim, Rung: RungStarted, Attempts: 4, Last: last}, false, last.Add(24 * time.Minute)},
		{"5", Entry{Kind: KindClaim, Rung: RungStarted, Attempts: 5, Last: last}, false, last.Add(48 * time.Minute)},
		{"6: capped", Entry{Kind: KindClaim, Rung: RungStarted, Attempts: 6, Last: last}, false, last.Add(60 * time.Minute)},
		{"60: capped, no overflow", Entry{Kind: KindContinuation, Rung: RungAssigned, Attempts: 60, Last: last}, false, last.Add(60 * time.Minute)},
		{"at threshold: the backoff, then the threshold step", Entry{Kind: KindTrigger, Rung: RungAssigned, Attempts: 3, Last: last}, false, last.Add(12 * time.Minute)},
		{"past threshold: cap", Entry{Kind: KindTrigger, Rung: RungAssigned, Attempts: 4, Last: last}, false, last.Add(60 * time.Minute)},
		{"escalated: cap", Entry{Kind: KindClaim, Rung: RungEscalated, Attempts: 1, Last: last}, false, last.Add(60 * time.Minute)},
		{"degraded: paused cap", Entry{Kind: KindClaim, Rung: RungStarted, Attempts: 5, Last: last}, true, last.Add(10 * time.Minute)},
		{"degraded: the paused cap is a floor too", Entry{Kind: KindClaim, Rung: RungStarted, Attempts: 1, Last: last}, true, last.Add(10 * time.Minute)},
		{"degraded, only paused nudges", Entry{Kind: KindClaim, Rung: RungStarted, Paused: 3, Last: last}, true, last.Add(10 * time.Minute)},
		{"release unfinished: cap", Entry{Kind: KindClaim, Rung: RungClaimed, Attempts: 3, Last: last, Unreleased: true}, false, last.Add(60 * time.Minute)},
		{"only paused nudges: base", Entry{Kind: KindClaim, Rung: RungStarted, Paused: 2, Last: last}, false, last.Add(3 * time.Minute)},
	}
	for _, tc := range cases {
		if got := p.dueAt(tc.e, tc.degraded); !got.Equal(tc.want) {
			t.Errorf("%s: dueAt = %v, want %v", tc.name, got.Sub(t0), tc.want.Sub(t0))
		}
	}
}

// Step 8: what a due entry gets, by kind and rung.
func TestDecideDue(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	due := func(e Entry) Entry { e.Since, e.Last = t0, t0; return e }
	cases := []struct {
		name  string
		e     Entry
		obs   func(Observation) Observation
		want  []Action
		check func(*testing.T, Entry, Decision)
	}{
		{
			name: "below threshold: a counted nudge",
			e:    due(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 2}),
			want: []Action{Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 3}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if e.Attempts != 3 || !e.Last.Equal(now) {
					t.Errorf("entry %+v", e)
				}
			},
		},
		{
			name: "degraded: a paused nudge, uncounted, never escalated",
			e:    due(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 6}),
			obs:  func(o Observation) Observation { o.Provider = ProviderState{Degraded: true, Reason: "503"}; return o },
			want: []Action{Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 6, Paused: true}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if e.Attempts != 6 || e.Paused != 1 || e.Recycles != 0 || !e.Last.Equal(now) {
					t.Errorf("entry %+v", e)
				}
			},
		},
		{
			name: "degraded unstarted claim at threshold: no release",
			e:    due(Entry{Key: key("x"), Kind: KindClaim, Rung: RungClaimed, Attempts: 3}),
			obs:  func(o Observation) Observation { o.Provider.Degraded = true; return o },
			want: []Action{Nudge{Key: key("x"), Kind: KindClaim, Rung: RungClaimed, Attempt: 3, Paused: true}},
		},
		{
			name: "trigger at threshold: recycle, keep nudging",
			e:    due(Entry{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Attempts: 3}),
			want: []Action{Recycle{Key: key("x")}, Nudge{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Attempt: 4}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if !e.RecycleAt.Equal(now) {
					t.Errorf("recycle_at %v", e.RecycleAt)
				}
			},
		},
		{
			name: "trigger: no repost within RecycleRepost",
			e: func() Entry {
				e := due(Entry{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Attempts: 4})
				e.RecycleAt = now.Add(-29 * time.Minute)
				return e
			}(),
			want: []Action{Nudge{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Attempt: 5}},
		},
		{
			name: "trigger: repost after RecycleRepost",
			e: func() Entry {
				e := due(Entry{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Attempts: 4})
				e.RecycleAt = now.Add(-30 * time.Minute)
				return e
			}(),
			want: []Action{Recycle{Key: key("x")}, Nudge{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Attempt: 5}},
		},
		{
			name: "continuation at threshold: alert once, keep nudging, never recycle",
			e:    due(Entry{Key: key("x"), Kind: KindContinuation, Rung: RungAssigned, Attempts: 6}),
			want: []Action{Alert{Key: key("x"), Reason: AlertContinuationUnanswered}, Nudge{Key: key("x"), Kind: KindContinuation, Rung: RungAssigned, Attempt: 7}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if !e.Escalated {
					t.Error("escalated not set")
				}
			},
		},
		{
			name: "continuation already alerted: just nudge",
			e:    due(Entry{Key: key("x"), Kind: KindContinuation, Rung: RungAssigned, Attempts: 7, Escalated: true}),
			want: []Action{Nudge{Key: key("x"), Kind: KindContinuation, Rung: RungAssigned, Attempt: 8}},
		},
		{
			name: "unstarted claim at threshold: release, no nudge, no recycle",
			e:    due(Entry{Key: key("x"), Kind: KindClaim, Rung: RungClaimed, Attempts: 3}),
			want: []Action{Release{Key: key("x")}},
			check: func(t *testing.T, e Entry, d Decision) {
				if d.Next.Release == nil || *d.Next.Release != (ReleaseMark{Key: key("x"), Gen: 1, At: now}) {
					t.Errorf("mark %+v", d.Next.Release)
				}
				if e.Attempts != 3 || !e.Last.Equal(now) {
					t.Errorf("entry %+v, want the count kept and last stamped", e)
				}
			},
		},
		{
			name: "unstarted claim, agent cell unreadable: started, so nudged, not released",
			e:    due(Entry{Key: key("x"), Kind: KindClaim, Rung: RungClaimed, Attempts: 3}),
			obs:  func(o Observation) Observation { o.StartedRead = false; return o },
			check: func(t *testing.T, e Entry, d Decision) {
				if e.Rung != RungStarted || d.Next.Release != nil {
					t.Errorf("entry %+v mark %+v", e, d.Next.Release)
				}
			},
		},
		{
			name: "started claim at threshold: recycle keeping the claim",
			e:    due(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 6}),
			want: []Action{Recycle{Key: key("x"), KeepClaim: true}, Alert{Key: key("x"), Reason: AlertStartedRecycled}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if e.Recycles != 1 || !e.RecycleAt.Equal(now) || !e.Last.Equal(now) || e.Rung != RungStarted {
					t.Errorf("entry %+v", e)
				}
			},
		},
		{
			name: "started claim past R: escalate, alert, nudge",
			e:    due(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 6, Recycles: 1}),
			want: []Action{Alert{Key: key("x"), Reason: AlertStartedUnanswered}, Nudge{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Attempt: 7}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if e.Rung != RungEscalated || !e.Escalated || e.Recycles != 1 {
					t.Errorf("entry %+v", e)
				}
			},
		},
		{
			name: "escalated: a nudge at cap, forever",
			e:    due(Entry{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Attempts: 40, Recycles: 1, Escalated: true}),
			want: []Action{Nudge{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Attempt: 41}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := quiet(now, 1, Work{Key: tc.e.Key, Kind: tc.e.Kind})
			if tc.e.Rung >= RungStarted {
				o.Started[tc.e.Key] = SrcAgent
			}
			if tc.obs != nil {
				o = tc.obs(o)
			}
			d := Decide(rec(1, tc.e), o, p)
			if tc.want != nil {
				wantEffects(t, d, tc.want...)
			}
			if !d.Changed {
				t.Error("an act must write the cell first")
			}
			if tc.check != nil {
				tc.check(t, entry(t, d, tc.e.Key.Bead), d)
			}
		})
	}
}

// At most one nudge per seat per pass, to the earliest-due entry; the rest
// stay due.
func TestDecideOneNudgePerPass(t *testing.T) {
	now := t0.Add(time.Hour)
	prev := rec(1,
		Entry{Key: key("a"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 1, Last: now.Add(-4 * time.Minute)},
		Entry{Key: key("b"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 1, Last: now.Add(-10 * time.Minute)},
		Entry{Key: key("c"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 6, Last: now.Add(-2 * time.Hour)},
	)
	o := quiet(now, 1, trig("a"), claim("b"), claim("c"))
	o.Started[key("b")], o.Started[key("c")] = SrcAgent, SrcAgent
	d := Decide(prev, o, p)
	// c is earliest due (recycle, no nudge slot), then b (nudge); a waits.
	wantEffects(t, d, Recycle{Key: key("c"), KeepClaim: true}, Alert{Key: key("c"), Reason: AlertStartedRecycled}, Nudge{Key: key("b"), Kind: KindClaim, Rung: RungStarted, Attempt: 2})
	if e := entry(t, d, "a"); e.Attempts != 1 {
		t.Fatalf("a nudged too: %+v", e)
	}
	if !d.Next.LastNudge.Equal(now) {
		t.Fatalf("seat last nudge %v, want now", d.Next.LastNudge)
	}
}

// WL3 per seat: a seat is nudged at most once per Base, whichever of its
// work is due, and whatever work came and went in between.
func TestDecideSeatNudgeSpacing(t *testing.T) {
	now := t0.Add(time.Hour)
	prev := rec(1,
		Entry{Key: key("a"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 1, Last: t0},
		Entry{Key: key("b"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 1, Last: t0},
	)
	prev.LastNudge = now.Add(-DefaultBase + time.Second) // a nudge about work since finished
	o := quiet(now, 1, claim("a"), claim("b"))
	o.Started[key("a")], o.Started[key("b")] = SrcAgent, SrcAgent
	d := Decide(prev, o, p)
	wantEffects(t, d)
	d = Decide(d.Next, func() Observation { o.Now = now.Add(time.Second); return o }(), p)
	if n := countNudges(d); n != 1 {
		t.Fatalf("at Base: %d nudges, want 1", n)
	}
	o.Now = now.Add(time.Second + DefaultBase - time.Second)
	if n := countNudges(Decide(d.Next, o, p)); n != 0 {
		t.Fatal("the second entry was nudged within Base of the first")
	}
	o.Now = now.Add(time.Second + DefaultBase)
	if n := countNudges(Decide(d.Next, o, p)); n != 1 {
		t.Fatal("the second entry not nudged once Base passed")
	}
}

// A stamp from a clock that ran ahead is pulled back to now: the entry is
// not held back by the skew.
func TestDecideClampsFutureStamps(t *testing.T) {
	now := t0
	prev := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: now.Add(10 * time.Hour), Attempts: 6, Recycles: 1, Last: now.Add(5 * time.Hour)})
	prev.LastNudge = now.Add(5 * time.Hour)
	d := Decide(prev, startedBy1(quiet(now, 1, claim("s")), "s", SrcAgent), p)
	if e := entry(t, d, "s"); e.Last.After(now) || e.Since.After(now) || d.Next.LastNudge.After(now) {
		t.Fatalf("entry %+v seat %v: future stamps kept", e, d.Next.LastNudge)
	}
	if want := now.Add(DefaultCap); !due(d).Equal(want) {
		t.Fatalf("due %v, want a Cap from now", due(d).Sub(now))
	}
}

// A pass that changes nothing writes nothing, so a quiet seat costs no
// writes between nudges.
func TestDecideNoChurn(t *testing.T) {
	d := Decide(Record{}, startedBy1(quiet(t0, 1, claim("s")), "s", SrcAgent), p)
	for _, step := range []time.Duration{10 * time.Second, 30 * time.Second, 80 * time.Second} {
		d2 := Decide(d.Next, quiet(t0.Add(step), 1, claim("s")), p)
		if d2.Changed || len(d2.Actions) != 0 {
			t.Fatalf("+%v: changed=%v actions=%+v", step, d2.Changed, d2.Actions)
		}
	}
}

// The wedge, replayed: a started claim on a seat that never answers. The
// nudges come at grace, then +3, +6, +12, +24, +48 minutes; one recycle an
// hour later; then the escalation (with its nudge) and an hourly nudge,
// forever. Today's engine stops after 3.
func TestDecideWedgeSchedule(t *testing.T) {
	type ev struct {
		at   time.Duration
		what string
	}
	var got []ev
	r := Record{}
	tick := 30 * time.Second
	for now := t0; now.Before(t0.Add(8 * time.Hour)); now = now.Add(tick) {
		o := startedBy1(quiet(now, 1, claim("s")), "s", SrcCurrentClaim)
		if r.V != 0 {
			o.Started = map[ClaimKey]StartSource{} // seeds only on the first observation
		}
		d := Decide(r, o, p)
		for _, a := range effects(d) {
			switch a := a.(type) {
			case Nudge:
				got = append(got, ev{now.Sub(t0), "nudge"})
			case Recycle:
				got = append(got, ev{now.Sub(t0), "recycle"})
			case Alert:
				got = append(got, ev{now.Sub(t0), a.Reason})
			default:
				t.Fatalf("unexpected %#v", a)
			}
		}
		if at := due(d); at.After(now.Add(DefaultCap)) || at.IsZero() {
			t.Fatalf("at %v: due %v", now.Sub(t0), at)
		}
		r = d.Next
	}
	m := func(mins, secs int) time.Duration {
		return time.Duration(mins)*time.Minute + time.Duration(secs)*time.Second
	}
	want := []ev{
		{m(1, 30), "nudge"},
		{m(4, 30), "nudge"},
		{m(10, 30), "nudge"},
		{m(22, 30), "nudge"},
		{m(46, 30), "nudge"},
		{m(94, 30), "nudge"},
		{m(154, 30), "recycle"},
		{m(154, 30), AlertStartedRecycled},
		{m(214, 30), AlertStartedUnanswered},
		{m(214, 30), "nudge"},
		{m(274, 30), "nudge"},
		{m(334, 30), "nudge"},
		{m(394, 30), "nudge"},
		{m(454, 30), "nudge"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schedule:\n got %v\nwant %v", got, want)
	}
}

// An unstarted claim on a quiet seat is released after its third
// unanswered nudge, never before.
func TestDecideUnstartedSchedule(t *testing.T) {
	r := Record{}
	var nudges []time.Duration
	for now := t0; now.Before(t0.Add(time.Hour)); now = now.Add(30 * time.Second) {
		d := Decide(r, quiet(now, 1, claim("u")), p)
		for _, a := range effects(d) {
			switch a.(type) {
			case Nudge:
				nudges = append(nudges, now.Sub(t0))
			case Release:
				if len(nudges) != 3 || now.Sub(t0) != 22*time.Minute+30*time.Second {
					t.Fatalf("released at %v after %v", now.Sub(t0), nudges)
				}
				return
			}
		}
		r = d.Next
	}
	t.Fatalf("never released; nudges %v", nudges)
}

// Decide does not mutate its inputs.
func TestDecideDoesNotMutate(t *testing.T) {
	mark := &ReleaseMark{Key: key("u"), Gen: 1, At: t0}
	prev := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 6, Last: t0}, Entry{Key: key("u"), Kind: KindClaim, Rung: RungClaimed, Since: t0})
	prev.Release = mark
	before := EncodeLadder(prev)
	o := quiet(t0.Add(3*time.Hour), 1, claim("s"), claim("u"))
	o.Started[key("s")] = SrcAgent
	_ = Decide(prev, o, p)
	if EncodeLadder(prev) != before || *mark != (ReleaseMark{Key: key("u"), Gen: 1, At: t0}) {
		t.Fatal("Decide mutated prev")
	}
}

func TestPolicyNeverZero(t *testing.T) {
	q := Policy{}.withDefaults()
	want := DefaultPolicy()
	want.MaxRecycles = 0 // R = 0 is a policy: never recycle
	if !reflect.DeepEqual(q, want) {
		t.Fatalf("zero policy = %+v, want the default pacing", q)
	}
	q = Policy{Grace: -1, Base: 2 * time.Hour, Cap: time.Hour, PausedCap: 3 * time.Hour, MaxRecycles: -2}.withDefaults()
	if q.Grace != DefaultGrace || q.Base != time.Hour || q.PausedCap != time.Hour || q.MaxRecycles != 0 {
		t.Fatalf("normalized = %+v", q)
	}
	if got := (Policy{Threshold: map[Kind]map[Rung]int{KindClaim: {RungClaimed: 0}}}).withDefaults().threshold(KindClaim, RungClaimed); got != DefaultClaimedThreshold {
		t.Fatalf("zero threshold = %d", got)
	}
}

// A release that ran to an outcome is cleared; any other release is kept,
// and a release the lane could not finish is retried a backoff later.
func TestClearRelease(t *testing.T) {
	mark := &ReleaseMark{Key: key("u"), Gen: 3, At: t0}
	r := rec(3, Entry{Key: key("u"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Last: t0})
	r.Release = mark
	for _, tc := range []struct {
		name string
		key  ClaimKey
		gen  int64
		ok   bool
	}{{"named", key("u"), 3, true}, {"another key", key("v"), 3, false}, {"another generation", key("u"), 4, false}} {
		got, ok := ClearRelease(r, tc.key, tc.gen)
		if ok != tc.ok || (got.Release == nil) != tc.ok || r.Release != mark {
			t.Errorf("%s: ok=%v release=%+v", tc.name, ok, got.Release)
		}
	}
	if _, ok := ClearRelease(rec(3), key("u"), 3); ok {
		t.Error("cleared a release that was not there")
	}
	// Cleared but still outstanding and unstarted (the route was unserved,
	// so the claim was kept): the next release waits out a backoff.
	cleared, _ := ClearRelease(r, key("u"), 3)
	d := Decide(cleared, quiet(t0.Add(time.Minute), 3, claim("u")), p)
	wantEffects(t, d)
	if want := t0.Add(12 * time.Minute); !due(d).Equal(want) {
		t.Fatalf("due = %v, want the backoff after the attempt", due(d).Sub(t0))
	}
}

// A release that ran and did not release the claim (kept, or failed) never
// leaves it silent: it is nudged at Cap with one release_unfinished alert,
// and the release retried at the same pace; never sooner.
func TestDecideReleaseUnfinished(t *testing.T) {
	r := rec(1, Entry{Key: key("u"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Last: t0})
	r.Release = &ReleaseMark{Key: key("u"), Gen: 1, At: t0}
	r, ok := ReleaseUnfinished(r, key("u"), 1, t0)
	if !ok || r.Release != nil || !r.Entries[0].Unreleased {
		t.Fatalf("ReleaseUnfinished = %+v, %v", r, ok)
	}
	if _, ok := ReleaseUnfinished(r, key("u"), 1, t0); ok {
		t.Fatal("marked a release that was not pending")
	}
	type ev struct {
		at   time.Duration
		what string
	}
	var got []ev
	for now := t0.Add(30 * time.Second); now.Before(t0.Add(3*time.Hour + time.Minute)); now = now.Add(30 * time.Second) {
		d := Decide(r, quiet(now, 1, claim("u")), p)
		for _, a := range effects(d) {
			switch a := a.(type) {
			case Nudge:
				got = append(got, ev{now.Sub(t0), "nudge"})
			case Release:
				got = append(got, ev{now.Sub(t0), "release"})
			case Alert:
				got = append(got, ev{now.Sub(t0), a.Reason})
			}
		}
		r = d.Next
		if r.Release != nil { // kept again
			r, _ = ReleaseUnfinished(r, r.Release.Key, r.Release.Gen, now)
		}
	}
	want := []ev{
		{time.Hour, "release"},
		{time.Hour, AlertReleaseUnfinished},
		{time.Hour, "nudge"},
		{2 * time.Hour, "release"},
		{2 * time.Hour, "nudge"},
		{3 * time.Hour, "release"},
		{3 * time.Hour, "nudge"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after an unfinished release:\n got %v\nwant %v", got, want)
	}
}

// A release whose live read finds the claim handed to the seat's turn makes
// it started, with a fresh count, and clears the release.
func TestReleaseFoundStarted(t *testing.T) {
	r := rec(1, Entry{Key: key("u"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Last: t0, Unreleased: true, Escalated: true})
	r.Release = &ReleaseMark{Key: key("u"), Gen: 1, At: t0}
	now := t0.Add(time.Minute)
	got, ok := ReleaseFoundStarted(r, key("u"), 1, SrcClaimSession, now)
	e := got.Entries[0]
	if !ok || got.Release != nil || e.Rung != RungStarted || e.Attempts != 0 || e.Unreleased || e.Escalated || e.Seed != SrcClaimSession || !e.Since.Equal(now) {
		t.Fatalf("ReleaseFoundStarted = %+v, %v", got, ok)
	}
	if r.Entries[0].Rung != RungClaimed {
		t.Fatal("mutated its input")
	}
}

// A seat that refuses every nudge (a permission prompt) is retried no more
// often than PausedCap, its count does not move, and it is alerted at the
// third refusal and again every refusedRealertEvery refusals after, about
// hourly, while the refusals go on.
func TestDecideRefusedNudgesBackOff(t *testing.T) {
	r := Record{}
	var at []time.Duration
	var alerts []int // the refusal counts that alerted
	for now := t0; now.Before(t0.Add(6 * time.Hour)); now = now.Add(30 * time.Second) {
		d := Decide(r, startedBy1(quiet(now, 1, claim("s")), "s", SrcAgent), p)
		r = d.Next
		for _, a := range d.Actions {
			if n, ok := a.(Nudge); ok {
				at = append(at, now.Sub(t0))
				var alert, ok bool
				if r, alert, ok = NudgeRefused(r, n.Key, n.Attempt, now); !ok {
					t.Fatalf("NudgeRefused refused the attempt it was given")
				}
				if alert {
					alerts = append(alerts, r.Entries[0].Refused)
				}
			}
		}
	}
	var want []int
	for n := refusedAlertAt; n <= len(at); n += refusedRealertEvery {
		want = append(want, n)
	}
	if len(want) < 4 || fmt.Sprint(alerts) != fmt.Sprint(want) {
		t.Fatalf("alerts at refusals %v, want %v (%d refusals)", alerts, want, len(at))
	}
	for i := 2; i < len(at); i++ {
		if gap := at[i] - at[i-1]; gap < DefaultPausedCap {
			t.Fatalf("refused nudges %v apart (%v)", gap, at)
		}
	}
	if n := len(at); n > 6*6+2 {
		t.Fatalf("%d attempts in 6h", n)
	}
	if e := r.Entries[0]; e.Attempts != 0 || e.Rung != RungStarted || e.Recycles != 0 {
		t.Fatalf("entry %+v: refusals moved it toward escalation", e)
	}
	got, ok := NudgeTaken(r, key("s"))
	if !ok || got.Entries[0].Refused != 0 {
		t.Fatalf("NudgeTaken = %+v, %v", got.Entries[0], ok)
	}
	if _, ok := NudgeTaken(got, key("s")); ok {
		t.Fatal("NudgeTaken wrote with no refusals")
	}
	if _, _, ok := NudgeRefused(r, key("s"), 99, t0); ok {
		t.Fatal("NudgeRefused re-counted an attempt the cell no longer holds")
	}
}

// A new generation clears an unfinished release and refusals; a recycle
// records its generation.
func TestDecideGenerationClearsUnfinished(t *testing.T) {
	prev := rec(1, Entry{Key: key("u"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Last: t0, Unreleased: true, Refused: 2})
	d := Decide(prev, quiet(t0.Add(time.Minute), 2, claim("u")), p)
	if e := entry(t, d, "u"); e.Unreleased || e.Refused != 0 {
		t.Fatalf("entry %+v after a new generation", e)
	}
	r := rec(5, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 6, Last: t0})
	d = Decide(r, startedBy1(quiet(t0.Add(2*time.Hour), 5, claim("s")), "s", SrcAgent), p)
	if d.Next.RecycledGen != 5 {
		t.Fatalf("RecycledGen = %d, want the recycle's generation 5", d.Next.RecycledGen)
	}
}

// RecycledGen outlives the pass that set it: every later cell carries it, a
// trigger recycle records it too, and a cell that differs only in it, or in
// an entry's refusals, is a different cell (a write).
func TestDecideKeepsRecycledGen(t *testing.T) {
	prev := rec(4, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 1, Last: t0})
	prev.RecycledGen = 3
	d := Decide(prev, startedBy1(quiet(t0.Add(time.Minute), 4, claim("s")), "s", SrcAgent), p)
	if d.Next.RecycledGen != 3 {
		t.Fatalf("RecycledGen = %d, want 3 carried", d.Next.RecycledGen)
	}
	tr := rec(6, Entry{Key: key("t"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 3, Last: t0})
	d = Decide(tr, quiet(t0.Add(time.Hour), 6, trig("t")), p)
	if d.Next.RecycledGen != 6 {
		t.Fatalf("trigger recycle: RecycledGen = %d, want 6", d.Next.RecycledGen)
	}
	a := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0})
	b := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0})
	b.RecycledGen = 1
	if sameRecord(a, b) {
		t.Fatal("cells differing in RecycledGen compare equal")
	}
	c := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Refused: 1})
	if sameRecord(a, c) {
		t.Fatal("cells differing in an entry's refusals compare equal")
	}
}

// Progress the seat made since its last nudge ends a run of refusals too.
func TestDecideProgressClearsRefusals(t *testing.T) {
	last := t0.Add(10 * time.Minute)
	prev := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Paused: 2, Refused: 2, Last: last})
	o := quiet(last.Add(30*time.Minute), 1, claim("s"))
	o.Quiet, o.LastActivity = Quiet{State: NotQuiet}, last.Add(DefaultProgressReset)
	if e := entry(t, Decide(prev, o, p), "s"); e.Refused != 0 {
		t.Fatalf("entry %+v, want refusals cleared by progress", e)
	}
}

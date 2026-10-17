package workladder

import (
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

// effects is d's actions without the notes.
func effects(d Decision) []Action {
	var out []Action
	for _, a := range d.Actions {
		if _, ok := a.(Note); !ok {
			out = append(out, a)
		}
	}
	return out
}

func notes(d Decision) []Note {
	var out []Note
	for _, a := range d.Actions {
		if n, ok := a.(Note); ok {
			out = append(out, n)
		}
	}
	return out
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
		if !d.Due.Equal(t0.Add(DefaultGrace)) {
			t.Errorf("Due = %v, want t0+grace", d.Due)
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
		if n := notes(d); len(n) != 1 || n[0].Event != NoteRung {
			t.Fatalf("notes = %+v, want one rung note", n)
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
	t.Run("a finished entry is dropped with a note", func(t *testing.T) {
		prev := rec(1, Entry{Key: key("a"), Kind: KindClaim, Rung: RungClaimed, Since: t0}, Entry{Key: key("b"), Kind: KindTrigger, Rung: RungAssigned, Since: t0})
		d := Decide(prev, quiet(t0, 1, trig("b")), p)
		if len(d.Next.Entries) != 1 || d.Next.Entries[0].Key != key("b") {
			t.Fatalf("entries %+v, want only b", d.Next.Entries)
		}
		if n := notes(d); len(n) != 1 || n[0] != (Note{Key: key("a"), Event: NoteDropped}) {
			t.Fatalf("notes = %+v", n)
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
		if d.Changed || !d.Due.IsZero() || len(d.Actions) != 0 {
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
	if !d.Due.Equal(now.Add(DefaultGrace)) {
		t.Fatalf("Due = %v, want a fresh grace", d.Due)
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
		if !d.Due.Equal(t0.Add(time.Hour)) {
			t.Fatalf("Due = %v, want now: v is due and waiting", d.Due)
		}
	})
}

// Steps 4-6: a held gate, a runtime not proven alive, or a seat not proven
// quiet get no nudge, and are looked at again.
func TestDecideHolds(t *testing.T) {
	prev := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: t0})
	now := t0.Add(2 * time.Hour) // long past due
	until := now.Add(12 * time.Hour)
	cases := map[string]struct {
		mut func(*Observation)
		due time.Time
	}{
		"draining":            {func(o *Observation) { o.Gate = Gate{Held: true, Reason: "draining"} }, now.Add(DefaultGrace)},
		"heartbeat hold":      {func(o *Observation) { o.Gate = Gate{Held: true, Reason: "heartbeat", Until: until} }, until},
		"runtime dead":        {func(o *Observation) { o.Runtime = TriFalse }, now.Add(DefaultGrace)},
		"runtime unknown":     {func(o *Observation) { o.Runtime = TriUnknown }, now.Add(DefaultGrace)},
		"not quiet":           {func(o *Observation) { o.Quiet = Quiet{State: NotQuiet, Reason: "snapshot_not_idle"} }, now.Add(DefaultGrace)},
		"quiet unknown":       {func(o *Observation) { o.Quiet = Quiet{} }, now.Add(DefaultGrace)},
		"held while degraded": {func(o *Observation) { o.Gate = Gate{Held: true}; o.Provider.Degraded = true }, now.Add(DefaultGrace)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			o := quiet(now, 1, claim("s"))
			tc.mut(&o)
			d := Decide(prev, o, p)
			wantEffects(t, d)
			if !d.Due.Equal(tc.due) {
				t.Fatalf("Due = %v, want %v", d.Due, tc.due)
			}
			if d.Changed {
				t.Fatalf("a hold must not write: %+v", d.Next)
			}
		})
	}
}

// Step 6: activity ProgressReset past the last nudge restarts the count.
func TestDecideProgressReset(t *testing.T) {
	last := t0.Add(10 * time.Minute)
	prev := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 4, Paused: 1, Last: last})
	busy := func(act time.Time) Observation {
		o := quiet(last.Add(30*time.Minute), 1, claim("s"))
		o.Quiet = Quiet{State: NotQuiet}
		o.LastActivity = act
		return o
	}
	d := Decide(prev, busy(last.Add(DefaultProgressReset)), p)
	if e := entry(t, d, "s"); e.Attempts != 0 || e.Paused != 0 || !d.Changed {
		t.Fatalf("worked %v past the nudge: %+v, want the count reset", DefaultProgressReset, e)
	}
	d = Decide(prev, busy(last.Add(DefaultProgressReset-time.Second)), p)
	if e := entry(t, d, "s"); e.Attempts != 4 || d.Changed {
		t.Fatalf("worked under %v: %+v, want the count kept", DefaultProgressReset, e)
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
		{"degraded under the paused cap", Entry{Kind: KindClaim, Rung: RungStarted, Attempts: 1, Last: last}, true, last.Add(3 * time.Minute)},
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
				if e.Attempts != 3 {
					t.Errorf("entry %+v", e)
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
			want: []Action{Recycle{Key: key("x"), KeepClaim: true}},
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
	wantEffects(t, d, Recycle{Key: key("c"), KeepClaim: true}, Nudge{Key: key("b"), Kind: KindClaim, Rung: RungStarted, Attempt: 2})
	if e := entry(t, d, "a"); e.Attempts != 1 {
		t.Fatalf("a nudged too: %+v", e)
	}
	if !d.Due.Equal(now) {
		t.Fatalf("Due = %v, want now: a is due and waiting", d.Due.Sub(now))
	}
}

// Step 9: the next look is the earliest entry, never past now+Cap.
func TestDecideDueCapped(t *testing.T) {
	now := t0
	prev := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: now.Add(10 * time.Hour)}) // a clock-skewed since
	d := Decide(prev, startedBy1(quiet(now, 1, claim("s")), "s", SrcAgent), p)
	if !d.Due.Equal(now.Add(DefaultCap)) {
		t.Fatalf("Due = %v, want now+cap", d.Due.Sub(now))
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
		if d.Due.After(now.Add(DefaultCap)) || d.Due.IsZero() {
			t.Fatalf("at %v: Due %v", now.Sub(t0), d.Due)
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

package workladder

import (
	"testing"
	"time"
)

// The rate-limit rule (audit-4 lever 2): a seat whose provider refuses its
// turns with a rate limit is not ignoring its nudges, it cannot answer them.
// Once a refusal answers the ladder's own nudge, the seat's started claims
// are recycled to a fresh conversation, keeping the claims, within the
// recycle budget an unanswered started claim spends (WL3). If the fresh
// seat's provider refuses it too, the pool itself is refusing, and the
// claim's alert names that, once, and the claim is paced at Cap.

// rateLimited is o with the seat's latest n turns refused for a rate limit,
// the newest ending at at.
func rateLimited(o Observation, n int, at time.Time) Observation {
	o.RateLimited, o.RateLimitedAt = n, at
	return o
}

func TestDecideRateLimited(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	nudged := now.Add(-time.Minute)           // the ladder's last nudge
	answered := nudged.Add(2 * time.Second)   // a refusal answering it
	beforeIt := nudged.Add(-30 * time.Minute) // a refusal from before it
	notDue := func(e Entry) Entry { e.Since, e.Last = t0, nudged; return e }
	isDue := func(e Entry) Entry { e.Since, e.Last = t0, t0; return e }
	cases := []struct {
		name  string
		e     Entry
		obs   func(Observation) Observation
		want  []Action
		check func(*testing.T, Entry, Decision)
	}{
		{
			name: "started claim, its nudge refused: a fresh recycle keeping the claim, at once",
			e:    notDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 1}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2, answered) },
			want: []Action{Recycle{Key: key("x"), KeepClaim: true, Fresh: true}, Alert{Key: key("x"), Reason: AlertRateLimitedRecycled}},
			check: func(t *testing.T, e Entry, d Decision) {
				if e.Recycles != 1 || !e.FreshRecycled || !e.RecycleAt.Equal(now) || !e.Last.Equal(now) || e.Rung != RungStarted || e.RateLimitAlerted {
					t.Errorf("entry %+v", e)
				}
				if d.Next.RecycledGen != 1 {
					t.Errorf("recycled_gen = %d, want the decision's generation", d.Next.RecycledGen)
				}
			},
		},
		{
			name: "refusals older than the ladder's last nudge: not the rule's, the ladder as before",
			e:    isDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 1}),
			obs:  func(o Observation) Observation { return rateLimited(o, 3, t0.Add(-time.Minute)) },
			want: []Action{Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 2}},
		},
		{
			name: "refusals the ladder never nudged (its first observation): a nudge first, no recycle",
			e:    Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: now.Add(-2 * time.Minute)},
			obs:  func(o Observation) Observation { return rateLimited(o, 4, beforeIt) },
			want: []Action{Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 1}},
		},
		{
			name: "refused once: below the threshold, the ladder as before",
			e:    isDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 1}),
			obs:  func(o Observation) Observation { return rateLimited(o, 1, now) },
			want: []Action{Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 2}},
		},
		{
			name: "fresh recycle queued (another seat is the canary): nothing before its due time",
			e:    notDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 1}),
			obs: func(o Observation) Observation {
				o.FreshHold = FreshHoldQueued
				return rateLimited(o, 2, answered)
			},
			want: []Action{},
		},
		{
			name: "fresh recycle held standing (an attached seat): nothing before its due time",
			e:    notDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 1}),
			obs: func(o Observation) Observation {
				o.FreshHold = FreshHoldStanding
				return rateLimited(o, 2, answered)
			},
			want: []Action{},
		},
		{
			name: "fresh seat refused again: the pool itself, alert once, paced at Cap",
			e:    notDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 1, Recycles: 1, FreshRecycled: true, RecycleAt: now.Add(-10 * time.Minute)}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2, answered) },
			want: []Action{Alert{Key: key("x"), Reason: AlertProviderRateLimited}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if e.Rung != RungEscalated || !e.RateLimitAlerted || e.Recycles != 1 {
					t.Errorf("entry %+v", e)
				}
				if got, want := p.dueAt(e, false), e.Last.Add(DefaultCap); !got.Equal(want) {
					t.Errorf("next due %v, want Cap after its last nudge (%v)", got, want)
				}
			},
		},
		{
			name: "fresh seat refused again and due: alert and the nudge it is owed",
			e:    isDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 2, Recycles: 1, FreshRecycled: true, RecycleAt: now.Add(-10 * time.Minute)}),
			obs:  func(o Observation) Observation { return rateLimited(o, 3, now) },
			want: []Action{Alert{Key: key("x"), Reason: AlertProviderRateLimited}, Nudge{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Attempt: 3}},
		},
		{
			name: "the budget went to a drain that resumed the conversation: an accurate alert, no pool claim",
			e:    notDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 1, Recycles: 1}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2, answered) },
			want: []Action{Alert{Key: key("x"), Reason: AlertRateLimitedUnrecycled}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if e.Rung != RungEscalated || !e.RateLimitAlerted || e.Recycles != 1 || e.FreshRecycled {
					t.Errorf("entry %+v", e)
				}
			},
		},
		{
			name: "the alert was raised: never again, nudged at Cap",
			e:    isDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Attempts: 9, Recycles: 1, FreshRecycled: true, Escalated: true, RateLimitAlerted: true}),
			obs:  func(o Observation) Observation { return rateLimited(o, 5, now) },
			want: []Action{Nudge{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Attempt: 10}},
		},
		{
			name: "an escalated claim not yet named for the pool: the pool alert, once",
			e:    notDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Attempts: 9, Recycles: 1, FreshRecycled: true, Escalated: true, RecycleAt: now.Add(-10 * time.Minute)}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2, answered) },
			want: []Action{Alert{Key: key("x"), Reason: AlertProviderRateLimited}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if !e.RateLimitAlerted || !e.Escalated {
					t.Errorf("entry %+v", e)
				}
			},
		},
		{
			name: "degraded provider: WL4, no recycle and no alert, a paused nudge when due",
			e:    isDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempts: 2}),
			obs: func(o Observation) Observation {
				o.Provider = ProviderState{Degraded: true, Reason: "provider-health"}
				return rateLimited(o, 4, now)
			},
			want: []Action{Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 2, Paused: true}},
			check: func(t *testing.T, e Entry, _ Decision) {
				if e.Recycles != 0 || e.RateLimitAlerted {
					t.Errorf("entry %+v", e)
				}
			},
		},
		{
			name: "an unstarted claim is not the rule's: the release stands",
			e:    isDue(Entry{Key: key("x"), Kind: KindClaim, Rung: RungClaimed, Attempts: 3}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2, now) },
			want: []Action{Release{Key: key("x")}},
		},
		{
			name: "a trigger is not the rule's",
			e:    isDue(Entry{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Attempts: 1}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2, now) },
			want: []Action{Nudge{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Attempt: 2}},
		},
		{
			name: "a continuation is not the rule's",
			e:    isDue(Entry{Key: key("x"), Kind: KindContinuation, Rung: RungAssigned, Attempts: 1}),
			obs:  func(o Observation) Observation { return rateLimited(o, 2, now) },
			want: []Action{Nudge{Key: key("x"), Kind: KindContinuation, Rung: RungAssigned, Attempt: 2}},
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
			if len(tc.want) == 0 {
				if len(d.Actions) != 0 {
					t.Fatalf("effects = %#v, want none", d.Actions)
				}
			} else {
				wantEffects(t, d, tc.want...)
			}
			if tc.check != nil {
				tc.check(t, entry(t, d, tc.e.Key.Bead), d)
			}
		})
	}
}

// The rule waits for what every other act waits for: an open gate, a live
// runtime and a quiet seat. A seat that is starting, held, or working (it
// may be retrying the provider itself) is left alone.
func TestDecideRateLimitedNeedsAQuietOpenSeat(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	e := Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 1, Last: now.Add(-time.Minute)}
	for name, mut := range map[string]func(*Observation){
		"gate held":       func(o *Observation) { o.Gate = Gate{Held: true, Reason: "restart_pending"} },
		"not quiet":       func(o *Observation) { o.Quiet = Quiet{State: NotQuiet, Reason: "busy"} },
		"quiet unread":    func(o *Observation) { o.Quiet = Quiet{} },
		"runtime dead":    func(o *Observation) { o.Runtime = TriFalse },
		"runtime unknown": func(o *Observation) { o.Runtime = TriUnknown },
		"work held on its read": func(o *Observation) {
			o.Hold = map[ClaimKey]bool{key("x"): true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := rateLimited(startedBy1(quiet(now, 1, claim("x")), "x", SrcAgent), 3, now.Add(-30*time.Second))
			mut(&o)
			d := Decide(rec(1, e), o, p)
			for _, a := range d.Actions {
				switch a.(type) {
				case Recycle, Alert:
					t.Fatalf("acted on a seat it must leave alone: %#v", d.Actions)
				}
			}
		})
	}
}

// One fresh recycle restarts the whole seat, so every started claim on it
// that its refusal answered is spent on it, and no nudge goes to a seat that
// is being restarted.
func TestDecideRateLimitedRecyclesTheSeatOnce(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	prev := rec(1,
		Entry{Key: key("a"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: t0},
		Entry{Key: key("b"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 6, Last: t0},
		Entry{Key: key("t"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 1, Last: t0},
	)
	o := quiet(now, 1, claim("a"), claim("b"), trig("t"))
	o.Started[key("a")], o.Started[key("b")] = SrcAgent, SrcAgent
	d := Decide(prev, rateLimited(o, 2, now.Add(-30*time.Second)), p)
	wantEffects(t, d,
		Recycle{Key: key("a"), KeepClaim: true, Fresh: true}, Alert{Key: key("a"), Reason: AlertRateLimitedRecycled},
		Recycle{Key: key("b"), KeepClaim: true, Fresh: true}, Alert{Key: key("b"), Reason: AlertRateLimitedRecycled},
	)
	for _, b := range []string{"a", "b"} {
		if e := entry(t, d, b); e.Recycles != 1 || !e.FreshRecycled {
			t.Errorf("%s: %+v, want the fresh recycle spent", b, e)
		}
	}
	if e := entry(t, d, "t"); e.Attempts != 1 {
		t.Errorf("the trigger was nudged on a seat being restarted: %+v", e)
	}
}

// The fresh seat answers for itself: its own refusal of the ladder's nudge
// in its own generation raises the pool alert, and nothing earlier does.
func TestDecideRateLimitedWaitsForTheFreshSeat(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	e := Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 1, Last: now.Add(-time.Minute), Recycles: 1, FreshRecycled: true, RecycleAt: now.Add(-time.Minute)}
	prev := rec(1, e)
	prev.RecycledGen = 1
	at := func(d time.Duration, gen int64, refused int, refusedAt time.Time) Observation {
		return rateLimited(startedBy1(quiet(now.Add(d), gen, claim("x")), "x", SrcAgent), refused, refusedAt)
	}
	// The restart is on its way (same generation): nothing more.
	if d := Decide(prev, at(0, 1, 3, now.Add(-30*time.Second)), p); len(d.Actions) != 0 {
		t.Fatalf("acted again before the fresh seat answered: %#v", d.Actions)
	}
	// The fresh seat (generation 2) refuses its first turn: a fresh count,
	// no alert until the ladder nudges it and it refuses that too.
	d := Decide(prev, at(2*time.Minute, 2, 1, now.Add(90*time.Second)), p)
	if len(d.Actions) != 0 {
		t.Fatalf("acted on the fresh seat's first refusal: %#v", d.Actions)
	}
	d = Decide(d.Next, at(2*time.Minute+DefaultGrace, 2, 1, now.Add(90*time.Second)), p)
	wantEffects(t, d, Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 1})
	nudgedAt := now.Add(2*time.Minute + DefaultGrace)
	d = Decide(d.Next, at(2*time.Minute+DefaultGrace+time.Minute, 2, 2, nudgedAt.Add(time.Second)), p)
	wantEffects(t, d, Alert{Key: key("x"), Reason: AlertProviderRateLimited})
}

// WL8 holds for the rule: refusals already in the transcript when the ladder
// first sees a seat recycle nothing; a fresh recycle needs a refusal that
// answered the ladder's own nudge.
func TestDecideRateLimitedNoWaveAtDeploy(t *testing.T) {
	o := quiet(t0, 42, claim("x"))
	o.Started = map[ClaimKey]StartSource{key("x"): SrcCurrentClaim}
	o = rateLimited(o, 6, t0.Add(-11*time.Hour))
	d := Decide(Record{}, o, p)
	for now := t0; now.Sub(t0) < DefaultGrace+DefaultBase+2*DefaultBase; now = now.Add(30 * time.Second) {
		o.Now = now
		d = Decide(d.Next, o, p)
		for _, a := range d.Actions {
			if _, ok := a.(Recycle); ok {
				t.Fatalf("a recycle at %v after deploy on refusals from before it", now.Sub(t0))
			}
		}
	}
}

func TestRateLimitPolicyDefaults(t *testing.T) {
	// Two refusals in a row: the agent's own turn, and its answer to the
	// ladder's nudge.
	if got, want := DefaultPolicy().RateLimitThreshold, 2; got != want {
		t.Fatalf("RateLimitThreshold = %d, want %d", got, want)
	}
	if got := (Policy{}).withDefaults().RateLimitThreshold; got != DefaultRateLimitThreshold {
		t.Fatalf("an unset threshold fills to %d, got %d", DefaultRateLimitThreshold, got)
	}
	if got := (Policy{RateLimitThreshold: -1}).withDefaults().RateLimitThreshold; got != DefaultRateLimitThreshold {
		t.Fatalf("a negative threshold fills to %d, got %d", DefaultRateLimitThreshold, got)
	}
}

func TestCodecKeepsTheRateLimitFields(t *testing.T) {
	r := Record{V: 1, Gen: 3, Entries: []Entry{{Key: key("x"), Kind: KindClaim, Rung: RungEscalated, Since: t0, Escalated: true, Recycles: 1, FreshRecycled: true, RateLimitAlerted: true}}}
	got, err := DecodeLadder(EncodeLadder(r))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Entries[0].RateLimitAlerted || !got.Entries[0].FreshRecycled {
		t.Fatalf("decoded %+v lost the rate-limit fields", got.Entries[0])
	}
	r.Entries[0].FreshServed = true
	got, err = DecodeLadder(EncodeLadder(r))
	if err != nil || !got.Entries[0].FreshServed {
		t.Fatalf("decoded %+v (%v) lost fresh_served", got.Entries, err)
	}
	for _, mut := range []func(*Entry){func(e *Entry) { e.RateLimitAlerted = false }, func(e *Entry) { e.FreshRecycled = false }, func(e *Entry) { e.FreshServed = false }} {
		o := Record{V: 1, Gen: 3, Entries: []Entry{r.Entries[0]}}
		mut(&o.Entries[0])
		if sameRecord(r, o) {
			t.Fatalf("sameRecord must see the change: %+v", o.Entries[0])
		}
	}
}

// With more than one recycle a claim, a drain after a fresh recycle makes
// the last recycle a drain again: a later refusal is reported as the
// drain's (rate_limited_unrecycled), not as the pool's.
func TestDecideRateLimitedTracksTheLastRecycle(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	pol := DefaultPolicy()
	pol.MaxRecycles = 2
	e := Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 6, Last: t0, Recycles: 1, FreshRecycled: true, RecycleAt: now.Add(-10 * time.Minute)}
	d := Decide(rec(1, e), startedBy1(quiet(now, 1, claim("x")), "x", SrcAgent), pol)
	wantEffects(t, d, Recycle{Key: key("x"), KeepClaim: true}, Alert{Key: key("x"), Reason: AlertStartedRecycled})
	if got := entry(t, d, "x"); got.FreshRecycled || got.Recycles != 2 {
		t.Fatalf("entry %+v after a drain recycle, want FreshRecycled cleared", got)
	}
	d.Next.Entries[0].Attempts, d.Next.Entries[0].Last = 1, now.Add(time.Minute) // nudged in the next generation
	o := rateLimited(startedBy1(quiet(now.Add(2*time.Minute), 2, claim("x")), "x", SrcAgent), 2, now.Add(time.Minute+time.Second))
	d.Next.Gen = 2
	d = Decide(d.Next, o, pol)
	wantEffects(t, d, Alert{Key: key("x"), Reason: AlertRateLimitedUnrecycled})
}

// A refused started claim whose fresh recycle is queued (another seat is
// the provider's canary, or the provider refused a canary's fresh
// conversation: a wait that ends on its own) is paced as degraded: paused
// nudges that count toward nothing, so it never spends its one recycle on a
// drain that resumes the refused conversation (re-review R2). Under a
// standing hold (a person attached, a wait on a person), it is paced as
// ever: its silence still reaches the drain and the alerts (re-review M1).
func TestDecideRateLimitedHeldClaims(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	refused := func(e Entry, hold FreshHold, n int, at time.Time) Decision {
		o := rateLimited(startedBy1(quiet(now, 1, claim("x")), "x", SrcAgent), n, at)
		o.FreshHold = hold
		return Decide(rec(1, e), o, p)
	}
	atThreshold := Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 6, Last: t0}
	below := Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: t0}
	answered := t0.Add(time.Second)
	t.Run("queued, at the drain threshold: a paused nudge, no drain", func(t *testing.T) {
		d := refused(atThreshold, FreshHoldQueued, 3, answered)
		wantEffects(t, d, Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 6, Paused: true})
		if got := entry(t, d, "x"); got.Attempts != 6 || got.Recycles != 0 || got.Paused != 1 {
			t.Fatalf("entry %+v, want a paused nudge that counts nothing", got)
		}
	})
	t.Run("queued, below it: a paused nudge", func(t *testing.T) {
		wantEffects(t, refused(below, FreshHoldQueued, 3, answered), Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 2, Paused: true})
	})
	t.Run("standing, at the drain threshold: the drain and its alert", func(t *testing.T) {
		wantEffects(t, refused(atThreshold, FreshHoldStanding, 3, answered), Recycle{Key: key("x"), KeepClaim: true}, Alert{Key: key("x"), Reason: AlertStartedRecycled})
	})
	t.Run("standing, below it: a counted nudge", func(t *testing.T) {
		wantEffects(t, refused(below, FreshHoldStanding, 3, answered), Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 3})
	})
	t.Run("queued, but the refusal answered no nudge (a wedge after it): counted", func(t *testing.T) {
		e := below
		e.Last = t0.Add(time.Hour) // nudged an hour after the refusal, no answer since
		wantEffects(t, refused(e, FreshHoldQueued, 3, t0.Add(time.Minute)), Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 3})
	})
	t.Run("refused once only: not the rule's, so not queued", func(t *testing.T) {
		wantEffects(t, refused(below, FreshHoldQueued, 1, answered), Nudge{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Attempt: 3})
	})
	t.Run("queued pauses only the claim it queues: an unstarted claim is released", func(t *testing.T) {
		o := rateLimited(startedBy1(quiet(now, 1, claim("x"), claim("u")), "x", SrcAgent), 3, answered)
		o.FreshHold = FreshHoldQueued
		prev := rec(1, below, Entry{Key: key("u"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Last: t0})
		d := Decide(prev, o, p)
		var released bool
		for _, a := range d.Actions {
			if r, ok := a.(Release); ok && r.Key == key("u") {
				released = true
			}
		}
		if !released {
			t.Fatalf("actions %#v: the unstarted claim's release was paused with the queued claim", d.Actions)
		}
	})
}

// The pool is inferred refusing only from a fresh conversation refused soon
// after its restart (within Cap): a claim recycled fresh a day ago, served
// since and refused again now says nothing about fresh conversations, so it
// gets its own accurate alert, rate_limited_again, which holds no other seat
// (round-4 review M2).
func TestDecideRateLimitedAgainLongAfterItsFreshRecycle(t *testing.T) {
	now := t0.Add(30 * time.Hour)
	for name, tc := range map[string]struct {
		recycledAgo time.Duration
		want        string
	}{
		"refused soon after its fresh recycle: the pool": {5 * time.Minute, AlertProviderRateLimited},
		"at Cap: still the pool":                         {DefaultCap, AlertProviderRateLimited},
		"refused a day after it: again, not the pool":    {26 * time.Hour, AlertRateLimitedAgain},
	} {
		t.Run(name, func(t *testing.T) {
			e := Entry{
				Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: now.Add(-time.Hour), Attempts: 1, Last: now.Add(-time.Minute),
				Recycles: 1, FreshRecycled: true, RecycleAt: now.Add(-tc.recycledAgo),
			}
			o := rateLimited(startedBy1(quiet(now, 2, claim("x")), "x", SrcAgent), 2, now.Add(-time.Minute+time.Second))
			d := Decide(rec(2, e), o, p)
			wantEffects(t, d, Alert{Key: key("x"), Reason: tc.want})
			if got := entry(t, d, "x"); !got.RateLimitAlerted || got.Rung != RungEscalated {
				t.Fatalf("entry %+v, want alerted once and escalated", got)
			}
		})
	}
}

// A fresh conversation the provider served (a completed turn after the
// fresh recycle) and refused later says nothing about fresh conversations
// being refused: rate_limited_again, which holds no other seat, however
// soon (round-5 review S1). The mark is sticky: once served, always again.
func TestDecideRateLimitedFreshConversationServedThenRefused(t *testing.T) {
	now := t0.Add(30 * time.Hour)
	recycled := now.Add(-10 * time.Minute)
	e := Entry{
		Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: recycled, Attempts: 1, Last: now.Add(-time.Minute),
		Recycles: 1, FreshRecycled: true, RecycleAt: recycled,
	}
	// The fresh conversation completes a turn: the mark is recorded, whatever
	// else the pass does.
	served := startedBy1(quiet(now.Add(-5*time.Minute), 2, claim("x")), "x", SrcAgent)
	served.Quiet = Quiet{State: NotQuiet, Reason: "working"}
	served.ServedAt = recycled.Add(3 * time.Minute)
	d := Decide(rec(2, e), served, p)
	if got := entry(t, d, "x"); !got.FreshServed || !d.Changed {
		t.Fatalf("entry %+v changed %v: want the served mark recorded", got, d.Changed)
	}
	// Refused later, within Cap of the recycle: not the pool.
	o := rateLimited(startedBy1(quiet(now, 2, claim("x")), "x", SrcAgent), 2, now.Add(-time.Minute+time.Second))
	d = Decide(d.Next, o, p)
	wantEffects(t, d, Alert{Key: key("x"), Reason: AlertRateLimitedAgain})
	// A turn served before the recycle is not the fresh conversation's.
	before := startedBy1(quiet(now, 2, claim("x")), "x", SrcAgent)
	before.ServedAt = recycled.Add(-time.Minute)
	if got := entry(t, Decide(rec(2, e), before, p), "x"); got.FreshServed {
		t.Fatalf("entry %+v: a turn served before the fresh recycle marked it served", got)
	}
}

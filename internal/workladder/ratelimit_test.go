package workladder

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The rate-limit episode (ratelimit.go): one row of its
// transition table per test case, with the near misses.

// rlAt is the episode tests' clock: hours after t0, so no default bound
// reaches back before t0.
var rlAt = t0.Add(10 * time.Hour)

// rlSeat is a cell for a seat holding one started claim "s", counted at its
// start, whose last nudge reached it at delivered (zero: none this
// generation), with episode rl.
func rlSeat(delivered time.Time, rl RateLimit) Record {
	r := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: minTime(delivered, rlAt)})
	r.Delivered, r.RateLimit, r.LastNudge = delivered, rl, minTime(delivered, rlAt)
	return r
}

// rlPass is an acting pass at now over that seat, in conversation conv,
// with reading tr.
func rlPass(now time.Time, conv int64, tr TurnReading) Observation {
	o := startedBy1(quiet(now, 1, claim("s")), "s", SrcAgent)
	o.Conv, o.Turns = conv, tr
	return o
}

// refusedTurns is a reading of conversation conv whose newest n turns the
// provider refused with a rate limit, the newest ending at at.
func refusedTurns(conv int64, n int, at time.Time) TurnReading {
	return TurnReading{Read: true, Conv: conv, LastFailed: true, Refused: n, RefusedAt: at}
}

// servedTurns is a reading of conversation conv whose newest turn the
// provider completed at at.
func servedTurns(conv int64, at time.Time) TurnReading {
	return TurnReading{Read: true, Conv: conv, ServedAt: at}
}

func rlMoves(d Decision) []RateLimitMove {
	var out []RateLimitMove
	for _, a := range d.Actions {
		if m, ok := a.(RateLimitMove); ok {
			out = append(out, m)
		}
	}
	return out
}

func rlAlerts(d Decision) []string {
	var out []string
	for _, a := range d.Actions {
		if al, ok := a.(Alert); ok && strings.HasPrefix(al.Reason, "rate_limited_") {
			out = append(out, al.Reason)
		}
	}
	return out
}

func freshRestarts(d Decision) int {
	n := 0
	for _, a := range d.Actions {
		if _, ok := a.(FreshRestart); ok {
			n++
		}
	}
	return n
}

func move(tr RateLimitTransition, c RateLimitCause) RateLimitMove {
	return RateLimitMove{Transition: tr, Cause: c}
}

func TestRateLimitOpens(t *testing.T) {
	delivered := rlAt.Add(-4 * time.Minute)
	refusal := rlAt.Add(-time.Minute)
	d := Decide(rlSeat(delivered, RateLimit{}), rlPass(rlAt, 1, refusedTurns(1, 2, refusal)), p)
	want := RateLimit{Phase: RateLimitOpen, Conv: 1, Since: refusal, Checked: refusal}
	if !reflect.DeepEqual(d.Next.RateLimit, want) {
		t.Fatalf("episode %+v, want %+v", d.Next.RateLimit, want)
	}
	if got := rlMoves(d); !reflect.DeepEqual(got, []RateLimitMove{move(RateLimitTransitionOpened, "")}) {
		t.Fatalf("moves %+v, want opened", got)
	}
	if !d.Changed {
		t.Fatal("an opened episode is not written")
	}
	// The seat is paced paused from the pass that opens it.
	for _, a := range d.Actions {
		if n, ok := a.(Nudge); ok && !n.Paused {
			t.Fatalf("counted nudge %+v on the pass the episode opened", n)
		}
	}
	t.Run("on a refusal at the evidence bound", func(t *testing.T) {
		refusal := rlAt.Add(-DefaultCap)
		d := Decide(rlSeat(refusal.Add(-time.Minute), RateLimit{}), rlPass(rlAt, 1, refusedTurns(1, 2, refusal)), p)
		if want := (RateLimit{Phase: RateLimitOpen, Conv: 1, Since: refusal, Checked: refusal}); !reflect.DeepEqual(d.Next.RateLimit, want) {
			t.Fatalf("episode %+v, want %+v", d.Next.RateLimit, want)
		}
	})
}

// No answered refusal, no episode.
func TestRateLimitNearMisses(t *testing.T) {
	delivered := rlAt.Add(-4 * time.Minute)
	refusal := rlAt.Add(-time.Minute)
	for _, tc := range []struct {
		name string
		prev Record
		obs  Observation
	}{
		{"one refusal (below the threshold)", rlSeat(delivered, RateLimit{}), rlPass(rlAt, 1, refusedTurns(1, 1, refusal))},
		{"a refusal ending as the nudge was delivered is no answer", rlSeat(refusal, RateLimit{}), rlPass(rlAt, 1, refusedTurns(1, 3, refusal))},
		{"a refusal before the delivery is no answer", rlSeat(delivered, RateLimit{}), rlPass(rlAt, 1, refusedTurns(1, 3, delivered.Add(-time.Second)))},
		{"no nudge delivered this generation", rlSeat(time.Time{}, RateLimit{}), rlPass(rlAt, 1, refusedTurns(1, 3, refusal))},
		{"delivered in a previous generation", rlSeat(delivered, RateLimit{}), func() Observation {
			o := rlPass(rlAt, 1, refusedTurns(1, 3, refusal))
			o.Gen = 2
			return o
		}()},
		{"a reading of another conversation", rlSeat(delivered, RateLimit{}), rlPass(rlAt, 2, refusedTurns(1, 3, refusal))},
		{"no reading", rlSeat(delivered, RateLimit{}), rlPass(rlAt, 1, TurnReading{})},
		{"degraded", rlSeat(delivered, RateLimit{}), func() Observation {
			o := rlPass(rlAt, 1, refusedTurns(1, 3, refusal))
			o.Provider.Degraded = true
			return o
		}()},
		{"busy", rlSeat(delivered, RateLimit{}), func() Observation {
			o := rlPass(rlAt, 1, refusedTurns(1, 3, refusal))
			o.Quiet = Quiet{State: NotQuiet}
			return o
		}()},
		{"gated", rlSeat(delivered, RateLimit{}), func() Observation {
			o := rlPass(rlAt, 1, refusedTurns(1, 3, refusal))
			o.Gate = Gate{Held: true, Reason: "restart_pending"}
			return o
		}()},
		{"runtime unknown", rlSeat(delivered, RateLimit{}), func() Observation {
			o := rlPass(rlAt, 1, refusedTurns(1, 3, refusal))
			o.Runtime = TriUnknown
			return o
		}()},
		{
			"a refusal older than the evidence bound answers nothing", rlSeat(rlAt.Add(-DefaultCap-2*time.Minute), RateLimit{}),
			rlPass(rlAt, 1, refusedTurns(1, 3, rlAt.Add(-DefaultCap-time.Second))),
		},
		{"no outstanding work", rlSeat(delivered, RateLimit{}), func() Observation {
			o := rlPass(rlAt, 1, refusedTurns(1, 3, refusal))
			o.Outstanding = nil
			return o
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.prev, tc.obs, p)
			if d.Next.RateLimit.Phase != RateLimitClear || len(rlMoves(d)) != 0 {
				t.Fatalf("episode %+v, moves %+v: want none", d.Next.RateLimit, rlMoves(d))
			}
		})
	}
}

func TestRateLimitCloses(t *testing.T) {
	since := rlAt.Add(-20 * time.Minute)
	fresh := rlAt.Add(-15 * time.Minute)
	lastFresh := rlAt.Add(-40 * time.Minute)
	open := RateLimit{Phase: RateLimitOpen, Conv: 1, Since: since, Checked: rlAt.Add(-5 * time.Minute), LastFresh: lastFresh}
	exhausted := open
	exhausted.Phase, exhausted.Alerted = RateLimitExhausted, AlertRateLimitedAgain
	restarted := RateLimit{Phase: RateLimitRestarted, Conv: 1, Since: since, Checked: fresh, FreshAt: fresh, FromConv: 1, LastFresh: fresh}
	degraded := func(o Observation) Observation { o.Provider.Degraded = true; return o }
	for _, tc := range []struct {
		name  string
		rl    RateLimit
		obs   Observation
		cause RateLimitCause // "": stays open
	}{
		{"open, served since", open, rlPass(rlAt, 1, servedTurns(1, rlAt.Add(-time.Minute))), RateLimitCauseServed},
		{"open, served only before it began", open, rlPass(rlAt, 1, servedTurns(1, since)), ""},
		{"open, served while degraded (a hold lifts on service)", open, degraded(rlPass(rlAt, 1, servedTurns(1, rlAt.Add(-time.Minute)))), RateLimitCauseServed},
		{"open, served in another conversation's reading", open, rlPass(rlAt, 1, servedTurns(2, rlAt.Add(-time.Minute))), ""},
		{"exhausted, served since", exhausted, rlPass(rlAt, 1, servedTurns(1, rlAt.Add(-time.Minute))), RateLimitCauseServed},
		{"open, the conversation changed", open, rlPass(rlAt, 2, TurnReading{}), RateLimitCauseConversationChanged},
		{"exhausted, the conversation changed", exhausted, rlPass(rlAt, 3, TurnReading{}), RateLimitCauseConversationChanged},
		{"restarted, the new conversation served", restarted, rlPass(rlAt, 2, servedTurns(2, rlAt.Add(-time.Minute))), RateLimitCauseServed},
		{"restarted, served only before the restart", restarted, rlPass(rlAt, 2, servedTurns(2, fresh.Add(-time.Second))), ""},
		{"restarted, the conversation moved (its own restart)", restarted, rlPass(rlAt, 2, TurnReading{}), ""},
		{"open, no answered refusal past the evidence bound", open, rlPass(open.Checked.Add(DefaultCap+time.Second), 1, TurnReading{}), RateLimitCauseStale},
		{"open, no answered refusal at the evidence bound", open, rlPass(open.Checked.Add(DefaultCap), 1, TurnReading{}), ""},
		{"restarted, stale", restarted, rlPass(fresh.Add(DefaultCap+time.Second), 2, TurnReading{}), RateLimitCauseStale},
		{"exhausted, stale while degraded", exhausted, degraded(rlPass(open.Checked.Add(DefaultCap+time.Second), 1, TurnReading{})), RateLimitCauseStale},
		{"busy judges nothing", open, func() Observation {
			o := rlPass(rlAt, 1, servedTurns(1, rlAt.Add(-time.Minute)))
			o.Quiet = Quiet{State: NotQuiet}
			return o
		}(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(rlSeat(rlAt.Add(-30*time.Minute), tc.rl), tc.obs, p)
			if tc.cause == "" {
				if d.Next.RateLimit.Phase != tc.rl.Phase || len(rlMoves(d)) != 0 {
					t.Fatalf("episode %+v, moves %+v: want it standing", d.Next.RateLimit, rlMoves(d))
				}
				return
			}
			if got := rlMoves(d); !reflect.DeepEqual(got, []RateLimitMove{move(RateLimitTransitionClosed, tc.cause)}) {
				t.Fatalf("moves %+v, want closed %s", got, tc.cause)
			}
			// Clear, keeping LastFresh unless it is past both gaps by now.
			want := RateLimit{LastFresh: tc.rl.LastFresh}
			if tc.obs.Now.Sub(want.LastFresh) >= DefaultCap {
				want.LastFresh = time.Time{}
			}
			if !reflect.DeepEqual(d.Next.RateLimit, want) {
				t.Fatalf("episode %+v, want %+v", d.Next.RateLimit, want)
			}
		})
	}
}

// A new answered refusal keeps the episode's evidence fresh in every open
// phase, degraded too (a provider hold is no reason to forget it): Checked
// moves to the refusal's own end, never to the pass that reads it, and
// never back.
func TestRateLimitAnsweredRefreshesChecked(t *testing.T) {
	since := rlAt.Add(-5 * time.Minute)
	refusal := rlAt.Add(-time.Minute)
	for _, rl := range []RateLimit{
		{Phase: RateLimitOpen, Conv: 1, Since: since, Checked: since},
		{Phase: RateLimitExhausted, Conv: 1, Since: since, Checked: since, Alerted: AlertRateLimitedAgain},
	} {
		for _, deg := range []bool{false, true} {
			o := rlPass(rlAt, 1, refusedTurns(1, 2, refusal))
			o.Provider.Degraded = deg
			d := Decide(rlSeat(rlAt.Add(-3*time.Minute), rl), o, p)
			if !d.Next.RateLimit.Checked.Equal(refusal) || d.Next.RateLimit.Phase != rl.Phase || len(rlMoves(d)) != 0 {
				t.Fatalf("%v degraded=%v: episode %+v, moves %+v", rl.Phase, deg, d.Next.RateLimit, rlMoves(d))
			}
			// The same refusal read again later moves nothing.
			again := rl
			again.Checked = refusal
			o.Now = rlAt.Add(5 * time.Minute)
			if d := Decide(rlSeat(rlAt.Add(-3*time.Minute), again), o, p); !d.Next.RateLimit.Checked.Equal(refusal) {
				t.Fatalf("%v degraded=%v: re-reading the refusal moved Checked to %v", rl.Phase, deg, d.Next.RateLimit.Checked)
			}
		}
	}
}

// An exhausted seat whose nudges stop reaching it (every delivery fails)
// reads the same answered refusal on every pass for a day, about 144 paused
// nudges. That one refusal must not keep the episode standing: it closes
// stale RateLimitEvidenceMaxAge after the refusal ended, never reopens on
// it, and the seat keeps hearing from the ladder.
func TestRateLimitUndeliveredNudgesGoStale(t *testing.T) {
	refusal := rlAt
	r := rlSeat(rlAt.Add(-time.Minute), RateLimit{
		Phase: RateLimitExhausted, Conv: 1, Since: rlAt.Add(-20 * time.Minute), Checked: refusal, Alerted: AlertRateLimitedAgain,
	})
	r.Entries[0].Last, r.LastNudge = refusal, refusal
	var closed time.Time
	nudges := 0
	step := time.Minute
	end := rlAt.Add(24 * time.Hour)
	for now := rlAt.Add(step); !now.After(end); now = now.Add(step) {
		d := Decide(r, rlPass(now, 1, refusedTurns(1, 2, refusal)), p)
		if d.Changed {
			r = d.Next
		}
		for _, m := range rlMoves(d) {
			if m != move(RateLimitTransitionClosed, RateLimitCauseStale) || !closed.IsZero() {
				t.Fatalf("at +%v: move %+v (closed at +%v)", now.Sub(refusal), m, closed.Sub(refusal))
			}
			closed = now
		}
		for _, a := range d.Actions {
			if n, ok := a.(Nudge); ok {
				nudges++
				// The delivery fails: nothing reaches the seat.
				if out, _, ok := NudgeRefused(r, n.Key, n.Attempt, now); ok {
					r = out
				}
			}
		}
		if !closed.IsZero() && r.RateLimit.Phase != RateLimitClear {
			t.Fatalf("at +%v: the episode reopened on the stale refusal: %+v", now.Sub(refusal), r.RateLimit)
		}
	}
	if closed.IsZero() {
		t.Fatalf("the episode stood for a day on one refusal: %+v", r.RateLimit)
	}
	if late := closed.Sub(refusal); late <= DefaultCap || late > DefaultCap+step {
		t.Fatalf("closed stale %v after the refusal, want just past %v", late, DefaultCap)
	}
	if nudges < 100 {
		t.Fatalf("the seat was nudged %d times in a day: silenced", nudges)
	}
}

// confirmedPass is an acting pass that reads an answered refusal on a seat
// whose episode opened FreshConfirm ago.
func confirmedPass(rl RateLimit) (Record, Observation) {
	prev := rlSeat(rlAt.Add(-3*time.Minute), rl)
	return prev, rlPass(rlAt, rl.Conv, refusedTurns(rl.Conv, 2, rlAt.Add(-time.Minute)))
}

func openSince(since time.Time) RateLimit {
	return RateLimit{Phase: RateLimitOpen, Conv: 1, Since: since, Checked: since}
}

func TestRateLimitRestartsFresh(t *testing.T) {
	since := rlAt.Add(-DefaultPausedCap)
	prev, o := confirmedPass(openSince(since))
	d := Decide(prev, o, p)
	want := RateLimit{Phase: RateLimitRestarted, Conv: 1, Since: since, Checked: o.Turns.RefusedAt, FreshAt: rlAt, FromConv: 1, LastFresh: rlAt}
	if !reflect.DeepEqual(d.Next.RateLimit, want) {
		t.Fatalf("episode %+v, want %+v", d.Next.RateLimit, want)
	}
	if freshRestarts(d) != 1 || countNudges(d) != 0 || d.Next.RecycledGen != 1 {
		t.Fatalf("actions %+v, RecycledGen %d: want one fresh restart, no nudge, the restart's generation", d.Actions, d.Next.RecycledGen)
	}
	if got := rlMoves(d); !reflect.DeepEqual(got, []RateLimitMove{move(RateLimitTransitionRestarted, "")}) {
		t.Fatalf("moves %+v", got)
	}
	if len(rlAlerts(d)) != 0 {
		t.Fatalf("alerts %v on a restart", rlAlerts(d))
	}
	// The fresh restart spends no claim's recycle, and moves no count.
	if e := entry(t, d, "s"); e.Recycles != 0 || e.Attempts != 2 {
		t.Fatalf("entry %+v", e)
	}

	t.Run("not before FreshConfirm", func(t *testing.T) {
		prev, o := confirmedPass(openSince(rlAt.Add(-DefaultPausedCap + time.Second)))
		if d := Decide(prev, o, p); freshRestarts(d) != 0 || d.Next.RateLimit.Phase != RateLimitOpen {
			t.Fatalf("restarted early: %+v", d.Actions)
		}
	})
	t.Run("not without an answered refusal this pass", func(t *testing.T) {
		prev, o := confirmedPass(openSince(since))
		o.Turns = refusedTurns(1, 2, prev.Delivered) // the paused nudge is not answered yet
		if d := Decide(prev, o, p); freshRestarts(d) != 0 {
			t.Fatalf("restarted on no answer: %+v", d.Actions)
		}
	})
	t.Run("not while degraded", func(t *testing.T) {
		prev, o := confirmedPass(openSince(since))
		o.Provider.Degraded = true
		if d := Decide(prev, o, p); freshRestarts(d) != 0 || len(rlMoves(d)) != 0 {
			t.Fatalf("moved while degraded: %+v", d.Actions)
		}
	})
	// FreshConfirm is observed, never inferred from Since: a refusal
	// confirms only when a nudge delivered after the episode opened drew it.
	t.Run("not without a nudge delivered since it opened", func(t *testing.T) {
		prev, o := confirmedPass(openSince(since))
		prev.Delivered = since.Add(-time.Minute) // the nudge the opening refusal answered
		if d := Decide(prev, o, p); freshRestarts(d) != 0 || len(rlMoves(d)) != 0 || len(rlAlerts(d)) != 0 {
			t.Fatalf("confirmed on the opening refusal alone: %+v", d.Actions)
		}
	})
}

// An episode that stood through a long gate hold (an operator hold, a
// quarantine: the generation stays) moves on what it reads after the hold,
// not on the refusal it read before it: past RateLimitEvidenceMaxAge that
// refusal is no evidence, and within it only a refused probe confirms.
func TestRateLimitAfterALongHold(t *testing.T) {
	t.Run("past the evidence bound: closed stale, the ladder resumes", func(t *testing.T) {
		since := rlAt.Add(-3 * time.Hour)
		prev := rlSeat(since.Add(-2*time.Minute), openSince(since))
		d := Decide(prev, rlPass(rlAt, 1, refusedTurns(1, 2, since)), p)
		wantEffects(t, d, move(RateLimitTransitionClosed, RateLimitCauseStale), Nudge{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Attempt: 3})
	})
	t.Run("within it: a probe first, then the restart", func(t *testing.T) {
		since := rlAt.Add(-50 * time.Minute)
		prev := rlSeat(since.Add(-2*time.Minute), openSince(since))
		d := Decide(prev, rlPass(rlAt, 1, refusedTurns(1, 2, since)), p)
		wantEffects(t, d, Nudge{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Attempt: 2, Paused: true})
		if want := (RateLimit{Phase: RateLimitOpen, Conv: 1, Since: since, Checked: since}); !reflect.DeepEqual(d.Next.RateLimit, want) {
			t.Fatalf("episode %+v, want %+v", d.Next.RateLimit, want)
		}
		// The probe reaches the seat and is refused too: confirmed.
		r, _ := NudgeDelivered(d.Next, key("s"), 1, rlAt)
		if d := Decide(r, rlPass(rlAt.Add(2*time.Minute), 1, refusedTurns(1, 3, rlAt.Add(time.Minute))), p); freshRestarts(d) != 1 {
			t.Fatalf("a refused probe did not confirm: %+v", d.Actions)
		}
	})
}

// A seat whose work drains and comes back in the same generation (a pool
// sibling took its trigger, new work arrived): a refusal past the evidence
// bound opens nothing, and one within it opens an episode whose Since
// reaches back to it, which only a refused probe confirms.
func TestRateLimitWhenWorkReturns(t *testing.T) {
	refusal := rlAt.Add(-3 * time.Hour)
	workless := Record{V: 1, Gen: 1, Delivered: refusal.Add(-2 * time.Minute)}
	t.Run("past the evidence bound: no episode", func(t *testing.T) {
		d := Decide(workless, rlPass(rlAt, 1, refusedTurns(1, 2, refusal)), p)
		if d.Next.RateLimit.Phase != RateLimitClear || len(rlMoves(d)) != 0 {
			t.Fatalf("episode %+v, moves %+v", d.Next.RateLimit, rlMoves(d))
		}
	})
	t.Run("an episode the seat kept while workless closes stale", func(t *testing.T) {
		kept := workless
		kept.RateLimit = openSince(refusal)
		d := Decide(kept, rlPass(rlAt, 1, refusedTurns(1, 2, refusal)), p)
		if freshRestarts(d) != 0 || !reflect.DeepEqual(rlMoves(d), []RateLimitMove{move(RateLimitTransitionClosed, RateLimitCauseStale)}) {
			t.Fatalf("episode %+v, actions %+v", d.Next.RateLimit, d.Actions)
		}
	})
	t.Run("within it: the probe confirms, not the backdated Since", func(t *testing.T) {
		refusal := rlAt.Add(-30 * time.Minute)
		r := Record{V: 1, Gen: 1, Delivered: refusal.Add(-2 * time.Minute)}
		reading := refusedTurns(1, 2, refusal)
		var probed, restarted time.Time
		for now := rlAt; now.Before(rlAt.Add(10*time.Minute)) && restarted.IsZero(); now = now.Add(30 * time.Second) {
			d := Decide(r, rlPass(now, 1, reading), p)
			if d.Changed {
				r = d.Next
			}
			for _, a := range d.Actions {
				switch a.(type) {
				case Nudge: // delivered, and refused 30s later
					r, _ = NudgeDelivered(r, key("s"), 1, now)
					probed, reading = now, refusedTurns(1, 3, now.Add(30*time.Second))
				case FreshRestart:
					restarted = now
				}
			}
		}
		if !r.RateLimit.Since.Equal(refusal) || probed.IsZero() || !restarted.Equal(probed.Add(30*time.Second)) {
			t.Fatalf("since %v, probed %v, restarted %v after the work returned", r.RateLimit.Since.Sub(rlAt), probed.Sub(rlAt), restarted.Sub(rlAt))
		}
	})
}

func TestRateLimitExhaustsWithoutARestart(t *testing.T) {
	since := rlAt.Add(-DefaultPausedCap)
	for _, tc := range []struct {
		name   string
		mut    func(*RateLimit, *Observation)
		reason string
		cause  RateLimitCause
	}{
		{
			"refused again within SeatFreshGap of the last fresh restart", func(rl *RateLimit, _ *Observation) { rl.LastFresh = rlAt.Add(-DefaultCap + time.Minute) },
			AlertRateLimitedAgain, RateLimitCauseAgain,
		},
		{"a spent usage quota", func(_ *RateLimit, o *Observation) { o.Turns.Quota = true }, AlertRateLimitedQuota, RateLimitCauseQuota},
		{"a spent quota under a standing hold", func(_ *RateLimit, o *Observation) {
			o.Turns.Quota, o.RestartHold = true, RestartHoldAttached
		}, AlertRateLimitedQuota, RateLimitCauseQuota},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rl := openSince(since)
			prev, o := confirmedPass(rl)
			tc.mut(&prev.RateLimit, &o)
			d := Decide(prev, o, p)
			if freshRestarts(d) != 0 || d.Next.RateLimit.Phase != RateLimitExhausted || d.Next.RateLimit.Alerted != tc.reason {
				t.Fatalf("episode %+v, actions %+v", d.Next.RateLimit, d.Actions)
			}
			if got := rlAlerts(d); !reflect.DeepEqual(got, []string{tc.reason}) {
				t.Fatalf("alerts %v, want %s", got, tc.reason)
			}
			if got := rlMoves(d); !reflect.DeepEqual(got, []RateLimitMove{move(RateLimitTransitionExhausted, tc.cause)}) {
				t.Fatalf("moves %+v", got)
			}
		})
	}
	t.Run("a fresh restart older than SeatFreshGap restarts", func(t *testing.T) {
		prev, o := confirmedPass(openSince(since))
		prev.RateLimit.LastFresh = rlAt.Add(-DefaultCap)
		if d := Decide(prev, o, p); freshRestarts(d) != 1 {
			t.Fatalf("actions %+v", d.Actions)
		}
	})
}

func TestRateLimitHeld(t *testing.T) {
	since := rlAt.Add(-DefaultPausedCap)
	for _, tc := range []struct {
		hold  RestartHold
		cause RateLimitCause
	}{{RestartHoldAttached, RateLimitCauseAttached}, {RestartHoldAwaitingHuman, RateLimitCauseAwaitingHuman}} {
		t.Run(string(tc.cause), func(t *testing.T) {
			prev, o := confirmedPass(openSince(since))
			o.RestartHold = tc.hold
			d := Decide(prev, o, p)
			if freshRestarts(d) != 0 || d.Next.RateLimit.Phase != RateLimitOpen || d.Next.RateLimit.Alerted != AlertRateLimitedHeld {
				t.Fatalf("episode %+v, actions %+v", d.Next.RateLimit, d.Actions)
			}
			if got := rlMoves(d); !reflect.DeepEqual(got, []RateLimitMove{move(RateLimitTransitionHeld, tc.cause)}) {
				t.Fatalf("moves %+v", got)
			}
			// Held again: no second alert or move.
			prev2 := d.Next
			o2 := rlPass(rlAt.Add(10*time.Minute), 1, refusedTurns(1, 3, rlAt.Add(9*time.Minute)))
			o2.RestartHold = tc.hold
			prev2.Delivered = rlAt.Add(5 * time.Minute)
			d2 := Decide(prev2, o2, p)
			if len(rlMoves(d2)) != 0 || len(rlAlerts(d2)) != 0 || freshRestarts(d2) != 0 {
				t.Fatalf("held twice: %+v", d2.Actions)
			}
			// The hold ends: the restart, and no second alert.
			o2.RestartHold = RestartHoldNone
			d3 := Decide(prev2, o2, p)
			if freshRestarts(d3) != 1 || len(rlAlerts(d3)) != 0 || d3.Next.RateLimit.Alerted != AlertRateLimitedHeld {
				t.Fatalf("after the hold: %+v, episode %+v", d3.Actions, d3.Next.RateLimit)
			}
			// Its fresh conversation refused too: exhausted, with no
			// second alert (one per episode). The restart's new generation
			// clears Delivered, so only a nudge delivered after it counts.
			o4 := rlPass(rlAt.Add(15*time.Minute), 2, TurnReading{Read: true, Conv: 2})
			o4.Gen = 2
			d4 := Decide(d3.Next, o4, p)
			if !d4.Next.Delivered.IsZero() || d4.Next.RateLimit.Phase != RateLimitRestarted {
				t.Fatalf("after the restart: %+v", d4.Next)
			}
			r5, _ := NudgeDelivered(d4.Next, key("s"), 2, rlAt.Add(16*time.Minute))
			o5 := rlPass(rlAt.Add(20*time.Minute), 2, refusedTurns(2, 2, rlAt.Add(19*time.Minute)))
			o5.Gen = 2
			d5 := Decide(r5, o5, p)
			if d5.Next.RateLimit.Phase != RateLimitExhausted || len(rlAlerts(d5)) != 0 {
				t.Fatalf("fresh refused: episode %+v, actions %+v", d5.Next.RateLimit, d5.Actions)
			}
			if got := rlMoves(d5); !reflect.DeepEqual(got, []RateLimitMove{move(RateLimitTransitionExhausted, RateLimitCauseFreshRefused)}) {
				t.Fatalf("moves %+v", got)
			}
		})
	}
}

func TestRateLimitBlocked(t *testing.T) {
	since := rlAt.Add(-DefaultPausedCap)
	for _, tc := range []struct {
		name string
		mut  func(*Record, *Observation)
	}{
		{"another seat's fresh restart on the provider within the gap", func(_ *Record, o *Observation) {
			o.ProviderFreshAt = o.Now.Add(-DefaultProviderFreshGap + time.Second)
		}},
		{"a notice pending", func(r *Record, _ *Observation) {
			r.Entries[0].Notice = NoticeMark{Op: "op-1", At: rlAt.Add(-time.Minute)}
		}},
		{"a release pending", func(r *Record, o *Observation) {
			o.Outstanding = append(o.Outstanding, claim("u"))
			r.Entries = append(r.Entries, Entry{Key: key("u"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Last: t0})
			r.Release = &ReleaseMark{Key: key("u"), Gen: 1, At: rlAt.Add(-time.Minute)}
		}},
		{"a claim's live read could not tell", func(_ *Record, o *Observation) {
			o.Hold = map[ClaimKey]bool{key("s"): true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev, o := confirmedPass(openSince(since))
			tc.mut(&prev, &o)
			d := Decide(prev, o, p)
			if freshRestarts(d) != 0 || len(rlMoves(d)) != 0 || len(rlAlerts(d)) != 0 || d.Next.RateLimit.Phase != RateLimitOpen {
				t.Fatalf("blocked pass: episode %+v, actions %+v", d.Next.RateLimit, d.Actions)
			}
			// Still blocked at FreshConfirm + Cap: the held alert, once.
			prev.RateLimit.Since = rlAt.Add(-DefaultPausedCap - DefaultCap)
			if tc.name == "another seat's fresh restart on the provider within the gap" {
				o.ProviderFreshAt = o.Now.Add(-time.Minute)
			}
			d = Decide(prev, o, p)
			if got := rlMoves(d); !reflect.DeepEqual(got, []RateLimitMove{move(RateLimitTransitionHeld, RateLimitCauseBlocked)}) {
				t.Fatalf("moves %+v at the bound", got)
			}
			if got := rlAlerts(d); !reflect.DeepEqual(got, []string{AlertRateLimitedHeld}) {
				t.Fatalf("alerts %v at the bound", got)
			}
		})
	}
	t.Run("a provider fresh restart past the gap does not block", func(t *testing.T) {
		prev, o := confirmedPass(openSince(since))
		o.ProviderFreshAt = o.Now.Add(-DefaultProviderFreshGap)
		if d := Decide(prev, o, p); freshRestarts(d) != 1 {
			t.Fatalf("actions %+v", d.Actions)
		}
	})
}

func TestRateLimitAfterTheRestart(t *testing.T) {
	fresh := rlAt.Add(-20 * time.Minute)
	restarted := RateLimit{Phase: RateLimitRestarted, Conv: 1, Since: fresh.Add(-DefaultPausedCap), Checked: fresh, FreshAt: fresh, FromConv: 1, LastFresh: fresh}
	for _, tc := range []struct {
		name  string
		conv  int64
		cause RateLimitCause
	}{
		{"its fresh conversation refused", 2, RateLimitCauseFreshRefused},
		{"refused in the conversation it left: the restart did not take", 1, RateLimitCauseRestartLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := rlSeat(rlAt.Add(-5*time.Minute), restarted)
			d := Decide(prev, rlPass(rlAt, tc.conv, refusedTurns(tc.conv, 2, rlAt.Add(-time.Minute))), p)
			if d.Next.RateLimit.Phase != RateLimitExhausted || d.Next.RateLimit.Conv != tc.conv || freshRestarts(d) != 0 {
				t.Fatalf("episode %+v, actions %+v", d.Next.RateLimit, d.Actions)
			}
			if got := rlAlerts(d); !reflect.DeepEqual(got, []string{AlertRateLimitedFreshRefused}) {
				t.Fatalf("alerts %v", got)
			}
			if got := rlMoves(d); !reflect.DeepEqual(got, []RateLimitMove{move(RateLimitTransitionExhausted, tc.cause)}) {
				t.Fatalf("moves %+v", got)
			}
			// Exhausted: paced paused, never restarted or alerted again.
			next := d.Next
			next.Delivered = rlAt.Add(10 * time.Minute)
			for i := 1; i <= 9; i++ {
				now := rlAt.Add(time.Duration(i) * 10 * time.Minute)
				d := Decide(next, rlPass(now.Add(time.Minute), tc.conv, refusedTurns(tc.conv, 2+i, now.Add(30*time.Second))), p)
				if freshRestarts(d) != 0 || len(rlAlerts(d)) != 0 || len(rlMoves(d)) != 0 {
					t.Fatalf("pass %d after exhausted: %+v", i, d.Actions)
				}
				for _, a := range d.Actions {
					if n, ok := a.(Nudge); ok && !n.Paused {
						t.Fatalf("counted nudge %+v while exhausted", n)
					}
				}
				next = d.Next
				next.Delivered = now.Add(10 * time.Minute)
			}
		})
	}
	t.Run("a refusal from before the restart moves nothing", func(t *testing.T) {
		prev := rlSeat(fresh.Add(-time.Minute), restarted)
		d := Decide(prev, rlPass(rlAt, 1, refusedTurns(1, 2, fresh.Add(-time.Second))), p)
		if !reflect.DeepEqual(d.Next.RateLimit, restarted) || len(rlMoves(d)) != 0 {
			t.Fatalf("episode %+v, actions %+v", d.Next.RateLimit, d.Actions)
		}
	})
	// A restart that never landed (the same generation and conversation),
	// its paused nudges not reaching the seat: the refusal from before the
	// restart keeps answering the last delivery, but it neither exhausts the
	// restarted episode nor keeps it standing: the episode closes stale an
	// hour after the evidence it restarted on.
	t.Run("a refusal from before the restart keeps nothing standing", func(t *testing.T) {
		r := rlSeat(fresh.Add(-2*time.Minute), restarted)
		reading := refusedTurns(1, 2, fresh.Add(-time.Minute))
		var closed time.Time
		for now := fresh.Add(30 * time.Minute); closed.IsZero() && now.Before(fresh.Add(5*time.Hour)); now = now.Add(30 * time.Minute) {
			d := Decide(r, rlPass(now, 1, reading), p)
			if freshRestarts(d) != 0 || (d.Next.RateLimit.Phase == RateLimitRestarted && !d.Next.RateLimit.Checked.Equal(fresh)) {
				t.Fatalf("%v after the restart: episode %+v, actions %+v", now.Sub(fresh), d.Next.RateLimit, d.Actions)
			}
			if slices.Contains(rlMoves(d), move(RateLimitTransitionClosed, RateLimitCauseStale)) {
				closed = now
			}
			if d.Changed {
				r = d.Next
			}
		}
		if !closed.Equal(fresh.Add(90 * time.Minute)) {
			t.Fatalf("closed stale %v after the restart, want the first pass past RateLimitEvidenceMaxAge", closed.Sub(fresh))
		}
	})
}

// An open episode paces the seat paused, as a degraded provider does: no
// counted nudge, no recycle, no release, no escalation, nudges PausedCap
// apart.
func TestRateLimitPacesPaused(t *testing.T) {
	since := rlAt.Add(-time.Minute)
	for _, e := range []Entry{
		{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 6, Last: t0},                   // would recycle
		{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 6, Recycles: 1, Last: t0},      // would escalate
		{Key: key("s"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Last: t0},                   // would release
		{Key: key("s"), Kind: KindContinuation, Rung: RungAssigned, Since: t0, Attempts: 6, Last: t0},           // would alert
		{Key: key("s"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 3, Last: t0},                // would recycle
		{Key: key("s"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Last: t0, Unreleased: true}, // would retry its release
	} {
		prev := rec(1, e)
		prev.RateLimit = RateLimit{Phase: RateLimitOpen, Conv: 1, Since: since, Checked: since}
		o := quiet(rlAt, 1, Work{Key: e.Key, Kind: e.Kind})
		o.Conv = 1
		if e.Rung >= RungStarted {
			o.Started[e.Key] = SrcAgent
		}
		d := Decide(prev, o, p)
		wantEffects(t, d, Nudge{Key: e.Key, Kind: e.Kind, Rung: e.Rung, Attempt: e.Attempts, Paused: true})
		if d.Next.Release != nil {
			t.Fatalf("%v: a release decided while rate limited", e)
		}
		if got := p.dueAt(entry(t, d, "s"), true); !got.Equal(rlAt.Add(DefaultPausedCap)) {
			t.Fatalf("%v: next due %v, want PausedCap", e, got.Sub(rlAt))
		}
	}
}

func TestRateLimitTidiesLastFresh(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		keep bool
	}{{DefaultCap - time.Second, true}, {DefaultCap, false}} {
		prev := rlSeat(rlAt.Add(-3*time.Minute), RateLimit{LastFresh: rlAt.Add(-tc.age)})
		d := Decide(prev, rlPass(rlAt, 1, TurnReading{}), p)
		if kept := !d.Next.RateLimit.LastFresh.IsZero(); kept != tc.keep {
			t.Fatalf("LastFresh %v old: kept %v, want %v", tc.age, kept, tc.keep)
		}
	}
	// Only on a pass that may act on the seat.
	prev := rlSeat(rlAt.Add(-3*time.Minute), RateLimit{LastFresh: rlAt.Add(-2 * DefaultCap)})
	o := rlPass(rlAt, 1, TurnReading{})
	o.Gate.Held = true
	if d := Decide(prev, o, p); d.Next.RateLimit.LastFresh.IsZero() || d.Changed {
		t.Fatalf("a gated pass tidied the cell: %+v", d.Next.RateLimit)
	}
}

// An episode is the seat's: one restart and one alert however many claims
// it holds, keyed by its first started claim.
func TestRateLimitIsTheSeats(t *testing.T) {
	since := rlAt.Add(-DefaultPausedCap)
	prev := rec(1,
		Entry{Key: key("a"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 2, Last: t0},
		Entry{Key: key("b"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: t0},
		Entry{Key: key("c"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: t0},
	)
	prev.Delivered = rlAt.Add(-3 * time.Minute)
	prev.RateLimit = openSince(since)
	prev.RateLimit.LastFresh = rlAt.Add(-time.Minute) // within SeatFreshGap: alert again
	o := quiet(rlAt, 1, trig("a"), claim("b"), claim("c"))
	o.Started = map[ClaimKey]StartSource{key("b"): SrcAgent, key("c"): SrcAgent}
	o.Conv, o.Turns = 1, refusedTurns(1, 2, rlAt.Add(-time.Minute))
	d := Decide(prev, o, p)
	var alerts []Alert
	for _, a := range d.Actions {
		if al, ok := a.(Alert); ok {
			alerts = append(alerts, al)
		}
	}
	if !reflect.DeepEqual(alerts, []Alert{{Key: key("b"), Reason: AlertRateLimitedAgain}}) {
		t.Fatalf("alerts %+v, want one keyed by the first started claim", alerts)
	}
	if got := subject(rec(1, Entry{Key: key("z"), Kind: KindTrigger})); got != key("z") {
		t.Fatalf("subject with no started claim = %v", got)
	}
}

func TestRestartDue(t *testing.T) {
	since := rlAt.Add(-DefaultPausedCap)
	for _, tc := range []struct {
		rl   RateLimit
		want bool
	}{
		{openSince(since), true},
		{openSince(since.Add(time.Second)), false},
		{RateLimit{Phase: RateLimitRestarted, Since: since}, false},
		{RateLimit{Phase: RateLimitExhausted, Since: since}, false},
		{RateLimit{}, false},
	} {
		if got := RestartDue(Record{RateLimit: tc.rl}, rlAt, p); got != tc.want {
			t.Fatalf("RestartDue(%+v) = %v, want %v", tc.rl, got, tc.want)
		}
	}
}

// The ga-ylpbsy.40 table: a refusal that ends 2s, 9m59s, 10m30s or 19m after
// the nudge it answers opens the episode on the first acting pass that reads
// it, whatever it does to the activity clock, and the seat is restarted
// fresh at the first acting pass at or after Since + FreshConfirm that reads
// a refusal of a nudge delivered after the episode opened (a seat busy with
// a slow refusal cannot act until it ends), with its entry's counts
// unchanged.
func TestRateLimitSlowRefusals(t *testing.T) {
	for _, after := range []time.Duration{2 * time.Second, 9*time.Minute + 59*time.Second, 10*time.Minute + 30*time.Second, 19 * time.Minute} {
		t.Run(after.String(), func(t *testing.T) {
			r := Record{}
			var refusals []time.Time // the seat's turns, each refused
			var answering time.Time  // the end of the turn answering the last delivery
			var opened, restarted time.Time
			var since time.Time
			step := 30 * time.Second
			for now := t0; now.Before(t0.Add(3*time.Hour)) && restarted.IsZero(); now = now.Add(step) {
				if !answering.IsZero() && !now.Before(answering) {
					refusals = append(refusals, answering)
					answering = time.Time{}
				}
				o := startedBy1(quiet(now, 1, claim("s")), "s", SrcAgent)
				o.Conv = 1
				if !answering.IsZero() {
					o.Quiet = Quiet{State: NotQuiet, Reason: "turn running"}
				}
				o.LastActivity = minTime(lastOf(refusals), now)
				if n := len(refusals); n > 0 {
					o.Turns = refusedTurns(1, n, refusals[n-1])
				} else {
					o.Turns = TurnReading{Read: true, Conv: 1}
				}
				d := Decide(r, o, p)
				if d.Changed {
					r = d.Next
				}
				for _, a := range d.Actions {
					switch a := a.(type) {
					case Nudge:
						r, _ = NudgeDelivered(r, a.Key, 1, now)
						answering = now.Add(after)
					case RateLimitMove:
						switch a.Transition {
						case RateLimitTransitionOpened:
							opened, since = now, d.Next.RateLimit.Since
						case RateLimitTransitionRestarted:
							restarted = now
						}
					}
				}
				if !opened.IsZero() && restarted.IsZero() && (r.Entries[0].Attempts > 2 || r.Entries[0].Recycles > 0) {
					t.Fatalf("entry %+v moved while the episode stands", r.Entries[0])
				}
			}
			if opened.IsZero() || restarted.IsZero() {
				t.Fatalf("opened %v, restarted %v; refusals %v", opened, restarted, refusals)
			}
			// It opened on the first acting pass reading the second refusal.
			if second := refusals[1]; opened.Before(second) || opened.Sub(second) >= step {
				t.Fatalf("opened %v after the second refusal at %v", opened.Sub(second), second.Sub(t0))
			}
			if restarted.Before(since.Add(DefaultPausedCap)) || restarted.Sub(since) > DefaultPausedCap+after+DefaultPausedCap+step {
				t.Fatalf("restarted %v after the episode began", restarted.Sub(since))
			}
		})
	}
}

func lastOf(ts []time.Time) time.Time {
	if len(ts) == 0 {
		return time.Time{}
	}
	return ts[len(ts)-1]
}

// A new generation clears Delivered (a refusal must answer a nudge of this
// generation) and keeps the episode (it is about the conversation).
func TestRateLimitAcrossGenerations(t *testing.T) {
	rl := openSince(rlAt.Add(-time.Minute))
	prev := rlSeat(rlAt.Add(-3*time.Minute), rl)
	o := rlPass(rlAt, 1, TurnReading{})
	o.Gen = 2
	d := Decide(prev, o, p)
	if !d.Next.Delivered.IsZero() || !reflect.DeepEqual(d.Next.RateLimit, rl) {
		t.Fatalf("new generation: Delivered %v, episode %+v", d.Next.Delivered, d.Next.RateLimit)
	}
	o.Gen = 1
	if d := Decide(prev, o, p); !d.Next.Delivered.Equal(prev.Delivered) {
		t.Fatalf("same generation dropped Delivered: %v", d.Next.Delivered)
	}
	// A skewed stamp is pulled back to now.
	skew := prev
	skew.Delivered = rlAt.Add(time.Hour)
	skew.RateLimit.Since = rlAt.Add(time.Hour)
	if d := Decide(skew, o, p); !d.Next.Delivered.Equal(rlAt) || !d.Next.RateLimit.Since.Equal(rlAt) {
		t.Fatalf("skewed stamps kept: %v, %v", d.Next.Delivered, d.Next.RateLimit.Since)
	}
}

// A cell differing only in seat-level rate-limit state is a different cell,
// and a seat with no work but an open episode keeps its cell.
func TestRateLimitStateIsWritten(t *testing.T) {
	a := rec(1, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0})
	b := a
	b.Delivered = t0
	c := a
	c.RateLimit = RateLimit{LastFresh: t0}
	if sameRecord(a, b) || sameRecord(a, c) || !sameRecord(a, a) {
		t.Fatal("sameRecord misses seat-level rate-limit state")
	}
	prev := Record{V: 1, Gen: 1, RateLimit: openSince(rlAt.Add(-time.Minute))}
	o := quiet(rlAt, 1)
	o.Conv = 1
	d := Decide(prev, o, p)
	if d.Changed || !reflect.DeepEqual(d.Next.RateLimit, prev.RateLimit) {
		t.Fatalf("a workless seat's episode: changed %v, %+v", d.Changed, d.Next.RateLimit)
	}
	// It still closes stale.
	o.Now = rlAt.Add(DefaultCap + time.Minute)
	if d := Decide(prev, o, p); !d.Changed || d.Next.RateLimit.Phase != RateLimitClear {
		t.Fatalf("a workless seat's episode did not close stale: %+v", d.Next.RateLimit)
	}
}

func TestNudgeDelivered(t *testing.T) {
	at := rlAt
	r := rec(3, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0, Refused: 2})
	got, ok := NudgeDelivered(r, key("s"), 3, at)
	if !ok || !got.Delivered.Equal(at) || got.Entries[0].Refused != 0 {
		t.Fatalf("NudgeDelivered = %+v, %v", got, ok)
	}
	if r.Entries[0].Refused != 2 || !r.Delivered.IsZero() {
		t.Fatal("NudgeDelivered mutated its input")
	}
	if _, ok := NudgeDelivered(got, key("s"), 3, at.Add(-time.Minute)); ok {
		t.Fatal("an older delivery moved Delivered back")
	}
	if out, ok := NudgeDelivered(got, key("s"), 3, at.Add(time.Minute)); !ok || !out.Delivered.Equal(at.Add(time.Minute)) {
		t.Fatalf("a newer delivery = %v, %v", out.Delivered, ok)
	}
	// A delivery of another generation's nudge is not this generation's.
	if out, ok := NudgeDelivered(got, key("s"), 2, at.Add(time.Minute)); ok || !out.Delivered.Equal(at) {
		t.Fatalf("another generation's delivery = %v, %v", out.Delivered, ok)
	}
	if _, ok := NudgeDelivered(Record{}, key("s"), 0, at); ok {
		t.Fatal("NudgeDelivered wrote an absent cell")
	}
}

func TestSettleNoticeDeliveredAt(t *testing.T) {
	at := rlAt
	delivered := at.Add(2 * time.Minute)
	base := Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: at}
	for _, tc := range []struct {
		name   string
		fate   NoticeFate
		paused bool
		since  time.Time
		want   time.Time
	}{
		{"delivered", NoticeDelivered, false, t0, delivered},
		{"delivered paused", NoticeDelivered, true, t0, delivered},
		{"delivered, a later episode of the entry", NoticeDelivered, false, at.Add(time.Minute), delivered},
		{"failed", NoticeFailed, false, t0, time.Time{}},
		{"refused", NoticeRefused, false, t0, time.Time{}},
		{"withdrawn", NoticeWithdrawn, false, t0, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			e.Since = tc.since
			r := rec(1, marked(e, "op-1", at, tc.paused))
			out, _, ok := SettleNotice(r, "op-1", tc.fate, delivered)
			if !ok || !out.Delivered.Equal(tc.want) {
				t.Fatalf("Delivered = %v (ok %v), want %v", out.Delivered, ok, tc.want)
			}
		})
	}
	t.Run("an older delivery time never moves it back", func(t *testing.T) {
		r := rec(1, marked(base, "op-1", at, false))
		r.Delivered = delivered.Add(time.Minute)
		if out, _, _ := SettleNotice(r, "op-1", NoticeDelivered, delivered); !out.Delivered.Equal(r.Delivered) {
			t.Fatalf("Delivered = %v", out.Delivered)
		}
	})
}

func TestRateLimitCodec(t *testing.T) {
	ns := 123456789 * time.Nanosecond
	r := rec(4, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0})
	r.Delivered = t0.Add(ns)
	r.RateLimit = RateLimit{
		Phase: RateLimitRestarted, Conv: 3, Since: t0.Add(time.Minute), Checked: t0.Add(2 * time.Minute), FreshAt: t0.Add(3 * time.Minute),
		FromConv: 2, Alerted: AlertRateLimitedHeld, LastFresh: t0.Add(3 * time.Minute),
	}
	text := EncodeLadder(r)
	got, err := DecodeLadder(text)
	if err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("round trip = %+v, %v\nwant %+v", got, err, r)
	}
	for _, field := range []string{`"delivered":"2026-10-17T20:00:00.123456789Z"`, `"phase":"restarted"`, `"conv":3`, `"from_conv":2`, `"alerted":"rate_limited_held"`, `"fresh_at":`, `"last_fresh":`} {
		if !strings.Contains(text, field) {
			t.Fatalf("cell %s lacks %s", text, field)
		}
	}
	for _, ph := range []RateLimitPhase{RateLimitOpen, RateLimitExhausted} {
		r.RateLimit.Phase = ph
		if got, err := DecodeLadder(EncodeLadder(r)); err != nil || got.RateLimit.Phase != ph {
			t.Fatalf("phase %v round trip = %v, %v", ph, got.RateLimit.Phase, err)
		}
	}

	t.Run("omitempty", func(t *testing.T) {
		plain := rec(4, Entry{Key: key("s"), Kind: KindClaim, Rung: RungStarted, Since: t0})
		if text := EncodeLadder(plain); strings.Contains(text, "rate_limit") || strings.Contains(text, "delivered") {
			t.Fatalf("a cell with no rate-limit state writes it: %s", text)
		}
		plain.RateLimit = RateLimit{LastFresh: t0}
		text := EncodeLadder(plain)
		if !strings.Contains(text, `"rate_limit":{"last_fresh":"2026-10-17T20:00:00Z"}`) {
			t.Fatalf("a clear episode's last fresh restart: %s", text)
		}
		if got, err := DecodeLadder(text); err != nil || !reflect.DeepEqual(got, plain) {
			t.Fatalf("round trip = %+v, %v", got, err)
		}
	})

	t.Run("unreadable rate-limit state is a DecodeIssue", func(t *testing.T) {
		for name, cell := range map[string]string{
			"unknown phase": `{"v":1,"gen":1,"entries":[],"rate_limit":{"phase":"cooling"}}`,
			"clear phase":   `{"v":1,"gen":1,"entries":[],"rate_limit":{"phase":"clear"}}`,
			"unknown alert": `{"v":1,"gen":1,"entries":[],"rate_limit":{"phase":"open","alerted":"provider_rate_limited"}}`,
			"bad since":     `{"v":1,"gen":1,"entries":[],"rate_limit":{"phase":"open","since":"soon"}}`,
			"bad delivered": `{"v":1,"gen":1,"entries":[],"delivered":"later"}`,
		} {
			_, err := DecodeLadder(cell)
			var issue *DecodeIssue
			if !errors.As(err, &issue) {
				t.Fatalf("%s: DecodeLadder = %v, want a DecodeIssue", name, err)
			}
		}
	})

	// A cell written by the per-claim-flag builds (8d6r5, 8df992dfb3, and
	// the unpushed fresh_queued) decodes as-is, and the first rewrite drops
	// the flags.
	t.Run("an earlier build's per-claim flags", func(t *testing.T) {
		old := `{"v":1,"gen":8,"last_nudge":"2026-10-17T20:30:00Z","entries":[` +
			`{"store":"rig:gascity","bead":"s","kind":"claim","rung":"started","since":"2026-10-17T20:00:00Z","attempts":1,"paused":2,` +
			`"last":"2026-10-17T20:30:00Z","next":"2026-10-17T20:40:00Z","recycles":1,"escalated":false,"seed":"agent",` +
			`"recycle_at":"2026-10-17T20:10:00Z","rate_limit_alerted":true,"fresh_recycled":true,"fresh_served":false,"fresh_queued":true}]}`
		got, err := DecodeLadder(old)
		if err != nil {
			t.Fatalf("DecodeLadder: %v", err)
		}
		if e := got.Entries[0]; e.Recycles != 1 || !e.RecycleAt.Equal(t0.Add(10*time.Minute)) || !got.RateLimit.IsZero() {
			t.Fatalf("decoded %+v", got)
		}
		text := EncodeLadder(got)
		for _, flag := range []string{"rate_limit_alerted", "fresh_recycled", "fresh_served", "fresh_queued"} {
			if strings.Contains(text, flag) {
				t.Fatalf("rewrite keeps %s: %s", flag, text)
			}
		}
	})

	// An older build reads a cell this one writes: its decoder (main's
	// wire shape, without the seat-level fields) is plain json.Unmarshal.
	t.Run("an older reader", func(t *testing.T) {
		type oldEntry struct {
			Store, Bead, Kind, Rung string
			Attempts, Paused        int
		}
		type oldLadder struct {
			V           int        `json:"v"`
			Gen         int64      `json:"gen"`
			LastNudge   string     `json:"last_nudge,omitempty"`
			RecycledGen int64      `json:"recycled_gen,omitempty"`
			Entries     []oldEntry `json:"entries"`
		}
		var w oldLadder
		if err := json.Unmarshal([]byte(text), &w); err != nil || w.V != 1 || w.Gen != 4 || len(w.Entries) != 1 {
			t.Fatalf("old reader = %+v, %v", w, err)
		}
	})
}

// The alert reasons an episode records are exactly the ones its cell
// decodes, and every phase has a name.
func TestRateLimitVocabulary(t *testing.T) {
	alerts := []string{AlertRateLimitedFreshRefused, AlertRateLimitedAgain, AlertRateLimitedHeld, AlertRateLimitedQuota}
	for _, a := range alerts {
		if !rateLimitAlerts[a] {
			t.Fatalf("alert %s does not decode", a)
		}
	}
	if len(rateLimitAlerts) != len(alerts) {
		t.Fatalf("decodable alerts %v", rateLimitAlerts)
	}
	names := slices.Collect(func(yield func(string) bool) {
		for _, ph := range []RateLimitPhase{RateLimitClear, RateLimitOpen, RateLimitRestarted, RateLimitExhausted, RateLimitPhase(9)} {
			if !yield(ph.String()) {
				return
			}
		}
	})
	if want := []string{"clear", "open", "restarted", "exhausted", "unknown(9)"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("phase names %v, want %v", names, want)
	}
}

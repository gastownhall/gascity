package workladder

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The notice mark (W5): an entry whose last nudge was handed to an
// asynchronous delivery is held, and holds the seat's nudge slot, until the
// caller settles it.

func marked(e Entry, op string, at time.Time, paused bool) Entry {
	e.Notice = NoticeMark{Op: op, At: at, Paused: paused}
	return e
}

func TestDecideHoldsAMarkedEntry(t *testing.T) {
	now := t0.Add(10 * time.Hour)
	at := now.Add(-time.Minute)
	for _, tc := range []struct {
		name string
		e    Entry
		obs  func(Observation) Observation
	}{
		{"a due nudge", Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: t0}, func(o Observation) Observation { return startedBy1(o, "x", SrcAgent) }},
		{"a started claim's recycle", Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 6, Last: t0}, func(o Observation) Observation { return startedBy1(o, "x", SrcAgent) }},
		{"an unstarted claim's release", Entry{Key: key("x"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Last: t0}, func(o Observation) Observation { return o }},
		{"a continuation's alert", Entry{Key: key("x"), Kind: KindContinuation, Rung: RungAssigned, Since: t0, Attempts: 6, Last: t0}, func(o Observation) Observation { return o }},
		{"a trigger's recycle", Entry{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 3, Last: t0}, func(o Observation) Observation { return o }},
		{"a paused nudge", Entry{Key: key("x"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 1, Last: t0}, func(o Observation) Observation {
			o.Provider.Degraded = true
			return o
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.obs(quiet(now, 1, Work{Key: tc.e.Key, Kind: tc.e.Kind}))
			unmarked := Decide(rec(1, tc.e), o, p)
			if len(unmarked.Actions) == 0 {
				t.Fatalf("the unmarked entry is not due: the case tests nothing")
			}
			d := Decide(rec(1, marked(tc.e, "gc-op-1", at, false)), o, p)
			wantEffects(t, d)
			if got := entry(t, d, "x").Notice; got != (NoticeMark{Op: "gc-op-1", At: at}) {
				t.Fatalf("mark = %+v, want it kept", got)
			}
		})
	}
}

func TestDecideAMarkTakesTheSeatsNudgeSlot(t *testing.T) {
	now := t0.Add(10 * time.Hour)
	at := now.Add(-time.Minute)
	pending := marked(Entry{Key: key("a"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 1, Last: at}, "gc-op-1", at, false)
	t.Run("another due entry gets no nudge", func(t *testing.T) {
		e := Entry{Key: key("b"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: t0}
		o := startedBy1(quiet(now, 1, trig("a"), claim("b")), "b", SrcAgent)
		wantEffects(t, Decide(rec(1, pending, e), o, p))
	})
	t.Run("nor a trigger's recycle, which waits for the slot", func(t *testing.T) {
		e := Entry{Key: key("b"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Attempts: 3, Last: t0}
		wantEffects(t, Decide(rec(1, pending, e), quiet(now, 1, trig("a"), trig("b")), p))
	})
	t.Run("an unstarted claim's first release still runs", func(t *testing.T) {
		e := Entry{Key: key("b"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 3, Last: t0}
		wantEffects(t, Decide(rec(1, pending, e), quiet(now, 1, trig("a"), claim("b")), p), Release{Key: key("b")})
	})
	t.Run("a started claim's recycle still runs", func(t *testing.T) {
		e := Entry{Key: key("b"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 6, Last: t0}
		o := startedBy1(quiet(now, 1, trig("a"), claim("b")), "b", SrcAgent)
		wantEffects(t, Decide(rec(1, pending, e), o, p), Recycle{Key: key("b"), KeepClaim: true}, Alert{Key: key("b"), Reason: AlertStartedRecycled})
	})
	t.Run("settled, the other entry is nudged", func(t *testing.T) {
		e := Entry{Key: key("b"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Last: t0}
		settled := pending
		settled.Notice = NoticeMark{}
		settled.Last = now.Add(-2 * DefaultBase)
		o := startedBy1(quiet(now, 1, trig("a"), claim("b")), "b", SrcAgent)
		r := rec(1, settled, e)
		r.LastNudge = settled.Last
		d := Decide(r, o, p)
		if countNudges(d) != 1 {
			t.Fatalf("effects = %+v, want one nudge", d.Actions)
		}
	})
}

func TestDecideClearsAMarkWhenItsEpisodeEnds(t *testing.T) {
	now := t0.Add(10 * time.Hour)
	at := now.Add(-time.Minute)
	claimed := marked(Entry{Key: key("x"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 1, Last: at}, "gc-op-1", at, false)
	for _, tc := range []struct {
		name string
		prev Record
		obs  Observation
		kept bool
	}{
		{"kept within the episode", rec(1, claimed), quiet(now, 1, claim("x")), true},
		{"kept when the seat is not quiet", rec(1, claimed), func() Observation { o := quiet(now, 1, claim("x")); o.Quiet.State = NotQuiet; return o }(), true},
		{"a new generation", rec(1, claimed), quiet(now, 2, claim("x")), false},
		{"a rung rise", rec(1, claimed), startedBy1(quiet(now, 1, claim("x")), "x", SrcAgent), false},
		{"a progress reset", rec(1, claimed), func() Observation {
			o := quiet(now.Add(DefaultProgressReset+time.Minute), 1, claim("x"))
			o.LastActivity = at.Add(DefaultProgressReset)
			return o
		}(), false},
		{"a kind change", rec(1, claimed), quiet(now, 1, trig("x")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.prev, tc.obs, p)
			got := entry(t, d, "x").Notice
			if tc.kept != !got.IsZero() {
				t.Fatalf("mark = %+v, want kept %v", got, tc.kept)
			}
		})
	}
	t.Run("the entry dropped", func(t *testing.T) {
		d := Decide(rec(1, claimed), quiet(now, 1), p)
		if len(d.Next.Entries) != 0 || !d.Changed {
			t.Fatalf("next = %+v, want the entry, and its mark, gone", d.Next)
		}
	})
	t.Run("a mark from a clock ahead is clamped", func(t *testing.T) {
		ahead := marked(claimed, "gc-op-1", now.Add(time.Hour), false)
		o := quiet(now, 1, claim("x"))
		o.Quiet.State = NotQuiet
		if got := entry(t, Decide(rec(1, ahead), o, p), "x").Notice.At; !got.Equal(now) {
			t.Fatalf("mark at %v, want clamped to %v", got, now)
		}
	})
}

func TestMarkNotice(t *testing.T) {
	r := rec(1, Entry{Key: key("a"), Kind: KindTrigger, Rung: RungAssigned, Since: t0}, Entry{Key: key("b"), Kind: KindTrigger, Rung: RungAssigned, Since: t0})
	m := NoticeMark{Op: "gc-op-1", At: t0, Paused: true}
	out, ok := MarkNotice(r, key("b"), m)
	if !ok || out.Entries[1].Notice != m || !out.Entries[0].Notice.IsZero() {
		t.Fatalf("MarkNotice = %+v, %v; want only b marked", out.Entries, ok)
	}
	if !r.Entries[1].Notice.IsZero() {
		t.Fatal("MarkNotice mutated its input")
	}
	if _, ok := MarkNotice(r, key("c"), m); ok {
		t.Fatal("MarkNotice marked an entry that is not there")
	}
}

func TestSettleNotice(t *testing.T) {
	at := t0.Add(time.Hour)
	base := Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 2, Paused: 1, Last: at}
	type want struct {
		attempts, paused, refused int
		alert                     bool
	}
	for _, tc := range []struct {
		name    string
		fate    NoticeFate
		paused  bool
		refused int
		since   time.Time
		want    want
	}{
		{"delivered ends refusals", NoticeDelivered, false, 2, t0, want{2, 1, 0, false}},
		{"delivered paused ends refusals", NoticeDelivered, true, 2, t0, want{2, 1, 0, false}},
		{"failed counts", NoticeFailed, false, 0, t0, want{2, 1, 0, false}},
		{"failed keeps refusals", NoticeFailed, false, 2, t0, want{2, 1, 2, false}},
		{"refused re-counts paused", NoticeRefused, false, 0, t0, want{1, 2, 1, false}},
		{"the third refusal alerts", NoticeRefused, false, 2, t0, want{1, 2, 3, true}},
		{"the fourth refusal does not", NoticeRefused, false, 3, t0, want{1, 2, 4, false}},
		{"refusals that go on re-alert", NoticeRefused, false, refusedAlertAt + refusedRealertEvery - 1, t0, want{1, 2, refusedAlertAt + refusedRealertEvery, true}},
		{"withdrawn re-counts paused, no refusal", NoticeWithdrawn, false, 2, t0, want{1, 2, 2, false}},
		{"a paused mark refused records nothing", NoticeRefused, true, 2, t0, want{2, 1, 2, false}},
		{"a paused mark withdrawn records nothing", NoticeWithdrawn, true, 0, t0, want{2, 1, 0, false}},
		{"a later episode is not re-counted", NoticeRefused, false, 2, at.Add(time.Minute), want{2, 1, 2, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			e.Refused, e.Since = tc.refused, tc.since
			e = marked(e, "gc-op-1", at, tc.paused)
			r := rec(1, e)
			before := EncodeLadder(r)
			out, alert, ok := SettleNotice(r, "gc-op-1", tc.fate)
			if !ok {
				t.Fatal("SettleNotice did not find the mark")
			}
			got := out.Entries[0]
			if !got.Notice.IsZero() {
				t.Fatalf("mark %+v, want cleared", got.Notice)
			}
			if g := (want{got.Attempts, got.Paused, got.Refused, alert}); g != tc.want {
				t.Fatalf("settled = %+v, want %+v", g, tc.want)
			}
			if EncodeLadder(r) != before {
				t.Fatal("SettleNotice mutated its input")
			}
		})
	}
	t.Run("attempts floor at zero", func(t *testing.T) {
		e := marked(Entry{Key: key("x"), Kind: KindClaim, Rung: RungStarted, Since: t0, Last: at}, "gc-op-1", at, false)
		out, _, ok := SettleNotice(rec(1, e), "gc-op-1", NoticeWithdrawn)
		if !ok || out.Entries[0].Attempts != 0 || out.Entries[0].Paused != 1 {
			t.Fatalf("settled = %+v, %v", out.Entries[0], ok)
		}
	})
	t.Run("an unknown op", func(t *testing.T) {
		r := rec(1, marked(base, "gc-op-1", at, false))
		if out, alert, ok := SettleNotice(r, "gc-op-2", NoticeDelivered); ok || alert || !reflect.DeepEqual(out, r) {
			t.Fatalf("SettleNotice(unknown) = %+v, %v, %v", out, alert, ok)
		}
		if _, _, ok := SettleNotice(r, "", NoticeDelivered); ok {
			t.Fatal("SettleNotice matched an empty op id")
		}
	})
}

func TestLadderCodecRoundTripsTheNoticeMark(t *testing.T) {
	at := t0.Add(123456789 * time.Nanosecond)
	r := Record{V: 1, Gen: 3, Entries: []Entry{
		marked(Entry{Key: key("a"), Kind: KindClaim, Rung: RungStarted, Since: t0, Attempts: 1, Last: at}, "gc-op-1", at, false),
		marked(Entry{Key: key("b"), Kind: KindTrigger, Rung: RungAssigned, Since: t0, Paused: 1, Last: at}, "gc-op-2", at, true),
		{Key: key("c"), Kind: KindTrigger, Rung: RungAssigned, Since: t0},
	}}
	text := EncodeLadder(r)
	got, err := DecodeLadder(text)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !reflect.DeepEqual(got, r) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, r)
	}
	if strings.Count(text, `"notice"`) != 2 || strings.Count(text, `"paused":true`) != 1 {
		t.Fatalf("cell %s: want two notice marks, one paused, and none on the unmarked entry", text)
	}
	unmarked, err := DecodeLadder(`{"v":1,"gen":1,"entries":[{"store":"s","bead":"x","kind":"claim","rung":"started","since":"","attempts":0,"paused":0,"last":"","next":"","recycles":0,"escalated":false,"seed":"","recycle_at":""}]}`)
	if err != nil || !unmarked.Entries[0].Notice.IsZero() {
		t.Fatalf("a cell without the field = %+v, %v; want no mark", unmarked, err)
	}
}

func TestDecodeLadderRefusesABadNoticeMark(t *testing.T) {
	for name, notice := range map[string]string{
		"empty op": `{"op":"","at":"2026-10-17T20:00:00Z"}`,
		"no at":    `{"op":"gc-op-1"}`,
		"bad at":   `{"op":"gc-op-1","at":"yesterday"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeLadder(`{"v":1,"gen":1,"entries":[{"store":"s","bead":"x","kind":"claim","rung":"started","since":"","attempts":0,"paused":0,"last":"","next":"","recycles":0,"escalated":false,"seed":"","recycle_at":"","notice":` + notice + `}]}`)
			var issue *DecodeIssue
			if !errors.As(err, &issue) {
				t.Fatalf("DecodeLadder = %v, want a DecodeIssue", err)
			}
		})
	}
}

// WN1: no Decision nudges while an entry is marked, whatever the record and
// observation; the sequence property (TestLadderInvariants) runs with
// random marks and their settles.
func TestPropertyNoNudgeWhileMarked(t *testing.T) {
	forEachSequence(t, func(seed uint64) error {
		s := newSim(seed)
		s.marks = true
		return s.run()
	})
}

// A release that finds its claim started begins a new episode at the instant
// the nudge it shared a decision with was reserved: the mark is dropped with
// the old episode, as a rung rise drops it, so the old claimed-rung notice
// is never re-counted on the started episode, and its refusals end with it.
func TestReleaseFoundStartedDropsTheMark(t *testing.T) {
	at := t0.Add(time.Hour)
	e := Entry{Key: key("x"), Kind: KindClaim, Rung: RungClaimed, Since: t0, Attempts: 4, Refused: 2, Unreleased: true, Last: at}
	e = marked(e, "op-1", at, false)
	r := rec(1, e)
	r.Release = &ReleaseMark{Key: key("x"), Gen: 1, At: at}
	r, ok := ReleaseFoundStarted(r, key("x"), 1, SrcClaimSession, at)
	if !ok {
		t.Fatal("no release")
	}
	got := r.Entries[0]
	if !got.Notice.IsZero() || got.Rung != RungStarted || got.Attempts != 0 || got.Paused != 0 || got.Refused != 0 {
		t.Fatalf("entry %+v, want a fresh started episode with no mark", got)
	}
	for _, fate := range []NoticeFate{NoticeRefused, NoticeWithdrawn, NoticeFailed, NoticeDelivered} {
		if out, _, ok := SettleNotice(r, "op-1", fate); ok || !reflect.DeepEqual(out, r) {
			t.Fatalf("fate %d: settled %+v on the started episode", fate, out.Entries[0])
		}
	}
	if due := p.dueAt(got, false); !due.Equal(at.Add(p.Grace)) {
		t.Fatalf("first reminder due %v, want the grace %v", due, at.Add(p.Grace))
	}
}

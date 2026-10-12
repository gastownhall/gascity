package main

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

var inflightT0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

var (
	startEntry  = inflightEntry{Kind: "start", Key: rowKey{Leg: "sessions", ID: "gc-1"}}
	createEntry = inflightEntry{Kind: inflightCreate, Token: "tok-1", Identity: "worker/worker-1", Leg: "sessions"}
)

// mustAdd adds e and returns its sequence number.
func mustAdd(t *testing.T, m *inflightMap, e inflightEntry) uint64 {
	t.Helper()
	seq := m.add(e)
	if seq == 0 {
		t.Fatalf("add %+v refused", e)
	}
	return seq
}

// ambiguousCreate is a map holding createEntry, settled ambiguous at
// settledAt.
func ambiguousCreate(t *testing.T, settledAt time.Time) *inflightMap {
	t.Helper()
	m := newInflightMap()
	seq := mustAdd(t, m, createEntry)
	m.settle(settlement{Kind: inflightCreate, Seq: seq, Token: createEntry.Token, Outcome: settledAmbiguous, At: settledAt})
	return m
}

func tokens(ts ...string) inflightCensus {
	c := inflightCensus{Tokens: make(map[string]bool)}
	for _, tok := range ts {
		c.Tokens[tok] = true
	}
	return c
}

// Kills clearing an ambiguous create on the wrong marker (P5): only a census
// row that carries its token, on any leg, clears it before the hard bound.
func TestInflightClearsByMarker(t *testing.T) {
	settled := inflightT0.Add(time.Second)
	for name, tc := range map[string]struct {
		census inflightCensus
		clear  bool
	}{
		"its token":       {tokens("tok-1"), true},
		"another token":   {tokens("tok-2"), false},
		"no census token": {tokens(), false},
	} {
		t.Run(name, func(t *testing.T) {
			m := ambiguousCreate(t, settled)
			got := m.clearVisible(tc.census, settled.Add(time.Second))
			if cleared := len(got) == 1 && !got[0].HardBound; cleared != tc.clear || len(got) > 1 {
				t.Fatalf("clears = %+v, want cleared by token %v", got, tc.clear)
			}
			if tc.clear != (len(m.view().Entries) == 0) {
				t.Fatalf("entries left = %+v, want cleared %v", m.view().Entries, tc.clear)
			}
		})
	}
}

// Kills any settled effect but an ambiguous create held as in-flight work
// (P5, START-CREATE A5): a landed start (no marker, no inventory catch-up),
// an already-running start, a refused or failed effect of any kind, and a
// landed or failed create all clear at settlement.
func TestInflightOnlyAmbiguousCreatesOutliveSettlement(t *testing.T) {
	for _, k := range []string{"start", "write", "stop", "close", "rollback", "zombie", "kind-from-a-later-PR"} {
		m := newInflightMap()
		key := rowKey{Leg: "sessions", ID: "gc-" + k}
		seq := mustAdd(t, m, inflightEntry{Kind: k, Key: key})
		m.settle(settlement{Kind: k, Seq: seq, Key: key, At: inflightT0})
		if got := m.view().Entries; len(got) != 0 {
			t.Fatalf("kind %s: entries after its settlement = %+v, want none", k, got)
		}
	}
	for _, ambiguous := range []bool{false, true} {
		m := newInflightMap()
		seq := mustAdd(t, m, createEntry)
		s := settlement{Kind: inflightCreate, Seq: seq, Token: createEntry.Token, Outcome: settledLanded, At: inflightT0}
		if ambiguous {
			s.Outcome = settledAmbiguous
		}
		m.settle(s)
		if held := len(m.view().Entries) == 1; held != ambiguous {
			t.Fatalf("create ambiguous=%v: held %v after settlement, want %v", ambiguous, held, ambiguous)
		}
	}
}

// Kills a hard bound counted from submit: a create that settled late would
// clear before its token had 3 minutes to appear.
func TestInflightHardBoundCountsFromSettlement(t *testing.T) {
	settled := inflightT0.Add(2 * time.Minute)
	m := ambiguousCreate(t, settled)
	if got := m.clearVisible(tokens(), inflightT0.Add(inflightHardBound+time.Second)); len(got) != 0 {
		t.Fatalf("clears 3m after submit = %+v, want none: the bound counts from settlement", got)
	}
	if got := m.clearVisible(tokens(), settled.Add(inflightHardBound-time.Nanosecond)); len(got) != 0 {
		t.Fatalf("clears before the bound = %+v, want none", got)
	}
	if got := m.clearVisible(tokens(), settled.Add(inflightHardBound)); len(got) != 1 || !got[0].HardBound {
		t.Fatalf("clears at the bound = %+v, want one hard-bound clear", got)
	}
}

// Kills clearing a live start at 3 minutes (B-3), which would let a second
// start through while the first still runs. At startup_timeout = 3m the
// start's effect deadline is 3m10s (P3); running effects outlive any bound
// until their settlement is drained.
func TestHardBoundNeverClearsRunningEffect(t *testing.T) {
	cfg := config.SessionConfig{StartupTimeout: "3m"}
	deadline := cfg.StartupTimeoutDuration() + 10*time.Second
	if deadline != 3*time.Minute+10*time.Second {
		t.Fatalf("start deadline = %v, want 3m10s", deadline)
	}
	m := newInflightMap()
	startSeq := mustAdd(t, m, startEntry)
	mustAdd(t, m, createEntry)
	for _, at := range []time.Duration{inflightHardBound, inflightHardBound + time.Second, deadline - time.Nanosecond, 10 * time.Minute} {
		if got := m.clearVisible(tokens(), inflightT0.Add(at)); len(got) != 0 || len(m.view().Entries) != 2 {
			t.Fatalf("at +%v: clears %+v, entries %+v, want both running effects held", at, got, m.view().Entries)
		}
	}
	m.settle(settlement{Kind: "start", Seq: startSeq, Key: startEntry.Key, At: inflightT0.Add(deadline)})
	if got := m.view().Entries; len(got) != 1 || got[0].Kind != inflightCreate {
		t.Fatalf("entries after the start's settlement = %+v, want only the running create", got)
	}
}

// Kills an ambiguous create stuck forever when its row never appears, and a
// silent clear: it clears at the bound with a record naming its identity and
// leg, for the alert.
func TestInflightHardBoundAlertsAndClears(t *testing.T) {
	settled := inflightT0.Add(time.Second)
	m := ambiguousCreate(t, settled)
	got := m.clearVisible(tokens(), settled.Add(inflightHardBound))
	if len(got) != 1 || !got[0].HardBound || got[0].Entry.Identity != createEntry.Identity || got[0].Entry.Leg != "sessions" {
		t.Fatalf("clears = %+v, want one hard-bound record naming the identity and leg", got)
	}
	if left := m.view().Entries; len(left) != 0 {
		t.Fatalf("entries left = %+v, want none", left)
	}
}

// Kills an optimistic clear of an ambiguous create, which would let the
// pass overshoot the cap if the row did land: it counts every pass until its
// token shows, then its census row counts instead.
func TestInflightAmbiguousCountsUntilMarker(t *testing.T) {
	m := ambiguousCreate(t, inflightT0)
	for i := 1; i <= 3; i++ {
		now := inflightT0.Add(time.Duration(i) * time.Minute / 2)
		if got := m.clearVisible(tokens(), now); len(got) != 0 {
			t.Fatalf("pass %d: clears = %+v, want none before the token shows", i, got)
		}
		if n := m.view().uncensusedCreates(tokens()); n != 1 {
			t.Fatalf("pass %d: uncensused creates = %d, want the ambiguous create counted", i, n)
		}
	}
	landed := tokens("tok-1")
	if n := m.view().uncensusedCreates(landed); n != 0 {
		t.Fatalf("uncensused creates with the token's row = %d, want the row to count instead", n)
	}
	if got := m.clearVisible(landed, inflightT0.Add(2*time.Minute)); len(got) != 1 || got[0].HardBound {
		t.Fatalf("clears with the token = %+v, want one by marker", got)
	}
}

// countOnce is the city in-flight count (P4) over v and a census whose
// pending-create rows are pending, with tokens c: the distinct rows with a
// running start or a pending claim, plus the creates no census row carries.
func countOnce(v inflightView, pending map[rowKey]bool, c inflightCensus) int {
	rows := make(map[rowKey]bool, len(pending))
	for k := range pending {
		rows[k] = true
	}
	for _, e := range v.Entries {
		if e.Kind == "start" {
			rows[e.Key] = true
		}
	}
	return len(rows) + v.uncensusedCreates(c)
}

// Kills double counting (P4, I-inflight): a bring-up counts once whether the
// census shows its row before the create settles, after it, or after an
// ambiguous entry cleared, and a start on a pending row takes no further
// slot. Seeded worlds of creates, starts and pending rows.
func TestInflightCountsEffectOnce(t *testing.T) {
	for seed := int64(1); seed <= 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		m := newInflightMap()
		pending := make(map[rowKey]bool)
		c := tokens()
		bringUps := 0
		for i := 0; i < 1+rng.Intn(8); i++ {
			bringUps++
			row := rowKey{Leg: "sessions", ID: fmt.Sprintf("gc-%d", i)}
			switch rng.Intn(3) {
			case 0: // a start, maybe on a row that holds its pending claim
				m.add(inflightEntry{Kind: "start", Key: row})
				pending[row] = rng.Intn(2) == 0
			case 1: // a pending row with nothing running
				pending[row] = true
			default: // a create, running or settled, its row shown or not
				tok := fmt.Sprintf("tok-%d", i)
				seq := m.add(inflightEntry{Kind: inflightCreate, Token: tok, Leg: "sessions"})
				switch rng.Intn(3) {
				case 0:
					m.settle(settlement{Kind: inflightCreate, Seq: seq, Token: tok, Outcome: settledAmbiguous, At: inflightT0})
				case 1:
					m.settle(settlement{Kind: inflightCreate, Seq: seq, Token: tok, At: inflightT0}) // landed: its row shows
					pending[row], c.Tokens[tok] = true, true
					continue
				}
				if rng.Intn(2) == 0 {
					pending[row], c.Tokens[tok] = true, true
				}
			}
		}
		for k, p := range pending {
			if !p {
				delete(pending, k)
			}
		}
		if got := countOnce(m.view(), pending, c); got != bringUps {
			t.Fatalf("seed %d: count before clearing = %d, want %d\nentries %+v\npending %v tokens %v", seed, got, bringUps, m.view().Entries, pending, c.Tokens)
		}
		m.clearVisible(c, inflightT0.Add(time.Second))
		if got := countOnce(m.view(), pending, c); got != bringUps {
			t.Fatalf("seed %d: count after clearing = %d, want %d\nentries %+v\npending %v tokens %v", seed, got, bringUps, m.view().Entries, pending, c.Tokens)
		}
	}
}

// Kills a second entry for a row or token that has one (the pass proposes
// nothing for a row in flight), a refused kind, and a settlement that is
// duplicate, stale (an earlier submit's) or for an unknown entry clearing
// the row's or token's current effect (P3).
func TestInflightAddAndSettleOnce(t *testing.T) {
	m := newInflightMap()
	if m.add(inflightEntry{Kind: inflightCreate}) != 0 || m.add(inflightEntry{Kind: "stop"}) != 0 {
		t.Fatal("add accepted a create without its token or a row effect without its row")
	}
	createSeq := mustAdd(t, m, createEntry)
	startSeq := mustAdd(t, m, startEntry)
	if m.add(createEntry) != 0 || m.add(startEntry) != 0 {
		t.Fatal("add accepted a duplicate token or row")
	}
	mustAdd(t, m, inflightEntry{Key: rowKey{Leg: "sessions", ID: "gc-opaque"}}) // no kind is refused

	m.settle(settlement{Kind: inflightCreate, Seq: createSeq, Token: "tok-other", At: inflightT0})
	m.settle(settlement{Kind: "start", Seq: startSeq, Key: rowKey{Leg: "sessions", ID: "gc-other"}, At: inflightT0})
	m.settle(settlement{Kind: inflightCreate, Seq: createSeq, Token: createEntry.Token, Outcome: settledAmbiguous, At: inflightT0})
	m.settle(settlement{Kind: inflightCreate, Seq: createSeq, Token: createEntry.Token, At: inflightT0.Add(time.Minute)})
	if e := m.creates[createEntry.Token]; !e.Ambiguous || e.SettledAt != inflightT0 {
		t.Fatalf("create = %+v, want held by its first settlement", e)
	}

	// The start settles; the next start on the row is submitted; the first
	// start's settlement arrives again and must not clear the second.
	m.settle(settlement{Kind: "start", Seq: startSeq, Key: startEntry.Key, At: inflightT0})
	next := mustAdd(t, m, startEntry)
	m.settle(settlement{Kind: "start", Seq: startSeq, Key: startEntry.Key, At: inflightT0})
	if e, ok := m.running[startEntry.Key]; !ok || e.Seq != next {
		t.Fatalf("running[%v] = %+v (held %v), want the next start (seq %d) kept past a stale settlement", startEntry.Key, e, ok, next)
	}
	m.settle(settlement{Kind: "start", Seq: next, Key: startEntry.Key, At: inflightT0})
	if _, ok := m.running[startEntry.Key]; ok {
		t.Fatal("the next start's own settlement did not clear it")
	}
}

// Kills settle dispatching on anything but the kind (must-fix 1): a row
// effect's settlement clears its row whatever other fields it carries, a
// token included, so the row never wedges.
func TestInflightRowSettlementClearsWhateverItCarries(t *testing.T) {
	for name, extra := range map[string]func(*settlement){
		"plain":     func(*settlement) {},
		"token":     func(s *settlement) { s.Token = "tok-row" },
		"ambiguous": func(s *settlement) { s.Token, s.Outcome = "tok-row", settledAmbiguous },
		"error":     func(s *settlement) { s.Err = fmt.Errorf("provider: boom") },
	} {
		for _, k := range []string{"start", "zombie"} {
			m := newInflightMap()
			seq := mustAdd(t, m, inflightEntry{Kind: k, Key: startEntry.Key})
			s := settlement{Kind: k, Seq: seq, Key: startEntry.Key, At: inflightT0}
			extra(&s)
			m.settle(s)
			if got := m.view().Entries; len(got) != 0 {
				t.Fatalf("%s %s: entries after the row's settlement = %+v, want none", name, k, got)
			}
		}
	}
}

// Kills a clear of a running create (P5): only a settled ambiguous create
// clears by its token, so a running create whose row the census already
// shows stays until its settlement drains.
func TestInflightRunningCreateNeverClearsByToken(t *testing.T) {
	m := newInflightMap()
	mustAdd(t, m, createEntry)
	if got := m.clearVisible(tokens(createEntry.Token), inflightT0.Add(10*time.Minute)); len(got) != 0 || len(m.view().Entries) != 1 {
		t.Fatalf("clears %+v, entries %+v, want the running create held", got, m.view().Entries)
	}
}

// Kills an entry that outlives its effect (I8): once every effect's
// settlement drained and every ambiguous create's token showed or its bound
// passed, the map is empty.
func TestInflightEmptyAtQuiescence(t *testing.T) {
	m := newInflightMap()
	var settles []settlement
	for i := 0; i < 6; i++ {
		row := rowKey{Leg: "sessions", ID: fmt.Sprintf("gc-%d", i)}
		seq := mustAdd(t, m, inflightEntry{Kind: "start", Key: row})
		settles = append(settles, settlement{Kind: "start", Seq: seq, Key: row, At: inflightT0})
		tok := fmt.Sprintf("tok-%d", i)
		seq = mustAdd(t, m, inflightEntry{Kind: inflightCreate, Token: tok})
		s := settlement{Kind: inflightCreate, Seq: seq, Token: tok, Outcome: settledLanded, At: inflightT0}
		if i%2 == 0 {
			s.Outcome = settledAmbiguous
		}
		settles = append(settles, s)
	}
	for _, s := range settles {
		m.settle(s)
	}
	m.clearVisible(tokens("tok-0"), inflightT0.Add(time.Second))
	m.clearVisible(tokens(), inflightT0.Add(inflightHardBound))
	if got := m.view().Entries; len(got) != 0 {
		t.Fatalf("entries at quiescence = %+v, want none", got)
	}
}

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
	startEntry  = inflightEntry{Kind: inflightStart, Key: rowKey{Leg: "sessions", ID: "gc-1"}}
	createEntry = inflightEntry{Kind: inflightCreate, Token: "tok-1", Identity: "worker/worker-1", Leg: "sessions"}
)

// ambiguousCreate is a map holding createEntry, settled ambiguous at
// settledAt.
func ambiguousCreate(t *testing.T, settledAt time.Time) *inflightMap {
	t.Helper()
	m := newInflightMap()
	if !m.add(createEntry) {
		t.Fatal("add refused")
	}
	m.settle(settlement{Token: createEntry.Token, Ambiguous: true, At: settledAt})
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
	for k := inflightStart; k <= inflightZombie; k++ {
		m := newInflightMap()
		key := rowKey{Leg: "sessions", ID: fmt.Sprintf("gc-%d", k)}
		if !m.add(inflightEntry{Kind: k, Key: key}) {
			t.Fatalf("kind %d: add refused", k)
		}
		m.settle(settlement{Key: key, At: inflightT0})
		if got := m.view().Entries; len(got) != 0 {
			t.Fatalf("kind %d: entries after its settlement = %+v, want none", k, got)
		}
	}
	for _, ambiguous := range []bool{false, true} {
		m := newInflightMap()
		m.add(createEntry)
		m.settle(settlement{Token: createEntry.Token, Ambiguous: ambiguous, At: inflightT0})
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
	if !m.add(startEntry) || !m.add(createEntry) {
		t.Fatal("add refused")
	}
	for _, at := range []time.Duration{inflightHardBound, inflightHardBound + time.Second, deadline - time.Nanosecond, 10 * time.Minute} {
		if got := m.clearVisible(tokens(), inflightT0.Add(at)); len(got) != 0 || len(m.view().Entries) != 2 {
			t.Fatalf("at +%v: clears %+v, entries %+v, want both running effects held", at, got, m.view().Entries)
		}
	}
	m.settle(settlement{Key: startEntry.Key, At: inflightT0.Add(deadline)})
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
		if e.Kind == inflightStart {
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
				m.add(inflightEntry{Kind: inflightStart, Key: row})
				pending[row] = rng.Intn(2) == 0
			case 1: // a pending row with nothing running
				pending[row] = true
			default: // a create, running or settled, its row shown or not
				tok := fmt.Sprintf("tok-%d", i)
				m.add(inflightEntry{Kind: inflightCreate, Token: tok, Leg: "sessions"})
				switch rng.Intn(3) {
				case 0:
					m.settle(settlement{Token: tok, Ambiguous: true, At: inflightT0})
				case 1:
					m.settle(settlement{Token: tok, At: inflightT0}) // landed: its row shows
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
// nothing for a row in flight) and a settlement applied twice or to an
// unknown entry (P3).
func TestInflightAddAndSettleOnce(t *testing.T) {
	m := newInflightMap()
	if !m.add(createEntry) || !m.add(startEntry) || m.add(createEntry) || m.add(startEntry) ||
		m.add(inflightEntry{Kind: inflightCreate}) || m.add(inflightEntry{Kind: inflightStop}) || m.add(inflightEntry{Key: startEntry.Key}) {
		t.Fatal("add accepted a duplicate, an entry without its token or row, or no kind")
	}
	m.settle(settlement{Token: "tok-other", At: inflightT0})
	m.settle(settlement{Key: rowKey{Leg: "sessions", ID: "gc-other"}, At: inflightT0})
	m.settle(settlement{Token: createEntry.Token, Ambiguous: true, At: inflightT0})
	m.settle(settlement{Token: createEntry.Token, At: inflightT0.Add(time.Minute)})
	e := m.view().Entries
	if len(e) != 2 || e[1].Token != "tok-1" || !e[1].Ambiguous || e[1].SettledAt != inflightT0 {
		t.Fatalf("entries = %+v, want the start running and the create held by its first settlement", e)
	}
}

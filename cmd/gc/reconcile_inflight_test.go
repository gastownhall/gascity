package main

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

var inflightT0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// inflightWith is a map holding e, recorded at inflightT0 and, unless
// outcome is running, settled at settledAt with outcome and m.
func inflightWith(t *testing.T, e inflightEntry, outcome inflightState, m inflightMarker, wroteRow bool, settledAt time.Time) *inflightMap {
	t.Helper()
	im := newInflightMap()
	e.SubmittedAt = inflightT0
	if !im.add(e) {
		t.Fatalf("add %+v refused", e)
	}
	if outcome != inflightRunning {
		im.settle(settlement{Key: e.Key, Outcome: outcome, Marker: m, WroteRow: wroteRow, At: settledAt})
	}
	return im
}

func inflightKeys(m *inflightMap) []string {
	var out []string
	for _, e := range m.view().Entries {
		out = append(out, e.Key)
	}
	return out
}

func censusOf(rows map[rowKey]inflightRow, cleanLegs ...string) inflightCensus {
	c := inflightCensus{Rows: rows, Legs: make(map[string]bool)}
	for _, l := range cleanLegs {
		c.Legs[l] = true
	}
	return c
}

var (
	startEntry  = inflightEntry{Kind: inflightStart, Key: "row:sessions/gc-1", Leg: "sessions", Marker: inflightMarker{RowID: "gc-1"}}
	createEntry = inflightEntry{Kind: inflightCreate, Key: "create:worker/worker-1", Leg: "sessions", Marker: inflightMarker{InstanceToken: "tok-1"}}
	reopenEntry = inflightEntry{Kind: inflightReopen, Key: "row:sessions/gc-9", Leg: "sessions", Marker: inflightMarker{RowID: "gc-9"}}
)

// Kills: clearing on the wrong marker (C5.4(1)), which counts an effect twice
// (a marker missed) or not at all (a marker read from another row): a create
// by its row ID or token on any leg, a start by the generation its PreWake
// wrote or its row's absence from a clean read, a reopen by its row open on
// its leg.
func TestInflightClearsByMarker(t *testing.T) {
	settled := inflightT0.Add(time.Second)
	cases := []struct {
		name   string
		entry  inflightEntry
		marker inflightMarker
		census inflightCensus
		clear  bool
	}{
		{
			"create by row ID on a rig leg", createEntry,
			inflightMarker{RowID: "gc-7"},
			censusOf(map[rowKey]inflightRow{{Leg: "rig:a", ID: "gc-7"}: {}}, "sessions"), true,
		},
		{
			"create by token", createEntry,
			inflightMarker{},
			censusOf(map[rowKey]inflightRow{{Leg: "sessions", ID: "gc-8"}: {InstanceToken: "tok-1"}}), true,
		},
		{
			"create beside another token and row", createEntry,
			inflightMarker{RowID: "gc-7"},
			censusOf(map[rowKey]inflightRow{{Leg: "sessions", ID: "gc-8"}: {InstanceToken: "tok-2"}}, "sessions"), false,
		},
		{
			"start at its generation", startEntry,
			inflightMarker{Generation: 5},
			censusOf(map[rowKey]inflightRow{{Leg: "sessions", ID: "gc-1"}: {Generation: 5}}), true,
		},
		{
			"start past its generation", startEntry,
			inflightMarker{Generation: 5},
			censusOf(map[rowKey]inflightRow{{Leg: "sessions", ID: "gc-1"}: {Generation: 6}}), true,
		},
		{
			"start before its generation", startEntry,
			inflightMarker{Generation: 5},
			censusOf(map[rowKey]inflightRow{{Leg: "sessions", ID: "gc-1"}: {Generation: 4}}, "sessions"), false,
		},
		{
			"start's generation on another row", startEntry,
			inflightMarker{Generation: 5},
			censusOf(map[rowKey]inflightRow{{Leg: "sessions", ID: "gc-1"}: {Generation: 4}, {Leg: "sessions", ID: "gc-2"}: {Generation: 9}}, "sessions"), false,
		},
		{"start's row gone from a clean read", startEntry, inflightMarker{Generation: 5}, censusOf(nil, "sessions"), true},
		{"start's row gone from a read with an error", startEntry, inflightMarker{Generation: 5}, censusOf(nil, "rig:a"), false},
		{
			"reopen open on its leg", reopenEntry,
			inflightMarker{},
			censusOf(map[rowKey]inflightRow{{Leg: "sessions", ID: "gc-9"}: {}}), true,
		},
		{
			"reopen only on another leg", reopenEntry,
			inflightMarker{},
			censusOf(map[rowKey]inflightRow{{Leg: "rig:a", ID: "gc-9"}: {}}, "sessions"), false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := inflightWith(t, tc.entry, inflightLanded, tc.marker, true, settled)
			got := m.clearVisible(tc.census, settled.Add(time.Second))
			if cleared := len(got) == 1 && !got[0].HardBound; cleared != tc.clear || len(got) > 1 {
				t.Fatalf("clears = %+v, want cleared by marker %v", got, tc.clear)
			}
			if tc.clear != (len(m.view().Entries) == 0) {
				t.Fatalf("entries left = %v, want cleared %v", inflightKeys(m), tc.clear)
			}
		})
	}
}

// Kills a reintroduced inventory catch-up rule (D-15, B-2): a settled start
// clears on its marker alone, in the first pass whose census shows it. The
// map reads nothing but the census.
func TestInflightStartClearsOnMarkerAlone(t *testing.T) {
	settled := inflightT0.Add(time.Second)
	m := inflightWith(t, startEntry, inflightLanded, inflightMarker{Generation: 3}, true, settled)
	census := censusOf(map[rowKey]inflightRow{{Leg: "sessions", ID: "gc-1"}: {Generation: 3, StartLease: true}})
	if got := m.clearVisible(census, settled); len(got) != 1 || got[0].HardBound || got[0].Entry.Marker.Generation != 3 {
		t.Fatalf("clears = %+v, want the start cleared by its marker at once", got)
	}
}

// Kills a hard bound counted from submit: an entry that settled late would
// clear before its marker had 3 minutes to appear.
func TestInflightHardBoundCountsFromSettlement(t *testing.T) {
	settled := inflightT0.Add(2 * time.Minute)
	m := inflightWith(t, createEntry, inflightLanded, inflightMarker{RowID: "gc-7"}, true, settled)
	empty := censusOf(nil, "sessions")
	if got := m.clearVisible(empty, inflightT0.Add(inflightHardBound+time.Second)); len(got) != 0 {
		t.Fatalf("clears 3m after submit = %+v, want none: the bound counts from settlement", got)
	}
	if got := m.clearVisible(empty, settled.Add(inflightHardBound-time.Nanosecond)); len(got) != 0 {
		t.Fatalf("clears before the bound = %+v, want none", got)
	}
	if got := m.clearVisible(empty, settled.Add(inflightHardBound)); len(got) != 1 || !got[0].HardBound {
		t.Fatalf("clears at the bound = %+v, want one hard-bound clear", got)
	}
}

// Kills clearing a live start at 3 minutes (B-3), which would let a second
// start through while the first still runs. At startup_timeout = 3m the
// start's effect deadline is 3m10s (C1.14); the entry outlives the bound
// until it settles, and the bound then counts from the settlement.
func TestHardBoundNeverClearsRunningEffect(t *testing.T) {
	cfg := config.SessionConfig{StartupTimeout: "3m"}
	deadline := cfg.StartupTimeoutDuration() + 10*time.Second
	if deadline != 3*time.Minute+10*time.Second {
		t.Fatalf("start deadline = %v, want 3m10s", deadline)
	}
	m := inflightWith(t, startEntry, inflightRunning, inflightMarker{}, false, time.Time{})
	empty := censusOf(nil)
	for _, at := range []time.Duration{inflightHardBound, inflightHardBound + time.Second, deadline - time.Nanosecond, 10 * time.Minute} {
		if got := m.clearVisible(empty, inflightT0.Add(at)); len(got) != 0 {
			t.Fatalf("clears of a running start at +%v = %+v, want none", at, got)
		}
	}
	settled := inflightT0.Add(deadline)
	m.settle(settlement{Key: startEntry.Key, Outcome: inflightFailed, Marker: inflightMarker{Generation: 2}, WroteRow: true, At: settled})
	if got := m.clearVisible(empty, settled.Add(inflightHardBound-time.Nanosecond)); len(got) != 0 {
		t.Fatalf("clears before the bound from settlement = %+v, want none", got)
	}
	if got := m.clearVisible(empty, settled.Add(inflightHardBound)); len(got) != 1 || !got[0].HardBound {
		t.Fatalf("clears at the bound from settlement = %+v, want one hard-bound clear", got)
	}
}

// Kills an entry stuck forever when its marker never appears, and a silent
// clear: each landed, ambiguous or failed-after-write entry clears at the
// bound with a record naming the entry, its key and leg, for the alert.
func TestInflightHardBoundAlertsAndClears(t *testing.T) {
	settled := inflightT0.Add(time.Second)
	for _, tc := range []struct {
		outcome  inflightState
		wroteRow bool
	}{{inflightLanded, true}, {inflightAmbiguous, true}, {inflightFailed, true}} {
		m := inflightWith(t, createEntry, tc.outcome, inflightMarker{}, tc.wroteRow, settled)
		got := m.clearVisible(censusOf(nil, "sessions"), settled.Add(inflightHardBound))
		if len(got) != 1 || !got[0].HardBound || got[0].Entry.Key != createEntry.Key || got[0].Entry.Leg != "sessions" || got[0].Entry.State != tc.outcome {
			t.Fatalf("outcome %d: clears = %+v, want one hard-bound record naming the entry", tc.outcome, got)
		}
		if keys := inflightKeys(m); len(keys) != 0 {
			t.Fatalf("outcome %d: entries left = %v, want none", tc.outcome, keys)
		}
	}
}

// Kills write, stop or close entries lingering as phantom in-flight work
// (C5.2): whatever its outcome, only a create, start or reopen outlives its
// settlement.
func TestInflightOnlyCreateStartReopenOutliveSettlement(t *testing.T) {
	for k := inflightCreate; k <= inflightZombie; k++ {
		for _, outcome := range []inflightState{inflightLanded, inflightAmbiguous, inflightFailed} {
			e := inflightEntry{Kind: k, Key: fmt.Sprintf("row:sessions/gc-%d", k), Leg: "sessions", Marker: inflightMarker{RowID: "gc-1"}}
			m := inflightWith(t, e, outcome, inflightMarker{}, true, inflightT0)
			want := k == inflightCreate || k == inflightStart || k == inflightReopen
			if got := len(m.view().Entries) == 1; got != want {
				t.Fatalf("kind %d outcome %d: held after settlement %v, want %v", k, outcome, got, want)
			}
		}
	}
}

// Kills an effect that wrote nothing held until the hard bound, starving the
// cap (C5.4(2)): a failure without a write, and a refusal, clear at
// settlement; a failure after a write waits for its marker.
func TestInflightFailedWithoutWriteClearsAtOnce(t *testing.T) {
	for _, e := range []inflightEntry{createEntry, startEntry, reopenEntry} {
		for _, tc := range []struct {
			outcome  inflightState
			wroteRow bool
			held     bool
		}{{inflightFailed, false, false}, {inflightRefused, false, false}, {inflightFailed, true, true}} {
			m := inflightWith(t, e, tc.outcome, inflightMarker{}, tc.wroteRow, inflightT0)
			if got := len(m.view().Entries) == 1; got != tc.held {
				t.Fatalf("kind %d outcome %d wroteRow %v: held %v, want %v", e.Kind, tc.outcome, tc.wroteRow, got, tc.held)
			}
		}
	}
}

// Kills a start that found its runtime alive held as in flight (C5.4(2),
// C8.10): it wrote nothing, so it clears at settlement and frees its slot.
func TestInflightAlreadyRunningClearsAtOnce(t *testing.T) {
	m := inflightWith(t, startEntry, inflightAlreadyRunning, inflightMarker{}, false, inflightT0)
	if n, _ := m.view().capped(censusOf(nil)); n != 0 || len(m.view().Entries) != 0 {
		t.Fatalf("entries %v, capped %d, want the already-running start cleared", inflightKeys(m), n)
	}
}

// Kills an optimistic clear of an ambiguous create, which would let the
// pass overshoot the cap if the row did land: it counts every pass until its
// marker shows, then its row counts instead.
func TestInflightAmbiguousCountsUntilMarker(t *testing.T) {
	m := inflightWith(t, createEntry, inflightAmbiguous, inflightMarker{}, true, inflightT0)
	empty := censusOf(nil, "sessions")
	for i := 1; i <= 3; i++ {
		now := inflightT0.Add(time.Duration(i) * time.Minute / 2)
		if got := m.clearVisible(empty, now); len(got) != 0 {
			t.Fatalf("pass %d: clears = %+v, want none before the marker", i, got)
		}
		if n, _ := m.view().capped(empty); n != 1 {
			t.Fatalf("pass %d: capped = %d, want the ambiguous create counted", i, n)
		}
	}
	landed := censusOf(map[rowKey]inflightRow{{Leg: "sessions", ID: "gc-7"}: {InstanceToken: "tok-1", StartLease: true}}, "sessions")
	if got := m.clearVisible(landed, inflightT0.Add(2*time.Minute)); len(got) != 1 || got[0].HardBound {
		t.Fatalf("clears with the marker = %+v, want one by marker", got)
	}
	if got := countOnce(m.view(), landed); got != 1 {
		t.Fatalf("count after the clear = %d, want the row counted once", got)
	}
}

// countOnce is the city in-flight count over v and c (C5.13): the capped
// entries, plus each census start lease no entry stands for.
func countOnce(v inflightView, c inflightCensus) int {
	n, represented := v.capped(c)
	for k, r := range c.Rows {
		if r.StartLease && !represented[k] {
			n++
		}
	}
	return n
}

// Kills double counting (C5.13): an entry and the census row it produced
// count once, by row key or a create's token, whether the census shows the
// row before the settlement, after it, or after the entry cleared. Seeded
// worlds of creates, starts and pre-existing leases.
func TestInflightCountsEffectOnce(t *testing.T) {
	for seed := int64(1); seed <= 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		m := newInflightMap()
		c := censusOf(make(map[rowKey]inflightRow), "sessions")
		effects := 0
		for i := 0; i < 1+rng.Intn(8); i++ {
			effects++
			row := rowKey{Leg: "sessions", ID: fmt.Sprintf("gc-%d", i)}
			token := fmt.Sprintf("tok-%d", i)
			if rng.Intn(4) == 0 { // a lease from before a restart: no entry
				c.Rows[row] = inflightRow{StartLease: true}
				continue
			}
			kind, e := inflightCreate, inflightEntry{Kind: inflightCreate, Key: "create:w/" + token, Leg: "sessions", Marker: inflightMarker{InstanceToken: token}}
			if rng.Intn(2) == 0 {
				kind, e = inflightStart, inflightEntry{Kind: inflightStart, Key: rowBackoffKey(row), Leg: "sessions", Marker: inflightMarker{RowID: row.ID}}
				c.Rows[row] = inflightRow{Generation: 1}
			}
			m.add(e)
			visible := rng.Intn(2) == 0
			if rng.Intn(2) == 0 {
				s := settlement{Key: e.Key, Outcome: inflightLanded, WroteRow: true, At: inflightT0}
				if kind == inflightCreate && rng.Intn(2) == 0 {
					s.Marker.RowID = row.ID
				} else if kind == inflightStart {
					s.Marker.Generation = 2
				}
				m.settle(s)
			}
			if visible {
				r := inflightRow{StartLease: true, Generation: 2}
				if kind == inflightCreate {
					r.InstanceToken = token
				}
				c.Rows[row] = r
			}
		}
		if got := countOnce(m.view(), c); got != effects {
			t.Fatalf("seed %d: count before clearing = %d, want %d\nentries %+v\ncensus %+v", seed, got, effects, m.view().Entries, c.Rows)
		}
		m.clearVisible(c, inflightT0.Add(time.Second))
		if got := countOnce(m.view(), c); got != effects {
			t.Fatalf("seed %d: count after clearing = %d, want %d\nentries %+v\ncensus %+v", seed, got, effects, m.view().Entries, c.Rows)
		}
	}
}

// Kills a second entry for a key that has one (C5.5) and a settlement
// applied twice or to an unknown key (C5.6).
func TestInflightAddAndSettleOnce(t *testing.T) {
	m := newInflightMap()
	if !m.add(createEntry) || m.add(createEntry) || m.add(inflightEntry{Kind: inflightCreate}) || m.add(inflightEntry{Key: "row:x/y"}) {
		t.Fatal("add accepted a duplicate key, an empty key or no kind")
	}
	m.settle(settlement{Key: "create:other", Outcome: inflightFailed, At: inflightT0})
	m.settle(settlement{Key: createEntry.Key, Outcome: inflightLanded, Marker: inflightMarker{RowID: "gc-7"}, WroteRow: true, At: inflightT0})
	m.settle(settlement{Key: createEntry.Key, Outcome: inflightFailed, At: inflightT0.Add(time.Minute)})
	e := m.view().Entries
	if len(e) != 1 || e[0].State != inflightLanded || e[0].SettledAt != inflightT0 ||
		e[0].Marker != (inflightMarker{RowID: "gc-7", InstanceToken: "tok-1"}) {
		t.Fatalf("entries = %+v, want the first settlement only, merged into the submitted marker", e)
	}
}

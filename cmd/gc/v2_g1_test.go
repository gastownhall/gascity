package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Gate G1 (architecture §1.8 and §3.1; plan A3): the allocate and pass
// budgets at city scale, and G1b, gather on a production-shaped store. Both
// time the host they run on, so they run only when asked: GC_V2_G1=1, and
// GC_V2_G1B_STORE=<a read-only copy of a city's SQLite store directory>.
// The allocation ratchet always runs.

// v2G1Env is the gates' environment lookup: injected, never set. It reads
// the environment as the process started: TestMain scrubs GC_* before any
// test runs.
var v2G1Env = func() func(string) (string, bool) {
	env := make(map[string]string)
	for _, key := range []string{"GC_V2_G1", "GC_V2_G1B_STORE"} {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}()

// v2AllocateAllocsCeiling is the recorded allocations of one decide over
// the 300-session synthetic city (A3 measured 15,819), with headroom for
// -race jitter.
const v2AllocateAllocsCeiling = 16900

// withV2Backlog adds an unassigned, routed, ready backlog spread round-robin
// over the city's templates, so its demand view holds total work beads.
// G1 fixes the total, not the mix: A1's assigned work stays as it is, and
// the backlog is what a busy city queues.
func withV2Backlog(city v2BenchCity, total int) v2BenchCity {
	col := &city.In.Demand.Collected
	templates := len(city.In.Cfg.Agents)
	for i := len(city.Work) + len(col.UnassignedRouted); i < total; i++ {
		template := benchTemplate(i % templates)
		id := fmt.Sprintf("bk-%06d", i)
		col.UnassignedRouted = append(col.UnassignedRouted, beads.Bead{
			ID: id, Title: id, Type: "task", Status: "open", CreatedAt: censusNow,
			Metadata: map[string]string{"gc.routed_to": template},
		})
		col.UnassignedRoutedRefs = append(col.UnassignedRoutedRefs, "")
		d := col.DefaultDemand[template]
		d.WorkBeadIDs = append(d.WorkBeadIDs, id)
		d.Count++
		col.DefaultDemand[template] = d
		col.DefaultCounts[template]++
	}
	return city
}

// v2Pass is one pass's work over the city, as far as v2 builds it today:
// the census read through a primed cache holding four closed rows per open
// one, the allocator's decide over it, and every row's decide.
func v2Pass(tb testing.TB, city v2BenchCity) func() {
	tb.Helper()
	rows := slices.Clone(city.Sessions)
	for i := 0; i < 4*len(city.Sessions); i++ {
		row := poolRow(fmt.Sprintf("bx-%06d", i), benchTemplate(i%30), i/30+1, "asleep")
		row.Status = "closed"
		rows = append(rows, row)
	}
	cache := beads.NewCachingStoreForTest(censusStore(rows...), nil)
	if err := cache.Prime(context.Background()); err != nil {
		tb.Fatal(err)
	}
	legs := []classStoreCandidate{{ref: benchSessionsLeg, store: cache}}
	return func() {
		in := city.In
		c, err := readSessionCensus(in.Now, legs)
		if err != nil {
			tb.Fatal(err)
		}
		in.Census = c
		d, err := decideAllocation(in)
		if err != nil {
			tb.Fatal(err)
		}
		w := &World{Now: in.Now, Census: c, Observed: observeCensus(in.Obs, c, in.Now, in.ObsMaxAge)}
		for _, row := range c.Canonical() {
			decideRow(w, &d, row.Key)
		}
	}
}

// medianPerOp is op's median time per run over five benchmark rounds.
func medianPerOp(op func()) time.Duration {
	var per []time.Duration
	for range 5 {
		r := testing.Benchmark(func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				op()
			}
		})
		per = append(per, time.Duration(r.NsPerOp()))
	}
	slices.Sort(per)
	return per[len(per)/2]
}

// Kills: a reintroduced quadratic scan in the decide or the pass (the
// realization, the slot claim, the awake set, pool desired, the snapshot's
// ID lookup) at the scale gate G1 names.
func TestV2G1Budget(t *testing.T) {
	if v, _ := v2G1Env("GC_V2_G1"); v != "1" {
		t.Skip("gate G1 times this host: set GC_V2_G1=1")
	}
	// The budgets are the orchestrator's G1 ruling on A3's max review
	// (2026-10-07), revising the architecture's 50 ms allocate: allocate at
	// 1,000 sessions with the 10k backlog ≤90 ms; a whole pass ≤100 ms at
	// 1,000 and ≤250 ms at 5,000, the last measured on a quiet machine
	// before C9. Every city carries ten work beads per session.
	for _, tc := range []struct {
		sessions int
		pass     bool
		budget   time.Duration
	}{{1000, false, 90 * time.Millisecond}, {1000, true, 100 * time.Millisecond}, {5000, true, 250 * time.Millisecond}} {
		what := "allocate"
		if tc.pass {
			what = "pass"
		}
		t.Run(fmt.Sprintf("%s_sessions=%d", what, tc.sessions), func(t *testing.T) {
			city := withV2Backlog(newV2BenchCity(t, tc.sessions, 30), 10*tc.sessions)
			op := func() {
				if _, err := decideAllocation(city.In); err != nil {
					t.Fatal(err)
				}
			}
			if tc.pass {
				op = v2Pass(t, city)
			}
			got := medianPerOp(op)
			t.Logf("%s at %d sessions, %d work beads: %v (budget %v)", what, tc.sessions, 10*tc.sessions, got, tc.budget)
			if got > tc.budget {
				t.Errorf("%s at %d sessions = %v, over G1's %v", what, tc.sessions, got, tc.budget)
			}
		})
	}
}

// Kills: reintroduced per-request copies and rescans in the decide (A2,
// A3): each multiplies its allocations.
func TestV2AllocateAllocsRatchet(t *testing.T) {
	in := newV2BenchCity(t, 300, 30).In
	allocs := testing.AllocsPerRun(2, func() {
		if _, err := decideAllocation(in); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > v2AllocateAllocsCeiling {
		t.Fatalf("decide at 300 sessions allocates %.0f times, over the recorded %d", allocs, v2AllocateAllocsCeiling)
	}
}

// G1b (ruling 3): gather's per-pass store reads, the census and the demand
// gather over the same primed cache, on a copy of a production store
// (maintainer-city: about 109k closed session rows), at or under about
// 150 ms p99. A failure triggers A4. The city's config is not loaded (a
// production read): demand reads the assigned and routed work it needs no
// config for.
func TestV2GatherProductionShape(t *testing.T) {
	dir, _ := v2G1Env("GC_V2_G1B_STORE")
	if dir == "" {
		t.Skip("gate G1b reads a store copy: set GC_V2_G1B_STORE")
	}
	store, err := beads.OpenSQLiteStore(dir, beads.WithSQLiteStoreReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := store.(interface{ CloseStore() error }); ok {
		t.Cleanup(func() { _ = c.CloseStore() })
	}
	cache := beads.NewCachingStoreForTest(store, nil)
	t.Cleanup(cache.StopReconciler)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	legs := []classStoreCandidate{{ref: "city:g1b", store: cache}}
	cfg := &config.City{Workspace: config.Workspace{Name: "g1b"}}
	var per []time.Duration
	for range 100 {
		start := time.Now()
		c, err := readSessionCensus(start, legs)
		if err != nil {
			t.Fatal(err)
		}
		env := censusDemandEnv(c, cache, nil)
		env.CityName, env.Cfg = "g1b", cfg
		if _, err := gatherDemand(env, newV2DemandReads(start, &externalReadsRecording{})); err != nil {
			t.Fatal(err)
		}
		per = append(per, time.Since(start))
	}
	slices.Sort(per)
	p99 := per[98]
	t.Logf("census and demand gather p50 %v, p99 %v", per[49], p99)
	if p99 > 150*time.Millisecond {
		t.Errorf("census and demand gather p99 = %v, over G1b's 150ms: A4 (incremental census) is needed", p99)
	}
}

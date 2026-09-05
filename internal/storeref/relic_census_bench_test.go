package storeref

// What the census costs, and on which population.
//
// HasLegacyResidents lists the binding with AllowScan and IncludeClosed, so its
// cost is the binding's WHOLE history, not its working set — and the verdict is
// load-bearing on `gc bd`'s by-id path, the busiest one-shot route in the CLI.
// A one-shot process pays this before it answers anything.
//
// The verdict is computed LIVE, once per process per city — the one-shot funnel
// and the controller boot each take it exactly once and hold it in memory. There
// is no on-disk note: a remembered verdict is a status file, it goes stale the
// moment an operator rebuilds a binding, and nothing would clear it (AGENTS.md,
// "No status files — query live state").
//
// So this number is what a process pays, and this benchmark is the regression
// guard on it. The binding is seeded CLEAN because that is the expensive case:
// a scan that finds a relic can stop, a scan that finds none has read the whole
// history to prove it. Bounding it needs a store-level id-namespace predicate
// that beads.ListQuery does not have; ga-dx4ho carries that.

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func benchmarkRelicCensus(b *testing.B, rows int, closedFraction int) {
	b.Helper()
	store, err := beads.OpenSQLiteStore(b.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		b.Fatalf("OpenSQLiteStore: %v", err)
	}
	for i := range rows {
		bead, err := store.Create(beads.Bead{Title: fmt.Sprintf("graph node %d", i), Type: "task"})
		if err != nil {
			b.Fatalf("seeding row %d: %v", i, err)
		}
		if closedFraction > 0 && i%closedFraction == 0 {
			if err := store.Close(bead.ID); err != nil {
				b.Fatalf("closing row %d: %v", i, err)
			}
		}
	}
	binding := ClassBinding{Prefixes: []string{"gcg"}, Leg: Leg{Ref: "graph", Store: store}}
	b.ResetTimer()
	for b.Loop() {
		if HasLegacyResidents(binding) {
			b.Fatal("seeded binding reports relics; the fixture mints under its own prefix")
		}
	}
}

// The closed population is the one ga-qdt5y.19 added to the scan by widening the
// verdict from OPEN residents to ALL of them, so the fixture is mostly closed.
func BenchmarkRelicCensusCleanBinding_1k(b *testing.B)  { benchmarkRelicCensus(b, 1000, 2) }
func BenchmarkRelicCensusCleanBinding_10k(b *testing.B) { benchmarkRelicCensus(b, 10000, 2) }

// The open-only cost, for comparison: what the census charged before the
// verdict was widened.
func BenchmarkRelicCensusOpenOnly_10k(b *testing.B) {
	store, err := beads.OpenSQLiteStore(b.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		b.Fatalf("OpenSQLiteStore: %v", err)
	}
	for i := range 10000 {
		bead, err := store.Create(beads.Bead{Title: fmt.Sprintf("graph node %d", i), Type: "task"})
		if err != nil {
			b.Fatalf("seeding row %d: %v", i, err)
		}
		if i%2 == 0 {
			if err := store.Close(bead.ID); err != nil {
				b.Fatalf("closing row %d: %v", i, err)
			}
		}
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := OpenLegacyResidents(store, []string{"gcg"}); err != nil {
			b.Fatalf("OpenLegacyResidents: %v", err)
		}
	}
}

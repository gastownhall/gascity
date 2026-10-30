package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The v2 pass benchmarks (architecture §1.8; gate G1 is A3's). Run with
// go test -run '^$' -bench 'BenchmarkV2' -benchtime=1x ./cmd/gc. The large
// allocation cases take minutes and tens of GB on origin/main: select
// sizes with -bench on a shared host.

// BenchmarkV2DecideAllocation is the allocator's whole decide over the
// synthetic city.
func BenchmarkV2DecideAllocation(b *testing.B) {
	for _, tc := range []struct{ n, templates int }{{100, 30}, {300, 30}, {1000, 30}, {2000, 30}, {5000, 30}, {1000, 100}} {
		b.Run(fmt.Sprintf("sessions=%d/templates=%d", tc.n, tc.templates), func(b *testing.B) {
			in := newV2BenchCity(b, tc.n, tc.templates).In
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := decideAllocation(in); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkV2CensusRead is one census read through a primed CachingStore
// over a MemStore holding open session rows and four times as many closed.
func BenchmarkV2CensusRead(b *testing.B) {
	for _, open := range []int{100, 1000, 5000} {
		closed := 4 * open
		b.Run(fmt.Sprintf("open=%d/closed=%d", open, closed), func(b *testing.B) {
			rows := newV2BenchCity(b, open, 30).Sessions
			for i := 0; i < closed; i++ {
				row := poolRow(fmt.Sprintf("bx-%05d", i), benchTemplate(i%30), i/30+1, "asleep")
				row.Status = "closed"
				rows = append(rows, row)
			}
			cache := beads.NewCachingStoreForTest(censusStore(rows...), nil)
			if err := cache.Prime(context.Background()); err != nil {
				b.Fatal(err)
			}
			legs := []classStoreCandidate{{ref: benchSessionsLeg, store: cache}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c, err := readSessionCensus(censusNow, legs)
				if err != nil {
					b.Fatal(err)
				}
				if len(c.Rows) != open {
					b.Fatalf("census rows = %d, want %d", len(c.Rows), open)
				}
			}
		})
	}
}

// BenchmarkV2RowDecide is decideRow over every row, against the synthetic
// city's decided snapshot.
func BenchmarkV2RowDecide(b *testing.B) {
	for _, n := range []int{100, 300, 1000} {
		b.Run(fmt.Sprintf("sessions=%d/templates=30", n), func(b *testing.B) {
			in := newV2BenchCity(b, n, 30).In
			d, err := decideAllocation(in)
			if err != nil {
				b.Fatal(err)
			}
			w := &World{Now: in.Now, Census: in.Census, Observed: observeCensus(in.Obs, in.Census, in.Now, in.ObsMaxAge)}
			rows := in.Census.Canonical()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, row := range rows {
					decideRow(w, &d, row.Key)
				}
			}
		})
	}
}

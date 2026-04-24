package beadmail

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// BenchmarkArchiveMany measures the cost of an N-message batch close
// relative to N single-id Archive calls. Both paths run on a memstore so
// the bench isolates the bookkeeping cost of per-id Archive (Get + type
// check + Close) vs. one batch CloseAll. The real wall-clock win of
// ArchiveMany comes from skipping N-1 `bd` subprocess calls on [BdStore]
// — memstore only sees the in-process overhead, so the delta here is
// modest. This bench exists primarily as a regression guard; the 20-in-5s
// acceptance is measured against BdStore, not memstore.
func BenchmarkArchiveMany(b *testing.B) {
	for _, n := range []int{20, 200} {
		b.Run(benchName("ArchiveMany", n), func(b *testing.B) {
			runArchiveManyBench(b, n, true)
		})
		b.Run(benchName("SingleIdLoop", n), func(b *testing.B) {
			runArchiveManyBench(b, n, false)
		})
	}
}

func runArchiveManyBench(b *testing.B, n int, batch bool) {
	b.Helper()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		p, ids := benchSetup(b, n)
		b.StartTimer()
		if batch {
			if _, err := p.ArchiveMany(ids); err != nil {
				b.Fatalf("ArchiveMany: %v", err)
			}
			continue
		}
		for _, id := range ids {
			if err := p.Archive(id); err != nil {
				b.Fatalf("Archive %s: %v", id, err)
			}
		}
	}
}

func benchSetup(b *testing.B, n int) (*Provider, []string) {
	b.Helper()
	store := beads.NewMemStore()
	p := New(store)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		m, err := p.Send("alice", "bob", "", "bench")
		if err != nil {
			b.Fatalf("Send: %v", err)
		}
		ids = append(ids, m.ID)
	}
	return p, ids
}

func benchName(base string, n int) string {
	switch n {
	case 20:
		return base + "_N20"
	case 200:
		return base + "_N200"
	default:
		return base
	}
}

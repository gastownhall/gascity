package beadmail

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

// BenchmarkArchiveMany measures the cost of an N-message batch close
// relative to N single-id Archive calls. Both paths run on a memstore so
// the bench isolates the bookkeeping cost of per-id Archive (Get + type
// check + Close) vs. per-id Get + one batch CloseAll for the open subset.
// ArchiveMany pays a per-id Get to preserve [mail.ErrAlreadyArchived] and
// non-message reporting parity with single-id Archive; the wall-clock win
// on [BdStore] comes from collapsing N closes into one batched `bd close`
// subprocess. Memstore only sees the in-process overhead, so the delta
// here is modest. This bench exists primarily as a regression guard; the
// real acceptance target is measured against BdStore, not memstore.
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

// BenchmarkFilter measures Filter dispatch cost at inbox sizes that match the
// gas-ui ga-rdz acceptance shape (the /mail render budget is <250ms over a
// 100+-message fixture inbox). Memstore is the lower bound on backing-store
// cost; BdStore costs strictly more because of its bd-subprocess round-trips.
//
// Two shapes are exercised: a recipient-only filter (the inbox path) and a
// sender-only filter (the Sent tab). Both use the metadata-indexed query path
// from filterCandidates, not a broad type=message scan.
func BenchmarkFilter(b *testing.B) {
	for _, n := range []int{100, 500} {
		b.Run(benchName("FilterByRecipient", n), func(b *testing.B) {
			runFilterBench(b, n, mail.FilterOptions{Recipient: "bob", IncludeRead: true})
		})
		b.Run(benchName("FilterBySender", n), func(b *testing.B) {
			runFilterBench(b, n, mail.FilterOptions{Sender: "alice", IncludeRead: true})
		})
	}
}

func runFilterBench(b *testing.B, n int, opts mail.FilterOptions) {
	b.Helper()
	b.ReportAllocs()
	store := beads.NewMemStore()
	p := New(store)
	for i := 0; i < n; i++ {
		if _, err := p.Send("alice", "bob", "", "bench"); err != nil {
			b.Fatalf("Send: %v", err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msgs, err := p.Filter(opts)
		if err != nil {
			b.Fatalf("Filter: %v", err)
		}
		if len(msgs) == 0 {
			b.Fatalf("Filter returned 0 messages, want %d", n)
		}
	}
}

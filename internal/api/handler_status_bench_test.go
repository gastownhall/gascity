package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// benchCounterStore answers work counts from precomputed totals, modeling a
// hydration-free Counter (in production, the caching layer's in-memory
// count): the per-status answer is produced without rows crossing the
// store boundary.
type benchCounterStore struct {
	beads.Store
	counts map[string]int
}

func (s *benchCounterStore) Count(_ context.Context, query beads.ListQuery, _ ...string) (int, error) {
	return s.counts[query.Status], nil
}

// benchmarkStatusState seeds nStores rig stores with nBeads work beads each.
// With useCounter the stores expose beads.Counter; otherwise the status
// handler hydrates every bead through the legacy List path.
func benchmarkStatusState(b *testing.B, nStores, nBeads int, useCounter bool) *fakeState {
	b.Helper()
	state := newFakeState(b)
	for i := 0; i < nStores; i++ {
		mem := beads.NewMemStore()
		counts := map[string]int{}
		for j := 0; j < nBeads; j++ {
			status := []string{"open", "in_progress", "ready"}[j%3]
			created, err := mem.Create(beads.Bead{Type: "task", Title: fmt.Sprintf("bead-%d", j)})
			if err != nil {
				b.Fatalf("Create: %v", err)
			}
			if status != "open" {
				if err := mem.Update(created.ID, beads.UpdateOpts{Status: &status}); err != nil {
					b.Fatalf("Update: %v", err)
				}
			}
			counts[status]++
		}
		var store beads.Store = mem
		if useCounter {
			store = &benchCounterStore{Store: mem, counts: counts}
		}
		state.stores[fmt.Sprintf("rig-%d", i)] = store
	}
	return state
}

func benchmarkBuildStatusBody(b *testing.B, useCounter bool) {
	const nStores, nBeads = 8, 2000
	state := benchmarkStatusState(b, nStores, nBeads, useCounter)
	s := &Server{state: state, componentVersionsProbe: func() componentVersions { return componentVersions{} }}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body := s.buildStatusBody(ctx, false)
		if body.Work.Open == 0 {
			b.Fatal("Work.Open = 0, want seeded work")
		}
	}
}

// BenchmarkBuildStatusBodyListPath measures the legacy path: every status
// build hydrates every bead in every rig store before bucketing.
func BenchmarkBuildStatusBodyListPath(b *testing.B) {
	benchmarkBuildStatusBody(b, false)
}

// BenchmarkBuildStatusBodyCounterPath measures the #1896 fix: per-store
// Counter answers with zero row hydration.
func BenchmarkBuildStatusBodyCounterPath(b *testing.B) {
	benchmarkBuildStatusBody(b, true)
}

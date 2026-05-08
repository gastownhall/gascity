package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// seedSessionBeads populates a Store with the given number of open and
// closed session beads. Open beads carry a fresh session_name and template
// so newSessionBeadSnapshot's identity indexes get exercised the same way
// as in production.
func seedSessionBeads(tb testing.TB, store beads.Store, openCount, closedCount int) {
	tb.Helper()
	for i := 0; i < openCount; i++ {
		bead, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("open session %d", i),
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name": fmt.Sprintf("agent-open-%d", i),
				"template":     fmt.Sprintf("template-open-%d", i),
			},
		})
		if err != nil {
			tb.Fatalf("seed open session bead %d: %v", i, err)
		}
		_ = bead
	}
	for i := 0; i < closedCount; i++ {
		bead, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("closed session %d", i),
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name": fmt.Sprintf("agent-closed-%d", i),
				"template":     fmt.Sprintf("template-closed-%d", i),
			},
		})
		if err != nil {
			tb.Fatalf("seed closed session bead %d: %v", i, err)
		}
		if err := store.Close(bead.ID); err != nil {
			tb.Fatalf("close session bead %d: %v", i, err)
		}
	}
}

// BenchmarkLoadSessionBeadSnapshot_LargeStore exercises the hot-path
// snapshot loader against a store dominated by closed session beads. After
// the IncludeClosed drop in loadSessionBeadSnapshot, runtime should scale
// with the open count, not the open+closed total.
func BenchmarkLoadSessionBeadSnapshot_LargeStore(b *testing.B) {
	store := beads.NewMemStore()
	seedSessionBeads(b, store, 50, 5000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap, err := loadSessionBeadSnapshot(store)
		if err != nil {
			b.Fatal(err)
		}
		if got := len(snap.Open()); got != 50 {
			b.Fatalf("Open()=%d, want 50", got)
		}
	}
}

// BenchmarkLoadSessionBeadSnapshot_OpenOnlyBaseline establishes a control
// for BenchmarkLoadSessionBeadSnapshot_LargeStore: same open count, no
// closed history. The two benchmarks should report comparable ns/op.
func BenchmarkLoadSessionBeadSnapshot_OpenOnlyBaseline(b *testing.B) {
	store := beads.NewMemStore()
	seedSessionBeads(b, store, 50, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap, err := loadSessionBeadSnapshot(store)
		if err != nil {
			b.Fatal(err)
		}
		if got := len(snap.Open()); got != 50 {
			b.Fatalf("Open()=%d, want 50", got)
		}
	}
}

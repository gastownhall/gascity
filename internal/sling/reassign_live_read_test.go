package sling

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestReopenForReassignReadsTheLiveRow: --reassign on a bead the cache
// still serves as open, unassigned and unparked, while the backing holds
// the park the pool wrote since: the reopen reads the row LIVE, so the
// clean cached row does not return before any write, and the park is
// lifted.
func TestReopenForReassignReadsTheLiveRow(t *testing.T) {
	mem := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(mem, nil)
	b, err := cache.Create(beads.Bead{Title: "work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mem.SetMetadataBatch(b.ID, map[string]string{beadmeta.ParkedAtMetadataKey: "2026-09-12T02:02:00Z", beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "1", beadmeta.ParkIDMetadataKey: "cafe"}); err != nil {
		t.Fatal(err)
	}
	if cached, _ := cache.Get(b.ID); cached.Metadata[beadmeta.ParkedAtMetadataKey] != "" {
		t.Fatal("fixture: the cache still serves the clean row")
	}
	changed, err := reopenForReassign(b.ID, SlingDeps{Store: cache})
	if err != nil {
		t.Fatalf("reopenForReassign: %v", err)
	}
	if changed == "" {
		t.Fatal("the live row is parked: the reopen must lift the park, not return early on the cached clean row")
	}
	row, _ := mem.Get(b.ID)
	if row.Metadata[beadmeta.ParkedAtMetadataKey] != "" || row.Metadata[beadmeta.ParkIDMetadataKey] != "" {
		t.Fatalf("the park survived: %v", row.Metadata)
	}
}

package sling

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// unfencedStore hides the MemStore's conditional writer: the plain path.
type unfencedStore struct{ beads.Store }

// TestReopenForReassignClearsAParkWrittenAfterTheRead: --reassign read a
// bead; the pool's next failure parked it before the write. Fenced (a
// caching store stamped auto — the production seam resolves a writer), the
// stale write is refused, the row re-read live and the park cleared
// whatever the read showed. Unfenced (mode unset), a read that showed any
// of the record clears the whole family, so the park is cleared too; a
// read that showed no record at all is the store's own read→write window
// and the park survives — said here as the documented residual.
func TestReopenForReassignClearsAParkWrittenAfterTheRead(t *testing.T) {
	four := map[string]string{beadmeta.RoutedToMetadataKey: "worker", beadmeta.StartFailuresMetadataKey: "4", beadmeta.StartFailedAtMetadataKey: "2026-09-12T02:00:00Z", beadmeta.StartFailureMetadataKey: "boom", beadmeta.StartBackoffUntilMetadataKey: "2026-09-12T02:01:20Z"}
	none := map[string]string{beadmeta.RoutedToMetadataKey: "worker"}
	park := map[string]string{beadmeta.StartFailuresMetadataKey: "", beadmeta.ParkedAtMetadataKey: "2026-09-12T02:02:00Z", beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkIDMetadataKey: "cafe"}
	for _, tc := range []struct {
		name     string
		fenced   bool
		read     map[string]string
		survives bool
	}{
		{"fenced, record read", true, four, false},
		{"fenced, no record read", true, none, false},
		{"unfenced, record read", false, four, false},
		{"unfenced, no record read", false, none, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			var store beads.Store
			if tc.fenced {
				cache := beads.NewCachingStoreForTest(mem, nil)
				if !beads.StampConditionalWritesModeForTest(cache, gate.Auto) {
					t.Fatal("fixture: the stamp was refused")
				}
				store = cache
			} else {
				store = unfencedStore{mem}
			}
			b, err := store.Create(beads.Bead{Title: "work", Type: "task", Status: "in_progress", Assignee: "worker", Metadata: tc.read})
			if err != nil {
				t.Fatal(err)
			}
			if cache, ok := store.(*beads.CachingStore); ok {
				if err := cache.Prime(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if writer, _, _ := beads.ResolveConditionalWriter(store); (writer != nil) != tc.fenced {
				t.Fatalf("fixture: fenced=%v", writer != nil)
			}
			stale, err := store.Get(b.ID) // --reassign's read
			if err != nil {
				t.Fatal(err)
			}
			if err := mem.SetMetadataBatch(b.ID, park); err != nil { // the park lands on the backing
				t.Fatal(err)
			}
			changed, err := reopenForReassignInStore(store, b.ID, stale)
			if err != nil {
				t.Fatalf("reopenForReassignInStore: %v", err)
			}
			if changed == "" {
				t.Fatal("something changed")
			}
			row, _ := mem.Get(b.ID)
			parked := row.Metadata[beadmeta.ParkedAtMetadataKey] != ""
			if parked != tc.survives {
				t.Fatalf("park survives = %v, want %v: %v", parked, tc.survives, row.Metadata)
			}
			if !tc.survives {
				for _, key := range beadmeta.WorkStartFailureMetadataKeys {
					if row.Metadata[key] != "" {
						t.Fatalf("%s survived the reassign: %v", key, row.Metadata)
					}
				}
			}
			if row.Assignee != "" || row.Status != "open" {
				t.Fatalf("reopened: assignee=%q status=%q", row.Assignee, row.Status)
			}
		})
	}
	// Nothing to clear: nothing written (no fence, no update).
	mem := beads.NewMemStore()
	b, _ := mem.Create(beads.Bead{Title: "clean", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	before, _ := mem.Get(b.ID)
	if changed, err := reopenForReassignInStore(mem, b.ID, before); err != nil || changed != "" {
		t.Fatalf("an open, unassigned, unparked bead is left alone: %q err=%v", changed, err)
	}
	if after, _ := mem.Get(b.ID); after.Revision != before.Revision {
		t.Fatal("no write")
	}
}

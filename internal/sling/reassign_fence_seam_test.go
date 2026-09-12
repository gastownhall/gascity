package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// resolveTargetStore is the CLI's policy wrapper shape: it exposes no
// ConditionalWriter itself, only a declared resolution target.
type resolveTargetStore struct {
	beads.Store
	target beads.Store
}

func (s resolveTargetStore) ConditionalWritesResolveTarget() beads.Store { return s.target }

// TestReopenForReassignFencesThroughTheResolveTarget: --reassign read a
// bead with NO record; the pool's failure parked it before the write
// (the only race the whole-family clear cannot cover). Through the
// production wrapper — a resolution target, not a writer — the write is
// fenced on the row read, the moved row is re-read LIVE at the backing
// (the cache still serves the old row) and the park is cleared. Under
// beads.conditional_writes = "require" a store that cannot fence refuses
// the reopen rather than write unfenced.
func TestReopenForReassignFencesThroughTheResolveTarget(t *testing.T) {
	mem := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(mem, nil)
	if !beads.StampConditionalWritesModeForTest(cache, gate.Auto) {
		t.Fatal("fixture: the stamp was refused")
	}
	store := resolveTargetStore{Store: cache, target: cache}
	b, err := cache.Create(beads.Bead{Title: "work", Type: "task", Status: "in_progress", Assignee: "worker", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := store.Get(b.ID) // --reassign's read: no record at all
	if err != nil {
		t.Fatal(err)
	}
	if _, direct := beads.ConditionalWriterFor(store); direct {
		t.Fatal("fixture: the wrapper exposes no writer of its own")
	}
	// The first failed start parks the bead (max_start_failures=1) on the
	// backing, behind the cache.
	if err := mem.SetMetadataBatch(b.ID, map[string]string{beadmeta.StartFailuresMetadataKey: "1", beadmeta.ParkedAtMetadataKey: "2026-09-12T02:02:00Z", beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "1"}); err != nil {
		t.Fatal(err)
	}
	if cached, _ := store.Get(b.ID); cached.Metadata[beadmeta.ParkedAtMetadataKey] != "" {
		t.Fatal("fixture: the cache still serves the unparked row")
	}
	changed, err := reopenForReassignInStore(store, b.ID, stale)
	if err != nil {
		t.Fatalf("reopenForReassignInStore: %v", err)
	}
	if changed == "" {
		t.Fatal("something changed")
	}
	row, _ := mem.Get(b.ID)
	for _, key := range beadmeta.WorkStartFailureMetadataKeys {
		if row.Metadata[key] != "" {
			t.Fatalf("%s survived the fenced reassign: %v", key, row.Metadata)
		}
	}
	if row.Assignee != "" || row.Status != "open" {
		t.Fatalf("reopened: assignee=%q status=%q", row.Assignee, row.Status)
	}
	// require over a backing that reports itself incapable of fencing at
	// runtime: refused at resolve, nothing written (the write-time
	// unsupported arm is the same shape as writeWorkRecord's, pinned there).
	plain := beads.NewMemStore()
	plain.DisableConditionalWrites = true
	strict := beads.NewCachingStoreForTest(plain, nil)
	if !beads.StampConditionalWritesModeForTest(strict, gate.Require) {
		t.Fatal("fixture: the stamp was refused")
	}
	parked, err := strict.Create(beads.Bead{Title: "parked", Type: "task", Status: "in_progress", Assignee: "worker", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker", beadmeta.ParkedAtMetadataKey: "2026-09-12T02:02:00Z"}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := strict.Get(parked.ID)
	if _, err := reopenForReassignInStore(resolveTargetStore{Store: strict, target: strict}, parked.ID, before); err == nil {
		t.Fatal("require: a store that cannot fence must refuse the reopen")
	}
	if after, _ := plain.Get(parked.ID); after.Assignee != "worker" || after.Metadata[beadmeta.ParkedAtMetadataKey] == "" {
		t.Fatalf("require: nothing written unfenced: %+v", after)
	}
}

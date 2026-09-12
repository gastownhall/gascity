//go:build integration

package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreSetMetadataBatchKeepsAConcurrentUpdateAgainstRealDolt is
// the real-store proof of the compare-and-swap: a second writer changes other
// keys of the same bead between the merge's read and its checked write, on
// the pinned backend with its own row version and audit events. The refused
// swap is retried from a fresh read, every key of both writers survives, and
// exactly one event is recorded for the stamp — the refused attempt leaves
// none. The in-memory test owns the interleaving detail; this pins that the
// backend's version check is the one the store compares against.
//
// The proof never skips: a host that cannot open the pinned backend fails
// the test, so a lane cannot go green with the proof unexecuted.
func TestNativeDoltStoreSetMetadataBatchKeepsAConcurrentUpdateAgainstRealDolt(t *testing.T) {
	ctx := context.Background()
	store := openRealNativeDoltStoreForMergeProof(t, "merge-race")

	created, err := store.Create(Bead{
		Title:    "fenced entry step",
		Metadata: map[string]string{"gc.run_target": "pool", "gc.instantiating": "true", "gc.deferred_routed_to": "rig/pool"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := created.ID

	countEvents := func() int {
		t.Helper()
		storage, release, err := store.acquireStorage()
		if err != nil {
			t.Fatalf("acquire storage: %v", err)
		}
		defer release()
		events, err := storage.GetEvents(ctx, id, 100)
		if err != nil {
			t.Fatalf("GetEvents: %v", err)
		}
		return len(events)
	}

	reads := 0
	afterCompetingWrite := -1
	store.afterMetadataMergeRead = func(readID string) {
		reads++
		if reads != 1 || readID != id {
			return
		}
		// The competing writer: the fence activation, on other keys of the same
		// row, committing after the merge's read and before its checked write.
		if err := store.Update(id, UpdateOpts{Metadata: map[string]string{
			"gc.instantiating":      "",
			"gc.deferred_routed_to": "",
			"gc.routed_to":          "rig/pool",
		}}); err != nil {
			t.Errorf("competing Update: %v", err)
		}
		afterCompetingWrite = countEvents()
	}

	if err := store.SetMetadataBatch(id, map[string]string{"gc.heartbeat": "now"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if reads != 2 {
		t.Fatalf("merge reads = %d, want 2 (the swap refused after the competing write, then a fresh read)", reads)
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for key, want := range map[string]string{
		"gc.heartbeat":          "now",
		"gc.instantiating":      "",
		"gc.deferred_routed_to": "",
		"gc.routed_to":          "rig/pool",
		"gc.run_target":         "pool",
	} {
		if got.Metadata[key] != want {
			t.Errorf("%s = %q, want %q (the competing write was lost or the merge was)", key, got.Metadata[key], want)
		}
	}

	if afterCompetingWrite < 0 {
		t.Fatal("the competing write never ran")
	}
	if delta := countEvents() - afterCompetingWrite; delta != 1 {
		t.Fatalf("events recorded by the merge = %d, want 1: the committed stamp records one event and the refused swap none", delta)
	}
}

// TestNativeDoltStoreSetMetadataBatchRollsBackWhenTheEventInsertFails pins
// the other half of the write's atomicity on the pinned server backend: the
// audit event is inserted in the same transaction as the metadata update, so
// a failing event insert leaves the row as it was — metadata, row version and
// event count — and the caller sees the error. The failure is injected by
// taking the events table away for the one write; the library writes event
// ids explicitly, so the column's missing default is not a lever here.
func TestNativeDoltStoreSetMetadataBatchRollsBackWhenTheEventInsertFails(t *testing.T) {
	ctx := context.Background()
	scopeRoot := t.TempDir()
	port := startTestDoltServer(t)
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("create .beads directory: %v", err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	storage, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Fatalf("open the server-backed native storage the proof runs against: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Errorf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	accessor, ok := storage.(testRawDBGetter)
	if !ok {
		t.Fatal("server-backed storage does not expose a raw DB; the events table cannot be taken away")
	}
	db := accessor.DB()
	store, err := newNativeDoltStoreAt(ctx, scopeRoot, nil)
	if err != nil {
		t.Fatalf("newNativeDoltStoreAt: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	created, err := store.Create(Bead{Title: "rollback probe", Metadata: map[string]string{"gc.a": "1"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, err := storage.GetIssue(ctx, created.ID)
	if err != nil || before == nil {
		t.Fatalf("GetIssue before: (%v, %v)", before, err)
	}
	eventsBefore, err := storage.GetEvents(ctx, created.ID, 100)
	if err != nil {
		t.Fatalf("GetEvents before: %v", err)
	}

	if _, err := db.Exec("RENAME TABLE `events` TO `events_offline`"); err != nil {
		t.Fatalf("take the events table away: %v", err)
	}
	restored := false
	restoreEvents := func() {
		if restored {
			return
		}
		if _, err := db.Exec("RENAME TABLE `events_offline` TO `events`"); err != nil {
			t.Errorf("restore the events table: %v", err)
			return
		}
		restored = true
	}
	t.Cleanup(restoreEvents)
	writeErr := store.SetMetadataBatch(created.ID, map[string]string{"gc.b": "2"})
	restoreEvents()
	if writeErr == nil {
		t.Fatal("SetMetadataBatch succeeded with the events table gone; the event insert does not share the write's transaction")
	}
	if msg := strings.ToLower(writeErr.Error()); !strings.Contains(msg, "table not found") || !strings.Contains(msg, "events") {
		t.Fatalf("SetMetadataBatch error = %v, want the missing events table diagnosed: the library names the events table on every event-insert failure, so only the table-not-found text proves the injected fault was the one that failed the write", writeErr)
	}

	after, err := storage.GetIssue(ctx, created.ID)
	if err != nil || after == nil {
		t.Fatalf("GetIssue after: (%v, %v)", after, err)
	}
	if after.RowVersion != before.RowVersion {
		t.Fatalf("row version moved %d -> %d across a failed write: the metadata update was not rolled back with the event insert", before.RowVersion, after.RowVersion)
	}
	if string(after.Metadata) != string(before.Metadata) {
		t.Fatalf("metadata changed across a failed write: before %s, after %s", before.Metadata, after.Metadata)
	}
	eventsAfter, err := storage.GetEvents(ctx, created.ID, 100)
	if err != nil {
		t.Fatalf("GetEvents after: %v", err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("events = %d after a failed write, want %d (none recorded for a write that did not commit)", len(eventsAfter), len(eventsBefore))
	}
}

// openRealNativeDoltStoreForMergeProof opens the pinned native backend on a
// fresh directory and fails — never skips — when it cannot.
func openRealNativeDoltStoreForMergeProof(t *testing.T, actor string) *NativeDoltStore {
	t.Helper()
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Fatalf("open the pinned native beads storage the proof runs against: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Errorf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	return newNativeDoltStoreWithStorageAndPrefix(storage, actor, "gc")
}

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

// TestNativeDoltStoreSetMetadataBatchKeepsAnotherSessionsUpdate is the
// real-store proof that a metadata stamp keeps a competing writer's keys. Two
// handles on one sql-server are two sessions, the way two processes share a
// ledger. A stamps a key and reads the row. B then commits the fence
// activation: it clears two keys and sets a third. A stamps again without
// reading. A stamp merged against A's last view would write B's cleared keys
// back and drop B's new one. The facade merges against the row its own
// transaction reads, so every key of both writers survives, seen from either
// session. It runs against the issues table and the wisps table, whose writes
// are separate backend paths.
//
// The ordering is forced between transactions, not inside one: no hook reaches
// inside a facade transaction. A commit landing inside the stamp's own
// transaction is Dolt's to merge or refuse, and the store replays the refusal;
// TestNativeDoltStoreMetadataMergeSurvivesAConcurrentUpdateLoop exercises that
// window by racing the two writers.
//
// The proof never skips: a host that cannot start the pinned server fails the
// test, so a lane cannot go green with the proof unexecuted.
func TestNativeDoltStoreSetMetadataBatchKeepsAnotherSessionsUpdate(t *testing.T) {
	for tableName, ephemeral := range map[string]bool{"issues": false, "wisps": true} {
		t.Run(tableName, func(t *testing.T) {
			scopeRoot := initServerScopeForMergeProof(t)
			a := openNativeDoltStoreHandleForMergeProof(t, scopeRoot)
			b := openNativeDoltStoreHandleForMergeProof(t, scopeRoot)

			created, err := a.Create(Bead{
				Title:     "fenced entry step",
				Ephemeral: ephemeral,
				Metadata:  map[string]string{"gc.run_target": "pool", "gc.instantiating": "true", "gc.deferred_routed_to": "rig/pool"},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if created.Ephemeral != ephemeral {
				t.Fatalf("Ephemeral = %v, want %v: the variant does not exercise the table it names", created.Ephemeral, ephemeral)
			}
			id := created.ID

			if err := a.SetMetadataBatch(id, map[string]string{"gc.attempt": "1"}); err != nil {
				t.Fatalf("A's first stamp: %v", err)
			}
			view, err := a.Get(id)
			if err != nil {
				t.Fatalf("A's read: %v", err)
			}
			if view.Metadata["gc.instantiating"] != "true" || view.Metadata["gc.routed_to"] != "" {
				t.Fatalf("A's view = %#v, want the row as it stood before B's write", view.Metadata)
			}

			if err := b.Update(id, UpdateOpts{Metadata: map[string]string{
				"gc.instantiating":      "",
				"gc.deferred_routed_to": "",
				"gc.routed_to":          "rig/pool",
			}}); err != nil {
				t.Fatalf("B's competing Update: %v", err)
			}
			if err := a.SetMetadataBatch(id, map[string]string{"gc.heartbeat": "now"}); err != nil {
				t.Fatalf("A's second stamp: %v", err)
			}

			want := map[string]string{
				"gc.run_target":         "pool",
				"gc.attempt":            "1",
				"gc.instantiating":      "",
				"gc.deferred_routed_to": "",
				"gc.routed_to":          "rig/pool",
				"gc.heartbeat":          "now",
			}
			for session, store := range map[string]*NativeDoltStore{"A": a, "B": b} {
				got, err := store.Get(id)
				if err != nil {
					t.Fatalf("session %s Get: %v", session, err)
				}
				for key, value := range want {
					if got.Metadata[key] != value {
						t.Errorf("session %s: %s = %q, want %q: a stale merge lost a write (full: %#v)", session, key, got.Metadata[key], value, got.Metadata)
					}
				}
			}
		})
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

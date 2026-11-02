//go:build integration

package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreSameValueWriteCommitsNothing pins that the native store
// decides a no-op write itself, against the row its own write transaction
// read (the facade's DiscardNoopIssueUpdates): a write whose target the row
// already holds leaves the revision and updated_at as they were, makes no
// Dolt commit and inserts no events row. The control write changes the row
// and moves all three, so the counters are live.
func TestNativeDoltStoreSameValueWriteCommitsNothing(t *testing.T) {
	ctx := context.Background()
	port := startTestDoltServer(t)
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("create .beads directory: %v", err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	storage, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
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
		t.Fatal("server-backed storage does not expose a raw DB; commits and events cannot be counted")
	}
	db := accessor.DB()
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "noop-test", "gc")

	type mark struct {
		revision         int64
		updated          string
		commits, journal int
	}
	read := func(t *testing.T, id string) mark {
		t.Helper()
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		m := mark{revision: b.Revision, updated: b.UpdatedAt.String()}
		if err := db.QueryRow("SELECT COUNT(*) FROM dolt_log").Scan(&m.commits); err != nil {
			t.Fatalf("count dolt_log: %v", err)
		}
		if err := db.QueryRow("SELECT COUNT(*) FROM events").Scan(&m.journal); err != nil {
			t.Fatalf("count events: %v", err)
		}
		return m
	}
	strPtr := func(s string) *string { return &s }

	b, err := store.Create(Bead{Title: "pinned", Assignee: "alice", Labels: []string{"a"}, Metadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, tc := range []struct {
		name  string
		write func() error
	}{
		{"SetMetadata", func() error { return store.SetMetadata(b.ID, "k", "v") }},
		{"SetMetadataBatch", func() error { return store.SetMetadataBatch(b.ID, map[string]string{"k": "v"}) }},
		{"Update/Title", func() error { return store.Update(b.ID, UpdateOpts{Title: strPtr("pinned")}) }},
		{"Update/Status+Assignee", func() error {
			return store.Update(b.ID, UpdateOpts{Status: strPtr("open"), Assignee: strPtr("alice")})
		}},
		{"Update/Labels", func() error {
			return store.Update(b.ID, UpdateOpts{Labels: []string{"a"}, RemoveLabels: []string{"z"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := read(t, b.ID)
			if err := tc.write(); err != nil {
				t.Fatalf("write: %v", err)
			}
			if after := read(t, b.ID); after != before {
				t.Fatalf("same-value write moved the store: %+v -> %+v", before, after)
			}
		})
	}
	t.Run("control", func(t *testing.T) {
		before := read(t, b.ID)
		if err := store.SetMetadata(b.ID, "k", "changed"); err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}
		after := read(t, b.ID)
		if after.revision == before.revision || after.commits == before.commits || after.journal == before.journal {
			t.Fatalf("a changing write did not commit: %+v -> %+v", before, after)
		}
	})
}

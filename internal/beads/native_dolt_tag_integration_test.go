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

// TestNativeDoltStoreDoltTagOnARealServer tags a store served by a throwaway
// dolt sql-server (startTestDoltServer: a free port, never 3307, 45865 or
// 8123, stopped by its own process handle). The tag must point at the HEAD it
// reported, survive later writes unchanged, and be readable back by name.
func TestNativeDoltStoreDoltTagOnARealServer(t *testing.T) {
	ctx := context.Background()
	scopeRoot := t.TempDir()
	port := startTestDoltServer(t)
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	storage, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatal(err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "dolt-tag", "gc")
	if _, err := store.Create(Bead{Title: "before the tag"}); err != nil {
		t.Fatal(err)
	}

	tagger, ok := As[DoltTagger](store)
	if !ok {
		t.Fatal("NativeDoltStore does not answer As[DoltTagger]")
	}
	got, err := tagger.DoltTag("gc-storage-snapshot-test", "snapshot test")
	if err != nil {
		t.Fatalf("DoltTag: %v", err)
	}
	if got.Commit == "" {
		t.Fatal("DoltTag reported no commit")
	}
	if _, err := store.Create(Bead{Title: "after the tag"}); err != nil {
		t.Fatal(err)
	}
	commit, found, err := store.DoltTagCommit("gc-storage-snapshot-test")
	if err != nil || !found {
		t.Fatalf("DoltTagCommit = (%q, %v, %v), want the tag", commit, found, err)
	}
	if commit != got.Commit {
		t.Fatalf("the tag points at %s, want the HEAD it reported, %s", commit, got.Commit)
	}
	if _, found, err := store.DoltTagCommit("gc-storage-snapshot-missing"); err != nil || found {
		t.Fatalf("DoltTagCommit of a missing tag = (%v, %v), want not found", found, err)
	}
}

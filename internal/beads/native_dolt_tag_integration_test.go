//go:build integration

package beads

import (
	"context"
	"database/sql"
	"errors"
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

	tagger, ok := NativeDoltTagger(WorkStore{Store: store})
	if !ok {
		t.Fatal("NativeDoltTagger does not see the native store under a class handle")
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
	accessor, ok := storage.(testRawDBGetter)
	if !ok {
		t.Skip("storage does not expose a raw DB")
	}
	tagCommit := func(name string) (string, bool) {
		var commit string
		err := accessor.DB().QueryRow("SELECT tag_hash FROM dolt_tags WHERE tag_name = ?", name).Scan(&commit)
		if errors.Is(err, sql.ErrNoRows) {
			return "", false
		}
		if err != nil {
			t.Fatalf("reading tag %s: %v", name, err)
		}
		return commit, true
	}
	commit, found := tagCommit("gc-storage-snapshot-test")
	if !found || commit != got.Commit {
		t.Fatalf("the tag points at %q (found %v), want the HEAD it reported, %s", commit, found, got.Commit)
	}

	// A read-only (proxied) handle refuses the tag, a write, before it
	// reaches storage.
	store.readOnlyReason = "test"
	if _, err := tagger.DoltTag("gc-storage-snapshot-readonly", "x"); !errors.Is(err, ErrProxiedNativeReadOnly) {
		t.Fatalf("DoltTag on a read-only handle = %v, want ErrProxiedNativeReadOnly", err)
	}
	if _, found := tagCommit("gc-storage-snapshot-readonly"); found {
		t.Fatal("a read-only handle took the tag")
	}
}

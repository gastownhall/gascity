package beads_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestFileStore(t *testing.T) {
	factory := func() beads.Store {
		path := filepath.Join(t.TempDir(), "beads.json")
		s, err := beads.OpenFileStore(fsys.OSFS{}, path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	beadstest.RunStoreTests(t, factory)
	beadstest.RunSequentialIDTests(t, factory)
	beadstest.RunCreationOrderTests(t, factory)
	beadstest.RunDepTests(t, factory)
	beadstest.RunMetadataTests(t, factory)
}

func TestFileStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	// First process: create two beads.
	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := s1.Create(beads.Bead{Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s1.Create(beads.Bead{Title: "second"})
	if err != nil {
		t.Fatal(err)
	}

	// Second process: open a new FileStore on the same path.
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	// Verify Get works for both beads.
	got1, err := s2.Get(b1.ID)
	if err != nil {
		t.Fatalf("Get(%q) after reopen: %v", b1.ID, err)
	}
	if got1.Title != "first" {
		t.Errorf("Title = %q, want %q", got1.Title, "first")
	}

	got2, err := s2.Get(b2.ID)
	if err != nil {
		t.Fatalf("Get(%q) after reopen: %v", b2.ID, err)
	}
	if got2.Title != "second" {
		t.Errorf("Title = %q, want %q", got2.Title, "second")
	}

	// Verify next Create continues the sequence.
	b3, err := s2.Create(beads.Bead{Title: "third"})
	if err != nil {
		t.Fatal(err)
	}
	if b3.ID != "gc-3" {
		t.Errorf("third bead ID = %q, want %q", b3.ID, "gc-3")
	}
}

func TestFileStoreDepPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	// First process: create deps.
	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.DepAdd("a", "b", "blocks"); err != nil {
		t.Fatal(err)
	}

	// Second process: reopen and verify deps survived.
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	deps, err := s2.DepList("a", "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("DepList after reopen = %d deps, want 1", len(deps))
	}
	if deps[0].DependsOnID != "b" {
		t.Errorf("dep.DependsOnID = %q, want %q", deps[0].DependsOnID, "b")
	}
}

func TestFileStoreMetadataPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	// First process: create bead with metadata.
	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s1.Create(beads.Bead{Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.SetMetadata(b.ID, "convoy.owner", "mayor"); err != nil {
		t.Fatal(err)
	}

	// Second process: verify metadata survived.
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["convoy.owner"] != "mayor" {
		t.Errorf("Metadata[convoy.owner] = %q, want %q", got.Metadata["convoy.owner"], "mayor")
	}
}

func TestFileStoreOpenEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subdir", "beads.json")

	// Opening a non-existent file should succeed (creates parent dirs).
	s, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	// First bead should be gc-1.
	b, err := s.Create(beads.Bead{Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "gc-1" {
		t.Errorf("ID = %q, want %q", b.ID, "gc-1")
	}
}

func TestFileStoreOpenCorruptedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	if err := os.WriteFile(path, []byte("{not json!!!"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err == nil {
		t.Fatal("expected error for corrupted JSON")
	}
	if !strings.Contains(err.Error(), "opening file store") {
		t.Errorf("error = %q, want 'opening file store' prefix", err)
	}
}

func TestFileStoreOpenUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0 does not prevent reading on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root can read any file")
	}

	path := filepath.Join(t.TempDir(), "beads.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) }) //nolint:errcheck // best-effort cleanup

	_, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err == nil {
		t.Fatal("expected error for unreadable file")
	}
	if !strings.Contains(err.Error(), "opening file store") {
		t.Errorf("error = %q, want 'opening file store' prefix", err)
	}
}

// --- failure-path tests with fsys.Fake ---

func TestFileStoreOpenMkdirFails(t *testing.T) {
	f := fsys.NewFake()
	f.Errors["/city/.gc"] = fmt.Errorf("permission denied")

	_, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err == nil {
		t.Fatal("expected error when MkdirAll fails")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %q, want 'permission denied'", err)
	}
}

func TestFileStoreOpenReadFileFails(t *testing.T) {
	f := fsys.NewFake()
	f.Errors["/city/.gc/beads.json"] = fmt.Errorf("disk error")

	_, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err == nil {
		t.Fatal("expected error when ReadFile fails")
	}
	if !strings.Contains(err.Error(), "disk error") {
		t.Errorf("error = %q, want 'disk error'", err)
	}
}

func TestFileStoreOpenCorruptedJSONFake(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/.gc/beads.json"] = []byte("{not json!!!")

	_, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err == nil {
		t.Fatal("expected error for corrupted JSON")
	}
	if !strings.Contains(err.Error(), "opening file store") {
		t.Errorf("error = %q, want 'opening file store' prefix", err)
	}
}

func TestFileStoreSaveWriteFails(t *testing.T) {
	f := fsys.NewFake()
	s, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err != nil {
		t.Fatal(err)
	}

	// Inject error on the temp file write.
	f.Errors["/city/.gc/beads.json.tmp"] = fmt.Errorf("disk full")

	_, err = s.Create(beads.Bead{Title: "test"})
	if err == nil {
		t.Fatal("expected error when WriteFile fails")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error = %q, want 'disk full'", err)
	}
}

func TestFileStoreSaveRenameFails(t *testing.T) {
	f := fsys.NewFake()
	s, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err != nil {
		t.Fatal(err)
	}

	// Inject error on the rename (atomic commit step).
	f.Errors["/city/.gc/beads.json.tmp"] = fmt.Errorf("rename failed")

	_, err = s.Create(beads.Bead{Title: "test"})
	if err == nil {
		t.Fatal("expected error when Rename fails")
	}
	if !strings.Contains(err.Error(), "rename failed") {
		t.Errorf("error = %q, want 'rename failed'", err)
	}
}

// TestFileStoreConcurrentCreateWithFlock verifies that two FileStore instances
// backed by flock on the same file produce unique IDs (no collisions).
func TestFileStoreConcurrentCreateWithFlock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock not available on Windows")
	}

	dir := t.TempDir()
	beadsPath := filepath.Join(dir, "beads.json")
	lockPath := beadsPath + ".lock"

	const perStore = 20

	// Open two stores on the same file, each with its own flock.
	open := func() *beads.FileStore {
		s, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
		if err != nil {
			t.Fatal(err)
		}
		s.SetLocker(beads.NewFileFlock(lockPath))
		return s
	}

	s1 := open()
	s2 := open()

	// Run creates concurrently from both stores.
	var wg sync.WaitGroup
	ids := make(chan string, perStore*2)

	createN := func(s *beads.FileStore, prefix string) {
		defer wg.Done()
		for i := 0; i < perStore; i++ {
			b, err := s.Create(beads.Bead{Title: fmt.Sprintf("%s-%d", prefix, i)})
			if err != nil {
				t.Errorf("Create failed: %v", err)
				return
			}
			ids <- b.ID
		}
	}

	wg.Add(2)
	go createN(s1, "s1")
	go createN(s2, "s2")
	wg.Wait()
	close(ids)

	// All IDs must be unique.
	seen := make(map[string]bool)
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate ID: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != perStore*2 {
		t.Errorf("got %d unique IDs, want %d", len(seen), perStore*2)
	}

	// Reopen and verify all beads survived.
	s3 := open()
	all, err := s3.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != perStore*2 {
		t.Errorf("after reopen: %d beads, want %d", len(all), perStore*2)
	}
}

func TestFileStoreCloseWriteFails(t *testing.T) {
	f := fsys.NewFake()
	s, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err != nil {
		t.Fatal(err)
	}

	// Create a bead successfully first.
	b, err := s.Create(beads.Bead{Title: "test"})
	if err != nil {
		t.Fatal(err)
	}

	// Now inject error on the next save (Close flushes).
	f.Errors["/city/.gc/beads.json.tmp"] = fmt.Errorf("disk full")

	err = s.Close(b.ID)
	if err == nil {
		t.Fatal("expected error when save fails during Close")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error = %q, want 'disk full'", err)
	}
}

// BUG: PR #215 -- this test fails because FileStore has no cross-process
// flock. Two FileStore instances opened on the same empty file get
// independent seq counters (both starting at 0). Each produces "gc-1" for
// its first bead, and the second writer silently overwrites the first.
func TestFileStoreConcurrentInstances_DuplicateIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	// Simulate two processes opening the same file before either writes.
	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	// Both stores start with seq=0 and will independently assign gc-1.
	b1, err := s1.Create(beads.Bead{Title: "from-process-1"})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s2.Create(beads.Bead{Title: "from-process-2"})
	if err != nil {
		t.Fatal(err)
	}

	// With a cross-process flock, the second store would reload the file
	// after the first write and assign gc-2. Without the flock, both get gc-1.
	if b1.ID == b2.ID {
		t.Errorf("two concurrent FileStore instances produced the same bead ID %q; cross-process flock is missing", b1.ID)
	}
}

package beads_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

func TestHQStoreConformance(t *testing.T) {
	factory := func() beads.Store {
		t.Helper()
		store, err := beads.OpenHQStore(t.TempDir())
		if err != nil {
			t.Fatalf("OpenHQStore: %v", err)
		}
		t.Cleanup(func() {
			if err := store.Shutdown(); err != nil {
				t.Errorf("Shutdown: %v", err)
			}
		})
		return store
	}

	beadstest.RunStoreTests(t, factory)
	beadstest.RunDepTests(t, factory)
	beadstest.RunCreationOrderTests(t, factory)
}

func TestHQStoreRecoversFlushedSnapshotAfterSIGKILL(t *testing.T) {
	if os.Getenv("HQSTORE_SIGKILL_HELPER") == "1" {
		hqStoreSIGKILLHelper(t)
		return
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestHQStoreRecoversFlushedSnapshotAfterSIGKILL")
	cmd.Env = append(os.Environ(),
		"HQSTORE_SIGKILL_HELPER=1",
		"HQSTORE_DIR="+dir,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}

	// The helper writes the id file only AFTER Snapshot() returns, so its
	// presence guarantees a durable snapshot exists on disk.
	idPath := filepath.Join(dir, "created-id")
	var id string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(idPath)
		if err == nil {
			id = string(data)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if id == "" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("helper did not write created bead id")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing helper: %v", err)
	}
	_ = cmd.Wait()

	recovered, err := beads.OpenHQStore(dir)
	if err != nil {
		t.Fatalf("reopen after kill: %v", err)
	}
	t.Cleanup(func() {
		if err := recovered.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	got, err := recovered.Get(id)
	if err != nil {
		t.Fatalf("Get(%q) after kill: %v", id, err)
	}
	if got.Title != "persist-before-sigkill" {
		t.Fatalf("recovered title = %q, want persist-before-sigkill", got.Title)
	}
}

func hqStoreSIGKILLHelper(t *testing.T) {
	t.Helper()
	dir := os.Getenv("HQSTORE_DIR")
	if dir == "" {
		t.Fatal("HQSTORE_DIR is required")
	}
	// Disable the periodic snapshotter so the only durable state is the one we
	// force via Snapshot() — this makes the test deterministic about what
	// survives the kill.
	store, err := beads.OpenHQStore(dir, beads.WithHQStoreSnapshotInterval(0))
	if err != nil {
		t.Fatalf("helper OpenHQStore: %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "persist-before-sigkill"})
	if err != nil {
		t.Fatalf("helper Create: %v", err)
	}
	if err := store.Snapshot(); err != nil {
		t.Fatalf("helper Snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "created-id"), []byte(created.ID), 0o644); err != nil {
		t.Fatalf("helper write id: %v", err)
	}
	select {}
}

func TestHQStoreSnapshotRoundTripAcrossShutdown(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.OpenHQStore(dir, beads.WithHQStoreSnapshotInterval(0))
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	first, err := store.Create(beads.Bead{Title: "snapshotted", Metadata: map[string]string{"phase": "one"}})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := store.Create(beads.Bead{Title: "wisp", Type: "message", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if err := store.DepAdd(first.ID, second.ID, "tracks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	// Shutdown flushes a final snapshot even with the periodic snapshotter off.
	if err := store.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	recovered, err := beads.OpenHQStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() {
		if err := recovered.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	got, err := recovered.Get(first.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	if got.Metadata["phase"] != "one" {
		t.Fatalf("metadata phase = %q, want one", got.Metadata["phase"])
	}
	// Ephemeral routing must survive the snapshot round-trip.
	wisp, err := recovered.Get(second.ID)
	if err != nil {
		t.Fatalf("Get second: %v", err)
	}
	if !wisp.Ephemeral {
		t.Fatalf("recovered wisp Ephemeral = false, want true")
	}
	deps, err := recovered.DepList(first.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != second.ID {
		t.Fatalf("deps = %+v, want dependency on %s", deps, second.ID)
	}
}

func TestHQStorePeriodicSnapshotFlushes(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.OpenHQStore(dir, beads.WithHQStoreSnapshotInterval(50*time.Millisecond))
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	created, err := store.Create(beads.Bead{Title: "periodic"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Wait for the background snapshotter to publish a snapshot file.
	snapPath := filepath.Join(dir, "snapshot.jsonl.gz")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(snapPath); statErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, statErr := os.Stat(snapPath); statErr != nil {
		t.Fatalf("periodic snapshot not published: %v", statErr)
	}
	if err := store.LastSnapshotErr(); err != nil {
		t.Fatalf("background snapshot error: %v", err)
	}

	// A fresh open (without shutting down the first) should see the periodic
	// snapshot — reflecting what would survive a crash after the flush.
	recovered, err := beads.OpenHQStore(dir, beads.WithHQStoreSnapshotInterval(0))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() {
		if err := recovered.Shutdown(); err != nil {
			t.Errorf("Shutdown recovered: %v", err)
		}
	})
	if _, err := recovered.Get(created.ID); err != nil {
		t.Fatalf("Get(%q) from periodic snapshot: %v", created.ID, err)
	}
}

func makeHQSnapshotDirReadOnly(t *testing.T, dir string) func() {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat snapshot dir: %v", err)
	}
	originalMode := info.Mode().Perm()
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if err := os.Chmod(dir, originalMode); err != nil {
			t.Fatalf("restore snapshot dir permissions: %v", err)
		}
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("make snapshot dir read-only: %v", err)
	}
	t.Cleanup(restore)
	return restore
}

func TestHQStorePurgeExpired(t *testing.T) {
	store, err := beads.OpenHQStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	expired, err := store.Create(beads.Bead{
		Title:     "expired",
		Type:      "order-tracking",
		Ephemeral: true,
		Metadata: map[string]string{
			"expires_at": time.Now().Add(-time.Second).Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatalf("Create expired: %v", err)
	}
	live, err := store.Create(beads.Bead{
		Title:     "live",
		Type:      "order-tracking",
		Ephemeral: true,
		Metadata: map[string]string{
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatalf("Create live: %v", err)
	}

	purged, err := store.PurgeExpired()
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeExpired purged %d, want 1", purged)
	}
	if _, err := store.Get(expired.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get expired error = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(live.ID); err != nil {
		t.Fatalf("Get live: %v", err)
	}
}

func TestHQStorePurgeExpiredRetainsOpenAndRecentClosedMainTier(t *testing.T) {
	store, err := beads.OpenHQStore(t.TempDir(),
		beads.WithHQStoreSnapshotInterval(0),
		beads.WithHQStoreClosedTaskRetention(24*time.Hour),
	)
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	old := time.Now().Add(-48 * time.Hour)
	expiredClosed, err := store.Create(beads.Bead{
		Title:     "old closed",
		Status:    "closed",
		CreatedAt: old,
	})
	if err != nil {
		t.Fatalf("Create expiredClosed: %v", err)
	}
	recentClosed, err := store.Create(beads.Bead{
		Title:  "recent closed",
		Status: "closed",
	})
	if err != nil {
		t.Fatalf("Create recentClosed: %v", err)
	}
	oldOpen, err := store.Create(beads.Bead{
		Title:     "old open",
		Status:    "open",
		CreatedAt: old,
	})
	if err != nil {
		t.Fatalf("Create oldOpen: %v", err)
	}

	purged, err := store.PurgeExpired()
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeExpired purged %d, want 1", purged)
	}
	if _, err := store.Get(expiredClosed.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get expiredClosed error = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(recentClosed.ID); err != nil {
		t.Fatalf("Get recentClosed: %v", err)
	}
	if _, err := store.Get(oldOpen.ID); err != nil {
		t.Fatalf("Get oldOpen: %v", err)
	}
}

func TestHQStorePurgeExpiredSkipsClosedMainTierWithOpenChildren(t *testing.T) {
	store, err := beads.OpenHQStore(t.TempDir(),
		beads.WithHQStoreSnapshotInterval(0),
		beads.WithHQStoreClosedTaskRetention(24*time.Hour),
	)
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	old := time.Now().Add(-48 * time.Hour)
	parent, err := store.Create(beads.Bead{
		Title:     "old closed parent",
		Status:    "closed",
		CreatedAt: old,
	})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "open child",
		ParentID: parent.ID,
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}

	purged, err := store.PurgeExpired()
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if purged != 0 {
		t.Fatalf("PurgeExpired purged %d, want 0", purged)
	}
	if _, err := store.Get(parent.ID); err != nil {
		t.Fatalf("Get parent: %v", err)
	}
	if _, err := store.Get(child.ID); err != nil {
		t.Fatalf("Get child: %v", err)
	}
}

func TestHQStorePurgeExpiredClosedTaskRetentionDisabled(t *testing.T) {
	store, err := beads.OpenHQStore(t.TempDir(),
		beads.WithHQStoreSnapshotInterval(0),
		beads.WithHQStoreClosedTaskRetention(0),
	)
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	closed, err := store.Create(beads.Bead{
		Title:     "old closed",
		Status:    "closed",
		CreatedAt: time.Now().Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Create closed: %v", err)
	}
	purged, err := store.PurgeExpired()
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if purged != 0 {
		t.Fatalf("PurgeExpired purged %d, want 0", purged)
	}
	if _, err := store.Get(closed.ID); err != nil {
		t.Fatalf("Get closed: %v", err)
	}
}

func TestHQStoreConcurrentCreateUpdate(t *testing.T) {
	// Run with the periodic snapshotter active so the race detector also
	// exercises concurrent ExportAll reads against the write path.
	store, err := beads.OpenHQStore(t.TempDir(), beads.WithHQStoreSnapshotInterval(20*time.Millisecond))
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, err := store.Create(beads.Bead{
				Title:    fmt.Sprintf("worker-%d", i),
				Assignee: "builder",
			})
			if err != nil {
				errs <- err
				return
			}
			status := "in_progress"
			if err := store.Update(created.ID, beads.UpdateOpts{Status: &status}); err != nil {
				errs <- err
				return
			}
			if err := store.SetMetadataBatch(created.ID, map[string]string{"worker": fmt.Sprint(i)}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent worker error: %v", err)
		}
	}

	got, err := store.List(beads.ListQuery{Assignee: "builder", Status: "in_progress", AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != workers {
		t.Fatalf("List returned %d workers, want %d", len(got), workers)
	}
	seen := make(map[string]bool, workers)
	for _, b := range got {
		if seen[b.ID] {
			t.Fatalf("duplicate ID %q", b.ID)
		}
		seen[b.ID] = true
	}
}

func TestHQStoreListReadyConcurrentWithWriters(t *testing.T) {
	store, err := beads.OpenHQStore(t.TempDir(), beads.WithHQStoreSnapshotInterval(0))
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	const seedCount = 1000
	seedIDs := make([]string, 0, seedCount)
	for i := range seedCount {
		created, err := store.Create(beads.Bead{
			Title:    fmt.Sprintf("seed-%d", i),
			Assignee: "builder",
			Metadata: map[string]string{"seed": "true"},
		})
		if err != nil {
			t.Fatalf("Create seed %d: %v", i, err)
		}
		seedIDs = append(seedIDs, created.ID)
	}

	var (
		listObserved  atomic.Bool
		readyObserved atomic.Bool
		metadataOps   atomic.Uint64
		updateOps     atomic.Uint64
		createOps     atomic.Uint64
		wg            sync.WaitGroup
	)
	start := make(chan struct{})
	stop := make(chan struct{})
	errs := make(chan error, 8)
	runWorker := func(name string, fn func() error) {
		t.Helper()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := fn(); err != nil {
					select {
					case errs <- fmt.Errorf("%s: %w", name, err):
					case <-stop:
					}
					return
				}
			}
		}()
	}

	runWorker("list reader", func() error {
		got, err := store.List(beads.ListQuery{
			AllowScan:     true,
			IncludeClosed: true,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			return err
		}
		if len(got) == 0 {
			return errors.New("List returned no beads")
		}
		listObserved.Store(true)
		return nil
	})
	runWorker("ready reader", func() error {
		got, err := store.Ready(beads.ReadyQuery{Assignee: "builder"})
		if err != nil {
			return err
		}
		if len(got) == 0 {
			return errors.New("Ready returned no beads")
		}
		readyObserved.Store(true)
		return nil
	})
	runWorker("metadata writer", func() error {
		n := metadataOps.Add(1)
		id := seedIDs[int(n-1)%len(seedIDs)]
		return store.SetMetadataBatch(id, map[string]string{"race.metadata": fmt.Sprint(n)})
	})
	runWorker("update writer", func() error {
		n := updateOps.Add(1)
		id := seedIDs[int(n-1)%len(seedIDs)]
		title := fmt.Sprintf("updated-%d", n)
		return store.Update(id, beads.UpdateOpts{Title: &title})
	})
	runWorker("create writer", func() error {
		n := createOps.Add(1)
		b := beads.Bead{
			Title:    fmt.Sprintf("created-%d", n),
			Assignee: "builder",
		}
		if n%5 == 0 {
			b.Ephemeral = true
		}
		_, err := store.Create(b)
		return err
	})

	close(start)
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent worker error: %v", err)
	}
	if !listObserved.Load() {
		t.Fatal("List reader did not observe non-empty results")
	}
	if !readyObserved.Load() {
		t.Fatal("Ready reader did not observe non-empty results")
	}
}

func TestHQStoreListRecentDescSmallDataset(t *testing.T) {
	store, err := beads.OpenHQStore(t.TempDir(), beads.WithHQStoreSnapshotInterval(0))
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	var created []beads.Bead
	for i, b := range []beads.Bead{
		{Title: "old open", CreatedAt: base.Add(1 * time.Minute)},
		{Title: "new closed", Status: "closed", CreatedAt: base.Add(2 * time.Minute)},
		{Title: "newer wisp", Type: "message", Ephemeral: true, CreatedAt: base.Add(3 * time.Minute)},
		{Title: "newest open", CreatedAt: base.Add(4 * time.Minute)},
	} {
		got, err := store.Create(b)
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		created = append(created, got)
	}

	query := beads.ListQuery{
		AllowScan: true,
		Limit:     3,
		Sort:      beads.SortCreatedDesc,
		TierMode:  beads.TierBoth,
	}
	got, err := store.List(query)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{created[3].ID, created[2].ID, created[0].ID}
	if gotIDs := hqTestBeadIDs(got); !slices.Equal(gotIDs, want) {
		t.Fatalf("List IDs = %v, want %v", gotIDs, want)
	}
}

func TestHQStoreListRecentDescLargeMixedDataset(t *testing.T) {
	store, err := beads.OpenHQStore(t.TempDir(), beads.WithHQStoreSnapshotInterval(0))
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	const total = 2500
	base := time.Date(2026, 5, 27, 11, 0, 0, 0, time.UTC)
	all := make([]beads.Bead, 0, total)
	for i := range total {
		b := beads.Bead{
			Title:     fmt.Sprintf("mixed-%d", i),
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		if i%3 == 0 {
			b.Status = "closed"
		}
		if i%7 == 0 {
			b.Type = "message"
			b.Ephemeral = true
		}
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		all = append(all, created)
	}

	query := beads.ListQuery{
		AllowScan:     true,
		IncludeClosed: true,
		Limit:         5,
		Sort:          beads.SortCreatedDesc,
		TierMode:      beads.TierBoth,
	}
	got, err := store.List(query)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := beads.ApplyListQuery(all, query)
	if gotIDs, wantIDs := hqTestBeadIDs(got), hqTestBeadIDs(want); !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("List IDs = %v, want %v", gotIDs, wantIDs)
	}
}

func TestListRecentDescConcurrentWithWriters(t *testing.T) {
	store, err := beads.OpenHQStore(t.TempDir(), beads.WithHQStoreSnapshotInterval(0))
	if err != nil {
		t.Fatalf("OpenHQStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	const seedCount = 1000
	seedIDs := make([]string, 0, seedCount)
	base := time.Now().Add(-24 * time.Hour)
	for i := range seedCount {
		created, err := store.Create(beads.Bead{
			Title:     fmt.Sprintf("recent-seed-%d", i),
			CreatedAt: base.Add(time.Duration(i) * time.Second),
			Metadata:  map[string]string{"seed": "true"},
		})
		if err != nil {
			t.Fatalf("Create seed %d: %v", i, err)
		}
		seedIDs = append(seedIDs, created.ID)
	}

	var (
		listObserved atomic.Bool
		metadataOps  atomic.Uint64
		createOps    atomic.Uint64
		wg           sync.WaitGroup
	)
	start := make(chan struct{})
	stop := make(chan struct{})
	errs := make(chan error, 4)
	runWorker := func(name string, fn func() error) {
		t.Helper()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := fn(); err != nil {
					select {
					case errs <- fmt.Errorf("%s: %w", name, err):
					case <-stop:
					}
					return
				}
			}
		}()
	}

	runWorker("recent list reader", func() error {
		got, err := store.List(beads.ListQuery{
			AllowScan:     true,
			IncludeClosed: true,
			Limit:         5,
			Sort:          beads.SortCreatedDesc,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			return err
		}
		if len(got) == 0 {
			return errors.New("List returned no beads")
		}
		if len(got) > 5 {
			return fmt.Errorf("List returned %d beads, want at most 5", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].CreatedAt.Before(got[i].CreatedAt) {
				return fmt.Errorf("List returned non-descending CreatedAt order at %d", i)
			}
		}
		listObserved.Store(true)
		return nil
	})
	runWorker("metadata writer", func() error {
		n := metadataOps.Add(1)
		id := seedIDs[int(n-1)%len(seedIDs)]
		return store.SetMetadataBatch(id, map[string]string{"recent.race": fmt.Sprint(n)})
	})
	runWorker("create writer", func() error {
		n := createOps.Add(1)
		b := beads.Bead{Title: fmt.Sprintf("recent-created-%d", n)}
		if n%4 == 0 {
			b.Status = "closed"
		}
		if n%6 == 0 {
			b.Type = "message"
			b.Ephemeral = true
		}
		_, err := store.Create(b)
		return err
	})

	close(start)
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent worker error: %v", err)
	}
	if !listObserved.Load() {
		t.Fatal("List reader did not observe non-empty results")
	}
}

func hqTestBeadIDs(items []beads.Bead) []string {
	ids := make([]string, 0, len(items))
	for _, b := range items {
		ids = append(ids, b.ID)
	}
	return ids
}

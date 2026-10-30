package beads

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The tests in this file pin RowRefreshOptions.YieldToLocalWrites: a refresh
// that lands between a local write's backing commit and the cache's absorb of
// it would read the write's row before the cache holds it and announce the
// cache's own write as a refresh. With the option the refresh yields to the
// write instead (ErrRowRefreshYielded, an ErrRowRefreshFenced), and the write
// alone announces it.

// writeHookStore runs onWrite with the written row's id after each backing
// write commits, before the write returns to the cache.
type writeHookStore struct {
	*MemStore
	onWrite func(id string)
}

func (s *writeHookStore) hook(id string) {
	if s.onWrite != nil {
		s.onWrite(id)
	}
}

func (s *writeHookStore) Create(b Bead) (Bead, error) {
	created, err := s.MemStore.Create(b)
	if err == nil {
		s.hook(created.ID)
	}
	return created, err
}

func (s *writeHookStore) Update(id string, opts UpdateOpts) error {
	err := s.MemStore.Update(id, opts)
	if err == nil {
		s.hook(id)
	}
	return err
}

func (s *writeHookStore) Close(id string) error {
	err := s.MemStore.Close(id)
	if err == nil {
		s.hook(id)
	}
	return err
}

func (s *writeHookStore) Reopen(id string) error {
	err := s.MemStore.Reopen(id)
	if err == nil {
		s.hook(id)
	}
	return err
}

func (s *writeHookStore) SetMetadata(id, key, value string) error {
	err := s.MemStore.SetMetadata(id, key, value)
	if err == nil {
		s.hook(id)
	}
	return err
}

func (s *writeHookStore) SetMetadataBatch(id string, kvs map[string]string) error {
	err := s.MemStore.SetMetadataBatch(id, kvs)
	if err == nil {
		s.hook(id)
	}
	return err
}

func (s *writeHookStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	n, err := s.MemStore.CloseAll(ids, metadata)
	if err == nil {
		for _, id := range ids {
			s.hook(id)
		}
	}
	return n, err
}

func (s *writeHookStore) DepAdd(issueID, dependsOnID, depType string) error {
	err := s.MemStore.DepAdd(issueID, dependsOnID, depType)
	if err == nil {
		s.hook(issueID)
	}
	return err
}

func (s *writeHookStore) Delete(id string) error {
	err := s.MemStore.Delete(id)
	if err == nil {
		s.hook(id)
	}
	return err
}

func (s *writeHookStore) ApplyGuarded(ctx context.Context, b GuardedBatch) (GuardedResult, error) {
	res, err := s.MemStore.ApplyGuarded(ctx, b)
	if err == nil {
		ids, _ := guardedBatchTouched(b)
		for _, id := range ids {
			s.hook(id)
		}
	}
	return res, err
}

// refreshNotifications keeps the notifications a refresh emitted.
func refreshNotifications(events []string) []string {
	var out []string
	for _, e := range events {
		if strings.HasPrefix(e, ChangeRefresh.String()+" ") {
			out = append(out, e)
		}
	}
	return out
}

func TestCachingStoreRefreshRowReportYieldsToAnInFlightLocalWrite(t *testing.T) {
	t.Parallel()

	yield := RowRefreshOptions{YieldToLocalWrites: true, AnnounceUnheldClose: true}
	for _, tc := range []struct {
		name string
		// write runs the local write of a seeded row id and returns the id
		// the backing hook names.
		write func(t *testing.T, cache *CachingStore, id string) error
		// gone reports the row is deleted after the write.
		gone bool
		// edgesUnread skips the after-write check: MemStore's Get rows carry
		// no edges, so a refresh of a row whose edges a write installed
		// reports the edge-less row as an update whatever is in flight.
		edgesUnread bool
	}{
		{name: "create", write: func(_ *testing.T, cache *CachingStore, _ string) error {
			_, err := cache.Create(Bead{Title: "local"})
			return err
		}},
		{name: "update", write: func(_ *testing.T, cache *CachingStore, id string) error {
			title := "renamed"
			return cache.Update(id, UpdateOpts{Title: &title})
		}},
		{name: "close", write: func(_ *testing.T, cache *CachingStore, id string) error {
			return cache.Close(id)
		}},
		{name: "reopen", write: func(t *testing.T, cache *CachingStore, id string) error {
			if err := cache.Close(id); err != nil {
				t.Fatalf("Close: %v", err)
			}
			return cache.Reopen(id)
		}},
		{name: "set_metadata", write: func(_ *testing.T, cache *CachingStore, id string) error {
			return cache.SetMetadata(id, "k", "v")
		}},
		{name: "set_metadata_batch", write: func(_ *testing.T, cache *CachingStore, id string) error {
			return cache.SetMetadataBatch(id, map[string]string{"k": "v", "k2": "v2"})
		}},
		{name: "close_all", write: func(_ *testing.T, cache *CachingStore, id string) error {
			_, err := cache.CloseAll([]string{id}, map[string]string{"k": "v"})
			return err
		}},
		{name: "dep_add", edgesUnread: true, write: func(t *testing.T, cache *CachingStore, id string) error {
			other := mustCreateCached(t, cache)
			return cache.DepAdd(id, other.ID, "blocks")
		}},
		{name: "delete", gone: true, write: func(_ *testing.T, cache *CachingStore, id string) error {
			return cache.Delete(id)
		}},
		{name: "apply_guarded", write: func(t *testing.T, cache *CachingStore, id string) error {
			batcher, ok := cache.GuardedBatcherHandle()
			if !ok {
				t.Fatal("GuardedBatcherHandle: the backing applies no guarded batches")
			}
			title := "renamed"
			_, err := batcher.ApplyGuarded(context.Background(), GuardedBatch{Items: []GuardedItem{
				{Create: &Bead{ID: "gc-guarded", Title: "pinned"}},
				{Update: &GuardedUpdate{ID: id, Opts: UpdateOpts{Title: &title}}},
			}})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &writeHookStore{MemStore: NewMemStore()}
			cache, rec := newReportCacheForTest(t, backing)
			seed := mustCreateCached(t, cache)
			rec.take()

			var hooked []string
			var errs []error
			backing.onWrite = func(id string) {
				hooked = append(hooked, id)
				_, err := cache.RefreshRowReport(id, yield)
				errs = append(errs, err)
			}
			if err := tc.write(t, cache, seed.ID); err != nil {
				t.Fatalf("write: %v", err)
			}
			backing.onWrite = nil
			if len(hooked) == 0 {
				t.Fatal("the backing hook never ran")
			}
			for i, err := range errs {
				if !errors.Is(err, ErrRowRefreshYielded) || !errors.Is(err, ErrRowRefreshFenced) {
					t.Fatalf("refresh of %s inside the write = %v, want ErrRowRefreshYielded, an ErrRowRefreshFenced", hooked[i], err)
				}
			}
			if got := refreshNotifications(rec.take()); len(got) != 0 {
				t.Fatalf("the in-flight refresh notified %q, want nothing", got)
			}
			if tc.edgesUnread {
				return
			}
			// Once the write returned, the refresh finds nothing new.
			for _, id := range hooked {
				rep, err := cache.RefreshRowReport(id, yield)
				if tc.gone {
					if !errors.Is(err, ErrNotFound) {
						t.Fatalf("refresh of the deleted %s = %v, want ErrNotFound", id, err)
					}
				} else if err != nil {
					t.Fatalf("refresh of %s after the write: %v", id, err)
				}
				if rep.Event != "" {
					t.Fatalf("refresh of %s after the write announced %q, want nothing", id, rep.Event)
				}
			}
			assertEvents(t, rec.take())
		})
	}
}

// A write whose ids its backing call decides (Create, Tx, a graph apply)
// fences every yielding refresh while it is in flight; a write of known ids
// fences only those.
func TestCachingStoreRefreshRowReportYieldsOnlyToWritesThatMayTouchTheRow(t *testing.T) {
	t.Parallel()

	backing := &writeHookStore{MemStore: NewMemStore()}
	cache, rec := newReportCacheForTest(t, backing)
	seed := mustCreateCached(t, cache)
	outOfBand, err := backing.MemStore.Create(Bead{Title: "out of band"})
	if err != nil {
		t.Fatal(err)
	}
	rec.take()

	var during error
	backing.onWrite = func(string) {
		_, during = cache.RefreshRowReport(outOfBand.ID, RowRefreshOptions{YieldToLocalWrites: true})
	}
	if err := cache.SetMetadata(seed.ID, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if during != nil {
		t.Fatalf("a refresh of another row during a known-id write = %v, want it to proceed", during)
	}
	if got := refreshNotifications(rec.take()); len(got) != 1 || !strings.Contains(got[0], "bead.created "+outOfBand.ID) {
		t.Fatalf("refresh notifications = %q, want the out-of-band create", got)
	}

	other, err := backing.MemStore.Create(Bead{Title: "out of band too"})
	if err != nil {
		t.Fatal(err)
	}
	backing.onWrite = func(string) {
		_, during = cache.RefreshRowReport(other.ID, RowRefreshOptions{YieldToLocalWrites: true})
	}
	if _, err := cache.Create(Bead{Title: "local"}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(during, ErrRowRefreshYielded) {
		t.Fatalf("a refresh during a create = %v, want ErrRowRefreshYielded", during)
	}

	backing.onWrite = nil
	var inTx error
	if err := cache.Tx("test", func(tx Tx) error {
		_, inTx = cache.RefreshRowReport(other.ID, RowRefreshOptions{YieldToLocalWrites: true})
		return tx.SetMetadataBatch(seed.ID, map[string]string{"tx": "1"})
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if !errors.Is(inTx, ErrRowRefreshYielded) {
		t.Fatalf("a refresh during a Tx = %v, want ErrRowRefreshYielded", inTx)
	}
	if rep, err := cache.RefreshRowReport(other.ID, RowRefreshOptions{YieldToLocalWrites: true}); err != nil || rep.Event != "bead.created" {
		t.Fatalf("refresh after the writes = %q, %v; want the out-of-band create", rep.Event, err)
	}
}

// A create makes a row the cache does not hold, so while one is in flight a
// yielding refresh yields for every row the cache does not hold (the created
// one among them) but not for a held row: a steady stream of creates does not
// stall the refresh of the rows the cache already holds.
func TestCachingStoreRefreshRowReportYieldsToACreateOnlyForUnheldRows(t *testing.T) {
	t.Parallel()

	backing := &writeHookStore{MemStore: NewMemStore()}
	cache, rec := newReportCacheForTest(t, backing)
	held := mustCreateCached(t, cache)
	if err := backing.MemStore.SetMetadata(held.ID, "k", "out of band"); err != nil {
		t.Fatal(err)
	}
	unheld, err := backing.MemStore.Create(Bead{Title: "out of band"})
	if err != nil {
		t.Fatal(err)
	}
	rec.take()

	yield := RowRefreshOptions{YieldToLocalWrites: true}
	var created string
	var heldErr, unheldErr, createdErr error
	var heldEvent string
	backing.onWrite = func(id string) {
		created = id
		var rep RowRefresh
		rep, heldErr = cache.RefreshRowReport(held.ID, yield)
		heldEvent = rep.Event
		_, unheldErr = cache.RefreshRowReport(unheld.ID, yield)
		_, createdErr = cache.RefreshRowReport(id, yield)
	}
	if _, err := cache.Create(Bead{Title: "local"}); err != nil {
		t.Fatal(err)
	}
	backing.onWrite = nil
	if created == "" {
		t.Fatal("the backing hook never ran")
	}
	if heldErr != nil || heldEvent != "bead.updated" {
		t.Errorf("refresh of a held row during a create = %q, %v; want its out-of-band update", heldEvent, heldErr)
	}
	if !errors.Is(unheldErr, ErrRowRefreshYielded) {
		t.Errorf("refresh of an unheld row during a create = %v, want ErrRowRefreshYielded", unheldErr)
	}
	if !errors.Is(createdErr, ErrRowRefreshYielded) {
		t.Errorf("refresh of the created row during its create = %v, want ErrRowRefreshYielded", createdErr)
	}
	for _, e := range refreshNotifications(rec.take()) {
		if strings.HasSuffix(e, " "+created) {
			t.Errorf("the in-flight refresh notified %q for the cache's own create", e)
		}
	}
}

// batchCallHookStore runs onCall as each CloseAll or ApplyGuarded reaches the
// backing, whatever rows it names, before the backing applies it.
type batchCallHookStore struct {
	*MemStore
	onCall func()
}

func (s *batchCallHookStore) hook() {
	if s.onCall != nil {
		s.onCall()
	}
}

func (s *batchCallHookStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.hook()
	return s.MemStore.CloseAll(ids, metadata)
}

func (s *batchCallHookStore) ApplyGuarded(ctx context.Context, b GuardedBatch) (GuardedResult, error) {
	s.hook()
	return s.MemStore.ApplyGuarded(ctx, b)
}

// An empty CloseAll or guarded batch names no row, so it is not in flight as
// a write whose rows its backing call decides: a yielding refresh of another
// row proceeds while the call runs.
func TestCachingStoreRefreshRowReportDoesNotYieldToAnEmptyBatchWrite(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		write func(t *testing.T, cache *CachingStore)
	}{
		{name: "close_all", write: func(t *testing.T, cache *CachingStore) {
			if n, err := cache.CloseAll(nil, map[string]string{"k": "v"}); n != 0 || err != nil {
				t.Fatalf("CloseAll of no ids = %d, %v; want 0, nil", n, err)
			}
		}},
		{name: "apply_guarded", write: func(t *testing.T, cache *CachingStore) {
			batcher, ok := cache.GuardedBatcherHandle()
			if !ok {
				t.Fatal("GuardedBatcherHandle: the backing applies no guarded batches")
			}
			if _, err := batcher.ApplyGuarded(context.Background(), GuardedBatch{}); !errors.Is(err, ErrInvalidGuardedBatch) {
				t.Fatalf("ApplyGuarded of an empty batch = %v, want ErrInvalidGuardedBatch", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &batchCallHookStore{MemStore: NewMemStore()}
			cache, rec := newReportCacheForTest(t, backing)
			outOfBand, err := backing.Create(Bead{Title: "out of band"})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			var during error
			backing.onCall = func() {
				calls++
				_, during = cache.RefreshRowReport(outOfBand.ID, RowRefreshOptions{YieldToLocalWrites: true})
			}
			tc.write(t, cache)
			backing.onCall = nil
			if calls != 1 {
				t.Fatalf("the write reached the backing %d times, want once", calls)
			}
			if during != nil {
				t.Fatalf("a refresh of another row during the empty write = %v, want it to proceed", during)
			}
			if got := refreshNotifications(rec.take()); len(got) != 1 || !strings.Contains(got[0], "bead.created "+outOfBand.ID) {
				t.Fatalf("refresh notifications = %q, want the out-of-band create", got)
			}
		})
	}
}

// A refresh yields before its backing read, so a caller retrying a row while
// a graph apply holds the fence for seconds reads nothing, and again after
// the read, for a write begun during it.
func TestCachingStoreRefreshRowReportYieldsBeforeItsBackingRead(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	counted := &observationReadCountingStore{Store: mem}
	backing := &getHookStore{Store: counted}
	cache, rec := newReportCacheForTest(t, backing)
	outOfBand, err := mem.Create(Bead{Title: "out of band"})
	if err != nil {
		t.Fatal(err)
	}
	rec.take()
	yield := RowRefreshOptions{YieldToLocalWrites: true}

	_, end := cache.beginLocalWrite()
	before := counted.readCalls.Load()
	for range 3 {
		if _, err := cache.RefreshRowReport(outOfBand.ID, yield); !errors.Is(err, ErrRowRefreshYielded) {
			t.Fatalf("a refresh during a write = %v, want ErrRowRefreshYielded", err)
		}
	}
	if reads := counted.readCalls.Load() - before; reads != 0 {
		t.Fatalf("the yielding refreshes made %d backing reads, want none", reads)
	}
	end()

	// A create in flight may be making the row the cache does not hold.
	_, endCreate := cache.beginLocalCreate()
	before = counted.readCalls.Load()
	if _, err := cache.RefreshRowReport(outOfBand.ID, yield); !errors.Is(err, ErrRowRefreshYielded) {
		t.Fatalf("a refresh of an unheld row during a create = %v, want ErrRowRefreshYielded", err)
	}
	if reads := counted.readCalls.Load() - before; reads != 0 {
		t.Fatalf("the refresh yielding to a create made %d backing reads, want none", reads)
	}
	endCreate()

	var endDuring func()
	backing.onGet = func(id string) { _, endDuring = cache.beginLocalWrite(id) }
	if _, err := cache.RefreshRowReport(outOfBand.ID, yield); !errors.Is(err, ErrRowRefreshYielded) {
		t.Fatalf("a refresh during a write begun in its read = %v, want ErrRowRefreshYielded", err)
	}
	if endDuring == nil {
		t.Fatal("the refresh never read the row")
	}
	endDuring()
	if got := refreshNotifications(rec.take()); len(got) != 0 {
		t.Fatalf("the yielding refreshes notified %q, want nothing", got)
	}
	if rep, err := cache.RefreshRowReport(outOfBand.ID, yield); err != nil || rep.Event != "bead.created" {
		t.Fatalf("refresh after the writes = %q, %v; want the out-of-band create", rep.Event, err)
	}
}

// A local write that began during the refresh's backing read and absorbed
// its row before the refresh took the lock, but has not returned yet, makes
// the refresh yield rather than fence: the yield spends none of the ledger
// feed's retries, and the row is the write's.
func TestCachingStoreRefreshRowReportYieldsBeforeItFencesOnAnInFlightWrite(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	backing := &getHookStore{Store: mem}
	cache, rec := newReportCacheForTest(t, backing)
	seed := mustCreateCached(t, cache)
	rec.take()

	var end func()
	backing.onGet = func(id string) {
		// The write starts and absorbs its row while the refresh reads.
		_, end = cache.beginLocalWrite(id)
		cache.mu.Lock()
		cache.noteLocalMutationLocked(id)
		cache.mu.Unlock()
	}
	_, err := cache.RefreshRowReport(seed.ID, RowRefreshOptions{YieldToLocalWrites: true})
	if end == nil {
		t.Fatal("the refresh never read the row")
	}
	end()
	if !errors.Is(err, ErrRowRefreshYielded) {
		t.Fatalf("refresh = %v, want ErrRowRefreshYielded", err)
	}
	// Without the yield option the same race is a fence.
	backing.onGet = func(id string) {
		cache.mu.Lock()
		cache.noteLocalMutationLocked(id)
		cache.mu.Unlock()
	}
	if _, err := cache.RefreshRowReport(seed.ID, RowRefreshOptions{}); !errors.Is(err, ErrRowRefreshFenced) || errors.Is(err, ErrRowRefreshYielded) {
		t.Fatalf("refresh without the yield = %v, want ErrRowRefreshFenced only", err)
	}
}

// RefreshRow (v2's lag repair) does not yield: its fences are unchanged.
func TestCachingStoreRefreshRowDoesNotYieldToInFlightWrites(t *testing.T) {
	t.Parallel()

	backing := &writeHookStore{MemStore: NewMemStore()}
	cache, _ := newReportCacheForTest(t, backing)
	seed := mustCreateCached(t, cache)
	var during error
	backing.onWrite = func(id string) { _, during = cache.RefreshRow(id) }
	if err := cache.SetMetadata(seed.ID, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if during != nil {
		t.Fatalf("RefreshRow inside a write = %v, want it to proceed", during)
	}
}

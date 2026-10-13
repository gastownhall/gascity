package beads

import (
	"errors"
	"strings"
	"testing"
)

// The tests in this file pin RowRefreshOptions.YieldToLocalWrites: a refresh
// that lands between a local write's backing commit and the cache's absorb of
// it would read the write's row before the cache holds it and announce the
// cache's own write as a refresh. With the option the refresh yields to the
// write instead (ErrRowRefreshFenced), and the write alone announces it.

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
				if !errors.Is(err, ErrRowRefreshFenced) {
					t.Fatalf("refresh of %s inside the write = %v, want ErrRowRefreshFenced", hooked[i], err)
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
	if !errors.Is(during, ErrRowRefreshFenced) {
		t.Fatalf("a refresh during a create = %v, want ErrRowRefreshFenced", during)
	}

	backing.onWrite = nil
	var inTx error
	if err := cache.Tx("test", func(tx Tx) error {
		_, inTx = cache.RefreshRowReport(other.ID, RowRefreshOptions{YieldToLocalWrites: true})
		return tx.SetMetadataBatch(seed.ID, map[string]string{"tx": "1"})
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if !errors.Is(inTx, ErrRowRefreshFenced) {
		t.Fatalf("a refresh during a Tx = %v, want ErrRowRefreshFenced", inTx)
	}
	if rep, err := cache.RefreshRowReport(other.ID, RowRefreshOptions{YieldToLocalWrites: true}); err != nil || rep.Event != "bead.created" {
		t.Fatalf("refresh after the writes = %q, %v; want the out-of-band create", rep.Event, err)
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

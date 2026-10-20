package beads

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The tests in this file pin the whole window RowRefreshOptions.YieldToLocalWrites
// covers, for every write entry point the local-write census lists: from the
// write's start, through its backing call and the reads it makes after that
// call commits, to the absorb of its rows. A refresh of a row the write
// touches yields anywhere in it. windowStore probes after every backing call
// the write makes, so a write that registers the wrong ids, or ends its
// registration before its absorb, is caught where its post-commit read lands.

// windowStore runs probe after every backing call, reads included, and
// implements every optional write capability a CachingStore forwards, the
// ones MemStore lacks with simple fakes over it.
type windowStore struct {
	*MemStore
	// probe, when set, runs after each backing call; created names the rows
	// a create or graph apply just made.
	probe   func(created ...string)
	probing bool
}

func (s *windowStore) after(created ...string) {
	if s.probe == nil || s.probing {
		return
	}
	s.probing = true
	defer func() { s.probing = false }()
	s.probe(created...)
}

func (s *windowStore) Get(id string) (Bead, error) {
	b, err := s.MemStore.Get(id)
	s.after()
	return b, err
}

func (s *windowStore) DepList(id, direction string) ([]Dep, error) {
	deps, err := s.MemStore.DepList(id, direction)
	s.after()
	return deps, err
}

func (s *windowStore) Create(b Bead) (Bead, error) {
	created, err := s.MemStore.Create(b)
	s.after(created.ID)
	return created, err
}

func (s *windowStore) CreateWithStorage(b Bead, _ StorageClass) (Bead, error) {
	return s.Create(b)
}

func (s *windowStore) Update(id string, opts UpdateOpts) error {
	err := s.MemStore.Update(id, opts)
	s.after()
	return err
}

func (s *windowStore) Close(id string) error {
	err := s.MemStore.Close(id)
	s.after()
	return err
}

func (s *windowStore) Reopen(id string) error {
	err := s.MemStore.Reopen(id)
	s.after()
	return err
}

func (s *windowStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	n, err := s.MemStore.CloseAll(ids, metadata)
	s.after()
	return n, err
}

func (s *windowStore) SetMetadata(id, key, value string) error {
	err := s.MemStore.SetMetadata(id, key, value)
	s.after()
	return err
}

func (s *windowStore) SetMetadataBatch(id string, kvs map[string]string) error {
	err := s.MemStore.SetMetadataBatch(id, kvs)
	s.after()
	return err
}

func (s *windowStore) DepAdd(issueID, dependsOnID, depType string) error {
	err := s.MemStore.DepAdd(issueID, dependsOnID, depType)
	s.after()
	return err
}

func (s *windowStore) DepRemove(issueID, dependsOnID string) error {
	err := s.MemStore.DepRemove(issueID, dependsOnID)
	s.after()
	return err
}

func (s *windowStore) Delete(id string) error {
	err := s.MemStore.Delete(id)
	s.after()
	return err
}

func (s *windowStore) DeleteBatch(ids []string) error {
	for _, id := range ids {
		if err := s.MemStore.Delete(id); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	s.after()
	return nil
}

func (s *windowStore) Tx(commitMsg string, fn func(Tx) error) error {
	err := s.MemStore.Tx(commitMsg, fn)
	s.after()
	return err
}

func (s *windowStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	err := s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
	s.after()
	return err
}

func (s *windowStore) CloseIfMatch(id string, expectedRevision int64) error {
	err := s.MemStore.CloseIfMatch(id, expectedRevision)
	s.after()
	return err
}

func (s *windowStore) DeleteIfMatch(id string, expectedRevision int64) error {
	err := s.MemStore.DeleteIfMatch(id, expectedRevision)
	s.after()
	return err
}

func (s *windowStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	swapped, err := s.MemStore.CompareAndSetMetadataKey(id, key, expected, next)
	s.after()
	return swapped, err
}

func (s *windowStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	released, err := s.MemStore.ReleaseIfCurrent(id, expectedAssignee)
	s.after()
	return released, err
}

func (s *windowStore) ApplyGuarded(ctx context.Context, b GuardedBatch) (GuardedResult, error) {
	res, err := s.MemStore.ApplyGuarded(ctx, b)
	s.after()
	return res, err
}

// Claim is a fake two-argument claim: it takes an unassigned or already-held
// row in progress for assignee.
func (s *windowStore) Claim(id, assignee string) (Bead, bool, error) {
	row, err := s.MemStore.Get(id)
	if err != nil {
		return Bead{}, false, err
	}
	if row.Assignee != "" && row.Assignee != assignee {
		return row, false, nil
	}
	status := "in_progress"
	if err := s.MemStore.Update(id, UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		return Bead{}, false, err
	}
	claimed, err := s.MemStore.Get(id)
	s.after()
	return claimed, err == nil, err
}

// TransferIfCurrent is a fake conditional transfer of an assignment.
func (s *windowStore) TransferIfCurrent(id, fromAssignee, toAssignee string) (bool, error) {
	row, err := s.MemStore.Get(id)
	if err != nil {
		return false, err
	}
	if row.Assignee != fromAssignee {
		return false, nil
	}
	if err := s.MemStore.Update(id, UpdateOpts{Assignee: &toAssignee}); err != nil {
		return false, err
	}
	s.after()
	return true, nil
}

// UpdateIfAssignment is a fake status-and-assignee guarded update.
func (s *windowStore) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts UpdateOpts) (bool, error) {
	row, err := s.MemStore.Get(id)
	if err != nil {
		return false, err
	}
	if row.Status != expectedStatus || row.Assignee != expectedAssignee {
		return false, nil
	}
	if err := s.MemStore.Update(id, opts); err != nil {
		return false, err
	}
	s.after()
	return true, nil
}

// CloseWithMetadataIfMatch is a fake atomic conditional close.
func (s *windowStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (Bead, error) {
	row, err := s.MemStore.Get(id)
	if err != nil {
		return Bead{}, err
	}
	if row.Revision != expectedRevision {
		return Bead{}, &PreconditionFailedError{ID: id, Expected: expectedRevision, Current: row.Revision}
	}
	if err := s.MemStore.SetMetadataBatch(id, metadata); err != nil {
		return Bead{}, err
	}
	if err := s.MemStore.Close(id); err != nil {
		return Bead{}, err
	}
	closed, err := s.MemStore.Get(id)
	s.after()
	return closed, err
}

// ApplyGraphPlan is a fake graph apply that creates the plan's nodes.
func (s *windowStore) ApplyGraphPlan(_ context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	res := &GraphApplyResult{IDs: map[string]string{}}
	created := make([]string, 0, len(plan.Nodes))
	for _, node := range plan.Nodes {
		b, err := s.MemStore.Create(Bead{Title: node.Title})
		if err != nil {
			return nil, err
		}
		res.IDs[node.Key] = b.ID
		created = append(created, b.ID)
	}
	s.after(created...)
	return res, nil
}

func (s *windowStore) ApplyGraphPlanWithStorage(ctx context.Context, plan *GraphApplyPlan, _ StorageClass) (*GraphApplyResult, error) {
	return s.ApplyGraphPlan(ctx, plan)
}

// windowSeed is the state a window case writes over: a held open row, a
// second held row, and the revision the seed row carries.
type windowSeed struct {
	id, other string
	revision  int64
}

func TestCachingStoreYieldCoversEveryWriteFromStartToAbsorb(t *testing.T) {
	t.Parallel()

	closed := "closed"
	inProgress := "in_progress"
	worker := "w1"
	assign := func(t *testing.T, cache *CachingStore, s windowSeed) {
		t.Helper()
		if err := cache.Update(s.id, UpdateOpts{Status: &inProgress, Assignee: &worker}); err != nil {
			t.Fatal(err)
		}
	}
	graphPlan := &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "a", Title: "a"}, {Key: "b", Title: "b"}}}
	for _, tc := range []struct {
		name string
		// setup shapes the seed before the write; nil for none.
		setup func(t *testing.T, cache *CachingStore, s windowSeed)
		// write runs the write over the seed.
		write func(cache *CachingStore, s windowSeed) error
		// targets names the seeded rows the write touches; the rows it
		// creates are added as the backing reports them.
		targets func(s windowSeed) []string
	}{
		{name: "Create", targets: func(windowSeed) []string { return nil }, write: func(cache *CachingStore, _ windowSeed) error {
			_, err := cache.Create(Bead{Title: "local"})
			return err
		}},
		{name: "CreateWithStorage", targets: func(windowSeed) []string { return nil }, write: func(cache *CachingStore, _ windowSeed) error {
			_, err := cache.CreateWithStorage(Bead{Title: "local"}, StorageNoHistory)
			return err
		}},
		{name: "Update", write: func(cache *CachingStore, s windowSeed) error {
			title := "renamed"
			return cache.Update(s.id, UpdateOpts{Title: &title})
		}},
		{name: "Close", write: func(cache *CachingStore, s windowSeed) error { return cache.Close(s.id) }},
		{name: "Reopen", setup: func(t *testing.T, cache *CachingStore, s windowSeed) {
			if err := cache.Close(s.id); err != nil {
				t.Fatal(err)
			}
		}, write: func(cache *CachingStore, s windowSeed) error { return cache.Reopen(s.id) }},
		{name: "CloseAll", write: func(cache *CachingStore, s windowSeed) error {
			_, err := cache.CloseAll([]string{s.id, s.other}, map[string]string{"k": "v"})
			return err
		}, targets: func(s windowSeed) []string { return []string{s.id, s.other} }},
		{name: "SetMetadata", write: func(cache *CachingStore, s windowSeed) error {
			return cache.SetMetadata(s.id, "k", "v")
		}},
		{name: "SetMetadataBatch", write: func(cache *CachingStore, s windowSeed) error {
			return cache.SetMetadataBatch(s.id, map[string]string{"k": "v", "k2": "v2"})
		}},
		{name: "DepAdd", write: func(cache *CachingStore, s windowSeed) error {
			return cache.DepAdd(s.id, s.other, "blocks")
		}},
		{name: "DepRemove", setup: func(t *testing.T, cache *CachingStore, s windowSeed) {
			if err := cache.DepAdd(s.id, s.other, "blocks"); err != nil {
				t.Fatal(err)
			}
		}, write: func(cache *CachingStore, s windowSeed) error { return cache.DepRemove(s.id, s.other) }},
		{name: "Delete", write: func(cache *CachingStore, s windowSeed) error { return cache.Delete(s.id) }},
		{name: "DeleteBatch", write: func(cache *CachingStore, s windowSeed) error {
			return cache.DeleteBatch([]string{s.id, s.other})
		}, targets: func(s windowSeed) []string { return []string{s.id, s.other} }},
		{name: "DeleteIfMatch", write: func(cache *CachingStore, s windowSeed) error {
			return cache.DeleteIfMatch(s.id, s.revision)
		}},
		{name: "Tx", write: func(cache *CachingStore, s windowSeed) error {
			return cache.Tx("window", func(tx Tx) error {
				return tx.SetMetadataBatch(s.id, map[string]string{"tx": "1"})
			})
		}},
		{name: "UpdateIfMatch", write: func(cache *CachingStore, s windowSeed) error {
			return cache.UpdateIfMatch(s.id, s.revision, UpdateOpts{Metadata: map[string]string{"k": "v"}})
		}},
		{name: "CloseIfMatch", write: func(cache *CachingStore, s windowSeed) error {
			return cache.CloseIfMatch(s.id, s.revision)
		}},
		{name: "CompareAndSetMetadataKey", write: func(cache *CachingStore, s windowSeed) error {
			_, err := cache.CompareAndSetMetadataKey(s.id, "k", "", "v")
			return err
		}},
		{name: "ReleaseIfCurrent", setup: assign, write: func(cache *CachingStore, s windowSeed) error {
			_, err := cache.ReleaseIfCurrent(s.id, worker)
			return err
		}},
		{name: "TransferIfCurrent", setup: assign, write: func(cache *CachingStore, s windowSeed) error {
			_, err := cache.TransferIfCurrent(s.id, worker, "w2")
			return err
		}},
		{name: "Claim", write: func(cache *CachingStore, s windowSeed) error {
			_, _, err := cache.Claim(s.id, worker)
			return err
		}},
		{name: "UpdateIfAssignment", write: func(cache *CachingStore, s windowSeed) error {
			_, err := cache.UpdateIfAssignment(s.id, "open", "", UpdateOpts{Status: &closed})
			return err
		}},
		{name: "CloseWithMetadataIfMatch", write: func(cache *CachingStore, s windowSeed) error {
			_, err := closeWithCacheHandle(cache, s.id, s.revision, map[string]string{"k": "v"})
			return err
		}},
		{name: "ApplyGuarded", write: func(cache *CachingStore, s windowSeed) error {
			batcher, ok := cache.GuardedBatcherHandle()
			if !ok {
				return errors.New("GuardedBatcherHandle: the backing applies no guarded batches")
			}
			title := "renamed"
			_, err := batcher.ApplyGuarded(context.Background(), GuardedBatch{Items: []GuardedItem{
				{Create: &Bead{ID: "gc-guarded", Title: "pinned"}},
				{Update: &GuardedUpdate{ID: s.id, Opts: UpdateOpts{Title: &title}}},
			}})
			return err
		}, targets: func(s windowSeed) []string { return []string{s.id, "gc-guarded"} }},
		{name: "ApplyGraphPlan", write: func(cache *CachingStore, _ windowSeed) error {
			applier, ok := cache.GraphApplyHandle()
			if !ok {
				return errors.New("GraphApplyHandle: the backing applies no graphs")
			}
			_, err := applier.ApplyGraphPlan(context.Background(), graphPlan)
			return err
		}, targets: func(windowSeed) []string { return nil }},
		{name: "ApplyGraphPlanWithStorage", write: func(cache *CachingStore, _ windowSeed) error {
			applier, ok := cache.GraphApplyHandle()
			if !ok {
				return errors.New("GraphApplyHandle: the backing applies no graphs")
			}
			storage, ok := applier.(StorageGraphApplyStore)
			if !ok {
				return errors.New("the graph-apply handle applies no storage-classed graphs")
			}
			_, err := storage.ApplyGraphPlanWithStorage(context.Background(), graphPlan, StorageNoHistory)
			return err
		}, targets: func(windowSeed) []string { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backing := &windowStore{MemStore: NewMemStore()}
			cache, rec := newReportCacheForTest(t, backing)
			seed := windowSeed{id: mustCreateCached(t, cache).ID, other: mustCreateCached(t, cache).ID}
			if tc.setup != nil {
				tc.setup(t, cache, seed)
			}
			row, err := backing.MemStore.Get(seed.id)
			if err != nil {
				t.Fatal(err)
			}
			seed.revision = row.Revision
			targets := []string{seed.id}
			if tc.targets != nil {
				targets = tc.targets(seed)
			}
			rec.take()

			probes := 0
			var failures []string
			backing.probe = func(created ...string) {
				targets = append(targets, created...)
				for _, id := range targets {
					probes++
					if _, err := cache.RefreshRowReport(id, RowRefreshOptions{YieldToLocalWrites: true, AnnounceUnheldClose: true}); !errors.Is(err, ErrRowRefreshYielded) {
						failures = append(failures, fmt.Sprintf("refresh of %s inside the write = %v, want ErrRowRefreshYielded", id, err))
					}
				}
			}
			err = tc.write(cache, seed)
			backing.probe = nil
			if err != nil {
				t.Fatalf("write: %v", err)
			}
			if probes == 0 {
				t.Fatal("no backing call probed a row the write touches; the case is vacuous")
			}
			for _, f := range failures {
				t.Error(f)
			}
			if got := refreshNotifications(rec.take()); len(got) != 0 {
				t.Errorf("the in-window refreshes notified %q, want nothing", got)
			}
		})
	}
}

// Every exported write the local-write census lists has a row in the window
// table, so a write added later gets one too.
func TestCachingStoreYieldWindowTableCoversTheCensus(t *testing.T) {
	t.Parallel()

	covered := map[string]bool{}
	for _, name := range []string{
		"Create", "CreateWithStorage", "Update", "Close", "Reopen", "CloseAll", "SetMetadata",
		"SetMetadataBatch", "DepAdd", "DepRemove", "Delete", "DeleteBatch", "DeleteIfMatch", "Tx",
		"UpdateIfMatch", "CloseIfMatch", "CompareAndSetMetadataKey", "ReleaseIfCurrent",
		"TransferIfCurrent", "Claim", "UpdateIfAssignment",
	} {
		covered["CachingStore."+name] = true
	}
	covered["cachingAtomicConditionalCloser.CloseWithMetadataIfMatch"] = true
	covered["cachingGuardedBatcher.ApplyGuarded"] = true
	covered["cachingGraphApplyStore.ApplyGraphPlan"] = true
	_, exported := localWriteCensus(t, packageGoFiles(t))
	for _, m := range exported {
		if _, exempt := localWriteCensusExempt[m]; !exempt && !covered[m] {
			t.Errorf("%s is a write with no row in TestCachingStoreYieldCoversEveryWriteFromStartToAbsorb", m)
		}
	}
}

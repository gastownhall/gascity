package beads

import (
	"context"
	"fmt"
	"testing"
)

// The close-path x bead.closed emission matrix: every way a CachingStore can
// learn that a bead closed, crossed with every way a concurrent read can
// observe the same close before the writer claims it, must put exactly one
// bead.closed notification out for that bead (gastownhall/gascity#6860).
// beadstest.RunCloseNotificationTests covers the public paths per backing;
// this matrix drives the internal interleavings (a racing read inside the
// write's backing-write -> claim window, a dirty-row read) deterministically,
// without sleeps.

// closeMatrixBacking is a MemStore with every write capability the cache
// forwards (conditional writes, the assignment guard), and two hooks: afterWrite
// runs once right after the next backing write commits, and onGet once inside
// the next backing Get. Together they land a racing reader anywhere in a
// write's window.
type closeMatrixBacking struct {
	*MemStore
	afterWrite func()
	onGet      func()
}

func (s *closeMatrixBacking) fireAfterWrite() {
	if hook := s.afterWrite; hook != nil {
		s.afterWrite = nil
		hook()
	}
}

func (s *closeMatrixBacking) Get(id string) (Bead, error) {
	b, err := s.MemStore.Get(id)
	if hook := s.onGet; hook != nil {
		s.onGet = nil
		hook()
	}
	return b, err
}

func (s *closeMatrixBacking) Close(id string) error {
	err := s.MemStore.Close(id)
	s.fireAfterWrite()
	return err
}

func (s *closeMatrixBacking) CloseAll(ids []string, metadata map[string]string) (int, error) {
	n, err := s.MemStore.CloseAll(ids, metadata)
	s.fireAfterWrite()
	return n, err
}

func (s *closeMatrixBacking) Update(id string, opts UpdateOpts) error {
	err := s.MemStore.Update(id, opts)
	s.fireAfterWrite()
	return err
}

func (s *closeMatrixBacking) CloseIfMatch(id string, expectedRevision int64) error {
	err := s.MemStore.CloseIfMatch(id, expectedRevision)
	s.fireAfterWrite()
	return err
}

func (s *closeMatrixBacking) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	err := s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
	s.fireAfterWrite()
	return err
}

// UpdateIfAssignment emulates the assignment guard over the MemStore.
func (s *closeMatrixBacking) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts UpdateOpts) (bool, error) {
	current, err := s.MemStore.Get(id)
	if err != nil {
		return false, err
	}
	if current.Status != expectedStatus || current.Assignee != expectedAssignee {
		return false, nil
	}
	err = s.MemStore.Update(id, opts)
	s.fireAfterWrite()
	return err == nil, err
}

var (
	_ ConditionalWriter        = (*closeMatrixBacking)(nil)
	_ AssignmentGuardedUpdater = (*closeMatrixBacking)(nil)
)

// closeMatrixWriter is one way the cache itself closes a bead.
type closeMatrixWriter struct {
	name  string
	close func(cs *CachingStore, id string) error
}

func closeMatrixWriters() []closeMatrixWriter {
	closed := "closed"
	revision := func(cs *CachingStore, id string) int64 {
		b, err := cs.backing.Get(id)
		if err != nil {
			return 0
		}
		return b.Revision
	}
	return []closeMatrixWriter{
		{"Close", func(cs *CachingStore, id string) error { return cs.Close(id) }},
		{"CloseAll", func(cs *CachingStore, id string) error { _, err := cs.CloseAll([]string{id}, nil); return err }},
		{"UpdateClosed", func(cs *CachingStore, id string) error { return cs.Update(id, UpdateOpts{Status: &closed}) }},
		{"CloseIfMatch", func(cs *CachingStore, id string) error { return cs.CloseIfMatch(id, revision(cs, id)) }},
		{"UpdateIfMatchClosed", func(cs *CachingStore, id string) error {
			return cs.UpdateIfMatch(id, revision(cs, id), UpdateOpts{Status: &closed})
		}},
		{"UpdateIfAssignmentClosed", func(cs *CachingStore, id string) error {
			ok, err := cs.UpdateIfAssignment(id, "open", "", UpdateOpts{Status: &closed})
			if err == nil && !ok {
				err = fmt.Errorf("guard refused")
			}
			return err
		}},
	}
}

// closeMatrixRacer is a concurrent reader that observes the close.
type closeMatrixRacer struct {
	name string
	read func(t *testing.T, cs *CachingStore, id string)
}

func closeMatrixRacers() []closeMatrixRacer {
	return []closeMatrixRacer{
		{"LiveList", func(t *testing.T, cs *CachingStore, _ string) {
			if _, err := cs.List(ListQuery{AllowScan: true, IncludeClosed: true, Live: true}); err != nil {
				t.Errorf("racing live List: %v", err)
			}
		}},
		{"DirtyGet", func(t *testing.T, cs *CachingStore, id string) {
			cs.mu.Lock()
			cs.markDirtyLocked(id)
			cs.mu.Unlock()
			if _, err := cs.Get(id); err != nil {
				t.Errorf("racing dirty Get: %v", err)
			}
		}},
		{"RefreshRow", func(t *testing.T, cs *CachingStore, id string) {
			if _, err := cs.RefreshRow(id); err != nil {
				t.Errorf("racing RefreshRow: %v", err)
			}
		}},
		{"Reconcile", func(_ *testing.T, cs *CachingStore, _ string) {
			cs.ReconcileNowForTest()
		}},
	}
}

// newCloseMatrixCache primes a cache over a fresh closeMatrixBacking holding
// one open seed bead. With cached false the seed is created behind the cache
// after the prime, so the cache has never held it.
func newCloseMatrixCache(t *testing.T, cached bool) (*closeMatrixBacking, *CachingStore, *closeEventRecorder, Bead) {
	t.Helper()
	backing := &closeMatrixBacking{MemStore: NewMemStore()}
	var seed Bead
	var err error
	if cached {
		if seed, err = backing.Create(Bead{Title: "matrix seed", Status: "open"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	rec := &closeEventRecorder{}
	cs := NewCachingStoreForTest(backing, rec.onChange(t))
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if !cached {
		if seed, err = backing.Create(Bead{Title: "matrix seed", Status: "open"}); err != nil {
			t.Fatalf("Create behind the cache: %v", err)
		}
	}
	rec.reset()
	return backing, cs, rec, seed
}

// settleCloseMatrix drains every queued close and lets a live list and two
// reconcile passes run, so a close announced late, or twice, shows.
func settleCloseMatrix(t *testing.T, cs *CachingStore) {
	t.Helper()
	cs.announceUnannouncedCloses()
	if _, err := cs.List(ListQuery{AllowScan: true, IncludeClosed: true, Live: true}); err != nil {
		t.Fatalf("settling live List: %v", err)
	}
	cs.ReconcileNowForTest()
	cs.ReconcileNowForTest()
}

func assertOneBeadClosed(t *testing.T, rec *closeEventRecorder, id string) {
	t.Helper()
	if got := rec.count("bead.closed", id, "closed"); got != 1 {
		t.Fatalf("bead.closed notifications for %s = %d, want exactly 1; events=%s", id, got, rec)
	}
}

// assertMatrixCloseAnnounced applies the writer's contract. CloseAll is the
// one writer that announces only the closes of rows the cache held: its
// backing reports a count, not which ids it closed, and in a live cache an
// uncached id in a batch close is almost always a row already closed and
// evicted, so announcing uncached ids would re-announce old closes. A bead
// created behind the cache and batch-closed before any scan cached it is
// therefore not announced (the same blind spot as a bead created and closed
// between two scans); it must never be announced twice.
func assertMatrixCloseAnnounced(t *testing.T, rec *closeEventRecorder, w closeMatrixWriter, cached bool, id string) {
	t.Helper()
	if w.name == "CloseAll" && !cached {
		if got := rec.count("bead.closed", id, "closed"); got > 1 {
			t.Fatalf("bead.closed notifications for %s = %d, want at most 1; events=%s", id, got, rec)
		}
		return
	}
	assertOneBeadClosed(t, rec, id)
}

// TestCloseMatrixWriterAlone: each writer, cached or never-cached seed, no
// racing reader.
func TestCloseMatrixWriterAlone(t *testing.T) {
	t.Parallel()
	for _, w := range closeMatrixWriters() {
		for _, cached := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/cached=%v", w.name, cached), func(t *testing.T) {
				t.Parallel()
				_, cs, rec, seed := newCloseMatrixCache(t, cached)
				if err := w.close(cs, seed.ID); err != nil {
					t.Fatalf("%s: %v", w.name, err)
				}
				settleCloseMatrix(t, cs)
				assertMatrixCloseAnnounced(t, rec, w, cached, seed.ID)
			})
		}
	}
}

// TestCloseMatrixWriterRacedAfterBackingWrite: a reader observes the close
// right after the writer's backing write commits, before the writer claims it.
func TestCloseMatrixWriterRacedAfterBackingWrite(t *testing.T) {
	t.Parallel()
	for _, w := range closeMatrixWriters() {
		for _, r := range closeMatrixRacers() {
			for _, cached := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/cached=%v", w.name, r.name, cached), func(t *testing.T) {
					t.Parallel()
					backing, cs, rec, seed := newCloseMatrixCache(t, cached)
					backing.afterWrite = func() { r.read(t, cs, seed.ID) }
					if err := w.close(cs, seed.ID); err != nil {
						t.Fatalf("%s: %v", w.name, err)
					}
					if backing.afterWrite != nil {
						t.Fatal("the racing reader never ran; the race is vacuous")
					}
					settleCloseMatrix(t, cs)
					assertMatrixCloseAnnounced(t, rec, w, cached, seed.ID)
				})
			}
		}
	}
}

// TestCloseMatrixWriterRacedInsideRefresh: a reader observes the close inside
// the writer's post-write refresh read (the next backing Get after the write).
func TestCloseMatrixWriterRacedInsideRefresh(t *testing.T) {
	t.Parallel()
	for _, w := range closeMatrixWriters() {
		for _, r := range closeMatrixRacers() {
			for _, cached := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/cached=%v", w.name, r.name, cached), func(t *testing.T) {
					t.Parallel()
					backing, cs, rec, seed := newCloseMatrixCache(t, cached)
					backing.afterWrite = func() {
						backing.onGet = func() { r.read(t, cs, seed.ID) }
					}
					if err := w.close(cs, seed.ID); err != nil {
						t.Fatalf("%s: %v", w.name, err)
					}
					settleCloseMatrix(t, cs)
					assertMatrixCloseAnnounced(t, rec, w, cached, seed.ID)
				})
			}
		}
	}
}

// TestCloseMatrixExternalClose: a close made behind the cache (bd close, gc bd
// close) is announced exactly once by whichever reader sees it first.
func TestCloseMatrixExternalClose(t *testing.T) {
	t.Parallel()
	for _, r := range closeMatrixRacers() {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			backing, cs, rec, seed := newCloseMatrixCache(t, true)
			if err := backing.MemStore.Close(seed.ID); err != nil {
				t.Fatalf("external close: %v", err)
			}
			r.read(t, cs, seed.ID)
			settleCloseMatrix(t, cs)
			assertOneBeadClosed(t, rec, seed.ID)
		})
	}
}

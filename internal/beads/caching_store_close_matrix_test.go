package beads

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The close-path x bead.closed emission matrix: every way a CachingStore can
// learn that a bead closed, crossed with every way a concurrent read can
// observe the same close before the writer claims it, must put exactly one
// bead.closed notification out per close of that bead
// (gastownhall/gascity#6860).
// beadstest.RunCloseNotificationTests covers the public paths per backing;
// this matrix drives the internal interleavings (a racing read inside the
// write's backing-write -> claim window, a dirty-row read) deterministically,
// without sleeps.

// closeMatrixBacking is a MemStore with every write capability the cache
// forwards (conditional writes, the assignment guard), and two hooks: afterWrite
// runs once right after the next backing write commits, and onGet once inside
// the next backing Get. Together they land a racing reader anywhere in a
// write's window. getErr, when set, fails the next Get once.
type closeMatrixBacking struct {
	*MemStore
	afterWrite func()
	onGet      func()
	getErr     error
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
	if injected := s.getErr; injected != nil {
		s.getErr = nil
		return Bead{}, injected
	}
	return b, err
}

func (s *closeMatrixBacking) Close(id string) error {
	err := s.MemStore.Close(id)
	s.fireAfterWrite()
	return err
}

func (s *closeMatrixBacking) Reopen(id string) error {
	err := s.MemStore.Reopen(id)
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

// announceFirstCloseElsewhere is the in-flight hook of the reopen cases: a
// racing reader observes the first close and the drain announces it, so that
// close is announced by something other than its writer, which is still in
// flight. It reports false (after failing t) when no announcement went out,
// which would make the case vacuous.
func announceFirstCloseElsewhere(t *testing.T, cs *CachingStore, rec *closeEventRecorder, r closeMatrixRacer, id string) bool {
	t.Helper()
	r.read(t, cs, id)
	cs.announceUnannouncedCloses()
	if got := rec.count("bead.closed", id, "closed"); got != 1 {
		t.Errorf("the racing %s announced the first close %d times, want 1; events=%s", r.name, got, rec)
		return false
	}
	return true
}

// inFlightCloseAnnounceable reports whether racer r can announce the close
// writer w makes while w is still in flight. For a bead the cache never held
// it cannot when w is CloseAll, which registers no close intent (the blind spot
// assertMatrixCloseAnnounced describes), or when r is a reconcile pass, which
// announces only the close of a row it held.
func inFlightCloseAnnounceable(w closeMatrixWriter, r closeMatrixRacer, cached bool) bool {
	return cached || (w.name != "CloseAll" && r.name != "Reconcile")
}

// TestCloseMatrixCloseAfterReopenWhileFirstInFlight: the first close is
// announced elsewhere while its writer is still in flight, then a reopen
// lands and a second close commits. The second close is a new close of a bead
// the cache saw open, so it is announced too: two bead.closed in all. A close
// announced before a reopen must not swallow a close begun after it.
func TestCloseMatrixCloseAfterReopenWhileFirstInFlight(t *testing.T) {
	t.Parallel()
	for _, first := range closeMatrixWriters() {
		for _, second := range closeMatrixWriters() {
			for _, r := range closeMatrixRacers() {
				for _, cached := range []bool{true, false} {
					if !inFlightCloseAnnounceable(first, r, cached) {
						continue
					}
					t.Run(fmt.Sprintf("%s/%s/%s/cached=%v", first.name, second.name, r.name, cached), func(t *testing.T) {
						t.Parallel()
						backing, cs, rec, seed := newCloseMatrixCache(t, cached)
						backing.afterWrite = func() {
							if !announceFirstCloseElsewhere(t, cs, rec, r, seed.ID) {
								return
							}
							if err := cs.Reopen(seed.ID); err != nil {
								t.Errorf("Reopen: %v", err)
								return
							}
							if err := second.close(cs, seed.ID); err != nil {
								t.Errorf("second close %s: %v", second.name, err)
							}
						}
						if err := first.close(cs, seed.ID); err != nil {
							t.Fatalf("first close %s: %v", first.name, err)
						}
						if backing.afterWrite != nil {
							t.Fatal("the in-flight hook never ran; the race is vacuous")
						}
						settleCloseMatrix(t, cs)
						if got := rec.count("bead.closed", seed.ID, "closed"); got != 2 {
							t.Fatalf("bead.closed notifications for %s = %d, want 2 (one per close); events=%s", seed.ID, got, rec)
						}
					})
				}
			}
		}
	}
}

// TestCloseMatrixReopenBeforeInFlightClaim: the first close is announced
// elsewhere and then reopened while its writer is still in flight. The
// writer's late claim must not announce that close again: the bead is open
// now, so a second bead.closed would report a close that no longer stands.
func TestCloseMatrixReopenBeforeInFlightClaim(t *testing.T) {
	t.Parallel()
	for _, w := range closeMatrixWriters() {
		for _, r := range closeMatrixRacers() {
			for _, cached := range []bool{true, false} {
				if !inFlightCloseAnnounceable(w, r, cached) {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/cached=%v", w.name, r.name, cached), func(t *testing.T) {
					t.Parallel()
					backing, cs, rec, seed := newCloseMatrixCache(t, cached)
					backing.afterWrite = func() {
						if !announceFirstCloseElsewhere(t, cs, rec, r, seed.ID) {
							return
						}
						if err := cs.Reopen(seed.ID); err != nil {
							t.Errorf("Reopen: %v", err)
						}
					}
					if err := w.close(cs, seed.ID); err != nil {
						t.Fatalf("%s: %v", w.name, err)
					}
					if backing.afterWrite != nil {
						t.Fatal("the in-flight hook never ran; the race is vacuous")
					}
					settleCloseMatrix(t, cs)
					assertOneBeadClosed(t, rec, seed.ID)
					got, err := cs.Get(seed.ID)
					if err != nil {
						t.Fatalf("Get after the reopen: %v", err)
					}
					if got.Status != "open" {
						t.Fatalf("status after the reopen = %q, want open", got.Status)
					}
				})
			}
		}
	}
}

// readWithoutDrain is the live list racer stopped short of its drain: it
// installs what it reads, queuing any close it observes, and leaves the queue
// as a read does between its install and its deferred drain.
func readWithoutDrain(t *testing.T, cs *CachingStore) {
	t.Helper()
	query := ListQuery{AllowScan: true, IncludeClosed: true, Live: true}
	cs.mu.RLock()
	startSeq, startScan := cs.mutationSeq, cs.scanGen
	cs.mu.RUnlock()
	items, err := cs.backing.List(query)
	if err != nil {
		t.Errorf("live List without its drain: %v", err)
		return
	}
	cs.refreshCachedBeads(query, startSeq, startScan, items)
}

// TestCloseMatrixQueuedCloseAfterReopenWhileFirstInFlight: the first close is
// announced elsewhere while its writer is still in flight, then the bead is
// reopened behind the cache (RefreshRow installs the reopen) and closed again,
// and a live list has installed the second close but not yet drained it when
// the writer claims. The writer's claim declines, since its own close was
// announced, but the queued close is a new one and must still go out: two
// bead.closed in all.
func TestCloseMatrixQueuedCloseAfterReopenWhileFirstInFlight(t *testing.T) {
	t.Parallel()
	for _, w := range closeMatrixWriters() {
		for _, r := range closeMatrixRacers() {
			for _, cached := range []bool{true, false} {
				if !inFlightCloseAnnounceable(w, r, cached) {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/cached=%v", w.name, r.name, cached), func(t *testing.T) {
					t.Parallel()
					backing, cs, rec, seed := newCloseMatrixCache(t, cached)
					backing.afterWrite = func() {
						if !announceFirstCloseElsewhere(t, cs, rec, r, seed.ID) {
							return
						}
						if err := backing.MemStore.Reopen(seed.ID); err != nil {
							t.Errorf("external reopen: %v", err)
							return
						}
						if _, err := cs.RefreshRow(seed.ID); err != nil {
							t.Errorf("RefreshRow after the reopen: %v", err)
							return
						}
						if err := backing.MemStore.Close(seed.ID); err != nil {
							t.Errorf("external close: %v", err)
							return
						}
						readWithoutDrain(t, cs)
						if !cs.hasUnannouncedCloses.Load() {
							t.Error("the second close is not queued; the case is vacuous")
						}
					}
					if err := w.close(cs, seed.ID); err != nil {
						t.Fatalf("%s: %v", w.name, err)
					}
					if backing.afterWrite != nil {
						t.Fatal("the in-flight hook never ran; the race is vacuous")
					}
					settleCloseMatrix(t, cs)
					if got := rec.count("bead.closed", seed.ID, "closed"); got != 2 {
						t.Fatalf("bead.closed notifications for %s = %d, want 2 (one per close); events=%s", seed.ID, got, rec)
					}
				})
			}
		}
	}
}

// closeMatrixReopener is one way the cache itself reopens a bead.
type closeMatrixReopener struct {
	name   string
	reopen func(cs *CachingStore, id string) error
}

func closeMatrixReopeners() []closeMatrixReopener {
	open := "open"
	return []closeMatrixReopener{
		{"Reopen", func(cs *CachingStore, id string) error { return cs.Reopen(id) }},
		{"UpdateOpen", func(cs *CachingStore, id string) error { return cs.Update(id, UpdateOpts{Status: &open}) }},
		{"UpdateIfMatchOpen", func(cs *CachingStore, id string) error {
			b, err := cs.backing.Get(id)
			if err != nil {
				return err
			}
			return cs.UpdateIfMatch(id, b.Revision, UpdateOpts{Status: &open})
		}},
	}
}

// TestCloseMatrixCloseAfterUncachedReopenWhileFirstInFlight: as
// TestCloseMatrixCloseAfterReopenWhileFirstInFlight, but a reconcile pass
// evicts the row as it announces the first close, and the reopen's refresh
// read fails, so the reopen installs no row the cache could see open. The
// reopen itself still starts a new close: the second close is announced, by
// its writer or by a read that installs it first. Reconcile is not that read,
// since it installs no closed row, and CloseAll is not a second writer, since
// it announces only the closes of rows the cache held
// (assertMatrixCloseAnnounced).
func TestCloseMatrixCloseAfterUncachedReopenWhileFirstInFlight(t *testing.T) {
	t.Parallel()
	errRefresh := errors.New("injected refresh failure")
	racers := closeMatrixRacers()
	reconcile := racers[3]
	if reconcile.name != "Reconcile" {
		t.Fatalf("racer 3 is %s, want Reconcile", reconcile.name)
	}
	unraced := closeMatrixRacer{"Unraced", func(*testing.T, *CachingStore, string) {}}
	secondRacers := append([]closeMatrixRacer{unraced}, racers[:3]...)
	for _, first := range closeMatrixWriters() {
		for _, reopener := range closeMatrixReopeners() {
			for _, second := range closeMatrixWriters() {
				if second.name == "CloseAll" {
					continue
				}
				for _, r := range secondRacers {
					t.Run(fmt.Sprintf("%s/%s/%s/%s", first.name, reopener.name, second.name, r.name), func(t *testing.T) {
						t.Parallel()
						backing, cs, rec, seed := newCloseMatrixCache(t, true)
						backing.afterWrite = func() {
							if !announceFirstCloseElsewhere(t, cs, rec, reconcile, seed.ID) {
								return
							}
							backing.afterWrite = func() { backing.getErr = errRefresh }
							if err := reopener.reopen(cs, seed.ID); err != nil {
								t.Errorf("%s: %v", reopener.name, err)
								return
							}
							if backing.getErr != nil || backing.afterWrite != nil {
								t.Error("the reopen's refresh read did not fail; the case is vacuous")
								return
							}
							backing.afterWrite = func() { r.read(t, cs, seed.ID) }
							if err := second.close(cs, seed.ID); err != nil {
								t.Errorf("second close %s: %v", second.name, err)
							}
							if backing.afterWrite != nil {
								t.Error("the second close's racer never ran; the race is vacuous")
							}
						}
						if err := first.close(cs, seed.ID); err != nil {
							t.Fatalf("first close %s: %v", first.name, err)
						}
						if backing.afterWrite != nil {
							t.Fatal("the in-flight hook never ran; the race is vacuous")
						}
						settleCloseMatrix(t, cs)
						if got := rec.count("bead.closed", seed.ID, "closed"); got != 2 {
							t.Fatalf("bead.closed notifications for %s = %d, want 2 (one per close); events=%s", seed.ID, got, rec)
						}
					})
				}
			}
		}
	}
}

// TestCloseMatrixDrainAfterUninstalledReopen: a live list has queued an
// external close but not yet drained it, a reopen installs nothing (a fenced
// update whose refetch fails evicts the row), and a second close commits while
// another read drains the queue. The drained close is the one before the
// reopen, so it does not cover the second close: that one is owed its own
// bead.closed, two in all. CloseAll is not a second writer here, since it
// announces only the closes of rows the cache held
// (assertMatrixCloseAnnounced).
func TestCloseMatrixDrainAfterUninstalledReopen(t *testing.T) {
	t.Parallel()
	errRefresh := errors.New("injected refresh failure")
	open := "open"
	for _, second := range closeMatrixWriters() {
		if second.name == "CloseAll" {
			continue
		}
		t.Run(second.name, func(t *testing.T) {
			t.Parallel()
			backing, cs, rec, seed := newCloseMatrixCache(t, true)
			if err := backing.MemStore.Close(seed.ID); err != nil {
				t.Fatalf("external close: %v", err)
			}
			readWithoutDrain(t, cs)
			if !cs.hasUnannouncedCloses.Load() {
				t.Fatal("the external close is not queued; the case is vacuous")
			}
			current, err := backing.MemStore.Get(seed.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			backing.afterWrite = func() { backing.getErr = errRefresh }
			if err := cs.UpdateIfMatch(seed.ID, current.Revision, UpdateOpts{Status: &open}); err != nil {
				t.Fatalf("UpdateIfMatch open: %v", err)
			}
			if backing.getErr != nil || backing.afterWrite != nil {
				t.Fatal("the reopen's refetch did not fail; the case is vacuous")
			}
			if !cs.hasUnannouncedCloses.Load() {
				t.Fatalf("the queued close did not survive the reopen; events=%s", rec)
			}
			if got := rec.count("bead.closed", seed.ID, ""); got != 0 {
				t.Fatalf("the external close was announced %d times before the second close; the case is vacuous", got)
			}
			backing.afterWrite = func() { cs.announceUnannouncedCloses() }
			if err := second.close(cs, seed.ID); err != nil {
				t.Fatalf("second close %s: %v", second.name, err)
			}
			if backing.afterWrite != nil {
				t.Fatal("the draining read never ran; the race is vacuous")
			}
			settleCloseMatrix(t, cs)
			if got := rec.count("bead.closed", seed.ID, "closed"); got != 2 {
				t.Fatalf("bead.closed notifications for %s = %d, want 2 (one per close); events=%s", seed.ID, got, rec)
			}
		})
	}
}

// TestCloseMatrixDrainOfEndedIntentQueuedClose: as
// TestCloseMatrixDrainAfterUninstalledReopen, but the queued close was queued
// while an earlier closing write was in flight, and that write has since ended
// (the state TestCloseMatrixQueuedCloseAfterReopenWhileFirstInFlight leaves
// before its settle). The queued close belongs to an intent that no longer
// exists, so draining it covers no close of a later intent: the first close
// (announced elsewhere), the queued close, and the second writer's close are
// three bead.closed in all.
func TestCloseMatrixDrainOfEndedIntentQueuedClose(t *testing.T) {
	t.Parallel()
	errRefresh := errors.New("injected refresh failure")
	open := "open"
	liveList := closeMatrixRacers()[0]
	if liveList.name != "LiveList" {
		t.Fatalf("racer 0 is %s, want LiveList", liveList.name)
	}
	for _, second := range closeMatrixWriters() {
		if second.name == "CloseAll" {
			continue
		}
		t.Run(second.name, func(t *testing.T) {
			t.Parallel()
			backing, cs, rec, seed := newCloseMatrixCache(t, true)
			backing.afterWrite = func() {
				if !announceFirstCloseElsewhere(t, cs, rec, liveList, seed.ID) {
					return
				}
				if err := backing.MemStore.Reopen(seed.ID); err != nil {
					t.Errorf("external reopen: %v", err)
					return
				}
				if _, err := cs.RefreshRow(seed.ID); err != nil {
					t.Errorf("RefreshRow after the reopen: %v", err)
					return
				}
				if err := backing.MemStore.Close(seed.ID); err != nil {
					t.Errorf("external close: %v", err)
					return
				}
				readWithoutDrain(t, cs)
			}
			if err := cs.Close(seed.ID); err != nil {
				t.Fatalf("first close: %v", err)
			}
			if backing.afterWrite != nil {
				t.Fatal("the in-flight hook never ran; the race is vacuous")
			}
			if !cs.hasUnannouncedCloses.Load() {
				t.Fatal("the external close is not queued after the first writer ended; the case is vacuous")
			}
			cs.mu.Lock()
			intents := len(cs.closeIntents)
			cs.mu.Unlock()
			if intents != 0 {
				t.Fatalf("close intents still open after the first writer ended: %d; the case is vacuous", intents)
			}
			current, err := backing.MemStore.Get(seed.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			backing.afterWrite = func() { backing.getErr = errRefresh }
			if err := cs.UpdateIfMatch(seed.ID, current.Revision, UpdateOpts{Status: &open}); err != nil {
				t.Fatalf("UpdateIfMatch open: %v", err)
			}
			if backing.getErr != nil || backing.afterWrite != nil {
				t.Fatal("the reopen's refetch did not fail; the case is vacuous")
			}
			if !cs.hasUnannouncedCloses.Load() {
				t.Fatalf("the queued close did not survive the reopen; events=%s", rec)
			}
			if got := rec.count("bead.closed", seed.ID, "closed"); got != 1 {
				t.Fatalf("bead.closed notifications before the second close = %d, want 1 (the first close); events=%s", got, rec)
			}
			backing.afterWrite = func() { cs.announceUnannouncedCloses() }
			if err := second.close(cs, seed.ID); err != nil {
				t.Fatalf("second close %s: %v", second.name, err)
			}
			if backing.afterWrite != nil {
				t.Fatal("the draining read never ran; the race is vacuous")
			}
			settleCloseMatrix(t, cs)
			if got := rec.count("bead.closed", seed.ID, "closed"); got != 3 {
				t.Fatalf("bead.closed notifications for %s = %d, want 3 (one per close); events=%s", seed.ID, got, rec)
			}
		})
	}
}

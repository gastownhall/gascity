package beads

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// blockedParkingStore models a bd-backed store that holds rows bd records as
// blocked. mapBdStatus folds that status to "open", so such a row reads as
// Status "open" with IsBlocked nil and its raw status in SourceStatus, while a
// Live "open" list is bd's raw open (nativeListPushesRawOpen) and omits it.
// Get counts the point reads the cache's Live stale-row sweep performs.
type blockedParkingStore struct {
	Store
	mu     sync.Mutex
	parked map[string]string
	gets   map[string]int
}

func newBlockedParkingStore() *blockedParkingStore {
	return &blockedParkingStore{
		Store:  NewMemStore(),
		parked: make(map[string]string),
		gets:   make(map[string]int),
	}
}

// park records id as holding the raw bd status.
func (s *blockedParkingStore) park(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parked[id] = status
}

// unpark makes id a plain open row again, as another process's bd update does.
func (s *blockedParkingStore) unpark(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.parked, id)
}

func (s *blockedParkingStore) getCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[id]
}

// stamp sets the recorded status a bd-backed read reports for a live row.
func (s *blockedParkingStore) stamp(b Bead) Bead {
	if status, ok := s.parked[b.ID]; ok && b.Status != "closed" {
		b.SourceStatus = status
	}
	return b
}

func (s *blockedParkingStore) List(query ListQuery) ([]Bead, error) {
	rows, err := s.Store.List(query)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Bead, 0, len(rows))
	for _, row := range rows {
		if _, parked := s.parked[row.ID]; parked && query.Live && query.Status == "open" {
			continue
		}
		out = append(out, s.stamp(row))
	}
	return out, nil
}

func (s *blockedParkingStore) Get(id string) (Bead, error) {
	s.mu.Lock()
	s.gets[id]++
	s.mu.Unlock()
	row, err := s.Store.Get(id)
	if err != nil {
		return Bead{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stamp(row), nil
}

// blockedRefreshFixture is a primed cache over a store holding one plain open
// task and one task bd records as blocked, with no unmet dependency on either.
type blockedRefreshFixture struct {
	t       *testing.T
	backing *blockedParkingStore
	cache   *CachingStore
	plain   string
	blocked string
}

func newBlockedRefreshFixture(t *testing.T) *blockedRefreshFixture {
	t.Helper()
	backing := newBlockedParkingStore()
	plain, err := backing.Create(Bead{Title: "plain", Type: "task"})
	if err != nil {
		t.Fatalf("Create plain: %v", err)
	}
	blocked, err := backing.Create(Bead{Title: "blocked", Type: "task"})
	if err != nil {
		t.Fatalf("Create blocked: %v", err)
	}
	backing.park(blocked.ID, "blocked")
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	f := &blockedRefreshFixture{t: t, backing: backing, cache: cache, plain: plain.ID, blocked: blocked.ID}
	if got := f.cachedRow(f.blocked).SourceStatus; got != "blocked" {
		t.Fatalf("primed SourceStatus of %s = %q, want %q: the fixture does not hold a bd-blocked row", f.blocked, got, "blocked")
	}
	return f
}

func (f *blockedRefreshFixture) cachedRow(id string) Bead {
	f.t.Helper()
	f.cache.mu.RLock()
	defer f.cache.mu.RUnlock()
	row, ok := f.cache.beads[id]
	if !ok {
		f.t.Fatalf("%s is not cached", id)
	}
	return cloneBead(row)
}

func (f *blockedRefreshFixture) isCached(id string) bool {
	f.cache.mu.RLock()
	defer f.cache.mu.RUnlock()
	_, ok := f.cache.beads[id]
	return ok
}

// liveOpenList runs the Live "open" read that drives the stale-row sweep.
func (f *blockedRefreshFixture) liveOpenList() []Bead {
	f.t.Helper()
	rows, err := f.cache.List(ListQuery{Status: "open", Live: true, AllowScan: true})
	if err != nil {
		f.t.Fatalf("Live open List: %v", err)
	}
	return rows
}

// readyHandles are the cache's readiness readers: Ready (the c.beads scan),
// ReadyContext and ReadyJudgmentContext (cachedReadyJudgment, handles file),
// the cached handle's Ready (cachedReadyLocked, handles file) and CachedReady.
func (f *blockedRefreshFixture) readyHandles() []struct {
	name string
	read func() ([]Bead, error)
} {
	cache := f.cache
	return []struct {
		name string
		read func() ([]Bead, error)
	}{
		{"Ready", func() ([]Bead, error) { return cache.Ready() }},
		{"ReadyContext", func() ([]Bead, error) { return cache.ReadyContext(context.Background()) }},
		{"ReadyJudgmentContext", func() ([]Bead, error) {
			j, err := cache.ReadyJudgmentContext(context.Background(), ReadyQuery{TierMode: TierIssues})
			return j.Rows, err
		}},
		{"Handles().Cached.Ready", func() ([]Bead, error) { return cache.Handles().Cached.Ready() }},
		{"CachedReady", func() ([]Bead, error) {
			rows, ok := cache.CachedReady()
			if !ok {
				return nil, ErrCacheUnavailable
			}
			return rows, nil
		}},
	}
}

// assertReady fails unless every readiness handle answers from the cache with
// the plain row, and with the blocked row exactly when wantBlocked is set. A
// handle that declines fails too, so a pass is never a cache that stopped
// answering.
func (f *blockedRefreshFixture) assertReady(stage string, wantBlocked bool) {
	f.t.Helper()
	for _, h := range f.readyHandles() {
		rows, err := h.read()
		if err != nil {
			f.t.Errorf("%s (%s): %v", h.name, stage, err)
			if errors.Is(err, ErrCacheUnavailable) {
				f.t.Errorf("%s (%s) declined: the cache must still answer readiness", h.name, stage)
			}
			continue
		}
		got := sortedIDs(rows)
		if !containsID(got, f.plain) {
			f.t.Errorf("%s (%s) = %v, want it to offer the plain row %s", h.name, stage, got, f.plain)
		}
		if has := containsID(got, f.blocked); has != wantBlocked {
			f.t.Errorf("%s (%s) = %v: blocked row %s offered = %v, want %v", h.name, stage, got, f.blocked, has, wantBlocked)
		}
	}
}

// A row the backing records as blocked is not ready: bd ready withholds it, so
// no cache readiness reader may offer it while the cache holds that recorded
// status. Read back with the status open it is offered again, which proves the
// withholding is the recorded status and not the fixture (ga-1qz2dic, same
// class as ga-3mv5d3).
func TestCachingStoreReadyWithholdsBdBlockedRow(t *testing.T) {
	t.Parallel()
	f := newBlockedRefreshFixture(t)
	if got := f.cachedRow(f.blocked); got.IsBlocked != nil || got.Status != "open" {
		t.Fatalf("cached blocked row = status %q is_blocked %s, want open and null", got.Status, ptrStr(got.IsBlocked))
	}
	f.assertReady("recorded blocked", false)

	f.cache.mu.Lock()
	row := f.cache.beads[f.blocked]
	row.SourceStatus = "open"
	f.cache.beads[f.blocked] = row
	f.cache.mu.Unlock()
	f.assertReady("read back open", true)
}

// (a) A repeated Live open list makes no point read for a cached blocked row
// that list omits by design, and the row stays cached. This is the pin of the
// existing SourceStatus skip in staleLiveCacheIDs, not a new behavior.
func TestCachingStoreLiveOpenListSkipsReGetOfBlockedRow(t *testing.T) {
	t.Parallel()
	f := newBlockedRefreshFixture(t)
	before := f.backing.getCount(f.blocked)
	for i := 0; i < 3; i++ {
		rows := f.liveOpenList()
		if got := sortedIDs(rows); !containsID(got, f.plain) || containsID(got, f.blocked) {
			t.Fatalf("Live open list %d = %v, want the plain row and not the blocked one", i, got)
		}
	}
	if got := f.backing.getCount(f.blocked) - before; got != 0 {
		t.Fatalf("Live open lists made %d Gets for the blocked row, want 0", got)
	}
	if !f.isCached(f.blocked) {
		t.Fatalf("blocked row %s was dropped from the cache by the Live open lists", f.blocked)
	}
	if got := f.cachedRow(f.blocked).SourceStatus; got != "blocked" {
		t.Fatalf("cached SourceStatus = %q after Live open lists, want %q", got, "blocked")
	}
}

// (b) A Live list that names no status cannot tell a parked row from a missing
// one, so the sweep still re-reads it: the skip is scoped to a named status.
func TestCachingStoreStatuslessLiveListStillReGetsBlockedRow(t *testing.T) {
	t.Parallel()
	f := newBlockedRefreshFixture(t)
	// A status-less list from the backing that omits the row, as a store
	// that cannot see it would.
	hide := &listOmitsStore{Store: f.backing, omit: f.blocked}
	f.cache.backing = hide
	before := f.backing.getCount(f.blocked)
	if _, err := f.cache.List(ListQuery{Live: true, AllowScan: true}); err != nil {
		t.Fatalf("status-less Live List: %v", err)
	}
	if got := f.backing.getCount(f.blocked) - before; got != 1 {
		t.Fatalf("status-less Live list made %d Gets for the blocked row, want 1", got)
	}
}

// listOmitsStore drops one row from every List answer.
type listOmitsStore struct {
	Store
	omit string
}

func (s *listOmitsStore) List(query ListQuery) ([]Bead, error) {
	rows, err := s.Store.List(query)
	if err != nil {
		return nil, err
	}
	out := rows[:0:0]
	for _, row := range rows {
		if row.ID != s.omit {
			out = append(out, row)
		}
	}
	return out, nil
}

// (c) A blocked row that is since closed or deleted is not stranded by the
// skip: the periodic reconcile re-verifies it (recoverMissingFromList) and
// removes it.
func TestCachingStoreReconcileRemovesBlockedRowClosedOrDeleted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		gone func(*blockedRefreshFixture) error
	}{
		{"closed", func(f *blockedRefreshFixture) error { return f.backing.Close(f.blocked) }},
		{"deleted", func(f *blockedRefreshFixture) error { return f.backing.Delete(f.blocked) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newBlockedRefreshFixture(t)
			if err := tc.gone(f); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			f.liveOpenList()
			if !f.isCached(f.blocked) {
				t.Fatalf("a Live open list evicted %s: the skip is meant to leave it for the reconcile", f.blocked)
			}
			f.cache.ReconcileForTest()
			if f.isCached(f.blocked) && f.cachedRow(f.blocked).Status != "closed" {
				t.Fatalf("after reconcile %s is still cached as %q", f.blocked, f.cachedRow(f.blocked).Status)
			}
		})
	}
}

// (d) A cache event's status patch clears SourceStatus, since an event's
// status does not say what the store recorded. The next Live open list reads
// the row once, which restores it, and the lists after that read nothing. The
// restored row is withheld from readiness again. The gap between the patch and
// that list is accepted (ruling R4).
func TestCachingStoreEventStatusPatchRestoresBlockedRowOnNextLiveOpenList(t *testing.T) {
	t.Parallel()
	f := newBlockedRefreshFixture(t)
	// A verified event installs the backing row whole, SourceStatus included,
	// when the merged row reproduces the read. The patch clears it only when
	// the merge differs from the read in a field the event does not carry, so
	// another process's priority change, which has no event here, makes it so.
	title := "blocked, renamed"
	priority := 1
	f.backing.Store.(*MemStore).WriteRowForTest(f.blocked, func(b *Bead) {
		b.Title = title
		b.Priority = &priority
	})
	payload, err := json.Marshal(map[string]string{"id": f.blocked, "title": title, "status": "open"})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	f.cache.ApplyEvent("bead.updated", payload)
	if got := f.cachedRow(f.blocked); got.SourceStatus != "" || got.Title != title {
		t.Fatalf("after the event: SourceStatus %q title %q, want it cleared and the title patched", got.SourceStatus, got.Title)
	}

	before := f.backing.getCount(f.blocked)
	f.liveOpenList()
	if got := f.backing.getCount(f.blocked) - before; got != 1 {
		t.Fatalf("first Live open list made %d Gets for the blocked row, want exactly 1", got)
	}
	if got := f.cachedRow(f.blocked).SourceStatus; got != "blocked" {
		t.Fatalf("SourceStatus after the restoring list = %q, want %q", got, "blocked")
	}
	f.assertReady("restored", false)

	before = f.backing.getCount(f.blocked)
	f.liveOpenList()
	f.liveOpenList()
	if got := f.backing.getCount(f.blocked) - before; got != 0 {
		t.Fatalf("later Live open lists made %d Gets for the restored row, want 0", got)
	}
}

// (e) A row another process unblocks, with no event, becomes cache-ready once
// the next Live open list or reconcile reads it as open again.
func TestCachingStoreUnblockedRowBecomesReadyAfterNextRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		read func(*blockedRefreshFixture)
	}{
		{"live open list", func(f *blockedRefreshFixture) { f.liveOpenList() }},
		{"reconcile", func(f *blockedRefreshFixture) { f.cache.ReconcileForTest() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newBlockedRefreshFixture(t)
			f.assertReady("still blocked", false)
			f.backing.unpark(f.blocked)
			tc.read(f)
			f.assertReady("after "+tc.name, true)
		})
	}
}

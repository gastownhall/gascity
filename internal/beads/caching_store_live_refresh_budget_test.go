package beads

import (
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// liveRefreshHiddenStore models the status-model mismatch behind the serial
// live-list refresh: a row the backing store holds as "open" that its live
// list never returns (bd's deferred and blocked rows fold to "open" in the
// cache but are excluded by the live --status=open filter). Such a row stays
// stale on every live list, however often it is re-read. Get costs getDelay
// of (fake) time, the CLI-spawn transport the time box protects against.
type liveRefreshHiddenStore struct {
	Store
	hidden   map[string]struct{}
	getDelay time.Duration

	mu   sync.Mutex
	gets []string
}

func (s *liveRefreshHiddenStore) List(query ListQuery) ([]Bead, error) {
	rows, err := s.Store.List(query)
	if err != nil || !query.Live {
		return rows, err
	}
	visible := make([]Bead, 0, len(rows))
	for _, row := range rows {
		if _, hide := s.hidden[row.ID]; hide {
			continue
		}
		visible = append(visible, row)
	}
	return visible, nil
}

func (s *liveRefreshHiddenStore) Get(id string) (Bead, error) {
	s.mu.Lock()
	s.gets = append(s.gets, id)
	s.mu.Unlock()
	if s.getDelay > 0 {
		time.Sleep(s.getDelay) // synctest fake clock: the cost of one slow Get
	}
	return s.Store.Get(id)
}

// takeGets returns and clears the ids Get was asked for, in call order.
func (s *liveRefreshHiddenStore) takeGets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.gets
	s.gets = nil
	return out
}

// liveRefreshHiddenRows is how many stale rows the fixture hides from the live list.
const liveRefreshHiddenRows = 10

// newLiveRefreshFixture primes a cache over visible live-listed rows and
// liveRefreshHiddenRows hidden stale rows, and returns the cache, the store, and the sorted hidden
// ids. The prime's own reads are cleared from the store's Get record.
func newLiveRefreshFixture(t *testing.T, visible int, getDelay time.Duration) (*CachingStore, *liveRefreshHiddenStore, []string, []string) {
	t.Helper()
	mem := NewMemStore()
	store := &liveRefreshHiddenStore{Store: mem, hidden: map[string]struct{}{}, getDelay: getDelay}
	var visibleIDs, hiddenIDs []string
	for i := 0; i < visible; i++ {
		b, err := mem.Create(Bead{Title: fmt.Sprintf("visible-%d", i), Type: "task"})
		if err != nil {
			t.Fatalf("Create visible: %v", err)
		}
		visibleIDs = append(visibleIDs, b.ID)
	}
	for i := 0; i < liveRefreshHiddenRows; i++ {
		b, err := mem.Create(Bead{Title: fmt.Sprintf("hidden-%d", i), Type: "task"})
		if err != nil {
			t.Fatalf("Create hidden: %v", err)
		}
		store.hidden[b.ID] = struct{}{}
		hiddenIDs = append(hiddenIDs, b.ID)
	}
	sort.Strings(visibleIDs)
	sort.Strings(hiddenIDs)
	cache := NewCachingStoreForTest(store, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	store.takeGets()
	return cache, store, visibleIDs, hiddenIDs
}

func liveOpenQuery() ListQuery {
	return ListQuery{Live: true, Status: "open"}
}

// A live list's stale-row refresh is cache maintenance, so it stops starting
// Gets once its time box has passed, instead of paying one Get per stale row
// however slow each one is. The returned rows are the backing list's and are
// not affected.
func TestCachingStoreLiveListStopsStartingRefreshGetsAfterTimeBox(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const perGet = 600 * time.Millisecond
		cache, store, visibleIDs, hiddenIDs := newLiveRefreshFixture(t, 2, perGet)

		start := time.Now()
		rows, err := cache.List(liveOpenQuery())
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("List: %v", err)
		}

		if got := sortedBeadIDs(rows); !slices.Equal(got, visibleIDs) {
			t.Fatalf("live list rows = %v, want the backing list's rows %v unchanged", got, visibleIDs)
		}
		gets := store.takeGets()
		if len(gets) < 3 || len(gets) > 5 {
			t.Fatalf("refresh Gets = %d (%v), want 3-5: a 2s time box over 600ms Gets, not all %d stale rows", len(gets), gets, len(hiddenIDs))
		}
		// The box stops STARTING Gets; the Get in flight when it expires finishes.
		if elapsed > 3*time.Second {
			t.Fatalf("live list took %v, want about 2s of refresh plus at most one Get in flight", elapsed)
		}
		// The rows that were not refreshed stay exactly as the cache holds them.
		cache.mu.RLock()
		defer cache.mu.RUnlock()
		for _, id := range hiddenIDs {
			if _, ok := cache.beads[id]; !ok {
				t.Fatalf("cache dropped stale row %s that the time box never re-read", id)
			}
		}
	})
}

// Rows the refresh re-reads stay stale on the next list (the live list still
// omits them), so a truncated call must not re-read the same subset forever:
// the next call resumes after the last row the previous one refreshed, and
// the whole stale set is covered over a few calls.
func TestCachingStoreLiveListRefreshResumesWhereTheTimeBoxStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, store, _, hiddenIDs := newLiveRefreshFixture(t, 1, 600*time.Millisecond)

		var calls [][]string
		for i := 0; i < 4; i++ {
			if _, err := cache.List(liveOpenQuery()); err != nil {
				t.Fatalf("List %d: %v", i+1, err)
			}
			calls = append(calls, store.takeGets())
		}

		first, second := calls[0], calls[1]
		if len(first) == 0 || len(second) == 0 {
			t.Fatalf("refresh Gets per call = %v, want every call to refresh something", calls)
		}
		if len(first) >= len(hiddenIDs) {
			t.Fatalf("first call refreshed %d of %d stale rows, want a truncated call", len(first), len(hiddenIDs))
		}
		last := first[len(first)-1]
		at := slices.Index(hiddenIDs, last)
		if at < 0 || at+1 >= len(hiddenIDs) {
			t.Fatalf("first call ended at %q, want a stale id with a successor in %v", last, hiddenIDs)
		}
		if second[0] != hiddenIDs[at+1] {
			t.Fatalf("second call started at %q, want %q (the id after the first call's last, %q); calls = %v", second[0], hiddenIDs[at+1], last, calls)
		}
		seen := map[string]struct{}{}
		for _, ids := range calls {
			for _, id := range ids {
				seen[id] = struct{}{}
			}
		}
		for _, id := range hiddenIDs {
			if _, ok := seen[id]; !ok {
				t.Fatalf("stale row %s was never refreshed in %d calls: %v", id, len(calls), calls)
			}
		}
	})
}

// A truncating call says so once per window, through the cache's log sink,
// and is not a cache problem: ProblemCount and LastProblemAt (which back the
// reconciler's backoff) do not move.
func TestCachingStoreLiveListRefreshTruncationLogsOncePerWindowWithoutProblemStats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, _, _, _ := newLiveRefreshFixture(t, 1, 600*time.Millisecond)
		var lines []string
		cache.problemf = func(msg string) { lines = append(lines, msg) }
		before := cache.Stats()

		for i := 0; i < 3; i++ {
			if _, err := cache.List(liveOpenQuery()); err != nil {
				t.Fatalf("List %d: %v", i+1, err)
			}
		}
		if len(lines) != 1 {
			t.Fatalf("truncation log lines after 3 truncating calls in one window = %d (%q), want exactly 1", len(lines), lines)
		}

		time.Sleep(cacheProblemLogWindow + time.Second) // synctest fake clock
		if _, err := cache.List(liveOpenQuery()); err != nil {
			t.Fatalf("List after window: %v", err)
		}
		if len(lines) != 2 {
			t.Fatalf("truncation log lines after the window passed = %d (%q), want 2", len(lines), lines)
		}

		after := cache.Stats()
		if after.ProblemCount != before.ProblemCount || !after.LastProblemAt.Equal(before.LastProblemAt) {
			t.Fatalf("truncation moved problem stats: count %d -> %d, last %v -> %v; it is not a cache problem",
				before.ProblemCount, after.ProblemCount, before.LastProblemAt, after.LastProblemAt)
		}
	})
}

// A fast Get path finishes inside the budget: every stale row is refreshed
// and nothing is logged, as before the time box.
func TestCachingStoreLiveListFastRefreshIsNotTruncated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, store, _, hiddenIDs := newLiveRefreshFixture(t, 1, 0)
		var lines []string
		cache.problemf = func(msg string) { lines = append(lines, msg) }

		if _, err := cache.List(liveOpenQuery()); err != nil {
			t.Fatalf("List: %v", err)
		}
		if gets := store.takeGets(); len(gets) != len(hiddenIDs) {
			t.Fatalf("refresh Gets = %d, want all %d stale rows on a fast path", len(gets), len(hiddenIDs))
		}
		if len(lines) != 0 {
			t.Fatalf("log lines = %q, want none when the refresh was not truncated", lines)
		}
	})
}

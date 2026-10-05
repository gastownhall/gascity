package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
)

func fakeReads(fs *fsys.Fake, path string) (stats, reads int) {
	for _, c := range fs.Calls {
		if c.Path != path {
			continue
		}
		switch c.Method {
		case "Stat":
			stats++
		case "ReadFile":
			reads++
		}
	}
	return stats, reads
}

// Kills: a file read on every pass. Each reader stats its file once per pass
// and re-reads it only when the stat changes. Provider-health entries still
// age out at the pass clock between rewrites, and a corrupt suspension file
// keeps reporting its error until it changes.
func TestProviderHealthAndSuspensionReadersStatMemoized(t *testing.T) {
	const city = "/city"
	fs := fsys.NewFake()
	healthPath := filepath.Join(city, providerHealthCacheRelPath)
	writeHealth := func(status string, probedAt time.Time, mod time.Time) {
		fs.Files[healthPath] = []byte(fmt.Sprintf(`{"providers":[{"provider":"claude","status":%q,"probed_at":%d}]}`, status, probedAt.Unix()))
		fs.ModTimes[healthPath] = mod
	}
	writeHealth("unhealthy", censusNow, censusNow)

	// The pass clock is well past every write, so no load is racy.
	settled := func() time.Time { return censusNow.Add(time.Hour) }
	health := newProviderHealthReader(fs, city)
	health.memo.now = settled
	for i := 0; i < 3; i++ {
		if healthy, present := health.snapshot(censusNow).check("claude"); healthy || !present {
			t.Fatalf("pass %d: claude healthy=%v present=%v, want red", i, healthy, present)
		}
	}
	if stats, reads := fakeReads(fs, healthPath); stats != 3 || reads != 1 {
		t.Fatalf("health: %d stats, %d reads over 3 passes, want 3 and 1", stats, reads)
	}
	if healthy, present := health.snapshot(censusNow.Add(providerHealthTTL)).check("claude"); healthy || !present {
		t.Fatalf("entry exactly TTL old: healthy=%v present=%v, want still red", healthy, present)
	}
	if healthy, present := health.snapshot(censusNow.Add(providerHealthTTL + time.Second)).check("claude"); !healthy || present {
		t.Fatalf("aged entry: healthy=%v present=%v, want fail-open", healthy, present)
	}
	writeHealth("healthy", censusNow.Add(2*time.Minute), censusNow.Add(2*time.Minute))
	if healthy, present := health.snapshot(censusNow.Add(2 * time.Minute)).check("claude"); !healthy || !present {
		t.Fatalf("rewritten file: healthy=%v present=%v, want green", healthy, present)
	}
	if _, reads := fakeReads(fs, healthPath); reads != 2 {
		t.Fatalf("health: %d reads after one rewrite, want 2", reads)
	}

	suspPath := citylayout.SuspensionStateFile(city)
	susp := newSuspensionReader(fs, city)
	susp.memo.now = settled
	for i := 0; i < 2; i++ {
		if st, err := susp.state(); err != nil || st.City.Suspended != nil {
			t.Fatalf("absent suspension file: %+v, %v, want the zero state", st, err)
		}
	}
	fs.Files[suspPath] = []byte(`{"city":{"suspended":true}}`)
	fs.ModTimes[suspPath] = censusNow
	for i := 0; i < 2; i++ {
		if st, err := susp.state(); err != nil || st.City.Suspended == nil || !*st.City.Suspended {
			t.Fatalf("suspended city: %+v, %v", st, err)
		}
	}
	fs.Files[suspPath] = []byte(`{"city":`)
	fs.ModTimes[suspPath] = censusNow.Add(time.Second)
	for i := 0; i < 2; i++ {
		if _, err := susp.state(); err == nil {
			t.Fatal("corrupt suspension file read without error")
		}
	}
	if stats, reads := fakeReads(fs, suspPath); stats != 6 || reads != 3 {
		t.Fatalf("suspension: %d stats, %d reads over 6 passes and 3 file states, want 6 and 3", stats, reads)
	}
}

// Kills: a memo key missing its size or its mtime, and a same-size rewrite
// within one mtime tick served stale. A load within the filesystem's mtime
// granularity of the file's mtime is racy, so the memo reloads until a load
// lands after the window.
func TestStatMemoReloadsOnSizeMtimeAndRacyRewrite(t *testing.T) {
	const path = "/city/f"
	fs := fsys.NewFake()
	clock := censusNow
	loads := 0
	m := statMemo[string]{fs: fs, path: path, now: func() time.Time { return clock }, load: func() string {
		loads++
		return string(fs.Files[path])
	}}
	write := func(data string, mod time.Time) {
		fs.Files[path] = []byte(data)
		fs.ModTimes[path] = mod
	}
	read := func(want string, wantLoads int, when string) {
		t.Helper()
		if got := m.read(); got != want || loads != wantLoads {
			t.Fatalf("%s: read %q after %d loads, want %q after %d", when, got, loads, want, wantLoads)
		}
	}

	write("aaaa", censusNow.Add(-time.Minute))
	read("aaaa", 1, "first read")
	read("aaaa", 1, "unchanged")
	write("bbbbbb", censusNow.Add(-time.Minute))
	read("bbbbbb", 2, "same mtime, new size")
	write("cccccc", censusNow.Add(-30*time.Second))
	read("cccccc", 3, "same size, new mtime")

	// Written and loaded within one mtime tick, then rewritten at the same
	// size in that tick: the stat is unchanged, the content is not.
	write("dddddd", clock)
	read("dddddd", 4, "racy load")
	write("eeeeee", clock)
	clock = clock.Add(time.Second)
	read("eeeeee", 5, "same-size rewrite within the tick")
	clock = clock.Add(statMemoMtimeGranularity)
	read("eeeeee", 6, "first load past the window")
	read("eeeeee", 6, "settled")
}

// cacheBackingCounter counts backing list reads once armed, and fails reads
// while failing is set.
type cacheBackingCounter struct {
	*beads.MemStore
	armed   atomic.Bool
	failing atomic.Bool
	reads   atomic.Int64
}

func (b *cacheBackingCounter) List(q beads.ListQuery) ([]beads.Bead, error) {
	if b.armed.Load() {
		b.reads.Add(1)
	}
	if b.failing.Load() {
		return nil, errors.New("backing down")
	}
	return b.MemStore.List(q)
}

func (b *cacheBackingCounter) Get(id string) (beads.Bead, error) {
	if b.failing.Load() {
		return beads.Bead{}, errors.New("backing down")
	}
	return b.MemStore.Get(id)
}

// Kills: a live episode read in the pass, a strict read that declines on
// any dirty row, and an unbounded last good. On an exact leg episodes come
// from the cache through its dirty-row overlay, so a dirty episode reads
// current without a backing list; another leg reads strict from memory. A
// failed read serves the last good within cacheLagBound, and a store
// with no cache, or a last good past the bound, is an error rather than "no
// episodes".
func TestStartupHealthEpisodesReadFromSessionsCache(t *testing.T) {
	quarantined := censusNow.Add(time.Hour).Format(time.RFC3339)
	backing := &cacheBackingCounter{MemStore: censusStore(
		beads.Bead{ID: "gc-e1", Type: session.StartupHealthEpisodeType, Status: "open", Metadata: map[string]string{
			session.StartupHealthSessionNameMetadataKey:      "worker-1",
			session.StartupHealthConsecutiveMetadataKey:      "4",
			session.StartupHealthQuarantinedUntilMetadataKey: quarantined,
		}},
		censusSession("gc-1", map[string]string{"state": "asleep"}),
	)}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	backing.armed.Store(true)

	var r episodeReader
	episodes, at, err := r.read(cache, true, censusNow)
	if err != nil || !at.Equal(censusNow) || episodes["worker-1"].ConsecutiveCount != 4 || episodes["worker-1"].QuarantinedUntil.IsZero() {
		t.Fatalf("clean cache: %+v at %v, %v, want worker-1 quarantined with 4 failures read now", episodes, at, err)
	}

	// A lost conditional write leaves the episode dirty; the backing moved on.
	if err := cache.UpdateIfMatch("gc-e1", 999, beads.UpdateOpts{Metadata: map[string]string{"x": "y"}}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("UpdateIfMatch = %v, want a precondition failure", err)
	}
	if err := backing.SetMetadata("gc-e1", session.StartupHealthConsecutiveMetadataKey, "5"); err != nil {
		t.Fatal(err)
	}
	var strict episodeReader
	if _, _, err := strict.read(cache, false, censusNow); !errors.Is(err, errEpisodesUncached) {
		t.Fatalf("non-exact leg, dirty cache, no last good: err = %v, want errEpisodesUncached", err)
	}
	later := censusNow.Add(time.Minute)
	episodes, at, err = r.read(cache, true, later)
	if err != nil || !at.Equal(later) || episodes["worker-1"].ConsecutiveCount != 5 {
		t.Fatalf("exact leg, dirty episode: %+v at %v, %v, want the current count 5 read now", episodes, at, err)
	}
	if n := backing.reads.Load(); n != 0 {
		t.Fatalf("backing listed %d times, want 0 (one dirty row refreshes by Get)", n)
	}

	// The overlay refreshed the row; dirty it again, then fail the backing.
	if err := cache.UpdateIfMatch("gc-e1", 999, beads.UpdateOpts{Metadata: map[string]string{"x": "y"}}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("UpdateIfMatch = %v, want a precondition failure", err)
	}
	backing.failing.Store(true)
	if episodes, at, err = r.read(cache, true, later.Add(cacheLagBound)); err != nil || !at.Equal(later) || episodes["worker-1"].ConsecutiveCount != 5 {
		t.Fatalf("failed read within the bound: %+v at %v, %v, want the last good", episodes, at, err)
	}
	if _, _, err := r.read(cache, true, later.Add(cacheLagBound+time.Second)); !errors.Is(err, errEpisodesUncached) {
		t.Fatalf("failed read past the bound: err = %v, want errEpisodesUncached", err)
	}

	var uncached episodeReader
	if _, _, err := uncached.read(censusStore(), true, censusNow); !errors.Is(err, errEpisodesUncached) {
		t.Fatalf("uncached store: err = %v, want errEpisodesUncached", err)
	}
}

// Kills: an episode tie-break other than legacy's. LoadStartupHealthEpisode
// takes the first row of ListByMetadata, which orders newest created first,
// ties by the largest bead ID.
func TestStartupHealthEpisodesNewestCreatedWinsTiesByLargestID(t *testing.T) {
	episode := func(id, name, count string, created time.Time) beads.Bead {
		return beads.Bead{ID: id, Type: session.StartupHealthEpisodeType, Status: "open", CreatedAt: created, Metadata: map[string]string{
			session.StartupHealthSessionNameMetadataKey: name,
			session.StartupHealthConsecutiveMetadataKey: count,
		}}
	}
	cache := beads.NewCachingStoreForTest(censusStore(
		episode("gc-e1", "w", "1", censusNow.Add(-time.Hour)),
		episode("gc-e2", "w", "2", censusNow.Add(-2*time.Hour)),
		episode("gc-e3", "x", "3", censusNow.Add(-time.Hour)),
		episode("gc-e4", "x", "4", censusNow.Add(-time.Hour)),
	), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	// The cache lists in map order, so read repeatedly: an unsorted pick
	// would show.
	for i := 0; i < 20; i++ {
		var r episodeReader
		episodes, _, err := r.read(cache, true, censusNow)
		if err != nil || episodes["w"].ConsecutiveCount != 1 || episodes["x"].ConsecutiveCount != 4 {
			t.Fatalf("read %d: episodes = %+v, %v, want w from gc-e1 (newest) and x from gc-e4 (tie, largest ID)", i, episodes, err)
		}
	}
}

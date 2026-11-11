package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

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

// Kills: a strict episode read that declines on any dirty row, and a full
// backing List per pass. Episodes come through the sessions store's cache,
// so a dirty episode reads current by one Get, without a backing List.
func TestStartupHealthEpisodesReadThroughSessionsCache(t *testing.T) {
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

	episodes, err := readStartupHealthEpisodes(cache)
	if err != nil || episodes["worker-1"].ConsecutiveCount != 4 || episodes["worker-1"].QuarantinedUntil.IsZero() {
		t.Fatalf("clean cache: %+v, %v, want worker-1 quarantined with 4 failures", episodes, err)
	}

	// A lost conditional write leaves the episode dirty; the backing moved on.
	if err := cache.UpdateIfMatch("gc-e1", 999, beads.UpdateOpts{Metadata: map[string]string{"x": "y"}}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("UpdateIfMatch = %v, want a precondition failure", err)
	}
	if err := backing.SetMetadata("gc-e1", session.StartupHealthConsecutiveMetadataKey, "5"); err != nil {
		t.Fatal(err)
	}
	if episodes, err = readStartupHealthEpisodes(cache); err != nil || episodes["worker-1"].ConsecutiveCount != 5 {
		t.Fatalf("dirty episode: %+v, %v, want the current count 5", episodes, err)
	}
	if n := backing.reads.Load(); n != 0 {
		t.Fatalf("backing listed %d times, want 0 (one dirty row refreshes by Get)", n)
	}
}

// Kills: a fail-closed episode read, and a last good served past a failure.
// A failed read returns its error and no episodes, so no session reads as
// quarantined (SESS-607), even right after a good read.
func TestEpisodeReadErrorFailsOpen(t *testing.T) {
	backing := &cacheBackingCounter{MemStore: censusStore(
		beads.Bead{ID: "gc-e1", Type: session.StartupHealthEpisodeType, Status: "open", Metadata: map[string]string{
			session.StartupHealthSessionNameMetadataKey:      "worker-1",
			session.StartupHealthQuarantinedUntilMetadataKey: censusNow.Add(time.Hour).Format(time.RFC3339),
		}},
	)}
	if episodes, err := readStartupHealthEpisodes(backing); err != nil || episodes["worker-1"].QuarantinedUntil.IsZero() {
		t.Fatalf("good read: %+v, %v, want worker-1 quarantined", episodes, err)
	}
	backing.failing.Store(true)
	episodes, err := readStartupHealthEpisodes(backing)
	if err == nil || len(episodes) != 0 || !episodes["worker-1"].QuarantinedUntil.IsZero() {
		t.Fatalf("failed read: %+v, %v, want an error and no episodes", episodes, err)
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
		episodes, err := readStartupHealthEpisodes(cache)
		if err != nil || episodes["w"].ConsecutiveCount != 1 || episodes["x"].ConsecutiveCount != 4 {
			t.Fatalf("read %d: episodes = %+v, %v, want w from gc-e1 (newest) and x from gc-e4 (tie, largest ID)", i, episodes, err)
		}
	}
}

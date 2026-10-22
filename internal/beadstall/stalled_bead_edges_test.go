package beadstall

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestDetectStalledBeadMissingPriorityReportsP2 uses the configured fallback for an unset priority.
func TestDetectStalledBeadMissingPriorityReportsP2(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	updated := now.Add(-8 * time.Hour)
	got := detectStalledBead(beads.Bead{ID: "ga-target", Status: "in_progress", UpdatedAt: updated}, thresholds{
		byPriority: map[int]time.Duration{0: time.Hour, 2: 24 * time.Hour},
		fallback:   8 * time.Hour,
	}, now)
	if got.priority != 2 || got.threshold != 8*time.Hour || !got.stalled {
		t.Fatalf("missing-priority detection = %+v, want P2 at the configured 8h fallback boundary", got)
	}
	if got.beadID != "ga-target" || !got.updatedAt.Equal(updated) || got.age != 8*time.Hour {
		t.Fatalf("missing-priority detection lost identity or timestamps: %+v", got)
	}
}

// TestEpisodeStoreCorruptMetadataReturnsContext refuses malformed persisted episodes.
func TestEpisodeStoreCorruptMetadataReturnsContext(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ key, value string }{
		{key: "stalled_bead_last_known_updated_at", value: "not-a-time"},
		{key: "stalled_bead_first_stale_at", value: "not-a-time"},
		{key: "stalled_bead_priority", value: "urgent"},
		{key: "stalled_bead_alert_disposition", value: "unknown"},
	} {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()
			store := beads.NewMemStore()
			metadata := beads.StringMap{
				"stalled_bead_id":                    "ga-target",
				"stalled_bead_last_known_updated_at": "2026-10-22T04:00:00Z",
				"stalled_bead_first_stale_at":        "2026-10-22T12:00:00Z",
				"stalled_bead_priority":              "1",
				"stalled_bead_alert_disposition":     "pending",
			}
			metadata[tt.key] = tt.value
			tracker, err := store.Create(beads.Bead{Title: "corrupt episode", Type: episodeBeadType, Metadata: metadata})
			if err != nil {
				t.Fatalf("create corrupt tracker: %v", err)
			}
			_, ok, err := newEpisodeStore(store).load("ga-target")
			if err == nil || ok {
				t.Fatalf("load corrupt episode = (ok=%v, err=%v), want a visible failure", ok, err)
			}
			for _, part := range []string{"ga-target", tracker.ID, tt.key, tt.value} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("load error %q omits %q", err, part)
				}
			}
		})
	}
}

// TestEpisodeStoreClearMissingDoesNotCreateTrackingWork keeps absent episodes absent.
func TestEpisodeStoreClearMissingDoesNotCreateTrackingWork(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	episodes := newEpisodeStore(store)
	if err := episodes.clear("ga-missing"); err != nil {
		t.Fatalf("clear missing episode: %v", err)
	}
	if _, ok, err := episodes.load("ga-missing"); err != nil || ok {
		t.Fatalf("load missing episode after clear = (%v, %v), want (false, nil)", ok, err)
	}
	trackers, err := store.List(beads.ListQuery{Type: episodeBeadType, IncludeClosed: true})
	if err != nil || len(trackers) != 0 {
		t.Fatalf("tracking beads after missing clear = (%+v, %v), want none", trackers, err)
	}
}

// TestEpisodeStoreSaveAfterClearPreservesHistoryAndStartsFresh records a new stall separately.
func TestEpisodeStoreSaveAfterClearPreservesHistoryAndStartsFresh(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	episodes := newEpisodeStore(store)
	now := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	first := episode{
		beadID:             "ga-target",
		lastKnownUpdatedAt: now.Add(-8 * time.Hour),
		firstStaleAt:       now,
		priority:           1,
		alertDisposition:   alertSent,
	}
	if err := episodes.save(first); err != nil {
		t.Fatalf("save first episode: %v", err)
	}
	if err := episodes.clear(first.beadID); err != nil {
		t.Fatalf("clear first episode: %v", err)
	}
	closed, err := store.List(beads.ListQuery{Type: episodeBeadType, IncludeClosed: true})
	if err != nil || len(closed) != 1 || closed[0].Status != "closed" {
		t.Fatalf("cleared trackers = (%+v, %v), want one closed episode", closed, err)
	}
	before := closed[0]
	next := first
	next.lastKnownUpdatedAt = now
	next.firstStaleAt = now.Add(6 * time.Hour)
	next.alertDisposition = alertPending
	if err := newEpisodeStore(store).save(next); err != nil {
		t.Fatalf("save later episode: %v", err)
	}
	loaded, ok, err := newEpisodeStore(store).load(next.beadID)
	if err != nil || !ok || !reflect.DeepEqual(loaded, next) {
		t.Fatalf("later episode = (%+v, %v, %v), want (%+v, true, nil)", loaded, ok, err, next)
	}
	trackers, err := store.List(beads.ListQuery{Type: episodeBeadType, IncludeClosed: true})
	if err != nil || len(trackers) != 2 {
		t.Fatalf("episode history = (%+v, %v), want two episodes", trackers, err)
	}
	for _, tracker := range trackers {
		if tracker.ID == before.ID {
			if !reflect.DeepEqual(tracker, before) {
				t.Fatalf("closed episode history mutated:\n before: %+v\n  after: %+v", before, tracker)
			}
		} else if tracker.Status != "open" {
			t.Fatalf("new tracking bead status = %q, want open", tracker.Status)
		}
	}
}

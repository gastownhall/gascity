package beadstall

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestDefaultThresholdsCoverEveryPriorityAndFallback(t *testing.T) {
	t.Parallel()

	want := map[int]time.Duration{
		0: 2 * time.Hour,
		1: 6 * time.Hour,
		2: 24 * time.Hour,
		3: 72 * time.Hour,
	}
	limits := defaultThresholds()
	for priority, threshold := range want {
		priority, threshold := priority, threshold
		t.Run("P"+string(rune('0'+priority)), func(t *testing.T) {
			t.Parallel()
			if got := limits.forPriority(&priority); got != threshold {
				t.Fatalf("threshold for P%d = %s, want %s", priority, got, threshold)
			}
		})
	}

	unknown := 99
	for name, priority := range map[string]*int{"unconfigured": &unknown, "missing": nil} {
		t.Run(name+" priority uses P2 fallback", func(t *testing.T) {
			t.Parallel()
			if got := limits.forPriority(priority); got != 24*time.Hour {
				t.Fatalf("fallback threshold = %s, want 24h", got)
			}
		})
	}
}

func TestThresholdsAllowExplicitOverrides(t *testing.T) {
	t.Parallel()

	priority := 1
	limits := thresholds{
		byPriority: map[int]time.Duration{1: 90 * time.Minute},
		fallback:   8 * time.Hour,
	}
	if got := limits.forPriority(&priority); got != 90*time.Minute {
		t.Fatalf("configured threshold = %s, want 90m", got)
	}
	priority = 7
	if got := limits.forPriority(&priority); got != 8*time.Hour {
		t.Fatalf("configured fallback = %s, want 8h", got)
	}
}

func TestDetectStalledBeadUsesInjectedClockAndExactBoundary(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	priority := 0
	limits := defaultThresholds()
	tests := []struct {
		name      string
		status    string
		updatedAt time.Time
		stalled   bool
		wantAge   time.Duration
	}{
		{name: "fresh", status: "in_progress", updatedAt: now.Add(-2*time.Hour + time.Nanosecond), wantAge: 2*time.Hour - time.Nanosecond},
		{name: "exact boundary", status: "in_progress", updatedAt: now.Add(-2 * time.Hour), stalled: true, wantAge: 2 * time.Hour},
		{name: "stale", status: "in_progress", updatedAt: now.Add(-3 * time.Hour), stalled: true, wantAge: 3 * time.Hour},
		{name: "future timestamp", status: "in_progress", updatedAt: now.Add(time.Minute)},
		{name: "open bead", status: "open", updatedAt: now.Add(-3 * time.Hour), wantAge: 3 * time.Hour},
		{name: "closed bead", status: "closed", updatedAt: now.Add(-3 * time.Hour), wantAge: 3 * time.Hour},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := detectStalledBead(beads.Bead{
				ID:        "ga-target",
				Status:    tt.status,
				Priority:  &priority,
				UpdatedAt: tt.updatedAt,
			}, limits, now)
			if got.beadID != "ga-target" || got.priority != priority {
				t.Fatalf("identity = (%q, P%d), want (ga-target, P0)", got.beadID, got.priority)
			}
			if got.stalled != tt.stalled {
				t.Fatalf("stalled = %v, want %v (age=%s threshold=%s)", got.stalled, tt.stalled, got.age, got.threshold)
			}
			if got.age != tt.wantAge {
				t.Fatalf("age = %s, want %s", got.age, tt.wantAge)
			}
			if got.threshold != 2*time.Hour {
				t.Fatalf("threshold = %s, want 2h", got.threshold)
			}
		})
	}
}

func TestEpisodeTransitionsAlertOncePerUpdatedAtEpisode(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	updatedAt := now.Add(-3 * time.Hour)
	current := detection{
		beadID:    "ga-target",
		priority:  0,
		updatedAt: updatedAt,
		age:       3 * time.Hour,
		threshold: 2 * time.Hour,
		stalled:   true,
	}

	first := advanceEpisode(nil, current, now)
	if !first.alert || !first.changed || first.clear {
		t.Fatalf("first stale transition = %+v, want alert+changed without clear", first)
	}
	if first.episode.beadID != current.beadID || first.episode.lastKnownUpdatedAt != updatedAt || first.episode.firstStaleAt != now || first.episode.alertDisposition != alertPending {
		t.Fatalf("first episode = %+v, want target/update/first-stale/pending recorded", first.episode)
	}

	sent := first.episode
	sent.alertDisposition = alertSent
	repeated := advanceEpisode(&sent, current, now.Add(time.Hour))
	if repeated.alert || repeated.clear || repeated.changed {
		t.Fatalf("same sent episode transition = %+v, want no-op", repeated)
	}

	pendingRetry := first.episode
	retry := advanceEpisode(&pendingRetry, current, now.Add(time.Minute))
	if !retry.alert || retry.clear {
		t.Fatalf("same pending episode transition = %+v, want retry alert", retry)
	}

	changed := current
	changed.updatedAt = now.Add(-150 * time.Minute)
	next := advanceEpisode(&sent, changed, now.Add(time.Hour))
	if !next.alert || !next.changed || next.episode.lastKnownUpdatedAt != changed.updatedAt || next.episode.alertDisposition != alertPending {
		t.Fatalf("changed updated_at transition = %+v, want a new pending alert episode", next)
	}

	otherTarget := current
	otherTarget.beadID = "ga-other"
	other := advanceEpisode(&sent, otherTarget, now)
	if !other.alert || other.episode.beadID != "ga-other" {
		t.Fatalf("different target transition = %+v, want independent episode keyed by bead ID", other)
	}
}

func TestEpisodeTransitionsClearWhenStallEnds(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	prior := episode{
		beadID:             "ga-target",
		lastKnownUpdatedAt: now.Add(-3 * time.Hour),
		firstStaleAt:       now.Add(-time.Hour),
		priority:           0,
		alertDisposition:   alertSent,
	}

	for _, name := range []string{"fresh again", "left in_progress"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			current := detection{beadID: prior.beadID, updatedAt: now, stalled: false}
			got := advanceEpisode(&prior, current, now)
			if !got.clear || !got.changed || got.alert {
				t.Fatalf("transition = %+v, want clear+changed without alert", got)
			}
		})
	}

	noEpisode := advanceEpisode(nil, detection{beadID: prior.beadID}, now)
	if noEpisode.clear || noEpisode.changed || noEpisode.alert {
		t.Fatalf("fresh target without episode = %+v, want no-op", noEpisode)
	}
}

func TestEpisodeStoreRoundTripsAndClearsMetadataBackedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	episodes := newEpisodeStore(store)
	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	want := episode{
		beadID:             "ga-target",
		lastKnownUpdatedAt: now.Add(-3 * time.Hour),
		firstStaleAt:       now,
		priority:           0,
		alertDisposition:   alertPending,
	}

	if err := episodes.save(want); err != nil {
		t.Fatalf("save episode: %v", err)
	}
	reopened, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reopen file store: %v", err)
	}
	loaded, ok, err := newEpisodeStore(reopened).load(want.beadID)
	if err != nil {
		t.Fatalf("load episode: %v", err)
	}
	if !ok || !reflect.DeepEqual(loaded, want) {
		t.Fatalf("loaded episode = (%+v, %v), want (%+v, true)", loaded, ok, want)
	}

	want.alertDisposition = alertSent
	if err := newEpisodeStore(reopened).save(want); err != nil {
		t.Fatalf("upsert sent disposition: %v", err)
	}
	loaded, ok, err = newEpisodeStore(reopened).load(want.beadID)
	if err != nil || !ok || !reflect.DeepEqual(loaded, want) {
		t.Fatalf("loaded updated episode = (%+v, %v, %v), want (%+v, true, nil)", loaded, ok, err, want)
	}

	if err := newEpisodeStore(reopened).clear(want.beadID); err != nil {
		t.Fatalf("clear episode: %v", err)
	}
	clearedStore, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reopen cleared file store: %v", err)
	}
	_, ok, err = newEpisodeStore(clearedStore).load(want.beadID)
	if err != nil {
		t.Fatalf("load cleared episode: %v", err)
	}
	if ok {
		t.Fatal("cleared episode still exists")
	}
}

func TestEpisodeStoreTrackingNeverBecomesReadyOrMutatesTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	priority := 1
	target, err := store.Create(beads.Bead{
		Title:       "target work",
		Status:      "in_progress",
		Type:        "task",
		Priority:    &priority,
		Assignee:    "gascity/worker",
		Description: "operator notes must survive",
		Metadata: beads.StringMap{
			"session_id":   "session-42",
			"session_name": "worker-42",
			"custom":       "preserve-me",
		},
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	before, err := store.Get(target.ID)
	if err != nil {
		t.Fatalf("get target before episode writes: %v", err)
	}

	episodes := newEpisodeStore(store)
	value := episode{
		beadID:             target.ID,
		lastKnownUpdatedAt: before.UpdatedAt,
		firstStaleAt:       before.UpdatedAt.Add(6 * time.Hour),
		priority:           priority,
		alertDisposition:   alertPending,
	}
	if err := episodes.save(value); err != nil {
		t.Fatalf("save episode: %v", err)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("list ready work: %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("ready work = %+v, want tracking bead excluded", ready)
	}
	trackers, err := store.List(beads.ListQuery{Type: episodeBeadType, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list tracking beads: %v", err)
	}
	if len(trackers) != 1 || trackers[0].Metadata["stalled_bead_id"] != target.ID {
		t.Fatalf("tracking beads = %+v, want one metadata-keyed episode", trackers)
	}
	if err := episodes.clear(target.ID); err != nil {
		t.Fatalf("clear episode: %v", err)
	}
	after, err := store.Get(target.ID)
	if err != nil {
		t.Fatalf("get target after episode writes: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("target mutated by episode persistence:\n before: %+v\n  after: %+v", before, after)
	}
}

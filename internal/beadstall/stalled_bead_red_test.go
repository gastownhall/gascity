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
	limits := DefaultThresholds()
	for priority, threshold := range want {
		priority, threshold := priority, threshold
		t.Run("P"+string(rune('0'+priority)), func(t *testing.T) {
			t.Parallel()
			if got := limits.ForPriority(&priority); got != threshold {
				t.Fatalf("threshold for P%d = %s, want %s", priority, got, threshold)
			}
		})
	}

	unknown := 99
	for name, priority := range map[string]*int{"unconfigured": &unknown, "missing": nil} {
		t.Run(name+" priority uses P2 fallback", func(t *testing.T) {
			t.Parallel()
			if got := limits.ForPriority(priority); got != 24*time.Hour {
				t.Fatalf("fallback threshold = %s, want 24h", got)
			}
		})
	}
}

func TestThresholdsAllowExplicitOverrides(t *testing.T) {
	t.Parallel()

	priority := 1
	limits := Thresholds{
		byPriority: map[int]time.Duration{1: 90 * time.Minute},
		fallback:   8 * time.Hour,
	}
	if got := limits.ForPriority(&priority); got != 90*time.Minute {
		t.Fatalf("configured threshold = %s, want 90m", got)
	}
	priority = 7
	if got := limits.ForPriority(&priority); got != 8*time.Hour {
		t.Fatalf("configured fallback = %s, want 8h", got)
	}
}

func TestDetectStalledBeadUsesInjectedClockAndExactBoundary(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	priority := 0
	limits := DefaultThresholds()
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
			got := Detect(beads.Bead{
				ID:        "ga-target",
				Status:    tt.status,
				Priority:  &priority,
				UpdatedAt: tt.updatedAt,
			}, limits, now)
			if got.BeadID != "ga-target" || got.Priority != priority {
				t.Fatalf("identity = (%q, P%d), want (ga-target, P0)", got.BeadID, got.Priority)
			}
			if got.Stalled != tt.stalled {
				t.Fatalf("stalled = %v, want %v (age=%s threshold=%s)", got.Stalled, tt.stalled, got.Age, got.Threshold)
			}
			if got.Age != tt.wantAge {
				t.Fatalf("age = %s, want %s", got.Age, tt.wantAge)
			}
			if got.Threshold != 2*time.Hour {
				t.Fatalf("threshold = %s, want 2h", got.Threshold)
			}
		})
	}
}

func TestEpisodeTransitionsAlertOncePerUpdatedAtEpisode(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	updatedAt := now.Add(-3 * time.Hour)
	current := Detection{
		BeadID:    "ga-target",
		Priority:  0,
		UpdatedAt: updatedAt,
		Age:       3 * time.Hour,
		Threshold: 2 * time.Hour,
		Stalled:   true,
	}

	first := Advance(nil, current, now)
	if !first.Alert || !first.Changed || first.Clear {
		t.Fatalf("first stale transition = %+v, want alert+changed without clear", first)
	}
	if first.Episode.BeadID != current.BeadID || first.Episode.LastKnownUpdatedAt != updatedAt || first.Episode.FirstStaleAt != now || first.Episode.AlertDisposition != AlertPending {
		t.Fatalf("first episode = %+v, want target/update/first-stale/pending recorded", first.Episode)
	}

	sent := first.Episode
	sent.AlertDisposition = AlertSent
	repeated := Advance(&sent, current, now.Add(time.Hour))
	if repeated.Alert || repeated.Clear || repeated.Changed {
		t.Fatalf("same sent episode transition = %+v, want no-op", repeated)
	}

	pendingRetry := first.Episode
	retry := Advance(&pendingRetry, current, now.Add(time.Minute))
	if !retry.Alert || retry.Clear {
		t.Fatalf("same pending episode transition = %+v, want retry alert", retry)
	}

	changed := current
	changed.UpdatedAt = now.Add(-150 * time.Minute)
	next := Advance(&sent, changed, now.Add(time.Hour))
	if !next.Alert || !next.Changed || next.Episode.LastKnownUpdatedAt != changed.UpdatedAt || next.Episode.AlertDisposition != AlertPending {
		t.Fatalf("changed updated_at transition = %+v, want a new pending alert episode", next)
	}

	otherTarget := current
	otherTarget.BeadID = "ga-other"
	other := Advance(&sent, otherTarget, now)
	if !other.Alert || other.Episode.BeadID != "ga-other" {
		t.Fatalf("different target transition = %+v, want independent episode keyed by bead ID", other)
	}
}

func TestEpisodeTransitionsClearWhenStallEnds(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	prior := Episode{
		BeadID:             "ga-target",
		LastKnownUpdatedAt: now.Add(-3 * time.Hour),
		FirstStaleAt:       now.Add(-time.Hour),
		Priority:           0,
		AlertDisposition:   AlertSent,
	}

	for _, name := range []string{"fresh again", "left in_progress"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			current := Detection{BeadID: prior.BeadID, UpdatedAt: now, Stalled: false}
			got := Advance(&prior, current, now)
			if !got.Clear || !got.Changed || got.Alert {
				t.Fatalf("transition = %+v, want clear+changed without alert", got)
			}
		})
	}

	noEpisode := Advance(nil, Detection{BeadID: prior.BeadID}, now)
	if noEpisode.Clear || noEpisode.Changed || noEpisode.Alert {
		t.Fatalf("fresh target without episode = %+v, want no-op", noEpisode)
	}
}

func TestEpisodeStoreRoundTripsAndClearsMetadataBackedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	episodes := NewEpisodeStore(beads.WorkStore{Store: store})
	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	want := Episode{
		BeadID:             "ga-target",
		LastKnownUpdatedAt: now.Add(-3 * time.Hour),
		FirstStaleAt:       now,
		Priority:           0,
		AlertDisposition:   AlertPending,
	}

	if err := episodes.Save(want); err != nil {
		t.Fatalf("save episode: %v", err)
	}
	reopened, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reopen file store: %v", err)
	}
	loaded, ok, err := NewEpisodeStore(beads.WorkStore{Store: reopened}).Load(want.BeadID)
	if err != nil {
		t.Fatalf("load episode: %v", err)
	}
	if !ok || !reflect.DeepEqual(loaded, want) {
		t.Fatalf("loaded episode = (%+v, %v), want (%+v, true)", loaded, ok, want)
	}

	want.AlertDisposition = AlertSent
	if err := NewEpisodeStore(beads.WorkStore{Store: reopened}).Save(want); err != nil {
		t.Fatalf("upsert sent disposition: %v", err)
	}
	loaded, ok, err = NewEpisodeStore(beads.WorkStore{Store: reopened}).Load(want.BeadID)
	if err != nil || !ok || !reflect.DeepEqual(loaded, want) {
		t.Fatalf("loaded updated episode = (%+v, %v, %v), want (%+v, true, nil)", loaded, ok, err, want)
	}

	if err := NewEpisodeStore(beads.WorkStore{Store: reopened}).Clear(want.BeadID); err != nil {
		t.Fatalf("clear episode: %v", err)
	}
	clearedStore, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reopen cleared file store: %v", err)
	}
	_, ok, err = NewEpisodeStore(beads.WorkStore{Store: clearedStore}).Load(want.BeadID)
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
	status := "in_progress"
	if err := store.Update(target.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("start target work: %v", err)
	}
	store, err = beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reopen started target: %v", err)
	}
	before, err := store.Get(target.ID)
	if err != nil {
		t.Fatalf("get target before episode writes: %v", err)
	}

	episodes := NewEpisodeStore(beads.WorkStore{Store: store})
	value := Episode{
		BeadID:             target.ID,
		LastKnownUpdatedAt: before.UpdatedAt,
		FirstStaleAt:       before.UpdatedAt.Add(6 * time.Hour),
		Priority:           priority,
		AlertDisposition:   AlertPending,
	}
	if err := episodes.Save(value); err != nil {
		t.Fatalf("save episode: %v", err)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("list ready work: %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("ready work = %+v, want tracking bead excluded", ready)
	}
	trackers, err := store.List(beads.ListQuery{Type: EpisodeBeadType, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list tracking beads: %v", err)
	}
	if len(trackers) != 1 || trackers[0].Metadata["stalled_bead_id"] != target.ID {
		t.Fatalf("tracking beads = %+v, want one metadata-keyed episode", trackers)
	}
	if err := episodes.Clear(target.ID); err != nil {
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

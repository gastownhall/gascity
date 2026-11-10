package beadstall_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beadstall"
	"github.com/gastownhall/gascity/internal/config"
)

// TestResolveThresholdsLayersDefaultsOverridesAndFallback pins the one
// resolver a caller uses: built-in defaults, then [stalled_beads] overrides,
// then the fallback, with the fallback judging a bead that has no priority.
func TestResolveThresholdsLayersDefaultsOverridesAndFallback(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	priority := func(p int) *int { return &p }
	for _, tt := range []struct {
		name     string
		cfg      config.StalledBeadsConfig
		priority *int
		want     time.Duration
	}{
		{name: "defaults P0", priority: priority(0), want: 2 * time.Hour},
		{name: "defaults P1", priority: priority(1), want: 6 * time.Hour},
		{name: "defaults P2", priority: priority(2), want: 24 * time.Hour},
		{name: "defaults P3", priority: priority(3), want: 72 * time.Hour},
		{name: "defaults P4 uses the fallback", priority: priority(4), want: 24 * time.Hour},
		{name: "defaults unset priority", want: 24 * time.Hour},
		{name: "override replaces one default", cfg: config.StalledBeadsConfig{Thresholds: map[string]string{"P1": "90m"}}, priority: priority(1), want: 90 * time.Minute},
		{name: "override leaves other defaults", cfg: config.StalledBeadsConfig{Thresholds: map[string]string{"P1": "90m"}}, priority: priority(0), want: 2 * time.Hour},
		{name: "lowercase label", cfg: config.StalledBeadsConfig{Thresholds: map[string]string{"p3": "1h"}}, priority: priority(3), want: time.Hour},
		{name: "override can name P4", cfg: config.StalledBeadsConfig{Thresholds: map[string]string{"P4": "96h"}}, priority: priority(4), want: 96 * time.Hour},
		{name: "fallback applies to an unconfigured priority", cfg: config.StalledBeadsConfig{FallbackThreshold: "8h"}, priority: priority(4), want: 8 * time.Hour},
		{name: "fallback applies to an unset priority", cfg: config.StalledBeadsConfig{FallbackThreshold: "8h"}, want: 8 * time.Hour},
		{name: "fallback does not replace a configured priority", cfg: config.StalledBeadsConfig{FallbackThreshold: "8h"}, priority: priority(2), want: 24 * time.Hour},
		// A bead without a priority is reported as P2 but judged by the
		// fallback: a P2 override moves labeled P2 beads and nothing else.
		{name: "P2 override does not move an unset priority", cfg: config.StalledBeadsConfig{Thresholds: map[string]string{"P2": "48h"}}, want: 24 * time.Hour},
		{name: "P2 override moves a labeled P2", cfg: config.StalledBeadsConfig{Thresholds: map[string]string{"P2": "48h"}}, priority: priority(2), want: 48 * time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			limits, err := beadstall.ResolveThresholds(tt.cfg)
			if err != nil {
				t.Fatalf("ResolveThresholds: %v", err)
			}
			got := beadstall.Detect(beads.Bead{ID: "ga-target", Status: "in_progress", Priority: tt.priority, UpdatedAt: now.Add(-tt.want)}, limits, now)
			if got.Threshold != tt.want || !got.Stalled {
				t.Fatalf("detection = %+v, want threshold %s and stalled at exactly that age", got, tt.want)
			}
		})
	}

	t.Run("unset priority is reported as P2 and judged by the fallback", func(t *testing.T) {
		t.Parallel()
		limits, err := beadstall.ResolveThresholds(config.StalledBeadsConfig{
			Thresholds:        map[string]string{"P2": "48h"},
			FallbackThreshold: "8h",
		})
		if err != nil {
			t.Fatalf("ResolveThresholds: %v", err)
		}
		got := beadstall.Detect(beads.Bead{ID: "ga-target", Status: "in_progress", UpdatedAt: now.Add(-9 * time.Hour)}, limits, now)
		if got.Priority != 2 || got.Threshold != 8*time.Hour || !got.Stalled {
			t.Fatalf("unset-priority detection = %+v, want P2 judged by the 8h fallback", got)
		}
	})
}

// TestResolveThresholdsFailsClosedOnInvalidConfig requires the config error to
// reach the caller and no usable thresholds to come back with it.
func TestResolveThresholdsFailsClosedOnInvalidConfig(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name string
		cfg  config.StalledBeadsConfig
		key  string
	}{
		{name: "bad duration", cfg: config.StalledBeadsConfig{Thresholds: map[string]string{"P1": "soon"}}, key: "thresholds.P1"},
		{name: "bad label", cfg: config.StalledBeadsConfig{Thresholds: map[string]string{"urgent": "1h"}}, key: "urgent"},
		{name: "bad fallback", cfg: config.StalledBeadsConfig{FallbackThreshold: "-1h"}, key: "fallback_threshold"},
		{name: "case duplicate", cfg: config.StalledBeadsConfig{Thresholds: map[string]string{"P1": "1h", "p1": "2h"}}, key: "p1"},
		{name: "priority above range", cfg: config.StalledBeadsConfig{Thresholds: map[string]string{"P7": "1h"}}, key: "P7"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			limits, err := beadstall.ResolveThresholds(tt.cfg)
			if err == nil {
				t.Fatal("invalid config was accepted")
			}
			for _, part := range []string{"[stalled_beads]", tt.key} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q omits %q", err, part)
				}
			}
			// Fail closed: the zero value must never classify a bead as
			// stalled, so a caller that ignores the error raises no alarm.
			got := beadstall.Detect(beads.Bead{ID: "ga-target", Status: "in_progress", UpdatedAt: now.Add(-1000 * time.Hour)}, limits, now)
			if got.Stalled {
				t.Fatalf("thresholds returned with an error classified a bead as stalled: %+v", got)
			}
		})
	}
}

// TestExportedSurfaceDrivesAnEpisodeFromAnotherPackage walks one stall from
// detection through alert, sent and clear using only the exported API.
func TestExportedSurfaceDrivesAnEpisodeFromAnotherPackage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	limits, err := beadstall.ResolveThresholds(config.StalledBeadsConfig{})
	if err != nil {
		t.Fatalf("ResolveThresholds: %v", err)
	}
	p0 := 0
	target := beads.Bead{ID: "ga-target", Status: "in_progress", Priority: &p0, UpdatedAt: now.Add(-3 * time.Hour)}
	episodes := beadstall.NewEpisodeStore(beads.WorkStore{Store: beads.NewMemStore()})

	prior, ok, err := episodes.Load(target.ID)
	if err != nil || ok {
		t.Fatalf("Load before any stall = (%v, %v), want (false, nil)", ok, err)
	}
	var priorPtr *beadstall.Episode
	if ok {
		priorPtr = &prior
	}
	first := beadstall.Advance(priorPtr, beadstall.Detect(target, limits, now), now)
	if !first.Alert || !first.Changed || first.Clear || first.Episode.AlertDisposition != beadstall.AlertPending {
		t.Fatalf("first transition = %+v, want a pending episode and an alert", first)
	}
	if err := episodes.Save(first.Episode); err != nil {
		t.Fatalf("Save: %v", err)
	}

	sent := first.Episode
	sent.AlertDisposition = beadstall.AlertSent
	if err := episodes.Save(sent); err != nil {
		t.Fatalf("Save sent: %v", err)
	}
	loaded, ok, err := episodes.Load(target.ID)
	if err != nil || !ok || loaded != sent {
		t.Fatalf("Load = (%+v, %v, %v), want (%+v, true, nil)", loaded, ok, err, sent)
	}
	repeat := beadstall.Advance(&loaded, beadstall.Detect(target, limits, now.Add(time.Hour)), now.Add(time.Hour))
	if repeat.Alert || repeat.Changed || repeat.Clear {
		t.Fatalf("repeat transition = %+v, want a no-op for a sent episode", repeat)
	}

	target.UpdatedAt = now
	recovered := beadstall.Advance(&loaded, beadstall.Detect(target, limits, now), now)
	if !recovered.Clear || recovered.Alert {
		t.Fatalf("recovery transition = %+v, want clear without alert", recovered)
	}
	if err := episodes.Clear(target.ID); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, ok, err := episodes.Load(target.ID); err != nil || ok {
		t.Fatalf("Load after Clear = (%v, %v), want (false, nil)", ok, err)
	}
	if beadstall.EpisodeBeadType != "stalled-bead-episode" {
		t.Fatalf("EpisodeBeadType = %q, want stalled-bead-episode", beadstall.EpisodeBeadType)
	}
}

// TestEpisodeStoreLoadsTheNewestEpisodeFirst makes a duplicate tracking bead
// resolve to the most recent one, as the startup-health episode store does.
func TestEpisodeStoreLoadsTheNewestEpisodeFirst(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	now := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	for i, disposition := range []beadstall.AlertDisposition{beadstall.AlertSent, beadstall.AlertPending} {
		if _, err := store.Create(beads.Bead{Title: "duplicate episode", Type: beadstall.EpisodeBeadType, Metadata: beads.StringMap{
			"stalled_bead_id":                    "ga-target",
			"stalled_bead_last_known_updated_at": now.Add(time.Duration(i) * time.Hour).Format(time.RFC3339Nano),
			"stalled_bead_first_stale_at":        now.Format(time.RFC3339Nano),
			"stalled_bead_priority":              "1",
			"stalled_bead_alert_disposition":     string(disposition),
		}}); err != nil {
			t.Fatalf("create tracker %d: %v", i, err)
		}
	}
	got, ok, err := beadstall.NewEpisodeStore(beads.WorkStore{Store: store}).Load("ga-target")
	if err != nil || !ok {
		t.Fatalf("Load = (%+v, %v, %v), want an episode", got, ok, err)
	}
	if got.AlertDisposition != beadstall.AlertPending {
		t.Fatalf("Load returned the %q episode, want the newest (pending) one", got.AlertDisposition)
	}
}

// listIgnoringMetadata is a store whose List drops the metadata filter, which
// is how a backend that cannot push the filter down answers.
type listIgnoringMetadata struct{ beads.Store }

func (s listIgnoringMetadata) List(q beads.ListQuery) ([]beads.Bead, error) {
	q.Metadata = nil
	return s.Store.List(q)
}

// TestEpisodeStoreRefusesATrackerForAnotherBead keeps one bead's episode from
// being read, rewritten or closed as another's.
func TestEpisodeStoreRefusesATrackerForAnotherBead(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	for _, op := range []struct {
		name string
		run  func(*beadstall.EpisodeStore) error
	}{
		{name: "load", run: func(s *beadstall.EpisodeStore) error { _, _, err := s.Load("ga-asked"); return err }},
		{name: "save", run: func(s *beadstall.EpisodeStore) error {
			return s.Save(beadstall.Episode{BeadID: "ga-asked", LastKnownUpdatedAt: now, FirstStaleAt: now, AlertDisposition: beadstall.AlertPending})
		}},
		{name: "clear", run: func(s *beadstall.EpisodeStore) error { return s.Clear("ga-asked") }},
	} {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			raw := beads.NewMemStore()
			stored, err := raw.Create(beads.Bead{Title: "other episode", Type: beadstall.EpisodeBeadType, Metadata: beads.StringMap{
				"stalled_bead_id":                    "ga-other",
				"stalled_bead_last_known_updated_at": now.Format(time.RFC3339Nano),
				"stalled_bead_first_stale_at":        now.Format(time.RFC3339Nano),
				"stalled_bead_priority":              "1",
				"stalled_bead_alert_disposition":     "sent",
			}})
			if err != nil {
				t.Fatalf("create tracker: %v", err)
			}
			err = op.run(beadstall.NewEpisodeStore(beads.WorkStore{Store: listIgnoringMetadata{Store: raw}}))
			if err == nil {
				t.Fatal("a tracker for another bead was accepted")
			}
			for _, part := range []string{"ga-asked", "ga-other", stored.ID} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q omits %q", err, part)
				}
			}
			after, err := raw.Get(stored.ID)
			if err != nil {
				t.Fatalf("get tracker: %v", err)
			}
			if after.Status != stored.Status || after.Metadata["stalled_bead_alert_disposition"] != "sent" || after.Metadata["stalled_bead_id"] != "ga-other" {
				t.Fatalf("the other bead's tracker was mutated: %+v", after)
			}
		})
	}
}

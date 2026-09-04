package main

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
)

// seedStartupHealthEpisode saves ep into mem via the same session-store
// serialization path production code reads (cliSessionFrontDoor wraps mem
// verbatim absent a [beads.classes.sessions] relocation), so tests exercise
// the real metadata shape instead of a hand-rolled duplicate of it.
func seedStartupHealthEpisode(t *testing.T, mem beads.Store, ep session.StartupHealthEpisode) {
	t.Helper()
	s := session.NewStore(beads.SessionStore{Store: mem})
	if err := s.SaveStartupHealthEpisode(ep); err != nil {
		t.Fatalf("seeding startup-health episode %q: %v", ep.SessionName, err)
	}
}

func TestStartupHealthEpisodesCheckNoEpisodesIsOK(t *testing.T) {
	cityDir := t.TempDir()
	store := beads.NewMemStore()

	result := newStartupHealthEpisodesCheck(&config.City{}, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			t.Fatalf("unexpected store path %q", path)
		}
		return store, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (no episodes recorded): %#v", result.Status, result)
	}
}

func TestStartupHealthEpisodesCheckBelowThresholdIsOK(t *testing.T) {
	cityDir := t.TempDir()
	mem := beads.NewMemStore()
	seedStartupHealthEpisode(t, mem, session.StartupHealthEpisode{
		SessionName:      "chaos-worker-1",
		ConsecutiveCount: 2,
	})

	result := newStartupHealthEpisodesCheck(&config.City{}, cityDir, func(_ string) (beads.Store, error) {
		return mem, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (below wake-attempt threshold): %#v", result.Status, result)
	}
	if len(result.Details) != 0 {
		t.Errorf("Details = %v, want empty (healthy episodes must not surface)", result.Details)
	}
}

func TestStartupHealthEpisodesCheckAtThresholdIsError(t *testing.T) {
	cityDir := t.TempDir()
	mem := beads.NewMemStore()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	seedStartupHealthEpisode(t, mem, session.StartupHealthEpisode{
		SessionName:      "chaos-worker-2",
		ConsecutiveCount: 5,
		FirstFailureAt:   now.Add(-20 * time.Minute),
		LastFailureAt:    now,
		QuarantinedUntil: now.Add(5 * time.Minute),
	})

	result := newStartupHealthEpisodesCheck(&config.City{}, cityDir, func(_ string) (beads.Store, error) {
		return mem, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error (at/above wake-attempt threshold): %#v", result.Status, result)
	}
	if result.Severity != doctor.SeverityBlocking {
		t.Errorf("Severity = %v, want SeverityBlocking", result.Severity)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "chaos-worker-2") {
		t.Errorf("Details missing session name:\n%s", details)
	}
	if !strings.Contains(details, "startup_death") {
		t.Errorf("Details missing episode kind:\n%s", details)
	}
}

func TestStartupHealthEpisodesCheckRecoveredEpisodeIsOK(t *testing.T) {
	cityDir := t.TempDir()
	mem := beads.NewMemStore()
	seedStartupHealthEpisode(t, mem, session.ClearStartupHealthEpisode("chaos-worker-3"))

	result := newStartupHealthEpisodesCheck(&config.City{}, cityDir, func(_ string) (beads.Store, error) {
		return mem, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (episode cleared on successful start): %#v", result.Status, result)
	}
}

func TestStartupHealthEpisodesCheckMalformedEpisodeIsError(t *testing.T) {
	cityDir := t.TempDir()
	mem := beads.NewMemStoreFrom(0, []beads.Bead{
		{
			ID:     "SH-1",
			Title:  "Startup health: (malformed)",
			Type:   session.StartupHealthEpisodeType,
			Status: "open",
			Metadata: map[string]string{
				session.StartupHealthConsecutiveMetadataKey: "5",
			},
		},
	}, nil)

	result := newStartupHealthEpisodesCheck(&config.City{}, cityDir, func(_ string) (beads.Store, error) {
		return mem, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error (malformed episode missing session name): %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "malformed") {
		t.Errorf("Details missing malformed marker:\n%s", details)
	}
}

func TestStartupHealthEpisodesCheckDuplicateSessionNamesBothReported(t *testing.T) {
	cityDir := t.TempDir()
	mem := beads.NewMemStoreFrom(0, []beads.Bead{
		{
			ID:     "SH-1",
			Type:   session.StartupHealthEpisodeType,
			Status: "open",
			Metadata: map[string]string{
				session.StartupHealthSessionNameMetadataKey: "dup-worker",
				session.StartupHealthConsecutiveMetadataKey: "5",
			},
		},
		{
			ID:     "SH-2",
			Type:   session.StartupHealthEpisodeType,
			Status: "open",
			Metadata: map[string]string{
				session.StartupHealthSessionNameMetadataKey: "dup-worker",
				session.StartupHealthConsecutiveMetadataKey: "6",
			},
		},
	}, nil)

	result := newStartupHealthEpisodesCheck(&config.City{}, cityDir, func(_ string) (beads.Store, error) {
		return mem, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error: %#v", result.Status, result)
	}
	count := strings.Count(strings.Join(result.Details, "\n"), "dup-worker")
	if count != 2 {
		t.Errorf("\"dup-worker\" appears %d times in Details, want 2 (both duplicate episodes must be reported, no dedup)", count)
	}
}

func TestStartupHealthEpisodesCheckStoreErrorIsGraceful(t *testing.T) {
	cityDir := t.TempDir()
	boom := errors.New("store offline")

	result := newStartupHealthEpisodesCheck(&config.City{}, cityDir, func(_ string) (beads.Store, error) {
		return nil, boom
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (store outage is an infra hiccup, not a finding): %#v", result.Status, result)
	}
}

func TestStartupHealthEpisodesCheckDeterministicOrdering(t *testing.T) {
	cityDir := t.TempDir()
	mem := beads.NewMemStore()
	for _, name := range []string{"zeta", "alpha", "mu"} {
		seedStartupHealthEpisode(t, mem, session.StartupHealthEpisode{SessionName: name, ConsecutiveCount: 5})
	}

	result := newStartupHealthEpisodesCheck(&config.City{}, cityDir, func(_ string) (beads.Store, error) {
		return mem, nil
	}).Run(&doctor.CheckContext{})

	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error: %#v", result.Status, result)
	}
	if !sort.StringsAreSorted(result.Details) {
		t.Errorf("Details not sorted: %v", result.Details)
	}
}

func TestStartupHealthEpisodesCheckCanFixIsFalse(t *testing.T) {
	check := newStartupHealthEpisodesCheck(&config.City{}, t.TempDir(), nil)
	if check.CanFix() {
		t.Fatal("expected CanFix to return false; this check is detection-only")
	}
}

func TestStartupHealthEpisodesCheckWarmupEligibleIsFalse(t *testing.T) {
	check := newStartupHealthEpisodesCheck(&config.City{}, t.TempDir(), nil)
	if check.WarmupEligible() {
		t.Fatal("expected WarmupEligible to return false; this check is not part of the gc start warm-up scan")
	}
}

func TestStartupHealthEpisodesCheckNeverLeaksLastDetail(t *testing.T) {
	cityDir := t.TempDir()
	mem := beads.NewMemStore()
	const secret = "sk-secret123"
	seedStartupHealthEpisode(t, mem, session.StartupHealthEpisode{
		SessionName:      "leaky-worker",
		ConsecutiveCount: 5,
		LastDetail:       "auth failed: token " + secret,
	})

	result := newStartupHealthEpisodesCheck(&config.City{}, cityDir, func(_ string) (beads.Store, error) {
		return mem, nil
	}).Run(&doctor.CheckContext{})

	if strings.Contains(result.Message, secret) {
		t.Errorf("Message leaks LastDetail: %q", result.Message)
	}
	if strings.Contains(strings.Join(result.Details, "\n"), secret) {
		t.Errorf("Details leak LastDetail: %v", result.Details)
	}
	if strings.Contains(result.FixHint, secret) {
		t.Errorf("FixHint leaks LastDetail: %q", result.FixHint)
	}
}

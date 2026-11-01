package session

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestStartHoldUntilReturnsLaterOfQuarantineAndBackoff pins that the single
// value the start gate consults is the later of the two independent holds: the
// short self-healing retry backoff and the threshold quarantine. Neither may
// shorten the other.
func TestStartHoldUntilReturnsLaterOfQuarantineAndBackoff(t *testing.T) {
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		quarantine time.Time
		backoff    time.Time
		want       time.Time
	}{
		{"both zero", time.Time{}, time.Time{}, time.Time{}},
		{"backoff only", time.Time{}, base.Add(30 * time.Second), base.Add(30 * time.Second)},
		{"quarantine only", base.Add(5 * time.Minute), time.Time{}, base.Add(5 * time.Minute)},
		{"quarantine later", base.Add(5 * time.Minute), base.Add(4 * time.Minute), base.Add(5 * time.Minute)},
		{"backoff later", base.Add(5 * time.Minute), base.Add(30 * time.Minute), base.Add(30 * time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep := StartupHealthEpisode{QuarantinedUntil: tc.quarantine, BackoffUntil: tc.backoff}
			if got := ep.StartHoldUntil(); !got.Equal(tc.want) {
				t.Errorf("StartHoldUntil() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStartupHealthEpisodeBackoffUntilRoundTripsThroughMetadata pins that the
// backoff hold survives a save/load cycle: it is the field that bounds the
// retry cadence across controller restarts, so a projection that dropped it
// would silently restore the unbounded 65s retry loop.
func TestStartupHealthEpisodeBackoffUntilRoundTripsThroughMetadata(t *testing.T) {
	store := beads.NewMemStore()
	s := NewStore(beads.SessionStore{Store: store})
	backoffUntil := time.Date(2026, 8, 20, 12, 8, 0, 0, time.UTC)
	ep := StartupHealthEpisode{
		SessionName:      "w",
		ConsecutiveCount: 3,
		BackoffUntil:     backoffUntil,
	}
	if err := s.SaveStartupHealthEpisode(ep); err != nil {
		t.Fatalf("SaveStartupHealthEpisode: %v", err)
	}
	got, err := s.LoadStartupHealthEpisode("w")
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if !got.BackoffUntil.Equal(backoffUntil) {
		t.Errorf("BackoffUntil = %v, want %v", got.BackoffUntil, backoffUntil)
	}
}

// TestClearStartupHealthEpisodeClearsBackoffUntil pins that a successful start
// releases the backoff hold, not just the quarantine — otherwise a recovered
// session would keep being gated by a stale hold.
func TestClearStartupHealthEpisodeClearsBackoffUntil(t *testing.T) {
	store := beads.NewMemStore()
	s := NewStore(beads.SessionStore{Store: store})
	seeded := StartupHealthEpisode{
		SessionName:      "w",
		ConsecutiveCount: 4,
		BackoffUntil:     time.Date(2026, 8, 20, 12, 8, 0, 0, time.UTC),
	}
	if err := s.SaveStartupHealthEpisode(seeded); err != nil {
		t.Fatalf("SaveStartupHealthEpisode: %v", err)
	}
	if err := s.SaveStartupHealthEpisode(ClearStartupHealthEpisode("w")); err != nil {
		t.Fatalf("SaveStartupHealthEpisode(cleared): %v", err)
	}
	got, err := s.LoadStartupHealthEpisode("w")
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if !got.BackoffUntil.IsZero() {
		t.Errorf("BackoffUntil = %v after clear, want zero", got.BackoffUntil)
	}
}

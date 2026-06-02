package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// stubProviderHealthLister returns canned beads (and optional error) for
// ListByLabel, isolating loadProviderHealthSnapshot from any real store.
type stubProviderHealthLister struct {
	beads []beads.Bead
	err   error
	calls int
}

func (s *stubProviderHealthLister) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if label != providerHealthLabel {
		return nil, nil
	}
	return s.beads, nil
}

func healthBead(provider, status string, updated time.Time) beads.Bead {
	return beads.Bead{
		Status:    "open",
		Labels:    []string{providerHealthLabel},
		UpdatedAt: updated,
		Metadata: map[string]string{
			providerHealthProviderKey: provider,
			providerHealthStatusKey:   status,
		},
	}
}

func TestProviderHealthSnapshotHealthyFailsOpen(t *testing.T) {
	// A nil lister, unknown providers, and the empty provider name all resolve
	// to healthy so a missing signal can never wedge respawns.
	snap, err := loadProviderHealthSnapshot(nil)
	if err != nil {
		t.Fatalf("nil lister: unexpected error: %v", err)
	}
	if !snap.healthy("") {
		t.Error("empty provider name should be healthy")
	}
	if !snap.healthy("unknown-provider") {
		t.Error("unknown provider should be healthy (fail-open)")
	}
}

func TestProviderHealthSnapshotReadErrorFailsOpen(t *testing.T) {
	lister := &stubProviderHealthLister{err: errors.New("store down")}
	snap, err := loadProviderHealthSnapshot(lister)
	if err == nil {
		t.Fatal("expected the read error to surface")
	}
	if !snap.healthy("anything") {
		t.Error("read error should yield an all-healthy (fail-open) snapshot")
	}
}

func TestProviderHealthSnapshotMarksUnhealthy(t *testing.T) {
	now := time.Now()
	lister := &stubProviderHealthLister{beads: []beads.Bead{
		healthBead("red-provider", providerHealthStatusUnhealthy, now),
		healthBead("green-provider", providerHealthStatusHealthy, now),
	}}
	snap, err := loadProviderHealthSnapshot(lister)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.healthy("red-provider") {
		t.Error("red-provider marked unhealthy should not be healthy")
	}
	if !snap.healthy("green-provider") {
		t.Error("green-provider marked healthy should be healthy")
	}
}

func TestProviderHealthSnapshotLatestWins(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	recent := time.Now()
	// Out-of-order input: the stale unhealthy bead precedes the fresh healthy
	// one. The newest timestamp must win regardless of slice order.
	lister := &stubProviderHealthLister{beads: []beads.Bead{
		healthBead("p", providerHealthStatusUnhealthy, old),
		healthBead("p", providerHealthStatusHealthy, recent),
	}}
	snap, err := loadProviderHealthSnapshot(lister)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !snap.healthy("p") {
		t.Error("most recent bead (healthy) should win over the stale unhealthy one")
	}
}

func TestProviderHealthSnapshotClosedBeadIsHealthy(t *testing.T) {
	now := time.Now()
	closed := healthBead("p", providerHealthStatusUnhealthy, now)
	closed.Status = "closed"
	lister := &stubProviderHealthLister{beads: []beads.Bead{closed}}
	snap, err := loadProviderHealthSnapshot(lister)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !snap.healthy("p") {
		t.Error("a closed unhealthy bead means the condition resolved: healthy")
	}
}

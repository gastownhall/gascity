package doctor

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

func TestStalledBeadsCheckReportsActionableDetailsWithoutWriting(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	priority := 1
	delegate := beads.NewMemStoreFrom(10, []beads.Bead{
		{ID: "ga-z-stale", Title: "z", Status: "in_progress", Type: "task", Priority: &priority, UpdatedAt: now.Add(-7 * time.Hour)},
		{ID: "ga-a-stale", Title: "a", Status: "in_progress", Type: "task", Priority: &priority, UpdatedAt: now.Add(-6 * time.Hour)},
		{ID: "ga-fresh", Title: "fresh", Status: "in_progress", Type: "task", Priority: &priority, UpdatedAt: now.Add(-time.Hour)},
		{ID: "ga-open", Title: "open", Status: "open", Type: "task", Priority: &priority, UpdatedAt: now.Add(-100 * time.Hour)},
	}, nil)
	recording := beadstest.NewRecordingStore(delegate)
	check := newStalledBeadsCheck(recording, func() time.Time { return now })

	result := check.Run(&CheckContext{Verbose: true})
	if result.Status == StatusOK {
		t.Fatalf("status = OK, want a diagnostic for stalled beads; result=%+v", result)
	}
	joined := result.Message + "\n" + strings.Join(result.Details, "\n")
	for _, value := range []string{
		"ga-a-stale", "ga-z-stale", "P1", "6h0m0s", "7h0m0s", "threshold=6h0m0s",
	} {
		if !strings.Contains(joined, value) {
			t.Errorf("diagnostic %q does not contain %q", joined, value)
		}
	}
	if strings.Contains(joined, "ga-fresh") || strings.Contains(joined, "ga-open") {
		t.Errorf("diagnostic includes non-stalled beads: %q", joined)
	}
	if len(result.Details) < 2 || strings.Index(joined, "ga-a-stale") > strings.Index(joined, "ga-z-stale") {
		t.Errorf("diagnostic order is not stable by bead ID: details=%#v", result.Details)
	}
	if calls := recording.Calls(); len(calls) != 0 {
		t.Fatalf("doctor check mutated bead state: %+v", calls)
	}
	if check.CanFix() {
		t.Fatal("observational stalled-beads check must not offer a fix")
	}
}

func TestStalledBeadsCheckSharesExactBoundaryAndPriorityFallback(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(3, []beads.Bead{
		{ID: "ga-boundary", Status: "in_progress", Type: "task", Priority: doctorIntPointer(0), UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "ga-default", Status: "in_progress", Type: "task", UpdatedAt: now.Add(-24 * time.Hour)},
		{ID: "ga-before", Status: "in_progress", Type: "task", Priority: doctorIntPointer(0), UpdatedAt: now.Add(-2*time.Hour + time.Nanosecond)},
	}, nil)
	result := newStalledBeadsCheck(store, func() time.Time { return now }).Run(&CheckContext{Verbose: true})
	joined := result.Message + "\n" + strings.Join(result.Details, "\n")
	for _, value := range []string{"ga-boundary", "priority=P0", "threshold=2h0m0s", "ga-default", "priority=P2", "threshold=24h0m0s"} {
		if !strings.Contains(joined, value) {
			t.Errorf("boundary/fallback diagnostic %q does not contain %q", joined, value)
		}
	}
	if strings.Contains(joined, "ga-before") {
		t.Errorf("bead younger than exact threshold was reported: %q", joined)
	}
}

func TestStalledBeadsCheckReturnsOKWhenNothingIsStalled(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(2, []beads.Bead{
		{ID: "ga-fresh", Status: "in_progress", Type: "task", Priority: doctorIntPointer(0), UpdatedAt: now.Add(-time.Hour)},
		{ID: "ga-open", Status: "open", Type: "task", Priority: doctorIntPointer(0), UpdatedAt: now.Add(-100 * time.Hour)},
	}, nil)
	result := newStalledBeadsCheck(store, func() time.Time { return now }).Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; result=%+v", result.Status, result)
	}
	if !strings.Contains(strings.ToLower(result.Message), "no stalled") {
		t.Fatalf("OK message = %q, want useful no-stalls summary", result.Message)
	}
}

func TestStalledBeadsCheckSurfacesStoreErrors(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("ledger unavailable")
	store := doctorListFailStore{Store: beads.NewMemStore(), err: sentinel}
	result := newStalledBeadsCheck(store, time.Now).Run(&CheckContext{})
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; result=%+v", result.Status, result)
	}
	if !strings.Contains(result.Message+strings.Join(result.Details, ""), sentinel.Error()) {
		t.Fatalf("diagnostic = %+v, want store error %q", result, sentinel)
	}
}

func TestStalledBeadsCheckIdentityIsStable(t *testing.T) {
	t.Parallel()

	check := newStalledBeadsCheck(beads.NewMemStore(), time.Now)
	if got := check.Name(); got != "stalled-beads" {
		t.Fatalf("check name = %q, want stalled-beads", got)
	}
	if got := []bool{check.CanFix(), check.WarmupEligible()}; !reflect.DeepEqual(got, []bool{false, false}) {
		t.Fatalf("check capabilities = %v, want observational non-warmup check", got)
	}
}

func doctorIntPointer(value int) *int {
	return &value
}

type doctorListFailStore struct {
	beads.Store
	err error
}

func (s doctorListFailStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, fmt.Errorf("list in-progress beads: %w", s.err)
}

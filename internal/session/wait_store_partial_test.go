package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// partialWaitListStore returns its seeded rows alongside a beads.PartialResultError
// from List, modeling a degraded backing read where some rows parsed and some were
// skipped. It mirrors the beads/api partial-result fixture technique.
type partialWaitListStore struct {
	beads.Store
	rows []beads.Bead
}

func (s partialWaitListStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return s.rows, &beads.PartialResultError{Op: "bd list", Err: errors.New("skipped 1 corrupt wait")}
}

// hardFailWaitListStore returns a non-partial (hard) List error so the
// short-circuit-to-nil-rows path can be characterized.
type hardFailWaitListStore struct {
	beads.Store
}

func (s hardFailWaitListStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("disk is on fire")
}

// TestWaitsForSession_FoldsPartialResultRowsThrough pins the finding-3 fix on the
// session-scoped path: a PartialResultError from the backing List keeps the
// surviving rows and folds the error through (mirroring ListAllSessionBeads),
// instead of discarding reachable waits.
func TestWaitsForSession_FoldsPartialResultRowsThrough(t *testing.T) {
	wait := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "ready"})
	s := waitStoreOver(partialWaitListStore{Store: beads.NewMemStore(), rows: []beads.Bead{wait}})

	got, err := s.WaitsForSession("gc-session")
	if !beads.IsPartialResult(err) {
		t.Fatalf("err = %v, want PartialResultError folded through", err)
	}
	if len(got) != 1 || got[0].ID != "w-1" {
		t.Fatalf("waits = %+v, want the surviving w-1 row preserved", got)
	}
}

// TestListWaits_GlobalFoldsPartialResultRowsThrough pins the same fix on the
// global gc:wait scan behind ListWaits — the path the /waits handler and the CLI
// fallback take when no session filter is set.
func TestListWaits_GlobalFoldsPartialResultRowsThrough(t *testing.T) {
	wait := waitBeadFixture("w-1", "open", "gc-session", map[string]string{"state": "ready"})
	s := waitStoreOver(partialWaitListStore{Store: beads.NewMemStore(), rows: []beads.Bead{wait}})

	got, err := s.ListWaits("", "")
	if !beads.IsPartialResult(err) {
		t.Fatalf("err = %v, want PartialResultError folded through", err)
	}
	if len(got) != 1 || got[0].ID != "w-1" {
		t.Fatalf("waits = %+v, want the surviving w-1 row preserved", got)
	}
}

// TestWaitsForSession_HardErrorReturnsNilRows confirms a non-partial store error
// still short-circuits to nil rows so a total read failure is not mistaken for a
// partial success.
func TestWaitsForSession_HardErrorReturnsNilRows(t *testing.T) {
	s := waitStoreOver(hardFailWaitListStore{Store: beads.NewMemStore()})

	got, err := s.WaitsForSession("gc-session")
	if err == nil || beads.IsPartialResult(err) {
		t.Fatalf("err = %v, want a hard (non-partial) error", err)
	}
	if got != nil {
		t.Fatalf("waits = %+v, want nil rows on a hard error", got)
	}
}

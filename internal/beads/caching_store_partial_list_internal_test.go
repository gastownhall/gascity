package beads

import (
	"context"
	"errors"
	"testing"
)

func TestCachingStorePartialPrimeDoesNotClaimCompleteNonclosedList(t *testing.T) {
	t.Parallel()

	backing := counterMemStore{MemStore: NewMemStore()}
	for _, status := range []string{"open", "in_progress", "blocked"} {
		bead, err := backing.Create(Bead{Title: status})
		if err != nil {
			t.Fatalf("Create(%s): %v", status, err)
		}
		if status != "open" {
			if err := backing.Update(bead.ID, UpdateOpts{Status: &status}); err != nil {
				t.Fatalf("Update(%s): %v", status, err)
			}
		}
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	query := ListQuery{AllowScan: true}
	if cached, ok := cache.CachedList(query); ok {
		t.Fatalf("CachedList = %#v, true; want partial-cache refusal", cached)
	}
	rows, err := cache.List(query)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertAllNonclosedStatuses(t, "List", rows)
	if cached, ok := cache.CachedList(query); ok {
		t.Fatalf("CachedList after fallback = %#v, true; fallback must not promote a partial cache", cached)
	}

	count, err := cache.Count(context.Background(), query)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 3 {
		t.Fatalf("Count after partial prime = %d, want 3", count)
	}

	cached, err := cache.Handles().Cached.List(query)
	if err != nil {
		t.Fatalf("Cached.List: %v", err)
	}
	assertAllNonclosedStatuses(t, "Cached.List", cached)
}

func TestCachingStorePartialPrimeRefusesUnknownDependencyStatus(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	blocker, err := backing.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	blockedStatus := "blocked"
	if err := backing.Update(blocker.ID, UpdateOpts{Status: &blockedStatus}); err != nil {
		t.Fatalf("Update(blocker): %v", err)
	}
	candidate, err := backing.Create(Bead{Title: "candidate"})
	if err != nil {
		t.Fatalf("Create(candidate): %v", err)
	}
	if err := backing.DepAdd(candidate.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if rows, ok := cache.CachedReady(); ok {
		t.Fatalf("CachedReady = %#v, true; want refusal when a dependency status is outside the partial snapshot", rows)
	}
	if _, err := cache.cachedReadyOnly(ReadyQuery{}); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("cachedReadyOnly error = %v, want ErrCacheUnavailable", err)
	}

	ready, err := cache.Handles().Cached.Ready()
	if err != nil {
		t.Fatalf("Cached.Ready: %v", err)
	}
	for _, bead := range ready {
		if bead.ID == candidate.ID {
			t.Fatalf("Cached.Ready included %s despite live blocking dependency; ready=%v", candidate.ID, ready)
		}
	}
}

func assertAllNonclosedStatuses(t *testing.T, op string, rows []Bead) {
	t.Helper()
	if got := statusSet(rows); !got["open"] || !got["in_progress"] || !got["blocked"] || len(rows) != 3 {
		t.Fatalf("%s statuses = %v, want open, in_progress, blocked", op, got)
	}
}

func statusSet(rows []Bead) map[string]bool {
	statuses := make(map[string]bool, len(rows))
	for _, row := range rows {
		statuses[row.Status] = true
	}
	return statuses
}

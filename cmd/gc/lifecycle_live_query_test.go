package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestCollectAssignedWorkBeads_UsesCachedReadyEventStateForAssignedOpenHandoff(t *testing.T) {
	t.Parallel()

	backing := beads.NewMemStore()
	blocker, err := backing.Create(beads.Bead{
		Title:  "blocker",
		Type:   "task",
		Status: "open",
	})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	handoff, err := backing.Create(beads.Bead{
		Title:    "handoff",
		Type:     "task",
		Status:   "open",
		Assignee: "worker",
	})
	if err != nil {
		t.Fatalf("Create(handoff): %v", err)
	}
	if err := backing.DepAdd(handoff.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(%s <- %s): %v", handoff.ID, blocker.ID, err)
	}

	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if _, err := cache.DepList(handoff.ID, "down"); err != nil {
		t.Fatalf("DepList prime(%s): %v", handoff.ID, err)
	}

	closed := "closed"
	if err := backing.Update(blocker.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("Update(%s, closed): %v", blocker.ID, err)
	}
	closedBlocker, err := backing.Get(blocker.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", blocker.ID, err)
	}
	payload, err := json.Marshal(closedBlocker)
	if err != nil {
		t.Fatalf("Marshal(%s): %v", blocker.ID, err)
	}
	cache.ApplyEvent("bead.updated", payload)

	got, _ := collectAssignedWorkBeads(&config.City{}, cache)
	if len(got) != 1 || got[0].ID != handoff.ID {
		t.Fatalf("collectAssignedWorkBeads() = %#v, want [%s] from cached ready event state", got, handoff.ID)
	}
}

func TestCollectAssignedWorkBeads_UsesExplicitDepEventsForCachedReady(t *testing.T) {
	t.Parallel()

	t.Run("dep add", func(t *testing.T) {
		t.Parallel()

		backing := beads.NewMemStore()
		blocker, err := backing.Create(beads.Bead{
			Title:  "blocker",
			Type:   "task",
			Status: "open",
		})
		if err != nil {
			t.Fatalf("Create(blocker): %v", err)
		}
		handoff, err := backing.Create(beads.Bead{
			Title:    "handoff",
			Type:     "task",
			Status:   "open",
			Assignee: "worker",
		})
		if err != nil {
			t.Fatalf("Create(handoff): %v", err)
		}
		cache := beads.NewCachingStoreForTest(backing, nil)
		if err := cache.PrimeActive(); err != nil {
			t.Fatalf("PrimeActive: %v", err)
		}

		if err := backing.DepAdd(handoff.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("backing DepAdd(%s <- %s): %v", handoff.ID, blocker.ID, err)
		}
		cache.ApplyDepEvent(handoff.ID, []beads.Dep{{IssueID: handoff.ID, DependsOnID: blocker.ID, Type: "blocks"}})

		got, _ := collectAssignedWorkBeads(&config.City{}, cache)
		if len(got) != 0 {
			t.Fatalf("collectAssignedWorkBeads() = %#v, want explicit dep-add event to block handoff", got)
		}
	})

	t.Run("dep remove", func(t *testing.T) {
		t.Parallel()

		backing := beads.NewMemStore()
		blocker, err := backing.Create(beads.Bead{
			Title:  "blocker",
			Type:   "task",
			Status: "open",
		})
		if err != nil {
			t.Fatalf("Create(blocker): %v", err)
		}
		handoff, err := backing.Create(beads.Bead{
			Title:    "handoff",
			Type:     "task",
			Status:   "open",
			Assignee: "worker",
		})
		if err != nil {
			t.Fatalf("Create(handoff): %v", err)
		}
		if err := backing.DepAdd(handoff.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("backing DepAdd(%s <- %s): %v", handoff.ID, blocker.ID, err)
		}
		cache := beads.NewCachingStoreForTest(backing, nil)
		if err := cache.PrimeActive(); err != nil {
			t.Fatalf("PrimeActive: %v", err)
		}

		if err := backing.DepRemove(handoff.ID, blocker.ID); err != nil {
			t.Fatalf("backing DepRemove(%s <- %s): %v", handoff.ID, blocker.ID, err)
		}
		cache.ApplyDepEvent(handoff.ID, nil)

		got, _ := collectAssignedWorkBeads(&config.City{}, cache)
		if len(got) != 1 || got[0].ID != handoff.ID {
			t.Fatalf("collectAssignedWorkBeads() = %#v, want [%s] after explicit dep-remove event", got, handoff.ID)
		}
	})
}

func TestSessionHasOpenAssignedWorkInStore_UsesLiveOpenOwnership(t *testing.T) {
	t.Parallel()

	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{
		Title:    "open work",
		Type:     "task",
		Status:   "open",
		Assignee: "sess-1",
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}

	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	empty := ""
	if err := backing.Update(work.ID, beads.UpdateOpts{Assignee: &empty}); err != nil {
		t.Fatalf("Update(%s, unassign): %v", work.ID, err)
	}

	session := beads.Bead{ID: "sess-1"}
	hasAssignedWork, err := sessionHasOpenAssignedWorkInStore(cache, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkInStore: %v", err)
	}
	if hasAssignedWork {
		t.Fatal("sessionHasOpenAssignedWorkInStore() = true, want false after external open-work reassignment")
	}
}

type failLiveWispListStore struct {
	beads.Store
	mu   sync.Mutex
	fail bool
}

func (s *failLiveWispListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail && query.Live && (query.TierMode == beads.TierWisps || query.TierMode == beads.TierBoth) {
		return nil, errors.New("live wisp list should not be required")
	}
	return s.Store.List(query)
}

func (s *failLiveWispListStore) setFail(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

func TestSessionHasOpenAssignedWorkInStore_UsesCachedWispOwnership(t *testing.T) {
	t.Parallel()

	backing := &failLiveWispListStore{Store: beads.NewMemStore()}
	if _, err := backing.Create(beads.Bead{
		Title:     "wisp work",
		Type:      "task",
		Status:    "in_progress",
		Assignee:  "sess-1",
		Ephemeral: true,
	}); err != nil {
		t.Fatalf("Create(wisp): %v", err)
	}

	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	backing.setFail(true)

	session := beads.Bead{ID: "sess-1"}
	hasAssignedWork, err := sessionHasOpenAssignedWorkInStore(cache, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkInStore: %v", err)
	}
	if !hasAssignedWork {
		t.Fatal("sessionHasOpenAssignedWorkInStore() = false, want cached wisp ownership to count")
	}
}

func TestSessionHasOpenAssignedWorkInStore_FallsBackToLiveForCachedWispMiss(t *testing.T) {
	t.Parallel()

	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	wisp, err := backing.Create(beads.Bead{
		Title:     "new wisp work",
		Type:      "task",
		Status:    "in_progress",
		Assignee:  "sess-1",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(wisp): %v", err)
	}

	session := beads.Bead{ID: "sess-1"}
	hasAssignedWork, err := sessionHasOpenAssignedWorkInStore(cache, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkInStore: %v", err)
	}
	if !hasAssignedWork {
		t.Fatalf("sessionHasOpenAssignedWorkInStore() = false, want live wisp %s after cached miss", wisp.ID)
	}
}

func TestUnclaimWorkAssignedToRetiredSessionBead_UsesLiveOpenOwnership(t *testing.T) {
	t.Parallel()

	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{
		Title:    "open work",
		Type:     "task",
		Status:   "open",
		Assignee: "retired-session",
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}

	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	reassigned := "replacement-session"
	if err := backing.Update(work.ID, beads.UpdateOpts{Assignee: &reassigned}); err != nil {
		t.Fatalf("Update(%s, reassigned): %v", work.ID, err)
	}

	unclaimWorkAssignedToRetiredSessionBead(
		cache,
		nil,
		beads.Bead{ID: "retired-session"},
		"worker",
		io.Discard,
	)

	got, err := backing.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", work.ID, err)
	}
	if got.Assignee != reassigned {
		t.Fatalf("Assignee = %q, want %q; stale open ownership should not be cleared", got.Assignee, reassigned)
	}
}

func TestUnclaimWorkAssignedToRetiredSessionBead_IncludesEphemeralWork(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:     "wisp work",
		Type:      "task",
		Status:    "in_progress",
		Assignee:  "retired-session",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}

	unclaimWorkAssignedToRetiredSessionBead(
		store,
		nil,
		beads.Bead{ID: "retired-session"},
		"worker",
		io.Discard,
	)

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", work.ID, err)
	}
	if got.Status != "open" {
		t.Fatalf("Status = %q, want open", got.Status)
	}
	if got.Assignee != "" {
		t.Fatalf("Assignee = %q, want empty", got.Assignee)
	}
	if got.Metadata["gc.routed_to"] != "worker" {
		t.Fatalf("gc.routed_to = %q, want worker", got.Metadata["gc.routed_to"])
	}
}

func TestReassignWorkAssignedToRetiredSessionBead_IncludesEphemeralWork(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{
		Title:     "wisp work",
		Type:      "task",
		Status:    "in_progress",
		Assignee:  "retired-session",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}

	reassignWorkAssignedToRetiredSessionBead(
		store,
		nil,
		beads.Bead{ID: "retired-session"},
		"replacement-session",
		io.Discard,
	)

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", work.ID, err)
	}
	if got.Assignee != "replacement-session" {
		t.Fatalf("Assignee = %q, want replacement-session", got.Assignee)
	}
}

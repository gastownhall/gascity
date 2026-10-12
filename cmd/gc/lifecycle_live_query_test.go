package main

import (
	"context"
	"encoding/json"
	"io"
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

// depEventPayload is a bead.updated payload carrying b with an explicit edge
// set, empty included, the shape a class binding's event has.
func depEventPayload(t *testing.T, b beads.Bead, deps []beads.Dep) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(struct {
		beads.Bead
		Dependencies []beads.Dep `json:"dependencies"`
	}{b, deps})
	if err != nil {
		t.Fatalf("Marshal(%s): %v", b.ID, err)
	}
	return payload
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
		cache.ApplyEvent("bead.updated", depEventPayload(t, handoff, []beads.Dep{{IssueID: handoff.ID, DependsOnID: blocker.ID, Type: "blocks"}}))

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
		cache.ApplyEvent("bead.updated", depEventPayload(t, handoff, []beads.Dep{}))

		got, _ := collectAssignedWorkBeads(&config.City{}, cache)
		if len(got) != 1 || got[0].ID != handoff.ID {
			t.Fatalf("collectAssignedWorkBeads() = %#v, want [%s] after explicit dep-remove event", got, handoff.ID)
		}
	})
}

func TestSessionHasOpenAssignedWorkForConfig_UsesLiveOpenOwnership(t *testing.T) {
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

	// SeatWork lists live, so the cache's stale ownership does not answer.
	session := beads.Bead{ID: "sess-1"}
	hasAssignedWork, err := sessionHasOpenAssignedWorkForConfig(testSeatWork("", nil, cache, nil), session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForConfig: %v", err)
	}
	if hasAssignedWork {
		t.Fatal("sessionHasOpenAssignedWorkForConfig() = true, want false after external open-work reassignment")
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
		testWorkLegs("", nil, cache, nil), cache,
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
		testWorkLegs("", servingCity("worker"), store, nil), store,
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
	if got.Metadata["gc.run_target"] != "worker" {
		t.Fatalf("gc.run_target = %q, want worker", got.Metadata["gc.run_target"])
	}
	if got.Metadata["gc.routed_to"] != "" {
		t.Fatalf("gc.routed_to = %q, want empty canonical route fallback", got.Metadata["gc.routed_to"])
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
		testWorkLegs("", nil, store, nil), store,
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

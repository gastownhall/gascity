package graphv2

import (
	"time"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
)

// gk-3n3jk dedup veto (mayor word-said, ci-wisp-pvaz7t 2026-11-02T08:03Z):
// every observed re-pour of parked work presented first as a second synthetic
// "input convoy for <bead>" minted beside the still-open first one (gk-f4gr
// 3x, the gk-bx4v convoy pair, the second 5ryve convoy, the h8sdl/ojstx
// intake re-fires). The mint edge must reuse the open container instead, so
// a re-fire — from any caller or identity — is idempotent there.

func dedupStore(convoys []beads.Bead) *beads.MemStore {
	deps := make([]beads.Dep, 0, len(convoys))
	for _, c := range convoys {
		deps = append(deps, beads.Dep{IssueID: c.ID, DependsOnID: "TASK-1", Type: convoy.TrackingDepType})
	}
	all := append([]beads.Bead{{ID: "TASK-1", Type: "task", Status: "open"}}, convoys...)
	return beads.NewMemStoreFrom(1, all, deps)
}

func TestNormalizeInputConvoyReusesOpenConvoy(t *testing.T) {
	store := dedupStore([]beads.Bead{
		{ID: "CVY-OLD", Type: "convoy", Status: "open", Title: "input convoy for TASK-1"},
	})
	got, err := NormalizeInputConvoy(store, "TASK-1")
	if err != nil {
		t.Fatalf("NormalizeInputConvoy: %v", err)
	}
	if got != "CVY-OLD" {
		t.Fatalf("convoy id = %q, want the existing open container CVY-OLD, not a second mint", got)
	}
	after, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("store holds %d beads after the call, want the original 2: a re-fire must not mint beside an open convoy", len(after))
	}
}

func TestNormalizeInputConvoyPrefersOldestOpenConvoy(t *testing.T) {
	// MemStore seeds every bead with the same clock, so the age order the
	// veto relies on must be stated explicitly here.
	base := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	store := dedupStore([]beads.Bead{
		{ID: "CVY-OLD", Type: "convoy", Status: "open", CreatedAt: base},
		{ID: "CVY-NEW", Type: "convoy", Status: "open", CreatedAt: base.Add(time.Hour)},
	})
	got, err := NormalizeInputConvoy(store, "TASK-1")
	if err != nil {
		t.Fatalf("NormalizeInputConvoy: %v", err)
	}
	if got != "CVY-OLD" {
		t.Fatalf("convoy id = %q, want the oldest open container CVY-OLD (TrackingConvoysForItem sorts created_at ascending)", got)
	}
}

func TestNormalizeInputConvoyMintsWhenExistingConvoyClosed(t *testing.T) {
	store := dedupStore([]beads.Bead{
		{ID: "CVY-DONE", Type: "convoy", Status: "closed"},
	})
	got, err := NormalizeInputConvoy(store, "TASK-1")
	if err != nil {
		t.Fatalf("NormalizeInputConvoy: %v", err)
	}
	if got == "" || got == "CVY-DONE" {
		t.Fatalf("convoy id = %q, want a fresh mint: a closed container is not a brake on a new pour", got)
	}
	minted, err := store.Get(got)
	if err != nil {
		t.Fatalf("get minted convoy: %v", err)
	}
	if minted.Type != "convoy" {
		t.Fatalf("minted bead type = %q, want convoy", minted.Type)
	}
	if ok, err := convoy.HasTrack(store, got, "TASK-1"); err != nil || !ok {
		t.Fatalf("minted convoy does not track TASK-1: ok=%v err=%v", ok, err)
	}
}

// Normal pours must keep working: no container at all means a fresh mint,
// exactly as before the veto.
func TestNormalizeInputConvoyStillMintsWithoutExistingConvoy(t *testing.T) {
	store := dedupStore(nil)
	got, err := NormalizeInputConvoy(store, "TASK-1")
	if err != nil {
		t.Fatalf("NormalizeInputConvoy: %v", err)
	}
	minted, err := store.Get(got)
	if err != nil {
		t.Fatalf("get minted convoy: %v", err)
	}
	if minted.Title != "input convoy for TASK-1" {
		t.Fatalf("minted title = %q, want the synthetic convoy container", minted.Title)
	}
	if ok, err := convoy.HasTrack(store, got, "TASK-1"); err != nil || !ok {
		t.Fatalf("minted convoy does not track TASK-1: ok=%v err=%v", ok, err)
	}
}

package graphv2

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formulatest"
)

// gk-3n3jk dedup veto, scoped to automated pour surfaces (mayor word-said,
// ci-wisp-pvaz7t 2026-11-02T08:03Z): every observed re-pour of parked work
// presented first as a second synthetic "input convoy for <bead>" minted
// beside the still-open first one (gk-f4gr 3x, the gk-bx4v convoy pair, the
// second 5ryve convoy, the h8sdl/ojstx intake re-fires). The pour mint edge
// must reuse the open container instead, so an automated re-fire is
// idempotent there. Explicit `gc formula cook --attach` deliberately does NOT
// dedupe — two attaches are two invocations, each with its own container and
// workflow root (invocation_test.go, cmd_formula_test.go double-attach pins).

func dedupStore(convoys []beads.Bead) *beads.MemStore {
	deps := make([]beads.Dep, 0, len(convoys))
	for _, c := range convoys {
		deps = append(deps, beads.Dep{IssueID: c.ID, DependsOnID: "TASK-1", Type: convoy.TrackingDepType})
	}
	all := append([]beads.Bead{{ID: "TASK-1", Type: "task", Status: "open"}}, convoys...)
	return beads.NewMemStoreFrom(1, all, deps)
}

func TestNormalizeInputConvoyForPourReusesOpenConvoy(t *testing.T) {
	store := dedupStore([]beads.Bead{
		{ID: "CVY-OLD", Type: "convoy", Status: "open", Title: "input convoy for TASK-1"},
	})
	got, err := NormalizeInputConvoyForPour(store, "TASK-1")
	if err != nil {
		t.Fatalf("NormalizeInputConvoyForPour: %v", err)
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

func TestNormalizeInputConvoyForPourPrefersOldestOpenConvoy(t *testing.T) {
	// MemStore seeds every bead with the same clock, so the age order the
	// veto relies on must be stated explicitly here.
	base := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	store := dedupStore([]beads.Bead{
		{ID: "CVY-OLD", Type: "convoy", Status: "open", CreatedAt: base},
		{ID: "CVY-NEW", Type: "convoy", Status: "open", CreatedAt: base.Add(time.Hour)},
	})
	got, err := NormalizeInputConvoyForPour(store, "TASK-1")
	if err != nil {
		t.Fatalf("NormalizeInputConvoyForPour: %v", err)
	}
	if got != "CVY-OLD" {
		t.Fatalf("convoy id = %q, want the oldest open container CVY-OLD (TrackingConvoysForItem sorts created_at ascending)", got)
	}
}

func TestNormalizeInputConvoyForPourMintsWhenExistingConvoyClosed(t *testing.T) {
	store := dedupStore([]beads.Bead{
		{ID: "CVY-DONE", Type: "convoy", Status: "closed"},
	})
	got, err := NormalizeInputConvoyForPour(store, "TASK-1")
	if err != nil {
		t.Fatalf("NormalizeInputConvoyForPour: %v", err)
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

// Automated pours must keep working: no container at all means a fresh mint,
// exactly as before the veto.
func TestNormalizeInputConvoyForPourStillMintsWithoutExistingConvoy(t *testing.T) {
	store := dedupStore(nil)
	got, err := NormalizeInputConvoyForPour(store, "TASK-1")
	if err != nil {
		t.Fatalf("NormalizeInputConvoyForPour: %v", err)
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

// The option is the caller-facing seam: PrepareInvocation reached with
// WithReusableInputConvoy (the sling pour path) reuses; reached without (the
// explicit cook path) mints fresh — pinned here at the PrepareInvocation
// level so the two paths cannot silently converge again.
func TestPrepareInvocationInputConvoyReuseFollowsOption(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeFormula(t, dir, "work.formula.toml", `
formula = "work"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
	store := beads.NewMemStore()
	target, err := store.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}

	inv, err := PrepareInvocation(context.Background(), store, "work", []string{dir}, target.ID, nil)
	if err != nil {
		t.Fatalf("PrepareInvocation: %v", err)
	}
	pour, err := PrepareInvocation(context.Background(), store, "work", []string{dir}, target.ID, nil, WithReusableInputConvoy())
	if err != nil {
		t.Fatalf("PrepareInvocation pour: %v", err)
	}
	if pour.InputConvoy != inv.InputConvoy {
		t.Fatalf("pour path convoy = %s, want the open container %s reused", pour.InputConvoy, inv.InputConvoy)
	}
	fresh, err := PrepareInvocation(context.Background(), store, "work", []string{dir}, target.ID, nil)
	if err != nil {
		t.Fatalf("PrepareInvocation explicit: %v", err)
	}
	if fresh.InputConvoy == inv.InputConvoy {
		t.Fatalf("explicit path reused the open container %s, want a fresh mint", fresh.InputConvoy)
	}
}

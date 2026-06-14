package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// orderClaimedPoolHandoffSetup builds a sling against a worker pool for a bead
// an order has already claimed (status=in_progress, assignee=order:<name>),
// reproducing the gastownhall/gascity#3231 starting state. The agent is a
// multi-session pool in a rig so the bead is routed to the pool's claim queue
// rather than a single named session. MemStore.Create forces status=open, so
// the in_progress/assignee state is applied via a follow-up Update.
func orderClaimedPoolHandoffSetup(t *testing.T) (SlingOpts, SlingDeps, beads.Bead) {
	t.Helper()
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/myrig", Prefix: "gc"},
		},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	bead, err := deps.Store.Create(beads.Bead{Title: "hotspot work", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inProgress, orderActor := "in_progress", "order:mol-dog-jsonl"
	if err := deps.Store.Update(bead.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &orderActor}); err != nil {
		t.Fatalf("Update to order-claimed state: %v", err)
	}
	opts := SlingOpts{Target: a, BeadOrFormula: bead.ID, NoFormula: true, Reassign: true}
	return opts, deps, bead
}

// TestDoSling_Reassign_ReopensOrderClaimedBead is the regression test for
// gastownhall/gascity#3231. An order runs `bd update --claim` on a bead
// (status=in_progress, assignee=order:<name>) and then slings it to a worker
// pool with --reassign. Clearing the assignee alone is not enough: the bead
// stays in_progress, and IsReadyCandidate (which requires status=open) filters
// it out, so no pool worker ever claims it — "work looks in progress, but no
// polecat actually owns it." --reassign must reopen the bead so the target
// pool can claim it.
func TestDoSling_Reassign_ReopensOrderClaimedBead(t *testing.T) {
	opts, deps, bead := orderClaimedPoolHandoffSetup(t)
	if _, err := DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling --reassign: %v", err)
	}
	got, err := deps.Store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty after --reassign (order actor must not retain pool work)", got.Assignee)
	}
	if got.Status != "open" {
		t.Errorf("Status = %q, want open after --reassign (an in_progress bead handed to a pool must be reopened so it is claimable)", got.Status)
	}
}

// TestDoSling_Reassign_PreservesNonInProgressStatus guards the reopen from
// over-reaching: --reassign only reopens in_progress beads. A bead in another
// status (here, blocked) keeps its status; only the assignee is cleared.
func TestDoSling_Reassign_PreservesNonInProgressStatus(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs:      []config.Rig{{Name: "myrig", Path: "/myrig", Prefix: "gc"}},
	}
	a := config.Agent{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(2)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	bead, err := deps.Store.Create(beads.Bead{Title: "blocked work", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	blocked, orderActor := "blocked", "order:mol-dog-jsonl"
	if err := deps.Store.Update(bead.ID, beads.UpdateOpts{Status: &blocked, Assignee: &orderActor}); err != nil {
		t.Fatalf("Update to blocked state: %v", err)
	}
	opts := SlingOpts{Target: a, BeadOrFormula: bead.ID, NoFormula: true, Reassign: true}
	if _, err := DoSling(opts, deps, nil); err != nil {
		t.Fatalf("DoSling --reassign: %v", err)
	}
	got, err := deps.Store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", bead.ID, err)
	}
	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty after --reassign", got.Assignee)
	}
	if got.Status != "blocked" {
		t.Errorf("Status = %q, want blocked (reopen must only apply to in_progress beads)", got.Status)
	}
}

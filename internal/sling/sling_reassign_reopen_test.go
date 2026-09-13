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

// TestDoSling_ReassignFormula_DoesNotReopenCollidingBead is the regression
// guard for the standalone formula + --reassign hazard. LaunchFormula forwards
// Reassign and sets BeadOrFormula to the formula NAME (not a bead ID), and
// pre-flight runs the reassign reopen before the IsFormula dispatch. Without
// the shouldReopenForReassign guard, reopenForReassign was called on that name,
// so a bead whose ID happened to equal the formula name was silently
// cleared/reopened — disrupting work another actor had already claimed. A
// standalone formula launch must never touch a same-named bead.
func TestDoSling_ReassignFormula_DoesNotReopenCollidingBead(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	// Seed a bead whose ID collides with the "code-review" formula name and put
	// it in the order-claimed state (status=in_progress, assignee set) that
	// reopenForReassign would otherwise clear.
	deps.Store = seededStore("code-review")
	inProgress, orderActor := "in_progress", "order:mol-dog-jsonl"
	if err := deps.Store.Update("code-review", beads.UpdateOpts{Status: &inProgress, Assignee: &orderActor}); err != nil {
		t.Fatalf("Update colliding bead to order-claimed state: %v", err)
	}

	result, err := DoSling(SlingOpts{
		Target:        a,
		BeadOrFormula: "code-review",
		IsFormula:     true,
		Reassign:      true,
	}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling formula launch with --reassign: %v", err)
	}
	if result.Method != "formula" {
		t.Errorf("Method = %q, want formula (standalone formula launch)", result.Method)
	}

	got, err := deps.Store.Get("code-review")
	if err != nil {
		t.Fatalf("store.Get(code-review): %v", err)
	}
	if got.Assignee != orderActor {
		t.Errorf("Assignee = %q, want %q — a standalone formula launch must not reopen a bead sharing the formula name", got.Assignee, orderActor)
	}
	if got.Status != "in_progress" {
		t.Errorf("Status = %q, want in_progress — a standalone formula launch must not reopen a bead sharing the formula name", got.Status)
	}
}

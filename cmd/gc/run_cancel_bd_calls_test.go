package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// runCancelBdCalls is the bd command census of one run cancel.
type runCancelBdCalls struct {
	// children counts per-parent and batched children reads: `bd list --parent`,
	// `bd query "... parent=..."` and `bd dep list --type=parent-child`.
	children int
	// deps counts DOWN dependency reads: `bd dep list` without --direction=up.
	deps int
	// refresh counts reads of beads that are already closed: the cache's
	// read-back of the rows a close just wrote.
	refresh int
	total   int
}

// cancelRunAgainstFakeBd builds a run with members steps per workflow (the run
// root and one nested drain item workflow) in a fake bd, cancels it through
// the production store stack (bead policy over CachingStore over BdStore) the
// way internal/api's cancelRun does, and returns the bd commands it ran.
func cancelRunAgainstFakeBd(t *testing.T, members int) runCancelBdCalls {
	t.Helper()
	fake := beadstest.NewFakeBd()
	seed := func(b beads.Bead) beads.Bead {
		t.Helper()
		seeded, err := fake.Seed(b)
		if err != nil {
			t.Fatalf("seed %q: %v", b.Title, err)
		}
		return seeded
	}
	seedChain := func(rootID string) []beads.Bead {
		var steps []beads.Bead
		for i := range members {
			step := seed(beads.Bead{Title: fmt.Sprintf("step %d", i), ParentID: rootID, Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: rootID,
				beadmeta.StepIDMetadataKey:     fmt.Sprintf("step-%d", i),
			}})
			if i > 0 {
				if err := fake.SeedDep(step.ID, steps[i-1].ID, "blocks"); err != nil {
					t.Fatal(err)
				}
			}
			steps = append(steps, step)
		}
		return steps
	}

	run := seed(beads.Bead{Title: "run", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}})
	runSteps := seedChain(run.ID)
	drain := seed(beads.Bead{Title: "drain", ParentID: run.ID, Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: run.ID,
		beadmeta.KindMetadataKey:       beadmeta.KindDrain,
	}})
	// The teardown step is a wisp reachable only through the parent closure,
	// and its retry attempt is a run member: the attempt stays open only if
	// the subtree walk still reads the wisp tier through the batched read.
	seed(beads.Bead{Title: "teardown", ParentID: run.ID, Ephemeral: true, Metadata: map[string]string{
		beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown,
		beadmeta.StepIDMetadataKey:    "teardown",
	}})
	teardownAttempt := seed(beads.Bead{Title: "teardown attempt", ParentID: run.ID, Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: run.ID,
		beadmeta.StepIDMetadataKey:     "teardown",
	}})
	wispStep := seed(beads.Bead{Title: "wisp step", ParentID: run.ID, Ephemeral: true, Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: run.ID,
	}})
	item := seed(beads.Bead{Title: "drain item", Metadata: map[string]string{
		beadmeta.KindMetadataKey:           beadmeta.KindWorkflow,
		beadmeta.DrainControlIDMetadataKey: drain.ID,
	}})
	itemSteps := seedChain(item.ID)

	var calls runCancelBdCalls
	runner := func(dir, name string, args ...string) ([]byte, error) {
		calls.total++
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "list" && slices.Contains(args, "--parent"),
			args[0] == "query" && strings.Contains(joined, "parent="),
			args[0] == "dep" && slices.Contains(args, "--type=parent-child"):
			calls.children++
		case args[0] == "dep" && args[1] == "list" && !slices.Contains(args, "--direction=up"):
			calls.deps++
		case args[0] == "show":
			allClosed := true
			for _, id := range args[1:] {
				if strings.HasPrefix(id, "-") {
					continue
				}
				if b, err := fake.Bead(id); err != nil || b.Status != "closed" {
					allClosed = false
				}
			}
			if allClosed {
				calls.refresh++
			}
		}
		return fake.Run(dir, name, args...)
	}
	store := wrapWithCachingStore(context.Background(),
		wrapStoreWithBeadPolicies(beads.NewBdStore("/fake-bd", runner), &config.City{}), nil, false)

	// internal/api cancelRun for one open root: mark the run canceling, read
	// its workflow tree, then close nested drain item workflows first and the
	// run root last, each with its teardown tail spared.
	cancelRoot := func(rootID string) int {
		t.Helper()
		exclude, err := molecule.TeardownTailExclusion(store, rootID)
		if err != nil {
			t.Fatalf("TeardownTailExclusion(%s): %v", rootID, err)
		}
		n, err := sourceworkflow.CloseWorkflowSubtreeAsExcept(store, rootID, beadmeta.OutcomeCanceled,
			"run canceled via POST /runs/{id}/cancel",
			map[string]string{beadmeta.CancelRequestedMetadataKey: "true"}, exclude)
		if err != nil {
			t.Fatalf("CloseWorkflowSubtreeAsExcept(%s): %v", rootID, err)
		}
		return n
	}
	if err := store.SetMetadata(run.ID, beadmeta.CancelRequestedMetadataKey, "true"); err != nil {
		t.Fatalf("mark run canceling: %v", err)
	}
	tree, err := sourceworkflow.WorkflowTree(store, run.ID)
	if err != nil {
		t.Fatalf("WorkflowTree: %v", err)
	}
	if len(tree) != 2 || tree[0].Root.ID != item.ID || tree[1].Root.ID != run.ID {
		t.Fatalf("workflow tree roots = %v, want [%s %s]", tree, item.ID, run.ID)
	}
	closed := 0
	for _, node := range tree {
		closed += cancelRoot(node.Root.ID)
	}

	// root + steps + drain + wisp step per run, root + steps per item.
	if want := 2*(members+1) + 2; closed != want {
		t.Errorf("cancel closed %d beads, want %d", closed, want)
	}
	for _, b := range append(append([]beads.Bead{run, drain, wispStep, item}, runSteps...), itemSteps...) {
		if got, _ := fake.Bead(b.ID); got.Status != "closed" {
			t.Errorf("%s (%s) is %s after cancel, want closed", b.ID, b.Title, got.Status)
		}
	}
	if got, _ := fake.Bead(teardownAttempt.ID); got.Status == "closed" {
		t.Errorf("the teardown attempt %s was closed: the subtree walk lost the wisp teardown step", teardownAttempt.ID)
	}
	return calls
}

// TestRunCancelBdCallsDoNotGrowPerMember pins the cost of POST
// /runs/{id}/cancel against a bd store. On a loaded two-vCPU host each bd
// command took 0.6-1 s and a sixteen-bead cancel ran about a hundred of them
// (81 s), because the policy and cache wrappers hid bd's batched reads: the
// subtree walk read children per member, the close order read deps per
// member, and the close read every closed bead back one `bd show` at a time.
// Those three reads must not grow with the run.
func TestRunCancelBdCallsDoNotGrowPerMember(t *testing.T) {
	small := cancelRunAgainstFakeBd(t, 3)
	large := cancelRunAgainstFakeBd(t, 12)
	t.Logf("bd commands per cancel: 3 steps/workflow %+v; 12 steps/workflow %+v", small, large)
	if large.children != small.children {
		t.Errorf("children reads grew from %d to %d with the run; the subtree walk reads per member", small.children, large.children)
	}
	if large.deps != small.deps {
		t.Errorf("dependency reads grew from %d to %d with the run; the close order reads per member", small.deps, large.deps)
	}
	if large.refresh != small.refresh {
		t.Errorf("post-close reads grew from %d to %d with the run; the close refresh reads per bead", small.refresh, large.refresh)
	}
}

// TestRunCancelCloseOrderHonorsBlockersWhenBdRefusesTheBatch pins close
// ordering through the production stack on a bd backend that refuses the
// batched dep read (the bd/Postgres backend answers it with `operation
// "IssueRelations" not supported`, ga-7i7ts) but answers a one-id read. The
// refusal must send closeorder to its per-anchor reads, not read as "no edges":
// each step here is blocked by the NEXT one created, so an order that lost the
// edges (deepest first, then by id) closes every blocked step before its
// blocker.
func TestRunCancelCloseOrderHonorsBlockersWhenBdRefusesTheBatch(t *testing.T) {
	fake := beadstest.NewFakeBd()
	seed := func(b beads.Bead) beads.Bead {
		t.Helper()
		seeded, err := fake.Seed(b)
		if err != nil {
			t.Fatalf("seed %q: %v", b.Title, err)
		}
		return seeded
	}
	root := seed(beads.Bead{Title: "run", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}})
	var steps []beads.Bead
	for i := range 4 {
		steps = append(steps, seed(beads.Bead{Title: fmt.Sprintf("step %d", i), ParentID: root.ID, Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
		}}))
	}
	blockedBy := map[string]string{}
	for i := 0; i+1 < len(steps); i++ {
		if err := fake.SeedDep(steps[i].ID, steps[i+1].ID, "blocks"); err != nil {
			t.Fatal(err)
		}
		blockedBy[steps[i].ID] = steps[i+1].ID
	}

	var closeOrder []string
	var refusedBatches int
	runner := func(dir, name string, args ...string) ([]byte, error) {
		if args[0] == "dep" && args[1] == "list" && !slices.Contains(args, "--direction=up") {
			ids := 0
			for _, arg := range args[2:] {
				if !strings.HasPrefix(arg, "-") {
					ids++
				}
			}
			if ids > 1 {
				refusedBatches++
				return nil, fmt.Errorf(`exit status 1: Error: operation "IssueRelations" not supported by the postgres backend`)
			}
		}
		if args[0] == "close" {
			for _, arg := range args[1:] {
				if strings.HasPrefix(arg, "gc-") {
					closeOrder = append(closeOrder, arg)
				}
			}
		}
		return fake.Run(dir, name, args...)
	}
	store := wrapWithCachingStore(context.Background(),
		wrapStoreWithBeadPolicies(beads.NewBdStore("/fake-bd", runner), &config.City{}), nil, false)

	if _, err := sourceworkflow.CloseWorkflowSubtreeAsExcept(store, root.ID, beadmeta.OutcomeCanceled,
		"run canceled via POST /runs/{id}/cancel", map[string]string{beadmeta.CancelRequestedMetadataKey: "true"}, nil); err != nil {
		t.Fatalf("CloseWorkflowSubtreeAsExcept: %v", err)
	}
	if refusedBatches == 0 {
		t.Fatal("the close order never asked for the batch, so this test does not exercise the refusal")
	}
	position := make(map[string]int, len(closeOrder))
	for i, id := range closeOrder {
		if _, seen := position[id]; !seen {
			position[id] = i
		}
	}
	for blocked, blocker := range blockedBy {
		if position[blocker] > position[blocked] {
			t.Errorf("close order %v closes %s before its blocker %s; the refused batch was read as no edges", closeOrder, blocked, blocker)
		}
	}
}

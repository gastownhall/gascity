package main

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// slowReadStore holds every read until the read's own budget has expired, then
// answers anyway, the way a loaded store does: it ignores the caller's deadline.
// That makes the stop gate's ancestry read outlast the budget without sleeping.
type slowReadStore struct {
	beads.Store
	budget context.Context
}

func (s slowReadStore) Get(id string) (beads.Bead, error) {
	<-s.budget.Done()
	return s.Store.Get(id)
}

func (s slowReadStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	<-s.budget.Done()
	return s.Store.List(q)
}

// TestHookClaimStopGateReadDoesNotSpendTheCASBudget pins that a claim's CAS gets
// its own mutation budget, whatever the stop gate's ancestry reads spent. The
// stop gate runs against a store whose reads outlast the budget; the CAS must
// still run on a live context and claim the step.
func TestHookClaimStopGateReadDoesNotSpendTheCASBudget(t *testing.T) {
	old := hookClaimMutationTimeout
	hookClaimMutationTimeout = 200 * time.Millisecond
	t.Cleanup(func() { hookClaimMutationTimeout = old })

	f := newHookWorkflowStopFixture(t)
	root := f.root("workflow", "")
	step := f.step("step", root.ID, nil)
	f.served = []string{step.ID}

	ops := f.ops()
	ops.WorkflowStore = func(ctx context.Context, _ string, _ []string, _ string, _ string) beads.Store {
		return slowReadStore{Store: f.store, budget: ctx}
	}
	innerClaim := ops.Claim
	ops.Claim = func(ctx context.Context, dir string, env []string, id, assignee string) (beads.Bead, bool, error) {
		if err := ctx.Err(); err != nil {
			return beads.Bead{}, false, fmt.Errorf("claiming bead %q: timed out after 0s (caller deadline): %w", id, err)
		}
		return innerClaim(ctx, dir, env, id, assignee)
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", t.TempDir(), hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(f.claims) != 1 || f.claims[0] != step.ID {
		t.Fatalf("claims = %v, want [%s]: the stop gate's reads spent the CAS's budget; stderr=%s", f.claims, step.ID, stderr.String())
	}
}

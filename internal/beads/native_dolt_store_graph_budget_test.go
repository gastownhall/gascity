package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// budgetExhaustedTestPlan is the smallest plan that reaches the edge loop:
// two nodes and one blocking edge, so the AddDependency hook runs inside the
// context ApplyGraphPlanWithStorage derives for the plan.
func budgetExhaustedTestPlan() *GraphApplyPlan {
	return &GraphApplyPlan{
		CommitMessage: "gc: budget test graph",
		Nodes: []GraphApplyNode{
			{Key: "blocker", Title: "Blocker"},
			{Key: "child", Title: "Child"},
		},
		Edges: []GraphApplyEdge{
			{FromKey: "child", ToKey: "blocker"},
		},
	}
}

// budgetTestStore returns a native store whose AddDependency runs hook, over
// the in-memory storage fake the other graph-apply tests use.
func budgetTestStore(hook func(context.Context, *beadslib.Dependency) error) *NativeDoltStore {
	return newNativeDoltStoreForTest(&nativeDoltFailingDependencyStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		addDependency:        hook,
	})
}

// An apply whose own derived context expires while the caller's context is
// still live has spent the operation budget nativeGraphApplyDeadline sized for
// this plan. Mark that case so callers can apply a no-automatic-replay policy
// instead of paying for a second full attempt and then a sequential fallback
// (gastownhall/gascity#6333).
//
// Run inside synctest: the AddDependency hook blocks durably on the apply's own
// Done channel, so the bubble has no runnable goroutine and advances its
// synthetic clock straight to the derived deadline. The test therefore
// exercises the production budget arithmetic — the real
// nativeGraphApplyDeadline(plan) — in microseconds, with no package-level
// timeout mutated anywhere.
func TestApplyGraphPlanWrapsOwnDeadlineAsBudgetExhausted(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		plan := budgetExhaustedTestPlan()
		budget := nativeGraphApplyDeadline(plan)
		started := make(chan struct{})
		store := budgetTestStore(func(ctx context.Context, _ *beadslib.Dependency) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})

		_, err := store.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)

		select {
		case <-started:
		default:
			t.Fatal("apply never reached the edge loop; the budget was not what expired")
		}
		if elapsed := time.Since(start); elapsed != budget {
			t.Errorf("synthetic time elapsed = %s, want exactly the derived budget %s", elapsed, budget)
		}
		if err == nil {
			t.Fatal("ApplyGraphPlanWithStorage error = nil, want the spent-budget error")
		}
		if !errors.Is(err, ErrGraphApplyBudgetExhausted) {
			t.Errorf("error = %v, want errors.Is(err, ErrGraphApplyBudgetExhausted)", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want the deadline cause preserved", err)
		}
		if !strings.Contains(err.Error(), budget.String()) {
			t.Errorf("error = %v, want the spent budget %s named", err, budget)
		}
		if want := fmt.Sprintf("%d nodes/%d edges", len(plan.Nodes), len(plan.Edges)); !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want the plan size %q named", err, want)
		}
		if !strings.Contains(err.Error(), "adding edge") {
			t.Errorf("error = %v, want the failing edge named", err)
		}
	})
}

// The caller's own cancellation is not this apply's budget running out: the
// plan may be perfectly sized and the caller simply went away. The apply must
// report the cancellation as such and must not attach a policy marker that
// suppresses a retry the caller may still want.
func TestApplyGraphPlanDoesNotClaimBudgetExhaustedWhenCallerCancels(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		store := budgetTestStore(func(ctx context.Context, _ *beadslib.Dependency) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		})

		_, err := store.ApplyGraphPlanWithStorage(parent, budgetExhaustedTestPlan(), StorageDefault)
		if err == nil {
			t.Fatal("ApplyGraphPlanWithStorage error = nil, want the cancellation error")
		}
		if errors.Is(err, ErrGraphApplyBudgetExhausted) {
			t.Errorf("error = %v, want no spent-budget marker when the caller canceled", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want the caller's cancellation preserved", err)
		}
	})
}

// The other half of the discrimination: a single statement can fail with
// context.DeadlineExceeded from a deadline of its own (a pool per-I/O budget,
// say) while both the apply's context and the caller's are still live. Nothing
// has been spent then, so the error must stay a plain transient that callers
// are free to retry — the marker is about the operation budget, not about the
// words in the error.
func TestApplyGraphPlanDoesNotClaimBudgetExhaustedWhileBothContextsLive(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		store := budgetTestStore(func(ctx context.Context, _ *beadslib.Dependency) error {
			if ctx.Err() != nil {
				t.Error("apply context expired before the statement returned; the arm under test needs both contexts live")
			}
			return fmt.Errorf("failed to check for dependency cycle: %w", context.DeadlineExceeded)
		})

		_, err := store.ApplyGraphPlanWithStorage(context.Background(), budgetExhaustedTestPlan(), StorageDefault)
		if err == nil {
			t.Fatal("ApplyGraphPlanWithStorage error = nil, want the statement error")
		}
		if errors.Is(err, ErrGraphApplyBudgetExhausted) {
			t.Errorf("error = %v, want no spent-budget marker while both contexts are live", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want the statement's deadline cause preserved", err)
		}
	})
}

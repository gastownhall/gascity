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
	"github.com/steveyegge/beads/issueops"
)

// nativeDoltBudgetProbeStorage is the mem backing whose batch applies are a
// test hook, so a test can decide how an apply ends while the store's real
// plan-derived budget runs on the synthetic clock.
type nativeDoltBudgetProbeStorage struct {
	*nativeDoltMemStorage
	apply func(context.Context, issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error)
}

func (s *nativeDoltBudgetProbeStorage) BatchApplier() (issueops.BatchApplier, error) {
	return nativeDoltBudgetProbeApplier{storage: s}, nil
}

type nativeDoltBudgetProbeApplier struct {
	storage *nativeDoltBudgetProbeStorage
}

func (a nativeDoltBudgetProbeApplier) ApplyBatch(ctx context.Context, req issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	return a.storage.apply(ctx, req)
}

// nativeDoltStallUntilApplyCtxEnds blocks until ctx ends and reports ctx's own
// error, the shape of a batch still running when its transaction budget fires.
func nativeDoltStallUntilApplyCtxEnds(ctx context.Context, _ issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	<-ctx.Done()
	return issueops.ApplyBatchResult{}, ctx.Err()
}

func graphBudgetTwoNodePlan() *GraphApplyPlan {
	return &GraphApplyPlan{
		Nodes: []GraphApplyNode{
			{Key: "wf", Title: "Workflow", Type: "task"},
			{Key: "wf.step", Title: "Work", Type: "task"},
		},
		Edges: []GraphApplyEdge{{FromKey: "wf.step", ToKey: "wf", Type: "blocks"}},
	}
}

// requireGraphBudgetExhausted asserts the full contract of a budget-exhausted
// apply failure: the typed cause with the plan it was derived from, unwrapping
// to context.DeadlineExceeded, behind the store's usual prefix.
func requireGraphBudgetExhausted(t *testing.T, err error, plan *GraphApplyPlan) {
	t.Helper()
	if err == nil {
		t.Fatal("ApplyGraphPlanWithStorage error = nil, want the budget-exhausted failure")
	}
	var exhausted *GraphApplyBudgetExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("ApplyGraphPlanWithStorage error = %v, want a *GraphApplyBudgetExhaustedError in the chain", err)
	}
	if want := nativeGraphApplyDeadline(plan); exhausted.Budget != want {
		t.Errorf("exhausted.Budget = %s, want the plan-derived %s", exhausted.Budget, want)
	}
	if exhausted.Nodes != len(plan.Nodes) {
		t.Errorf("exhausted.Nodes = %d, want %d", exhausted.Nodes, len(plan.Nodes))
	}
	if exhausted.Edges != len(plan.Edges) {
		t.Errorf("exhausted.Edges = %d, want %d", exhausted.Edges, len(plan.Edges))
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false, want true", err)
	}
	if !strings.HasPrefix(err.Error(), "native graph apply: ") {
		t.Errorf("error text = %q, want the native graph apply prefix", err.Error())
	}
}

// TestNativeApplyGraphPlanReportsItsOwnBudgetExhausted: when the store's own
// plan-derived budget is the deadline that ends a native apply while the
// caller's context is live, the failure names that budget through a typed
// cause while the store's own failure text survives behind it.
func TestNativeApplyGraphPlanReportsItsOwnBudgetExhausted(t *testing.T) {
	t.Run("batch", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			plan := graphBudgetTwoNodePlan()
			storage := &nativeDoltBudgetProbeStorage{
				nativeDoltMemStorage: newNativeDoltMemStorage(),
				apply:                nativeDoltStallUntilApplyCtxEnds,
			}
			store := newNativeDoltStoreForTest(storage)

			start := time.Now()
			_, err := store.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)
			requireGraphBudgetExhausted(t, err, plan)
			if elapsed := time.Since(start); elapsed != nativeGraphApplyDeadline(plan) {
				t.Errorf("elapsed = %s, want exactly one derived budget %s", elapsed, nativeGraphApplyDeadline(plan))
			}
		})
	})

	t.Run("over-cap", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// 101 creates plus one edge exceeds issueops.MaxApplyBatchItems,
			// so this plan takes the hand-run transaction route.
			plan := &GraphApplyPlan{Nodes: make([]GraphApplyNode, 0, 101)}
			for i := 0; i < 101; i++ {
				plan.Nodes = append(plan.Nodes, GraphApplyNode{Key: fmt.Sprintf("n%03d", i), Title: fmt.Sprintf("Node %d", i), Type: "task"})
			}
			plan.Edges = []GraphApplyEdge{{FromKey: "n001", ToKey: "n000", Type: "blocks"}}
			storage := &nativeDoltFailingDependencyStorage{
				nativeDoltMemStorage: newNativeDoltMemStorage(),
				addDependency: func(ctx context.Context, _ *beadslib.Dependency) error {
					<-ctx.Done()
					return fmt.Errorf("failed to check for dependency cycle: %w", ctx.Err())
				},
			}
			store := newNativeDoltStoreForTest(storage)

			start := time.Now()
			_, err := store.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)
			requireGraphBudgetExhausted(t, err, plan)
			if !strings.Contains(err.Error(), "adding edge ") {
				t.Errorf("error text = %q, want the store's own edge failure preserved", err.Error())
			}
			if elapsed := time.Since(start); elapsed != nativeGraphApplyDeadline(plan) {
				t.Errorf("elapsed = %s, want exactly one derived budget %s", elapsed, nativeGraphApplyDeadline(plan))
			}
		})
	})

	t.Run("applier returns the cause", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			plan := graphBudgetTwoNodePlan()
			storage := &nativeDoltBudgetProbeStorage{
				nativeDoltMemStorage: newNativeDoltMemStorage(),
				apply: func(ctx context.Context, _ issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
					<-ctx.Done()
					return issueops.ApplyBatchResult{}, context.Cause(ctx)
				},
			}
			store := newNativeDoltStoreForTest(storage)

			_, err := store.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)
			requireGraphBudgetExhausted(t, err, plan)
			if got := strings.Count(err.Error(), "graph apply budget"); got != 1 {
				t.Errorf("budget phrase appears %d times in %q, want 1 (no doubled prefix)", got, err.Error())
			}
		})
	})
}

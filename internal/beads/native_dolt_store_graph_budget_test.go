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

// nativeDoltScriptedBatchStorage is native mem storage whose BatchApplier runs
// the test's apply func, so a test decides how the batch route's apply ends.
type nativeDoltScriptedBatchStorage struct {
	*nativeDoltMemStorage
	apply func(context.Context, issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error)
}

func (s *nativeDoltScriptedBatchStorage) BatchApplier() (issueops.BatchApplier, error) {
	return nativeDoltScriptedBatchApplier(s.apply), nil
}

type nativeDoltScriptedBatchApplier func(context.Context, issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error)

func (f nativeDoltScriptedBatchApplier) ApplyBatch(ctx context.Context, req issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	return f(ctx, req)
}

func newNativeDoltScriptedBatchStore(apply func(context.Context, issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error)) *NativeDoltStore {
	return newNativeDoltStoreForTest(&nativeDoltScriptedBatchStorage{nativeDoltMemStorage: newNativeDoltMemStorage(), apply: apply})
}

// batchBudgetPlan is a two-node, one-edge plan: well under the batch cap.
func batchBudgetPlan() *GraphApplyPlan {
	return &GraphApplyPlan{
		CommitMessage: "gc: budget test",
		Nodes:         []GraphApplyNode{{Key: "a", Title: "A"}, {Key: "b", Title: "B"}},
		Edges:         []GraphApplyEdge{{FromKey: "b", ToKey: "a", Type: "blocks"}},
	}
}

// overCapBudgetPlan is a chain of 51 nodes joined by 50 blocking edges, 101
// batch items, so it takes the over-cap transaction route.
func overCapBudgetPlan(t *testing.T) *GraphApplyPlan {
	t.Helper()
	const nodeCount = 51
	plan := &GraphApplyPlan{CommitMessage: "gc: over-cap budget test"}
	for i := 0; i < nodeCount; i++ {
		plan.Nodes = append(plan.Nodes, GraphApplyNode{Key: fmt.Sprintf("n%d", i), Title: fmt.Sprintf("Node %d", i)})
		if i > 0 {
			plan.Edges = append(plan.Edges, GraphApplyEdge{FromKey: fmt.Sprintf("n%d", i), ToKey: fmt.Sprintf("n%d", i-1), Type: "blocks"})
		}
	}
	if items := len(plan.Nodes) + len(plan.Edges); items <= issueops.MaxApplyBatchItems {
		t.Fatalf("test construction error: %d items does not exceed the %d-item cap", items, issueops.MaxApplyBatchItems)
	}
	return plan
}

func stallUntilDone(ctx context.Context, _ issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	<-ctx.Done()
	return issueops.ApplyBatchResult{}, ctx.Err()
}

func TestNativeApplyGraphPlanReportsItsOwnBudgetExhausted(t *testing.T) {
	cases := []struct {
		name      string
		plan      func(*testing.T) *GraphApplyPlan
		store     func() *NativeDoltStore
		wantInErr string
	}{
		{
			name:  "batch",
			plan:  func(*testing.T) *GraphApplyPlan { return batchBudgetPlan() },
			store: func() *NativeDoltStore { return newNativeDoltScriptedBatchStore(stallUntilDone) },
		},
		{
			name: "over-cap",
			plan: overCapBudgetPlan,
			store: func() *NativeDoltStore {
				return newNativeDoltStoreForTest(&nativeDoltFailingDependencyStorage{
					nativeDoltMemStorage: newNativeDoltMemStorage(),
					addDependency: func(ctx context.Context, _ *beadslib.Dependency) error {
						<-ctx.Done()
						return fmt.Errorf("failed to check for dependency cycle: %w", ctx.Err())
					},
				})
			},
			wantInErr: "adding edge ",
		},
		{
			name: "applier returns the cause",
			plan: func(*testing.T) *GraphApplyPlan { return batchBudgetPlan() },
			store: func() *NativeDoltStore {
				return newNativeDoltScriptedBatchStore(func(ctx context.Context, _ issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
					<-ctx.Done()
					return issueops.ApplyBatchResult{}, context.Cause(ctx)
				})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				plan := tc.plan(t)
				store := tc.store()
				budget := nativeGraphApplyDeadline(plan)

				start := time.Now()
				_, err := store.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)
				elapsed := time.Since(start)

				if err == nil {
					t.Fatal("ApplyGraphPlanWithStorage error = nil, want the spent budget reported")
				}
				if elapsed != budget {
					t.Errorf("elapsed = %s, want exactly the derived budget %s", elapsed, budget)
				}
				var exhausted *GraphApplyBudgetExhaustedError
				if !errors.As(err, &exhausted) {
					t.Fatalf("error = %v, want a *GraphApplyBudgetExhaustedError", err)
				}
				if exhausted.Budget != budget || exhausted.Nodes != len(plan.Nodes) || exhausted.Edges != len(plan.Edges) {
					t.Errorf("exhausted = %+v, want Budget=%s Nodes=%d Edges=%d", *exhausted, budget, len(plan.Nodes), len(plan.Edges))
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false, want true", err)
				}
				text := err.Error()
				if !strings.HasPrefix(text, "native graph apply: ") {
					t.Errorf("error = %q, want prefix %q", text, "native graph apply: ")
				}
				if got := strings.Count(text, exhausted.Error()); got != 1 {
					t.Errorf("error = %q names the budget %d times, want once", text, got)
				}
				if tc.wantInErr != "" && !strings.Contains(text, tc.wantInErr) {
					t.Errorf("error = %q, want the store's own failure %q kept", text, tc.wantInErr)
				}
			})
		})
	}
}

func TestNativeApplyGraphPlanDoesNotClaimItsBudgetForOtherDeadlines(t *testing.T) {
	t.Run("caller deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()
			_, err := newNativeDoltScriptedBatchStore(stallUntilDone).ApplyGraphPlanWithStorage(parent, batchBudgetPlan(), StorageDefault)
			if elapsed := time.Since(start); elapsed != 10*time.Second {
				t.Errorf("elapsed = %s, want the caller's 10s deadline", elapsed)
			}
			assertNotBudgetExhausted(t, err, "native graph apply: context deadline exceeded")
		})
	})
	t.Run("caller cancel", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			time.AfterFunc(time.Second, cancel)
			_, err := newNativeDoltScriptedBatchStore(stallUntilDone).ApplyGraphPlanWithStorage(parent, batchBudgetPlan(), StorageDefault)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("errors.Is(%v, context.Canceled) = false, want true", err)
			}
			assertNotBudgetExhausted(t, err, "native graph apply: context canceled")
		})
	})
	t.Run("statement deadline while the budget is live", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store := newNativeDoltScriptedBatchStore(func(context.Context, issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
				return issueops.ApplyBatchResult{}, fmt.Errorf("check cycle: %w", context.DeadlineExceeded)
			})
			_, err := store.ApplyGraphPlanWithStorage(context.Background(), batchBudgetPlan(), StorageDefault)
			assertNotBudgetExhausted(t, err, "native graph apply: check cycle: context deadline exceeded")
		})
	})
}

func assertNotBudgetExhausted(t *testing.T, err error, wantText string) {
	t.Helper()
	if err == nil {
		t.Fatal("ApplyGraphPlanWithStorage error = nil, want the apply failure")
	}
	var exhausted *GraphApplyBudgetExhaustedError
	if errors.As(err, &exhausted) {
		t.Errorf("error = %v claims the store's own budget, want the other deadline reported as is", err)
	}
	if err.Error() != wantText {
		t.Errorf("error = %q, want %q", err.Error(), wantText)
	}
}

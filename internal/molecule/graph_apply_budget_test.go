package molecule

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/formula"
)

// graphBudgetError builds the failure shape the native store returns when its
// own derived budget ended the apply: the typed cause first, the store's own
// failure behind it.
func graphBudgetError(budget time.Duration, nodes, edges int) error {
	return fmt.Errorf("native graph apply: %w: %w",
		&beads.GraphApplyBudgetExhaustedError{Budget: budget, Nodes: nodes, Edges: edges},
		fmt.Errorf("adding edge a->b: failed to check for dependency cycle: %w", context.DeadlineExceeded))
}

// twoStepBudgetRecipe is the minimal recipe every budget test instantiates:
// a root and one step, the same shape the non-transient fallback tests use.
func twoStepBudgetRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true},
			{ID: "wf.step", Title: "Work", Type: "task"},
		},
	}
}

// stallingBatchStorage is the minimal native backing for the end-to-end test:
// its batch applies block until their context ends, so the store's own
// derived budget is the deadline that fires, and its lifecycle door reports
// an error so a sequential fallback fails on an assertion, not a nil panic.
type stallingBatchStorage struct {
	beadslib.Storage
	applies    *atomic.Int32
	lifecycles *atomic.Int32
}

func (s *stallingBatchStorage) BatchApplier() (issueops.BatchApplier, error) { return s, nil }

func (s *stallingBatchStorage) ApplyBatch(ctx context.Context, _ issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	s.applies.Add(1)
	<-ctx.Done()
	return issueops.ApplyBatchResult{}, ctx.Err()
}

func (s *stallingBatchStorage) IssueLifecycle() (issueops.Lifecycle, error) {
	s.lifecycles.Add(1)
	return nil, errors.New("issue lifecycle door unavailable in test backing: the sequential fallback must not run")
}

// TestInstantiateDoesNotReapplyANativeGraphApplyThatSpentItsOwnBudget runs the
// real NativeDoltStore apply path: when the store's own plan-derived budget is
// the deadline that ends the apply while the caller's context is live,
// Instantiate surfaces the failure instead of re-running the identical apply
// and then falling back to sequential creation.
func TestInstantiateDoesNotReapplyANativeGraphApplyThatSpentItsOwnBudget(t *testing.T) {
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(true)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })

	synctest.Test(t, func(t *testing.T) {
		storage := &stallingBatchStorage{applies: &atomic.Int32{}, lifecycles: &atomic.Int32{}}
		store := beads.NewNativeDoltStoreOverStorageForTest(storage)

		start := time.Now()
		_, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), twoStepBudgetRecipe(), Options{})
		if got := storage.applies.Load(); got != 1 {
			t.Errorf("graph applies attempted = %d, want 1 (no replay of an apply that spent its own budget)", got)
		}
		if got := storage.lifecycles.Load(); got != 0 {
			t.Errorf("sequential fallback lifecycle calls = %d, want 0 (no fallback after a spent budget)", got)
		}
		if err == nil {
			t.Fatal("Instantiate error = nil, want the budget-exhausted apply failure")
		}
		var exhausted *beads.GraphApplyBudgetExhaustedError
		if !errors.As(err, &exhausted) {
			t.Fatalf("Instantiate error = %v, want the budget-exhausted cause in the chain", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false, want true", err)
		}
		if elapsed := time.Since(start); elapsed != exhausted.Budget {
			t.Errorf("elapsed = %s, want exactly one derived budget %s (no retry delay, no second budget)", elapsed, exhausted.Budget)
		}
	})
}

// TestInstantiateReturnsBudgetExhaustedWithoutReplayOrFallback pins the
// wrapper chain, not the text: errors.As on the returned error is the only
// assertion that keeps a reformatted wrapper from restoring the replay.
func TestInstantiateReturnsBudgetExhaustedWithoutReplayOrFallback(t *testing.T) {
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(true)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })

	synctest.Test(t, func(t *testing.T) {
		budgetErr := graphBudgetError(298*time.Second, 29, 60)
		store := &graphApplySpyStore{MemStore: beads.NewMemStore(), errs: []error{budgetErr, budgetErr}}

		_, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), twoStepBudgetRecipe(), Options{})
		if err == nil {
			t.Fatal("Instantiate error = nil, want the budget-exhausted failure surfaced to the caller")
		}
		var exhausted *beads.GraphApplyBudgetExhaustedError
		if !errors.As(err, &exhausted) {
			t.Fatalf("Instantiate error = %v, want the budget-exhausted cause in the chain", err)
		}
		if store.calls != 1 {
			t.Errorf("ApplyGraphPlan calls = %d, want 1 (the second seeded failure is never consumed)", store.calls)
		}
		open, listErr := store.ListOpen()
		if listErr != nil {
			t.Fatalf("ListOpen: %v", listErr)
		}
		if len(open) != 0 {
			t.Errorf("sequential fallback created %d beads, want 0", len(open))
		}
	})
}

// TestInstantiateReturnsBudgetExhaustedRetryWithoutFallback: an ordinary
// transient keeps its single retry, and the budget spent on that retry still
// ends the operation without a sequential fallback.
func TestInstantiateReturnsBudgetExhaustedRetryWithoutFallback(t *testing.T) {
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(true)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })

	synctest.Test(t, func(t *testing.T) {
		transient := fmt.Errorf("native graph apply: adding edge a->b: write tcp 127.0.0.1:1->127.0.0.1:2: i/o timeout")
		store := &graphApplySpyStore{MemStore: beads.NewMemStore(), errs: []error{transient, graphBudgetError(298*time.Second, 29, 60)}}

		start := time.Now()
		_, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), twoStepBudgetRecipe(), Options{})
		if err == nil {
			t.Fatal("Instantiate error = nil, want the budget-exhausted failure surfaced to the caller")
		}
		var exhausted *beads.GraphApplyBudgetExhaustedError
		if !errors.As(err, &exhausted) {
			t.Fatalf("Instantiate error = %v, want the budget-exhausted cause in the chain", err)
		}
		if store.calls != 2 {
			t.Errorf("ApplyGraphPlan calls = %d, want 2 (the ordinary transient still gets its single retry)", store.calls)
		}
		if elapsed := time.Since(start); elapsed != graphApplyTransientRetryDelay {
			t.Errorf("elapsed = %s, want exactly the %s retry delay", elapsed, graphApplyTransientRetryDelay)
		}
		open, listErr := store.ListOpen()
		if listErr != nil {
			t.Fatalf("ListOpen: %v", listErr)
		}
		if len(open) != 0 {
			t.Errorf("sequential fallback created %d beads, want 0", len(open))
		}
	})
}

// TestIsTransientGraphApplyErrorRejectsBudgetExhaustedButNotItsText: the typed
// gate is above the substring classifier, so the typed cause is never
// transient while the same text without the type still is — the classifier
// keeps matching genuinely recoverable transients.
func TestIsTransientGraphApplyErrorRejectsBudgetExhaustedButNotItsText(t *testing.T) {
	budgetErr := graphBudgetError(298*time.Second, 29, 60)
	if isTransientGraphApplyError(budgetErr) {
		t.Errorf("isTransientGraphApplyError(typed budget exhaustion) = true, want false")
	}
	wrapped := fmt.Errorf("instantiating formula %q: %w", "wf", budgetErr)
	if isTransientGraphApplyError(wrapped) {
		t.Errorf("isTransientGraphApplyError(wrapped typed budget exhaustion) = true, want false")
	}
	if !isTransientGraphApplyError(errors.New(budgetErr.Error())) {
		t.Errorf("isTransientGraphApplyError(plain text budget exhaustion) = false, want true (the substring classifier is untouched)")
	}
}

// TestInstantiateStillRetriesAnUntypedStatementDeadlineWhileBothContextsLive
// pins the other direction of the typed gate: an apply failing with an
// untyped deadline error — a statement timing out while both the caller's
// context and the store's budget are still live — keeps its single retry,
// and a retry that lands instantiates the workflow atomically.
func TestInstantiateStillRetriesAnUntypedStatementDeadlineWhileBothContextsLive(t *testing.T) {
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(true)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })

	store := &graphApplySpyStore{
		MemStore: beads.NewMemStore(),
		errs: []error{
			fmt.Errorf("native graph apply: adding edge a->b: failed to check for dependency cycle: context deadline exceeded"),
		},
	}

	result, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), twoStepBudgetRecipe(), Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if store.calls != 2 {
		t.Errorf("ApplyGraphPlan calls = %d, want 2 (an untyped deadline still gets its single retry)", store.calls)
	}
	if result.Created != 2 {
		t.Errorf("Created = %d, want 2", result.Created)
	}
	if result.RootID != "bd-1" {
		t.Errorf("RootID = %q, want bd-1", result.RootID)
	}
}

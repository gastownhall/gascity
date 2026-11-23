package molecule

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/formula"
)

// stallingGraphStorage is a native storage backing whose batch apply blocks
// until the apply's context ends, so the store's own derived budget is the
// deadline that fires. Its lifecycle role refuses, so a sequential fallback
// shows up as a counted call rather than a created bead. Nothing else is
// implemented: a path that strays onto another storage method panics on the
// nil embedded interface.
type stallingGraphStorage struct {
	beadslib.Storage
	applies    int
	lifecycles int
}

func (s *stallingGraphStorage) BatchApplier() (issueops.BatchApplier, error) { return s, nil }

func (s *stallingGraphStorage) ApplyBatch(ctx context.Context, _ issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	s.applies++
	<-ctx.Done()
	return issueops.ApplyBatchResult{}, ctx.Err()
}

func (s *stallingGraphStorage) IssueLifecycle() (issueops.Lifecycle, error) {
	s.lifecycles++
	return nil, errors.New("stallingGraphStorage: sequential fallback reached the lifecycle role")
}

func enableGraphApplyForTest(t *testing.T) {
	t.Helper()
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(true)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })
}

func twoStepBudgetRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true},
			{ID: "wf.step", Title: "Work", Type: "task"},
		},
	}
}

func budgetExhaustedGraphApplyError() error {
	return fmt.Errorf("native graph apply: %w: %w",
		&beads.GraphApplyBudgetExhaustedError{Budget: 298 * time.Second, Nodes: 29, Edges: 60},
		fmt.Errorf("adding edge a->b: failed to check for dependency cycle: %w", context.DeadlineExceeded))
}

func TestInstantiateDoesNotReapplyANativeGraphApplyThatSpentItsOwnBudget(t *testing.T) {
	enableGraphApplyForTest(t)
	synctest.Test(t, func(t *testing.T) {
		storage := &stallingGraphStorage{}
		store := beads.NewNativeDoltStoreOverStorageForTest(storage)

		start := time.Now()
		_, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), twoStepBudgetRecipe(), Options{})
		elapsed := time.Since(start)

		if storage.applies != 1 {
			t.Errorf("graph applies = %d, want 1 (no replay of a spent budget)", storage.applies)
		}
		if storage.lifecycles != 0 {
			t.Errorf("lifecycle role reached %d times, want 0 (no sequential fallback)", storage.lifecycles)
		}
		var exhausted *beads.GraphApplyBudgetExhaustedError
		if !errors.As(err, &exhausted) {
			t.Fatalf("Instantiate error = %v, want a *beads.GraphApplyBudgetExhaustedError", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false, want true", err)
		}
		if elapsed != exhausted.Budget {
			t.Errorf("elapsed = %s, want exactly one budget %s (no retry delay, no second budget)", elapsed, exhausted.Budget)
		}
	})
}

func TestInstantiateReturnsBudgetExhaustedWithoutReplayOrFallback(t *testing.T) {
	enableGraphApplyForTest(t)
	synctest.Test(t, func(t *testing.T) {
		store := &graphApplySpyStore{
			MemStore: beads.NewMemStore(),
			errs:     []error{budgetExhaustedGraphApplyError(), budgetExhaustedGraphApplyError()},
		}
		_, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), twoStepBudgetRecipe(), Options{})
		assertBudgetExhaustedSurfaced(t, store, err, 1)
	})
}

func TestInstantiateReturnsBudgetExhaustedRetryWithoutFallback(t *testing.T) {
	enableGraphApplyForTest(t)
	synctest.Test(t, func(t *testing.T) {
		store := &graphApplySpyStore{
			MemStore: beads.NewMemStore(),
			errs: []error{
				errors.New("native graph apply: read tcp 127.0.0.1:3307: i/o timeout"),
				budgetExhaustedGraphApplyError(),
			},
		}
		_, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), twoStepBudgetRecipe(), Options{})
		assertBudgetExhaustedSurfaced(t, store, err, 2)
	})
}

func assertBudgetExhaustedSurfaced(t *testing.T, store *graphApplySpyStore, err error, wantCalls int) {
	t.Helper()
	if store.calls != wantCalls {
		t.Errorf("ApplyGraphPlan calls = %d, want %d", store.calls, wantCalls)
	}
	var exhausted *beads.GraphApplyBudgetExhaustedError
	if !errors.As(err, &exhausted) {
		t.Errorf("Instantiate error = %v, want a *beads.GraphApplyBudgetExhaustedError", err)
	}
	open, listErr := store.ListOpen()
	if listErr != nil {
		t.Fatalf("ListOpen: %v", listErr)
	}
	if len(open) != 0 {
		t.Errorf("sequential fallback created %d beads, want 0", len(open))
	}
}

func TestIsTransientGraphApplyErrorRejectsBudgetExhausted(t *testing.T) {
	budgetErr := budgetExhaustedGraphApplyError()
	for name, err := range map[string]error{
		"direct":  budgetErr,
		"wrapped": fmt.Errorf("instantiating formula %q: %w", "wf", budgetErr),
	} {
		if isTransientGraphApplyError(err) {
			t.Errorf("%s: isTransientGraphApplyError(%v) = true, want false", name, err)
		}
	}
}

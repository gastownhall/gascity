package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// nestedScopeShapes returns the nested check-with-children shapes of issue
// #9412: each entry is one authored formula whose compile mints scope-checks
// for members of nested iteration scopes.
func nestedScopeShapes() []struct {
	name  string
	steps []*formula.Step
} {
	return []struct {
		name  string
		steps []*formula.Step
	}{
		{name: "two_level", steps: dispatchNestedCheckSteps(true)},
		{name: "three_level", steps: dispatchThreeLevelNestedCheckSteps()},
		{name: "retry_in_inner_body", steps: dispatchNestedCheckWithRetrySteps()},
		{name: "o1_parallel", steps: dispatchNestedCheckSteps(false)},
	}
}

// dispatchNestedCheckSteps builds the nested-check shape: an outer check loop
// whose body holds an inner check loop (children i1) and o1. With
// o1NeedsInner, o1 waits on the inner control.
func dispatchNestedCheckSteps(o1NeedsInner bool) []*formula.Step {
	o1 := &formula.Step{ID: "o1", Title: "Outer work after the inner loop"}
	if o1NeedsInner {
		o1.Needs = []string{"inner"}
	}
	return []*formula.Step{
		{
			ID:    "outer",
			Title: "Outer loop",
			Ralph: &formula.RalphSpec{MaxAttempts: 2, Check: &formula.RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
			Children: []*formula.Step{
				{
					ID:       "inner",
					Title:    "Inner loop",
					Ralph:    &formula.RalphSpec{MaxAttempts: 2, Check: &formula.RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
					Children: []*formula.Step{{ID: "i1", Title: "Inner work"}},
				},
				o1,
			},
		},
	}
}

// dispatchThreeLevelNestedCheckSteps nests a check three levels deep.
func dispatchThreeLevelNestedCheckSteps() []*formula.Step {
	innermost := &formula.Step{
		ID:       "inner",
		Title:    "Innermost loop",
		Ralph:    &formula.RalphSpec{MaxAttempts: 2, Check: &formula.RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
		Children: []*formula.Step{{ID: "i1", Title: "Innermost work"}},
	}
	mid := &formula.Step{
		ID:       "mid",
		Title:    "Mid loop",
		Ralph:    &formula.RalphSpec{MaxAttempts: 2, Check: &formula.RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
		Children: []*formula.Step{innermost},
	}
	return []*formula.Step{
		{
			ID:       "outer",
			Title:    "Outer loop",
			Ralph:    &formula.RalphSpec{MaxAttempts: 2, Check: &formula.RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
			Children: []*formula.Step{mid},
		},
	}
}

// dispatchNestedCheckWithRetrySteps adds a retry-managed step to the inner
// loop's body.
func dispatchNestedCheckWithRetrySteps() []*formula.Step {
	return []*formula.Step{
		{
			ID:    "outer",
			Title: "Outer loop",
			Ralph: &formula.RalphSpec{MaxAttempts: 2, Check: &formula.RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
			Children: []*formula.Step{
				{
					ID:    "inner",
					Title: "Inner loop",
					Ralph: &formula.RalphSpec{MaxAttempts: 2, Check: &formula.RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
					Children: []*formula.Step{
						{ID: "i1", Title: "Inner work"},
						{ID: "r1", Title: "Retry work", Retry: &formula.RetrySpec{MaxAttempts: 2}},
					},
				},
			},
		},
	}
}

// compileNestedShape expands and graph-controls one of the shapes above and
// returns every step keyed by ID.
func compileNestedShape(t *testing.T, steps []*formula.Step) map[string]*formula.Step {
	t.Helper()
	expanded, err := formula.ApplyRetries(steps)
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}
	expanded, err = formula.ApplyRalph(expanded)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}
	f := &formula.Formula{Steps: expanded}
	formula.ApplyGraphControls(f)
	byID := make(map[string]*formula.Step)
	var collect func([]*formula.Step)
	collect = func(steps []*formula.Step) {
		for _, s := range steps {
			byID[s.ID] = s
			if len(s.Children) > 0 {
				collect(s.Children)
			}
		}
	}
	collect(f.Steps)
	return byID
}

// TestNestedCheckScopeChecksMatchExactlyOneBody (issue #9412 AC-3): every
// scope-check minted for a nested check-with-children body must resolve
// exactly one scope body through the runtime's matchesScopeRef predicate.
// Before the fix, the inner member's scope-check named the outer iteration,
// and the inner scope body carried the outer scope ref, so a check could
// match two bodies.
func TestNestedCheckScopeChecksMatchExactlyOneBody(t *testing.T) {
	for _, shape := range nestedScopeShapes() {
		t.Run(shape.name, func(t *testing.T) {
			byID := compileNestedShape(t, shape.steps)

			var bodies, checks []*formula.Step
			for _, s := range byID {
				switch {
				case s.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindScope &&
					s.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleBody:
					bodies = append(bodies, s)
				case s.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindScopeCheck:
					checks = append(checks, s)
				}
			}
			if len(checks) == 0 {
				t.Fatalf("shape %s minted no scope-checks", shape.name)
			}
			if len(bodies) == 0 {
				t.Fatalf("shape %s has no scope bodies", shape.name)
			}
			for _, check := range checks {
				scopeRef := check.Metadata[beadmeta.ScopeRefMetadataKey]
				if scopeRef == "" {
					t.Errorf("%s carries no gc.scope_ref", check.ID)
					continue
				}
				matches := 0
				for _, body := range bodies {
					if matchesScopeRef(beads.Bead{Metadata: body.Metadata}, scopeRef) {
						matches++
					}
				}
				if matches != 1 {
					t.Errorf("%s gc.scope_ref = %q matched %d scope bodies, want exactly 1", check.ID, scopeRef, matches)
				}
			}
		})
	}
}

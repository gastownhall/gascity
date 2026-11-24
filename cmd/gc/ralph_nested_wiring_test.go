package main

// Wiring tests for a check-with-children nested in another check loop's body
// (issue #9412). Before the fix, the compile stamped the inner iteration scope
// and its members with the outer iteration's identity, the inner member's
// scope-check evaluated the outer scope and answered "continue", and the
// workflow never finished. These tests drive the fixed compile through the
// real control dispatcher.
//
// Like the other wiring tests, these mutate package-level seams, so none of
// them is parallel.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// wiringChildCheckTOML writes the check block for a [[steps.children]] entry.
func wiringChildCheckTOML(id string, maxAttempts int) string {
	return fmt.Sprintf("[steps.children.check]\nmax_attempts = %d\n\n[steps.children.check.check]\nmode = \"exec\"\npath = \".gc/scripts/%s-check.sh\"\ntimeout = \"30s\"\n\n", maxAttempts, id)
}

// wiringNestedCheckTOML builds the nested-check formula of issue #9412: an
// outer check loop whose body holds an inner check loop (children i1) and o1.
// With sequential true, o1 needs the inner control; otherwise o1 runs parallel
// to the inner loop. Both loops get their own check script, named after the
// loop id, so checkPassIter steers each one independently.
func wiringNestedCheckTOML(sequential bool) string {
	var b strings.Builder
	b.WriteString("[[steps]]\nid = \"outer\"\ntitle = \"Outer loop\"\n\n")
	b.WriteString(wiringCheckTOML("outer", 2))
	b.WriteString("[[steps.children]]\nid = \"inner\"\ntitle = \"Inner loop\"\n\n")
	b.WriteString(wiringChildCheckTOML("inner", 2))
	b.WriteString("[[steps.children.children]]\nid = \"i1\"\ntitle = \"Inner work\"\n\n")
	b.WriteString("[[steps.children]]\nid = \"o1\"\ntitle = \"Outer work after the inner loop\"\n")
	if sequential {
		b.WriteString("needs = [\"inner\"]\n")
	}
	return wiringFormulaTOML(b.String())
}

// TestRalphFinalizeNestedChecksBothPass (issue #9412 AC-5): with both checks
// passing on their first iteration, the inner iteration scope and the inner
// control close pass, then o1, then the outer control, and the workflow root
// closes pass with no bead left open.
func TestRalphFinalizeNestedChecksBothPass(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sequential bool
	}{
		{name: "o1_after_inner", sequential: true},
		{name: "o1_parallel", sequential: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := runWiringWorkflow(t, wiringScenario{
				formula:       wiringNestedCheckTOML(tc.sequential),
				checkPassIter: map[string]int{"outer": 1, "inner": 1},
			})

			innerScope := res.get(t, "outer.iteration.1.inner.iteration.1")
			ralphAssertOutcome(t, "inner iteration scope", innerScope, "pass")
			innerControl := res.get(t, "outer.iteration.1.inner")
			ralphAssertOutcome(t, "inner control", innerControl, "pass")
			o1 := res.get(t, "outer.iteration.1.o1")
			ralphAssertOutcome(t, "outer member o1", o1, "pass")
			if tc.sequential {
				// o1 waits on the inner control's scope-check, so it can only
				// close after both the inner scope and the inner control.
				if !o1.UpdatedAt.After(innerControl.UpdatedAt) {
					t.Errorf("o1 updated at %v, inner control at %v, want o1 later", o1.UpdatedAt, innerControl.UpdatedAt)
				}
				if !o1.UpdatedAt.After(innerScope.UpdatedAt) {
					t.Errorf("o1 updated at %v, inner scope at %v, want o1 later", o1.UpdatedAt, innerScope.UpdatedAt)
				}
			}

			control := res.get(t, "outer")
			ralphAssertOutcome(t, "outer control", control, "pass")
			res.assertControlPassedBeforeFinalizer(t, control)
			ralphAssertOutcome(t, "workflow root", res.root(t), "pass")
			res.assertSourceFailureKeysEmpty(t)
			assertAllWorkflowBeadsClosed(t, res)
		})
	}
}

// assertAllWorkflowBeadsClosed fails when any bead of the workflow molecule
// (the root included) is not closed.
func assertAllWorkflowBeadsClosed(t *testing.T, res *wiringResult) {
	t.Helper()
	for _, b := range append([]beads.Bead{res.root(t)}, res.all...) {
		if b.Status != "closed" {
			t.Errorf("bead %s (%s) status = %q, want closed", b.ID, beadRef(b), b.Status)
		}
	}
}

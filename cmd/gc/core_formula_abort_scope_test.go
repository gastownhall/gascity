package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
)

const (
	// corePolecatScope is the id of the scope body mol-polecat-base declares once
	// for every formula that extends it. A member's gc.scope_ref must equal it.
	corePolecatScope = "body"
	// corePolecatOnFail is the failure policy of every scope member: a member that
	// fails skips the members still open and finalizes the workflow failed.
	corePolecatOnFail = "abort_scope"
)

// corePolecatVariants are the core formulas that extend mol-polecat-base, with
// the terminal step each adds after the shared self-review.
var corePolecatVariants = []struct{ formula, terminal string }{
	{"mol-polecat-commit", "commit-and-push"},
	{"mol-polecat-report", "write-report"},
}

// corePolecatMembers are the steps of a polecat workflow that belong to the
// abort scope. load-context is the only step left out: it runs ahead of every
// step that can stop the workflow, so there is nothing for an abort to skip.
func corePolecatMembers(terminal string) []string {
	return []string{"workspace-setup", "preflight-tests", "implement", "self-review", terminal}
}

// corePolecatSteps compiles a core formula and returns its steps.
func corePolecatSteps(t *testing.T, name string) []formula.RecipeStep {
	t.Helper()
	recipe, err := formula.CompileWithoutRuntimeVarValidation(context.Background(), name, []string{coreDrainAckFormulaDir}, coreDrainAckVars)
	if err != nil {
		t.Fatalf("compiling %s: %v", name, err)
	}
	return recipe.Steps
}

// corePolecatWorkflow returns every bead of the run's workflow except the root,
// in ID order.
func corePolecatWorkflow(t *testing.T, run coreDrainAckRun) []beads.Bead {
	t.Helper()
	members, err := molecule.ListSubtree(run.store, run.rootID)
	if err != nil {
		t.Fatalf("list %s: %v", run.rootID, err)
	}
	members = slices.DeleteFunc(members, func(b beads.Bead) bool { return b.ID == run.rootID })
	slices.SortFunc(members, func(a, b beads.Bead) int { return strings.Compare(a.ID, b.ID) })
	return members
}

// TestCorePolecatFormulasDeclareTheAbortScope pins the v2 idiom for a worker
// step that must stop its own workflow early (ga-0nj7wm). mol-polecat-base
// declares one scope body; every step that can stop the workflow, the variant's
// terminal included, is an abort_scope member of it.
//
// The terminal must be a member. A step's dependency on a scope member is
// rewritten to the member's scope-check control, which closes itself pass even
// when it aborts the scope, so a terminal outside the scope still runs after an
// earlier member hard-fails.
func TestCorePolecatFormulasDeclareTheAbortScope(t *testing.T) {
	chdirToRealPackageDir(t)

	for _, v := range corePolecatVariants {
		t.Run(v.formula, func(t *testing.T) {
			var bodies, members []string
			var notAbortScope, loadContextRef []string
			for _, step := range corePolecatSteps(t, v.formula) {
				id := strings.TrimPrefix(step.ID, v.formula+".")
				if step.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindScope && step.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleBody {
					bodies = append(bodies, id)
				}
				if id == "load-context" && step.Metadata[beadmeta.ScopeRefMetadataKey] != "" {
					loadContextRef = append(loadContextRef, step.Metadata[beadmeta.ScopeRefMetadataKey])
				}
				if step.Metadata[beadmeta.ScopeRefMetadataKey] != corePolecatScope || step.Metadata[beadmeta.ScopeRoleMetadataKey] != beadmeta.ScopeRoleMember {
					continue
				}
				members = append(members, id)
				if step.Metadata[beadmeta.OnFailMetadataKey] != corePolecatOnFail {
					notAbortScope = append(notAbortScope, id)
				}
			}

			if !slices.Equal(bodies, []string{corePolecatScope}) {
				t.Errorf("%s scope bodies = %v, want exactly [%s] (declared once in mol-polecat-base)", v.formula, bodies, corePolecatScope)
			}
			slices.Sort(members)
			want := corePolecatMembers(v.terminal)
			slices.Sort(want)
			if !slices.Equal(members, want) {
				t.Errorf("%s scope members (gc.scope_ref=%s, gc.scope_role=member) = %v, want %v", v.formula, corePolecatScope, members, want)
			}
			if len(notAbortScope) != 0 {
				t.Errorf("%s members without gc.on_fail=%s: %v", v.formula, corePolecatOnFail, notAbortScope)
			}
			if len(loadContextRef) != 0 {
				t.Errorf("%s load-context joined scope %v; it runs ahead of every exit that can stop the workflow", v.formula, loadContextRef)
			}
		})
	}
}

// TestCorePolecatScopeMembersCloseWithAnExplicitPassOutcome guards the contract
// the scope adds. A member declaring gc.on_fail=abort_scope is fail-closed: one
// closed with no gc.outcome counts as FAILED and aborts the scope. The pool
// worker prompt tells workers to close a step with a bare `gc bd close`, so each
// member's own text must say to close with gc.outcome=pass, or a workflow whose
// every step succeeded would abort at its first member.
func TestCorePolecatScopeMembersCloseWithAnExplicitPassOutcome(t *testing.T) {
	chdirToRealPackageDir(t)

	for _, v := range corePolecatVariants {
		t.Run(v.formula, func(t *testing.T) {
			members := 0
			for _, step := range corePolecatSteps(t, v.formula) {
				if step.Metadata[beadmeta.ScopeRefMetadataKey] != corePolecatScope || step.Metadata[beadmeta.ScopeRoleMetadataKey] != beadmeta.ScopeRoleMember {
					continue
				}
				members++
				if !strings.Contains(step.Description, "gc.outcome=pass") {
					t.Errorf("%s is a scope member but never tells the worker to close with gc.outcome=pass; a bare close fails it and aborts the scope", step.ID)
				}
			}
			if members == 0 {
				t.Fatalf("%s has no scope members", v.formula)
			}
		})
	}
}

// TestMolPolecatReportStopExitsCloseTheirOwnStepFailed pins the three mid-step
// exits of mol-polecat-report that stop the workflow for a human. `gc runtime
// drain-ack` first releases every claim the session still holds, so a step that
// mails and acks hands itself back to the pool and a fresh session runs it
// again (ga-rd3fgm). Each exit must close its own step as a hard failure before
// the ack, which is what trips the scope-check into the abort branch.
func TestMolPolecatReportStopExitsCloseTheirOwnStepFailed(t *testing.T) {
	chdirToRealPackageDir(t)

	steps := make(map[string]string)
	for _, step := range corePolecatSteps(t, "mol-polecat-report") {
		steps[step.ID] = step.Description
	}
	for _, id := range []string{"workspace-setup", "implement", "self-review"} {
		t.Run(id, func(t *testing.T) {
			description, ok := steps["mol-polecat-report."+id]
			if !ok {
				t.Fatalf("mol-polecat-report has no %s step", id)
			}
			var exits []string
			for _, block := range coreDrainAckBlocks(description) {
				if strings.Contains(block, "gc runtime drain-ack") {
					exits = append(exits, block)
				}
			}
			if len(exits) == 0 {
				t.Fatalf("%s has no fenced exit block that runs `gc runtime drain-ack`:\n%s", id, description)
			}
			for _, block := range exits {
				before, _, _ := strings.Cut(block, "gc runtime drain-ack")
				for _, want := range []string{"gc.outcome=fail", "gc.failure_class=hard", "gc.failure_reason="} {
					if !strings.Contains(before, want) {
						t.Errorf("%s exit must close its own step with %s before acking:\n%s", id, want, block)
					}
				}
			}
		})
	}
}

// TestCoreFormulasDoNotTellWorkersToMarkThemselvesStuck keeps the retired
// idiom out. "Mark yourself stuck" has no v2 meaning: nothing reads it, so a
// worker that follows it leaves its step open for the pool to run again.
func TestCoreFormulasDoNotTellWorkersToMarkThemselvesStuck(t *testing.T) {
	chdirToRealPackageDir(t)

	entries, err := os.ReadDir(coreDrainAckFormulaDir)
	if err != nil {
		t.Fatalf("reading core formulas: %v", err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}
		data, err := os.ReadFile(coreDrainAckFormulaDir + entry.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}
		if strings.Contains(strings.ToLower(string(data)), "mark yourself stuck") {
			t.Errorf("%s still tells a worker to \"mark yourself stuck\"; say to close its own step gc.outcome=fail and drain-ack", entry.Name())
		}
	}
}

// TestCorePolecatMemberFailureStopsTheWorkflow walks each polecat formula as a
// pool worker up to a member, hard-fails that member the way its exit block
// does, drain-acks, and serves the controls with the real dispatcher.
//
// A hard-failed member must stop the workflow: the failed step stays closed
// instead of going back to the pool, every step after it is skipped so nothing
// is left for a fresh session to run, the scope body closes failed, and the
// workflow finalizes FAILED for the escalation target.
func TestCorePolecatMemberFailureStopsTheWorkflow(t *testing.T) {
	chdirToRealPackageDir(t)

	for _, v := range corePolecatVariants {
		for _, member := range []string{"workspace-setup", "implement", "self-review"} {
			t.Run(v.formula+"/"+member, func(t *testing.T) {
				run := startCoreDrainAckRun(t, v.formula, v.formula+"."+member)
				failed := run.terminal
				run.closeStep(t, failed.ID, map[string]string{
					beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
					beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
					beadmeta.FailureReasonMetadataKey: "blocked",
				})
				run.drainAck(t)

				var problems []string
				var body beads.Bead
				for _, b := range corePolecatWorkflow(t, run) {
					if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindScope && b.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleBody {
						body = b
					}
					if b.ID != failed.ID && coreDrainAckIsWorkStep(b) && b.Status != "closed" {
						problems = append(problems, "step "+b.Metadata[beadmeta.StepRefMetadataKey]+" is still "+b.Status+" (gc.routed_to="+b.Metadata[beadmeta.RoutedToMetadataKey]+"): a fresh session would run it")
					}
				}
				if body.ID == "" {
					problems = append(problems, "the workflow has no scope body, so nothing aborts the steps after the failed one")
				} else if body.Status != "closed" || body.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
					problems = append(problems, "scope body is status="+body.Status+" gc.outcome="+body.Metadata[beadmeta.OutcomeMetadataKey]+", want closed and fail")
				}
				if step := run.get(t, failed.ID); step.Status != "closed" || step.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
					problems = append(problems, "the failed step is status="+step.Status+" gc.outcome="+step.Metadata[beadmeta.OutcomeMetadataKey]+", want it left closed and failed")
				}
				if root := run.get(t, run.rootID); root.Status != "closed" || root.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
					problems = append(problems, "workflow root is status="+root.Status+" gc.outcome="+root.Metadata[beadmeta.OutcomeMetadataKey]+", want closed and fail")
				}
				if len(problems) != 0 {
					t.Fatalf("after %s hard-failed and the session drain-acked:\n  %s\n%s", failed.Metadata[beadmeta.StepRefMetadataKey], strings.Join(problems, "\n  "), run.report(t))
				}
			})
		}
	}
}

// TestCorePolecatScopedWorkflowFinalizesPassedWhenEveryStepPasses is the other
// half of the abort contract: with every step closed gc.outcome=pass the scope
// must converge passed and the workflow must finalize passed, so putting the
// steps in an abort_scope never turns a successful run into a failed one.
func TestCorePolecatScopedWorkflowFinalizesPassedWhenEveryStepPasses(t *testing.T) {
	chdirToRealPackageDir(t)

	for _, v := range corePolecatVariants {
		t.Run(v.formula, func(t *testing.T) {
			run := startCoreDrainAckRun(t, v.formula, v.formula+"."+v.terminal)
			run.closeStep(t, run.terminal.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
			run.serveControls(t)

			var problems []string
			sawBody := false
			for _, b := range corePolecatWorkflow(t, run) {
				if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindScope && b.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleBody {
					sawBody = true
					if b.Status != "closed" || b.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
						problems = append(problems, "scope body is status="+b.Status+" gc.outcome="+b.Metadata[beadmeta.OutcomeMetadataKey]+", want closed and pass")
					}
					continue
				}
				if coreDrainAckIsWorkStep(b) && (b.Status != "closed" || b.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass) {
					problems = append(problems, "step "+b.Metadata[beadmeta.StepRefMetadataKey]+" is status="+b.Status+" gc.outcome="+b.Metadata[beadmeta.OutcomeMetadataKey]+", want closed and pass")
				}
			}
			if !sawBody {
				problems = append(problems, "the workflow has no scope body")
			}
			if root := run.get(t, run.rootID); root.Status != "closed" || root.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
				problems = append(problems, "workflow root is status="+root.Status+" gc.outcome="+root.Metadata[beadmeta.OutcomeMetadataKey]+", want closed and pass")
			}
			if len(problems) != 0 {
				t.Fatalf("after every step closed gc.outcome=pass:\n  %s\n%s", strings.Join(problems, "\n  "), run.report(t))
			}
		})
	}
}

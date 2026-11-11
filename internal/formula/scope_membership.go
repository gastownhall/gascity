package formula

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// inheritedScopeRefs maps each top-level step that names a scope to the scope
// it names. mergeSteps replaces an overridden step wholesale, so a snapshot of
// the parent steps taken before the merge is the only record of what an
// override erased; droppedScopeRefs compares it with the merged steps.
func inheritedScopeRefs(steps []*Step) map[string]string {
	refs := make(map[string]string)
	for _, step := range steps {
		if step == nil {
			continue
		}
		if ref := step.Metadata[beadmeta.ScopeRefMetadataKey]; ref != "" {
			refs[step.ID] = ref
		}
	}
	return refs
}

// validateScopeMembership rejects a merged formula in which a step names a
// scope body that does not exist. The check reads metadata only, never step
// content, and reports nothing for a formula that names no scope.
//
// droppedScopeRefs and uncoveredSinks are deliberately not called. They would
// reject an extender written before its base declared scope membership, so they
// wait for the pack that supplies the base to be live (ga-prk8k5).
func validateScopeMembership(f *Formula) error {
	problems := danglingScopeRefs(collectGraphSteps(f.Steps))
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("formula %q: scope membership:\n  - %s", f.Formula, strings.Join(problems, "\n  - "))
}

// droppedScopeRefs finds overridden steps that lost the scope their parent put
// them in.
func droppedScopeRefs(steps []*Step, inherited map[string]string) []string {
	var problems []string
	for _, step := range steps {
		if step == nil {
			continue
		}
		scope, wasMember := inherited[step.ID]
		if !wasMember || declaresScopeRef(step) {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"step %q overrides an inherited scope member and drops its gc.scope_ref %q; redeclare the membership, or set gc.scope_ref = \"\" to leave the scope on purpose",
			step.ID, scope))
	}
	return problems
}

// danglingScopeRefs finds steps whose gc.scope_ref names no scope body. Left
// alone that only surfaces at run time, when the step's scope-check cannot
// resolve its body and fails with ErrControlGraphMalformed on every run. A ref
// that holds a {{placeholder}} is skipped: cook substitutes it after Resolve,
// so it cannot be judged here.
func danglingScopeRefs(steps []*Step) []string {
	var problems []string
	for _, step := range steps {
		ref := step.Metadata[beadmeta.ScopeRefMetadataKey]
		if ref == "" || strings.Contains(ref, "{{") {
			continue
		}
		named := namedScopeStep(steps, ref)
		switch {
		case named == nil:
			problems = append(problems, fmt.Sprintf(
				"step %q has gc.scope_ref %q, but the merged formula declares no scope body named %q (gc.kind=scope, gc.scope_role=body)",
				step.ID, ref, ref))
		case !isScopeBody(named):
			problems = append(problems, fmt.Sprintf(
				"step %q has gc.scope_ref %q, but %q is not a scope body (it needs gc.kind=scope and gc.scope_role=body)",
				step.ID, ref, named.ID))
		}
	}
	return problems
}

// uncoveredSinks finds graph sinks that run downstream of a scope's members or
// body without belonging to any scope. A member's scope-check always closes
// pass, so a step outside the scope still runs after the scope aborts: only a
// member is stopped by abort_scope. Teardown work is never a sink
// (graphSinkStepIDs), and a sink unrelated to every scope is left alone.
func uncoveredSinks(top, steps []*Step) []string {
	var bodies []*Step
	stepByID := make(map[string]*Step, len(steps))
	for _, step := range steps {
		stepByID[step.ID] = step
		if isScopeBody(step) {
			bodies = append(bodies, step)
		}
	}
	if len(bodies) == 0 {
		return nil
	}

	dependents := dependentsByID(steps)
	downstream := make(map[string]map[string]bool, len(bodies))
	for _, body := range bodies {
		downstream[body.ID] = reachableFrom(scopeRootIDs(steps, body), dependents)
	}

	var problems []string
	for _, id := range graphSinkStepIDs(top) {
		sink := stepByID[id]
		if sink == nil || sink.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindScope || declaresScopeRef(sink) {
			continue
		}
		for _, body := range bodies {
			if downstream[body.ID][id] {
				problems = append(problems, fmt.Sprintf(
					"step %q is a sink downstream of scope %q but is not a member of it; declare gc.scope_ref and gc.scope_role, or set gc.scope_ref = \"\" to sit outside every scope on purpose",
					id, body.ID))
			}
		}
	}
	return problems
}

// declaresScopeRef reports whether a step states its scope membership by
// mentioning gc.scope_ref. Setting it to the empty string is the explicit
// opt-out, for a step that deliberately sits outside every scope.
func declaresScopeRef(step *Step) bool {
	_, declared := step.Metadata[beadmeta.ScopeRefMetadataKey]
	return declared
}

func isScopeBody(step *Step) bool {
	return step.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindScope &&
		step.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleBody
}

// namedScopeStep finds the step a gc.scope_ref names: one whose id, gc.step_ref
// or gc.step_id equals ref or ends in "."+ref (beadmeta.NodeIsScope). It is a
// compile-time lookup, not the runtime's. A scope body wins when several steps
// match.
func namedScopeStep(steps []*Step, ref string) *Step {
	var named *Step
	for _, step := range steps {
		if !beadmeta.NodeIsScope(step.ID, step.Metadata, ref) {
			continue
		}
		if isScopeBody(step) {
			return step
		}
		if named == nil {
			named = step
		}
	}
	return named
}

// scopeRootIDs lists the steps a scope's downstream work is measured from: the
// body itself and every step whose gc.scope_ref resolves to it.
func scopeRootIDs(steps []*Step, body *Step) []string {
	roots := []string{body.ID}
	for _, step := range steps {
		if ref := step.Metadata[beadmeta.ScopeRefMetadataKey]; ref != "" && beadmeta.NodeIsScope(body.ID, body.Metadata, ref) {
			roots = append(roots, step.ID)
		}
	}
	return roots
}

// dependentsByID maps each step ID to the steps that depend on it.
func dependentsByID(steps []*Step) map[string][]string {
	dependents := make(map[string][]string, len(steps))
	for _, step := range steps {
		for _, dep := range step.DependsOn {
			dependents[dep] = append(dependents[dep], step.ID)
		}
		for _, dep := range step.Needs {
			dependents[dep] = append(dependents[dep], step.ID)
		}
	}
	return dependents
}

// reachableFrom returns every step ID that depends, directly or transitively,
// on one of roots.
func reachableFrom(roots []string, dependents map[string][]string) map[string]bool {
	reached := make(map[string]bool)
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, next := range dependents[id] {
			if !reached[next] {
				reached[next] = true
				queue = append(queue, next)
			}
		}
	}
	return reached
}

package formula

import (
	"fmt"

	semver "github.com/Masterminds/semver/v3"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// inputLifecycleCompilerCapability is the first formula compiler that knows
// input_lifecycle; lastCompilerWithoutInputLifecycle is the graph compiler
// before it. A graph declaration must exclude the older one: it ignores the
// field and would compile the workflow as externally owned, so its input
// settlement would retire the graph's own tail.
const (
	inputLifecycleCompilerCapability  = "2.1.0"
	lastCompilerWithoutInputLifecycle = "2.0.0"
	inputLifecycleRequirement         = ">=" + inputLifecycleCompilerCapability
)

// validateInputLifecycleDeclaration checks one formula's own input_lifecycle
// declaration: the value is external or graph, and only a workflow declares
// it (expansions and aspects contribute steps to another formula's root and
// own no input).
func validateInputLifecycleDeclaration(f *Formula) []string {
	switch f.InputLifecycle {
	case "":
		return nil
	case beadmeta.InputLifecycleExternal, beadmeta.InputLifecycleGraph:
	default:
		return []string{fmt.Sprintf("input_lifecycle: invalid value %q (must be %s or %s)",
			f.InputLifecycle, beadmeta.InputLifecycleExternal, beadmeta.InputLifecycleGraph)}
	}
	if f.Type == TypeExpansion || f.Type == TypeAspect {
		return []string{fmt.Sprintf("input_lifecycle: not allowed on %s formulas (declare it on the workflow)", f.Type)}
	}
	return nil
}

// validateInputLifecycleRequirement checks a resolved formula's inherited
// input_lifecycle against its composed compiler requirements: any declaration
// needs a formulas v2 graph workflow, and graph ownership needs a requirement
// that excludes every compiler older than inputLifecycleCompilerCapability.
func validateInputLifecycleRequirement(f *Formula) error {
	if f.InputLifecycle == "" {
		return nil
	}
	if !UsesGraphCompiler(f) {
		return fmt.Errorf("formula %q: input_lifecycle requires a formulas v2 graph workflow ([requires] formula_compiler = %q)", f.Formula, inputLifecycleRequirement)
	}
	if f.InputLifecycle != beadmeta.InputLifecycleGraph {
		return nil
	}
	constraints, err := formulaCompilerConstraints(f)
	if err != nil {
		return err
	}
	previous, err := semver.NewVersion(lastCompilerWithoutInputLifecycle)
	if err != nil {
		return fmt.Errorf("formula %q: parsing compiler version: %w", f.Formula, err)
	}
	for _, candidate := range constraints {
		constraint, err := semver.NewConstraint(candidate.Raw)
		if err != nil {
			return invalidFormulaCompilerRequirement(candidate.Raw, err)
		}
		if !constraint.Check(previous) {
			return nil
		}
	}
	return fmt.Errorf("formula %q: input_lifecycle = %q requires [requires] formula_compiler = %q; older compilers ignore it and would retire the graph's own tail when its input settles",
		f.Formula, beadmeta.InputLifecycleGraph, inputLifecycleRequirement)
}

package bootstrap

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

// coreEscalationFormulas are the core-pack formulas that tell an agent to mail
// someone when it cannot finish its work. Core ships no coordinator or
// work-health role, so the recipient must be configurable and must default to
// the reserved `human` alias, which resolves in every city.
var coreEscalationFormulas = []string{
	"mol-do-work",
	"mol-polecat-base",
	"mol-polecat-commit",
	"mol-polecat-report",
	"mol-prompt-synth",
}

func TestCoreEscalationTargetDefaultsToHuman(t *testing.T) {
	// No formula_v2 gate manipulation here on purpose. The compiler v2 flag
	// already defaults to enabled — the formula package's init stores it true
	// and rollout.ForTest's defaults agree — so toggling it was a no-op that
	// only coupled this test to the legacy globals and tripped the
	// internal/bootstrap ceiling in TestLegacyFormulaV2MechanismFrozen. That
	// freeze scan is textual and counts comments, so this note deliberately
	// does not name the legacy identifiers.
	for _, name := range coreEscalationFormulas {
		t.Run(name, func(t *testing.T) {
			recipe, err := formula.CompileWithoutRuntimeVarValidation(
				context.Background(), name, coreFormulaSearchPaths(t), nil)
			if err != nil {
				t.Fatalf("compile %s: %v", name, err)
			}

			def, ok := recipe.Vars["escalation_target"]
			if !ok || def == nil || def.Default == nil {
				t.Fatalf("%s: escalation_target must be declared with a default "+
					"(variants inherit it from mol-polecat-base)", name)
			}
			if *def.Default != "human" {
				t.Fatalf("%s: escalation_target default = %q, want %q", name, *def.Default, "human")
			}

			// With defaults applied, every step must render a resolvable
			// recipient — no leftover placeholder, no unshipped gastown role.
			vars := map[string]string{}
			for varName, varDef := range recipe.Vars {
				if varDef != nil && varDef.Default != nil {
					vars[varName] = *varDef.Default
				}
			}
			for _, step := range recipe.Steps {
				rendered := formula.Substitute(step.Description, vars)
				if strings.Contains(rendered, "{{escalation_target}}") {
					t.Errorf("%s/%s: escalation_target left unsubstituted", name, step.ID)
				}
				if strings.Contains(strings.ToLower(rendered), "witness") {
					t.Errorf("%s/%s: still routes escalation to witness, a role core does not ship", name, step.ID)
				}
			}
		})
	}
}

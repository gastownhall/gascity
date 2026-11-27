package dispatch

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

func nestedCheckStep(id string, children ...*formula.Step) *formula.Step {
	return &formula.Step{
		ID:       id,
		Title:    id,
		Type:     "task",
		Ralph:    &formula.RalphSpec{MaxAttempts: 2, Check: &formula.RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
		Children: children,
	}
}

func nestedPlainStep(id string, needs ...string) *formula.Step {
	return &formula.Step{ID: id, Title: id, Type: "task", Needs: needs}
}

// compileNestedCheckSteps runs the compile stages that shape loop membership
// (retries, checks, graph controls) and returns the flat step list.
func compileNestedCheckSteps(t *testing.T, steps []*formula.Step) []*formula.Step {
	t.Helper()
	retried, err := formula.ApplyRetries(steps)
	if err != nil {
		t.Fatalf("ApplyRetries: %v", err)
	}
	expanded, err := formula.ApplyRalph(retried)
	if err != nil {
		t.Fatalf("ApplyRalph: %v", err)
	}
	f := &formula.Formula{Steps: expanded}
	formula.ApplyGraphControls(f)
	return f.Steps
}

// compiledStepBeads projects compiled steps onto the beads instantiation
// creates from them: the step's metadata, with gc.step_ref filled from the
// step ID where the compile left it empty.
func compiledStepBeads(steps []*formula.Step) []beads.Bead {
	out := make([]beads.Bead, 0, len(steps))
	for _, step := range steps {
		meta := make(map[string]string, len(step.Metadata)+1)
		for k, v := range step.Metadata {
			meta[k] = v
		}
		if meta[beadmeta.StepRefMetadataKey] == "" {
			meta[beadmeta.StepRefMetadataKey] = step.ID
		}
		out = append(out, beads.Bead{ID: step.ID, Metadata: meta})
	}
	return out
}

// TestNestedCheckScopeChecksResolveExactlyOneBody pins #9412 at the runtime
// boundary: every scope-check the compile mints for a check-with-children
// nested in a check body resolves, through matchesScopeRef, to exactly one
// scope body, so no scope-check can evaluate or close the wrong iteration.
func TestNestedCheckScopeChecksResolveExactlyOneBody(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		steps []*formula.Step
	}{
		{"two_level", []*formula.Step{nestedCheckStep("outer",
			nestedCheckStep("inner", nestedPlainStep("i1")),
			nestedPlainStep("o1", "inner"),
		)}},
		{"three_level", []*formula.Step{nestedCheckStep("outer",
			nestedCheckStep("mid", nestedCheckStep("inner", nestedPlainStep("i1"))),
		)}},
		{"retry_in_inner_body", []*formula.Step{nestedCheckStep("outer",
			nestedCheckStep("inner", &formula.Step{ID: "r", Title: "r", Type: "task", Retry: &formula.RetrySpec{MaxAttempts: 2}}),
			nestedPlainStep("o1", "inner"),
		)}},
		{"o1_parallel", []*formula.Step{nestedCheckStep("outer",
			nestedCheckStep("inner", nestedPlainStep("i1")),
			nestedPlainStep("o1"),
		)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			all := compiledStepBeads(compileNestedCheckSteps(t, tc.steps))
			var bodies []beads.Bead
			for _, b := range all {
				if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindScope && b.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleBody {
					bodies = append(bodies, b)
				}
			}
			checks := 0
			for _, check := range all {
				if check.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindScopeCheck {
					continue
				}
				checks++
				scopeRef := check.Metadata[beadmeta.ScopeRefMetadataKey]
				var matched []string
				for _, body := range bodies {
					if matchesScopeRef(body, scopeRef) {
						matched = append(matched, body.ID)
					}
				}
				if len(matched) != 1 {
					t.Errorf("scope-check %s (gc.scope_ref=%q) matches bodies %v, want exactly one", check.ID, scopeRef, matched)
				}
			}
			if checks == 0 {
				t.Fatal("compile minted no scope-checks")
			}
		})
	}
}

// TestNestedCheckCompileIterationMatchesRuntimeRespawn pins #9412's parity
// contract: outer iteration 1 (the compile) and outer iteration 2
// (buildAttemptRecipe with its seeded inner loop) give the inner control, the
// inner iteration scope, the inner member and its scope-check the same
// membership identity, and each gc.scope_ref names the bead's innermost
// enclosing iteration root.
func TestNestedCheckCompileIterationMatchesRuntimeRespawn(t *testing.T) {
	t.Parallel()

	compiled := compileNestedCheckSteps(t, []*formula.Step{nestedCheckStep("outer",
		nestedCheckStep("inner", nestedPlainStep("i1")),
		nestedPlainStep("o1", "inner"),
	)})
	compileMeta := make(map[string]map[string]string, len(compiled))
	var spec *formula.Step
	for _, step := range compiled {
		compileMeta[step.ID] = step.Metadata
		if step.ID == "outer.spec" {
			spec = &formula.Step{}
			if err := json.Unmarshal([]byte(step.Description), spec); err != nil {
				t.Fatalf("decoding outer spec: %v", err)
			}
		}
	}
	if spec == nil {
		t.Fatal("compile produced no outer.spec")
	}
	control := beads.Bead{ID: "outer", Metadata: map[string]string{
		beadmeta.StepIDMetadataKey:  "outer",
		beadmeta.StepRefMetadataKey: "outer",
	}}
	recipe := buildAttemptRecipe(spec, control, 2)
	runtimeMeta := make(map[string]map[string]string, len(recipe.Steps))
	for _, step := range recipe.Steps {
		runtimeMeta[step.ID] = step.Metadata
	}

	kindOf := func(meta map[string]string) string {
		if kind := meta[beadmeta.KindMetadataKey]; kind != "" {
			return kind
		}
		return beadmeta.KindTask
	}
	for _, tc := range []struct {
		suffix    string // ID below the outer iteration root
		enclosing string // innermost enclosing iteration root below it; "" = the outer iteration root itself, "-" = none (no gc.scope_ref)
	}{
		{suffix: ".inner", enclosing: ""},
		{suffix: ".inner.iteration.1", enclosing: "-"},
		{suffix: ".inner.iteration.1.i1", enclosing: ".inner.iteration.1"},
		{suffix: ".inner.iteration.1.i1-scope-check", enclosing: ".inner.iteration.1"},
	} {
		for _, side := range []struct {
			name string
			root string
			meta map[string]map[string]string
		}{
			{"compile", "outer.iteration.1", compileMeta},
			{"runtime", "outer.iteration.2", runtimeMeta},
		} {
			meta := side.meta[side.root+tc.suffix]
			if meta == nil {
				t.Errorf("%s: missing %s%s", side.name, side.root, tc.suffix)
				continue
			}
			scopeRef, hasScopeRef := meta[beadmeta.ScopeRefMetadataKey]
			switch tc.enclosing {
			case "-":
				if hasScopeRef {
					t.Errorf("%s %s%s gc.scope_ref = %q, want absent", side.name, side.root, tc.suffix, scopeRef)
				}
			default:
				rootMeta := side.meta[side.root+tc.enclosing]
				if rootMeta == nil {
					t.Errorf("%s: missing enclosing root %s%s", side.name, side.root, tc.enclosing)
					continue
				}
				if want := rootMeta[beadmeta.StepRefMetadataKey]; scopeRef == "" || scopeRef != want {
					t.Errorf("%s %s%s gc.scope_ref = %q, want the enclosing root's gc.step_ref %q", side.name, side.root, tc.suffix, scopeRef, want)
				}
			}
		}
		compile, runtime := compileMeta["outer.iteration.1"+tc.suffix], runtimeMeta["outer.iteration.2"+tc.suffix]
		if compile == nil || runtime == nil {
			continue
		}
		if kc, kr := kindOf(compile), kindOf(runtime); kc != kr {
			t.Errorf("%s gc.kind: compile %q, runtime %q", tc.suffix, kc, kr)
		}
		for _, key := range []string{
			beadmeta.ScopeRoleMetadataKey,
			beadmeta.StepIDMetadataKey,
			beadmeta.RalphStepIDMetadataKey,
			beadmeta.OnFailMetadataKey,
		} {
			if compile[key] != runtime[key] {
				t.Errorf("%s %s: compile %q, runtime %q", tc.suffix, key, compile[key], runtime[key])
			}
		}
		_, compileScoped := compile[beadmeta.ScopeRefMetadataKey]
		_, runtimeScoped := runtime[beadmeta.ScopeRefMetadataKey]
		if compileScoped != runtimeScoped {
			t.Errorf("%s gc.scope_ref present: compile %v, runtime %v", tc.suffix, compileScoped, runtimeScoped)
		}
	}
	if !strings.HasPrefix(runtimeMeta["outer.iteration.2.inner.iteration.1"][beadmeta.ControlForMetadataKey], "outer.iteration.2.inner") {
		t.Errorf("runtime inner scope gc.control_for = %q, want the iteration-2 inner control", runtimeMeta["outer.iteration.2.inner.iteration.1"][beadmeta.ControlForMetadataKey])
	}
}

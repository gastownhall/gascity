package formula

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

var (
	graphV2DirectReservedRefPattern = regexp.MustCompile(`\{\{-?\s*\.?\s*(issue|bead_id|convoy_id)\s*(?:\|[^}]*)?-?\}\}`)
	graphV2IndexReservedRefPattern  = regexp.MustCompile(`\{\{-?[^}]*\bindex\b[^}]*["'](issue|bead_id|convoy_id)["'][^}]*-?\}\}`)
)

var graphV2ReservedVarNames = map[string]struct{}{
	"convoy_id": {},
	"issue":     {},
	"bead_id":   {},
}

// ValidateGraphV2ReservedSymbols enforces the graph.v2 reserved input contract.
// Graph formulas may reference convoy_id only for targeted invocations. The
// legacy issue variable is tolerated as a deprecated one-release compat alias
// (resolved to the single tracked member of the input convoy, see #2941);
// bead_id never has a graph.v2 compatibility mapping.
func ValidateGraphV2ReservedSymbols(f *Formula, allowConvoyReference bool) error {
	errs := graphV2ReservedSymbolErrors(f, allowConvoyReference)
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("formulas v2 reserved variable validation failed:\n  - %s", strings.Join(errs, "\n  - "))
}

// ValidateGraphV2ReservedSymbolsTransitively enforces graph.v2 reserved input
// rules across the resolved formula plus any expansion templates and aspect
// advice that can materialize into its graph before condition filtering.
func ValidateGraphV2ReservedSymbolsTransitively(f *Formula, parser *Parser, allowConvoyReference bool) error {
	scanner := graphV2TransitiveReferenceScanner{
		parser:               parser,
		allowConvoyReference: allowConvoyReference,
		visited:              make(map[string]bool),
	}
	if err := scanner.scanFormula("", f); err != nil {
		return err
	}
	if len(scanner.errs) == 0 {
		return nil
	}
	sort.Strings(scanner.errs)
	return fmt.Errorf("formulas v2 reserved variable validation failed:\n  - %s", strings.Join(scanner.errs, "\n  - "))
}

// ValidateGraphV2ExpandedFormula enforces graph.v2-only constraints after
// control flow, advice, and expansion templates have been materialized.
func ValidateGraphV2ExpandedFormula(f *Formula, allowConvoyReference bool) error {
	if f == nil || !UsesGraphCompiler(f) {
		return nil
	}
	errs := graphV2ReservedSymbolErrors(f, allowConvoyReference)
	collectGraphV2DrainValidationErrors("steps", f.Steps, &errs)
	collectGraphV2DrainValidationErrors("template", f.Template, &errs)
	collectGraphV2DescriptionFileValidationErrors("steps", f.Steps, &errs)
	collectGraphV2DescriptionFileValidationErrors("template", f.Template, &errs)
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("formulas v2 expanded formula validation failed:\n  - %s", strings.Join(errs, "\n  - "))
}

// ValidateGraphV2RecipeReservedSymbols validates reserved references after a
// graph.v2 formula has been flattened into a recipe.
func ValidateGraphV2RecipeReservedSymbols(recipe *Recipe, allowConvoyReference bool) error {
	errs := graphV2RecipeReservedSymbolErrors(recipe, allowConvoyReference, nil)
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("formulas v2 reserved variable validation failed:\n  - %s", strings.Join(errs, "\n  - "))
}

// GraphV2FormulaReferencesInputConvoy reports whether a formula requires a
// targeted graph.v2 invocation because it references convoy_id (or the
// deprecated issue alias) or contains a drain control.
func GraphV2FormulaReferencesInputConvoy(f *Formula) bool {
	if f == nil {
		return false
	}
	if GraphV2FormulaHasDrain(f) {
		return true
	}
	found := false
	_ = ValidateGraphV2ReservedSymbolsWithVisitor(f, true, func(name string) {
		if graphV2NameRequiresInputConvoy(name) {
			found = true
		}
	})
	return found
}

// graphV2NameRequiresInputConvoy reports whether a reserved runtime variable
// reference forces a targeted graph.v2 invocation. convoy_id is the canonical
// input; issue is the deprecated compat alias that resolves to the single
// tracked member of the input convoy and therefore also needs one.
func graphV2NameRequiresInputConvoy(name string) bool {
	return name == "convoy_id" || name == "issue"
}

// GraphV2FormulaReferencesInputConvoyTransitively reports whether a resolved
// graph.v2 formula or any expansion/aspect formula it can materialize requires
// an input convoy before condition filtering can remove steps.
func GraphV2FormulaReferencesInputConvoyTransitively(f *Formula, parser *Parser) (bool, error) {
	scanner := graphV2TransitiveReferenceScanner{
		parser:               parser,
		allowConvoyReference: true,
		visited:              make(map[string]bool),
		visit: func(_, _ string) {
			// Keep scanning after finding convoy_id so callers get load/resolve
			// errors from every reachable expansion path.
		},
	}
	if err := scanner.scanFormula("", f); err != nil {
		return false, err
	}
	return scanner.requiresInputConvoy, nil
}

// GraphV2RecipeReferencesInputConvoy reports whether a compiled graph.v2
// recipe requires a targeted invocation because it references convoy_id (or
// the deprecated issue alias) or has a drain control step.
func GraphV2RecipeReferencesInputConvoy(recipe *Recipe) bool {
	if recipe == nil {
		return false
	}
	if GraphV2RecipeHasDrain(recipe) {
		return true
	}
	found := false
	_ = graphV2RecipeReservedSymbolErrors(recipe, true, func(_, name string) {
		if graphV2NameRequiresInputConvoy(name) {
			found = true
		}
	})
	return found
}

// GraphV2LegacyIssueRefsTransitively returns the template paths in a resolved
// formula — plus any expansion templates and aspect advice that can
// materialize into its graph — that reference or declare the deprecated
// legacy work-bead variables (issue, bead_id). In graph.v2 formulas, issue is
// tolerated for one release as a compat alias (#2941); callers should surface
// every returned path as a deprecation warning. The scan does not gate on the
// formula's compiler contract so whole-pack audits can also flag legacy
// formulas that would poison graph.v2 compositions.
func GraphV2LegacyIssueRefsTransitively(f *Formula, parser *Parser) []string {
	if f == nil {
		return nil
	}
	var refs []string
	scanner := graphV2TransitiveReferenceScanner{
		parser:               parser,
		allowConvoyReference: true,
		visited:              make(map[string]bool),
		visit:                graphV2LegacyIssueRefCollector(&refs),
	}
	// Load/resolve errors on expansion paths surface through the validation
	// entry points; this scan only gathers deprecation warnings.
	_ = scanner.scanFormula("", f)
	sort.Strings(refs)
	return refs
}

// GraphV2RecipeLegacyIssueRefs returns the deprecated legacy work-bead
// references found in a compiled graph.v2 recipe.
func GraphV2RecipeLegacyIssueRefs(recipe *Recipe) []string {
	var refs []string
	_ = graphV2RecipeReservedSymbolErrors(recipe, true, graphV2LegacyIssueRefCollector(&refs))
	sort.Strings(refs)
	return refs
}

func graphV2LegacyIssueRefCollector(refs *[]string) func(path, name string) {
	return func(path, name string) {
		if name != "issue" && name != "bead_id" {
			return
		}
		*refs = append(*refs, path+": "+name)
	}
}

// GraphV2FormulaHasDrain reports whether any step in a formula uses drain.
func GraphV2FormulaHasDrain(f *Formula) bool {
	if f == nil {
		return false
	}
	return graphV2StepsHaveDrain(f.Steps) || graphV2StepsHaveDrain(f.Template)
}

// GraphV2RecipeHasDrain reports whether any compiled recipe step is a drain
// control step.
func GraphV2RecipeHasDrain(recipe *Recipe) bool {
	if recipe == nil {
		return false
	}
	for _, step := range recipe.Steps {
		if strings.TrimSpace(step.Metadata[beadmeta.KindMetadataKey]) == "drain" {
			return true
		}
	}
	return false
}

func graphV2StepsHaveDrain(steps []*Step) bool {
	for _, step := range steps {
		if step == nil {
			continue
		}
		if step.Drain != nil {
			return true
		}
		if graphV2StepsHaveDrain(step.Children) {
			return true
		}
		if step.Loop != nil && graphV2StepsHaveDrain(step.Loop.Body) {
			return true
		}
	}
	return false
}

func graphV2ReservedSymbolErrors(f *Formula, allowConvoyReference bool) []string {
	if f == nil || !UsesGraphCompiler(f) {
		return nil
	}
	var errs []string
	collectGraphV2ReservedRefsInFormula("", f, allowConvoyReference, &errs, nil, true)
	return errs
}

func collectGraphV2DrainValidationErrors(prefix string, steps []*Step, errs *[]string) {
	for i, step := range steps {
		if step == nil {
			continue
		}
		stepPrefix := fmt.Sprintf("%s[%d] (%s)", prefix, i, step.ID)
		if step.Drain != nil {
			validateDrain(step.Drain, errs, stepPrefix, step, true)
		}
		collectGraphV2DrainValidationErrors(stepPrefix+".children", step.Children, errs)
		if step.Loop != nil {
			collectGraphV2DrainValidationErrors(stepPrefix+".loop.body", step.Loop.Body, errs)
		}
	}
}

func collectGraphV2DescriptionFileValidationErrors(prefix string, steps []*Step, errs *[]string) {
	for i, step := range steps {
		if step == nil {
			continue
		}
		stepPrefix := fmt.Sprintf("%s[%d] (%s)", prefix, i, step.ID)
		if path := strings.TrimSpace(step.DescriptionFile); path != "" {
			*errs = append(*errs, fmt.Sprintf("%s.description_file %q was not resolved", stepPrefix, path))
		}
		collectGraphV2DescriptionFileValidationErrors(stepPrefix+".children", step.Children, errs)
		if step.Loop != nil {
			collectGraphV2DescriptionFileValidationErrors(stepPrefix+".loop.body", step.Loop.Body, errs)
		}
	}
}

// ValidateGraphV2ReservedSymbolsWithVisitor validates graph.v2 reserved-symbol
// usage and calls visit for every reserved runtime variable reference it finds.
func ValidateGraphV2ReservedSymbolsWithVisitor(f *Formula, allowConvoyReference bool, visit func(name string)) error {
	if f == nil {
		return nil
	}
	var pathVisit func(path, name string)
	if visit != nil {
		pathVisit = func(_, name string) { visit(name) }
	}
	var errs []string
	collectGraphV2ReservedRefsInFormula("", f, allowConvoyReference, &errs, pathVisit, false)
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("formulas v2 reserved variable validation failed:\n  - %s", strings.Join(errs, "\n  - "))
}

func collectGraphV2ReservedRefsInFormula(prefix string, f *Formula, allowConvoyReference bool, errs *[]string, visit func(path, name string), checkVarNames bool) {
	if f == nil {
		return
	}
	if checkVarNames {
		for name := range f.Vars {
			if !isGraphV2ReservedVarName(name) {
				continue
			}
			declPath := graphV2Path(prefix, "vars."+name)
			if visit != nil {
				visit(declPath, strings.TrimSpace(name))
			}
			if strings.TrimSpace(name) == "issue" {
				// Deprecated one-release compat alias (#2941): declaring
				// vars.issue is tolerated; the runtime injects the single
				// tracked member of the input convoy.
				continue
			}
			*errs = append(*errs, fmt.Sprintf("%s: formulas v2 reserved variable cannot be declared", declPath))
		}
	}
	collectGraphV2ReservedRefsInStringWithVisitor(graphV2Path(prefix, "description"), f.Description, allowConvoyReference, errs, visit)
	for name, def := range f.Vars {
		if def == nil {
			continue
		}
		collectGraphV2ReservedRefsInStringWithVisitor(graphV2Path(prefix, "vars."+name+".description"), def.Description, allowConvoyReference, errs, visit)
		if def.Default != nil {
			collectGraphV2ReservedRefsInStringWithVisitor(graphV2Path(prefix, "vars."+name+".default"), *def.Default, allowConvoyReference, errs, visit)
		}
		collectGraphV2ReservedRefsInStringWithVisitor(graphV2Path(prefix, "vars."+name+".pattern"), def.Pattern, allowConvoyReference, errs, visit)
	}
	collectGraphV2ReservedRefsInStepsWithVisitor(graphV2Path(prefix, "steps"), f.Steps, allowConvoyReference, errs, visit)
	collectGraphV2ReservedRefsInStepsWithVisitor(graphV2Path(prefix, "template"), f.Template, allowConvoyReference, errs, visit)
	collectGraphV2ReservedRefsInCompose(graphV2Path(prefix, "compose"), f.Compose, allowConvoyReference, errs, visit)
	collectGraphV2ReservedRefsInAdvice(graphV2Path(prefix, "advice"), f.Advice, allowConvoyReference, errs, visit)
	for i, pointcut := range f.Pointcuts {
		if pointcut == nil {
			continue
		}
		pointcutPrefix := fmt.Sprintf("%s[%d]", graphV2Path(prefix, "pointcuts"), i)
		collectGraphV2ReservedRefsInStringWithVisitor(pointcutPrefix+".glob", pointcut.Glob, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(pointcutPrefix+".type", pointcut.Type, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(pointcutPrefix+".label", pointcut.Label, allowConvoyReference, errs, visit)
	}
}

func graphV2Path(prefix, leaf string) string {
	if prefix == "" {
		return leaf
	}
	if leaf == "" {
		return prefix
	}
	return prefix + "." + leaf
}

func graphV2RecipeReservedSymbolErrors(recipe *Recipe, allowConvoyReference bool, visit func(path, name string)) []string {
	if recipe == nil || !recipeDeclaresGraphV2Contract(recipe) {
		return nil
	}
	var errs []string
	collectGraphV2ReservedRefsInStringWithVisitor("description", recipe.Description, allowConvoyReference, &errs, visit)
	for name, def := range recipe.Vars {
		if isGraphV2ReservedVarName(name) {
			if visit != nil {
				visit("vars."+name, strings.TrimSpace(name))
			}
			if strings.TrimSpace(name) != "issue" {
				errs = append(errs, fmt.Sprintf("vars.%s: formulas v2 reserved variable cannot be declared", name))
			}
		}
		if def == nil {
			continue
		}
		collectGraphV2ReservedRefsInStringWithVisitor("vars."+name+".description", def.Description, allowConvoyReference, &errs, visit)
		if def.Default != nil {
			collectGraphV2ReservedRefsInStringWithVisitor("vars."+name+".default", *def.Default, allowConvoyReference, &errs, visit)
		}
		collectGraphV2ReservedRefsInStringWithVisitor("vars."+name+".pattern", def.Pattern, allowConvoyReference, &errs, visit)
	}
	for i, step := range recipe.Steps {
		stepPrefix := fmt.Sprintf("recipe.steps[%d] (%s)", i, step.ID)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".title", step.Title, allowConvoyReference, &errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".description", step.Description, allowConvoyReference, &errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".notes", step.Notes, allowConvoyReference, &errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".assignee", step.Assignee, allowConvoyReference, &errs, visit)
		for j, label := range step.Labels {
			collectGraphV2ReservedRefsInStringWithVisitor(fmt.Sprintf("%s.labels[%d]", stepPrefix, j), label, allowConvoyReference, &errs, visit)
		}
		for key, value := range step.Metadata {
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".metadata."+key, value, allowConvoyReference, &errs, visit)
		}
		if step.Gate != nil {
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".gate.type", step.Gate.Type, allowConvoyReference, &errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".gate.id", step.Gate.ID, allowConvoyReference, &errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".gate.timeout", step.Gate.Timeout, allowConvoyReference, &errs, visit)
		}
		if strings.TrimSpace(step.Metadata[beadmeta.KindMetadataKey]) == "drain" {
			validateGraphV2RecipeDrainStep(stepPrefix, step, &errs)
		}
	}
	return errs
}

func recipeDeclaresGraphV2Contract(recipe *Recipe) bool {
	root := recipe.RootStep()
	return root != nil && strings.EqualFold(strings.TrimSpace(root.Metadata[beadmeta.FormulaContractMetadataKey]), "graph.v2")
}

func validateGraphV2RecipeDrainStep(prefix string, step RecipeStep, errs *[]string) {
	context := strings.TrimSpace(step.Metadata[beadmeta.DrainContextMetadataKey])
	switch context {
	case "", "separate":
	case "shared":
	default:
		*errs = append(*errs, fmt.Sprintf("%s.drain: context must be separate or shared", prefix))
	}
	formulaName := strings.TrimSpace(step.Metadata[beadmeta.DrainFormulaMetadataKey])
	if formulaName == "" {
		*errs = append(*errs, fmt.Sprintf("%s.drain: formula is required", prefix))
	}
	if strings.Contains(formulaName, "{{") {
		*errs = append(*errs, fmt.Sprintf("%s.drain: templated item formula names are not supported in v0", prefix))
	}
	memberAccess := strings.TrimSpace(step.Metadata[beadmeta.DrainMemberAccessMetadataKey])
	switch memberAccess {
	case "", "read":
	case "exclusive":
	default:
		*errs = append(*errs, fmt.Sprintf("%s.drain: member_access must be read or exclusive", prefix))
	}
	if raw := strings.TrimSpace(step.Metadata[beadmeta.DrainMaxUnitsMetadataKey]); raw != "" {
		maxUnits, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			*errs = append(*errs, fmt.Sprintf("%s.drain: max_units must be an integer", prefix))
		case maxUnits < 1:
			*errs = append(*errs, fmt.Sprintf("%s.drain: max_units must be >= 1", prefix))
		case maxUnits > 100:
			*errs = append(*errs, fmt.Sprintf("%s.drain: max_units must be <= 100 in v0", prefix))
		}
	}
	switch strings.TrimSpace(step.Metadata[beadmeta.DrainOnItemFailureMetadataKey]) {
	case "", "skip_remaining", "continue":
	default:
		*errs = append(*errs, fmt.Sprintf("%s.drain: on_item_failure must be skip_remaining or continue", prefix))
	}
	if strings.TrimSpace(step.Metadata[beadmeta.DrainContinuationGroupMetadataKey]) != "" && context != "shared" {
		*errs = append(*errs, fmt.Sprintf("%s.drain: continuation_group is valid only with context = \"shared\"", prefix))
	}
	if context == "shared" && strings.TrimSpace(step.Metadata[beadmeta.DrainItemSingleLaneMetadataKey]) != "true" {
		*errs = append(*errs, fmt.Sprintf("%s.drain.item: shared drains require single_lane = true", prefix))
	}
	if strings.TrimSpace(step.Assignee) != "" {
		*errs = append(*errs, fmt.Sprintf("%s: drain cannot be combined with assignee", prefix))
	}
	if step.Gate != nil {
		*errs = append(*errs, fmt.Sprintf("%s: drain cannot be combined with gate", prefix))
	}
}

func collectGraphV2ReservedRefsInStepsWithVisitor(prefix string, steps []*Step, allowConvoyReference bool, errs *[]string, visit func(path, name string)) {
	for i, step := range steps {
		if step == nil {
			continue
		}
		stepPrefix := fmt.Sprintf("%s[%d]", prefix, i)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".title", step.Title, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".description", step.Description, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".notes", step.Notes, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".type", step.Type, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".assignee", step.Assignee, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".expand", step.Expand, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".condition", step.Condition, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".waits_for", step.WaitsFor, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".timeout", step.Timeout, allowConvoyReference, errs, visit)
		for j, label := range step.Labels {
			collectGraphV2ReservedRefsInStringWithVisitor(fmt.Sprintf("%s.labels[%d]", stepPrefix, j), label, allowConvoyReference, errs, visit)
		}
		for j, dep := range step.DependsOn {
			collectGraphV2ReservedRefsInStringWithVisitor(fmt.Sprintf("%s.depends_on[%d]", stepPrefix, j), dep, allowConvoyReference, errs, visit)
		}
		for j, need := range step.Needs {
			collectGraphV2ReservedRefsInStringWithVisitor(fmt.Sprintf("%s.needs[%d]", stepPrefix, j), need, allowConvoyReference, errs, visit)
		}
		for k, v := range step.Metadata {
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".metadata."+k, v, allowConvoyReference, errs, visit)
		}
		for k, v := range step.ExpandVars {
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".expand_vars."+k, v, allowConvoyReference, errs, visit)
		}
		if step.Drain != nil {
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".drain.context", step.Drain.Context, allowConvoyReference, errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".drain.formula", step.Drain.Formula, allowConvoyReference, errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".drain.continuation_group", step.Drain.ContinuationGroup, allowConvoyReference, errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".drain.member_access", step.Drain.MemberAccess, allowConvoyReference, errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".drain.on_item_failure", step.Drain.OnItemFailure, allowConvoyReference, errs, visit)
		}
		collectGraphV2ReservedRefsInStepsWithVisitor(stepPrefix+".children", step.Children, allowConvoyReference, errs, visit)
		if step.Loop != nil {
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".loop.until", step.Loop.Until, allowConvoyReference, errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".loop.range", step.Loop.Range, allowConvoyReference, errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".loop.var", step.Loop.Var, allowConvoyReference, errs, visit)
			collectGraphV2ReservedRefsInStepsWithVisitor(stepPrefix+".loop.body", step.Loop.Body, allowConvoyReference, errs, visit)
		}
		if step.Gate != nil {
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".gate.type", step.Gate.Type, allowConvoyReference, errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".gate.id", step.Gate.ID, allowConvoyReference, errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".gate.timeout", step.Gate.Timeout, allowConvoyReference, errs, visit)
		}
		if step.OnComplete != nil {
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".on_complete.for_each", step.OnComplete.ForEach, allowConvoyReference, errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".on_complete.bond", step.OnComplete.Bond, allowConvoyReference, errs, visit)
			for k, v := range step.OnComplete.Vars {
				collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".on_complete.vars."+k, v, allowConvoyReference, errs, visit)
			}
		}
		if step.Ralph != nil && step.Ralph.Check != nil {
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".check.mode", step.Ralph.Check.Mode, allowConvoyReference, errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".check.path", step.Ralph.Check.Path, allowConvoyReference, errs, visit)
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".check.timeout", step.Ralph.Check.Timeout, allowConvoyReference, errs, visit)
		}
		if step.Retry != nil {
			collectGraphV2ReservedRefsInStringWithVisitor(stepPrefix+".retry.on_exhausted", step.Retry.OnExhausted, allowConvoyReference, errs, visit)
		}
	}
}

func collectGraphV2ReservedRefsInCompose(prefix string, compose *ComposeRules, allowConvoyReference bool, errs *[]string, visit func(path, name string)) {
	if compose == nil {
		return
	}
	for i, bp := range compose.BondPoints {
		if bp == nil {
			continue
		}
		bpPrefix := fmt.Sprintf("%s.bond_points[%d]", prefix, i)
		collectGraphV2ReservedRefsInStringWithVisitor(bpPrefix+".id", bp.ID, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(bpPrefix+".description", bp.Description, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(bpPrefix+".after_step", bp.AfterStep, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(bpPrefix+".before_step", bp.BeforeStep, allowConvoyReference, errs, visit)
	}
	for i, hook := range compose.Hooks {
		if hook == nil {
			continue
		}
		hookPrefix := fmt.Sprintf("%s.hooks[%d]", prefix, i)
		collectGraphV2ReservedRefsInStringWithVisitor(hookPrefix+".trigger", hook.Trigger, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(hookPrefix+".attach", hook.Attach, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(hookPrefix+".at", hook.At, allowConvoyReference, errs, visit)
		for k, v := range hook.Vars {
			collectGraphV2ReservedRefsInStringWithVisitor(hookPrefix+".vars."+k, v, allowConvoyReference, errs, visit)
		}
	}
	for i, rule := range compose.Expand {
		if rule == nil {
			continue
		}
		rulePrefix := fmt.Sprintf("%s.expand[%d]", prefix, i)
		collectGraphV2ReservedRefsInStringWithVisitor(rulePrefix+".target", rule.Target, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(rulePrefix+".with", rule.With, allowConvoyReference, errs, visit)
		for k, v := range rule.Vars {
			collectGraphV2ReservedRefsInStringWithVisitor(rulePrefix+".vars."+k, v, allowConvoyReference, errs, visit)
		}
	}
	for i, rule := range compose.Map {
		if rule == nil {
			continue
		}
		rulePrefix := fmt.Sprintf("%s.map[%d]", prefix, i)
		collectGraphV2ReservedRefsInStringWithVisitor(rulePrefix+".select", rule.Select, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(rulePrefix+".with", rule.With, allowConvoyReference, errs, visit)
		for k, v := range rule.Vars {
			collectGraphV2ReservedRefsInStringWithVisitor(rulePrefix+".vars."+k, v, allowConvoyReference, errs, visit)
		}
	}
	for i, rule := range compose.Branch {
		if rule == nil {
			continue
		}
		rulePrefix := fmt.Sprintf("%s.branch[%d]", prefix, i)
		collectGraphV2ReservedRefsInStringWithVisitor(rulePrefix+".from", rule.From, allowConvoyReference, errs, visit)
		for j, step := range rule.Steps {
			collectGraphV2ReservedRefsInStringWithVisitor(fmt.Sprintf("%s.steps[%d]", rulePrefix, j), step, allowConvoyReference, errs, visit)
		}
		collectGraphV2ReservedRefsInStringWithVisitor(rulePrefix+".join", rule.Join, allowConvoyReference, errs, visit)
	}
	for i, rule := range compose.Gate {
		if rule == nil {
			continue
		}
		rulePrefix := fmt.Sprintf("%s.gate[%d]", prefix, i)
		collectGraphV2ReservedRefsInStringWithVisitor(rulePrefix+".before", rule.Before, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInStringWithVisitor(rulePrefix+".condition", rule.Condition, allowConvoyReference, errs, visit)
	}
	for i, aspect := range compose.Aspects {
		collectGraphV2ReservedRefsInStringWithVisitor(fmt.Sprintf("%s.aspects[%d]", prefix, i), aspect, allowConvoyReference, errs, visit)
	}
}

func collectGraphV2ReservedRefsInAdvice(prefix string, advice []*AdviceRule, allowConvoyReference bool, errs *[]string, visit func(path, name string)) {
	for i, rule := range advice {
		if rule == nil {
			continue
		}
		rulePrefix := fmt.Sprintf("%s[%d]", prefix, i)
		collectGraphV2ReservedRefsInStringWithVisitor(rulePrefix+".target", rule.Target, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInAdviceStep(rulePrefix+".before", rule.Before, allowConvoyReference, errs, visit)
		collectGraphV2ReservedRefsInAdviceStep(rulePrefix+".after", rule.After, allowConvoyReference, errs, visit)
		if rule.Around != nil {
			for j, step := range rule.Around.Before {
				collectGraphV2ReservedRefsInAdviceStep(fmt.Sprintf("%s.around.before[%d]", rulePrefix, j), step, allowConvoyReference, errs, visit)
			}
			for j, step := range rule.Around.After {
				collectGraphV2ReservedRefsInAdviceStep(fmt.Sprintf("%s.around.after[%d]", rulePrefix, j), step, allowConvoyReference, errs, visit)
			}
		}
	}
}

func collectGraphV2ReservedRefsInAdviceStep(prefix string, step *AdviceStep, allowConvoyReference bool, errs *[]string, visit func(path, name string)) {
	if step == nil {
		return
	}
	collectGraphV2ReservedRefsInStringWithVisitor(prefix+".id", step.ID, allowConvoyReference, errs, visit)
	collectGraphV2ReservedRefsInStringWithVisitor(prefix+".title", step.Title, allowConvoyReference, errs, visit)
	collectGraphV2ReservedRefsInStringWithVisitor(prefix+".description", step.Description, allowConvoyReference, errs, visit)
	collectGraphV2ReservedRefsInStringWithVisitor(prefix+".type", step.Type, allowConvoyReference, errs, visit)
	for k, v := range step.Args {
		collectGraphV2ReservedRefsInStringWithVisitor(prefix+".args."+k, v, allowConvoyReference, errs, visit)
	}
	for k, v := range step.Output {
		collectGraphV2ReservedRefsInStringWithVisitor(prefix+".output."+k, v, allowConvoyReference, errs, visit)
	}
}

type graphV2TransitiveReferenceScanner struct {
	parser               *Parser
	allowConvoyReference bool
	visited              map[string]bool
	errs                 []string
	requiresInputConvoy  bool
	visit                func(path, name string)
}

func (s *graphV2TransitiveReferenceScanner) scanFormula(prefix string, f *Formula) error {
	if f == nil {
		return nil
	}
	key := graphV2ScanKey(f)
	if s.visited[key] {
		return nil
	}
	s.visited[key] = true

	visit := func(path, name string) {
		if graphV2NameRequiresInputConvoy(name) {
			s.requiresInputConvoy = true
		}
		if s.visit != nil {
			s.visit(path, name)
		}
	}
	collectGraphV2ReservedRefsInFormula(prefix, f, s.allowConvoyReference, &s.errs, visit, true)
	if GraphV2FormulaHasDrain(f) {
		s.requiresInputConvoy = true
	}
	if s.parser == nil {
		return nil
	}
	if err := s.scanExpansionRefs(graphV2Path(prefix, "steps"), f.Steps); err != nil {
		return err
	}
	if err := s.scanExpansionRefs(graphV2Path(prefix, "template"), f.Template); err != nil {
		return err
	}
	if f.Compose != nil {
		for i, rule := range f.Compose.Expand {
			if rule == nil || strings.TrimSpace(rule.With) == "" {
				continue
			}
			if err := s.scanExpansionFormula(fmt.Sprintf("%s.expand[%d]", graphV2Path(prefix, "compose"), i), rule.With); err != nil {
				return err
			}
		}
		for i, rule := range f.Compose.Map {
			if rule == nil || strings.TrimSpace(rule.With) == "" {
				continue
			}
			if err := s.scanExpansionFormula(fmt.Sprintf("%s.map[%d]", graphV2Path(prefix, "compose"), i), rule.With); err != nil {
				return err
			}
		}
		for i, aspectName := range f.Compose.Aspects {
			if strings.TrimSpace(aspectName) == "" {
				continue
			}
			if err := s.scanAspectFormula(fmt.Sprintf("%s.aspects[%d]", graphV2Path(prefix, "compose"), i), aspectName); err != nil {
				return err
			}
		}
	}
	return nil
}

func graphV2ScanKey(f *Formula) string {
	if f.Source != "" {
		return f.Formula + "\x00" + f.Source
	}
	return f.Formula
}

func (s *graphV2TransitiveReferenceScanner) scanExpansionRefs(prefix string, steps []*Step) error {
	for i, step := range steps {
		if step == nil {
			continue
		}
		stepPrefix := fmt.Sprintf("%s[%d]", prefix, i)
		if strings.TrimSpace(step.Expand) != "" {
			if err := s.scanExpansionFormula(stepPrefix+".expand", step.Expand); err != nil {
				return err
			}
		}
		if err := s.scanExpansionRefs(stepPrefix+".children", step.Children); err != nil {
			return err
		}
		if step.Loop != nil {
			if err := s.scanExpansionRefs(stepPrefix+".loop.body", step.Loop.Body); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *graphV2TransitiveReferenceScanner) scanExpansionFormula(prefix, name string) error {
	expansion, err := loadResolvedExpansionFormula(s.parser, name, prefix, nil)
	if err != nil {
		return err
	}
	return s.scanFormula(prefix+" "+strconv.Quote(name), expansion)
}

func (s *graphV2TransitiveReferenceScanner) scanAspectFormula(prefix, name string) error {
	aspectFormula, err := s.parser.LoadByName(name)
	if err != nil {
		return fmt.Errorf("%s: loading aspect %q: %w", prefix, name, err)
	}
	resolved, err := s.parser.Resolve(aspectFormula)
	if err != nil {
		return fmt.Errorf("%s: resolving aspect %q: %w", prefix, name, err)
	}
	if resolved.Type != TypeAspect {
		return fmt.Errorf("%s: %q is not an aspect formula (type=%s)", prefix, name, resolved.Type)
	}
	return s.scanFormula(prefix+" "+strconv.Quote(name), resolved)
}

func collectGraphV2ReservedRefsInStringWithVisitor(path, value string, allowConvoyReference bool, errs *[]string, visit func(path, name string)) {
	for _, pattern := range []*regexp.Regexp{graphV2DirectReservedRefPattern, graphV2IndexReservedRefPattern} {
		for _, match := range pattern.FindAllStringSubmatch(value, -1) {
			if len(match) < 2 {
				continue
			}
			name := match[1]
			if visit != nil {
				visit(path, name)
			}
			if name == "convoy_id" && allowConvoyReference {
				if match[0] != "{{convoy_id}}" {
					*errs = append(*errs, fmt.Sprintf("%s: convoy_id references must use {{convoy_id}} exactly", path))
				}
				continue
			}
			switch name {
			case "convoy_id":
				*errs = append(*errs, fmt.Sprintf("%s: convoy_id requires a targeted formulas v2 invocation", path))
			case "issue":
				// Deprecated one-release compat alias (#2941): {{issue}}
				// resolves to the single tracked member of the input convoy.
				// GraphV2LegacyIssueRefs surfaces these for deprecation
				// warnings.
			case "bead_id":
				*errs = append(*errs, fmt.Sprintf("%s: %s is not available in v2 formulas; use convoy_id", path, name))
			}
		}
	}
}

func isGraphV2ReservedVarName(name string) bool {
	_, ok := graphV2ReservedVarNames[strings.TrimSpace(name)]
	return ok
}

// GraphV2OutputJSONWarnings returns one warning string per step in a graph.v2
// formula that sets gc.output_json_required without using drain. graph.v1
// formulas and steps that use drain are not flagged — drain is the graph.v2
// canonical fan-out primitive.
func GraphV2OutputJSONWarnings(f *Formula) []string {
	if !UsesGraphCompiler(f) {
		return nil
	}
	var warnings []string
	collectOutputJSONWarnings(f.Steps, f.Formula, &warnings)
	return warnings
}

func collectOutputJSONWarnings(steps []*Step, formulaName string, out *[]string) {
	for _, step := range steps {
		if step.Drain == nil && strings.TrimSpace(step.Metadata[beadmeta.OutputJSONRequiredMetadataKey]) == "true" {
			*out = append(*out, fmt.Sprintf(
				"formula %s step %s: gc.output_json is deprecated; use drain in v2 formulas (see: engdocs/drain-fanout.md)",
				formulaName, step.ID,
			))
		}
		collectOutputJSONWarnings(step.Children, formulaName, out)
		if step.Loop != nil {
			collectOutputJSONWarnings(step.Loop.Body, formulaName, out)
		}
	}
}

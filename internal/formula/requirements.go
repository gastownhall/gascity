package formula

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	semver "github.com/Masterminds/semver/v3"
)

const (
	currentFormulaCompilerCapability = "2.0.0"
	defaultFormulaCompilerCapability = "1.0.0"
	graphV2Requirement               = ">=2.0.0"
)

var semverLiteralPattern = regexp.MustCompile(`\d+(?:\.\d+){0,2}`)

type formulaCompilerConstraint struct {
	Raw    string
	Source string
}

// Requirements declares minimum host capabilities needed by a formula.
type Requirements struct {
	FormulaCompiler string `json:"formula_compiler,omitempty" toml:"formula_compiler,omitempty"`
}

// UnmarshalTOML decodes the top-level [requires] table and rejects unknown
// axes so formula authors do not get a false sense of compatibility.
func (r *Requirements) UnmarshalTOML(data interface{}) error {
	raw, ok := data.(map[string]interface{})
	if !ok {
		return fmt.Errorf("formula.requirement_invalid: requires must be a table")
	}
	for key, value := range raw {
		switch key {
		case "formula_compiler":
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("formula.compiler_requirement_invalid: formula_compiler must be a semver comparator, for example %q", graphV2Requirement)
			}
			r.FormulaCompiler = text
		default:
			return unknownRequirementError(key)
		}
	}
	return nil
}

// UnmarshalJSON decodes legacy JSON formulas consistently with TOML formulas.
func (r *Requirements) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("formula.requirement_invalid: requires must be an object: %w", err)
	}
	for key, value := range raw {
		switch key {
		case "formula_compiler":
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				return fmt.Errorf("formula.compiler_requirement_invalid: formula_compiler must be a semver comparator, for example %q", graphV2Requirement)
			}
			r.FormulaCompiler = text
		default:
			return unknownRequirementError(key)
		}
	}
	return nil
}

// ValidateHostRequirements verifies that the active city capability satisfies
// the formula's declared compiler requirements.
func ValidateHostRequirements(f *Formula, formulaV2Enabled bool) error {
	constraints, err := formulaCompilerConstraints(f)
	if err != nil {
		return err
	}
	if err := validateFormulaCompilerConstraintSet(f.Formula, constraints); err != nil {
		return err
	}
	if len(constraints) == 0 {
		return nil
	}
	hostVersion := activeFormulaCompilerCapability(formulaV2Enabled)
	host, err := semver.NewVersion(hostVersion)
	if err != nil {
		return fmt.Errorf("formula.compiler_requirement_invalid: invalid host formula compiler capability %q: %w", hostVersion, err)
	}
	for _, candidate := range constraints {
		constraint, err := semver.NewConstraint(candidate.Raw)
		if err != nil {
			return invalidFormulaCompilerRequirement(candidate.Raw, err)
		}
		if !constraint.Check(host) {
			return unsatisfiedFormulaCompilerRequirement(candidate.Raw, candidate.Source, hostVersion, formulaV2Enabled)
		}
	}
	return nil
}

func validateRequirementDeclarations(f *Formula) []string {
	if f == nil {
		return nil
	}
	var errs []string
	constraints, err := formulaCompilerConstraints(f)
	if err != nil {
		errs = append(errs, err.Error())
		return errs
	}
	if err := validateFormulaCompilerConstraintSet(f.Formula, constraints); err != nil {
		errs = append(errs, err.Error())
	}
	return errs
}

func formulaCompilerConstraints(f *Formula) ([]formulaCompilerConstraint, error) {
	if f == nil {
		return nil, nil
	}
	constraints := f.compilerRequirementSources
	if len(constraints) == 0 {
		constraints = directFormulaCompilerConstraints(f)
	}
	constraints = append([]formulaCompilerConstraint(nil), constraints...)
	for _, candidate := range constraints {
		if _, err := semver.NewConstraint(candidate.Raw); err != nil {
			return nil, invalidFormulaCompilerRequirement(candidate.Raw, err)
		}
	}
	return constraints, nil
}

func directFormulaCompilerConstraints(f *Formula) []formulaCompilerConstraint {
	if f == nil {
		return nil
	}
	var constraints []formulaCompilerConstraint
	if declaresGraphV2Contract(f) {
		constraints = append(constraints, formulaCompilerConstraint{Raw: graphV2Requirement, Source: formulaCompilerConstraintSource(f, `contract = "graph.v2"`)})
	}
	if raw := formulaCompilerRequirement(f); raw != "" {
		constraints = append(constraints, formulaCompilerConstraint{Raw: raw, Source: formulaCompilerConstraintSource(f, "[requires]")})
	}
	return constraints
}

func formulaCompilerConstraintSource(f *Formula, suffix string) string {
	name := "<unknown>"
	if f != nil && strings.TrimSpace(f.Formula) != "" {
		name = strings.TrimSpace(f.Formula)
	}
	return fmt.Sprintf("formula %q %s", name, suffix)
}

func setFormulaCompilerConstraints(f *Formula, constraints []formulaCompilerConstraint) {
	if f == nil || len(constraints) == 0 {
		return
	}
	f.compilerRequirementSources = append([]formulaCompilerConstraint(nil), constraints...)
	parts := make([]string, 0, len(constraints))
	for _, constraint := range constraints {
		parts = append(parts, constraint.Raw)
	}
	f.Requires = &Requirements{FormulaCompiler: strings.Join(parts, ", ")}
}

func addFormulaCompilerConstraints(f *Formula, extra []formulaCompilerConstraint) error {
	if f == nil || len(extra) == 0 {
		return nil
	}
	constraints, err := formulaCompilerConstraints(f)
	if err != nil {
		return err
	}
	constraints = append(constraints, extra...)
	if err := validateFormulaCompilerConstraintSet(f.Formula, constraints); err != nil {
		return err
	}
	setFormulaCompilerConstraints(f, constraints)
	return nil
}

func validateFormulaCompilerConstraintSet(formulaName string, constraints []formulaCompilerConstraint) error {
	if len(constraints) < 2 {
		return nil
	}
	parsed := make([]*semver.Constraints, 0, len(constraints))
	for _, constraint := range constraints {
		c, err := semver.NewConstraint(constraint.Raw)
		if err != nil {
			return invalidFormulaCompilerRequirement(constraint.Raw, err)
		}
		parsed = append(parsed, c)
	}
	if compilerConstraintsHaveIntersection(parsed, constraints) {
		return nil
	}
	return formulaCompilerRequirementConflict(formulaName, constraints)
}

func compilerConstraintsHaveIntersection(parsed []*semver.Constraints, constraints []formulaCompilerConstraint) bool {
	for _, candidate := range compilerRequirementCandidateVersions(constraints) {
		matches := true
		for _, constraint := range parsed {
			if !constraint.Check(candidate) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func compilerRequirementCandidateVersions(constraints []formulaCompilerConstraint) []*semver.Version {
	seen := make(map[string]bool)
	var out []*semver.Version
	add := func(major, minor, patch int) {
		if major < 0 || minor < 0 || patch < 0 {
			return
		}
		raw := fmt.Sprintf("%d.%d.%d", major, minor, patch)
		if seen[raw] {
			return
		}
		v, err := semver.NewVersion(raw)
		if err != nil {
			return
		}
		seen[raw] = true
		out = append(out, v)
	}
	addLiteral := func(raw string) {
		parts := strings.Split(raw, ".")
		nums := [3]int{}
		for i := range nums {
			if i >= len(parts) {
				break
			}
			n, err := strconv.Atoi(parts[i])
			if err != nil {
				return
			}
			nums[i] = n
		}
		major, minor, patch := nums[0], nums[1], nums[2]
		add(major, minor, patch)
		add(major, minor, patch+1)
		if patch > 0 {
			add(major, minor, patch-1)
		}
		add(major, minor+1, 0)
		if minor > 0 {
			add(major, minor-1, 999)
		}
		add(major+1, 0, 0)
		if major > 0 {
			add(major-1, 999, 999)
		}
	}

	add(0, 0, 0)
	add(1, 0, 0)
	add(2, 0, 0)
	add(3, 0, 0)
	add(999, 999, 999)
	for _, constraint := range constraints {
		for _, raw := range semverLiteralPattern.FindAllString(constraint.Raw, -1) {
			addLiteral(raw)
		}
	}
	return out
}

func formulaCompilerRequirementConflict(formulaName string, constraints []formulaCompilerConstraint) error {
	name := strings.TrimSpace(formulaName)
	if name == "" {
		name = "<unknown>"
	}
	parts := make([]string, 0, len(constraints))
	for _, constraint := range constraints {
		source := constraint.Source
		if source != "" {
			source = " from " + source
		}
		parts = append(parts, fmt.Sprintf("%s%s", constraint.Raw, source))
	}
	return fmt.Errorf("formula.compiler_requirement_conflict: formula %q has non-overlapping formula_compiler requirements: %s", name, strings.Join(parts, "; "))
}

// UsesGraphCompiler reports whether f declares compiler-v2 graph workflow
// semantics through [requires] formula_compiler or legacy contract = "graph.v2".
func UsesGraphCompiler(f *Formula) bool {
	return declaresGraphCompilerRequirement(f)
}

func declaresGraphCompilerRequirement(f *Formula) bool {
	defaultCapability, err := semver.NewVersion(defaultFormulaCompilerCapability)
	if err != nil {
		return false
	}
	currentCapability, err := semver.NewVersion(currentFormulaCompilerCapability)
	if err != nil {
		return false
	}
	constraints, err := formulaCompilerConstraints(f)
	if err != nil {
		return false
	}
	for _, candidate := range constraints {
		constraint, err := semver.NewConstraint(candidate.Raw)
		if err != nil {
			return false
		}
		if !constraint.Check(defaultCapability) && constraint.Check(currentCapability) {
			return true
		}
	}
	return false
}

func formulaCompilerRequirement(f *Formula) string {
	if f == nil || f.Requires == nil {
		return ""
	}
	return strings.TrimSpace(f.Requires.FormulaCompiler)
}

func activeFormulaCompilerCapability(formulaV2Enabled bool) string {
	if !formulaV2Enabled {
		return defaultFormulaCompilerCapability
	}
	return currentFormulaCompilerCapability
}

func cloneRequirements(req *Requirements) *Requirements {
	if req == nil {
		return nil
	}
	return &Requirements{FormulaCompiler: req.FormulaCompiler}
}

func invalidFormulaCompilerRequirement(raw string, err error) error {
	return fmt.Errorf("formula.compiler_requirement_invalid: formula_compiler must be a semver comparator, for example %q (got %q: %w)", graphV2Requirement, raw, err)
}

func unsatisfiedFormulaCompilerRequirement(raw, source, hostVersion string, formulaV2Enabled bool) error {
	reason := ""
	if !formulaV2Enabled {
		reason = " because [daemon] formula_v2 is disabled"
	}
	if source != "" {
		source = " from " + source
	}
	return fmt.Errorf("formula.compiler_requirement_unsatisfied: formula requires formula_compiler %s%s, but this city has formula compiler capability %s%s", raw, source, hostVersion, reason)
}

func unknownRequirementError(key string) error {
	return fmt.Errorf("formula.requirement_unknown: unknown formula requirement %q; supported requirements: formula_compiler", key)
}

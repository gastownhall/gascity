package formula

import (
	"encoding/json"
	"fmt"
	"strings"

	semver "github.com/Masterminds/semver/v3"
)

const (
	currentFormulaCompilerCapability = "2.0.0"
	defaultFormulaCompilerCapability = "1.0.0"
	graphV2Requirement               = ">=2.0.0"
)

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
	if raw := formulaCompilerRequirement(f); raw != "" {
		constraint, err := semver.NewConstraint(raw)
		if err != nil {
			errs = append(errs, invalidFormulaCompilerRequirement(raw, err).Error())
		} else if declaresGraphV2Contract(f) {
			graphV2, graphErr := semver.NewVersion(currentFormulaCompilerCapability)
			if graphErr != nil || !constraint.Check(graphV2) {
				errs = append(errs, fmt.Sprintf("formula.compiler_requirement_conflict: contract = %q requires formula_compiler %s but [requires] formula_compiler = %q does not include %s", f.Contract, graphV2Requirement, raw, currentFormulaCompilerCapability))
			}
		}
	}
	return errs
}

func formulaCompilerConstraints(f *Formula) ([]formulaCompilerConstraint, error) {
	if f == nil {
		return nil, nil
	}
	var constraints []formulaCompilerConstraint
	if declaresGraphV2Contract(f) {
		constraints = append(constraints, formulaCompilerConstraint{Raw: graphV2Requirement, Source: `contract = "graph.v2"`})
	}
	if raw := formulaCompilerRequirement(f); raw != "" {
		constraints = append(constraints, formulaCompilerConstraint{Raw: raw, Source: "requires.formula_compiler"})
	}
	for _, candidate := range constraints {
		if _, err := semver.NewConstraint(candidate.Raw); err != nil {
			return nil, invalidFormulaCompilerRequirement(candidate.Raw, err)
		}
	}
	return constraints, nil
}

func declaresGraphCompilerRequirement(f *Formula) bool {
	if declaresGraphV2Contract(f) {
		return true
	}
	raw := formulaCompilerRequirement(f)
	if raw == "" {
		return false
	}
	constraint, err := semver.NewConstraint(raw)
	if err != nil {
		return false
	}
	defaultCapability, err := semver.NewVersion(defaultFormulaCompilerCapability)
	if err != nil {
		return false
	}
	currentCapability, err := semver.NewVersion(currentFormulaCompilerCapability)
	if err != nil {
		return false
	}
	return !constraint.Check(defaultCapability) && constraint.Check(currentCapability)
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

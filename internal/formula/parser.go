package formula

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Formula file extensions. TOML is preferred, JSON is legacy fallback.
const (
	FormulaExtTOML = CanonicalTOMLExt
	// PACKV2-CUTOVER: remove legacy formula filename support after the infix migration window closes.
	FormulaLegacyExtTOML = LegacyTOMLExt
	FormulaExtJSON       = ".formula.json"
	FormulaExt           = FormulaExtJSON // Legacy alias for backwards compatibility
)

// Parser handles loading and resolving formulas.
//
// NOTE: Parser is NOT thread-safe. Create a new Parser per goroutine or
// synchronize access externally. The cache and resolving maps have no
// internal synchronization.
type Parser struct {
	// searchPaths are directories to search for formulas (in order).
	searchPaths []string

	// cache stores loaded formulas by name.
	cache map[string]*Formula

	// resolvingSet tracks formulas currently being resolved (for cycle detection).
	resolvingSet map[string]bool

	// resolvingChain tracks the order of formulas being resolved (for error messages).
	resolvingChain []string
}

// NewParser creates a new formula parser.
// searchPaths are directories to search for formulas when resolving extends.
// Default paths are: .beads/formulas, ~/.beads/formulas, $GT_ROOT/.beads/formulas
func NewParser(searchPaths ...string) *Parser {
	paths := searchPaths
	if len(paths) == 0 {
		paths = defaultSearchPaths()
	}
	return &Parser{
		searchPaths:    paths,
		cache:          make(map[string]*Formula),
		resolvingSet:   make(map[string]bool),
		resolvingChain: nil,
	}
}

// defaultSearchPaths returns the default formula search paths.
func defaultSearchPaths() []string {
	var paths []string

	// Project-level formulas
	if cwd, err := os.Getwd(); err == nil {
		paths = append(paths, filepath.Join(cwd, ".beads", "formulas"))
	}

	// User-level formulas
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".beads", "formulas"))
	}

	// Orchestrator formulas (via GT_ROOT)
	if gtRoot := os.Getenv("GT_ROOT"); gtRoot != "" {
		paths = append(paths, filepath.Join(gtRoot, ".beads", "formulas"))
	}

	return paths
}

// ParseFile parses a formula from a file path.
// Detects format from extension: .toml, .formula.toml, or .formula.json.
func (p *Parser) ParseFile(path string) (*Formula, error) {
	// Check cache first
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}

	if cached, ok := p.cache[absPath]; ok {
		return cached, nil
	}

	// Read and parse the file
	// #nosec G304 -- absPath comes from controlled search paths or explicit user input
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// Detect format from extension
	var formula *Formula
	if IsTOMLFilename(path) {
		formula, err = p.ParseTOML(data)
	} else {
		formula, err = p.Parse(data)
	}
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	formula.Source = absPath

	// Set source tracing info on all steps (gt-8tmz.18)
	SetSourceInfo(formula)

	// Resolve description_file references relative to the formula file's directory.
	formulaDir := filepath.Dir(absPath)
	resolveDescriptionFiles(formula.Steps, formulaDir)
	resolveDescriptionFiles(formula.Template, formulaDir)

	p.cache[absPath] = formula

	// Also cache by name for extends resolution
	p.cache[formula.Formula] = formula

	return formula, nil
}

// Parse parses a formula from JSON bytes.
func (p *Parser) Parse(data []byte) (*Formula, error) {
	var formula Formula
	if err := json.Unmarshal(data, &formula); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}

	// Set defaults
	if formula.Version == 0 {
		formula.Version = 1
	}
	if formula.Type == "" {
		formula.Type = TypeWorkflow
	}

	return &formula, nil
}

// ParseTOML parses a formula from TOML bytes.
func (p *Parser) ParseTOML(data []byte) (*Formula, error) {
	var formula Formula
	if err := toml.Unmarshal(data, &formula); err != nil {
		return nil, fmt.Errorf("toml: %w", err)
	}

	// Set defaults
	if formula.Version == 0 {
		formula.Version = 1
	}
	if formula.Type == "" {
		formula.Type = TypeWorkflow
	}

	return &formula, nil
}

// Resolve fully resolves a formula, processing extends and expansions.
// Returns a new formula with all inheritance applied.
func (p *Parser) Resolve(formula *Formula) (*Formula, error) {
	// Check for cycles
	if p.resolvingSet[formula.Formula] {
		// Build the cycle chain for a clear error message
		chain := append(slices.Clone(p.resolvingChain), formula.Formula)
		return nil, fmt.Errorf("circular extends detected: %s", strings.Join(chain, " -> "))
	}
	p.resolvingSet[formula.Formula] = true
	p.resolvingChain = append(p.resolvingChain, formula.Formula)
	defer func() {
		delete(p.resolvingSet, formula.Formula)
		p.resolvingChain = p.resolvingChain[:len(p.resolvingChain)-1]
	}()

	// If no extends, just validate and return
	if len(formula.Extends) == 0 {
		if err := formula.Validate(); err != nil {
			return nil, err
		}
		return formula, nil
	}

	// Build merged formula from parents
	merged := &Formula{
		Formula:     formula.Formula,
		Description: formula.Description,
		Version:     formula.Version,
		Contract:    formula.Contract,
		Type:        formula.Type,
		Source:      formula.Source,
		Phase:       formula.Phase,
		Pour:        formula.Pour,
		Vars:        make(map[string]*VarDef),
		Steps:       nil,
		Template:    nil,
		Compose:     nil,
	}

	// Apply each parent in order
	for _, parentName := range formula.Extends {
		parent, err := p.loadFormula(parentName)
		if err != nil {
			return nil, fmt.Errorf("extends %s: %w", parentName, err)
		}

		// Resolve parent recursively
		parent, err = p.Resolve(parent)
		if err != nil {
			return nil, fmt.Errorf("resolve parent %s: %w", parentName, err)
		}

		if merged.Contract == "" {
			merged.Contract = parent.Contract
		}

		// Phase cascades from the first parent that declares one; child
		// declaration wins because merged was seeded from the child.
		if merged.Phase == "" {
			merged.Phase = parent.Phase
		}

		// Pour is an opt-in escalation: any parent or the child requesting
		// pour promotes the merged formula. With a plain bool the zero value
		// is indistinguishable from "unset", so OR is the simplest coherent
		// rule that preserves monotonic opt-in; a *bool field would allow
		// explicit child opt-out but isn't worth the complexity for this flag.
		if !merged.Pour {
			merged.Pour = parent.Pour
		}

		// Merge parent vars (parent vars are inherited, child overrides)
		for name, varDef := range parent.Vars {
			if _, exists := merged.Vars[name]; !exists {
				merged.Vars[name] = varDef
			}
		}

		// Merge parent steps (append, child steps come after)
		merged.Steps = append(merged.Steps, parent.Steps...)

		// Parent templates append in declaration order. Only the child gets
		// override semantics so parent-parent conflicts still surface later.
		merged.Template = append(merged.Template, parent.Template...)

		// Merge parent compose rules
		merged.Compose = mergeComposeRules(merged.Compose, parent.Compose)
	}

	// Apply child overrides
	for name, varDef := range formula.Vars {
		merged.Vars[name] = varDef
	}

	// Merge child steps: override parent steps by ID (preserving position),
	// append new child steps at the end.
	merged.Steps = mergeSteps(merged.Steps, formula.Steps)
	merged.Template = mergeSteps(merged.Template, formula.Template)

	merged.Compose = mergeComposeRules(merged.Compose, formula.Compose)

	// Use child description if set
	if formula.Description != "" {
		merged.Description = formula.Description
	}

	if err := merged.Validate(); err != nil {
		return nil, err
	}

	return merged, nil
}

// loadFormula loads a formula by name from search paths.
// Tries canonical TOML first (.toml), then legacy infixed TOML, then JSON.
func (p *Parser) loadFormula(name string) (*Formula, error) {
	// Check cache first
	if cached, ok := p.cache[name]; ok {
		return cached, nil
	}

	// Search for the formula file - try TOML first, then JSON
	extensions := []string{FormulaExtTOML, FormulaLegacyExtTOML, FormulaExtJSON}
	for _, dir := range p.searchPaths {
		for _, ext := range extensions {
			path := filepath.Join(dir, name+ext)
			if _, err := os.Stat(path); err == nil {
				return p.ParseFile(path)
			}
		}
	}

	return nil, fmt.Errorf("formula %q not found in search paths", name)
}

// LoadByName loads a formula by name from search paths.
// This is the public API for loading formulas used by expansion operators.
func (p *Parser) LoadByName(name string) (*Formula, error) {
	return p.loadFormula(name)
}

// mergeSteps merges child steps into parent steps.
// Child steps with the same ID as a parent step replace the parent step
// in-place (preserving position). Child steps with new IDs are appended.
func mergeSteps(parent, child []*Step) []*Step {
	// Index parent steps by ID for quick lookup
	parentIdx := make(map[string]int, len(parent))
	for i, s := range parent {
		parentIdx[s.ID] = i
	}

	// Copy parent steps (will be modified in-place for overrides)
	result := make([]*Step, len(parent))
	copy(result, parent)

	// Apply child steps
	for _, cs := range child {
		if idx, exists := parentIdx[cs.ID]; exists {
			// Override: replace parent step at same position
			result[idx] = cs
		} else {
			// New step: append at end
			result = append(result, cs)
		}
	}

	return result
}

// mergeComposeRules merges two compose rule sets.
func mergeComposeRules(base, overlay *ComposeRules) *ComposeRules {
	if overlay == nil {
		return base
	}
	if base == nil {
		return overlay
	}

	result := &ComposeRules{
		BondPoints: append([]*BondPoint{}, base.BondPoints...),
		Hooks:      append([]*Hook{}, base.Hooks...),
		Expand:     append([]*ExpandRule{}, base.Expand...),
		Map:        append([]*MapRule{}, base.Map...),
	}

	// Add overlay bond points (override by ID)
	existingBP := make(map[string]int)
	for i, bp := range result.BondPoints {
		existingBP[bp.ID] = i
	}
	for _, bp := range overlay.BondPoints {
		if idx, exists := existingBP[bp.ID]; exists {
			result.BondPoints[idx] = bp
		} else {
			result.BondPoints = append(result.BondPoints, bp)
		}
	}

	// Add overlay hooks (append, no override)
	result.Hooks = append(result.Hooks, overlay.Hooks...)

	// Add overlay expand rules (append, no override)
	result.Expand = append(result.Expand, overlay.Expand...)

	// Add overlay map rules (append, no override)
	result.Map = append(result.Map, overlay.Map...)

	return result
}

// varPattern matches {{variable}} placeholders.
var varPattern = regexp.MustCompile(`\{\{([a-zA-Z_][a-zA-Z0-9_]*)\}\}`)

// ExtractVariables finds all {{variable}} references in a formula.
func ExtractVariables(formula *Formula) []string {
	seen := make(map[string]bool)
	var vars []string

	// Helper to extract vars from a string
	extract := func(s string) {
		matches := varPattern.FindAllStringSubmatch(s, -1)
		for _, match := range matches {
			if len(match) >= 2 && !seen[match[1]] {
				seen[match[1]] = true
				vars = append(vars, match[1])
			}
		}
	}

	// Extract from formula fields
	extract(formula.Description)

	// Extract from steps
	var extractFromStep func(*Step)
	extractFromStep = func(step *Step) {
		extract(step.Title)
		extract(step.Description)
		extract(step.Assignee)
		extract(step.Condition)
		for _, l := range step.Labels {
			extract(l)
		}
		for _, child := range step.Children {
			extractFromStep(child)
		}
	}

	for _, step := range formula.Steps {
		extractFromStep(step)
	}

	return vars
}

// Substitute replaces {{variable}} placeholders with values.
func Substitute(s string, vars map[string]string) string {
	return varPattern.ReplaceAllStringFunc(s, func(match string) string {
		// Extract variable name from {{name}}
		name := match[2 : len(match)-2]
		if val, ok := vars[name]; ok {
			return val
		}
		return match // Keep unresolved placeholders
	})
}

// CheckResidualVars returns the names of any {{...}} placeholders remaining
// in s after substitution. A non-empty return indicates a var name typo or
// a missing or misspelled --var flag.
func CheckResidualVars(s string) []string {
	matches := varPattern.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		names = append(names, m[1])
	}
	return names
}

// CheckResidualTimeoutVars returns unresolved {{var}} and {var} placeholders
// in timeout strings after all available substitutions have been applied.
func CheckResidualTimeoutVars(s string) []string {
	seen := make(map[string]bool)
	var names []string
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}

	for _, name := range CheckResidualVars(s) {
		add(name)
	}
	for _, match := range rangeVarPattern.FindAllStringSubmatchIndex(s, -1) {
		start, end := match[0], match[1]
		if start > 0 && s[start-1] == '{' {
			continue
		}
		if end < len(s) && s[end] == '}' {
			continue
		}
		add(s[match[2]:match[3]])
	}
	return names
}

// ValidateVars checks that all required variables are provided
// and all values pass their constraints.
func ValidateVars(formula *Formula, values map[string]string) error {
	errs, _ := CollectVarValidationErrors(formula.Vars, values)
	return formatVarValidationErrors(errs)
}

// ValidateVarDefs validates explicit var definitions against provided values.
// This is the recipe-level equivalent of ValidateVars, used after formula
// compilation when only the remaining VarDef map is available.
func ValidateVarDefs(defs map[string]*VarDef, values map[string]string) error {
	errs, _ := CollectVarValidationErrors(defs, values)
	return formatVarValidationErrors(errs)
}

// ValidateProvidedVarDefs validates constraints for values the caller supplied
// without requiring every required variable to be present.
func ValidateProvidedVarDefs(defs map[string]*VarDef, values map[string]string) error {
	providedDefs := make(map[string]*VarDef)
	for name := range values {
		if def, ok := defs[name]; ok {
			providedDefs[name] = def
		}
	}
	errs, _ := collectVarValidationErrors(providedDefs, values, false)
	return formatVarValidationErrors(errs)
}

// CollectVarValidationErrors validates explicit var definitions against the
// provided values and returns raw error strings plus the set of missing
// required vars. Callers that need the historical wrapped error can pass the
// returned error strings through formatVarValidationErrors.
func CollectVarValidationErrors(defs map[string]*VarDef, values map[string]string) ([]string, map[string]bool) {
	return collectVarValidationErrors(defs, values, true)
}

func collectVarValidationErrors(defs map[string]*VarDef, values map[string]string, requireMissing bool) ([]string, map[string]bool) {
	var errs []string
	missingRequired := make(map[string]bool)
	names := make([]string, 0, len(defs))
	for name := range defs {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		def := defs[name]
		if def == nil {
			continue
		}
		val, provided := values[name]

		// Check required
		if requireMissing && def.Required && !provided {
			errs = append(errs, fmt.Sprintf("variable %q is required", name))
			missingRequired[name] = true
			continue
		}

		// Use default if not provided
		if !provided && def.Default != nil {
			val = *def.Default
		}

		// Skip further validation if no value
		if val == "" {
			continue
		}

		// Check enum constraint
		if len(def.Enum) > 0 {
			found := false
			for _, allowed := range def.Enum {
				if val == allowed {
					found = true
					break
				}
			}
			if !found {
				errs = append(errs, fmt.Sprintf("variable %q: value %q not in allowed values %v", name, val, def.Enum))
			}
		}

		// Check pattern constraint
		if def.Pattern != "" {
			re, err := regexp.Compile(def.Pattern)
			if err != nil {
				errs = append(errs, fmt.Sprintf("variable %q: invalid pattern %q: %v", name, def.Pattern, err))
			} else if !re.MatchString(val) {
				errs = append(errs, fmt.Sprintf("variable %q: value %q does not match pattern %q", name, val, def.Pattern))
			}
		}
	}

	if len(missingRequired) == 0 {
		missingRequired = nil
	}
	return errs, missingRequired
}

func formatVarValidationErrors(errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("variable validation failed:\n  - %s", strings.Join(errs, "\n  - "))
}

// ApplyDefaults returns a new map with default values filled in.
func ApplyDefaults(formula *Formula, values map[string]string) map[string]string {
	result := make(map[string]string)

	// Copy provided values
	for k, v := range values {
		result[k] = v
	}

	// Apply defaults for missing values
	for name, def := range formula.Vars {
		if _, exists := result[name]; !exists && def.Default != nil {
			result[name] = *def.Default
		}
	}

	return result
}

// SetSourceInfo sets SourceFormula and SourceLocation on all steps in a formula.
// Called after parsing to enable source tracing during cooking (gt-8tmz.18).
// resolveDescriptionFiles walks all steps and replaces DescriptionFile
// with the file's contents. Paths are resolved relative to baseDir
// (the formula file's directory).
func resolveDescriptionFiles(steps []*Step, baseDir string) {
	for _, step := range steps {
		if step == nil || step.DescriptionFile == "" {
			continue
		}
		path := step.DescriptionFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(baseDir, path)
		}
		// #nosec G304 -- path comes from formula author, same trust as description
		data, err := os.ReadFile(path)
		if err == nil {
			step.Description = string(data)
		}
		step.DescriptionFile = "" // consumed; don't serialize
		if len(step.Children) > 0 {
			resolveDescriptionFiles(step.Children, baseDir)
		}
	}
	// Also handle template steps (expansion formulas).
}

// SetSourceInfo populates the SourceFormula and SourcePath fields on each
// step in the formula, recording the originating formula name and step path.
func SetSourceInfo(formula *Formula) {
	setSourceInfoRecursive(formula.Steps, formula.Formula, "steps")
	// Also set source info on template steps for expansion formulas
	setSourceInfoRecursive(formula.Template, formula.Formula, "template")
}

// setSourceInfoRecursive recursively sets source info on steps.
func setSourceInfoRecursive(steps []*Step, formulaName, pathPrefix string) {
	for i, step := range steps {
		step.SourceFormula = formulaName
		step.SourceLocation = fmt.Sprintf("%s[%d]", pathPrefix, i)

		if len(step.Children) > 0 {
			childPath := fmt.Sprintf("%s[%d].children", pathPrefix, i)
			setSourceInfoRecursive(step.Children, formulaName, childPath)
		}

		// Handle loop body steps
		if step.Loop != nil && len(step.Loop.Body) > 0 {
			bodyPath := fmt.Sprintf("%s[%d].loop.body", pathPrefix, i)
			setSourceInfoRecursive(step.Loop.Body, formulaName, bodyPath)
		}
	}
}

// Package formula provides parsing and validation for .formula.json files.
//
// Formulas are high-level workflow templates that compile down to proto beads.
// They support:
//   - Variable definitions with defaults and validation
//   - Step definitions that become issue hierarchies
//   - Composition rules for bonding formulas together
//   - Inheritance via extends
//
// Example .formula.json:
//
//	{
//	  "formula": "mol-feature",
//	  "description": "Standard feature workflow",
//	  "version": 1,
//	  "type": "workflow",
//	  "vars": {
//	    "component": {
//	      "description": "Component name",
//	      "required": true
//	    }
//	  },
//	  "steps": [
//	    {"id": "design", "title": "Design {{component}}", "type": "task"},
//	    {"id": "implement", "title": "Implement {{component}}", "depends_on": ["design"]}
//	  ]
//	}
package formula

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Type categorizes formulas by their purpose.
type Type string

const (
	// TypeWorkflow is a standard workflow template (sequence of steps).
	TypeWorkflow Type = "workflow"

	// TypeExpansion is a macro that expands into multiple steps.
	// Used for common patterns like "test + lint + build".
	TypeExpansion Type = "expansion"

	// TypeAspect is a cross-cutting concern that can be applied to other formulas.
	// Examples: add logging steps, add approval gates.
	TypeAspect Type = "aspect"
)

// IsValid checks if the formula type is recognized.
func (t Type) IsValid() bool {
	switch t {
	case TypeWorkflow, TypeExpansion, TypeAspect:
		return true
	}
	return false
}

// Formula is the root structure for .formula.json files.
type Formula struct {
	// Formula is the unique identifier/name for this formula.
	// Convention: mol-<name> for molecules, exp-<name> for expansions.
	Formula string `json:"formula"`

	// Description explains what this formula does.
	Description string `json:"description,omitempty"`

	// Version is the formula revision.
	// It is intentionally not a graph.v2 opt-in: legacy molecule formulas use
	// this field for their own revisions and must keep hierarchy-first
	// molecule semantics unless they explicitly declare a graph contract or use
	// graph-only step constructs.
	Version int `json:"version"`

	// Contract opts the formula into a specific runtime contract.
	// "graph.v2" enables graph-first workflow compilation when formula_v2 is enabled.
	Contract string `json:"contract,omitempty" toml:"contract,omitempty"`

	// Type categorizes the formula: workflow, expansion, or aspect.
	Type Type `json:"type"`

	// Extends is a list of parent formulas to inherit from.
	// The child formula inherits all vars, steps, and compose rules.
	// Child definitions override parent definitions with the same ID.
	Extends []string `json:"extends,omitempty"`

	// Vars defines template variables with defaults and validation.
	Vars map[string]*VarDef `json:"vars,omitempty"`

	// Steps defines the work items to create.
	Steps []*Step `json:"steps,omitempty"`

	// Template defines expansion template steps (for TypeExpansion formulas).
	// Template steps use {target} and {target.description} placeholders
	// that get substituted when the expansion is applied to a target step.
	Template []*Step `json:"template,omitempty"`

	// Compose defines composition/bonding rules.
	Compose *ComposeRules `json:"compose,omitempty"`

	// Advice defines step transformations (before/after/around).
	// Applied during cooking to insert steps around matching targets.
	Advice []*AdviceRule `json:"advice,omitempty"`

	// Pointcuts defines target patterns for aspect formulas.
	// Used with TypeAspect to specify which steps the aspect applies to.
	Pointcuts []*Pointcut `json:"pointcuts,omitempty"`

	// Phase indicates the recommended instantiation phase: "liquid" (pour) or "vapor" (wisp).
	// If "vapor", bd pour will warn and suggest using bd mol wisp instead.
	// Patrol and release workflows should typically use "vapor" since they're operational.
	Phase string `json:"phase,omitempty"`

	// Pour controls whether steps are materialized as individual child issues.
	// If true, each step becomes a DB row with dependency tracking (checkpoint recovery).
	// If false (default), only the root issue is created; steps are read inline at prime time.
	// Reserve pour=true for critical, infrequent work (e.g. releases) where step-level
	// tracking is worth the DB overhead. Patrol formulas should NOT set this.
	Pour bool `json:"pour,omitempty"`

	// Source tracks where this formula was loaded from (set by parser).
	Source string `json:"source,omitempty"`
}

// VarDef defines a template variable with optional validation.
type VarDef struct {
	// Description explains what this variable is for.
	Description string `json:"description,omitempty"`

	// Default is the value to use if not provided.
	// nil means no default (variable must be provided if referenced).
	// Non-nil (including &"") means the variable has an explicit default.
	Default *string `json:"default,omitempty"`

	// Required indicates the variable must be provided (no default).
	Required bool `json:"required,omitempty"`

	// Enum lists the allowed values (if non-empty).
	Enum []string `json:"enum,omitempty"`

	// Pattern is a regex pattern the value must match.
	Pattern string `json:"pattern,omitempty"`

	// Type is the expected value type: string (default), int, bool.
	Type string `json:"type,omitempty"`
}

// UnmarshalTOML implements toml.Unmarshaler for VarDef.
// This allows vars to be defined as either simple strings or tables:
//
//	[vars]
//	wisp_type = "patrol"           # simple string -> Default = "patrol"
//
//	[vars.component]               # table with full definition
//	description = "Component name"
//	required = true
func (v *VarDef) UnmarshalTOML(data interface{}) error {
	switch val := data.(type) {
	case string:
		// Simple string value becomes the default
		v.Default = &val
		return nil
	case map[string]interface{}:
		// Table format - parse each field
		if desc, ok := val["description"].(string); ok {
			v.Description = desc
		}
		if def, ok := val["default"].(string); ok {
			v.Default = &def
		}
		if req, ok := val["required"].(bool); ok {
			v.Required = req
		}
		if enum, ok := val["enum"].([]interface{}); ok {
			for _, e := range enum {
				if s, ok := e.(string); ok {
					v.Enum = append(v.Enum, s)
				}
			}
		}
		if pattern, ok := val["pattern"].(string); ok {
			v.Pattern = pattern
		}
		if typ, ok := val["type"].(string); ok {
			v.Type = typ
		}
		return nil
	default:
		return fmt.Errorf("type mismatch for formula.VarDef: expected string or table but found %T", data)
	}
}

// Step defines a work item to create when the formula is instantiated.
type Step struct {
	// ID is the unique identifier within this formula.
	// Used for dependency references and bond points.
	ID string `json:"id"`

	// Title is the issue title (supports {{variable}} substitution).
	Title string `json:"title"`

	// Description is the issue description (supports substitution).
	Description string `json:"description,omitempty"`

	// DescriptionFile is a path to a file whose contents replace Description.
	// Resolved relative to the formula file's directory at compile time.
	// If both Description and DescriptionFile are set, DescriptionFile wins.
	DescriptionFile string `json:"description_file,omitempty" toml:"description_file,omitempty"`

	// Notes are additional notes for the issue (supports substitution).
	Notes string `json:"notes,omitempty"`

	// Type is the issue type: task, bug, feature, epic, chore.
	Type string `json:"type,omitempty"`

	// Priority is the issue priority (0-4).
	Priority *int `json:"priority,omitempty"`

	// Labels are applied to the created issue.
	// TOML key is "tags" (formula author facing); JSON/Go name is "labels" (bead facing).
	Labels []string `json:"labels,omitempty" toml:"tags,omitempty"`

	// Metadata is copied to the cooked issue metadata as string key/value pairs.
	// Reserved runtime keys under the gc.* namespace may be added by transforms.
	Metadata map[string]string `json:"metadata,omitempty" toml:"metadata,omitempty"`

	// DependsOn lists step IDs this step blocks on (within the formula).
	DependsOn []string `json:"depends_on,omitempty" toml:"depends_on,omitempty"`

	// Needs is a simpler alias for DependsOn - lists sibling step IDs that must complete first.
	// Either Needs or DependsOn can be used; they are merged during cooking.
	Needs []string `json:"needs,omitempty" toml:"needs,omitempty"`

	// WaitsFor specifies a fanout gate type for this step.
	// Values: "all-children" (wait for all dynamic children) or "any-children" (wait for first).
	// When set, the cooked issue gets a "gate:<value>" label.
	WaitsFor string `json:"waits_for,omitempty" toml:"waits_for,omitempty"`

	// Assignee is the default assignee (supports substitution).
	Assignee string `json:"assignee,omitempty"`

	// Expand references an expansion formula to inline here.
	// When set, this step is replaced by the expansion's template steps.
	// See ApplyInlineExpansions in expand.go for implementation.
	Expand string `json:"expand,omitempty"`

	// ExpandVars are variable overrides for the expansion.
	// Merged with the expansion formula's default vars during inline expansion.
	ExpandVars map[string]string `json:"expand_vars,omitempty" toml:"expand_vars,omitempty"`

	// Condition makes this step optional based on a variable.
	// Format: "{{var}}" (truthy), "!{{var}}" (negated), "{{var}} == value", "{{var}} != value".
	// Evaluated at cook/pour time via FilterStepsByCondition.
	Condition string `json:"condition,omitempty"`

	// Children are nested steps (for creating epic hierarchies).
	Children []*Step `json:"children,omitempty"`

	// Gate defines an async wait condition for this step.
	// When set, bd cook creates a gate issue that blocks this step.
	// Close the gate issue (bd close bd-xxx.gate-stepid) to unblock.
	Gate *Gate `json:"gate,omitempty"`

	// Loop defines iteration for this step.
	// When set, the step becomes a container that expands its body.
	Loop *LoopSpec `json:"loop,omitempty"`

	// OnComplete defines actions triggered when this step completes.
	// Used for runtime expansion over step output (the for-each construct).
	OnComplete *OnCompleteSpec `json:"on_complete,omitempty" toml:"on_complete,omitempty"`

	// Ralph wraps this step in an inline run/check retry loop.
	// The original step becomes a logical container, and the actionable work is
	// emitted as first-class graph steps.
	// JSON storage intentionally retains the legacy "ralph" field name for
	// backward-compatible step snapshots; the parser also accepts canonical
	// public "check" input.
	Ralph *RalphSpec `json:"ralph,omitempty" toml:"ralph,omitempty"`

	// Retry wraps an executable step in an inline attempt/eval retry loop.
	// The original step becomes a stable logical container, and the actionable
	// work is emitted as first-class graph steps.
	Retry *RetrySpec `json:"retry,omitempty" toml:"retry,omitempty"`

	// Timeout is the maximum duration for this step's Ralph check script.
	// Gate condition scripts use gate.timeout instead.
	// Format: Go duration string (e.g., "5m", "2m30s", "300s").
	// Overrides DefaultGateTimeout (5m) unless check.timeout is set.
	Timeout string `json:"timeout,omitempty" toml:"timeout,omitempty"`

	// Source tracing fields: track where this step came from.
	// These are set during parsing/transformation and copied to Issues during cooking.

	// SourceFormula is the formula name where this step was defined.
	// For inherited steps, this is the parent formula, not the final composed formula.
	SourceFormula string `json:"-"` // Internal only, not serialized to JSON

	// SourceLocation is the path within the source formula.
	// Format: "steps[0]", "steps[2].children[1]", "advice[0].after", "loop.body[0]"
	SourceLocation string `json:"-"` // Internal only, not serialized to JSON
}

// UnmarshalJSON accepts the canonical public "check" spelling while keeping the
// internal runtime field wired through Ralph.
func (s *Step) UnmarshalJSON(data []byte) error {
	type stepAlias Step

	var decoded stepAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*s = Step(decoded)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	rawCheck, hasCheck := raw["check"]
	rawRalph, hasRalph := raw["ralph"]
	return s.normalizeCheckAlias(hasCheck, rawCheck, hasRalph, rawRalph)
}

// UnmarshalTOML accepts the canonical public "check" spelling while keeping the
// internal runtime field wired through Ralph.
func (s *Step) UnmarshalTOML(data interface{}) error {
	raw, ok := data.(map[string]interface{})
	if !ok {
		return fmt.Errorf("type mismatch for formula.Step: expected table but found %T", data)
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("encode formula.Step: %w", err)
	}

	var decoded stepTOMLAlias
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return fmt.Errorf("decode formula.Step: %w", err)
	}
	step, err := decoded.toStep()
	if err != nil {
		return err
	}
	*s = step

	rawCheck, hasCheck := raw["check"]
	rawRalph, hasRalph := raw["ralph"]
	return s.normalizeCheckAlias(hasCheck, rawCheck, hasRalph, rawRalph)
}

func (s *Step) normalizeCheckAlias(hasCheck bool, rawCheck interface{}, hasRalph bool, rawRalph interface{}) error {
	if hasCheck && hasRalph {
		return fmt.Errorf("step.check: cannot be specified more than once")
	}

	switch {
	case hasCheck:
		spec, err := decodePublicCheckSpec(rawCheck)
		if err != nil {
			return err
		}
		s.Ralph = spec
	case hasRalph:
		if err := validatePublicCheckSpecShape(rawRalph); err != nil {
			return err
		}
	}

	return nil
}

type stepTOMLAlias struct {
	ID              string            `json:"id"`
	Title           string            `json:"title"`
	Description     string            `json:"description,omitempty"`
	DescriptionFile string            `json:"description_file,omitempty"`
	Notes           string            `json:"notes,omitempty"`
	Type            string            `json:"type,omitempty"`
	Priority        *int              `json:"priority,omitempty"`
	Labels          []string          `json:"tags,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	DependsOn       []string          `json:"depends_on,omitempty"`
	Needs           []string          `json:"needs,omitempty"`
	WaitsFor        string            `json:"waits_for,omitempty"`
	Assignee        string            `json:"assignee,omitempty"`
	Expand          string            `json:"expand,omitempty"`
	ExpandVars      map[string]string `json:"expand_vars,omitempty"`
	Condition       string            `json:"condition,omitempty"`
	Children        []*stepTOMLAlias  `json:"children,omitempty"`
	Gate            *Gate             `json:"gate,omitempty"`
	Loop            *loopTOMLAlias    `json:"loop,omitempty"`
	OnComplete      *OnCompleteSpec   `json:"on_complete,omitempty"`
	Check           json.RawMessage   `json:"check,omitempty"`
	Ralph           json.RawMessage   `json:"ralph,omitempty"`
	Retry           *RetrySpec        `json:"retry,omitempty"`
	Timeout         string            `json:"timeout,omitempty"`
}

type loopTOMLAlias struct {
	Count int              `json:"count,omitempty"`
	Until string           `json:"until,omitempty"`
	Max   int              `json:"max,omitempty"`
	Range string           `json:"range,omitempty"`
	Var   string           `json:"var,omitempty"`
	Body  []*stepTOMLAlias `json:"body"`
}

func (a stepTOMLAlias) toStep() (Step, error) {
	hasCheck := len(a.Check) > 0
	hasRalph := len(a.Ralph) > 0
	if hasCheck && hasRalph {
		return Step{}, fmt.Errorf("step.check: cannot be specified more than once")
	}

	children := make([]*Step, 0, len(a.Children))
	for _, child := range a.Children {
		if child == nil {
			continue
		}
		step, err := child.toStep()
		if err != nil {
			return Step{}, err
		}
		children = append(children, &step)
	}

	var ralph *RalphSpec
	switch {
	case hasCheck:
		spec, err := decodePublicCheckSpec(a.Check)
		if err != nil {
			return Step{}, err
		}
		ralph = spec
	case hasRalph:
		spec, err := decodePublicCheckSpec(a.Ralph)
		if err != nil {
			return Step{}, err
		}
		ralph = spec
	}
	loop, err := a.Loop.toLoopSpec()
	if err != nil {
		return Step{}, err
	}

	return Step{
		ID:              a.ID,
		Title:           a.Title,
		Description:     a.Description,
		DescriptionFile: a.DescriptionFile,
		Notes:           a.Notes,
		Type:            a.Type,
		Priority:        a.Priority,
		Labels:          a.Labels,
		Metadata:        a.Metadata,
		DependsOn:       a.DependsOn,
		Needs:           a.Needs,
		WaitsFor:        a.WaitsFor,
		Assignee:        a.Assignee,
		Expand:          a.Expand,
		ExpandVars:      a.ExpandVars,
		Condition:       a.Condition,
		Children:        children,
		Gate:            a.Gate,
		Loop:            loop,
		OnComplete:      a.OnComplete,
		Ralph:           ralph,
		Retry:           a.Retry,
		Timeout:         a.Timeout,
	}, nil
}

func (a *loopTOMLAlias) toLoopSpec() (*LoopSpec, error) {
	if a == nil {
		return nil, nil
	}

	body := make([]*Step, 0, len(a.Body))
	for _, child := range a.Body {
		if child == nil {
			continue
		}
		step, err := child.toStep()
		if err != nil {
			return nil, err
		}
		body = append(body, &step)
	}

	return &LoopSpec{
		Count: a.Count,
		Until: a.Until,
		Max:   a.Max,
		Range: a.Range,
		Var:   a.Var,
		Body:  body,
	}, nil
}

func decodePublicCheckSpec(raw interface{}) (*RalphSpec, error) {
	if err := validatePublicCheckSpecShape(raw); err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("step.check: encode spec: %w", err)
	}
	if string(encoded) == "null" {
		return nil, nil
	}

	var spec RalphSpec
	if err := json.Unmarshal(encoded, &spec); err != nil {
		return nil, fmt.Errorf("step.check: decode spec: %w", err)
	}
	return &spec, nil
}

func validatePublicCheckSpecShape(raw interface{}) error {
	if raw == nil {
		return nil
	}

	encoded, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("step.check: encode spec: %w", err)
	}
	if string(encoded) == "null" {
		return nil
	}

	var spec map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &spec); err != nil {
		return fmt.Errorf("step.check: expected an object")
	}

	for key, value := range spec {
		switch key {
		case "max_attempts":
			continue
		case "check":
			if err := validatePublicCheckBodyShape(value); err != nil {
				return err
			}
		case "exec", "inference":
			return fmt.Errorf("step.check: unsupported key %q (expected max_attempts or check)", key)
		default:
			continue
		}
	}

	return nil
}

func validatePublicCheckBodyShape(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("step.check.check: expected an object")
	}

	for key := range body {
		switch key {
		case "exec", "inference":
			return fmt.Errorf("step.check.check: unsupported key %q (expected mode, path, or timeout)", key)
		default:
			continue
		}
	}

	return nil
}

// Gate defines an async wait condition for formula steps.
// When a step has a Gate, bd cook creates a gate issue that blocks the step.
// The gate must be closed (manually or via watchers) to unblock the step.
type Gate struct {
	// Type is the condition type: gh:run, gh:pr, timer, human, mail.
	Type string `json:"type"`

	// ID is the condition identifier (e.g., workflow name for gh:run).
	ID string `json:"id,omitempty"`

	// Timeout is how long to wait before escalation (e.g., "1h", "24h").
	Timeout string `json:"timeout,omitempty"`
}

// RalphSpec defines an inline run/check retry loop.
type RalphSpec struct {
	// MaxAttempts bounds the total number of run/check attempts, including the first.
	MaxAttempts int `json:"max_attempts,omitempty" toml:"max_attempts,omitempty"`

	// Check defines how each attempt is validated.
	Check *RalphCheckSpec `json:"check,omitempty" toml:"check,omitempty"`
}

// RalphCheckSpec defines the validation step for a Ralph attempt.
type RalphCheckSpec struct {
	// Mode is the checker implementation. V0 supports only "exec".
	Mode string `json:"mode,omitempty" toml:"mode,omitempty"`

	// Path is the repo-relative or absolute script path to execute.
	Path string `json:"path,omitempty" toml:"path,omitempty"`

	// Timeout bounds script execution (for example "2m").
	Timeout string `json:"timeout,omitempty" toml:"timeout,omitempty"`
}

// RetrySpec defines first-class transient retry semantics for an executable step.
type RetrySpec struct {
	// MaxAttempts bounds the total number of attempts, including the first.
	MaxAttempts int `json:"max_attempts,omitempty" toml:"max_attempts,omitempty"`

	// OnExhausted controls the terminal outcome when a transient failure
	// exhausts the attempt budget. Supported values: "hard_fail", "soft_fail".
	OnExhausted string `json:"on_exhausted,omitempty" toml:"on_exhausted,omitempty"`
}

// LoopSpec defines iteration over a body of steps.
// One of Count, Until, or Range must be specified.
type LoopSpec struct {
	// Count is the fixed number of iterations.
	// When set, the loop body is expanded Count times.
	Count int `json:"count,omitempty"`

	// Until is a condition that ends the loop.
	// Format matches condition evaluator syntax (e.g., "step.status == 'complete'").
	Until string `json:"until,omitempty"`

	// Max is the maximum iterations for conditional loops.
	// Required when Until is set, to prevent unbounded loops.
	Max int `json:"max,omitempty"`

	// Range specifies a computed range for iteration.
	// Format: "start..end" where start and end can be:
	//   - Integers: "1..10"
	//   - Expressions: "1..2^{disks}" (evaluated at cook time)
	//   - Variables: "{start}..{count}" (substituted from Vars)
	// Supports: + - * / ^ (power) and parentheses.
	Range string `json:"range,omitempty"`

	// Var is the variable name exposed to body steps.
	// For Range loops, this is set to the current iteration value.
	// Example: var: "move_num" with range: "1..7" exposes {move_num}=1,2,...,7
	Var string `json:"var,omitempty"`

	// Body contains the steps to repeat.
	Body []*Step `json:"body"`
}

// OnCompleteSpec defines actions triggered when a step completes.
// Used for runtime expansion over step output (the for-each construct).
//
// Example YAML:
//
//	step: survey-workers
//	on_complete:
//	  for_each: output.polecats
//	  bond: mol-polecat-arm
//	  vars:
//	    polecat_name: "{item.name}"
//	    rig: "{item.rig}"
//	  parallel: true
type OnCompleteSpec struct {
	// ForEach is the path to the iterable collection in step output.
	// Format: "output.<field>" or "output.<field>.<nested>"
	// The collection must be an array at runtime.
	ForEach string `json:"for_each,omitempty" toml:"for_each,omitempty"`

	// Bond is the formula to instantiate for each item.
	// A new molecule is created for each element in the ForEach collection.
	Bond string `json:"bond,omitempty"`

	// Vars are variable bindings for each iteration.
	// Supports placeholders:
	//   - {item} - the current item value (for primitives)
	//   - {item.field} - a field from the current item (for objects)
	//   - {index} - the zero-based iteration index
	Vars map[string]string `json:"vars,omitempty"`

	// Parallel runs all bonded molecules concurrently (default behavior).
	// Set to true to make this explicit.
	Parallel bool `json:"parallel,omitempty"`

	// Sequential runs bonded molecules one at a time.
	// Each molecule starts only after the previous one completes.
	// Mutually exclusive with Parallel.
	Sequential bool `json:"sequential,omitempty"`
}

// BranchRule defines parallel execution paths that rejoin.
// Creates a fork-join pattern: from -> [parallel steps] -> join.
type BranchRule struct {
	// From is the step ID that precedes the parallel paths.
	// All branch steps will depend on this step.
	From string `json:"from"`

	// Steps are the step IDs that run in parallel.
	// These steps will all depend on From.
	Steps []string `json:"steps"`

	// Join is the step ID that follows all parallel paths.
	// This step will depend on all Steps completing.
	Join string `json:"join"`
}

// GateRule defines a condition that must be satisfied before a step proceeds.
// Gates are evaluated at runtime by the patrol executor.
type GateRule struct {
	// Before is the step ID that the gate applies to.
	// The condition must be satisfied before this step can start.
	Before string `json:"before"`

	// Condition is the expression to evaluate.
	// Format matches condition evaluator syntax (e.g., "tests.status == 'complete'").
	Condition string `json:"condition"`
}

// ComposeRules define how formulas can be bonded together.
type ComposeRules struct {
	// BondPoints are named locations where other formulas can attach.
	BondPoints []*BondPoint `json:"bond_points,omitempty" toml:"bond_points,omitempty"`

	// Hooks are automatic attachments triggered by labels or conditions.
	Hooks []*Hook `json:"hooks,omitempty"`

	// Expand applies an expansion template to a single target step.
	// The target step is replaced by the expanded template steps.
	Expand []*ExpandRule `json:"expand,omitempty"`

	// Map applies an expansion template to all steps matching a pattern.
	// Each matching step is replaced by the expanded template steps.
	Map []*MapRule `json:"map,omitempty"`

	// Branch defines fork-join parallel execution patterns.
	// Each rule creates dependencies for parallel paths that rejoin.
	Branch []*BranchRule `json:"branch,omitempty"`

	// Gate defines conditional waits before steps.
	// Each rule adds a condition that must be satisfied at runtime.
	Gate []*GateRule `json:"gate,omitempty"`

	// Aspects lists aspect formula names to apply to this formula.
	// Aspects are applied after expansions, adding before/after/around
	// steps to matching targets based on the aspect's advice rules.
	// Example: ["security-audit", "logging"]
	Aspects []string `json:"aspects,omitempty"`
}

// ExpandRule applies an expansion template to a single target step.
type ExpandRule struct {
	// Target is the step ID to expand.
	Target string `json:"target"`

	// With is the name of the expansion formula to apply.
	With string `json:"with"`

	// Vars are variable overrides for the expansion.
	Vars map[string]string `json:"vars,omitempty"`
}

// MapRule applies an expansion template to all matching steps.
type MapRule struct {
	// Select is a glob pattern matching step IDs to expand.
	// Examples: "*.implement", "shiny.*"
	Select string `json:"select"`

	// With is the name of the expansion formula to apply.
	With string `json:"with"`

	// Vars are variable overrides for the expansion.
	Vars map[string]string `json:"vars,omitempty"`
}

// BondPoint is a named attachment site for composition.
type BondPoint struct {
	// ID is the unique identifier for this bond point.
	ID string `json:"id"`

	// Description explains what should be attached here.
	Description string `json:"description,omitempty"`

	// AfterStep is the step ID after which to attach.
	// Mutually exclusive with BeforeStep.
	AfterStep string `json:"after_step,omitempty" toml:"after_step,omitempty"`

	// BeforeStep is the step ID before which to attach.
	// Mutually exclusive with AfterStep.
	BeforeStep string `json:"before_step,omitempty" toml:"before_step,omitempty"`

	// Parallel makes attached steps run in parallel with the anchor step.
	Parallel bool `json:"parallel,omitempty"`
}

// Hook defines automatic formula attachment based on conditions.
type Hook struct {
	// Trigger is what activates this hook.
	// Formats: "label:security", "type:bug", "priority:0-1".
	Trigger string `json:"trigger"`

	// Attach is the formula to attach when triggered.
	Attach string `json:"attach"`

	// At is the bond point to attach at (default: end).
	At string `json:"at,omitempty"`

	// Vars are variable overrides for the attached formula.
	Vars map[string]string `json:"vars,omitempty"`
}

// Pointcut defines a target pattern for advice application.
// Used in aspect formulas to specify which steps the advice applies to.
type Pointcut struct {
	// Glob is a glob pattern to match step IDs.
	// Examples: "*.implement", "shiny.*", "review"
	Glob string `json:"glob,omitempty"`

	// Type matches steps by their type field.
	// Examples: "task", "bug", "epic"
	Type string `json:"type,omitempty"`

	// Label matches steps that have a specific label.
	Label string `json:"label,omitempty"`
}

// AdviceRule defines a step transformation rule.
// Advice operators insert steps before, after, or around matching targets.
type AdviceRule struct {
	// Target is a glob pattern matching step IDs to apply advice to.
	// Examples: "*.implement", "design", "shiny.*"
	Target string `json:"target"`

	// Before inserts a step before the target.
	Before *AdviceStep `json:"before,omitempty"`

	// After inserts a step after the target.
	After *AdviceStep `json:"after,omitempty"`

	// Around wraps the target with before and after steps.
	Around *AroundAdvice `json:"around,omitempty"`
}

// AdviceStep defines a step to insert via advice.
type AdviceStep struct {
	// ID is the step identifier. Supports {step.id} substitution.
	ID string `json:"id"`

	// Title is the step title. Supports {step.id} substitution.
	Title string `json:"title,omitempty"`

	// Description is the step description.
	Description string `json:"description,omitempty"`

	// Type is the issue type (task, bug, etc).
	Type string `json:"type,omitempty"`

	// Args are additional context passed to the step.
	Args map[string]string `json:"args,omitempty"`

	// Output defines expected outputs from this step.
	Output map[string]string `json:"output,omitempty"`
}

// AroundAdvice wraps a target with before and after steps.
type AroundAdvice struct {
	// Before is a list of steps to insert before the target.
	Before []*AdviceStep `json:"before,omitempty"`

	// After is a list of steps to insert after the target.
	After []*AdviceStep `json:"after,omitempty"`
}

func requiresExplicitGraphContract(f *Formula) bool {
	if f == nil || strings.TrimSpace(f.Contract) != "" {
		return false
	}
	if f.Version < 2 {
		if stepsRequireDetachedGraphContract(f.Steps) {
			return true
		}
		return stepsRequireDetachedGraphContract(f.Template)
	}
	if stepsRequireGraphContract(f.Steps) {
		return true
	}
	return stepsRequireGraphContract(f.Template)
}

func stepsRequireDetachedGraphContract(steps []*Step) bool {
	for _, step := range steps {
		if stepRequiresDetachedGraphContract(step) {
			return true
		}
	}
	return false
}

func stepRequiresDetachedGraphContract(step *Step) bool {
	if step == nil {
		return false
	}
	if metadataRequiresGraphContract(step.Metadata) {
		return true
	}
	if step.Loop != nil && stepsRequireDetachedGraphContract(step.Loop.Body) {
		return true
	}
	return stepsRequireDetachedGraphContract(step.Children)
}

func stepsRequireGraphContract(steps []*Step) bool {
	for _, step := range steps {
		if stepRequiresGraphContract(step) {
			return true
		}
	}
	return false
}

func stepRequiresGraphContract(step *Step) bool {
	if step == nil {
		return false
	}
	if step.Ralph != nil || step.Retry != nil || step.OnComplete != nil || metadataRequiresGraphContract(step.Metadata) {
		return true
	}
	if step.Loop != nil && stepsRequireGraphContract(step.Loop.Body) {
		return true
	}
	return stepsRequireGraphContract(step.Children)
}

func metadataRequiresGraphContract(metadata map[string]string) bool {
	for rawKey, rawValue := range metadata {
		key := strings.TrimSpace(rawKey)
		value := strings.TrimSpace(rawValue)
		switch key {
		case "gc.kind":
			switch value {
			case "scope", "cleanup", "scope-check", "workflow-finalize", "retry", "retry-run", "retry-eval", "ralph", "run", "check":
				return true
			}
		case "gc.scope_name", "gc.scope_role", "gc.scope_ref", "gc.continuation_group", "gc.on_fail":
			return true
		}
	}
	return false
}

// Validate checks the formula for structural errors.
func (f *Formula) Validate() error {
	var errs []string

	if f.Formula == "" {
		errs = append(errs, "formula: name is required")
	}

	if f.Version < 1 {
		errs = append(errs, "version: must be >= 1")
	}

	if contract := strings.TrimSpace(f.Contract); contract != "" && !strings.EqualFold(contract, "graph.v2") {
		errs = append(errs, fmt.Sprintf("contract: invalid value %q (must be graph.v2)", f.Contract))
	}
	if requiresExplicitGraphContract(f) {
		errs = append(errs, `contract: formulas that use graph-only constructs must declare contract = "graph.v2" explicitly`)
	}

	if f.Type != "" && !f.Type.IsValid() {
		errs = append(errs, fmt.Sprintf("type: invalid value %q (must be workflow, expansion, or aspect)", f.Type))
	}

	// Validate variables
	for name, v := range f.Vars {
		if name == "" {
			errs = append(errs, "vars: variable name cannot be empty")
			continue
		}
		if v.Required && v.Default != nil {
			errs = append(errs, fmt.Sprintf("vars.%s: cannot have both required:true and default", name))
		}
	}

	// Validate steps - track where each ID was first defined for better error messages
	stepIDLocations := make(map[string]string) // ID -> location where first defined
	for i, step := range f.Steps {
		prefix := fmt.Sprintf("steps[%d]", i)
		if step.ID == "" {
			errs = append(errs, fmt.Sprintf("%s: id is required", prefix))
			continue
		}
		if firstLoc, exists := stepIDLocations[step.ID]; exists {
			errs = append(errs, fmt.Sprintf("%s: duplicate id %q (first defined at %s)", prefix, step.ID, firstLoc))
		} else {
			stepIDLocations[step.ID] = prefix
		}

		if step.Title == "" && step.Expand == "" {
			errs = append(errs, fmt.Sprintf("%s (%s): title is required (unless using expand)", prefix, step.ID))
		}

		// Validate priority range
		if step.Priority != nil && (*step.Priority < 0 || *step.Priority > 4) {
			errs = append(errs, fmt.Sprintf("%s (%s): priority must be 0-4", prefix, step.ID))
		}

		// Validate timeout format
		if err := validateStepTimeout(prefix, step.ID, step.Timeout, step.Ralph != nil, nil, true); err != "" {
			errs = append(errs, err)
		}
		validateLoopBodyTimeouts(step.Loop, &errs, fmt.Sprintf("%s (%s).loop", prefix, step.ID), nil, true)

		if step.Ralph != nil {
			validateRalph(step.Ralph, &errs, fmt.Sprintf("%s (%s)", prefix, step.ID), step)
		}
		if step.Retry != nil {
			validateRetry(step.Retry, &errs, fmt.Sprintf("%s (%s)", prefix, step.ID), step)
		}

		// Collect child IDs (for dependency validation)
		collectChildIDs(step.Children, stepIDLocations, &errs, prefix)
	}

	// Validate step dependencies reference valid IDs (including children)
	for i, step := range f.Steps {
		for _, dep := range step.DependsOn {
			if _, exists := stepIDLocations[dep]; !exists {
				errs = append(errs, fmt.Sprintf("steps[%d] (%s): depends_on references unknown step %q", i, step.ID, dep))
			}
		}
		// Validate needs field - same validation as depends_on
		for _, need := range step.Needs {
			if _, exists := stepIDLocations[need]; !exists {
				errs = append(errs, fmt.Sprintf("steps[%d] (%s): needs references unknown step %q", i, step.ID, need))
			}
		}
		// Validate waits_for field
		// Valid formats: "all-children", "any-children", "children-of(step-id)"
		if step.WaitsFor != "" {
			if err := validateWaitsFor(step.WaitsFor, stepIDLocations); err != nil {
				errs = append(errs, fmt.Sprintf("steps[%d] (%s): %s", i, step.ID, err.Error()))
			}
		}
		// Validate on_complete field - runtime expansion
		if step.OnComplete != nil {
			validateOnComplete(step.OnComplete, &errs, fmt.Sprintf("steps[%d] (%s)", i, step.ID))
		}
		// Validate children's depends_on and needs recursively
		validateChildDependsOn(step.Children, stepIDLocations, &errs, fmt.Sprintf("steps[%d]", i))
	}

	// Validate compose rules
	if f.Compose != nil {
		for i, bp := range f.Compose.BondPoints {
			if bp.ID == "" {
				errs = append(errs, fmt.Sprintf("compose.bond_points[%d]: id is required", i))
			}
			if bp.AfterStep != "" && bp.BeforeStep != "" {
				errs = append(errs, fmt.Sprintf("compose.bond_points[%d] (%s): cannot have both after_step and before_step", i, bp.ID))
			}
			if bp.AfterStep != "" {
				if _, exists := stepIDLocations[bp.AfterStep]; !exists {
					errs = append(errs, fmt.Sprintf("compose.bond_points[%d] (%s): after_step references unknown step %q", i, bp.ID, bp.AfterStep))
				}
			}
			if bp.BeforeStep != "" {
				if _, exists := stepIDLocations[bp.BeforeStep]; !exists {
					errs = append(errs, fmt.Sprintf("compose.bond_points[%d] (%s): before_step references unknown step %q", i, bp.ID, bp.BeforeStep))
				}
			}
		}

		for i, hook := range f.Compose.Hooks {
			if hook.Trigger == "" {
				errs = append(errs, fmt.Sprintf("compose.hooks[%d]: trigger is required", i))
			}
			if hook.Attach == "" {
				errs = append(errs, fmt.Sprintf("compose.hooks[%d]: attach is required", i))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("formula validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return nil
}

func validateStepTimeout(prefix, stepID, raw string, hasRalph bool, allowedLoopVars map[string]struct{}, allowUnresolvedVars bool) string {
	if raw == "" {
		return ""
	}
	if err := validatePositiveTimeout(fmt.Sprintf("%s (%s)", prefix, stepID), raw, allowedLoopVars, allowUnresolvedVars); err != "" {
		return err
	}
	if !hasRalph {
		return fmt.Sprintf("%s (%s): timeout requires check; convergence gate scripts use convergence.gate_timeout or --gate-timeout", prefix, stepID)
	}
	return ""
}

func validatePositiveTimeout(prefix, raw string, allowedLoopVars map[string]struct{}, allowUnresolvedVars bool) string {
	if raw == "" {
		return ""
	}
	parseRaw := substituteAllowedTimeoutLoopVars(raw, allowedLoopVars)
	if allowUnresolvedVars && rangeVarPattern.MatchString(parseRaw) {
		return ""
	}
	d, err := time.ParseDuration(parseRaw)
	if err != nil {
		return fmt.Sprintf("%s: invalid timeout %q: %v", prefix, raw, err)
	}
	if d <= 0 {
		return fmt.Sprintf("%s: timeout must be positive, got %v", prefix, d)
	}
	return ""
}

func validateRalphCheckTimeout(prefix, raw string, allowedLoopVars map[string]struct{}, allowUnresolvedVars bool) string {
	return validatePositiveTimeout(prefix, raw, allowedLoopVars, allowUnresolvedVars)
}

func substituteAllowedTimeoutLoopVars(raw string, allowedLoopVars map[string]struct{}) string {
	if len(allowedLoopVars) == 0 {
		return raw
	}
	return rangeVarPattern.ReplaceAllStringFunc(raw, func(match string) string {
		name := match[1 : len(match)-1]
		if _, ok := allowedLoopVars[name]; ok {
			return "1"
		}
		return match
	})
}

func validateLoopBodyTimeouts(loop *LoopSpec, errs *[]string, prefix string, allowedLoopVars map[string]struct{}, allowUnresolvedVars bool) {
	if loop == nil {
		return
	}
	validateNestedStepTimeoutsWithOptions(loop.Body, errs, prefix+".body", timeoutLoopVarsFor(loop, allowedLoopVars), allowUnresolvedVars)
}

func timeoutLoopVarsFor(loop *LoopSpec, parent map[string]struct{}) map[string]struct{} {
	if loop == nil || loop.Var == "" {
		return parent
	}
	vars := make(map[string]struct{}, len(parent)+1)
	for k, v := range parent {
		vars[k] = v
	}
	vars[loop.Var] = struct{}{}
	return vars
}

func validateNestedStepTimeoutsWithOptions(steps []*Step, errs *[]string, prefix string, allowedLoopVars map[string]struct{}, allowUnresolvedVars bool) {
	for i, step := range steps {
		if step == nil {
			continue
		}
		stepPrefix := fmt.Sprintf("%s[%d]", prefix, i)
		if err := validateStepTimeout(stepPrefix, step.ID, step.Timeout, step.Ralph != nil, allowedLoopVars, allowUnresolvedVars); err != "" {
			*errs = append(*errs, err)
		}
		if step.Ralph != nil && step.Ralph.Check != nil {
			if err := validateRalphCheckTimeout(fmt.Sprintf("%s (%s).check.check", stepPrefix, step.ID), step.Ralph.Check.Timeout, allowedLoopVars, allowUnresolvedVars); err != "" {
				*errs = append(*errs, err)
			}
		}
		validateNestedStepTimeoutsWithOptions(step.Children, errs, stepPrefix+".children", allowedLoopVars, allowUnresolvedVars)
		validateLoopBodyTimeouts(step.Loop, errs, fmt.Sprintf("%s (%s).loop", stepPrefix, step.ID), allowedLoopVars, allowUnresolvedVars)
	}
}

// collectChildIDs recursively collects step IDs from children.
// idLocations maps ID -> location where first defined (for better duplicate error messages).
func collectChildIDs(children []*Step, idLocations map[string]string, errs *[]string, prefix string) {
	for i, child := range children {
		childPrefix := fmt.Sprintf("%s.children[%d]", prefix, i)
		if child.ID == "" {
			*errs = append(*errs, fmt.Sprintf("%s: id is required", childPrefix))
			continue
		}
		if firstLoc, exists := idLocations[child.ID]; exists {
			*errs = append(*errs, fmt.Sprintf("%s: duplicate id %q (first defined at %s)", childPrefix, child.ID, firstLoc))
		} else {
			idLocations[child.ID] = childPrefix
		}

		if child.Title == "" && child.Expand == "" {
			*errs = append(*errs, fmt.Sprintf("%s (%s): title is required", childPrefix, child.ID))
		}

		// Validate priority range for children
		if child.Priority != nil && (*child.Priority < 0 || *child.Priority > 4) {
			*errs = append(*errs, fmt.Sprintf("%s (%s): priority must be 0-4", childPrefix, child.ID))
		}

		// Validate timeout format for children
		if err := validateStepTimeout(childPrefix, child.ID, child.Timeout, child.Ralph != nil, nil, true); err != "" {
			*errs = append(*errs, err)
		}
		validateLoopBodyTimeouts(child.Loop, errs, fmt.Sprintf("%s (%s).loop", childPrefix, child.ID), nil, true)

		if child.Ralph != nil {
			validateRalph(child.Ralph, errs, fmt.Sprintf("%s (%s)", childPrefix, child.ID), child)
		}
		if child.Retry != nil {
			validateRetry(child.Retry, errs, fmt.Sprintf("%s (%s)", childPrefix, child.ID), child)
		}

		collectChildIDs(child.Children, idLocations, errs, childPrefix)
	}
}

// WaitsForSpec holds the parsed waits_for field.
type WaitsForSpec struct {
	// Gate is the gate type: "all-children" or "any-children"
	Gate string
	// SpawnerID is the step ID whose children to wait for.
	// Empty means infer from context (typically first step in needs).
	SpawnerID string
}

// ParseWaitsFor parses a waits_for value into its components.
// Returns nil if the value is empty.
func ParseWaitsFor(value string) *WaitsForSpec {
	if value == "" {
		return nil
	}

	// Simple gate types - spawner inferred from needs
	if value == "all-children" || value == "any-children" {
		return &WaitsForSpec{Gate: value}
	}

	// children-of(step-id) syntax
	if strings.HasPrefix(value, "children-of(") && strings.HasSuffix(value, ")") {
		stepID := value[len("children-of(") : len(value)-1]
		return &WaitsForSpec{
			Gate:      "all-children", // Default gate type
			SpawnerID: stepID,
		}
	}

	// Invalid - return nil (validation should have caught this)
	return nil
}

// validateWaitsFor validates the waits_for field value.
// Valid formats:
//   - "all-children": wait for all dynamically-bonded children
//   - "any-children": wait for first child to complete
//   - "children-of(step-id)": wait for children of a specific step
func validateWaitsFor(value string, stepIDLocations map[string]string) error {
	// Simple gate types
	if value == "all-children" || value == "any-children" {
		return nil
	}

	// children-of(step-id) syntax
	if strings.HasPrefix(value, "children-of(") && strings.HasSuffix(value, ")") {
		stepID := value[len("children-of(") : len(value)-1]
		if stepID == "" {
			return fmt.Errorf("waits_for children-of() requires a step ID")
		}
		if _, exists := stepIDLocations[stepID]; !exists {
			return fmt.Errorf("waits_for references unknown step %q in children-of()", stepID)
		}
		return nil
	}

	return fmt.Errorf("waits_for has invalid value %q (must be all-children, any-children, or children-of(step-id))", value)
}

// validateChildDependsOn recursively validates depends_on and needs references for children.
func validateChildDependsOn(children []*Step, idLocations map[string]string, errs *[]string, prefix string) {
	for i, child := range children {
		childPrefix := fmt.Sprintf("%s.children[%d]", prefix, i)
		for _, dep := range child.DependsOn {
			if _, exists := idLocations[dep]; !exists {
				*errs = append(*errs, fmt.Sprintf("%s (%s): depends_on references unknown step %q", childPrefix, child.ID, dep))
			}
		}
		// Validate needs field
		for _, need := range child.Needs {
			if _, exists := idLocations[need]; !exists {
				*errs = append(*errs, fmt.Sprintf("%s (%s): needs references unknown step %q", childPrefix, child.ID, need))
			}
		}
		// Validate waits_for field
		if child.WaitsFor != "" {
			if err := validateWaitsFor(child.WaitsFor, idLocations); err != nil {
				*errs = append(*errs, fmt.Sprintf("%s (%s): %s", childPrefix, child.ID, err.Error()))
			}
		}
		// Validate on_complete field
		if child.OnComplete != nil {
			validateOnComplete(child.OnComplete, errs, fmt.Sprintf("%s (%s)", childPrefix, child.ID))
		}
		validateChildDependsOn(child.Children, idLocations, errs, childPrefix)
	}
}

// validateOnComplete validates an OnCompleteSpec.
func validateOnComplete(oc *OnCompleteSpec, errs *[]string, prefix string) {
	// Check that for_each and bond are both present or both absent
	if oc.ForEach != "" && oc.Bond == "" {
		*errs = append(*errs, fmt.Sprintf("%s.on_complete: bond is required when for_each is set", prefix))
	}
	if oc.ForEach == "" && oc.Bond != "" {
		*errs = append(*errs, fmt.Sprintf("%s.on_complete: for_each is required when bond is set", prefix))
	}

	// Validate for_each path format
	if oc.ForEach != "" {
		if !strings.HasPrefix(oc.ForEach, "output.") {
			*errs = append(*errs, fmt.Sprintf("%s.on_complete: for_each must start with 'output.' (got %q)", prefix, oc.ForEach))
		}
	}

	// Check parallel and sequential are mutually exclusive
	if oc.Parallel && oc.Sequential {
		*errs = append(*errs, fmt.Sprintf("%s.on_complete: cannot set both parallel and sequential", prefix))
	}
}

func validateRalph(spec *RalphSpec, errs *[]string, prefix string, step *Step) {
	if spec.MaxAttempts < 1 {
		*errs = append(*errs, fmt.Sprintf("%s.check: max_attempts must be >= 1", prefix))
	}
	if spec.Check == nil {
		*errs = append(*errs, fmt.Sprintf("%s.check: check is required", prefix))
	} else {
		if spec.Check.Mode == "" {
			*errs = append(*errs, fmt.Sprintf("%s.check.check: mode is required", prefix))
		} else if spec.Check.Mode != "exec" {
			*errs = append(*errs, fmt.Sprintf("%s.check.check: unsupported mode %q (only exec is supported)", prefix, spec.Check.Mode))
		}
		if spec.Check.Path == "" {
			*errs = append(*errs, fmt.Sprintf("%s.check.check: path is required", prefix))
		}
		if err := validateRalphCheckTimeout(fmt.Sprintf("%s.check.check", prefix), spec.Check.Timeout, nil, true); err != "" {
			*errs = append(*errs, err)
		}
	}

	if step.Loop != nil {
		*errs = append(*errs, fmt.Sprintf("%s: check cannot be combined with loop", prefix))
	}
	if step.OnComplete != nil {
		*errs = append(*errs, fmt.Sprintf("%s: check cannot be combined with on_complete", prefix))
	}
	if step.Gate != nil {
		*errs = append(*errs, fmt.Sprintf("%s: check cannot be combined with gate", prefix))
	}
	if step.Expand != "" {
		*errs = append(*errs, fmt.Sprintf("%s: check cannot be combined with expand", prefix))
	}
	if step.Assignee != "" {
		*errs = append(*errs, fmt.Sprintf("%s: check cannot be combined with assignee (route work via gc.run_target instead)", prefix))
	}
	if step.Retry != nil {
		*errs = append(*errs, fmt.Sprintf("%s: check cannot be combined with retry", prefix))
	}
}

func validateRetry(spec *RetrySpec, errs *[]string, prefix string, step *Step) {
	if spec.MaxAttempts < 1 {
		*errs = append(*errs, fmt.Sprintf("%s.retry: max_attempts must be >= 1", prefix))
	}
	switch spec.OnExhausted {
	case "", "hard_fail", "soft_fail":
	default:
		*errs = append(*errs, fmt.Sprintf("%s.retry: unsupported on_exhausted %q (want hard_fail or soft_fail)", prefix, spec.OnExhausted))
	}

	if step.Ralph != nil {
		*errs = append(*errs, fmt.Sprintf("%s: retry cannot be combined with check", prefix))
	}
	if step.Loop != nil {
		*errs = append(*errs, fmt.Sprintf("%s: retry cannot be combined with loop", prefix))
	}
	if step.OnComplete != nil {
		*errs = append(*errs, fmt.Sprintf("%s: retry cannot be combined with on_complete", prefix))
	}
	if step.Gate != nil {
		*errs = append(*errs, fmt.Sprintf("%s: retry cannot be combined with gate", prefix))
	}
	if step.Expand != "" {
		*errs = append(*errs, fmt.Sprintf("%s: retry cannot be combined with expand", prefix))
	}
	if len(step.Children) > 0 {
		*errs = append(*errs, fmt.Sprintf("%s: retry cannot be combined with children", prefix))
	}
}

// GetRequiredVars returns the names of all required variables.
func (f *Formula) GetRequiredVars() []string {
	var required []string
	for name, v := range f.Vars {
		if v.Required {
			required = append(required, name)
		}
	}
	return required
}

// GetStepByID finds a step by its ID (searches recursively).
func (f *Formula) GetStepByID(id string) *Step {
	for _, step := range f.Steps {
		if found := findStepByID(step, id); found != nil {
			return found
		}
	}
	return nil
}

// findStepByID recursively searches for a step by ID.
func findStepByID(step *Step, id string) *Step {
	if step.ID == id {
		return step
	}
	for _, child := range step.Children {
		if found := findStepByID(child, id); found != nil {
			return found
		}
	}
	return nil
}

// StringPtr returns a pointer to s. Useful for constructing VarDef literals.
func StringPtr(s string) *string { return &s }

// GetBondPoint finds a bond point by ID.
func (f *Formula) GetBondPoint(id string) *BondPoint {
	if f.Compose == nil {
		return nil
	}
	for _, bp := range f.Compose.BondPoints {
		if bp.ID == id {
			return bp
		}
	}
	return nil
}

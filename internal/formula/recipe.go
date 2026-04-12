package formula

import (
	"sort"
	"strings"
)

// Recipe is the output of formula compilation. It contains a flattened,
// ordered list of steps with namespaced IDs and all dependency edges.
// Variable placeholders ({{var}}) are preserved — substitution happens
// at instantiation time, not compilation time.
type Recipe struct {
	// Name is the formula name (e.g., "mol-feature").
	Name string

	// Description is the formula's description field.
	Description string

	// Steps is the flattened, ordered step list. Steps[0] is always the
	// root workflow bead. Subsequent entries are in creation order (parent
	// before children, depth-first).
	Steps []RecipeStep

	// Deps is the complete set of dependency edges between steps.
	Deps []RecipeDep

	// Vars holds variable definitions from the formula for default
	// handling during instantiation.
	Vars map[string]*VarDef

	// Phase is the recommended phase: "vapor" (ephemeral) or "liquid"
	// (persistent). Empty string means no recommendation.
	Phase string

	// Pour is true if the formula recommends full materialization
	// (creating child step beads, not just the root).
	Pour bool

	// RootOnly is true when only the root bead should be created,
	// without materializing child steps. This is the default for
	// vapor-phase formulas (patrol wisps).
	RootOnly bool
}

// RecipeStep represents a single step in a compiled recipe.
type RecipeStep struct {
	// ID is the namespaced step identifier (e.g., "mol-feature.implement").
	// For the root workflow bead, this is the formula name itself.
	ID string

	// Title may contain {{variable}} placeholders.
	Title string

	// Description may contain {{variable}} placeholders.
	Description string

	// Notes may contain {{variable}} placeholders.
	Notes string

	// Type is the step type: "molecule", "task", "bug", "epic", "gate", "chore", etc.
	// Root steps default to "molecule". Steps with children are promoted to "epic".
	Type string

	// Priority is 0-4 (0 = highest). Nil means default (2).
	Priority *int

	// Labels from the formula step definition.
	Labels []string

	// Assignee is the agent/user to assign this step to.
	Assignee string

	// IsRoot is true for the root workflow bead (Steps[0]).
	IsRoot bool

	// Metadata is copied to the bead metadata as string key/value pairs.
	Metadata map[string]string

	// Gate holds async gate configuration if this step has one.
	Gate *RecipeGate
}

// RecipeGate describes an async coordination gate on a step.
type RecipeGate struct {
	Type    string // "all-children", "any-children", etc.
	ID      string
	Timeout string
}

// RecipeDep represents a dependency edge between two recipe steps.
type RecipeDep struct {
	// StepID is the step that has the dependency (the blocked step).
	StepID string

	// DependsOnID is the step that must complete first.
	DependsOnID string

	// Type is the dependency type: "blocks", "parent-child", "waits-for".
	Type string

	// Metadata holds optional JSON metadata (e.g., waits-for gate config).
	Metadata string
}

// RootStep returns the root step (always Steps[0]) or nil if empty.
func (r *Recipe) RootStep() *RecipeStep {
	if len(r.Steps) == 0 {
		return nil
	}
	return &r.Steps[0]
}

// StepByID returns the step with the given ID, or nil if not found.
func (r *Recipe) StepByID(id string) *RecipeStep {
	for i := range r.Steps {
		if r.Steps[i].ID == id {
			return &r.Steps[i]
		}
	}
	return nil
}

// VariableNames returns the sorted list of variable names defined in
// the formula.
func (r *Recipe) VariableNames() []string {
	names := make([]string, 0, len(r.Vars))
	for name := range r.Vars {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ReferencedVarDefs returns the subset of Vars that are still referenced
// as {{placeholder}} patterns in the compiled recipe's steps. Vars used
// only for compile-time condition filtering are excluded, preventing
// over-rejection when callers validate vars against a compiled recipe.
func (r *Recipe) ReferencedVarDefs() map[string]*VarDef {
	if len(r.Vars) == 0 {
		return nil
	}

	referenced := make(map[string]bool)
	for _, step := range r.Steps {
		collectVarRefs(step.Title, referenced)
		collectVarRefs(step.Description, referenced)
		collectVarRefs(step.Notes, referenced)
		collectVarRefs(step.Assignee, referenced)
		for _, v := range step.Metadata {
			collectVarRefs(v, referenced)
		}
	}

	result := make(map[string]*VarDef, len(referenced))
	for name := range referenced {
		if def, ok := r.Vars[name]; ok {
			result[name] = def
		}
	}
	return result
}

// collectVarRefs extracts {{varname}} references from s into the seen set.
func collectVarRefs(s string, seen map[string]bool) {
	for {
		start := strings.Index(s, "{{")
		if start < 0 {
			return
		}
		end := strings.Index(s[start:], "}}")
		if end < 0 {
			return
		}
		name := s[start+2 : start+end]
		if isValidVarName(name) {
			seen[name] = true
		}
		s = s[start+end+2:]
	}
}

// isValidVarName checks if name matches [a-zA-Z_][a-zA-Z0-9_]*.
func isValidVarName(name string) bool {
	if len(name) == 0 {
		return false
	}
	for i, c := range name {
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if i == 0 {
			if !letter && c != '_' {
				return false
			}
		} else {
			digit := c >= '0' && c <= '9'
			if !letter && !digit && c != '_' {
				return false
			}
		}
	}
	return true
}

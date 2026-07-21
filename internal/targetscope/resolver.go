// Trusted-source resolution of the target scope, and the write-back that keeps
// the workflow's consumed branch variable equal to the declared scope branch.
//
// THE SPLIT THIS CLOSES. Resolving a winner for the scope OBJECT alone is not
// enough. The workflow never reads gc.target_scope — template substitution
// reads the flattened formula-variable map, and mol-scoped-work consumes
// {{base_branch}} as the actual worktree base and diff base. Injecting the
// routed branch into base_branch INDEPENDENTLY of scope resolution lets an
// explicit gc_target_branch=release land on the object while base_branch=main
// stays in the consumed map: the workflow then executes against main while the
// close gate validates the commit against release. So the reconciled winner is
// written BACK into the carrier the formula actually reads, and the post-
// condition vars[carrier] == scope.Branch holds by construction.
//
// PROVENANCE MUST SURVIVE UNTIL RESOLUTION. The final variable map is
// provenance-lossy: an explicit --var base_branch=X and an auto-injected
// base_branch land in the same key, and by the time the map exists the reason a
// key is present is gone. Resolution therefore consumes typed LAYERS with
// provenance intact, never the flattened map — only the explicit layer may feed
// precedence 1, while the routed branch is durable intent at precedence 3.
package targetscope

import (
	"fmt"
	"sort"
	"strings"
)

// The invocation-surface variable names.
//
// gc_target_branch and gc_target_worktree are scalar strings, not JSON: the
// invocation surface is map[string]string, so v1 carries the two scope fields
// as two scalar vars. The JSON object is the persisted metadata form only.
const (
	// VarTargetBranch is the dedicated scope-branch input. It exists so a
	// formula that consumes neither carrier can still be handed a scope
	// branch without declaring a branch variable.
	VarTargetBranch = "gc_target_branch"
	// VarTargetWorktree is the dedicated scope-worktree input. No existing
	// carrier corresponds to it, so it has no alias set.
	VarTargetWorktree = "gc_target_worktree"
	// VarBaseBranch and VarCarrierTargetBranch are the pre-existing,
	// formula-consumed branch carriers.
	VarBaseBranch          = "base_branch"
	VarCarrierTargetBranch = "target_branch"
)

// BranchAliases is the branch-carrier alias set. All three are co-equal aliases
// of one fact at a given provenance layer; VarTargetBranch does NOT silently
// outrank the carriers.
var BranchAliases = []string{VarTargetBranch, VarBaseBranch, VarCarrierTargetBranch}

// ScopeInputVars are claimed by the resolver as typed scope inputs. They are
// excluded from the flattened variable map so they never reach template
// substitution, the root key, or the persisted runtime-vars blob.
var ScopeInputVars = []string{VarTargetBranch, VarTargetWorktree}

// LayerName identifies a provenance layer for diagnostics.
type LayerName string

const (
	LayerExplicit LayerName = "explicit invocation var"
	LayerMember   LayerName = "declared member scope"
	LayerRig      LayerName = "rig formula_vars"
	LayerRouting  LayerName = "routing-injected branch"
	LayerFormula  LayerName = "formula default"
)

// Sources is the provenance-typed input set.
type Sources struct {
	// Explicit holds raw, UNFLATTENED "key=value" invocation vars. It stays
	// unflattened so that two occurrences of one carrier with different values
	// remain visible: flattening would silently apply last-write-wins to a
	// branch carrier, which v1 rejects instead.
	Explicit []string
	// Member is an already-declared, valid member scope, if any.
	Member    Scope
	MemberSet bool
	// Rig is the rig-scoped formula_vars layer.
	Rig map[string]string
	// Routing is the routing-injected durable intent. The source injects at
	// most one carrier, so this layer cannot self-conflict.
	Routing map[string]string
	// FormulaDefaults are the formula's [vars.*].default values. Including
	// this layer is what keeps the scope object and the consumed carrier
	// identical in the default-only case.
	FormulaDefaults map[string]string
}

// Resolved is the outcome of trusted-source resolution.
type Resolved struct {
	Scope Scope
	// BranchLayer and WorktreeLayer record which layer supplied each field,
	// for error messages and tests. Empty when the field is unknown.
	BranchLayer   LayerName
	WorktreeLayer LayerName
}

// Resolve computes the reconciled scope from trusted sources.
//
// Cross-layer precedence is first-candidate-wins: explicit invocation vars, a
// declared member scope, durable intent (rig then routing), formula defaults.
// Within a layer there is NO tiebreak — disagreeing aliases are a loud operator
// error, because silently picking one would be exactly the "which branch did
// this actually run against" ambiguity the object exists to remove.
//
// storeRoot anchors a relative worktree to its mandatory absolute form. The
// value is normalized HERE, at the boundary, so the single absolute string is
// what gets copied to the root and to every member.
func Resolve(src Sources, storeRoot string) (Resolved, error) {
	explicit := explicitOccurrences(src.Explicit)

	branch, branchLayer, err := resolveBranch(src, explicit)
	if err != nil {
		return Resolved{}, err
	}
	worktree, worktreeLayer, err := resolveWorktree(src, explicit)
	if err != nil {
		return Resolved{}, err
	}

	scope := Scope{V: SchemaVersion, Branch: branch, Worktree: worktree}
	normalized, err := Normalize(scope, storeRoot)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Scope: normalized, BranchLayer: branchLayer, WorktreeLayer: worktreeLayer}, nil
}

func resolveBranch(src Sources, explicit map[string][]string) (string, LayerName, error) {
	if value, ok, err := candidate(LayerExplicit, BranchAliases, explicit); err != nil || ok {
		return value, LayerExplicit, err
	}
	if src.MemberSet && src.Member.Branch != "" {
		return src.Member.Branch, LayerMember, nil
	}
	for _, layer := range []struct {
		name LayerName
		vars map[string]string
	}{
		{LayerRig, src.Rig},
		{LayerRouting, src.Routing},
		{LayerFormula, src.FormulaDefaults},
	} {
		if value, ok, err := candidate(layer.name, BranchAliases, single(layer.vars)); err != nil || ok {
			return value, layer.name, err
		}
	}
	return "", "", nil
}

func resolveWorktree(src Sources, explicit map[string][]string) (string, LayerName, error) {
	// No alias set: only the dedicated input carries a worktree, so a
	// within-layer conflict is impossible and only cross-layer precedence
	// applies.
	aliases := []string{VarTargetWorktree}
	if value, ok, err := candidate(LayerExplicit, aliases, explicit); err != nil || ok {
		return value, LayerExplicit, err
	}
	if src.MemberSet && src.Member.Worktree != "" {
		return src.Member.Worktree, LayerMember, nil
	}
	for _, layer := range []struct {
		name LayerName
		vars map[string]string
	}{
		{LayerRig, src.Rig},
		{LayerRouting, src.Routing},
		{LayerFormula, src.FormulaDefaults},
	} {
		if value, ok, err := candidate(layer.name, aliases, single(layer.vars)); err != nil || ok {
			return value, layer.name, err
		}
	}
	return "", "", nil
}

// candidate applies the within-layer rule to one alias set.
//
// Empty carriers are treated as ABSENT for candidate collection, matching the
// existing injector's skip-empty behavior; an explicit `base_branch=` therefore
// neither blocks reconciliation nor survives it.
func candidate(layer LayerName, aliases []string, values map[string][]string) (string, bool, error) {
	seen := map[string][]string{} // value -> the alias names that carried it
	for _, alias := range aliases {
		for _, raw := range values[alias] {
			value := strings.TrimSpace(raw)
			if value == "" {
				continue
			}
			seen[value] = append(seen[value], alias)
		}
	}
	switch len(seen) {
	case 0:
		return "", false, nil
	case 1:
		for value := range seen {
			return value, true, nil
		}
	}
	return "", false, fmt.Errorf("conflicting target branch at the %s layer: %s; "+
		"these are aliases of one value and v1 has no tiebreak — supply a single branch",
		layer, describeConflict(seen))
}

func describeConflict(seen map[string][]string) string {
	parts := make([]string, 0, len(seen))
	for value, aliases := range seen {
		sort.Strings(aliases)
		parts = append(parts, fmt.Sprintf("%s=%q", strings.Join(dedupe(aliases), "/"), value))
	}
	sort.Strings(parts)
	return strings.Join(parts, " vs ")
}

func dedupe(in []string) []string {
	out := in[:0:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// explicitOccurrences preserves EVERY occurrence of each key so a repeated
// branch carrier with differing values is caught rather than silently
// last-write-wins.
func explicitOccurrences(raw []string) map[string][]string {
	out := map[string][]string{}
	for _, entry := range raw {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		out[key] = append(out[key], value)
	}
	return out
}

func single(vars map[string]string) map[string][]string {
	if len(vars) == 0 {
		return nil
	}
	out := make(map[string][]string, len(vars))
	for key, value := range vars {
		out[key] = []string{value}
	}
	return out
}

// ApplyToVars writes the reconciled branch back into the carrier the formula
// actually consumes, and removes the typed scope inputs from the map.
//
// The write is UNCONDITIONAL: the value being written is the reconciled winner,
// so it cannot lose a conflicting intent — a genuine conflict would already
// have rejected during Resolve. Overwriting is the point, because the carrier
// may hold a routed value that lost to a higher-precedence layer.
//
// When the branch is unknown, the carrier is left alone so the formula's own
// default still applies through the normal overlay, and the scope carries that
// same default.
func ApplyToVars(vars map[string]string, resolved Resolved, usesBaseBranch, usesTargetBranch bool) {
	if vars == nil {
		return
	}
	for _, name := range ScopeInputVars {
		delete(vars, name)
	}
	branch := resolved.Scope.Branch
	if branch == "" {
		return
	}
	if usesBaseBranch {
		vars[VarBaseBranch] = branch
	}
	if usesTargetBranch {
		vars[VarCarrierTargetBranch] = branch
	}
}

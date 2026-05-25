// Package graphv2 centralizes graph.v2 input-convoy invocation rules.
package graphv2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"sort"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
)

var keyedLocks [256]sync.Mutex

const (
	// ConvoyIDVar is the reserved system variable passed to targeted graph.v2
	// formula invocations.
	ConvoyIDVar = "convoy_id"

	// RuntimeVarsMetadataKey stores the caller/runtime vars a graph.v2 workflow
	// root received, excluding graph.v2 reserved variables injected by runtime.
	RuntimeVarsMetadataKey = "gc.graphv2_vars.v1"

	syntheticMetadataKey      = "gc.synthetic"
	syntheticKindMetadataKey  = "gc.synthetic_kind"
	syntheticInputBeadKey     = "gc.input_bead_id"
	graphV2InvocationKey      = "gc.graphv2_invocation_key"
	singletonSyntheticKind    = "singleton-convoy"
	singletonInvocationPrefix = "graphv2-singleton:"
	previewSingletonPrefix    = "preview-singleton:"
)

// Invocation describes a normalized graph.v2 formula invocation.
type Invocation struct {
	Formula     *formula.Formula
	FormulaName string
	InputConvoy string
	Vars        map[string]string
	Targeted    bool
	Singleton   bool
}

// LoadFormula resolves a formula without compiling it to a recipe.
func LoadFormula(formulaName string, searchPaths []string) (*formula.Formula, error) {
	resolved, _, err := loadFormulaWithParser(formulaName, searchPaths)
	return resolved, err
}

func loadFormulaWithParser(formulaName string, searchPaths []string) (*formula.Formula, *formula.Parser, error) {
	parser := formula.NewParser(searchPaths...).SetSource(formula.SourceFromEnv())
	f, err := parser.LoadByName(formulaName)
	if err != nil {
		return nil, nil, err
	}
	resolved, err := parser.Resolve(f)
	if err != nil {
		return nil, nil, err
	}
	return resolved, parser, nil
}

// IsGraphV2Formula reports whether the named formula declares graph.v2.
func IsGraphV2Formula(formulaName string, searchPaths []string) (bool, *formula.Formula, error) {
	resolved, err := LoadFormula(formulaName, searchPaths)
	if err != nil {
		return false, nil, err
	}
	return strings.EqualFold(strings.TrimSpace(resolved.Contract), "graph.v2"), resolved, nil
}

// PrepareInvocation validates and normalizes a graph.v2 invocation. Non-graph
// formulas are returned with Formula set and no input convoy.
func PrepareInvocation(ctx context.Context, store beads.Store, formulaName string, searchPaths []string, targetID string, vars map[string]string) (Invocation, error) {
	resolved, parser, err := loadFormulaWithParser(formulaName, searchPaths)
	if err != nil {
		return Invocation{}, err
	}
	inv := Invocation{
		Formula:     resolved,
		FormulaName: formulaName,
		Vars:        maps.Clone(vars),
		Targeted:    strings.TrimSpace(targetID) != "",
	}
	if inv.Vars == nil {
		inv.Vars = make(map[string]string)
	}
	if !strings.EqualFold(strings.TrimSpace(resolved.Contract), "graph.v2") {
		return inv, nil
	}
	if err := ValidateNoReservedUserVars(inv.Vars); err != nil {
		return Invocation{}, err
	}
	inv.Vars = EffectiveRuntimeVars(resolved, inv.Vars)
	formulaRequiresTarget, err := formula.GraphV2FormulaReferencesInputConvoyTransitively(resolved, parser)
	if err != nil {
		return Invocation{}, err
	}
	recipe, err := compileValidationRecipe(ctx, formulaName, searchPaths, inv.Vars)
	if err != nil {
		return Invocation{}, err
	}
	recipeRequiresTarget := formula.GraphV2RecipeReferencesInputConvoy(recipe)
	if !inv.Targeted {
		if formulaRequiresTarget {
			if err := formula.ValidateGraphV2ReservedSymbolsTransitively(resolved, parser, false); err != nil {
				return Invocation{}, err
			}
			return Invocation{}, fmt.Errorf("graph.v2 formula %q requires a target convoy", formulaName)
		}
		if recipeRequiresTarget {
			if err := formula.ValidateGraphV2RecipeReservedSymbols(recipe, false); err != nil {
				return Invocation{}, err
			}
			return Invocation{}, fmt.Errorf("graph.v2 formula %q requires a target convoy", formulaName)
		}
		if err := formula.ValidateGraphV2RecipeReservedSymbols(recipe, false); err != nil {
			return Invocation{}, err
		}
		return inv, nil
	}
	if err := formula.ValidateGraphV2RecipeReservedSymbols(recipe, true); err != nil {
		return Invocation{}, err
	}
	if err := molecule.ValidateRecipeRuntimeVars(recipe, molecule.Options{Vars: varsWithConvoyPlaceholder(inv.Vars)}); err != nil {
		return Invocation{}, err
	}
	if err := validateDrainItemFormulas(formulaName, searchPaths, recipe, inv.Vars); err != nil {
		return Invocation{}, err
	}
	if store == nil {
		return Invocation{}, fmt.Errorf("graph.v2 formula %q requires a bead store to normalize target %s", formulaName, targetID)
	}
	convoyID, singleton, err := NormalizeInputConvoy(store, targetID)
	if err != nil {
		return Invocation{}, err
	}
	inv.InputConvoy = convoyID
	inv.Singleton = singleton
	inv.Vars[ConvoyIDVar] = convoyID
	return inv, nil
}

func varsWithConvoyPlaceholder(vars map[string]string) map[string]string {
	out := maps.Clone(vars)
	if out == nil {
		out = make(map[string]string, 1)
	}
	out[ConvoyIDVar] = "graphv2-validation-placeholder"
	return out
}

// EffectiveRuntimeVars returns formula defaults overlaid by caller vars. It
// mirrors molecule instantiation's runtime var view so graph.v2 metadata and
// root keys use the same effective inputs that template substitution sees.
func EffectiveRuntimeVars(f *formula.Formula, vars map[string]string) map[string]string {
	out := make(map[string]string, len(vars))
	if f != nil {
		for name, def := range f.Vars {
			if def == nil || def.Default == nil {
				continue
			}
			out[name] = *def.Default
		}
	}
	for key, value := range vars {
		out[key] = value
	}
	if len(out) == 0 {
		return map[string]string{}
	}
	return out
}

func compileValidationRecipe(ctx context.Context, formulaName string, searchPaths []string, vars map[string]string) (*formula.Recipe, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	validationVars := nonReservedRuntimeVars(vars)
	if validationVars == nil {
		validationVars = make(map[string]string, 1)
	}
	validationVars[ConvoyIDVar] = "graphv2-validation-placeholder"
	return formula.CompileWithoutRuntimeVarValidation(ctx, formulaName, searchPaths, validationVars)
}

func validateDrainItemFormulas(parentName string, searchPaths []string, recipe *formula.Recipe, parentVars map[string]string) error {
	for _, itemFormula := range drainItemFormulaNames(recipe) {
		vars := nonReservedRuntimeVars(parentVars)
		if vars == nil {
			vars = make(map[string]string, 1)
		}
		vars[ConvoyIDVar] = "graphv2-validation-placeholder"
		recipe, err := formula.CompileWithoutRuntimeVarValidation(context.Background(), itemFormula, searchPaths, vars)
		if err != nil {
			return fmt.Errorf("validating drain item formula %q for graph.v2 formula %q: %w", itemFormula, parentName, err)
		}
		root := recipe.RootStep()
		if root == nil || root.Metadata["gc.kind"] != "workflow" || !strings.EqualFold(root.Metadata["gc.formula_contract"], "graph.v2") {
			return fmt.Errorf("drain item formula %q for graph.v2 formula %q must declare contract = \"graph.v2\"", itemFormula, parentName)
		}
		if err := molecule.ValidateRecipeRuntimeVars(recipe, molecule.Options{Vars: vars}); err != nil {
			return fmt.Errorf("validating drain item formula %q runtime vars for graph.v2 formula %q: %w", itemFormula, parentName, err)
		}
	}
	return nil
}

// RuntimeVarsMetadata encodes non-reserved runtime vars for persistence on a
// graph.v2 workflow root. It returns an empty string when no vars need storage.
func RuntimeVarsMetadata(vars map[string]string) string {
	filtered := nonReservedRuntimeVars(vars)
	if len(filtered) == 0 {
		return ""
	}
	data, err := json.Marshal(filtered)
	if err != nil {
		return ""
	}
	return string(data)
}

// ParseRuntimeVarsMetadata decodes RuntimeVarsMetadata output, dropping any
// graph.v2 reserved vars defensively.
func ParseRuntimeVarsMetadata(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, err
	}
	return nonReservedRuntimeVars(decoded), nil
}

func nonReservedRuntimeVars(vars map[string]string) map[string]string {
	if len(vars) == 0 {
		return nil
	}
	out := make(map[string]string, len(vars))
	for key, value := range vars {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		switch trimmed {
		case ConvoyIDVar, "issue", "bead_id":
			continue
		default:
			out[trimmed] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func drainItemFormulaNames(recipe *formula.Recipe) []string {
	if recipe == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, step := range recipe.Steps {
		if strings.TrimSpace(step.Metadata["gc.kind"]) != "drain" {
			continue
		}
		name := strings.TrimSpace(step.Metadata["gc.drain_formula"])
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// ValidateNoReservedUserVars rejects caller-supplied values for graph.v2
// reserved variables before the runtime injects convoy_id.
func ValidateNoReservedUserVars(vars map[string]string) error {
	for key := range vars {
		switch strings.TrimSpace(key) {
		case ConvoyIDVar, "issue", "bead_id":
			return fmt.Errorf("graph.v2 reserved variable %q cannot be supplied by the caller", key)
		}
	}
	return nil
}

// NormalizeInputConvoy returns targetID when it is already a convoy, otherwise
// it creates or reuses a visible synthetic singleton convoy tracking targetID.
func NormalizeInputConvoy(store beads.Store, targetID string) (convoyID string, singleton bool, err error) {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return "", false, fmt.Errorf("graph.v2 target is required")
	}
	target, err := store.Get(targetID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return "", false, fmt.Errorf("graph.v2 target %s not found: %w", targetID, err)
		}
		return "", false, fmt.Errorf("loading graph.v2 target %s: %w", targetID, err)
	}
	if convoycore.IsTerminalStatus(target.Status) {
		return "", false, fmt.Errorf("graph.v2 target %s is %s", target.ID, target.Status)
	}
	if target.Type == "convoy" {
		return target.ID, false, nil
	}
	singletonConvoy, err := EnsureSingletonConvoy(store, target)
	if err != nil {
		return "", false, err
	}
	return singletonConvoy.ID, true, nil
}

// EnsureSingletonConvoy creates or reuses the synthetic one-item convoy for a
// graph.v2 invocation target.
func EnsureSingletonConvoy(store beads.Store, target beads.Bead) (beads.Bead, error) {
	if convoycore.IsTerminalStatus(target.Status) {
		return beads.Bead{}, fmt.Errorf("graph.v2 target %s is %s", target.ID, target.Status)
	}
	key := singletonInvocationPrefix + strings.TrimSpace(target.ID)
	if key == singletonInvocationPrefix {
		return beads.Bead{}, fmt.Errorf("singleton convoy target id is empty")
	}
	unlock := lockKey(key)
	defer unlock()
	existing, err := findExistingSingleton(store, target.ID)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("looking up singleton convoy for %s: %w", target.ID, err)
	}
	if existing.ID != "" {
		if err := ensureTrack(store, existing.ID, target.ID); err != nil {
			return beads.Bead{}, err
		}
		return existing, nil
	}
	metadata := map[string]string{
		syntheticMetadataKey:     "true",
		syntheticKindMetadataKey: singletonSyntheticKind,
		syntheticInputBeadKey:    target.ID,
		graphV2InvocationKey:     key,
	}
	created, err := store.Create(beads.Bead{
		Title:    "input convoy for " + target.ID,
		Type:     "convoy",
		Priority: target.Priority,
		Metadata: metadata,
	})
	if err != nil {
		return beads.Bead{}, fmt.Errorf("creating singleton convoy for %s: %w", target.ID, err)
	}
	if err := convoycore.TrackItem(store, created.ID, target.ID); err != nil {
		return beads.Bead{}, fmt.Errorf("tracking %s from singleton convoy %s: %w", target.ID, created.ID, err)
	}
	return created, nil
}

// PreparePreviewInvocation validates graph.v2 preview inputs without creating
// singleton convoys or workflow roots.
func PreparePreviewInvocation(ctx context.Context, store beads.Store, formulaName string, searchPaths []string, targetID string, userVars map[string]string) (Invocation, error) {
	resolved, parser, err := loadFormulaWithParser(formulaName, searchPaths)
	if err != nil {
		return Invocation{}, fmt.Errorf("loading formula %q: %w", formulaName, err)
	}
	inv := Invocation{
		Formula:     resolved,
		FormulaName: formulaName,
		Vars:        maps.Clone(userVars),
		Targeted:    strings.TrimSpace(targetID) != "",
	}
	if !strings.EqualFold(strings.TrimSpace(resolved.Contract), "graph.v2") {
		return inv, nil
	}
	if err := ValidateNoReservedUserVars(userVars); err != nil {
		return Invocation{}, err
	}
	inv.Vars = EffectiveRuntimeVars(resolved, inv.Vars)
	formulaRequiresTarget, err := formula.GraphV2FormulaReferencesInputConvoyTransitively(resolved, parser)
	if err != nil {
		return Invocation{}, err
	}
	recipe, err := compileValidationRecipe(ctx, formulaName, searchPaths, inv.Vars)
	if err != nil {
		return Invocation{}, err
	}
	recipeRequiresTarget := formula.GraphV2RecipeReferencesInputConvoy(recipe)
	if !formulaRequiresTarget && !recipeRequiresTarget {
		if err := formula.ValidateGraphV2RecipeReservedSymbols(recipe, false); err != nil {
			return Invocation{}, err
		}
		return inv, nil
	}
	if !inv.Targeted {
		if formulaRequiresTarget {
			if err := formula.ValidateGraphV2ReservedSymbolsTransitively(resolved, parser, false); err != nil {
				return Invocation{}, err
			}
		}
		if recipeRequiresTarget {
			if err := formula.ValidateGraphV2RecipeReservedSymbols(recipe, false); err != nil {
				return Invocation{}, err
			}
		}
		return Invocation{}, fmt.Errorf("graph.v2 target is required")
	}
	if err := formula.ValidateGraphV2RecipeReservedSymbols(recipe, true); err != nil {
		return Invocation{}, err
	}
	inputConvoyID, singleton, err := PreviewInputConvoyID(store, targetID)
	if err != nil {
		return Invocation{}, err
	}
	inv.Singleton = singleton
	inv.Targeted = true
	inv.InputConvoy = inputConvoyID
	if inv.Vars == nil {
		inv.Vars = make(map[string]string, 1)
	}
	inv.Vars[ConvoyIDVar] = inputConvoyID
	if err := validateDrainItemFormulas(formulaName, searchPaths, recipe, inv.Vars); err != nil {
		return Invocation{}, err
	}
	return inv, nil
}

// PreviewInputConvoyID returns the read-only input convoy ID a graph.v2 preview
// should use for targetID without creating or repairing singleton state.
func PreviewInputConvoyID(store beads.Store, targetID string) (inputConvoyID string, singleton bool, err error) {
	targetID = strings.TrimSpace(targetID)
	if store == nil {
		return "", false, fmt.Errorf("graph.v2 preview requires a bead store")
	}
	target, err := store.Get(targetID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return "", false, fmt.Errorf("graph.v2 target %s not found: %w", targetID, err)
		}
		return "", false, fmt.Errorf("loading graph.v2 target %s: %w", targetID, err)
	}
	if convoycore.IsTerminalStatus(target.Status) {
		return "", false, fmt.Errorf("graph.v2 target %s is %s", target.ID, target.Status)
	}
	if target.Type == "convoy" {
		return target.ID, false, nil
	}
	existing, err := findExistingSingleton(store, target.ID)
	if err != nil {
		return "", false, fmt.Errorf("looking up singleton convoy for %s: %w", target.ID, err)
	}
	if existing.ID == "" {
		return previewSingletonPrefix + target.ID, true, nil
	}
	hasTrack, err := convoycore.HasTrack(store, existing.ID, target.ID)
	if err != nil {
		return "", false, fmt.Errorf("checking singleton convoy %s track for %s: %w", existing.ID, target.ID, err)
	}
	if !hasTrack {
		return "", false, fmt.Errorf("graph.v2 preview singleton convoy %s is missing track for %s", existing.ID, target.ID)
	}
	return existing.ID, true, nil
}

func findExistingSingleton(store beads.Store, targetID string) (beads.Bead, error) {
	key := singletonInvocationPrefix + strings.TrimSpace(targetID)
	if key == singletonInvocationPrefix {
		return beads.Bead{}, nil
	}
	existing, err := store.ListByMetadata(map[string]string{graphV2InvocationKey: key}, 1)
	if err != nil || len(existing) == 0 {
		return beads.Bead{}, err
	}
	return existing[0], nil
}

// LockKey serializes process-local graph.v2 materialization for a deterministic
// key and returns an unlock function. It is intentionally process-local; store
// level uniqueness remains a future multi-controller requirement.
func LockKey(key string) func() {
	return lockKey(key)
}

func lockKey(key string) func() {
	mu := &keyedLocks[lockStripe(key)]
	mu.Lock()
	return mu.Unlock
}

func lockStripe(key string) uint8 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return uint8(h.Sum32())
}

func ensureTrack(store beads.Store, convoyID, itemID string) error {
	hasTrack, err := convoycore.HasTrack(store, convoyID, itemID)
	if err != nil {
		return fmt.Errorf("checking track dependency %s -> %s: %w", convoyID, itemID, err)
	}
	if hasTrack {
		return nil
	}
	if err := convoycore.TrackItem(store, convoyID, itemID); err != nil {
		return fmt.Errorf("repairing track dependency %s -> %s: %w", convoyID, itemID, err)
	}
	return nil
}

// RootKey returns the stable graph.v2 workflow root key for an input convoy and
// invocation variables.
func RootKey(inputConvoyID, formulaName string, vars map[string]string, scopeKind, scopeRef string) string {
	return "graphv2-root:" + strings.TrimSpace(inputConvoyID) + ":" + strings.TrimSpace(formulaName) + ":" + varsFingerprint(vars) + ":" + dispatchScope(scopeKind, scopeRef)
}

func varsFingerprint(vars map[string]string) string {
	if len(vars) == 0 {
		return "empty"
	}
	keys := make([]string, 0, len(vars))
	for key := range vars {
		if strings.TrimSpace(key) == ConvoyIDVar {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return "empty"
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, key := range keys {
		h.Write([]byte(key))
		h.Write([]byte{0})
		h.Write([]byte(vars[key]))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

func dispatchScope(scopeKind, scopeRef string) string {
	scopeKind = strings.TrimSpace(scopeKind)
	scopeRef = strings.TrimSpace(scopeRef)
	if scopeKind == "" && scopeRef == "" {
		return "default"
	}
	return scopeKind + "=" + scopeRef
}

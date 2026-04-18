// Package sling implements work routing operations for Gas City.
// It provides DoSling and DoSlingBatch for dispatching beads to agents,
// including formula instantiation, graph workflow decoration, and
// convoy auto-creation.
package sling

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// BeadQuerier can retrieve a single bead by ID.
type BeadQuerier interface {
	Get(id string) (beads.Bead, error)
}

// BeadChildQuerier extends BeadQuerier with the ability to query child beads.
type BeadChildQuerier interface {
	BeadQuerier
	List(query beads.ListQuery) ([]beads.Bead, error)
}

// SlingOpts captures the user's intent for a sling operation.
type SlingOpts struct {
	Target        config.Agent
	BeadOrFormula string
	IsFormula     bool
	OnFormula     string
	NoFormula     bool
	SkipPoke      bool
	Title         string
	Vars          []string
	Merge         string // "", "direct", "mr", "local"
	NoConvoy      bool
	Owned         bool
	Nudge         bool
	Force         bool
	DryRun        bool
	ScopeKind     string
	ScopeRef      string
}

// AgentResolver resolves an agent name to a config.Agent.
type AgentResolver interface {
	ResolveAgent(cfg *config.City, name, rigContext string) (config.Agent, bool)
}

// BranchResolver resolves the default branch for a directory.
type BranchResolver interface {
	DefaultBranch(dir string) string
}

// Notifier sends wake notifications to the controller and control
// dispatcher. Methods are best-effort.
type Notifier interface {
	PokeController(cityPath string)
	PokeControlDispatch(cityPath string)
}

// BeadRouter routes a bead to an agent using typed structured data.
// Replaces the shell-string SlingRunner for callers using the intent API.
type BeadRouter interface {
	Route(ctx context.Context, req RouteRequest) error
}

// RouteRequest describes a bead routing operation in typed terms.
type RouteRequest struct {
	BeadID   string
	Target   string            // qualified agent name
	Metadata map[string]string // gc.routed_to, pool label, etc.
	WorkDir  string            // rig directory for command execution
	Env      map[string]string // extra env vars (GC_SLING_TARGET, etc.)
}

// SlingDeps bundles infrastructure dependencies for sling operations.
type SlingDeps struct {
	CityName string
	CityPath string
	Cfg      *config.City
	SP       runtime.Provider
	Runner   SlingRunner
	Store    beads.Store
	StoreRef string

	// Narrow interfaces (matches established internal package patterns).
	Resolver AgentResolver  // agent name resolution
	Branches BranchResolver // git default branch lookup (nil = skip)
	Notify   Notifier       // controller/dispatcher wake (nil = skip)
	Router   BeadRouter     // typed bead routing (nil = use Runner)
	// DirectSessionResolver optionally materializes direct graph assignee
	// targets to concrete session bead IDs.
	DirectSessionResolver func(store beads.Store, cityName, cityPath string, cfg *config.City, target, rigContext string) (string, bool, error)
}

// SlingResult holds the structured output of a sling operation.
// Contains only data fields -- callers format display strings.
type SlingResult struct {
	BeadID      string // the routed bead ID (or wisp root for formula)
	Target      string // qualified agent name
	Method      string // "bead", "formula", "on-formula", "default-on-formula"
	WorkflowID  string // non-empty for graph workflow launches
	ConvoyID    string // non-empty if auto-convoy was created
	WispRootID  string // non-empty for on-formula/default-formula attachment
	FormulaName string // formula used (for display)
	Idempotent  bool   // true if bead was already routed (skipped)
	DryRun      bool   // true if this was a dry-run (no mutations)

	// Structured warnings (callers decide how to display).
	AgentSuspended bool     // target agent is suspended
	PoolEmpty      bool     // pool max=0
	AutoBurned     []string // IDs of auto-burned stale molecules
	MetadataErrors []string // non-fatal metadata write failures
	BeadWarnings   []string // pre-flight bead state warnings

	// Batch fields (populated by DoSlingBatch).
	ContainerType string // "convoy", "epic", etc. (batch only)
	Routed        int
	Failed        int
	Skipped       int // total skipped (idempotent + non-open)
	IdempotentCt  int // how many were skipped due to idempotency
	Total         int
	NudgeAgent    *config.Agent // non-nil if caller should nudge

	// Per-child results for batch operations.
	Children []SlingChildResult
}

// SlingChildResult holds the outcome for a single child in batch sling.
type SlingChildResult struct {
	BeadID      string
	Status      string // bead status (for skipped non-open children)
	Routed      bool
	Skipped     bool // idempotent or non-open
	Failed      bool
	FailReason  string
	WispRootID  string // if formula attached
	FormulaName string // formula used
}

// Sling provides intent-based work routing operations. Construct via New.
type Sling struct {
	deps SlingDeps
}

// New creates a Sling instance after validating required deps.
func New(deps SlingDeps) (*Sling, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	return &Sling{deps: deps}, nil
}

// RouteOpts holds options for plain bead routing.
type RouteOpts struct {
	Merge    string // "", "direct", "mr", "local"
	NoConvoy bool
	Owned    bool
	Nudge    bool
	Force    bool
	DryRun   bool
	SkipPoke bool
}

// FormulaOpts holds options for formula-based operations.
type FormulaOpts struct {
	Title     string
	Vars      []string
	Merge     string
	Nudge     bool
	Force     bool
	DryRun    bool
	SkipPoke  bool
	ScopeKind string
	ScopeRef  string
}

// RouteBead routes a plain bead to an agent.
func (s *Sling) RouteBead(_ context.Context, beadID string, target config.Agent, opts RouteOpts) (SlingResult, error) {
	return DoSling(SlingOpts{
		Target:        target,
		BeadOrFormula: beadID,
		Merge:         opts.Merge,
		NoConvoy:      opts.NoConvoy,
		Owned:         opts.Owned,
		Nudge:         opts.Nudge,
		Force:         opts.Force,
		SkipPoke:      opts.SkipPoke,
		DryRun:        opts.DryRun,
	}, s.deps, s.deps.Store)
}

// LaunchFormula instantiates a formula and routes the resulting wisp.
func (s *Sling) LaunchFormula(_ context.Context, formulaName string, target config.Agent, opts FormulaOpts) (SlingResult, error) {
	return DoSling(SlingOpts{
		Target:        target,
		BeadOrFormula: formulaName,
		IsFormula:     true,
		Title:         opts.Title,
		Vars:          opts.Vars,
		Merge:         opts.Merge,
		Nudge:         opts.Nudge,
		Force:         opts.Force,
		SkipPoke:      opts.SkipPoke,
		DryRun:        opts.DryRun,
		ScopeKind:     opts.ScopeKind,
		ScopeRef:      opts.ScopeRef,
	}, s.deps, s.deps.Store)
}

// AttachFormula attaches a formula wisp to an existing bead and routes the bead.
func (s *Sling) AttachFormula(_ context.Context, formulaName, beadID string, target config.Agent, opts FormulaOpts) (SlingResult, error) {
	return DoSling(SlingOpts{
		Target:        target,
		BeadOrFormula: beadID,
		OnFormula:     formulaName,
		Title:         opts.Title,
		Vars:          opts.Vars,
		Merge:         opts.Merge,
		Nudge:         opts.Nudge,
		Force:         opts.Force,
		SkipPoke:      opts.SkipPoke,
		DryRun:        opts.DryRun,
		ScopeKind:     opts.ScopeKind,
		ScopeRef:      opts.ScopeRef,
	}, s.deps, s.deps.Store)
}

// ExpandConvoy expands a convoy and routes each open child.
func (s *Sling) ExpandConvoy(_ context.Context, convoyID string, target config.Agent, opts RouteOpts, querier BeadChildQuerier) (SlingResult, error) {
	return DoSlingBatch(SlingOpts{
		Target:        target,
		BeadOrFormula: convoyID,
		Merge:         opts.Merge,
		NoConvoy:      opts.NoConvoy,
		Owned:         opts.Owned,
		Nudge:         opts.Nudge,
		Force:         opts.Force,
		SkipPoke:      opts.SkipPoke,
		DryRun:        opts.DryRun,
	}, s.deps, querier)
}

// ScaleInfo holds pool scaling parameters for an agent.
type ScaleInfo struct {
	Min int
	Max int
}

// SlingRunner executes a shell command in the given directory with optional
// extra env vars and returns combined output.
type SlingRunner func(dir, command string, env map[string]string) (string, error)

// SlingTracef writes to the sling trace log if GC_SLING_TRACE is set.
func SlingTracef(format string, args ...any) {
	path := strings.TrimSpace(os.Getenv("GC_SLING_TRACE"))
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()                                                                                    //nolint:errcheck // best-effort trace log
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...)) //nolint:errcheck
}

// FindRigByPrefix finds a rig whose effective prefix matches (case-insensitive).
func FindRigByPrefix(cfg *config.City, prefix string) (config.Rig, bool) {
	lp := strings.ToLower(prefix)
	for _, r := range cfg.Rigs {
		if strings.ToLower(r.EffectivePrefix()) == lp {
			return r, true
		}
	}
	return config.Rig{}, false
}

// RigDirForBead resolves the rig directory for a bead ID by extracting
// the bead prefix and looking up the rig path.
func RigDirForBead(cfg *config.City, beadID string) string {
	bp := BeadPrefix(beadID)
	if bp == "" {
		return ""
	}
	if rig, ok := FindRigByPrefix(cfg, bp); ok {
		return rig.Path
	}
	return ""
}

// RigDirForAgent returns the rig directory for an agent by matching its Dir
// field to a rig Name.
func RigDirForAgent(cfg *config.City, a config.Agent) string {
	if a.Dir == "" {
		return ""
	}
	for _, r := range cfg.Rigs {
		if r.Name == a.Dir {
			return r.Path
		}
	}
	return ""
}

// SlingDirForBead returns the directory for sling command execution.
func SlingDirForBead(cfg *config.City, cityPath, beadID string) string {
	if dir := RigDirForBead(cfg, beadID); dir != "" {
		return dir
	}
	return cityPath
}

// BuildSlingCommand replaces {} in the sling query template with the bead ID.
// The bead ID is shell-quoted to prevent command injection.
func BuildSlingCommand(template, beadID string) string {
	return strings.ReplaceAll(template, "{}", shellquote.Quote(beadID))
}

// FormatBeadLabel formats a bead ID with optional title for display.
func FormatBeadLabel(id, title string) string {
	if title != "" {
		return id + " — " + fmt.Sprintf("%q", title)
	}
	return id
}

// BeadPrefix extracts the rig prefix from a bead ID by taking the lowercase
// letters before the first dash. "HW-7" → "hw", "FE-123" → "fe".
// Returns "" if the ID has no dash (can't determine prefix).
func BeadPrefix(beadID string) string {
	i := strings.Index(beadID, "-")
	if i <= 0 {
		return ""
	}
	return strings.ToLower(beadID[:i])
}

// RigPrefixForAgent returns the rig prefix that an agent's rig uses for bead IDs.
func RigPrefixForAgent(a config.Agent, cfg *config.City) string {
	if a.Dir == "" || cfg == nil {
		return ""
	}
	for _, r := range cfg.Rigs {
		if r.Name == a.Dir {
			return r.EffectivePrefix()
		}
	}
	return ""
}

// CheckCrossRig returns a warning message if a rig-scoped agent receives
// a bead from a different rig. Returns "" if routing is safe.
func CheckCrossRig(beadID string, a config.Agent, cfg *config.City) string {
	if cfg == nil || a.Dir == "" {
		return ""
	}
	bp := BeadPrefix(beadID)
	if bp == "" {
		return ""
	}
	rp := RigPrefixForAgent(a, cfg)
	if rp == "" {
		return ""
	}
	if strings.EqualFold(bp, rp) {
		return ""
	}
	return fmt.Sprintf("cross-rig routing — bead %s (prefix %q) → agent %s (rig prefix %q)", beadID, bp, a.QualifiedName(), rp)
}

// BeadExistsInStore checks if a bead exists in the given store.
func BeadExistsInStore(store beads.Store, id string) bool {
	if store == nil {
		return false
	}
	_, err := store.Get(id)
	return err == nil
}

// LooksLikeBeadID reports whether a string looks like a bead ID.
func LooksLikeBeadID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// Bead IDs are typically prefix-NNN or just NNN.
	// They don't contain spaces, slashes, or common text punctuation.
	if strings.ContainsAny(s, " \t\n/\\") {
		return false
	}
	// If it contains a digit, it's likely a bead ID.
	for _, c := range s {
		if c >= '0' && c <= '9' {
			return true
		}
	}
	return false
}

// IsCustomSlingQuery reports whether the agent has a custom sling_query
// (not the default bd-based one).
func IsCustomSlingQuery(a config.Agent) bool {
	q := strings.TrimSpace(a.EffectiveSlingQuery())
	return q != "" && !strings.HasPrefix(q, "bd ")
}

// BeadPriorityOverride reads the priority from an existing bead for use
// as a priority override when creating child beads.
func BeadPriorityOverride(store BeadQuerier, beadID string) *int {
	if store == nil || beadID == "" {
		return nil
	}
	bead, err := store.Get(beadID)
	if err != nil {
		return nil
	}
	return ClonePriorityPtr(bead.Priority)
}

// ClonePriorityPtr returns a copy of an *int, or nil if nil.
func ClonePriorityPtr(v *int) *int {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

// BeadMetadataTarget walks the bead's parent chain looking for a "target"
// metadata value (used for branch targeting).
func BeadMetadataTarget(store beads.Store, beadID string) string {
	if store == nil || beadID == "" {
		return ""
	}

	seen := make(map[string]struct{}, 8)
	rootID := beadID
	for beadID != "" {
		if _, ok := seen[beadID]; ok {
			return ""
		}
		seen[beadID] = struct{}{}

		b, err := store.Get(beadID)
		if err != nil {
			return ""
		}
		if target := strings.TrimSpace(b.Metadata["target"]); target != "" {
			if beadID == rootID || b.Type == "convoy" {
				return target
			}
		}
		beadID = strings.TrimSpace(b.ParentID)
	}
	return ""
}

// SlingFormulaSearchPaths returns the formula search paths for the current
// sling context.
func SlingFormulaSearchPaths(deps SlingDeps, a config.Agent) []string {
	if deps.Cfg == nil {
		return nil
	}
	return deps.Cfg.FormulaLayers.SearchPaths(a.Dir)
}

// SlingFormulaUsesBaseBranch reports whether the formula conventionally
// uses a base_branch variable.
func SlingFormulaUsesBaseBranch(formulaName string) bool {
	return strings.HasPrefix(formulaName, "mol-polecat-") || formulaName == "mol-scoped-work"
}

// SlingFormulaUsesTargetBranch reports whether the formula conventionally
// uses a target_branch variable.
func SlingFormulaUsesTargetBranch(formulaName string) bool {
	return formulaName == "mol-refinery-patrol"
}

// SlingFormulaRepoDir returns the best repo directory for formula variable
// resolution.
func SlingFormulaRepoDir(beadID string, deps SlingDeps, a config.Agent) string {
	if deps.Cfg != nil {
		if dir := RigDirForBead(deps.Cfg, beadID); dir != "" {
			return dir
		}
		if dir := RigDirForAgent(deps.Cfg, a); dir != "" {
			return dir
		}
	}
	return deps.CityPath
}

// SlingFormulaTargetBranch resolves the target branch for formula variables.
func SlingFormulaTargetBranch(beadID string, deps SlingDeps, a config.Agent) string {
	if target := BeadMetadataTarget(deps.Store, beadID); target != "" {
		return target
	}
	if deps.Branches != nil {
		return deps.Branches.DefaultBranch(SlingFormulaRepoDir(beadID, deps, a))
	}
	return ""
}

// BuildSlingFormulaVars builds the variable map for formula instantiation.
func BuildSlingFormulaVars(formulaName, beadID string, userVars []string, a config.Agent, deps SlingDeps) map[string]string {
	vars := make(map[string]string, len(userVars)+3)
	for _, v := range userVars {
		key, value, ok := strings.Cut(v, "=")
		if ok && key != "" {
			vars[key] = value
		}
	}
	addVar := func(key, value string) {
		if value == "" {
			return
		}
		if _, explicit := vars[key]; explicit {
			return
		}
		vars[key] = value
	}

	if beadID != "" {
		addVar("issue", beadID)
	}

	autoBranch := SlingFormulaTargetBranch(beadID, deps, a)
	if SlingFormulaUsesBaseBranch(formulaName) {
		addVar("base_branch", autoBranch)
	}
	if SlingFormulaUsesTargetBranch(formulaName) {
		addVar("target_branch", autoBranch)
	}

	return vars
}

// ResolveSlingEnv returns extra env vars for the sling command.
func ResolveSlingEnv(a config.Agent, deps SlingDeps) map[string]string {
	if agentutil.IsMultiSessionAgent(&a) {
		return nil
	}
	sn := agentutil.LookupSessionName(deps.Store, deps.CityName, a.QualifiedName(), deps.Cfg.Workspace.SessionTemplate)
	return map[string]string{"GC_SLING_TARGET": sn}
}

// TargetType returns a human-readable label for the agent type.
func TargetType(a *config.Agent) string {
	if a == nil {
		return "unknown"
	}
	if a.SupportsInstanceExpansion() {
		return "pool"
	}
	return "agent"
}

// WorkflowStoreRefForDir maps a store directory to a "city:<name>" or
// "rig:<name>" store ref string.
func WorkflowStoreRefForDir(storeDir, cityPath, cityName string, cfg *config.City) string {
	if strings.TrimSpace(storeDir) == "" || strings.TrimSpace(cityPath) == "" {
		return ""
	}
	storeDir = NormalizePathForCompare(storeDir)
	cityPath = NormalizePathForCompare(cityPath)
	if storeDir == cityPath {
		cityName = strings.TrimSpace(cityName)
		if cityName == "" {
			cityName = "city"
		}
		return "city:" + cityName
	}
	if cfg == nil {
		return ""
	}
	for _, rig := range cfg.Rigs {
		rigPath := rig.Path
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		if SamePath(rigPath, storeDir) {
			return "rig:" + rig.Name
		}
	}
	return ""
}

// IsGraphWorkflowAttachment checks whether a bead is a graph.v2 workflow root.
func IsGraphWorkflowAttachment(store beads.Store, rootID string) bool {
	if store == nil || rootID == "" {
		return false
	}
	b, err := store.Get(rootID)
	if err != nil {
		return false
	}
	return b.Metadata["gc.kind"] == "workflow" && b.Metadata["gc.formula_contract"] == "graph.v2"
}

// InstantiateSlingFormula compiles and instantiates a formula, applying
// graph routing if the formula is a graph.v2 workflow.
func InstantiateSlingFormula(ctx context.Context, formulaName string, searchPaths []string, opts molecule.Options, sourceBeadID, scopeKind, scopeRef string, a config.Agent, deps SlingDeps) (*molecule.Result, error) {
	SlingTracef("instantiate start formula=%s source=%s agent=%s parent=%s", formulaName, sourceBeadID, a.QualifiedName(), opts.ParentID)
	if opts.PriorityOverride == nil && sourceBeadID != "" {
		opts.PriorityOverride = BeadPriorityOverride(deps.Store, sourceBeadID)
	}
	compileStart := time.Now()
	recipe, err := formula.Compile(ctx, formulaName, searchPaths, opts.Vars)
	if err != nil {
		SlingTracef("instantiate compile-error formula=%s dur=%s err=%v", formulaName, time.Since(compileStart), err)
		return nil, err
	}
	SlingTracef("instantiate compiled formula=%s dur=%s steps=%d", formulaName, time.Since(compileStart), len(recipe.Steps))
	if err := ApplyGraphRouting(recipe, &a, a.QualifiedName(), opts.Vars, sourceBeadID, scopeKind, scopeRef, deps.StoreRef, deps.Store, deps.CityName, deps.Cfg, deps); err != nil {
		SlingTracef("instantiate decorate-error formula=%s err=%v", formulaName, err)
		return nil, err
	}
	instantiateStart := time.Now()
	result, err := molecule.Instantiate(ctx, deps.Store, recipe, opts)
	if err != nil {
		SlingTracef("instantiate molecule-error formula=%s dur=%s err=%v", formulaName, time.Since(instantiateStart), err)
		return nil, err
	}
	SlingTracef("instantiate done formula=%s dur=%s root=%s created=%d graph=%t", formulaName, time.Since(instantiateStart), result.RootID, result.Created, result.GraphWorkflow)
	return result, nil
}

// ShouldPromoteWorkflowLaunchStatus reports whether a bead's status should
// be promoted to in_progress when a workflow launches.
func ShouldPromoteWorkflowLaunchStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "open", "ready", "todo", "triage", "backlog":
		return true
	default:
		return false
	}
}

// PromoteWorkflowLaunchBead sets a bead to in_progress if its current status
// is eligible for promotion.
func PromoteWorkflowLaunchBead(store beads.Store, beadID string) error {
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return nil
	}
	bead, err := store.Get(beadID)
	if err != nil {
		return err
	}
	if !ShouldPromoteWorkflowLaunchStatus(bead.Status) {
		return nil
	}
	status := "in_progress"
	return store.Update(beadID, beads.UpdateOpts{Status: &status})
}

// BeadCheckResult holds the result of pre-flight bead state checks.
type BeadCheckResult struct {
	Idempotent bool
	Warnings   []string
}

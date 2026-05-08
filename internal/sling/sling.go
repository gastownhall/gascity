// Package sling implements work routing operations for Gas City.
// It provides DoSling and DoSlingBatch for dispatching beads to agents,
// including formula instantiation, graph workflow decoration, and
// convoy auto-creation.
package sling

import (
	"context"
	"errors"
	"fmt"
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
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
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
	// InlineText is set only by the CLI path for ad-hoc task text. API
	// callers always provide explicit bead or formula references.
	InlineText bool
	ScopeKind  string
	ScopeRef   string
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

// SourceWorkflowStore is one entry from the list of bead stores the sling
// layer should consult when enforcing source-workflow singleton invariants.
// StoreRef identifies the store scope (e.g. "city:foo" / "rig:alpha").
type SourceWorkflowStore struct {
	Store    beads.Store
	StoreRef string
}

// RouteRequest describes a bead routing operation in typed terms.
type RouteRequest struct {
	BeadID   string
	Target   string            // qualified agent name
	Metadata map[string]string // gc.routed_to, pool label, etc.
	WorkDir  string            // rig directory for command execution
	Env      map[string]string // extra env vars (GC_SLING_TARGET, etc.)
	Force    bool              // allow best-effort routing when the bead is absent
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
	// ValidationQuerier overrides Store for existence checks when a caller has
	// already resolved the bead through a narrower view.
	ValidationQuerier BeadQuerier
	// SourceWorkflowStores lists every bead store that may contain workflow
	// roots for source-workflow singleton checks and recovery.
	SourceWorkflowStores func() ([]SourceWorkflowStore, error)
	Tracer               func(format string, args ...any)

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
	WorkflowID  string // if graph workflow attached
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
	// InlineText is set only by the CLI path for ad-hoc task text. API
	// callers always provide explicit bead or formula references.
	InlineText bool
	SkipPoke   bool
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
		InlineText:    opts.InlineText,
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
		InlineText:    opts.InlineText,
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

// SlingTracef calls the package-level trace function if set. Wire via
// SetTracer at process startup; the domain package never opens files or
// reads environment variables directly.
func SlingTracef(format string, args ...any) {
	if globalTracer != nil {
		globalTracer(format, args...)
	}
}

var globalTracer func(format string, args ...any)

// SetTracer installs the package-level trace function. Call once at
// process startup from the CLI edge.
func SetTracer(fn func(format string, args ...any)) {
	globalTracer = fn
}

// FindRigByPrefix finds a rig whose effective prefix matches (case-insensitive).
func FindRigByPrefix(cfg *config.City, prefix string) (config.Rig, bool) {
	if cfg == nil {
		return config.Rig{}, false
	}
	lp := strings.ToLower(prefix)
	for _, r := range cfg.Rigs {
		if strings.ToLower(r.EffectivePrefix()) == lp {
			return r, true
		}
	}
	return config.Rig{}, false
}

// IsHQPrefix reports whether prefix matches the city's HQ bead prefix.
func IsHQPrefix(cfg *config.City, prefix string) bool {
	prefix = strings.TrimSpace(prefix)
	return cfg != nil && prefix != "" && strings.EqualFold(prefix, config.EffectiveHQPrefix(cfg))
}

// RigDirForBead resolves the rig directory for a bead ID by extracting
// the bead prefix and looking up the rig path. Honors hyphenated rig
// prefixes via BeadPrefixForCity.
func RigDirForBead(cfg *config.City, beadID string) string {
	bp := BeadPrefixForCity(cfg, beadID)
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

// BuildSlingCommandForAgent expands any PathContext placeholders in a custom
// sling_query, then replaces {} with the bead ID. Malformed templates fall back
// to the raw sling_query so routing behavior remains non-fatal. The returned
// warning is non-empty when template expansion failed and the raw template was
// used as fallback.
func BuildSlingCommandForAgent(fieldName, template, beadID, cityPath, cityName string, a config.Agent, rigs []config.Rig) (command, warning string) {
	if strings.Contains(template, "{{") {
		expanded, err := workdirutil.ExpandCommandTemplate(template, cityPath, cityName, a, rigs)
		if err != nil {
			if fieldName == "" {
				fieldName = "sling_query"
			}
			warning = fmt.Sprintf("BuildSlingCommandForAgent: agent %q field %q: %v (using raw command)", a.QualifiedName(), fieldName, err)
		} else {
			template = expanded
		}
	}
	return BuildSlingCommand(template, beadID), warning
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
//
// This is a config-free heuristic. For inputs whose rig prefix may itself
// contain hyphens ("agent-diagnostics-hnn" routed to rig "agent-diagnostics"),
// callers must use BeadPrefixForCity, which resolves the longest matching
// configured prefix.
func BeadPrefix(beadID string) string {
	i := strings.Index(beadID, "-")
	if i <= 0 {
		return ""
	}
	return strings.ToLower(beadID[:i])
}

// BeadPrefixForCity returns the configured rig (or HQ) prefix that beadID
// belongs to, preferring the longest match so hyphenated rig prefixes resolve
// correctly. It does not require the suffix to pass the short bead-ID shape
// gate; callers that need to decide bead ID vs inline text should use
// LooksLikeConfiguredBeadID. Falls back to BeadPrefix when no configured
// prefix matches. Returns "" if the bead has no dash and no configured-prefix
// match.
func BeadPrefixForCity(cfg *config.City, beadID string) string {
	if p := matchConfiguredBeadPrefixCandidate(cfg, beadID); p != "" {
		return p
	}
	return BeadPrefix(beadID)
}

// LooksLikeConfiguredBeadID reports whether s parses as a bead ID whose
// prefix matches the city's HQ prefix or any configured rig's effective
// prefix. Unlike BeadIDParts, it accepts hyphenated rig prefixes
// (e.g. "agent-diagnostics-hnn" with rig "agent-diagnostics"). The
// trailing suffix must be alphanumeric (allowing an optional ".child"
// hierarchical part) and at most 8 characters long.
func LooksLikeConfiguredBeadID(cfg *config.City, s string) bool {
	return matchConfiguredBeadPrefix(cfg, s) != ""
}

// matchConfiguredBeadPrefix returns the longest configured prefix
// (HQ or rig) that beadID begins with, provided the trailing suffix
// passes the bead-suffix shape gate. Match is case-insensitive on the
// prefix; the returned value is the lower-cased configured prefix.
// Returns "" if no configured prefix matches.
func matchConfiguredBeadPrefix(cfg *config.City, beadID string) string {
	return matchConfiguredBeadPrefixBySuffix(cfg, beadID, true)
}

func matchConfiguredBeadPrefixCandidate(cfg *config.City, beadID string) string {
	return matchConfiguredBeadPrefixBySuffix(cfg, beadID, false)
}

func matchConfiguredBeadPrefixBySuffix(cfg *config.City, beadID string, requireValidSuffix bool) string {
	beadID = strings.TrimSpace(beadID)
	if cfg == nil || beadID == "" || strings.ContainsAny(beadID, " \t\n") {
		return ""
	}
	candidates := configuredBeadPrefixes(cfg)
	// Track the longest matching prefix; equal-length ties keep the first
	// match, matching the order semantics of FindRigByPrefix.
	best := ""
	bestLen := 0
	lower := strings.ToLower(beadID)
	for _, p := range candidates {
		lp := strings.ToLower(p)
		if len(lp) <= bestLen {
			continue
		}
		if !strings.HasPrefix(lower, lp+"-") {
			continue
		}
		if requireValidSuffix {
			suffix := beadID[len(lp)+1:]
			if !validBeadSuffix(suffix) {
				continue
			}
		}
		best = lp
		bestLen = len(lp)
	}
	return best
}

// configuredBeadPrefixes returns every prefix the city accepts for bead
// IDs: the city's HQ prefix plus each rig's effective prefix. Empty
// entries are skipped. The caller picks the longest match; order only
// matters when equal-length matches tie, in which case the first match
// (HQ before rigs, then cfg.Rigs declaration order) is kept. Note that
// config validation rejects duplicate prefixes, so ties should not
// appear in valid configs.
func configuredBeadPrefixes(cfg *config.City) []string {
	if cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Rigs)+1)
	if hq := config.EffectiveHQPrefix(cfg); hq != "" {
		out = append(out, hq)
	}
	for i := range cfg.Rigs {
		if p := cfg.Rigs[i].EffectivePrefix(); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validBeadSuffix reports whether suffix is a plausible bead-ID suffix:
// a non-empty alphanumeric base of at most 8 characters, optionally
// followed by ".child" hierarchical parts. The hierarchical portion is
// not validated, matching BeadIDParts which truncates at the first dot
// before validating the base.
func validBeadSuffix(suffix string) bool {
	base := suffix
	if dot := strings.IndexByte(suffix, '.'); dot > 0 {
		base = suffix[:dot]
	}
	if base == "" || len(base) > 8 {
		return false
	}
	for _, c := range base {
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
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
	err := CrossRigRouteError(beadID, a, cfg)
	if err == nil {
		return ""
	}
	return err.Error()
}

// CrossRigError reports that a rig-scoped agent was asked to route a bead from
// a different rig.
type CrossRigError struct {
	BeadID     string
	BeadPrefix string
	Target     string
	RigPrefix  string
}

// Error returns the cross-rig routing diagnostic.
func (e *CrossRigError) Error() string {
	return fmt.Sprintf("cross-rig routing — bead %s (prefix %q) → agent %s (rig prefix %q)", e.BeadID, e.BeadPrefix, e.Target, e.RigPrefix)
}

// CrossRigRouteError returns a typed cross-rig error when routing is unsafe.
func CrossRigRouteError(beadID string, a config.Agent, cfg *config.City) *CrossRigError {
	if cfg == nil || a.Dir == "" {
		return nil
	}
	bp := BeadPrefixForCity(cfg, beadID)
	if bp == "" {
		return nil
	}
	rp := RigPrefixForAgent(a, cfg)
	if rp == "" {
		return nil
	}
	if strings.EqualFold(bp, rp) {
		return nil
	}
	return &CrossRigError{
		BeadID:     beadID,
		BeadPrefix: bp,
		Target:     a.QualifiedName(),
		RigPrefix:  rp,
	}
}

// ProbeBeadInStore checks if a bead exists in the given store and surfaces
// non-not-found lookup errors.
func ProbeBeadInStore(store beads.Store, id string) (bool, error) {
	return probeBeadInQuerier(store, id)
}

func probeBeadInQuerier(querier BeadQuerier, id string) (bool, error) {
	if querier == nil {
		return false, fmt.Errorf("store unavailable")
	}
	_, err := querier.Get(id)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, beads.ErrNotFound) {
		return false, nil
	}
	return false, err
}

// LooksLikeBeadID reports whether a string loosely resembles a bead ID.
//
// Deprecated: use BeadIDParts for the stricter routing heuristic.
func LooksLikeBeadID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.ContainsAny(s, " \t\n/\\") {
		return false
	}
	for _, c := range s {
		if c >= '0' && c <= '9' {
			return true
		}
	}
	return false
}

// BeadIDParts trims surrounding whitespace and parses a bead-like string into
// prefix and base suffix, ignoring any hierarchical ".child" suffix. It
// validates the structured bead-ID shape used by the CLI's stricter routing
// heuristic.
func BeadIDParts(s string) (prefix, baseSuffix string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\n") {
		return "", "", false
	}
	i := strings.Index(s, "-")
	if i <= 0 || i == len(s)-1 || strings.Count(s, "-") != 1 {
		return "", "", false
	}
	prefix = s[:i]
	for idx, c := range prefix {
		if idx == 0 {
			if ('A' > c || c > 'Z') && ('a' > c || c > 'z') {
				return "", "", false
			}
			continue
		}
		if ('0' > c || c > '9') && ('a' > c || c > 'z') && ('A' > c || c > 'Z') {
			return "", "", false
		}
	}
	suffix := s[i+1:]
	baseSuffix = suffix
	if dot := strings.IndexByte(suffix, '.'); dot > 0 {
		baseSuffix = suffix[:dot]
	}
	if baseSuffix == "" {
		return "", "", false
	}
	for _, c := range baseSuffix {
		if ('0' > c || c > '9') && ('a' > c || c > 'z') && ('A' > c || c > 'Z') {
			return "", "", false
		}
	}
	return prefix, baseSuffix, true
}

// MissingBeadError reports that a requested bead reference did not resolve in
// the target store.
type MissingBeadError struct {
	BeadID   string
	StoreRef string
}

// Error returns the missing-bead diagnostic.
func (e *MissingBeadError) Error() string {
	return fmt.Sprintf("bead %q not found in store %s", e.BeadID, e.StoreRef)
}

// BeadLookupError reports an operational failure while checking whether a bead
// exists in the target store.
type BeadLookupError struct {
	BeadID   string
	StoreRef string
	Err      error
}

// Error returns the lookup-failure diagnostic.
func (e *BeadLookupError) Error() string {
	return fmt.Sprintf("getting bead %q from store %s: %v", e.BeadID, e.StoreRef, e.Err)
}

// Unwrap returns the underlying lookup failure.
func (e *BeadLookupError) Unwrap() error {
	return e.Err
}

func normalizeSlingQuery(query string) string {
	return strings.Join(strings.Fields(query), " ")
}

// IsCustomSlingQuery reports whether the agent has a user-defined sling_query
// whose behavior differs from the built-in metadata-stamping default. Explicit
// pins of the documented default command retain default routing semantics;
// bd-based queries with extra side effects still count as custom.
func IsCustomSlingQuery(a config.Agent) bool {
	q := strings.TrimSpace(a.SlingQuery)
	if q == "" {
		return false
	}
	return normalizeSlingQuery(q) != normalizeSlingQuery(a.DefaultSlingQuery())
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
	vars := make(map[string]string, len(userVars)+6)
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
	addRoutingVar := func(key, value string) {
		if _, explicit := vars[key]; explicit {
			return
		}
		vars[key] = value
	}

	if beadID != "" {
		addVar("issue", beadID)
	}
	addRoutingVar("rig_name", a.Dir)
	addRoutingVar("binding_name", a.BindingName)
	addRoutingVar("binding_prefix", a.BindingPrefix())

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
	recipe, err := formula.CompileWithoutRuntimeVarValidation(ctx, formulaName, searchPaths, opts.Vars)
	if err != nil {
		SlingTracef("instantiate compile-error formula=%s dur=%s err=%v", formulaName, time.Since(compileStart), err)
		return nil, err
	}
	if err := molecule.ValidateRecipeRuntimeVars(recipe, opts); err != nil {
		SlingTracef("instantiate validate-error formula=%s err=%v", formulaName, err)
		return nil, err
	}
	SlingTracef("instantiate compiled formula=%s dur=%s steps=%d", formulaName, time.Since(compileStart), len(recipe.Steps))
	if err := ApplyGraphRouting(recipe, &a, a.QualifiedName(), opts.Vars, sourceBeadID, scopeKind, scopeRef, deps.StoreRef, deps.Store, deps.CityName, deps.Cfg, deps); err != nil {
		SlingTracef("instantiate decorate-error formula=%s err=%v", formulaName, err)
		return nil, err
	}
	privatizeAttachedRootOnlyWisp(recipe, sourceBeadID)
	instantiateStart := time.Now()
	result, err := molecule.Instantiate(ctx, deps.Store, recipe, opts)
	if err != nil {
		SlingTracef("instantiate molecule-error formula=%s dur=%s err=%v", formulaName, time.Since(instantiateStart), err)
		return nil, err
	}
	SlingTracef("instantiate done formula=%s dur=%s root=%s created=%d graph=%t", formulaName, time.Since(instantiateStart), result.RootID, result.Created, result.GraphWorkflow)
	return result, nil
}

func privatizeAttachedRootOnlyWisp(recipe *formula.Recipe, sourceBeadID string) {
	if recipe == nil || !recipe.RootOnly || strings.TrimSpace(sourceBeadID) == "" || len(recipe.Steps) == 0 {
		return
	}
	root := &recipe.Steps[0]
	if root.Metadata["gc.kind"] != "wisp" {
		return
	}
	root.Type = "molecule"
	root.Metadata = mapsCloneWithout(root.Metadata, "gc.kind")
}

func mapsCloneWithout(in map[string]string, drop string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		if key == drop {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

package session

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const (
	// NamedSessionMetadataKey records that a bead belongs to a configured named session.
	NamedSessionMetadataKey = "configured_named_session"
	// NamedSessionIdentityMetadata records the configured named session identity on a bead.
	NamedSessionIdentityMetadata = "configured_named_identity"
	// NamedSessionModeMetadata records the configured named session mode on a bead.
	NamedSessionModeMetadata = "configured_named_mode"
)

// NamedSessionSpec is the resolved runtime view of a configured named session.
type NamedSessionSpec struct {
	Named       *config.NamedSession
	Agent       *config.Agent
	Identity    string
	SessionName string
	Mode        string
}

// NormalizeNamedSessionTarget trims whitespace and trailing separators from a named session target.
func NormalizeNamedSessionTarget(target string) string {
	target = strings.TrimSpace(target)
	target = strings.TrimSuffix(target, "/")
	return target
}

// TargetBasename returns the unqualified name portion of a session target.
func TargetBasename(target string) string {
	target = NormalizeNamedSessionTarget(target)
	if i := strings.LastIndex(target, "/"); i >= 0 {
		return target[i+1:]
	}
	return target
}

// FindNamedSessionSpec resolves a fully qualified named session identity.
func FindNamedSessionSpec(cfg *config.City, cityName, identity string) (NamedSessionSpec, bool) {
	identity = NormalizeNamedSessionTarget(identity)
	if cfg == nil || identity == "" {
		return NamedSessionSpec{}, false
	}
	named := config.FindNamedSession(cfg, identity)
	if named == nil {
		return NamedSessionSpec{}, false
	}
	agentCfg := config.FindAgent(cfg, named.TemplateQualifiedName())
	if agentCfg == nil {
		return NamedSessionSpec{}, false
	}
	return NamedSessionSpec{
		Named:       named,
		Agent:       agentCfg,
		Identity:    identity,
		SessionName: config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity),
		Mode:        named.ModeOrDefault(),
	}, true
}

// NamedSessionBackingTemplate returns the resolved backing agent template for a named session spec.
func NamedSessionBackingTemplate(spec NamedSessionSpec) string {
	if spec.Agent != nil {
		return spec.Agent.QualifiedName()
	}
	if spec.Named != nil {
		return spec.Named.TemplateQualifiedName()
	}
	return ""
}

// ResolveNamedSessionSpecForConfigTarget resolves a config-facing token to a named session spec when possible.
func ResolveNamedSessionSpecForConfigTarget(cfg *config.City, cityName, target, rigContext string) (NamedSessionSpec, bool, error) {
	target = NormalizeNamedSessionTarget(target)
	if cfg == nil || target == "" {
		return NamedSessionSpec{}, false, nil
	}

	qualified := strings.Contains(target, "/")
	identities := map[string]bool{target: true}
	if !qualified && rigContext != "" {
		identities[rigContext+"/"+target] = true
	}

	// Collect every configured named session whose identity, runtime
	// session_name, or in-scope bare leaf matches the target. Bare leaf
	// matches are how packs-V2 imports like `gastown.mayor` accept a
	// user typing `mayor`. We fold every match shape into one candidate
	// set so rig/city and direct/fallback collisions surface as
	// ErrAmbiguous uniformly instead of the direct-match loop silently
	// winning.
	matched := NamedSessionSpec{}
	found := false
	for i := range cfg.NamedSessions {
		ns := &cfg.NamedSessions[i]
		identity := ns.QualifiedName()
		spec, ok := FindNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		match := false
		switch {
		case identities[identity]:
			match = true
		case spec.SessionName == target:
			match = true
		case !qualified && namedSessionBareName(ns) == target:
			// Rig-scoped named sessions are only reachable by bare
			// name from inside the rig, matching the pre-refactor
			// agent-template resolver.
			if ns.Dir == "" || (rigContext != "" && ns.Dir == rigContext) {
				match = true
			}
		}
		if !match {
			continue
		}
		if found && matched.Identity != spec.Identity {
			return NamedSessionSpec{}, false, fmt.Errorf("%w: %q matches multiple configured named sessions", ErrAmbiguous, target)
		}
		matched = spec
		found = true
	}
	if found {
		return matched, true, nil
	}
	return NamedSessionSpec{}, false, nil
}

// namedSessionBareName returns the unqualified public leaf name for a
// configured named session — the part a user would type without binding
// or rig prefixes. For `{BindingName: "gastown", Template: "mayor"}` it
// returns "mayor"; for `{Name: "boot", BindingName: "gastown"}` it
// returns "boot".
func namedSessionBareName(ns *config.NamedSession) string {
	if ns == nil {
		return ""
	}
	if ns.Name != "" {
		return ns.Name
	}
	return ns.Template
}

// FindNamedSessionSpecForTarget resolves a session-facing token to a named session spec.
func FindNamedSessionSpecForTarget(cfg *config.City, cityName, target, rigContext string) (NamedSessionSpec, bool, error) {
	target = NormalizeNamedSessionTarget(target)
	if cfg == nil || target == "" {
		return NamedSessionSpec{}, false, nil
	}
	return ResolveNamedSessionSpecForConfigTarget(cfg, cityName, target, rigContext)
}

// IsNamedSessionBead reports whether a bead was created for a configured named session.
func IsNamedSessionBead(b beads.Bead) bool {
	return strings.TrimSpace(b.Metadata[NamedSessionMetadataKey]) == "true"
}

// NamedSessionIdentity returns the configured named session identity stored on a bead.
func NamedSessionIdentity(b beads.Bead) string {
	return strings.TrimSpace(b.Metadata[NamedSessionIdentityMetadata])
}

// NamedSessionMode returns the configured named session mode stored on a bead.
func NamedSessionMode(b beads.Bead) string {
	return strings.TrimSpace(b.Metadata[NamedSessionModeMetadata])
}

// NamedSessionBeadMatchesSpec reports whether a bead belongs to the named session spec.
func NamedSessionBeadMatchesSpec(b beads.Bead, spec NamedSessionSpec) bool {
	if IsNamedSessionBead(b) && NamedSessionIdentity(b) == spec.Identity {
		return true
	}
	template := NormalizeNamedSessionTarget(strings.TrimSpace(b.Metadata["template"]))
	agentName := NormalizeNamedSessionTarget(strings.TrimSpace(b.Metadata["agent_name"]))
	backingTemplate := NamedSessionBackingTemplate(spec)
	return template == backingTemplate || agentName == backingTemplate
}

// NamedSessionContinuityEligible reports whether a bead can preserve named session continuity.
func NamedSessionContinuityEligible(b beads.Bead) bool {
	continuity := strings.TrimSpace(b.Metadata["continuity_eligible"])
	if continuity == "false" {
		return false
	}
	switch strings.TrimSpace(b.Metadata["state"]) {
	case "archived":
		return continuity == "true"
	case "closing", "closed", string(StateFailedCreate):
		return false
	default:
		return true
	}
}

// BeadConflictsWithNamedSession reports whether a bead blocks a configured named session identity.
func BeadConflictsWithNamedSession(b beads.Bead, spec NamedSessionSpec) bool {
	if IsNamedSessionBead(b) && NamedSessionIdentity(b) == spec.Identity {
		return false
	}
	if strings.TrimSpace(b.Metadata["session_name"]) == spec.SessionName {
		return !NamedSessionBeadMatchesSpec(b, spec)
	}
	if strings.TrimSpace(b.Metadata["alias"]) == spec.Identity {
		return true
	}
	return false
}

// ConfiguredNamedSessionLookup is the bounded lookup result for a configured named session.
type ConfiguredNamedSessionLookup struct {
	Canonical    beads.Bead
	HasCanonical bool
	Conflict     beads.Bead
	HasConflict  bool
}

// FindCanonicalConfiguredNamedSessionBead finds the live bead that owns a
// configured named session using exact metadata-filtered store queries.
func FindCanonicalConfiguredNamedSessionBead(store beads.Store, spec NamedSessionSpec) (beads.Bead, bool, error) {
	lookup, err := lookupConfiguredNamedSession(store, spec, false)
	if err != nil {
		return beads.Bead{}, false, err
	}
	return lookup.Canonical, lookup.HasCanonical, nil
}

// LookupConfiguredNamedSession finds the canonical bead or first live conflict
// for a configured named session using exact metadata-filtered store queries.
// The result is stitched from several sequential store reads; downstream
// uniqueness and claim serialization remain the authority under concurrent
// bead mutation.
func LookupConfiguredNamedSession(store beads.Store, spec NamedSessionSpec) (ConfiguredNamedSessionLookup, error) {
	return lookupConfiguredNamedSession(store, spec, true)
}

func lookupConfiguredNamedSession(store beads.Store, spec NamedSessionSpec, includeConflict bool) (ConfiguredNamedSessionLookup, error) {
	if store == nil {
		return ConfiguredNamedSessionLookup{}, nil
	}
	spec.Identity = NormalizeNamedSessionTarget(spec.Identity)
	spec.SessionName = strings.TrimSpace(spec.SessionName)
	if spec.Identity == "" && spec.SessionName == "" {
		return ConfiguredNamedSessionLookup{}, nil
	}

	candidates := make([]beads.Bead, 0, 4)
	seen := make(map[string]bool)
	var runtimeSessionNameMatches []beads.Bead

	if spec.Identity != "" {
		matches, err := listConfiguredNamedSessionBeadsByMetadata(store, NamedSessionIdentityMetadata, spec.Identity)
		if err != nil {
			return ConfiguredNamedSessionLookup{}, fmt.Errorf("listing canonical named session candidates: %w", err)
		}
		candidates = appendUniqueNamedSessionCandidates(candidates, seen, matches)
		if bead, ok := FindCanonicalNamedSessionBead(candidates, spec); ok {
			return ConfiguredNamedSessionLookup{Canonical: bead, HasCanonical: true}, nil
		}
	}

	if spec.SessionName != "" {
		matches, err := listConfiguredNamedSessionBeadsByMetadata(store, "session_name", spec.SessionName)
		if err != nil {
			return ConfiguredNamedSessionLookup{}, fmt.Errorf("listing canonical named session candidates by session_name: %w", err)
		}
		runtimeSessionNameMatches = matches
		candidates = appendUniqueNamedSessionCandidates(candidates, seen, matches)
		if bead, ok := FindCanonicalNamedSessionBead(candidates, spec); ok {
			return ConfiguredNamedSessionLookup{Canonical: bead, HasCanonical: true}, nil
		}
	}

	if spec.Identity != "" && spec.Identity != spec.SessionName {
		matches, err := listConfiguredNamedSessionBeadsByMetadata(store, "session_name", spec.Identity)
		if err != nil {
			return ConfiguredNamedSessionLookup{}, fmt.Errorf("listing canonical named session candidates by bare identity: %w", err)
		}
		candidates = appendUniqueNamedSessionCandidates(candidates, seen, matches)
		if bead, ok := FindCanonicalNamedSessionBead(candidates, spec); ok {
			return ConfiguredNamedSessionLookup{Canonical: bead, HasCanonical: true}, nil
		}
	}

	if !includeConflict {
		return ConfiguredNamedSessionLookup{}, nil
	}

	conflictCandidates := append([]beads.Bead{}, runtimeSessionNameMatches...)
	if spec.Identity != "" {
		matches, err := listConfiguredNamedSessionBeadsByMetadata(store, "alias", spec.Identity)
		if err != nil {
			return ConfiguredNamedSessionLookup{}, fmt.Errorf("listing alias conflicts: %w", err)
		}
		conflictCandidates = appendUniqueNamedSessionCandidates(conflictCandidates, make(map[string]bool, len(conflictCandidates)+len(matches)), matches)
	}
	if bead, conflict := FindNamedSessionConflict(conflictCandidates, spec); conflict {
		return ConfiguredNamedSessionLookup{Conflict: bead, HasConflict: true}, nil
	}
	return ConfiguredNamedSessionLookup{}, nil
}

func listConfiguredNamedSessionBeadsByMetadata(store beads.Store, key, value string) ([]beads.Bead, error) {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return nil, nil
	}
	items, err := store.List(beads.ListQuery{
		Metadata: map[string]string{key: value},
	})
	if err != nil {
		return nil, err
	}
	out := make([]beads.Bead, 0, len(items))
	for _, b := range items {
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		RepairEmptyType(store, &b)
		out = append(out, b)
	}
	return out, nil
}

func appendUniqueNamedSessionCandidates(dst []beads.Bead, seen map[string]bool, src []beads.Bead) []beads.Bead {
	for _, b := range dst {
		seen[b.ID] = true
	}
	for _, b := range src {
		if seen[b.ID] {
			continue
		}
		dst = append(dst, b)
		seen[b.ID] = true
	}
	return dst
}

// NamedSessionResolutionCandidates returns the live session beads that can own
// or conflict with the configured named-session spec.
//
// The implementation issues a single label-scoped store.List for gc:session
// beads and applies the four metadata predicates in process. Targeted
// per-key metadata lookups would be marginally cheaper per call against an
// indexed store, but every named-session resolution drives four sequential
// bd subprocess invocations through the BdStore exec runner. Under
// reconciler/wake load — N agents × 4 sequential bd subprocesses each —
// that fan-out saturates the bd CLI and the underlying Dolt connection
// pool, tipping individual list invocations past the 120s subprocess
// timeout (gascity ga-pa57, ga-sed; mayor escalation 2026-04-26). Folding
// the four metadata predicates into one label-scoped scan caps per-resolve
// bd invocations at one and bounds the candidate set by the active
// session count, which is small. Measured under 20-parallel load on a
// representative city: 5.2s → 1.3s. Interactive session-targeting paths
// that must avoid label-wide scans use LookupConfiguredNamedSession instead.
func NamedSessionResolutionCandidates(store beads.Store, spec NamedSessionSpec) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	identity := NormalizeNamedSessionTarget(spec.Identity)
	sessionName := strings.TrimSpace(spec.SessionName)
	if identity == "" && sessionName == "" {
		return nil, nil
	}
	items, err := store.List(beads.ListQuery{Label: LabelSession})
	if err != nil {
		return nil, err
	}
	candidates := make([]beads.Bead, 0, len(items))
	for _, b := range items {
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		if !beadMatchesNamedSessionResolutionFilter(b, identity, sessionName) {
			continue
		}
		RepairEmptyType(store, &b)
		candidates = append(candidates, b)
	}
	return candidates, nil
}

// beadMatchesNamedSessionResolutionFilter reports whether a bead matches any
// of the metadata predicates that NamedSessionResolutionCandidates folds
// in process: configured-named-identity, session_name against the runtime
// name, session_name against the bare identity, or alias against the bare
// identity. Empty arguments disable their respective predicates so the
// behavior matches ExactMetadataSessionCandidates' empty-filter handling.
func beadMatchesNamedSessionResolutionFilter(b beads.Bead, identity, sessionName string) bool {
	if identity != "" {
		if strings.TrimSpace(b.Metadata[NamedSessionIdentityMetadata]) == identity {
			return true
		}
		if strings.TrimSpace(b.Metadata["session_name"]) == identity {
			return true
		}
		if strings.TrimSpace(b.Metadata["alias"]) == identity {
			return true
		}
	}
	if sessionName != "" && strings.TrimSpace(b.Metadata["session_name"]) == sessionName {
		return true
	}
	return false
}

// FindNamedSessionConflict finds the first live session bead that blocks a configured named session.
func FindNamedSessionConflict(candidates []beads.Bead, spec NamedSessionSpec) (beads.Bead, bool) {
	for _, b := range candidates {
		if !IsSessionBeadOrRepairable(b) || b.Status == "closed" {
			continue
		}
		if BeadConflictsWithNamedSession(b, spec) {
			return b, true
		}
	}
	return beads.Bead{}, false
}

// FindClosedNamedSessionBead finds the newest closed bead for a named session identity.
func FindClosedNamedSessionBead(store beads.Store, identity string) (beads.Bead, bool, error) {
	return FindClosedNamedSessionBeadForSessionName(store, identity, "")
}

// FindClosedNamedSessionBeadForSessionName finds a closed bead for a named session identity.
func FindClosedNamedSessionBeadForSessionName(store beads.Store, identity, sessionName string) (beads.Bead, bool, error) {
	if store == nil {
		return beads.Bead{}, false, nil
	}
	identity = NormalizeNamedSessionTarget(identity)
	sessionName = strings.TrimSpace(sessionName)
	candidates, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			NamedSessionIdentityMetadata: identity,
		},
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
	})
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("listing closed named session beads for %q: %w", identity, err)
	}
	var fallback beads.Bead
	hasFallback := false
	for _, b := range candidates {
		if b.Status != "closed" {
			continue
		}
		if !closedNamedSessionReopenEligible(b) {
			continue
		}
		if sessionName != "" {
			if strings.TrimSpace(b.Metadata["session_name"]) == sessionName {
				return b, true, nil
			}
			continue
		}
		if strings.TrimSpace(b.Metadata["session_name"]) != "" {
			return b, true, nil
		}
		if !hasFallback {
			fallback = b
			hasFallback = true
		}
	}
	if hasFallback {
		return fallback, true, nil
	}
	return beads.Bead{}, false, nil
}

func closedNamedSessionReopenEligible(b beads.Bead) bool {
	if strings.TrimSpace(b.Metadata["continuity_eligible"]) == "false" {
		return false
	}
	switch strings.TrimSpace(b.Metadata["close_reason"]) {
	case "duplicate", "duplicate-repair", "gc_swept", "orphaned", "reconfigured", "stale-session", string(StateFailedCreate):
		return false
	}
	switch strings.TrimSpace(b.Metadata["state"]) {
	case "duplicate", "duplicate-repair", "gc_swept", "orphaned", "reconfigured", "stale-session", string(StateFailedCreate):
		return false
	}
	return true
}

// FindCanonicalNamedSessionBead finds the active bead that owns a configured named session.
func FindCanonicalNamedSessionBead(candidates []beads.Bead, spec NamedSessionSpec) (beads.Bead, bool) {
	identity := NormalizeNamedSessionTarget(spec.Identity)
	for _, b := range candidates {
		if !IsSessionBeadOrRepairable(b) || b.Status == "closed" || !NamedSessionContinuityEligible(b) {
			continue
		}
		if IsNamedSessionBead(b) && NamedSessionIdentity(b) == identity {
			return b, true
		}
	}
	for _, b := range candidates {
		if !IsSessionBeadOrRepairable(b) || b.Status == "closed" || !NamedSessionContinuityEligible(b) {
			continue
		}
		if !NamedSessionBeadMatchesSpec(b, spec) {
			continue
		}
		sn := strings.TrimSpace(b.Metadata["session_name"])
		if sn == spec.SessionName || sn == identity {
			return b, true
		}
	}
	return beads.Bead{}, false
}

// FindConflictingNamedSessionSpecForBead finds the configured named session blocked by a bead.
func FindConflictingNamedSessionSpecForBead(cfg *config.City, cityName string, b beads.Bead) (NamedSessionSpec, bool, error) {
	if cfg == nil {
		return NamedSessionSpec{}, false, nil
	}
	var matched NamedSessionSpec
	found := false
	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := FindNamedSessionSpec(cfg, cityName, identity)
		if !ok || !BeadConflictsWithNamedSession(b, spec) {
			continue
		}
		if found && matched.Identity != spec.Identity {
			return NamedSessionSpec{}, false, fmt.Errorf("%w: bead %s conflicts with multiple configured named sessions", ErrAmbiguous, b.ID)
		}
		matched = spec
		found = true
	}
	return matched, found, nil
}

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

	var identities []string
	if strings.Contains(target, "/") {
		identities = append(identities, target)
	} else {
		identities = append(identities, target)
		if rigContext != "" {
			identities = append(identities, rigContext+"/"+target)
		}
	}
	var matched NamedSessionSpec
	found := false
	seen := make(map[string]bool, len(identities))
	for _, identity := range identities {
		if identity == "" || seen[identity] {
			continue
		}
		seen[identity] = true
		if spec, ok := FindNamedSessionSpec(cfg, cityName, identity); ok {
			if found && matched.Identity != spec.Identity {
				return NamedSessionSpec{}, false, fmt.Errorf("%w: %q matches multiple configured named sessions", ErrAmbiguous, target)
			}
			matched = spec
			found = true
		}
	}
	if found {
		return matched, true, nil
	}

	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := FindNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		if spec.SessionName != target {
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
	case "closing", "closed":
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

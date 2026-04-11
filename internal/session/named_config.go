package session

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const (
	NamedSessionMetadataKey      = "configured_named_session"
	NamedSessionIdentityMetadata = "configured_named_identity"
	NamedSessionModeMetadata     = "configured_named_mode"
)

type NamedSessionSpec struct {
	Named       *config.NamedSession
	Agent       *config.Agent
	Identity    string
	SessionName string
	Mode        string
}

func NormalizeNamedSessionTarget(target string) string {
	target = strings.TrimSpace(target)
	target = strings.TrimSuffix(target, "/")
	return target
}

func TargetBasename(target string) string {
	target = NormalizeNamedSessionTarget(target)
	if i := strings.LastIndex(target, "/"); i >= 0 {
		return target[i+1:]
	}
	return target
}

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

func NamedSessionBackingTemplate(spec NamedSessionSpec) string {
	if spec.Agent != nil {
		return spec.Agent.QualifiedName()
	}
	if spec.Named != nil {
		return spec.Named.TemplateQualifiedName()
	}
	return ""
}

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

func FindNamedSessionSpecForTarget(cfg *config.City, cityName, target, rigContext string) (NamedSessionSpec, bool, error) {
	target = NormalizeNamedSessionTarget(target)
	if cfg == nil || target == "" {
		return NamedSessionSpec{}, false, nil
	}
	return ResolveNamedSessionSpecForConfigTarget(cfg, cityName, target, rigContext)
}

func IsNamedSessionBead(b beads.Bead) bool {
	return strings.TrimSpace(b.Metadata[NamedSessionMetadataKey]) == "true"
}

func NamedSessionIdentity(b beads.Bead) string {
	return strings.TrimSpace(b.Metadata[NamedSessionIdentityMetadata])
}

func NamedSessionMode(b beads.Bead) string {
	return strings.TrimSpace(b.Metadata[NamedSessionModeMetadata])
}

func NamedSessionBeadMatchesSpec(b beads.Bead, spec NamedSessionSpec) bool {
	if IsNamedSessionBead(b) && NamedSessionIdentity(b) == spec.Identity {
		return true
	}
	template := NormalizeNamedSessionTarget(strings.TrimSpace(b.Metadata["template"]))
	agentName := NormalizeNamedSessionTarget(strings.TrimSpace(b.Metadata["agent_name"]))
	backingTemplate := NamedSessionBackingTemplate(spec)
	return template == backingTemplate || agentName == backingTemplate
}

func NamedSessionContinuityEligible(b beads.Bead) bool {
	if strings.TrimSpace(b.Metadata["continuity_eligible"]) == "false" {
		return false
	}
	switch strings.TrimSpace(b.Metadata["state"]) {
	case "archived", "closing", "closed":
		return false
	default:
		return true
	}
}

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

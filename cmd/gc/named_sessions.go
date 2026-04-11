package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	namedSessionMetadataKey      = "configured_named_session"
	namedSessionIdentityMetadata = "configured_named_identity"
	namedSessionModeMetadata     = "configured_named_mode"
)

type namedSessionSpec struct {
	Named       *config.NamedSession
	Agent       *config.Agent
	Identity    string
	SessionName string
	Mode        string
}

func normalizeNamedSessionTarget(target string) string {
	target = strings.TrimSpace(target)
	target = strings.TrimSuffix(target, "/")
	return target
}

func targetBasename(target string) string {
	target = normalizeNamedSessionTarget(target)
	if i := strings.LastIndex(target, "/"); i >= 0 {
		return target[i+1:]
	}
	return target
}

func findNamedSessionSpec(cfg *config.City, cityName, identity string) (namedSessionSpec, bool) {
	identity = normalizeNamedSessionTarget(identity)
	if cfg == nil || identity == "" {
		return namedSessionSpec{}, false
	}
	named := config.FindNamedSession(cfg, identity)
	if named == nil {
		return namedSessionSpec{}, false
	}
	agentCfg := config.FindAgent(cfg, named.TemplateQualifiedName())
	if agentCfg == nil {
		return namedSessionSpec{}, false
	}
	return namedSessionSpec{
		Named:       named,
		Agent:       agentCfg,
		Identity:    identity,
		SessionName: config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity),
		Mode:        named.ModeOrDefault(),
	}, true
}

func namedSessionBackingTemplate(spec namedSessionSpec) string {
	if spec.Agent != nil {
		return spec.Agent.QualifiedName()
	}
	if spec.Named != nil {
		return spec.Named.TemplateQualifiedName()
	}
	return ""
}

func resolveNamedSessionSpecForConfigTarget(cfg *config.City, cityName, target, rigContext string) (namedSessionSpec, bool, error) {
	target = normalizeNamedSessionTarget(target)
	if cfg == nil || target == "" {
		return namedSessionSpec{}, false, nil
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
	var matched namedSessionSpec
	found := false
	seen := make(map[string]bool, len(identities))
	for _, identity := range identities {
		if identity == "" || seen[identity] {
			continue
		}
		seen[identity] = true
		if spec, ok := findNamedSessionSpec(cfg, cityName, identity); ok {
			if found && matched.Identity != spec.Identity {
				return namedSessionSpec{}, false, fmt.Errorf("%w: %q matches multiple configured named sessions", session.ErrAmbiguous, target)
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
		spec, ok := findNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		if spec.SessionName != target {
			continue
		}
		if found && matched.Identity != spec.Identity {
			return namedSessionSpec{}, false, fmt.Errorf("%w: %q matches multiple configured named sessions", session.ErrAmbiguous, target)
		}
		matched = spec
		found = true
	}
	if found {
		return matched, true, nil
	}
	return namedSessionSpec{}, false, nil
}

func findNamedSessionSpecForTarget(cfg *config.City, cityName, target string) (namedSessionSpec, bool, error) {
	target = normalizeNamedSessionTarget(target)
	if cfg == nil || target == "" {
		return namedSessionSpec{}, false, nil
	}
	if spec, ok, err := resolveNamedSessionSpecForConfigTarget(cfg, cityName, target, currentRigContext(cfg)); err != nil {
		return namedSessionSpec{}, false, err
	} else if ok {
		return spec, true, nil
	}

	var matched namedSessionSpec
	found := false
	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := findNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		if spec.SessionName == target {
			if found {
				return namedSessionSpec{}, false, fmt.Errorf("%w: %q matches multiple configured named sessions", session.ErrAmbiguous, target)
			}
			matched = spec
			found = true
		}
	}
	return matched, found, nil
}

func isNamedSessionBead(b beads.Bead) bool {
	return strings.TrimSpace(b.Metadata[namedSessionMetadataKey]) == "true"
}

func namedSessionIdentity(b beads.Bead) string {
	return strings.TrimSpace(b.Metadata[namedSessionIdentityMetadata])
}

func namedSessionMode(b beads.Bead) string {
	return strings.TrimSpace(b.Metadata[namedSessionModeMetadata])
}

func namedSessionBeadMatchesSpec(b beads.Bead, spec namedSessionSpec) bool {
	if isNamedSessionBead(b) && namedSessionIdentity(b) == spec.Identity {
		return true
	}
	template := normalizeNamedSessionTarget(strings.TrimSpace(b.Metadata["template"]))
	agentName := normalizeNamedSessionTarget(strings.TrimSpace(b.Metadata["agent_name"]))
	backingTemplate := namedSessionBackingTemplate(spec)
	return template == backingTemplate || agentName == backingTemplate
}

func namedSessionContinuityEligible(b beads.Bead) bool {
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

func findCanonicalNamedSessionBead(sessionBeads *sessionBeadSnapshot, spec namedSessionSpec) (beads.Bead, bool) {
	if sessionBeads == nil {
		return beads.Bead{}, false
	}
	identity := normalizeNamedSessionTarget(spec.Identity)
	// First pass: look for beads explicitly tagged as this named session.
	for _, b := range sessionBeads.Open() {
		if !namedSessionContinuityEligible(b) {
			continue
		}
		if isNamedSessionBead(b) && namedSessionIdentity(b) == identity {
			return b, true
		}
	}
	// Second pass: adopt pre-existing session beads whose canonical runtime
	// session_name matches the named session. This
	// covers beads created before the named session config was added
	// (e.g., implicit agents promoted to named sessions).
	for _, b := range sessionBeads.Open() {
		if !namedSessionContinuityEligible(b) {
			continue
		}
		if !namedSessionBeadMatchesSpec(b, spec) {
			continue
		}
		sn := strings.TrimSpace(b.Metadata["session_name"])
		if sn == spec.SessionName || sn == identity {
			return b, true
		}
	}
	return beads.Bead{}, false
}

// findClosedNamedSessionBead searches for a closed bead that was previously
// the canonical bead for the given named session identity. Uses a targeted
// metadata query (Store.ListByMetadata) so only matching beads are returned
// — no bulk scan of all closed beads.
func findClosedNamedSessionBead(store beads.Store, identity string) (beads.Bead, bool) {
	return findClosedNamedSessionBeadForSessionName(store, identity, "")
}

func findClosedNamedSessionBeadForSessionName(store beads.Store, identity, sessionName string) (beads.Bead, bool) {
	identity = normalizeNamedSessionTarget(identity)
	sessionName = strings.TrimSpace(sessionName)
	candidates, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			namedSessionIdentityMetadata: identity,
		},
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
	})
	if err != nil {
		return beads.Bead{}, false
	}
	var fallback beads.Bead
	hasFallback := false
	for _, b := range candidates {
		if b.Status != "closed" {
			continue
		}
		if sessionName != "" {
			if strings.TrimSpace(b.Metadata["session_name"]) == sessionName {
				return b, true
			}
			continue
		}
		if strings.TrimSpace(b.Metadata["session_name"]) != "" {
			return b, true
		}
		if !hasFallback {
			fallback = b
			hasFallback = true
		}
	}
	if hasFallback {
		return fallback, true
	}
	return beads.Bead{}, false
}

func beadConflictsWithNamedSession(b beads.Bead, spec namedSessionSpec) bool {
	if isNamedSessionBead(b) && namedSessionIdentity(b) == spec.Identity {
		return false
	}
	if strings.TrimSpace(b.Metadata["session_name"]) == spec.SessionName {
		return !namedSessionBeadMatchesSpec(b, spec)
	}
	if strings.TrimSpace(b.Metadata["alias"]) == spec.Identity {
		return true
	}
	return false
}

func findNamedSessionConflict(sessionBeads *sessionBeadSnapshot, spec namedSessionSpec) (beads.Bead, bool) {
	if sessionBeads == nil {
		return beads.Bead{}, false
	}
	for _, b := range sessionBeads.Open() {
		if beadConflictsWithNamedSession(b, spec) {
			return b, true
		}
	}
	return beads.Bead{}, false
}

func findConflictingNamedSessionSpecForBead(cfg *config.City, cityName string, b beads.Bead) (namedSessionSpec, bool, error) {
	if cfg == nil {
		return namedSessionSpec{}, false, nil
	}
	var matched namedSessionSpec
	found := false
	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := findNamedSessionSpec(cfg, cityName, identity)
		if !ok || !beadConflictsWithNamedSession(b, spec) {
			continue
		}
		if found && matched.Identity != spec.Identity {
			return namedSessionSpec{}, false, fmt.Errorf("%w: bead %s conflicts with multiple configured named sessions", session.ErrAmbiguous, b.ID)
		}
		matched = spec
		found = true
	}
	return matched, found, nil
}

func sessionAliasHistoryContains(metadata map[string]string, target string) bool {
	for _, alias := range session.AliasHistory(metadata) {
		if alias == target {
			return true
		}
	}
	return false
}

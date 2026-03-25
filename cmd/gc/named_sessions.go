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
	agentCfg := config.FindAgent(cfg, identity)
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

func resolveNamedSessionSpecForConfigTarget(cfg *config.City, cityName, target, rigContext string) (namedSessionSpec, bool, error) {
	target = normalizeNamedSessionTarget(target)
	if cfg == nil || target == "" {
		return namedSessionSpec{}, false, nil
	}

	var identities []string
	if !strings.Contains(target, "/") && rigContext != "" {
		identities = append(identities, rigContext+"/"+target)
	}
	identities = append(identities, target)
	seen := make(map[string]bool, len(identities))
	for _, identity := range identities {
		if identity == "" || seen[identity] {
			continue
		}
		seen[identity] = true
		if spec, ok := findNamedSessionSpec(cfg, cityName, identity); ok {
			return spec, true, nil
		}
	}

	var matched namedSessionSpec
	found := false
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

	if resolved, ok := resolveSessionTemplate(cfg, target, rigContext); ok {
		if spec, ok := findNamedSessionSpec(cfg, cityName, resolved.QualifiedName()); ok {
			return spec, true, nil
		}
	}

	if strings.Contains(target, "/") {
		return namedSessionSpec{}, false, nil
	}

	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		spec, ok := findNamedSessionSpec(cfg, cityName, identity)
		if !ok {
			continue
		}
		if targetBasename(spec.Identity) != target {
			continue
		}
		if found && matched.Identity != spec.Identity {
			return namedSessionSpec{}, false, fmt.Errorf("%w: %q matches multiple configured named sessions", session.ErrAmbiguous, target)
		}
		matched = spec
		found = true
	}
	return matched, found, nil
}

func findNamedSessionSpecForTarget(cfg *config.City, cityName string, store beads.Store, target string) (namedSessionSpec, bool, error) {
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

	sessionBeads, err := loadSessionBeadSnapshot(store)
	if err != nil {
		return namedSessionSpec{}, false, err
	}
	for _, b := range sessionBeads.Open() {
		if !isNamedSessionBead(b) {
			continue
		}
		if !sessionAliasHistoryContains(b.Metadata, target) {
			continue
		}
		spec, ok := findNamedSessionSpec(cfg, cityName, namedSessionIdentity(b))
		if !ok {
			continue
		}
		if found && matched.Identity != spec.Identity {
			return namedSessionSpec{}, false, fmt.Errorf("%w: %q matches multiple configured named sessions", session.ErrAmbiguous, target)
		}
		matched = spec
		found = true
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

func findCanonicalNamedSessionBead(sessionBeads *sessionBeadSnapshot, identity string) (beads.Bead, bool) {
	if sessionBeads == nil {
		return beads.Bead{}, false
	}
	identity = normalizeNamedSessionTarget(identity)
	for _, b := range sessionBeads.Open() {
		if isNamedSessionBead(b) && namedSessionIdentity(b) == identity {
			return b, true
		}
	}
	return beads.Bead{}, false
}

func beadConflictsWithNamedSession(b beads.Bead, spec namedSessionSpec) bool {
	if isNamedSessionBead(b) && namedSessionIdentity(b) == spec.Identity {
		return false
	}
	if strings.TrimSpace(b.Metadata["session_name"]) == spec.SessionName {
		return true
	}
	if strings.TrimSpace(b.Metadata["alias"]) == spec.Identity {
		return true
	}
	for _, alias := range session.AliasHistory(b.Metadata) {
		if alias == spec.Identity {
			return true
		}
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

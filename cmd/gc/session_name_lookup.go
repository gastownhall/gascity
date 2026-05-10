package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const poolManagedMetadataKey = "pool_managed"

type poolSessionCreateIdentity struct {
	AgentName string
	Alias     string
	Slot      int
}

func isPoolManagedSessionBead(bead beads.Bead) bool {
	if isEphemeralSessionBead(bead) {
		return true
	}
	if strings.TrimSpace(bead.Metadata[poolManagedMetadataKey]) == boolMetadata(true) {
		return true
	}
	return strings.TrimSpace(bead.Metadata["pool_slot"]) != ""
}

func resolveLegacyPoolTemplate(cfg *config.City, storedTemplate string) string {
	storedTemplate = strings.TrimSpace(storedTemplate)
	if cfg == nil || storedTemplate == "" {
		return ""
	}
	if findAgentByTemplate(cfg, storedTemplate) != nil {
		return storedTemplate
	}
	match := ""
	for i := range cfg.Agents {
		agentCfg := &cfg.Agents[i]
		if !agentCfg.SupportsInstanceExpansion() {
			continue
		}
		_, localTemplate := config.ParseQualifiedName(agentCfg.QualifiedName())
		if localTemplate != storedTemplate {
			continue
		}
		if match != "" && match != agentCfg.QualifiedName() {
			return ""
		}
		match = agentCfg.QualifiedName()
	}
	return match
}

func sessionBeadStoredTemplate(bead beads.Bead) string {
	storedTemplate := strings.TrimSpace(bead.Metadata["template"])
	if storedTemplate != "" {
		return storedTemplate
	}
	return strings.TrimSpace(bead.Metadata["common_name"])
}

func resolvedTemplateForIdentity(identity string, cfg *config.City) string {
	identity = strings.TrimSpace(identity)
	if cfg == nil || identity == "" {
		return ""
	}
	if findAgentByTemplate(cfg, identity) != nil {
		return identity
	}
	if resolved := resolveLegacyPoolTemplate(cfg, identity); resolved != "" {
		return resolved
	}
	match := ""
	for i := range cfg.Agents {
		agentCfg := &cfg.Agents[i]
		if !agentCfg.SupportsInstanceExpansion() {
			continue
		}
		slot := resolvePersistedPoolIdentitySlot(agentCfg, true, identity)
		if slot <= 0 {
			continue
		}
		if poolSlotHasConfiguredBound(agentCfg) && !inBoundsPoolSlot(agentCfg, slot) {
			continue
		}
		if match != "" && match != agentCfg.QualifiedName() {
			return ""
		}
		match = agentCfg.QualifiedName()
	}
	return match
}

func resolvedSessionTemplate(bead beads.Bead, cfg *config.City) string {
	template := normalizedSessionTemplate(bead, cfg)
	if template != "" && (cfg == nil || findAgentByTemplate(cfg, template) != nil) {
		return template
	}
	storedTemplate := sessionBeadStoredTemplate(bead)
	if storedTemplate == "" {
		return ""
	}
	if resolved := resolveLegacyPoolTemplate(cfg, storedTemplate); resolved != "" {
		return resolved
	}
	return storedTemplate
}

func storedTemplateMatchesPoolTemplate(storedTemplate, template string, cfg *config.City) bool {
	storedTemplate = strings.TrimSpace(storedTemplate)
	template = strings.TrimSpace(template)
	if storedTemplate == "" || template == "" {
		return false
	}
	if storedTemplate == template {
		return true
	}
	return resolveLegacyPoolTemplate(cfg, storedTemplate) == template
}

func createPoolSessionBead(
	store beads.Store,
	template string,
	sessionBeads *sessionBeadSnapshot,
	now time.Time,
	identity poolSessionCreateIdentity,
) (beads.Bead, error) {
	if store == nil {
		return beads.Bead{}, fmt.Errorf("session store unavailable for pool template %q", template)
	}
	instanceToken := sessionpkg.NewInstanceToken()
	agentName := strings.TrimSpace(identity.AgentName)
	title := targetBasename(template)
	if agentName == "" {
		agentName = template
	} else {
		title = agentName
	}
	meta := map[string]string{
		"template":                  template,
		"agent_name":                agentName,
		"state":                     "creating",
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAtNow(now),
		"session_origin":            "ephemeral",
		"generation":                "1",
		"continuation_epoch":        "1",
		"instance_token":            instanceToken,
		"session_name":              pendingPoolSessionName(template, instanceToken),
		poolManagedMetadataKey:      boolMetadata(true),
	}
	if alias := strings.TrimSpace(identity.Alias); alias != "" {
		meta["alias"] = alias
	}
	if identity.Slot > 0 {
		meta["pool_slot"] = strconv.Itoa(identity.Slot)
	}
	bead, err := store.Create(beads.Bead{
		Title:    title,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "agent:" + agentName},
		Metadata: meta,
	})
	if err != nil {
		return beads.Bead{}, err
	}
	sessionName := PoolSessionName(template, bead.ID)
	if err := store.SetMetadata(bead.ID, "session_name", sessionName); err != nil {
		_ = store.Close(bead.ID)
		return beads.Bead{}, err
	}
	bead.Metadata["session_name"] = sessionName
	if sessionBeads != nil {
		sessionBeads.add(bead)
	}
	return bead, nil
}

// resolveSessionName returns the session name for a qualified agent name.
// When a bead store is available, it looks up an existing session bead and
// returns its session_name metadata. When no bead is found (or no store is
// available), it falls back to the legacy SessionNameFor function.
//
// templateName is the base config template name (e.g., "worker" for pool
// instance "worker-1"). For non-pool agents, templateName == qualifiedName.
//
// Results are cached in p.beadNames for the duration of the build cycle.
func (p *agentBuildParams) resolveSessionName(qualifiedName, _ string) string {
	// Check cache first.
	if sn, ok := p.beadNames[qualifiedName]; ok {
		return sn
	}

	// Try bead store lookup if available.
	if p.sessionBeads != nil {
		sn := p.sessionBeads.FindSessionNameByTemplate(qualifiedName)
		if sn != "" {
			p.beadNames[qualifiedName] = sn
			return sn
		}
	}
	if p.beadStore != nil {
		sn := findSessionNameByTemplate(p.beadStore, qualifiedName)
		if sn != "" {
			p.beadNames[qualifiedName] = sn
			return sn
		}
	}

	// No bead found (or no store) → legacy path.
	sn := agent.SessionNameFor(p.cityName, qualifiedName, p.sessionTemplate)
	p.beadNames[qualifiedName] = sn
	return sn
}

// sessionNameFromBeadID derives the tmux session name from a bead ID.
// This is the universal naming convention: "s-" + beadID with "/" replaced.
func sessionNameFromBeadID(beadID string) string {
	return "s-" + strings.ReplaceAll(beadID, "/", "--")
}

func sessionBeadAgentName(bead beads.Bead) string {
	if bead.Metadata["agent_name"] != "" {
		return bead.Metadata["agent_name"]
	}
	for _, label := range bead.Labels {
		if strings.HasPrefix(label, "agent:") {
			return strings.TrimPrefix(label, "agent:")
		}
	}
	return ""
}

func normalizedSessionTemplate(bead beads.Bead, cfg *config.City) string {
	template := bead.Metadata["template"]
	if cfg == nil {
		return template
	}
	if template != "" && findAgentByTemplate(cfg, template) != nil {
		return template
	}
	agentName := sessionBeadAgentName(bead)
	if agentName != "" {
		if resolved := resolvedTemplateForIdentity(agentName, cfg); resolved != "" {
			return resolved
		}
	}
	if resolved := resolvedTemplateForIdentity(strings.TrimSpace(bead.Metadata["alias"]), cfg); resolved != "" {
		return resolved
	}
	return template
}

// findSessionNameByTemplate searches for an open session bead with the given
// template and returns its session_name metadata. Returns "" if not found.
// Pool instance beads (those with pool_slot metadata) are skipped to prevent
// a template query like "worker" from matching pool instance "worker-1".
//
// To avoid ambiguity between managed agent beads (created by syncSessionBeads)
// and ad-hoc session beads (created by gc session new), the function prefers
// beads with an agent_name field matching the query. If no agent_name match
// is found, falls back to template/common_name matching.
func findSessionNameByTemplate(store beads.Store, template string) string {
	template = strings.TrimSpace(template)
	if store == nil || template == "" {
		return ""
	}
	if sn := findSessionNameByMetadata(store, "agent_name", template, true); sn != "" {
		return sn
	}
	if sn := findSessionNameByAgentLabel(store, template); sn != "" {
		return sn
	}
	if sn := findSessionNameByMetadata(store, "template", template, false); sn != "" {
		return sn
	}
	return findSessionNameByMetadata(store, "common_name", template, false)
}

func findSessionNameByAgentLabel(store beads.Store, template string) string {
	items, err := store.List(beads.ListQuery{Label: "agent:" + template})
	if err != nil {
		return ""
	}
	return chooseSessionNameForTemplate(store, items, true, "", "")
}

func findSessionNameByMetadata(store beads.Store, key, value string, agentNameMatch bool) string {
	items, err := sessionpkg.ExactMetadataSessionCandidates(store, false, map[string]string{key: value})
	if err != nil {
		return ""
	}
	return chooseSessionNameForTemplate(store, items, agentNameMatch, key, value)
}

func chooseSessionNameForTemplate(store beads.Store, items []beads.Bead, agentNameMatch bool, key, value string) string {
	var fallback string
	for _, b := range items {
		if !sessionpkg.IsSessionBeadOrRepairable(b) || b.Status == "closed" {
			continue
		}
		sessionpkg.RepairEmptyType(store, &b)
		if key != "" && strings.TrimSpace(b.Metadata[key]) != value {
			continue
		}
		if agentNameMatch && isPoolManagedSessionBead(b) && sessionBeadAgentName(b) == b.Metadata["template"] {
			continue
		}
		if !agentNameMatch && isPoolManagedSessionBead(b) {
			continue
		}
		sessionName := strings.TrimSpace(b.Metadata["session_name"])
		if sessionName == "" {
			continue
		}
		if strings.TrimSpace(b.Metadata["configured_named_identity"]) != "" {
			return sessionName
		}
		if fallback == "" {
			fallback = sessionName
		}
	}
	return fallback
}

// lookupSessionName resolves a qualified agent name to its bead-derived
// session name by querying the bead store. Returns the session name and
// true if found, or ("", false) if no matching session bead exists.
//
// This is the CLI-facing equivalent of agentBuildParams.resolveSessionName,
// for use by commands that don't go through buildDesiredState.
func lookupSessionName(store beads.Store, qualifiedName string) (string, bool) {
	if store == nil {
		return "", false
	}
	sn := findSessionNameByTemplate(store, qualifiedName)
	if sn != "" {
		return sn, true
	}
	return "", false
}

// lookupSessionNameOrLegacy resolves a qualified agent name to its session
// name. Tries the bead store first; falls back to the legacy SessionNameFor
// function if no bead is found.
func lookupSessionNameOrLegacy(store beads.Store, cityName, qualifiedName, sessionTemplate string) string {
	if sn, ok := lookupSessionName(store, qualifiedName); ok {
		return sn
	}
	return agent.SessionNameFor(cityName, qualifiedName, sessionTemplate)
}

// lookupPoolSessionNames returns bead-backed session names for pool instances
// under the given template-qualified agent. The result maps the logical
// instance qualified name (for example "frontend/worker-1") to the actual
// runtime session name.
type poolLookupCandidate struct {
	sessionName         string
	score               int
	stateRank           int
	ownsPoolSessionName bool
}

func poolLookupCandidateStateRank(b beads.Bead) int {
	switch sessionMetadataState(b) {
	case "active":
		return 2
	case "creating":
		return 1
	default:
		return 0
	}
}

func poolLookupCandidatesEquivalent(a, b poolLookupCandidate) bool {
	return a.score == b.score &&
		a.stateRank == b.stateRank &&
		a.ownsPoolSessionName == b.ownsPoolSessionName
}

func lookupPoolSessionNameCandidates(store beads.Store, template string, cfg *config.City, cfgAgent *config.Agent) (map[string][]poolLookupCandidate, error) {
	result := make(map[string][]poolLookupCandidate)
	if store == nil {
		return result, nil
	}
	all, err := store.List(beads.ListQuery{
		Label: sessionBeadLabel,
	})
	if err != nil {
		return result, err
	}
	for _, b := range all {
		if !sessionpkg.IsSessionBeadOrRepairable(b) {
			continue
		}
		if b.Status == "closed" {
			continue
		}
		if isFailedCreateSessionBead(b) {
			continue
		}
		if isNamedSessionBead(b) || isManualSessionBeadForAgent(b, cfgAgent) {
			continue
		}
		storedTemplateMatches := storedTemplateMatchesPoolTemplate(sessionBeadStoredTemplate(b), template, cfg)
		resolveSlot := func(identity string) int {
			if cfgAgent != nil {
				return resolvePersistedPoolIdentitySlot(cfgAgent, storedTemplateMatches, identity)
			}
			return 0
		}
		qualifiedInstanceName := func(slot int) string {
			if cfgAgent != nil {
				return cfgAgent.QualifiedInstanceName(poolInstanceName(cfgAgent.Name, slot, cfgAgent))
			}
			return template + "-" + strconv.Itoa(slot)
		}
		agentSlot := resolveSlot(sessionBeadAgentName(b))
		aliasSlot := resolveSlot(strings.TrimSpace(b.Metadata["alias"]))
		sessionName := strings.TrimSpace(b.Metadata["session_name"])
		sessionNameSlot := 0
		if storedTemplateMatches && strings.TrimSpace(b.Metadata["alias"]) == "" && !beadOwnsPoolSessionName(b) {
			sessionNameSlot = resolveSlot(sessionName)
		}
		if cfgAgent != nil && poolSlotHasConfiguredBound(cfgAgent) {
			if agentSlot > 0 && !inBoundsPoolSlot(cfgAgent, agentSlot) {
				agentSlot = 0
			}
			if aliasSlot > 0 && !inBoundsPoolSlot(cfgAgent, aliasSlot) {
				aliasSlot = 0
			}
			if sessionNameSlot > 0 && !inBoundsPoolSlot(cfgAgent, sessionNameSlot) {
				sessionNameSlot = 0
			}
		}
		if !storedTemplateMatches && agentSlot == 0 && aliasSlot == 0 {
			continue
		}
		if sessionName == "" {
			continue
		}
		agentName := sessionBeadAgentName(b)
		if storedTemplateMatches && (agentName == template || agentName == targetBasename(template)) {
			agentName = ""
		}
		switch {
		case agentSlot > 0:
			agentName = qualifiedInstanceName(agentSlot)
		case aliasSlot > 0:
			agentName = qualifiedInstanceName(aliasSlot)
		case sessionNameSlot > 0:
			agentName = qualifiedInstanceName(sessionNameSlot)
		case agentName == "" && storedTemplateMatches && strings.TrimSpace(b.Metadata["pool_slot"]) != "":
			if slot, err := strconv.Atoi(strings.TrimSpace(b.Metadata["pool_slot"])); err == nil && slot > 0 {
				if cfgAgent == nil || !poolSlotHasConfiguredBound(cfgAgent) || inBoundsPoolSlot(cfgAgent, slot) {
					agentName = qualifiedInstanceName(slot)
				}
			}
		}
		if agentName == "" {
			continue
		}
		score := 0
		if strings.TrimSpace(b.Metadata["pool_slot"]) != "" {
			score += 2
		}
		if strings.TrimSpace(b.Metadata["template"]) == template {
			score++
		}
		if agentSlot > 0 {
			score += 2
		}
		if aliasSlot > 0 {
			score++
		}
		candidate := poolLookupCandidate{
			sessionName:         sessionName,
			score:               score,
			stateRank:           poolLookupCandidateStateRank(b),
			ownsPoolSessionName: beadOwnsPoolSessionName(b),
		}
		existing := result[agentName]
		replaced := false
		for idx := range existing {
			if existing[idx].sessionName != sessionName {
				continue
			}
			if candidate.score > existing[idx].score ||
				(candidate.score == existing[idx].score && candidate.stateRank > existing[idx].stateRank) ||
				(candidate.score == existing[idx].score && candidate.stateRank == existing[idx].stateRank && candidate.ownsPoolSessionName && !existing[idx].ownsPoolSessionName) {
				existing[idx] = candidate
			}
			replaced = true
			break
		}
		if !replaced {
			existing = append(existing, candidate)
		}
		result[agentName] = existing
	}
	for agentName, candidates := range result {
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].score != candidates[j].score {
				return candidates[i].score > candidates[j].score
			}
			if candidates[i].stateRank != candidates[j].stateRank {
				return candidates[i].stateRank > candidates[j].stateRank
			}
			if candidates[i].ownsPoolSessionName != candidates[j].ownsPoolSessionName {
				return candidates[i].ownsPoolSessionName
			}
			return candidates[i].sessionName < candidates[j].sessionName
		})
		result[agentName] = candidates
	}
	return result, nil
}

func lookupPoolSessionNames(store beads.Store, cfg *config.City, cfgAgent *config.Agent) (map[string]string, error) {
	template := ""
	if cfgAgent != nil {
		template = cfgAgent.QualifiedName()
	}
	candidates, err := lookupPoolSessionNameCandidates(store, template, cfg, cfgAgent)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(candidates))
	for agentName, ranked := range candidates {
		if len(ranked) == 0 {
			continue
		}
		if len(ranked) > 1 && poolLookupCandidatesEquivalent(ranked[0], ranked[1]) && ranked[0].sessionName != ranked[1].sessionName {
			continue
		}
		result[agentName] = ranked[0].sessionName
	}
	return result, nil
}

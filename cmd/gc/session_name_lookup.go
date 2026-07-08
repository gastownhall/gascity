package main

import (
	"errors"
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

type explicitBeadIDStore interface {
	IDPrefix() string
}

type poolSessionCreateIdentity struct {
	AgentName string
	Alias     string
	Slot      int
	Metadata  map[string]string
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

// isPoolManagedSessionInfo is the session.Info mirror of isPoolManagedSessionBead.
func isPoolManagedSessionInfo(i sessionpkg.Info) bool {
	if isEphemeralSessionInfo(i) {
		return true
	}
	if i.PoolManaged {
		return true
	}
	return strings.TrimSpace(i.PoolSlot) != ""
}

// isCanonicalPoolManagedSessionBeadForTemplate is the bead-shape companion to
// config.Agent.UsesCanonicalSingletonPoolIdentity: pool-managed, no pool slot,
// and canonical identity according to beadIdentifiesAsCanonical.
func isCanonicalPoolManagedSessionBeadForTemplate(bead beads.Bead, template string) bool {
	template = strings.TrimSpace(template)
	if template == "" || !isPoolManagedSessionBead(bead) {
		return false
	}
	if strings.TrimSpace(bead.Metadata["pool_slot"]) != "" {
		return false
	}
	return beadIdentifiesAsCanonical(bead, template)
}

// isCanonicalPoolManagedSessionInfoForTemplate is the session.Info mirror of
// isCanonicalPoolManagedSessionBeadForTemplate.
func isCanonicalPoolManagedSessionInfoForTemplate(i sessionpkg.Info, template string) bool {
	template = strings.TrimSpace(template)
	if template == "" || !isPoolManagedSessionInfo(i) {
		return false
	}
	if strings.TrimSpace(i.PoolSlot) != "" {
		return false
	}
	return infoIdentifiesAsCanonical(i, template)
}

func resolveLegacyPoolTemplate(cfg *config.City, storedTemplate string) string {
	storedTemplate = strings.TrimSpace(storedTemplate)
	if cfg == nil || storedTemplate == "" {
		return ""
	}
	if agent := findAgentByTemplate(cfg, storedTemplate); agent != nil {
		return agent.QualifiedName()
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

// sessionBeadStoredTemplateInfo is the session.Info mirror of sessionBeadStoredTemplate.
func sessionBeadStoredTemplateInfo(i sessionpkg.Info) string {
	storedTemplate := strings.TrimSpace(i.Template)
	if storedTemplate != "" {
		return storedTemplate
	}
	return strings.TrimSpace(i.CommonName)
}

func resolvedTemplateForIdentity(identity string, cfg *config.City) string {
	identity = strings.TrimSpace(identity)
	if cfg == nil || identity == "" {
		return ""
	}
	if agent := findAgentByTemplate(cfg, identity); agent != nil {
		return agent.QualifiedName()
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
		// normalizedSessionTemplate already returns the canonical qualified name
		// when an agent resolves, so this re-normalization is a defensive no-op
		// on that value (and still canonicalizes a non-canonical input).
		return normalizeAgentTemplateIdentity(cfg, template)
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

// resolvedSessionTemplateInfo is the session.Info mirror of resolvedSessionTemplate.
func resolvedSessionTemplateInfo(i sessionpkg.Info, cfg *config.City) string {
	template := normalizedSessionTemplateInfo(i, cfg)
	if template != "" && (cfg == nil || findAgentByTemplate(cfg, template) != nil) {
		return normalizeAgentTemplateIdentity(cfg, template)
	}
	storedTemplate := sessionBeadStoredTemplateInfo(i)
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
	if agentTemplateIdentitiesEquivalent(cfg, storedTemplate, template) {
		return true
	}
	return resolveLegacyPoolTemplate(cfg, storedTemplate) == template
}

func createPoolSessionBead(
	sessFront *sessionpkg.Store,
	template string,
	now time.Time,
	identity poolSessionCreateIdentity,
) (beads.Bead, error) {
	var raw beads.Store
	if sessFront != nil {
		raw = sessFront.Store().Store
	}
	return createPoolSessionBeadWithAlias(raw, template, nil, nil, now, identity, "")
}

// createPoolSessionBeadWithAlias creates a pool session bead and persists its
// session_name. When resolvedTmuxAlias is non-empty, that name is used in
// place of the universal PoolSessionName derivation when the live store and
// config reservation checks allow it. If the alias is already reserved, the
// bead ID is appended as a "-<beadID>" suffix and that fallback is checked too.
func createPoolSessionBeadWithAlias(
	store beads.Store,
	template string,
	cfg *config.City,
	sessionBeads *sessionBeadSnapshot,
	now time.Time,
	identity poolSessionCreateIdentity,
	resolvedTmuxAlias string,
) (beads.Bead, error) {
	if store == nil {
		return beads.Bead{}, fmt.Errorf("session store unavailable for pool template %q", template)
	}
	resolvedTmuxAlias, err := validateResolvedPoolTmuxAlias(template, resolvedTmuxAlias)
	if err != nil {
		return beads.Bead{}, err
	}
	instanceToken := sessionpkg.NewInstanceToken()
	agentName := strings.TrimSpace(identity.AgentName)
	title := targetBasename(template)
	if agentName == "" {
		agentName = template
	} else {
		title = agentName
	}
	explicitID := poolSessionExplicitBeadID(store, instanceToken)
	sessionName := pendingPoolSessionName(template, instanceToken)
	if explicitID != "" {
		sessionName = PoolSessionName(template, explicitID)
	}
	meta := map[string]string{
		"template":                  template,
		"agent_name":                agentName,
		"state":                     string(sessionpkg.StateStartPending),
		"pending_create_claim":      "true",
		"pending_create_started_at": pendingCreateStartedAtNow(now),
		"session_origin":            "ephemeral",
		"generation":                "1",
		"continuation_epoch":        "1",
		"instance_token":            instanceToken,
		"session_name":              sessionName,
		poolManagedMetadataKey:      boolMetadata(true),
	}
	if alias := strings.TrimSpace(identity.Alias); alias != "" {
		meta["alias"] = alias
	}
	if identity.Slot > 0 {
		meta["pool_slot"] = strconv.Itoa(identity.Slot)
	}
	for key, value := range identity.Metadata {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		meta[key] = strings.TrimSpace(value)
	}
	// Durable canonical-identity record (S19 Stage 2, WRITE-ONLY). Stamped AFTER
	// the identity.Metadata copy so a caller-supplied metadata entry can never
	// overwrite the config-resolved record — the canonical record is the one
	// authoritative identity (S2-3 honesty). The identity here is pool-resolved
	// config identity, so it is safe to stamp; agentName is non-empty. Slot is
	// coupled to the name.
	meta[sessionpkg.CanonicalInstanceNameMetadata] = agentName
	if identity.Slot > 0 {
		meta[sessionpkg.CanonicalPoolSlotMetadata] = strconv.Itoa(identity.Slot)
	}
	beadID, err := sessionFrontDoor(store).CreateSession(sessionpkg.CreateSpec{
		ID:        explicitID,
		Title:     title,
		AgentName: agentName,
		Metadata:  meta,
	})
	if err != nil {
		return beads.Bead{}, err
	}
	// S19 Stage 3 shadow: record the legacy canonical-identity stamp on the
	// pool-create path now that the bead ID exists (no-op unless the shadow
	// harness is enabled).
	recordLegacyCompareWrites(beadID, "poolSessionCreate", meta)
	bead, err := store.Get(beadID)
	if err != nil {
		return beads.Bead{}, err
	}
	sessionName, err = derivePoolSessionName(store, cfg, template, bead.ID, resolvedTmuxAlias, sessionBeads)
	if err != nil {
		_ = sessionFrontDoor(store).CloseWithoutReason(bead.ID)
		return beads.Bead{}, err
	}
	if bead.Metadata == nil {
		bead.Metadata = map[string]string{}
	}
	if bead.Metadata["session_name"] != sessionName {
		if err := sessionFrontDoor(store).SetMarker(bead.ID, "session_name", sessionName); err != nil {
			_ = sessionFrontDoor(store).CloseWithoutReason(bead.ID)
			return beads.Bead{}, err
		}
		bead.Metadata["session_name"] = sessionName
	}
	if sessionBeads != nil {
		sessionBeads.add(bead)
	}
	return bead, nil
}

// derivePoolSessionName picks the session_name for a fresh pool bead. When
// resolvedTmuxAlias is non-empty and unreserved in the live store, config, and
// current open snapshot, it wins; otherwise the bead ID is appended as a
// deterministic suffix.
func derivePoolSessionName(store beads.Store, cfg *config.City, template, beadID, resolvedTmuxAlias string, snapshot *sessionBeadSnapshot) (string, error) {
	resolvedTmuxAlias, err := validateResolvedPoolTmuxAlias(template, resolvedTmuxAlias)
	if err != nil {
		return "", err
	}
	if resolvedTmuxAlias == "" {
		return PoolSessionName(template, beadID), nil
	}
	sessionName := resolvedTmuxAlias
	if err := ensurePoolSessionNameAvailable(store, cfg, snapshot, sessionName, beadID); err != nil {
		if !errors.Is(err, sessionpkg.ErrSessionNameExists) {
			return "", fmt.Errorf("checking pool session_name for template %q: %w", template, err)
		}
		sessionName = resolvedTmuxAlias + "-" + beadID
	}
	if _, err := sessionpkg.ValidateExplicitName(sessionName); err != nil {
		return "", fmt.Errorf("derived pool session_name for template %q: %w", template, err)
	}
	if err := ensurePoolSessionNameAvailable(store, cfg, snapshot, sessionName, beadID); err != nil {
		return "", fmt.Errorf("derived pool session_name for template %q: %w", template, err)
	}
	return sessionName, nil
}

func ensurePoolSessionNameAvailable(store beads.Store, cfg *config.City, snapshot *sessionBeadSnapshot, name, selfID string) error {
	if openSessionNameTaken(snapshot, name, selfID) {
		return fmt.Errorf("%w: %q conflicts with live pool snapshot", sessionpkg.ErrSessionNameExists, name)
	}
	return sessionpkg.EnsureSessionNameAvailableWithConfig(store, cfg, name, selfID)
}

func validateResolvedPoolTmuxAlias(template, resolvedTmuxAlias string) (string, error) {
	resolvedTmuxAlias = strings.TrimSpace(resolvedTmuxAlias)
	if resolvedTmuxAlias == "" {
		return "", nil
	}
	validated, err := sessionpkg.ValidateExplicitName(resolvedTmuxAlias)
	if err != nil {
		return "", fmt.Errorf("tmux_alias for pool template %q resolved to invalid session name: %w", template, err)
	}
	return validated, nil
}

// openSessionNameTaken reports whether any open session bead in the snapshot
// (other than selfID) already advertises name as its session_name.
func openSessionNameTaken(snapshot *sessionBeadSnapshot, name, selfID string) bool {
	if snapshot == nil || strings.TrimSpace(name) == "" {
		return false
	}
	for _, b := range snapshot.OpenInfos() {
		if b.ID == selfID {
			continue
		}
		if strings.TrimSpace(b.SessionNameMetadata) == name {
			return true
		}
	}
	return false
}

func poolSessionExplicitBeadID(store beads.Store, instanceToken string) string {
	prefixStore, ok := store.(explicitBeadIDStore)
	if !ok {
		return ""
	}
	prefix := strings.Trim(strings.TrimSpace(prefixStore.IDPrefix()), "-")
	instanceToken = strings.TrimSpace(instanceToken)
	if prefix == "" || instanceToken == "" {
		return ""
	}
	return prefix + "-session-" + instanceToken
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

// sessionBeadAgentNameInfo is the session.Info mirror of sessionBeadAgentName:
// agent_name metadata (untrimmed), then the agent:<name> label fallback.
func sessionBeadAgentNameInfo(i sessionpkg.Info) string {
	if i.AgentName != "" {
		return i.AgentName
	}
	for _, label := range i.Labels {
		if strings.HasPrefix(label, "agent:") {
			return strings.TrimPrefix(label, "agent:")
		}
	}
	return ""
}

// sessionAgentMetricIdentity resolves the stable agent-identity label for the
// gc.agent.* lifecycle counters from a session bead. It mirrors the start
// path's tp.DisplayName() value space so stop and quarantine metrics join the
// start, crash, idle-kill, and max-age-kill counters:
//
//  1. agent_name metadata (the pool instance or qualified agent identity),
//  2. the agent: label (legacy aliased beads),
//  3. the configured pool-instance identity for legacy aliasless pooled beads
//     (namepool-aware via pooledFallbackIdentity when cfg resolves the agent),
//  4. the bare template as a last resort.
//
// cfg may be nil on call paths that only ever see beads carrying agent_name
// (manual kill, handoff); step 3 then degrades to the "<template>-<pool_slot>"
// synthesis, which already joins the start path for non-themed pools.
//
// The runtime session_name is intentionally excluded: it lives in a sanitized
// value space (/ -> --, . -> __) that cannot be joined against the agent
// identity used by starts, crashes, idle kills, and max-age kills.
func sessionAgentMetricIdentity(bead beads.Bead, cfg *config.City) string {
	if identity := sessionBeadAgentName(bead); identity != "" {
		return identity
	}
	if pooled := pooledFallbackIdentity(bead, cfg); pooled != "" {
		return pooled
	}
	return bead.Metadata["template"]
}

// pooledFallbackIdentity reconstructs the start-path instance identity for a
// legacy aliasless pooled session bead (template + pool_slot, no agent_name and
// no agent: label). When cfg resolves the bead's configured agent it reuses
// poolInstanceIdentity — the same derivation buildDesiredState uses for the
// start counter — so a namepool-themed pool instance records its themed
// identity (e.g. "rig/fenrir") instead of a non-joinable "rig/dog-3", and a
// canonical-singleton pool records its base identity instead of a phantom
// "rig/dog-1". Without cfg it falls back to the "<template>-<pool_slot>"
// synthesis, which already joins the start path for non-themed pools. Returns
// "" when the bead carries no pool_slot (it is not a pooled bead).
func pooledFallbackIdentity(bead beads.Bead, cfg *config.City) string {
	template := bead.Metadata["template"]
	slot := bead.Metadata["pool_slot"]
	if template == "" || slot == "" {
		return ""
	}
	if cfg != nil {
		if agent := findAgentByTemplate(cfg, template); agent != nil {
			if n, err := strconv.Atoi(strings.TrimSpace(slot)); err == nil {
				if _, qualifiedInstance := poolInstanceIdentity(agent, n, nil); qualifiedInstance != "" {
					return qualifiedInstance
				}
			}
		}
	}
	return template + "-" + slot
}

// sessionAgentMetricIdentityInfo is the session.Info sibling of
// sessionAgentMetricIdentity, reading typed Info fields instead of raw bead
// metadata. Equivalence-proven.
func sessionAgentMetricIdentityInfo(info sessionpkg.Info, cfg *config.City) string {
	if identity := sessionBeadAgentNameInfo(info); identity != "" {
		return identity
	}
	if pooled := pooledFallbackIdentityInfo(info, cfg); pooled != "" {
		return pooled
	}
	return info.Template
}

// pooledFallbackIdentityInfo is the session.Info sibling of
// pooledFallbackIdentity. Equivalence-proven.
func pooledFallbackIdentityInfo(info sessionpkg.Info, cfg *config.City) string {
	template := info.Template
	slot := info.PoolSlot
	if template == "" || slot == "" {
		return ""
	}
	if cfg != nil {
		if agent := findAgentByTemplate(cfg, template); agent != nil {
			if n, err := strconv.Atoi(strings.TrimSpace(slot)); err == nil {
				if _, qualifiedInstance := poolInstanceIdentity(agent, n, nil); qualifiedInstance != "" {
					return qualifiedInstance
				}
			}
		}
	}
	return template + "-" + slot
}

// sessionAgentMetricIdentityByName resolves the gc.agent.* identity label for a
// session referenced by its runtime session name, loading the session bead to
// read its identity metadata. Returns "" when the store is unavailable or the
// bead cannot be resolved. The handoff caller operates on a named session whose
// bead carries agent_name, so the namepool-aware pooled fallback is unreachable
// and cfg is intentionally nil here.
func sessionAgentMetricIdentityByName(store beads.Store, sessionName string) string {
	if store == nil {
		return ""
	}
	id, err := resolveSessionID(store, sessionName)
	if err != nil {
		return ""
	}
	bead, err := store.Get(id)
	if err != nil {
		return ""
	}
	return sessionAgentMetricIdentity(bead, nil)
}

func normalizedSessionTemplate(bead beads.Bead, cfg *config.City) string {
	template := bead.Metadata["template"]
	if cfg == nil {
		return template
	}
	if template != "" {
		if agent := findAgentByTemplate(cfg, template); agent != nil {
			return agent.QualifiedName()
		}
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

// normalizedSessionTemplateInfo is the session.Info mirror of normalizedSessionTemplate.
func normalizedSessionTemplateInfo(i sessionpkg.Info, cfg *config.City) string {
	template := i.Template
	if cfg == nil {
		return template
	}
	if template != "" {
		if agent := findAgentByTemplate(cfg, template); agent != nil {
			return agent.QualifiedName()
		}
	}
	agentName := sessionBeadAgentNameInfo(i)
	if agentName != "" {
		if resolved := resolvedTemplateForIdentity(agentName, cfg); resolved != "" {
			return resolved
		}
	}
	if resolved := resolvedTemplateForIdentity(strings.TrimSpace(i.Alias), cfg); resolved != "" {
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
	return chooseSessionNameForTemplate(store, items, true, "", "", template)
}

func findSessionNameByMetadata(store beads.Store, key, value string, agentNameMatch bool) string {
	items, err := sessionpkg.ExactMetadataSessionCandidates(store, false, map[string]string{key: value})
	if err != nil {
		return ""
	}
	return chooseSessionNameForTemplate(store, items, agentNameMatch, key, value, value)
}

func chooseSessionNameForTemplate(store beads.Store, items []beads.Bead, agentNameMatch bool, key, value, queryTemplate string) string {
	var fallback string
	var canonicalPoolFallback string
	for _, b := range items {
		if !sessionpkg.IsSessionBeadOrRepairable(b) || b.Status == "closed" {
			continue
		}
		sessionpkg.RepairEmptyType(store, &b)
		if key != "" && strings.TrimSpace(b.Metadata[key]) != value {
			continue
		}
		canonicalPoolManaged := isCanonicalPoolManagedSessionBeadForTemplate(b, queryTemplate)
		if agentNameMatch && isPoolManagedSessionBead(b) && sessionBeadAgentName(b) == b.Metadata["template"] && !canonicalPoolManaged {
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
		if canonicalPoolManaged {
			if canonicalPoolFallback == "" {
				canonicalPoolFallback = sessionName
			}
			continue
		}
		if fallback == "" {
			fallback = sessionName
		}
	}
	if fallback == "" {
		return canonicalPoolFallback
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
	case "creating", string(sessionpkg.StateStartPending):
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
	all, err := sessionpkg.ListAllSessionBeads(store, beads.ListQuery{})
	if err != nil {
		return result, err
	}
	for _, b := range all {
		// ListAllSessionBeads already filters via IsSessionBeadOrRepairable.
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
		if cfgAgent != nil && poolSlotHasConfiguredBound(cfgAgent) && !cfgAgent.UsesCanonicalSingletonPoolIdentity() {
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
		canonicalPoolManaged := cfgAgent.UsesCanonicalSingletonPoolIdentity() && isCanonicalPoolManagedSessionBeadForTemplate(b, template)
		staleCanonicalSingletonSlot := 0
		if cfgAgent.UsesCanonicalSingletonPoolIdentity() && isPoolManagedSessionBead(b) && !canonicalPoolManaged {
			switch {
			case agentSlot > 0:
				staleCanonicalSingletonSlot = agentSlot
			case aliasSlot > 0:
				staleCanonicalSingletonSlot = aliasSlot
			case sessionNameSlot > 0:
				staleCanonicalSingletonSlot = sessionNameSlot
			default:
				if slot, err := strconv.Atoi(strings.TrimSpace(b.Metadata["pool_slot"])); err == nil && slot > 0 {
					staleCanonicalSingletonSlot = slot
				}
			}
			if staleCanonicalSingletonSlot == 0 {
				continue
			}
		}
		switch {
		case canonicalPoolManaged:
			agentName = template
		case staleCanonicalSingletonSlot > 0:
			agentName = qualifiedInstanceName(staleCanonicalSingletonSlot)
		case storedTemplateMatches && (agentName == template || agentName == targetBasename(template)):
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

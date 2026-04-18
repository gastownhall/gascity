package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const poolManagedMetadataKey = "pool_managed"

func isPoolManagedSessionBead(bead beads.Bead) bool {
	if isEphemeralSessionBead(bead) {
		return true
	}
	if strings.TrimSpace(bead.Metadata[poolManagedMetadataKey]) == boolMetadata(true) {
		return true
	}
	return strings.TrimSpace(bead.Metadata["pool_slot"]) != ""
}

func createPoolSessionBead(
	store beads.Store,
	template string,
	sessionBeads *sessionBeadSnapshot,
) (beads.Bead, error) {
	if store == nil {
		return beads.Bead{}, fmt.Errorf("session store unavailable for pool template %q", template)
	}
	meta := map[string]string{
		"template":             template,
		"agent_name":           template,
		"state":                "creating",
		"pending_create_claim": "true",
		"session_origin":       "ephemeral",
		"generation":           "1",
		"continuation_epoch":   "1",
		"instance_token":       sessionpkg.NewInstanceToken(),
		poolManagedMetadataKey: boolMetadata(true),
	}
	bead, err := store.Create(beads.Bead{
		Title:    targetBasename(template),
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "agent:" + template},
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
		if resolved := resolveAgentTemplate(agentName, cfg); resolved != "" && findAgentByTemplate(cfg, resolved) != nil {
			return resolved
		}
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
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		return ""
	}
	return snapshot.FindSessionNameByTemplate(template)
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
func lookupPoolSessionNames(store beads.Store, template string) (map[string]string, error) {
	result := make(map[string]string)
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
		if b.Status == "closed" || b.Metadata["pool_slot"] == "" {
			continue
		}
		agentName := sessionBeadAgentName(b)
		if b.Metadata["template"] != template && resolvePoolSlot(agentName, template) == 0 {
			continue
		}
		sessionName := b.Metadata["session_name"]
		if sessionName == "" {
			continue
		}
		if agentName == "" {
			agentName = template + "-" + b.Metadata["pool_slot"]
		}
		if agentName != "" {
			result[agentName] = sessionName
		}
	}
	return result, nil
}

package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func sessionOrigin(bead beads.Bead) string {
	origin := strings.TrimSpace(bead.Metadata["session_origin"])
	if origin != "" {
		return origin
	}
	if isNamedSessionBead(bead) {
		return "named"
	}
	if isManualSessionBead(bead) {
		return "manual"
	}
	if strings.TrimSpace(bead.Metadata[poolManagedMetadataKey]) == boolMetadata(true) {
		return "ephemeral"
	}
	if strings.TrimSpace(bead.Metadata["pool_slot"]) != "" {
		return "ephemeral"
	}
	if strings.TrimSpace(bead.Metadata["dependency_only"]) == boolMetadata(true) {
		return "ephemeral"
	}
	template := strings.TrimSpace(bead.Metadata["template"])
	if template != "" {
		if slot := resolvePoolSlot(strings.TrimSpace(sessionBeadAgentName(bead)), template); slot > 0 {
			return "ephemeral"
		}
		if slot := resolvePoolSlot(strings.TrimSpace(bead.Metadata["session_name"]), template); slot > 0 {
			return "ephemeral"
		}
	}
	return ""
}

func isEphemeralSessionBead(bead beads.Bead) bool {
	return sessionOrigin(bead) == "ephemeral"
}

// sessionOriginInfo is the session.Info mirror of sessionOrigin: a
// field-for-field projection over the typed Info instead of raw bead metadata.
func sessionOriginInfo(i sessionpkg.Info) string {
	origin := strings.TrimSpace(i.SessionOrigin)
	if origin != "" {
		return origin
	}
	if isNamedSessionInfo(i) {
		return "named"
	}
	if isManualSessionInfo(i) {
		return "manual"
	}
	if i.PoolManaged {
		return "ephemeral"
	}
	if strings.TrimSpace(i.PoolSlot) != "" {
		return "ephemeral"
	}
	if i.DependencyOnly {
		return "ephemeral"
	}
	template := strings.TrimSpace(i.Template)
	if template != "" {
		if slot := resolvePoolSlot(strings.TrimSpace(sessionBeadAgentNameInfo(i)), template); slot > 0 {
			return "ephemeral"
		}
		if slot := resolvePoolSlot(strings.TrimSpace(i.SessionNameMetadata), template); slot > 0 {
			return "ephemeral"
		}
	}
	return ""
}

// isEphemeralSessionInfo is the session.Info mirror of isEphemeralSessionBead.
func isEphemeralSessionInfo(i sessionpkg.Info) bool {
	return sessionOriginInfo(i) == "ephemeral"
}

// Legacy pooled sessions created before manual-session origin backfill were
// persisted as session_origin="ephemeral" even though they were user-created.
// Pool-managed controller beads always stamp pool_managed/pool_slot, so a
// multi-session bead with ephemeral origin but without those markers is the
// upgrade shape we need to preserve and migrate.
func isLegacyManualSessionBeadForAgent(bead beads.Bead, cfgAgent *config.Agent) bool {
	if cfgAgent == nil || !cfgAgent.SupportsMultipleSessions() {
		return false
	}
	if strings.TrimSpace(bead.Metadata["session_origin"]) != "ephemeral" {
		return false
	}
	if isNamedSessionBead(bead) {
		return false
	}
	if strings.TrimSpace(bead.Metadata[poolManagedMetadataKey]) == boolMetadata(true) {
		return false
	}
	if strings.TrimSpace(bead.Metadata["pool_slot"]) != "" {
		return false
	}
	return strings.TrimSpace(bead.Metadata["dependency_only"]) != boolMetadata(true)
}

func isManualSessionBeadForAgent(bead beads.Bead, cfgAgent *config.Agent) bool {
	return isManualSessionBead(bead) || isLegacyManualSessionBeadForAgent(bead, cfgAgent)
}

func isEphemeralSessionBeadForAgent(bead beads.Bead, cfgAgent *config.Agent) bool {
	if isEphemeralSessionBead(bead) {
		return true
	}
	if cfgAgent == nil || !cfgAgent.SupportsInstanceExpansion() {
		return false
	}
	if isNamedSessionBead(bead) || isManualSessionBead(bead) {
		return false
	}
	return existingPoolSlot(cfgAgent, bead) > 0
}

// isEphemeralSessionInfoForAgent is the session.Info sibling of
// isEphemeralSessionBeadForAgent, reading Info classifiers instead of raw beads.
// Equivalence-proven.
func isEphemeralSessionInfoForAgent(info sessionpkg.Info, cfgAgent *config.Agent) bool {
	if isEphemeralSessionInfo(info) {
		return true
	}
	if cfgAgent == nil || !cfgAgent.SupportsInstanceExpansion() {
		return false
	}
	if isNamedSessionInfo(info) || isManualSessionInfo(info) {
		return false
	}
	return existingPoolSlotInfo(cfgAgent, info) > 0
}

// isLegacyManualSessionInfoForAgent is the session.Info sibling of
// isLegacyManualSessionBeadForAgent, reading typed Info fields instead of raw
// bead metadata. Equivalence-proven.
func isLegacyManualSessionInfoForAgent(info sessionpkg.Info, cfgAgent *config.Agent) bool {
	if cfgAgent == nil || !cfgAgent.SupportsMultipleSessions() {
		return false
	}
	if strings.TrimSpace(info.SessionOrigin) != "ephemeral" {
		return false
	}
	if isNamedSessionInfo(info) {
		return false
	}
	if info.PoolManaged {
		return false
	}
	if strings.TrimSpace(info.PoolSlot) != "" {
		return false
	}
	return !info.DependencyOnly
}

// isManualSessionInfoForAgent is the session.Info sibling of
// isManualSessionBeadForAgent. Equivalence-proven.
func isManualSessionInfoForAgent(info sessionpkg.Info, cfgAgent *config.Agent) bool {
	return isManualSessionInfo(info) || isLegacyManualSessionInfoForAgent(info, cfgAgent)
}

func templateParamsSessionOrigin(tp TemplateParams) string {
	switch {
	case strings.TrimSpace(tp.ConfiguredNamedIdentity) != "":
		return "named"
	case tp.ManualSession:
		return "manual"
	default:
		return "ephemeral"
	}
}

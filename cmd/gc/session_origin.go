package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

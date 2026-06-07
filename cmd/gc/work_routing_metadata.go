package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

func legacyWorkflowRunTarget(b beads.Bead) string {
	if strings.TrimSpace(b.Metadata["gc.kind"]) != "workflow" {
		return ""
	}
	if strings.TrimSpace(b.Metadata["gc.routed_to"]) != "" {
		return ""
	}
	return strings.TrimSpace(b.Metadata["gc.run_target"])
}

func routedToOrLegacyWorkflowTarget(b beads.Bead) string {
	if routedTo := strings.TrimSpace(b.Metadata["gc.routed_to"]); routedTo != "" {
		return routedTo
	}
	return legacyWorkflowRunTarget(b)
}

func routedToAndLegacyWorkflowCandidates(b beads.Bead) []string {
	routedTo := strings.TrimSpace(b.Metadata["gc.routed_to"])
	legacy := legacyWorkflowRunTarget(b)
	if routedTo == "" {
		if legacy == "" {
			return nil
		}
		return []string{legacy}
	}
	return []string{routedTo}
}

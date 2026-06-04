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

// WorkRoutingModel returns the advisory per-dispatch model choice carried by a
// work bead in its "gc.model" metadata, or "" when none is set. This is the
// key the model-advisor pack and the mol-review-quorum formula already write;
// it is consumed at session spawn so an advised model applies per task/shape
// rather than only per agent. The value is a provider OptionsSchema "model"
// choice value (e.g. "opus", "sonnet"); validation against the resolved
// provider's schema happens at the spawn site.
func WorkRoutingModel(b beads.Bead) string {
	return strings.TrimSpace(b.Metadata["gc.model"])
}

package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func legacyWorkflowRunTarget(b beads.Bead) string {
	if strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]) != beadmeta.KindWorkflow {
		return ""
	}
	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
		return ""
	}
	return strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey])
}

func routedToOrLegacyWorkflowTarget(b beads.Bead) string {
	if routedTo := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]); routedTo != "" {
		return routedTo
	}
	return legacyWorkflowRunTarget(b)
}

func routedToAndLegacyWorkflowCandidates(b beads.Bead) []string {
	routedTo := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
	legacy := legacyWorkflowRunTarget(b)
	if routedTo == "" {
		if legacy == "" {
			return nil
		}
		return []string{legacy}
	}
	return []string{routedTo}
}

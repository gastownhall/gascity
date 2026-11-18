package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// awakeWorkflowRoot returns the molecule a work bead belongs to: a step carries
// gc.root_bead_id; a workflow root (the bead that carries gc.input_convoy_id)
// is its own root. Standalone beads have no root.
func awakeWorkflowRoot(wb beads.Bead) string {
	if root := strings.TrimSpace(wb.Metadata[beadmeta.RootBeadIDMetadataKey]); root != "" {
		return root
	}
	if strings.TrimSpace(wb.Metadata[beadmeta.InputConvoyIDMetadataKey]) != "" {
		return wb.ID
	}
	return ""
}

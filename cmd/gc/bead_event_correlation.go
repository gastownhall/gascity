package main

import (
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// stampBeadSnapshotCorrelation copies the typed run/session/step join keys
// from an authoritative bead snapshot onto an event envelope. Export and
// accounting consumers intentionally read these typed fields rather than
// decoding free-form payload metadata.
func stampBeadSnapshotCorrelation(event *events.Event, bead beads.Bead) {
	if event == nil {
		return
	}
	event.RunID = beadmeta.ResolveRunID(bead.Metadata, bead.ID, "")
	event.SessionID = bead.Metadata[beadmeta.SessionIDMetadataKey]
	event.StepID = bead.Metadata[beadmeta.StepIDMetadataKey]
}

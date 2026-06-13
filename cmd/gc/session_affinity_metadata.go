package main

import (
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

var routedWorkSessionAffinityMetadataKeys = []string{
	beadmeta.SessionAffinityMetadataKey,
	beadmeta.ContinuationGroupMetadataKey,
}

func withClearedSessionAffinityMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		metadata = make(map[string]string, len(routedWorkSessionAffinityMetadataKeys))
	}
	for _, key := range routedWorkSessionAffinityMetadataKeys {
		metadata[key] = ""
	}
	return metadata
}

func clearSessionAffinityMetadataOnBead(store beads.Store, beadID string) error {
	for _, key := range routedWorkSessionAffinityMetadataKeys {
		if err := store.SetMetadata(beadID, key, ""); err != nil {
			return err
		}
	}
	return nil
}

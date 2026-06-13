package main

import "github.com/gastownhall/gascity/internal/beads"

var routedWorkSessionAffinityMetadataKeys = []string{
	"gc.session_affinity",
	"gc.continuation_group",
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

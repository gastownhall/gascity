package main

import (
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// withClearedSessionAffinityMetadata returns metadata with every
// beadmeta.SessionAffinityMetadataKeys entry set to the empty string,
// allocating the map when nil. cmd/gc clears affinity by persisting an empty
// value rather than deleting the key (as internal/dispatch does) because these
// helpers feed beads.UpdateOpts.Metadata, whose merge only touches supplied
// keys. Every consumer treats the keys as absent when strings.TrimSpace is
// empty, so empty-value and deleted are equivalent.
func withClearedSessionAffinityMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		metadata = make(map[string]string, len(beadmeta.SessionAffinityMetadataKeys))
	}
	for _, key := range beadmeta.SessionAffinityMetadataKeys {
		metadata[key] = ""
	}
	return metadata
}

// clearSessionAffinityMetadataOnBead persists an empty value for every
// session-affinity key on beadID. See withClearedSessionAffinityMetadata for
// why cmd/gc clears by empty value rather than key deletion.
func clearSessionAffinityMetadataOnBead(store beads.Store, beadID string) error {
	for _, key := range beadmeta.SessionAffinityMetadataKeys {
		if err := store.SetMetadata(beadID, key, ""); err != nil {
			return err
		}
	}
	return nil
}

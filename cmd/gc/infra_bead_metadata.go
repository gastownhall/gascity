package main

import (
	"maps"

	"github.com/gastownhall/gascity/internal/beads"
)

// cloneBeadMetadata returns a writable copy of metadata that shares no
// storage with it, so a caller can add keys without mutating the bead it was
// read from; nil metadata yields an empty map.
func cloneBeadMetadata(metadata beads.StringMap) beads.StringMap {
	if metadata == nil {
		return beads.StringMap{}
	}
	return maps.Clone(metadata)
}

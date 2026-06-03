//go:build !cgo || !gascity_native_beads

package main

import "github.com/gastownhall/gascity/internal/beads"

func openOptimizedDoltliteStore(_ string, _ *beads.BdStore) (beads.Store, bool) {
	return nil, false
}

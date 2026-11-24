package worker

import "github.com/gastownhall/gascity/internal/sessionlog"

// Compile-only scaffolding for ga-9iikxat3: the builder implements the approved
// outcome mapping and wires the existing discovery and settle sites.
func codexKeyedDiscovery(_ sessionlog.CodexKeyedLookup) (path string, scanClean, emptyIsPermanent bool) {
	return "", false, false
}

func sweepMissIsFinal(_ string, _, _ bool) bool {
	return false
}

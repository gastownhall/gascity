package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// proxiedScopeNoOpMessage is the one line every managed-Dolt verb prints when
// it is asked to operate on a scope bd owns. The dolt pack's orders fire on
// every city, including proxied ones, so the verbs have to be no-ops there
// rather than failures.
const proxiedScopeNoOpMessage = "dolt lifecycle is owned by bd for proxied scopes; nothing to do"

// cleanupSkipReasonProxiedScope tags the gc.dolt.cleanup.v1 skip envelope so
// automation can tell "bd owns this scope" apart from "nothing was found".
const cleanupSkipReasonProxiedScope = "bd-owned-proxied-scope"

// doltScopeIsBdOwnedProxied reports whether a scope's persisted beads metadata
// says bd owns its Dolt topology: backend dolt in proxied-server mode. Only
// the persisted binding counts — a scope with no beads metadata is not yet
// bound and stays under the managed-Dolt lens.
//
// LOCAL PREDICATE: this is deliberately narrower than
// scopeUsesProxiedDoltMode, which also applies the fresh-scope proxied-local
// default. Replace it with the canonical provider-ownership predicate once
// that lands (slice 1 merge step).
func doltScopeIsBdOwnedProxied(scopeRoot string) bool {
	metadataPath := filepath.Join(scopeRoot, ".beads", "metadata.json")
	backend, ok, err := contract.ReadMetadataBackend(fsys.OSFS{}, metadataPath)
	if err != nil || !ok || !contract.IsDoltBackend(backend) {
		return false
	}
	mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, metadataPath)
	if err != nil || !ok {
		return false
	}
	return contract.IsProxiedDoltMode(backend, mode)
}

// newProxiedScopeCleanupSkipReport builds the typed zero envelope a
// bd-owned proxied scope reports instead of running any cleanup stage.
func newProxiedScopeCleanupSkipReport() CleanupReport {
	return CleanupReport{
		OK:     true,
		Schema: CleanupSchemaVersion,
		Skipped: &CleanupSkipped{
			Reason:  cleanupSkipReasonProxiedScope,
			Message: proxiedScopeNoOpMessage,
		},
	}
}

// emitProxiedScopeCleanupSkip writes the no-op report in whichever shape the
// caller asked for. It probes nothing and writes no managed-Dolt state.
func emitProxiedScopeCleanupSkip(jsonOut bool, stdout io.Writer) {
	if jsonOut {
		emitReport(newProxiedScopeCleanupSkipReport(), PortResolution{}, cleanupOptions{JSON: true}, stdout, io.Discard)
		return
	}
	fmt.Fprintf(stdout, "%s\n", proxiedScopeNoOpMessage) //nolint:errcheck
}

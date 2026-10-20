package main

import (
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// bdWriteGuardNeedsRead reports whether a gc bd write naming writeIDs must
// read its target beads before bd runs. The read has two consumers. The
// exact-ID collision check (gcy-g4o) needs it wherever bd may resolve a
// write's id to a different bead. A scope whose bd resolves prefixed ids
// exactly cannot produce that collision for a write whose ids are all
// prefixed, but its bd still rewrites an unprefixed one
// (contract.LooksLikePrefixedID). The close-shaped writes (close, or update
// to closed) need the beads themselves, for the work-record close gate and
// the claim-identity close, whatever the scope.
func bdWriteGuardNeedsRead(bdArgs, writeIDs []string, scopeRoot string) bool {
	if _, closes := workRecordCloseTargets(bdArgs); closes {
		return true
	}
	for _, id := range writeIDs {
		if !contract.LooksLikePrefixedID(id) {
			return true
		}
	}
	return !bdScopeResolvesIDsExactly(scopeRoot)
}

// bdScopeResolvesIDsExactly reports whether the bd serving scopeRoot resolves
// every prefixed id argument exactly, per the backend its own
// .beads/metadata.json names. A scope with no readable marker of its own — a
// redirect, or one that inherits its city's backend — reports false and keeps
// the guard.
func bdScopeResolvesIDsExactly(scopeRoot string) bool {
	backend, named, err := contract.ReadMetadataBackend(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "metadata.json"))
	return err == nil && named && contract.BackendResolvesIDsExactly(backend)
}

// bdWriteGuardStore is the store the exact-ID write guard reads through: the
// scope's plain bd front door. The collision the guard exists for is bd's own
// prefix-match resolution of the id the write will send, so only bd's resolver
// can show it. A native or SQL-optimized store reads ids exactly, reports such
// an id absent, and so lets the write through to the bead bd then matches. It
// is also the cheaper read: one bd show, where opening a native-eligible Dolt
// scope costs a preflight and a library open first.
func bdWriteGuardStore(scopeRoot, cityPath string, cfg *config.City) beads.Store {
	if filepath.Clean(scopeRoot) == filepath.Clean(cityPath) {
		return bdStoreForCityWithConfig(scopeRoot, cityPath, cfg)
	}
	return bdStoreForRig(scopeRoot, cityPath, cfg)
}

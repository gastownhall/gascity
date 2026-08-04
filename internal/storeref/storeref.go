// Package storeref resolves a bead id to the store that physically owns it,
// across the work store and the relocated coordination-class SQLite stores.
//
// It is the standalone successor to coordrouter.Router's by-id read federation
// (prefixBackendForID + Get): the Router is the live graph_store=sqlite wiring
// today and is retired in the final phase of the infra/beads split, so this
// package carries the same routing forward over an explicit []beads.Store the
// caller assembles, with no central Router. Bead ids are prefix-disjoint across
// stores (reserved class prefixes are kept off HQ/rig work-store prefixes by
// config.ValidateRigs), so the owning store is the sole residence.
package storeref

import (
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// ScopeRigContext returns the rig identity encoded by a canonical workflow
// store reference and reports whether the reference identifies a known scope.
// City refs return an empty rig context with ok=true; rig refs return the rig
// name. Unknown, legacy-bare, and incomplete refs return ok=false so callers
// can apply an explicit compatibility fallback rather than mistaking them for
// the city store.
func ScopeRigContext(storeRef string) (rigContext string, ok bool) {
	storeRef = strings.TrimSpace(storeRef)
	switch {
	case strings.HasPrefix(storeRef, "city:"):
		return "", strings.TrimSpace(strings.TrimPrefix(storeRef, "city:")) != ""
	case strings.HasPrefix(storeRef, "rig:"):
		rigContext = strings.TrimSpace(strings.TrimPrefix(storeRef, "rig:"))
		return rigContext, rigContext != ""
	default:
		return "", false
	}
}

// HasIDPrefix is the optional accessor a store implements to declare the id
// prefix it mints (SQLiteStore, BdStore, CachingStore implement it; the bd/Dolt
// work store reports its configured prefix or "").
type HasIDPrefix interface {
	IDPrefix() string
}

// PrefixOwner returns the store whose IDPrefix() owns id's namespace
// (strings.HasPrefix(id, prefix+"-")), or nil when none claims it. It routes
// purely on the static id prefix and never reads a store. Mirrors
// coordrouter.Router.prefixBackendForID. nil stores and stores without an
// IDPrefix() (or an empty one, e.g. the work store) are skipped.
func PrefixOwner(id string, stores []beads.Store) beads.Store {
	for _, s := range stores {
		if s == nil {
			continue
		}
		if p, ok := s.(HasIDPrefix); ok {
			if pfx := p.IDPrefix(); pfx != "" && strings.HasPrefix(id, pfx+"-") {
				return s
			}
		}
	}
	return nil
}

// Resolve federates a point read: a bead lives in exactly one store, so it tries
// the prefix owner first (the cheap, fork-free path) and falls back to probing
// every store in turn, returning the first hit. It preserves the first hard
// (non-ErrNotFound) read failure seen across the owner probe and the fallback
// scan, returning beads.ErrNotFound only when every probe was a clean not-found.
// A hard failure means an authoritative store was unavailable, which must never
// be flattened into a clean miss — otherwise an unreachable owner store looks
// identical to a deleted bead. Mirrors coordrouter.Router.Get's multi-backend
// body, so it is a drop-in for the Router's by-id read once the Router is deleted.
func Resolve(id string, stores []beads.Store) (beads.Bead, error) {
	var firstErr error
	recordErr := func(err error) {
		if firstErr == nil && err != nil && !errors.Is(err, beads.ErrNotFound) {
			firstErr = err
		}
	}
	if owner := PrefixOwner(id, stores); owner != nil {
		got, err := owner.Get(id)
		if err == nil {
			return got, nil
		}
		// Owner miss (unknown prefix / partial migration) or hard failure: keep
		// the first hard error and fall through to the full probe below so
		// correctness is never reduced.
		recordErr(err)
	}
	for _, s := range stores {
		if s == nil {
			continue
		}
		got, err := s.Get(id)
		if err == nil {
			return got, nil
		}
		recordErr(err)
	}
	if firstErr != nil {
		return beads.Bead{}, firstErr
	}
	return beads.Bead{}, beads.ErrNotFound
}

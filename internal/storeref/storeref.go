// Package storeref resolves a bead id to the store that physically owns it,
// across the work store and the relocated coordination-class SQLite stores.
//
// It is the standalone successor to coordrouter.Router's by-id read federation
// (prefixBackendForID + Get): the Router is the live graph_store=sqlite wiring
// today and is retired in the final phase of the infra/beads split, so this
// package carries the same routing forward over an explicit []beads.Store the
// caller assembles, with no central Router. Bead ids are prefix-disjoint across
// stores (enforced by the class-prefix disjointness guard and
// config.ValidateReservedPrefixesIn), so the owning store is the sole residence.
package storeref

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

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
// every store in turn, returning the first hit. It returns beads.ErrNotFound when
// no store has the bead and every probe was a clean not-found. Mirrors
// coordrouter.Router.Get's multi-backend body, so it is a drop-in for the
// Router's by-id read once the Router is deleted.
func Resolve(id string, stores []beads.Store) (beads.Bead, error) {
	if owner := PrefixOwner(id, stores); owner != nil {
		if got, err := owner.Get(id); err == nil {
			return got, nil
		}
		// Owner miss (unknown prefix / partial migration): fall through to the
		// full probe below so correctness is never reduced.
	}
	var lastErr error
	for _, s := range stores {
		if s == nil {
			continue
		}
		got, err := s.Get(id)
		if err == nil {
			return got, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return beads.Bead{}, lastErr
	}
	return beads.Bead{}, beads.ErrNotFound
}

// Package storeref resolves a bead id to the store that physically owns it,
// across the work store and the relocated coordination-class SQLite stores.
//
// It is the standalone successor to coordrouter.Router's by-id read federation
// (prefixBackendForID + Get): the Router is the live graph_store=sqlite wiring
// today and is retired in the final phase of the infra/beads split, so this
// package carries the same routing forward over an explicit []beads.Store the
// caller assembles, with no central Router.
//
// # Ids are NOT prefix-disjoint across stores
//
// A bead is resident in exactly one store, but its id namespace can be claimed
// by more than one: config.ValidateRigs rejects duplicate prefixes and nothing
// else, and it deliberately does NOT reject a reserved coordination-class
// prefix on an HQ or rig work store — config.ReservedPrefixWarnings warns and
// allows it. So a work store can mint and hold ids inside a relocated class's
// namespace, and every resolver here PROBES a candidate list rather than
// assuming one store owns a prefix outright.
//
// # Two namespace predicates, deliberately different
//
//   - IDInNamespace (class_candidates.go) matches a CONFIGURED prefix — a
//     rig/HQ prefix or a reserved class prefix — and admits the bare id, because
//     a configured prefix can be a whole id.
//   - PrefixOwner matches a store's SELF-DECLARED IDPrefix() and requires the
//     "prefix-" separator, because a store that mints "gcg-1" never mints "gcg".
//
// Collapsing them would let a bare id capture a store that cannot hold it. Use
// the one whose prefix source matches where your prefix came from.
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
//
// A "class:<token>" binding ref is CITY scope. A binding serves the whole
// city's relocated classes and belongs to no rig, so it answers exactly as
// "city:<name>" does — which is also what it answered before the census named
// it as a leg of its own, when the reconciler's leading arm WAS the binding and
// recorded it under the city ref. Reporting it scope-less instead would make
// every binding-resident row invisible to the scope comparisons
// (rootStoreRefMatchesCandidate and its siblings): the census would gain the
// leg and lose the rows in the same commit.
func ScopeRigContext(storeRef string) (rigContext string, ok bool) {
	storeRef = strings.TrimSpace(storeRef)
	switch {
	case strings.HasPrefix(storeRef, "city:"):
		return "", strings.TrimSpace(strings.TrimPrefix(storeRef, "city:")) != ""
	case strings.HasPrefix(storeRef, classRefPrefix):
		return "", strings.TrimSpace(strings.TrimPrefix(storeRef, classRefPrefix)) != ""
	case strings.HasPrefix(storeRef, "rig:"):
		rigContext = strings.TrimSpace(strings.TrimPrefix(storeRef, "rig:"))
		return rigContext, rigContext != ""
	default:
		return "", false
	}
}

// IsClassRef reports whether a store-ref names a relocated class binding. It is
// the predicate the scope-vocabulary normalizers outside this package key on,
// so "class:" appears as a literal in exactly one file.
func IsClassRef(storeRef string) bool {
	storeRef = strings.TrimSpace(storeRef)
	return strings.HasPrefix(storeRef, classRefPrefix) &&
		strings.TrimSpace(strings.TrimPrefix(storeRef, classRefPrefix)) != ""
}

// HasIDPrefix is the optional accessor a store implements to declare the id
// prefix it mints (SQLiteStore, BdStore, CachingStore implement it; the bd/Dolt
// work store reports its configured prefix or "").
type HasIDPrefix interface {
	IDPrefix() string
}

// MintsInsideNamespace reports whether store declares a mint prefix that is one
// of prefixes — the boot-time check a topology constructor sets
// ClassBinding.MintsReserved from.
//
// It is a comparison of declarations, not a read: the store names the namespace
// it mints into and the binding names the namespaces it claims, and when they
// agree a bead this binding creates from now on is recognizable from its id
// alone. A store that declares nothing has verified nothing and reports false,
// which is the conservative answer — a false mint bit only keeps the residence
// probe, while a wrong true one retires it over beads it cannot recognize.
func MintsInsideNamespace(store beads.Store, prefixes []string) bool {
	declaring, ok := store.(HasIDPrefix)
	if !ok {
		return false
	}
	minted := strings.TrimSpace(declaring.IDPrefix())
	if minted == "" {
		return false
	}
	for _, prefix := range prefixes {
		if strings.TrimSpace(prefix) == minted {
			return true
		}
	}
	return false
}

// PrefixOwner returns the store whose IDPrefix() owns id's namespace
// (strings.HasPrefix(id, prefix+"-")), or nil when none claims it. It routes
// purely on the static id prefix and never reads a store. Mirrors
// coordrouter.Router.prefixBackendForID. nil stores and stores without an
// IDPrefix() (or an empty one, e.g. the work store) are skipped.
//
// This is the SELF-DECLARED-prefix predicate, and it requires the "prefix-"
// separator. For a prefix that came from config — a rig/HQ prefix, or a reserved
// class prefix — use IDInNamespace instead; it also admits the bare id, which
// this rule must not. See the package doc.
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

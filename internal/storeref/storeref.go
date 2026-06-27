// Package storeref resolves which bead store owns a given bead ID across an
// ordered set of stores. It unifies two resolution strategies that the city
// (cmd/gc) and the HTTP API (internal/api) had each duplicated:
//
//   - Prefix routing (PrefixOwner): pick the store whose configured ID prefix
//     owns the ID's namespace, using a longest-prefix match. This is a pure
//     function of the static ID and the supplied prefixes; it never reads a
//     store.
//   - Probe federation (Resolve): a bead lives in exactly one store, so try
//     each store's Get(id) in order and return the first hit.
//
// The helpers are deliberately free functions over caller-supplied data so the
// resolved store is the exact same concrete beads.Store value the caller would
// have used. No wrapping adapter is interposed, so downstream optional-capability
// type assertions on the returned store still succeed.
package storeref

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// PrefixMatch selects how a bead ID is tested against a candidate's prefix.
//
// The two modes preserve a real divergence between existing callers:
//
//   - ExactOrHyphen matches when the ID equals the prefix exactly OR begins with
//     "<prefix>-". internal/api/handler_beads.go uses this mode.
//   - HyphenOnly matches only when the ID begins with "<prefix>-"; a bare ID
//     equal to the prefix does NOT match. cmd/gc/api_state.go uses this mode.
//
// The mode is a parameter, not a package default, so each caller keeps its exact
// current behavior.
type PrefixMatch int

const (
	// ExactOrHyphen matches id == prefix OR id has the "prefix-" namespace.
	ExactOrHyphen PrefixMatch = iota
	// HyphenOnly matches only the "prefix-" namespace, never a bare id == prefix.
	HyphenOnly
)

// Candidate pairs a configured ID prefix with the store that owns it. Callers
// supply the prefix from their own configuration (the rig/city effective
// prefix), not from the store, so prefix routing stays a pure function of
// static configuration and does not depend on the store reporting its own
// prefix.
type Candidate struct {
	// Prefix is the configured bead ID prefix the store owns, without a
	// trailing "-". An empty prefix never matches and is skipped.
	Prefix string
	// Store is the store returned verbatim when this candidate's prefix wins.
	// It may be nil: a prefix can be configured before its store is loaded, and
	// callers distinguish "no prefix owns this id" from "the owning prefix has
	// no loaded store" via the matched return value of PrefixOwner.
	Store beads.Store
}

// ownsPrefix reports whether id falls under prefix per the given match mode.
func (m PrefixMatch) ownsPrefix(id, prefix string) bool {
	if prefix == "" {
		return false
	}
	if m == ExactOrHyphen && id == prefix {
		return true
	}
	return hasHyphenNamespace(id, prefix)
}

// hasHyphenNamespace reports whether id begins with "prefix-".
func hasHyphenNamespace(id, prefix string) bool {
	if len(id) <= len(prefix) {
		return false
	}
	return id[:len(prefix)] == prefix && id[len(prefix)] == '-'
}

// PrefixOwner returns the store whose configured prefix owns id's namespace,
// using a longest-prefix match under the given mode, and whether any prefix
// matched. On a length tie the first matching candidate in order wins.
// Candidates with an empty prefix are skipped; a candidate whose prefix wins
// still owns id even if its Store is nil (matched is true, store is nil), so
// callers can suppress a fallback when the owning store simply is not loaded.
//
// The returned store is the exact beads.Store value from the winning candidate;
// PrefixOwner never reads any store.
func PrefixOwner(id string, candidates []Candidate, match PrefixMatch) (store beads.Store, matched bool) {
	if id == "" {
		return nil, false
	}
	bestLen := -1
	for _, c := range candidates {
		if c.Prefix == "" || !match.ownsPrefix(id, c.Prefix) {
			continue
		}
		if len(c.Prefix) <= bestLen {
			continue
		}
		store = c.Store
		matched = true
		bestLen = len(c.Prefix)
	}
	return store, matched
}

// Resolve probes stores in order for the bead with the given ID and returns the
// first hit. Because a bead lives in exactly one store, the first store whose
// Get succeeds is authoritative. Nil stores are skipped. A store returning a
// not-found error (errors.Is(err, beads.ErrNotFound)) is treated as "absent
// here" and the probe continues; any other error is returned immediately. When
// no store has the bead and every probe was a clean not-found, Resolve returns
// beads.ErrNotFound.
func Resolve(id string, stores []beads.Store) (beads.Bead, error) {
	for _, store := range stores {
		if store == nil {
			continue
		}
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return beads.Bead{}, err
		}
		return b, nil
	}
	return beads.Bead{}, beads.ErrNotFound
}

// ErrAmbiguous reports that a bead ID resolved in more than one candidate store.
// ResolveOwner returns it (wrapped with both store indices) so callers that
// require a uniquely addressable bead can reject the ID rather than silently
// pick one store. Resolve does not surface this case — it stops at the first hit
// — so callers that must detect a bead present in multiple stores use
// ResolveOwner instead.
var ErrAmbiguous = errors.New("bead exists in multiple stores")

// ResolveOwner probes every store in order for the bead with the given ID and
// returns the index of the single store that owns it. Unlike Resolve, it does
// not stop at the first hit: it scans all candidates so a bead present in more
// than one store is rejected with an error wrapping ErrAmbiguous, preserving the
// "resolution requires a uniquely addressable bead id" contract. Nil stores are
// skipped. A store returning a not-found error (errors.Is(err, beads.ErrNotFound))
// is treated as "absent here"; any other error is returned immediately. When no
// store has the bead and every probe was a clean not-found, ResolveOwner returns
// beads.ErrNotFound. The returned index addresses the supplied stores slice
// (including any nil entries), so callers can recover the owning candidate's
// metadata (e.g. its directory) by indexing a parallel slice.
func ResolveOwner(id string, stores []beads.Store) (beads.Bead, int, error) {
	found := beads.Bead{}
	foundIndex := -1
	for i, store := range stores {
		if store == nil {
			continue
		}
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return beads.Bead{}, -1, err
		}
		if foundIndex >= 0 {
			return beads.Bead{}, -1, fmt.Errorf("%w: indices %d and %d", ErrAmbiguous, foundIndex, i)
		}
		found = b
		foundIndex = i
	}
	if foundIndex < 0 {
		return beads.Bead{}, -1, beads.ErrNotFound
	}
	return found, foundIndex, nil
}

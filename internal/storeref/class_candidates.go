package storeref

import (
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// ClassRouting describes ONE relocated coordination class for by-id candidate
// resolution: the reserved id prefix the class mints, the store it was
// relocated to, the work store it was relocated away from, and the configured
// work-store prefixes that can shadow the class namespace.
//
// The class is identified by its Prefix rather than by a class constant, so
// this package stays free of internal/config (which imports internal/beads and
// could not import back). Callers supply the prefix from
// config.ReservedClassPrefix.
type ClassRouting struct {
	// Prefix is the reserved class id prefix the relocated store mints, e.g.
	// "gcg" for the graph class. Empty disables the routing entirely.
	Prefix string

	// Class is the store the coordination class was relocated to. Nil, or a
	// value equal to Work, means the class is NOT relocated.
	Class beads.Store

	// Work is the city/HQ work store the class was relocated away from. It is
	// the trailing fallback leg of the candidate list: on a MIGRATED city an id
	// minted before relocation keeps its work-era prefix, and a bead the class
	// arm has not physically moved yet is still readable there.
	Work beads.Store

	// Shadows are the loaded work stores paired with the id prefixes they were
	// CONFIGURED with (the city/HQ prefix and each rig's effective prefix).
	// Reserved class prefixes are warned-and-allowed on work stores
	// (config.ReservedPrefixWarnings, not rejected by config.ValidateRigs), so a
	// work store can legitimately own ids inside — or under a longer prefix
	// starting with — the class namespace. Listing it here keeps those ids
	// reachable by id once the class relocates.
	Shadows []PrefixedStore
}

// PrefixedStore pairs a work store with the id prefix it was configured with.
type PrefixedStore struct {
	Prefix string
	Store  beads.Store
}

// ClassCandidates returns the by-id candidate PROBE LIST for id under routing,
// or nil when routing does not apply to id.
//
// It is the shared form of the class arm internal/api's by-id resolver used to
// carry inline. The contract, in order:
//
//   - Nil unless the class is actually relocated. "Relocated" is decided by
//     STORE IDENTITY — Class != nil && Class != Work, the same question
//     cmd/gc's resolveClassStore answers — never by a marker file or a
//     migration flag. A city whose class store IS its work store gets nil here
//     and keeps its legacy resolution byte-identical.
//   - Nil unless id is inside the class namespace (IDInNamespace).
//   - Otherwise: the class store FIRST, then every shadowing work store whose
//     configured prefix also covers id (most specific first), then Work.
//
// The result is a candidate list, never an unconditional route: a bead lives in
// exactly one store, so the caller probes the list in order and pins the first
// store that answers. The class store leads because it is the sole minter of
// the reserved namespace, so a class id pins it on the first probe; the rest of
// the list exists because "reserved prefix" is an ADVISORY on work stores, and
// on a migrated city an id can predate the relocation that gave the namespace
// away. Routing unconditionally on the prefix instead would strand exactly
// those ids.
func ClassCandidates(id string, routing ClassRouting) []beads.Store {
	id = strings.TrimSpace(id)
	if routing.Class == nil || routing.Class == routing.Work {
		return nil
	}
	if !IDInNamespace(id, routing.Prefix) {
		return nil
	}
	candidates := make([]beads.Store, 0, len(routing.Shadows)+2)
	seen := make(map[beads.Store]bool, len(routing.Shadows)+2)
	add := func(s beads.Store) {
		if s == nil || seen[s] {
			return
		}
		seen[s] = true
		candidates = append(candidates, s)
	}
	add(routing.Class)
	for _, shadow := range shadowsCovering(id, routing.Shadows) {
		add(shadow)
	}
	add(routing.Work)
	return candidates
}

// shadowsCovering returns the work stores whose configured prefix also covers
// id, most specific (longest configured prefix) first so the narrowest declared
// owner is probed before a broader one. Ties keep input order; two distinct
// prefixes of equal length cannot both cover the same id, so a tie only happens
// between duplicate prefixes, which config.ValidateRigs already rejects.
func shadowsCovering(id string, shadows []PrefixedStore) []beads.Store {
	matched := make([]PrefixedStore, 0, len(shadows))
	for _, shadow := range shadows {
		prefix := strings.TrimSpace(shadow.Prefix)
		if shadow.Store == nil || !IDInNamespace(id, prefix) {
			continue
		}
		matched = append(matched, PrefixedStore{Prefix: prefix, Store: shadow.Store})
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return len(matched[i].Prefix) > len(matched[j].Prefix)
	})
	stores := make([]beads.Store, 0, len(matched))
	for _, m := range matched {
		stores = append(stores, m.Store)
	}
	return stores
}

// IDInNamespace reports whether id falls under prefix's id namespace: a bare
// id equal to prefix, or anything under "prefix-". This is the CONFIGURED-prefix
// rule — it admits the bare form because a configured rig/HQ prefix can be a
// whole id.
//
// It is deliberately NOT the rule PrefixOwner applies. PrefixOwner routes on a
// store's SELF-DECLARED IDPrefix() and requires the "prefix-" separator, because
// a store that mints "gcg-1" never mints the bare id "gcg". Keep the two
// distinct: widening PrefixOwner would let a bare id capture a store that cannot
// hold it.
func IDInNamespace(id, prefix string) bool {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return false
	}
	return id == prefix || strings.HasPrefix(id, prefix+"-")
}

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

	// Work is the city/HQ work store the class was relocated away from. It stays
	// in the candidate list directly behind the class store — where the pre-seam
	// resolver already put it — as the fallback leg for an id inside the class
	// namespace that the class store does not hold. The reserved prefix is
	// warned-and-allowed on work stores, so one can legitimately mint and hold
	// such an id.
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
//   - Otherwise: the class store FIRST, then Work, then every shadowing work
//     store whose configured prefix also covers id (most specific first).
//
// # Why a probe list rather than a route
//
// A bead lives in exactly one store, so the caller probes the list in order and
// pins the first store that answers. The value of the shape is that it PROBES:
// the class store leads because it is the sole MINTER of the reserved
// namespace, but minting is not holding, and a reserved prefix is only an
// ADVISORY on work stores (config.ReservedPrefixWarnings warns;
// config.ValidateRigs does not reject). A work store can therefore legitimately
// hold an id inside the class namespace, and a resolver that routed
// unconditionally on the prefix would report those beads as absent.
//
// # What the order guarantees
//
// [class, work] is exactly the list the pre-seam internal/api arm returned, and
// it stays the HEAD of this one — the shadowing stores are appended behind it.
// Every probe the pre-seam list performed still happens, in the same order, and
// the added legs are reached only where it had already given up. So a store
// added here can neither change an answer that resolution already served nor
// turn a served read into an error by failing ahead of the store that holds the
// bead: the worst a broken shadow can do is turn a not-found into a hard error,
// which is the honest report when a store that could hold the id is unreachable.
//
// # NOT covered: a relocated bead that kept a legacy id
//
// IDInNamespace gates BEFORE the list is built, so this resolver only ever sees
// ids inside the class namespace. `gc storage migrate` preserves ids
// (cmd/gc/infra_class_migrate.go), so a bead it relocated keeps its HQ/rig-era
// prefix, is outside the namespace, and gets nil back here. This resolver does
// not cover that bead and must not be described as if it does.
//
// What covers it is a residence probe — asking the class store about EVERY id
// rather than only about prefixed ones. cmd/gc/cmd_bd_by_id.go's
// bdByIDClassDoor.resolve does exactly that, for exactly this reason. Nothing
// on the internal/api by-id path does, so there a legacy-prefixed relocated
// bead is still answered from the work store's retained pre-migration copy (the
// migration never deletes its source). Giving that path a residence probe is
// open work, not a property of this function.
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
	add(routing.Work)
	for _, shadow := range shadowsCovering(id, routing.Shadows) {
		add(shadow)
	}
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
// It is deliberately NOT the rule PrefixOwner applies, and the two are the
// package's two namespace predicates. Which to use:
//
//   - IDInNamespace, when the prefix comes from CONFIG — a rig/HQ prefix, or a
//     reserved class prefix from config.ReservedClassPrefix. A configured prefix
//     can be a whole id, so the bare form counts.
//   - PrefixOwner, when the prefix comes from the STORE — its self-declared
//     IDPrefix(). It requires the "prefix-" separator, because a store that
//     mints "gcg-1" never mints the bare id "gcg".
//
// Keep them distinct: widening PrefixOwner would let a bare id capture a store
// that cannot hold it.
func IDInNamespace(id, prefix string) bool {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return false
	}
	return id == prefix || strings.HasPrefix(id, prefix+"-")
}

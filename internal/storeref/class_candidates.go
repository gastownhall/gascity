package storeref

import (
	"fmt"
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
// # Now an internal helper of the ByID intent
//
// The list this returns IS Plan(ByID)'s in-namespace arm, computed by the same
// code (resolve.go's planByID) over a Topology built from the routing. The
// exported symbol is kept for the branches in flight; the residency boundary
// check forbids NEW direct callers, because answering "which store owns this
// bead" twice is the bug class this whole lane exists to close (#5125/#5127).
func ClassCandidates(id string, routing ClassRouting) []beads.Store {
	id = strings.TrimSpace(id)
	if routing.Class == nil || routing.Class == routing.Work {
		return nil
	}
	if !IDInNamespace(id, routing.Prefix) {
		return nil
	}
	plan, err := planByID(ByID{ID: id}, routing.topology())
	if err != nil {
		return nil
	}
	candidates := make([]beads.Store, 0, len(plan.Legs))
	for _, leg := range plan.Legs {
		candidates = append(candidates, leg.Leg.Store)
	}
	return candidates
}

// topology renders a ClassRouting as the Topology the resolver plans over. The
// refs are synthetic — this caller discards them — and the shadows are indexed
// so the resolver's ref ordering preserves the routing's own input order, which
// is what a tie between two equal-length prefixes falls back to.
func (r ClassRouting) topology() Topology {
	t := Topology{
		Work: Leg{Ref: WorkRef, Store: r.Work},
		Bindings: []ClassBinding{{
			Prefixes: []string{r.Prefix},
			Leg:      Leg{Ref: StoreRef("class:" + strings.TrimSpace(r.Prefix)), Store: r.Class},
		}},
	}
	for i, shadow := range r.Shadows {
		t.Rigs = append(t.Rigs, Leg{Ref: StoreRef(fmt.Sprintf("shadow:%04d", i)), Store: shadow.Store, Prefix: shadow.Prefix})
	}
	return t
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

package storeref

// Building bindings from opened routes, once.
//
// Three planes group a city's relocated classes by the store serving them — the
// CLI's storageRoutes, the API's State accessors, and the controller they share
// — and each then has to turn that grouping into ClassBindings. The turning is
// where the residence-probe retirement condition is decided, and it was written
// out twice: two derivations of the same question, in packages that never
// import each other, either of which could be edited alone.
//
// A disagreement there is silent in the worst direction. The two halves of the
// condition (ClassBinding.MintsReserved, ClassBinding.HasLegacyResidents) are
// pessimistic by construction, so a copy that drifted OPTIMISTIC would not fail
// a build or a read: it would retire the probe on one plane and keep it on the
// other, and the plane that retired it would answer "no such bead" for every id
// the split preserved. Sharing the body is what makes the pessimism a property
// of the program rather than of two files agreeing today.

import (
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// BindingOptions carries what differs between the planes that build bindings.
// Everything else — the namespace union, both halves of the retirement
// condition, the standing refusal, the ordering — is the same question and is
// answered by BuildBindings itself.
//
// The zero value is meaningful and pessimistic: no census to ask, no blind spot
// to correct for.
type BindingOptions struct {
	// Relics answers the boot census's question for a binding store: does it
	// still hold OPEN beads minted outside the reserved namespace?
	//
	// Nil means there is no census to ask, and the answer is true for every
	// store. A caller that censused nothing has cleared nothing, and only
	// "known to hold none" may retire a probe.
	Relics func(beads.Store) bool

	// KnownRelics answers the same question from a DURABLE record, keyed by the
	// binding's ref rather than by its store: has a census ever proven this
	// binding holds a bead outside its reserved namespaces?
	//
	// It is separate from Relics because it survives a boot that cannot read
	// the binding at all — the refused city, where the live census has no store
	// to ask and every answer falls back to the pessimistic default. A ref the
	// record does not name is "not known", so nil means "no record to ask" and
	// the answer is false for every binding: this bit is evidence, and its
	// absence must never be read as proof.
	KnownRelics func(StoreRef) bool

	// CompleteClasses rounds an observed class set up to include classes the
	// calling plane cannot see a store for.
	//
	// Nil means the plane observes every class directly, and the set is taken
	// verbatim. A plane with a blind spot supplies this so its StoreRef names
	// the same physical binding the other planes name — the ref is shared
	// vocabulary, and a plane that derived it from a short set would write
	// census rows the others' filters drop.
	CompleteClasses func([]coordclass.Class) []coordclass.Class
}

// BuildBindings turns a store->classes grouping of the RELOCATED classes into
// the bindings a Topology carries, and reports the city's standing refusal when
// any binding store is a refusing one.
//
// order fixes the iteration over byStore so the result does not depend on map
// order; the bindings come back sorted by ref regardless, because a plan built
// over them must be deterministic for every caller.
//
// An empty grouping yields no bindings and no refusal — that is a single-store
// city, which Topology.IsSingleStore reads as the identity fast path.
func BuildBindings(order []beads.Store, byStore map[beads.Store][]coordclass.Class, opts BindingOptions) ([]ClassBinding, error) {
	if len(order) == 0 {
		return nil, nil
	}
	var refused error
	bindings := make([]ClassBinding, 0, len(order))
	for _, store := range order {
		classes := byStore[store]
		if opts.CompleteClasses != nil {
			classes = opts.CompleteClasses(classes)
		}
		prefixes := ReservedPrefixesFor(classes)
		ref := ClassRef(classes)
		known := knownLegacyResidents(opts.KnownRelics, ref)
		bindings = append(bindings, ClassBinding{
			Classes:  classes,
			Prefixes: prefixes,
			Leg:      Leg{Ref: ref, Store: store},
			// Both bits are observations, and neither is ever optimistic by
			// default: the mint bit comes from the store's own declaration, so
			// a store that declares nothing reports false, and the relic bit
			// comes from a census that was actually run, so a binding no census
			// reached still has relics as far as this build knows.
			MintsReserved: MintsInsideNamespace(store, prefixes),
			// Proof implies the pessimistic bit, so the two cannot disagree —
			// a live census that answered "clean" on a binding a durable record
			// says is dirty is a census that could not read it.
			HasLegacyResidents:   known || hasLegacyResidents(opts.Relics, store),
			KnownLegacyResidents: known,
		})
		if refusing, ok := store.(RefusingStore); ok && refused == nil {
			refused = refusing.StorageRefusal()
		}
	}
	sort.SliceStable(bindings, func(i, j int) bool { return bindings[i].Leg.Ref < bindings[j].Leg.Ref })
	return bindings, refused
}

// ReservedPrefixesFor returns the reserved id namespaces a class set HOLDS —
// the prefix each class mints under plus any its store holds without minting,
// such as the nudge queue's "gcnq" records inside the nudges store.
//
// A namespace the binding does not declare is not a weaker claim, it is no
// claim: the resolver gives the binding no authority over it, and the id falls
// through to the work ledger, which answers it emptily and confidently.
func ReservedPrefixesFor(classes []coordclass.Class) []string {
	prefixes := make([]string, 0, len(classes))
	for _, class := range classes {
		prefixes = append(prefixes, config.ReservedClassPrefixesFor(class.String())...)
	}
	return prefixes
}

// hasLegacyResidents applies the census verdict, or the pessimistic default
// when there is no census to ask.
func hasLegacyResidents(relics func(beads.Store) bool, store beads.Store) bool {
	if relics == nil {
		return true
	}
	return relics(store)
}

// knownLegacyResidents applies the durable record, or false when there is none
// to ask. The default is the opposite of hasLegacyResidents's for the same
// reason it is safe: this bit only ever DENIES an answer, so an unknown must
// never assert it.
func knownLegacyResidents(known func(StoreRef) bool, ref StoreRef) bool {
	if known == nil {
		return false
	}
	return known(ref)
}

package storeref

// Topology: the opened stores of one city, as data.
//
// The residency resolver is PURE — it computes plans from a Topology value and
// opens nothing. Topology is therefore built by the process that already holds
// the opened stores, and built from those OPENED ROUTES rather than from
// configuration.
//
// That distinction is the whole reason this type exists as a value rather than
// as a config read. storageSplitShapeOf reads [storage] alone and answers "no
// split" for a city whose section was DELETED after it had already served one;
// that city's infrastructure beads are in a binding, its boot refuses, and a
// resolver built from config would hand it a work-only plan that reads the work
// ledger and reports "no work" forever. A resolver built from the opened routes
// gets the refusal, and fails loud with the sentence that names the remedy.
// (cmd/gc/class_store.go's cityQueryTopology carries the same lesson for the
// generated work_query.)

import (
	"errors"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// StoreRef is the stable identity of one leg. It is the vocabulary a persisted
// census row, a wake filter, or a partial-result diagnostic names a store with.
//
// Three families, and the third is new: "" is the city/HQ work store, "rig:<n>"
// is a rig work store, and "class:<token>" is a relocated class BINDING. Before
// the binding family existed, claims collected on the binding arm were recorded
// under ref "" and dropped by the ref-strict wake filter — the vocabulary gap
// was the bug (ga-whzrt).
type StoreRef string

// WorkRef is the city/HQ work store. Empty by construction: it is the ref every
// pre-split census row already carries, and widening it would rewrite history.
const WorkRef StoreRef = ""

// RigRef is the canonical ref of a rig work store.
func RigRef(name string) StoreRef { return StoreRef("rig:" + strings.TrimSpace(name)) }

// classRefPrefix is the binding family's marker, spelled once so ClassRef and
// the scope predicates that read it back cannot drift apart.
const classRefPrefix = "class:"

// ClassRef is the canonical ref of the binding serving a class set.
//
// The token is the classes' initials in name order, which identifies a binding
// uniquely because a class is served by exactly one binding and the
// infrastructure class names have distinct initials
// (TestClassRefIsStableAndCollisionFree fails the day one does not). It is a
// compact stable token rather than a digest so a corpus row, a partial-result
// diagnostic and an operator's eye all read the same string.
func ClassRef(classes []coordclass.Class) StoreRef {
	names := make([]string, 0, len(classes))
	for _, c := range classes {
		if n := c.String(); n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var token strings.Builder
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		token.WriteString(n[:1])
	}
	return StoreRef(classRefPrefix + token.String())
}

// Leg is one store the resolver can name, with the id prefix it was CONFIGURED
// with. The prefix is the shadow rule's input: a work store's configured prefix
// can cover an id inside a relocated class's namespace, because a reserved
// prefix is only advisory on work stores (config.ReservedPrefixWarnings warns;
// config.ValidateRigs does not reject).
type Leg struct {
	Ref    StoreRef
	Store  beads.Store
	Prefix string
}

// ClassBinding is one relocated class binding actually being served.
type ClassBinding struct {
	// Classes are the coordination classes this binding serves. A whole split
	// carries all five infrastructure classes on one binding.
	Classes []coordclass.Class

	// Prefixes are the reserved id namespaces those classes HOLD — the prefix
	// each mints under plus any its store holds without minting, such as the
	// nudge queue's. ReservedPrefixesFor derives them from the one table in
	// internal/config that declares them.
	Prefixes []string

	// Leg is the opened binding store.
	Leg Leg

	// MintsReserved reports whether this binding's workspace mints INSIDE the
	// reserved namespace. It is half of the residence-probe retirement
	// condition: when a binding mints truthfully, a new bead's residence is
	// decidable from its id alone.
	//
	// A constructor may only set this from a VERIFIED boot-time check. The
	// check is MintsInsideNamespace: the store declares the namespace it mints
	// into, the binding declares the namespaces it claims, and a store that
	// declares nothing reports false. A false bit only keeps the residence
	// probe; a wrong true one retires it over beads it cannot recognize.
	MintsReserved bool

	// HasLegacyResidents reports whether the binding is known to still hold
	// OPEN beads minted outside the reserved namespace — the relics `gc storage
	// migrate` produced by preserving ids. The residence probe retires only
	// when the binding both mints truthfully AND holds no such relic.
	//
	// Constructors set this TRUE until a census can say otherwise: "not known
	// to hold relics" and "known to hold none" are different claims, and only
	// the second may retire the probe. Defaulting false while the mint bit is
	// observed would retire the probe on any converged city the moment it
	// booted, stranding every id the migration preserved.
	HasLegacyResidents bool

	// KnownLegacyResidents reports whether a census has PROVEN this binding
	// holds such a relic. It is the same subject as HasLegacyResidents and the
	// opposite kind of claim: that field is a pessimistic default that a census
	// may lower, this one is evidence that only a census may raise. A
	// constructor with nothing to cite leaves it false.
	//
	// The distinction is worth a second bool because the two are read for
	// opposite purposes. HasLegacyResidents decides whether to KEEP a probe, so
	// its unknown must be true; this one decides whether to DENY an answer, so
	// its unknown must be false. Collapsing them would either retire probes on
	// unknown or deny reads on unknown, and both are the failure this package
	// exists to prevent, in one direction or the other.
	//
	// True here always implies HasLegacyResidents — every constructor derives
	// the pessimistic bit from the same or a weaker source — so "proven relics
	// on a retired probe" is not a state a caller has to reason about.
	KnownLegacyResidents bool
}

// Topology is the store arrangement of one city.
type Topology struct {
	// Work is the city/HQ work-class leg. Always present.
	Work Leg

	// Rigs are the rig work-class legs. Suspended rigs are excluded by the
	// constructor: it is TOLD which rigs are serving, it does not decide.
	Rigs []Leg

	// Bindings are the relocated class bindings actually being served. Empty
	// means a single-store city, and the identity fast-paths apply.
	Bindings []ClassBinding

	// Refused carries the standing storage refusal when boot could not serve
	// the configured split. Plans over a refused topology fail loud with the
	// refusal that names the remedy — never a degraded work-only answer.
	Refused error
}

// IsSingleStore reports whether the city relocates nothing. On a single-store
// city every class collapses to the work store and by-id resolution is the
// caller's own read: no probe, no funnel, byte-identical to the pre-split path.
func (t Topology) IsSingleStore() bool { return len(t.Bindings) == 0 }

// ClaimRefs returns the store-refs a claim held by ONE identity can be recorded
// under, whatever work scope the holder has: the work arm, then every relocated
// class binding this city serves.
//
// This is the wake filter's and the orphan-release index's question, and it is
// answered from the topology rather than from a Plan for two reasons. The
// asker matches persisted census refs and reads no store, so a plan's executor
// has nothing to do. And a REFUSED city still has claims recorded under its
// binding's ref, so the answer has to exist exactly where a plan correctly
// refuses — losing it there would drop every claim-holder on the city the
// refusal is about into the no-wake-reason drain.
//
// Legs that resolved to the same store collapse, so a city whose leading arm IS
// its binding answers with the one ref its census actually records. That
// collapse is why this is a Topology method and not a list of binding refs: the
// dedupe needs the stores.
func (t Topology) ClaimRefs() []StoreRef {
	refs := []StoreRef{t.Work.Ref}
	var seen storeSet
	seen.addIfNew(t.Work.Store)
	for _, b := range t.orderedBindings() {
		if b.Leg.Store != nil && !seen.addIfNew(b.Leg.Store) {
			continue
		}
		refs = append(refs, b.Leg.Ref)
	}
	return refs
}

// BindingFor returns the binding serving class c, if any.
func (t Topology) BindingFor(c coordclass.Class) (ClassBinding, bool) {
	for _, b := range t.orderedBindings() {
		for _, have := range b.Classes {
			if have == c {
				return b, true
			}
		}
	}
	return ClassBinding{}, false
}

// orderedBindings returns the bindings by ref ascending. Sorting here rather
// than trusting the constructor is what makes a plan deterministic for every
// caller, including one that assembled a Topology by hand in a test.
func (t Topology) orderedBindings() []ClassBinding {
	if len(t.Bindings) < 2 {
		return t.Bindings
	}
	out := append([]ClassBinding(nil), t.Bindings...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Leg.Ref < out[j].Leg.Ref })
	return out
}

// orderedRigs returns the rig legs by ref ascending — the "rig-name ascending"
// order the ready federation and the API's list fan-out already agree on.
func (t Topology) orderedRigs() []Leg {
	if len(t.Rigs) < 2 {
		return t.Rigs
	}
	out := append([]Leg(nil), t.Rigs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// coversID reports whether any of the binding's reserved prefixes claims id's
// namespace.
func (b ClassBinding) coversID(id string) bool { return idInAnyNamespace(id, b.Prefixes) }

// probeRetired reports whether the residence probe may be dropped for this
// binding: it mints truthfully AND holds no open legacy resident. Both halves
// are required — a point-in-time "zero relics" on a binding that still mints
// work-shaped ids re-strands the very next create.
func (b ClassBinding) probeRetired() bool { return b.MintsReserved && !b.HasLegacyResidents }

// probeRefusalPolicy is the error policy a residence probe over this binding
// carries.
//
// The tolerated-refusal carve-out rests on one sentence — a refused city still
// serves WORK, so a probe for an id no relocated class could own may skip the
// refusing leg — and a binding PROVEN to hold ids outside its reserved
// namespaces has falsified it. Skipping it there does not fall back to "no
// answer": the migration preserved those ids and deleted nothing, so it falls
// back to the frozen pre-migration copy in the work store, which answers
// confidently and wrongly.
func (b ClassBinding) probeRefusalPolicy() ErrPolicy {
	if b.KnownLegacyResidents {
		return PolicyFatal
	}
	return PolicyRefusalTolerated
}

// RefusingStore is the optional interface a store implements to declare that it
// is a standing storage refusal rather than a servable binding. A topology
// constructor uses it to detect a refused city from the OPENED routes without
// performing a read.
type RefusingStore interface {
	// StorageRefusal returns the refusal this store answers every operation
	// with, naming the remedy.
	StorageRefusal() error
}

// StandingRefusal is the optional interface an error implements to declare
// itself this build's standing verdict about the CITY's storage configuration,
// rather than a fault the read it came from ran into.
//
// The difference is actionable, which is why it is a type and not a string
// match: a refused city still serves its WORK beads from its work ledger, so a
// caller holding a work-shaped id has been told nothing about that id, while a
// caller holding an id only a relocated class could own has been told the one
// thing that matters.
type StandingRefusal interface {
	StandingStorageRefusal()
}

// IsStandingRefusal reports whether err is a standing storage refusal.
func IsStandingRefusal(err error) bool {
	var refusal StandingRefusal
	return errors.As(err, &refusal)
}

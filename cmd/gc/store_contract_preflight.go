package main

// The class contract, enumerated and warned at controller boot: step 1 of the
// three-step deconditionalization (warn, then require, then refuse).
//
// The .162 slice replaced one capability RESOLUTION with one capability
// REQUIREMENT: the session class states what it needs and a store that cannot
// meet it is a named error. This is that idiom applied to the whole set — every
// store this controller serves, every class it serves, and the capabilities the
// contract will demand of that pairing.
//
// It warns and does not refuse. The refusal is step 3, and it lands after the
// cutover soak closes: a preflight that started failing boots here would be
// exactly the destabilization the migration order exists to prevent. What it
// buys before then is the thing an operator could not get at all — a boot-time
// statement of which stores will not survive the contract, named well enough to
// act on.
//
// It deliberately overlaps preflightSessionClassConditionalWrites by one line on
// a broken city. The two answer different questions: that one says the session
// reconciler cannot fence TODAY (an ERROR, and the row win3 reads out of the
// status block), this one says what the contract will refuse TOMORROW. Step 3
// collapses them.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// storeContractEmissionCapability names the requirement that a store append a
// bead.* event after its own mutations.
//
// It is spelled out rather than named after a Go interface because it is not
// one: emission is wiring a process performs at open, and a store that has it
// and a store that does not are the same type. That is precisely why it was
// missing on every split city for as long as it was — nothing could assert on
// it (ga-f7v2ft.161 Q4).
const storeContractEmissionCapability = "recorder-wired bead.* emission"

// storeContractCapability is one requirement the contract states, the check that
// answers it against a store, and what an operator does about a no.
type storeContractCapability struct {
	name   string
	remedy string
	check  func(beads.Store) (bool, string)
}

// contractConditionalWriter requires that a store fence its own writes. The
// check is the eager, probe-backed one — the same question the session class
// asks — because boot is where that cost belongs.
var contractConditionalWriter = storeContractCapability{
	name:   "ConditionalWriter",
	remedy: "serve this class from a store that fences its own writes (a [storage] sqlite binding, or a native Dolt ledger)",
	check: func(store beads.Store) (bool, string) {
		if _, err := beads.RequiredConditionalWriter(store); err != nil {
			return false, err.Error()
		}
		return true, ""
	},
}

// contractAtomicCloser requires the fused fenced terminal close. Keyed drain-ack
// will not own an admission without it, so a class store that lacks it hands
// every admission back to legacy.
var contractAtomicCloser = storeContractCapability{
	name:   "AtomicConditionalCloser",
	remedy: "serve this class from a store that closes and stamps in one transaction (a [storage] sqlite binding, or a native Dolt ledger)",
	check: func(store beads.Store) (bool, string) {
		if _, ok := beads.AtomicConditionalCloserFor(store); !ok {
			return false, "the store advertises no atomic terminal closer, so keyed drain-ack hands every admission back to legacy"
		}
		return true, ""
	},
}

// contractEmission requires that a write be observable. Without it every
// event-triggered admission on the class degrades to patrol-tick cadence, with
// no error and no event to say so.
var contractEmission = storeContractCapability{
	name:   storeContractEmissionCapability,
	remedy: "the process that opens this store must wire its event recorder into it (storageRoutes.withControllerEmission / withCLIEmission)",
	check: func(store beads.Store) (bool, string) {
		if !storeEmitsBeadEvents(store) {
			return false, "no writer of this store records anything, so event-triggered admission falls back to patrol cadence"
		}
		return true, ""
	},
}

// beadEventEmitter is a store that appends a bead.* event after each of its own
// successful mutations.
type beadEventEmitter interface{ EmitsChangeEvents() bool }

// storeEmitsBeadEvents reports whether a mutation through store produces a
// bead.* event, asking the store rather than inferring it from the type: the
// wiring is an instance property, and the two layers that carry it — the
// controller's caching store and a relocated class store — are both types that
// also exist unwired.
func storeEmitsBeadEvents(store beads.Store) bool {
	if store == nil {
		return false
	}
	base, _, _ := unwrapBeadPolicyStore(store)
	emitter, ok := base.(beadEventEmitter)
	return ok && emitter.EmitsChangeEvents()
}

// storeContractFor returns the capabilities a coordination class requires of the
// store serving it (§2).
//
// The infrastructure classes carry the full set: the keyed reconciler's writes
// to them are revision-fenced by construction, its terminal closes are atomic by
// construction, and its admissions are event-triggered by construction.
//
// Work carries emission alone, and the omission is deliberate rather than
// lenient. The contract's work row states a fence too, but only for gc-owned
// engines: the pinned schema-v59 bd ships no --if-revision on any verb (§4), so
// demanding one of every bd-backed ledger would warn every city in the fleet
// about a gap no operator can close. That row joins when beads ships the flag.
func storeContractFor(class coordclass.Class) []storeContractCapability {
	if class.IsInfrastructure() {
		return []storeContractCapability{contractConditionalWriter, contractAtomicCloser, contractEmission}
	}
	return []storeContractCapability{contractEmission}
}

// storeContractSubject is one store-and-class pairing the contract judges.
type storeContractSubject struct {
	storeID string
	class   coordclass.Class
	store   beads.Store
}

// storeContractSubjects enumerates every store this controller serves beside the
// classes it serves from it.
//
// The enumeration is the point. Its predecessor probed work stores only, which
// is how a split city reported a clean boot while nothing at all was known about
// the store its session rows live in (ga-f7v2ft.162): the classes that had moved
// were exactly the classes nobody asked about.
func (cs *controllerState) storeContractSubjects() []storeContractSubject {
	cs.mu.RLock()
	routes := cs.storageRoutes
	city := cs.cityBeadStore
	rigs := make(map[string]beads.Store, len(cs.beadStores))
	for name, store := range cs.beadStores {
		rigs[name] = store
	}
	cs.mu.RUnlock()

	subjects := make([]storeContractSubject, 0, len(coordclass.Classes())+len(rigs))
	for _, class := range coordclass.Classes() {
		store, relocated := routes.storeFor(class)
		storeID := "city"
		if relocated {
			storeID = storageBindingStoreID(routes.binding)
		} else {
			store = city
		}
		if store == nil {
			continue
		}
		subjects = append(subjects, storeContractSubject{storeID: storeID, class: class, store: store})
	}

	rigNames := make([]string, 0, len(rigs))
	for name := range rigs {
		rigNames = append(rigNames, name)
	}
	sort.Strings(rigNames)
	for _, name := range rigNames {
		if rigs[name] == nil {
			continue
		}
		// A rig ledger serves work and nothing else: coordination classes are
		// city-keyed, so no rig store is ever a class binding.
		subjects = append(subjects, storeContractSubject{storeID: "rig/" + name, class: coordclass.ClassWork, store: rigs[name]})
	}
	return subjects
}

// storeContractViolation is one capability one store does not have, beside every
// class that store serves.
//
// Classes accumulate onto a violation instead of producing one line each,
// because a store serving five infrastructure classes with no fence is one
// missing fence, not five — and five identical lines is a thing an operator
// scrolls past rather than reads.
type storeContractViolation struct {
	storeID    string
	storeKind  string
	capability string
	remedy     string
	reason     string
	classes    []string
}

// String renders one violation as the operator-facing sentence.
func (v storeContractViolation) String() string {
	subject := fmt.Sprintf("class %q", v.classes[0])
	if len(v.classes) > 1 {
		quoted := make([]string, 0, len(v.classes))
		for _, class := range v.classes {
			quoted = append(quoted, fmt.Sprintf("%q", class))
		}
		subject = "classes " + strings.Join(quoted, ", ")
	}
	return fmt.Sprintf("store contract violation: store %q (%s) serving %s is missing %s: %s; remediation: %s",
		v.storeID, v.storeKind, subject, v.capability, v.reason, v.remedy)
}

// preflightStoreContract states, once at boot, every way this city's stores fall
// short of the class contract. One WARN line per store and capability, in a
// stable order, and the boot continues.
func (cs *controllerState) preflightStoreContract() {
	if cs == nil {
		return
	}
	type violationKey struct{ storeID, capability string }
	index := map[violationKey]int{}
	var violations []storeContractViolation

	for _, subject := range cs.storeContractSubjects() {
		for _, capability := range storeContractFor(subject.class) {
			ok, reason := capability.check(subject.store)
			if ok {
				continue
			}
			key := violationKey{storeID: subject.storeID, capability: capability.name}
			if at, seen := index[key]; seen {
				violations[at].classes = append(violations[at].classes, subject.class.String())
				continue
			}
			index[key] = len(violations)
			violations = append(violations, storeContractViolation{
				storeID:    subject.storeID,
				storeKind:  beads.InspectConditionalWrites(subject.store).StoreKind,
				capability: capability.name,
				remedy:     capability.remedy,
				reason:     reason,
				classes:    []string{subject.class.String()},
			})
		}
	}

	for _, violation := range violations {
		cs.rolloutWarnf("api: beads: WARN: %s\n", violation)
	}
}

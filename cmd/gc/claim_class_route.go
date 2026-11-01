package main

// Claim-time write routing by coordination class.
//
// `gc hook --claim` issues every claim-time write through
// beads.NewBdStore(dir, ...) — a bd subprocess rooted in the agent's WORK
// directory (hookClaimBdStoreContext). A relocated coordination class is not a
// bd workspace and cannot be expressed as a hookStore{dir, env} at all, so on a
// split city those writes run against a ledger that cannot see the bead.
//
// The read half closed first: the generated work query now reads through the
// federated `gc ready`, which covers the binding in process (ga-bvdha). That
// made relocated graph work VISIBLE to the worker without making it claimable —
// an assigned graph step was re-served and re-skipped every tick and no worker
// ever ran it. This file is the write half. It adds a CLASS axis to the existing
// hookClaimOps seam, alongside the rig axis in claim_cross_store.go, rather than
// a parallel claim path.
//
// # The order is EVERY work leg first, and that is not the by-id order
//
// A claim is a WRITE, so the store is found by PROBE — ask which store holds the
// bead — never by routing unconditionally on an id prefix. `gc storage migrate`
// preserves ids (infra_class_migrate.go), so a relocated bead keeps its
// HQ/rig-era prefix and a prefix route would miss exactly the beads that moved;
// the same reason bdByIDClassDoor.resolve probes residence.
//
// What differs from the by-id door is the ORDER of the probe, and the difference
// is load-bearing on the one id a probe order can disagree about: a CO-RESIDENT
// bead, which is the documented steady state of a migrated city because the
// migration copies and never deletes back.
//
//   - storeref.ClassCandidates and bdByIDClassDoor lead with the class store.
//     Their caller holds only an id.
//   - `gc ready` — the federated reader that produced this claim's candidate —
//     leads with the CITY work store and runs the graph leg LAST, so a
//     co-resident id resolves to the work store's row (#5148/#5158/#5161,
//     ready_federation.go).
//
// The claim must agree with the reader that served it, and the failure if it
// does not is not cosmetic: claiming the class copy while the reader keeps
// answering from the still-open work copy re-serves the same bead every tick,
// which is the treadmill this slice exists to end.
//
// "The work store" is a PLURAL, and reading it as a singular is how the order
// above stops being true. The claim runs against one leg of a fan-out
// (hookWorkQueryStores), and for the rig-scoped agents that are the shipped
// shape of every worker that claims, the city work store is the leg that runs
// LAST: the rig store is prepended as the primary and appendCityHookStore
// appends the city store at the end. So the not-found the FIRST-selected leg
// returns proves only that THAT leg does not hold the bead — the city work leg
// behind it may.
//
// The escalation is therefore a property of the whole fan-out, not of one leg.
// Before a not-found opens it, every OTHER leg is READ for the same id
// (anotherWorkLegHolds), and a leg that holds the bead closes it: the work
// store's answer stands, the federated loop drops the leg that could not resolve
// it, and the leg that holds it claims it on the next turn of the same
// invocation — at its own rank, not behind whatever else the fan-out is
// carrying. Co-residence therefore keeps the WORK copy on any leg that holds it,
// byte-identically to today.
//
// A read rather than a second pass, because a second pass would let unrelated
// lower-tier work in another leg be claimed first, which is the starvation this
// slice closes wearing a different hat. The reads are bounded — one per
// remaining leg, once per id, only for a bead the leg that answered could not
// resolve — and claimHookWorkWithRunner is what hands the route the leg set
// (hookClaimOps.ClassRoute).
//
// # The escalation signal is a proven absence, never a guess
//
// beads.ErrNotFound from the write proves the bead is not in the store the
// write ran against (hookClaimBeadIsElsewhere), and it opens the escalation only
// together with ok=false. Any other write error proves nothing on its own word:
// a write timeout, store contention or a controller-socket flap leaves ownership
// unresolved on a bead the session may already own there, so such an error is
// never retried against a second store because of what it says.
//
// It does not prove the bead IS there either, and treating it as if it did is
// how one lane strands every binding-resident bead: a work store whose write
// spells its miss in a shape the predicate does not recognize answers every
// claim of a binding-resident bead with an error nobody classifies, the claim
// never reaches the binding the reader served the bead from, and the controller
// spawns a seat for it every tick (ga-ylpbsy.30: a remote work store's wire
// not_found). So an unrecognized write error is settled by the READ question
// instead (legProvedAbsent) — the question the federated reader that served the
// candidate already asked this leg. A read that answers not-found proves the leg
// holds no row, and no write can have landed on a row that does not exist; a
// read that finds the bead, or that fails, proves nothing, and the work store's
// own error stands. Readiness and claimability therefore rest on the same answer
// from the same leg, whatever shape that leg's write errors take, and the read
// is paid only on a claim that already failed.
//
// ok=true is the same refusal for the same reason. hookClaimThroughStore returns
// (claimed, true, err) for exactly one shape — the CAS COMMITTED and the
// canonical readback then failed — and BdStore.Get produces an
// ErrNotFound-wrapping error for a plain miss, an empty result AND
// beads.ErrIDCollision, so that readback failure routinely satisfies the
// predicate on a claim that landed. Escalating it would claim one logical bead
// in two ledgers and swallow the signal both call sites stop on, so the
// committed claim is returned exactly as the work store reported it.
//
// A read failure from the binding is likewise an error and never absence:
// reading "the binding could not answer" as "the bead is not there" is the
// root-loss shape this whole lane exists to prevent. The one error that is not a
// fault is the one-shot funnel's standing refusal, handled exactly as
// classRoutedStoreForID handles it.
//
// # A binding that cannot claim is a city fact, not a bead fault
//
// The CAS the routed claim acquires through is a capability: over an engine
// without the two-argument Claim, the closed contract answers
// ErrBeadsAdapterCapability rather than emulating it. That is a standing
// property of the city's storage configuration, so it is refused at the DOOR —
// newHookClaimClassRoute verifies it once, the way storebinding's
// NewBeadsNudgeQueue verifies the same capability at construction — and
// claimHookWork then runs unrouted with one loud line rather than failing every
// claim of every bead in every store.
//
// If a refusal reaches a claim anyway (a forwarding wrapper carries the method
// its leaf lacks, so it passes the door and refuses per bead with
// beads.ErrClaimUnsupported), it is a per-BEAD skip and not a terminal
// tick: the escalation only ran because the work store was proved not to hold
// the bead (its not-found, or a not-found from reading the same leg), which
// proves this session owns nothing there and that no mutation is outstanding
// anywhere (the capability refusal is returned before any write). That is the
// same proof the work-side not-found carries, so it earns the same skip —
// hookClaimBindingRefusedTheClaim, kept separate so the work-side predicate
// stays exactly as narrow as it was.
//
// # A single-store city takes none of this
//
// hookClaimClassRouteForCity gates on cliSoleClassBinding — store identity, the
// same question resolveClassStore asks — and returns nil for a city that
// relocates nothing. classRoutedHookClaimOps then returns the ops value it was
// handed, unwrapped, so every claim-time write is the exact call it is today.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/executionevent"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storeref"
)

// errClaimRouteBindingCannotClaim reports that the relocated coordination-class
// binding has no compare-and-swap claim, so no claim-time write can be routed to
// it. It is a property of the city's storage configuration rather than of any
// bead, which is why the caller degrades to unrouted claiming instead of failing.
var errClaimRouteBindingCannotClaim = errors.New("the relocated coordination-class binding cannot claim")

// errRestampGraphResident reports that an adoption re-stamp was declined because
// the bead is resident in the relocated graph store, which has no
// conditional-transfer primitive — and needs none, since no production close
// path fences a graph-resident bead on its stored assignee.
//
// It wraps beads.ErrConditionalTransferUnsupported so a caller that only asks
// "was a transfer possible?" still sees the same class of answer, while a caller
// that must decide whether the un-moved spelling is HARMFUL can tell this
// resident case apart from the work store's identical error.
var errRestampGraphResident = fmt.Errorf("graph-resident bead: %w", beads.ErrConditionalTransferUnsupported)

// hookClaimClassRoute is the opened coordination-class front door a claim-time
// write falls back to, plus the per-invocation record of which bead ids the
// binding was PROVED to hold.
//
// The CAS stays the store's. Claims go through the closed contract's Claim —
// the same acquire half `gc bd update --claim` routes through (#5132) — rather
// than a read-then-write here, which would lose the single-winner guarantee.
//
// resident is a memo, not a cache with a lifetime: one `gc hook --claim` is one
// process, the claim path is strictly sequential (claimHookWorkWithRunner loops
// stores, tryHookClaim walks candidates), and the map dies with the invocation.
// Its job is to spare the follow-on writes for a bead the claim already resolved
// a doomed bd subprocess each.
//
// workLegs and readLeg carry the fan-out ordering rule: before a not-found from
// ONE leg is allowed to open the escalation, every other leg is read for the
// same id. workHeld memoizes that answer for the same reason resident memoizes
// the binding's.
type hookClaimClassRoute struct {
	// class is the raw binding store, needed by the lifecycle projector, which
	// takes a beads.Store rather than the closed contract.
	class beads.Store
	graph storebinding.GraphStore

	// topology is the residency frame holds() plans over, captured once at
	// construction. Data, not a path and not a func: a route that re-entered the
	// funnel per bead could resolve a DIFFERENT handle from the one class and
	// graph were opened over, and would then prove a bead absent in one engine
	// while the claim wrote through another.
	topology storeref.Topology

	resident map[string]bool

	// workLegs is the whole work fan-out this invocation claims across, and
	// readLeg reads one bead through one leg's bd context. Both are empty on a
	// caller that drives the seams directly rather than through the federated
	// loop, which then has no other leg to preempt.
	workLegs []hookStore
	readLeg  func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, error)
	workHeld map[string]bool

	// recorder opens the city event log the route's lifecycle events go to.
	// Nil resolves the city and opens its log per event (openCityRecorder); a
	// `gc hook` invocation sets it to its own city's (hookCity.recorder).
	recorder func() events.Recorder
}

// newHookClaimClassRoute opens the claim-time class front door over an already
// resolved binding store. Split from hookClaimClassRouteForCity so the routing
// is testable against a store a test controls rather than only against a city on
// disk.
//
// A binding without the two-argument claim CAS is refused here rather than
// discovered per-bead mid-tick: the capability is a property of the opened
// engine, the closed contract will not emulate it with a read-then-write, and a
// route that cannot perform the one write it exists for is worse than no route.
// Same check, same place and same reason as storebinding.NewBeadsNudgeQueue.
func newHookClaimClassRoute(class beads.Store) (*hookClaimClassRoute, error) {
	graph, err := storebinding.NewBeadsGraphStore(class)
	if err != nil {
		return nil, fmt.Errorf("projecting the claim-time class front door: %w", err)
	}
	if _, ok := class.(interface {
		Claim(id, assignee string) (beads.Bead, bool, error)
	}); !ok {
		return nil, fmt.Errorf("%w: %T has no compare-and-swap assignment claim", errClaimRouteBindingCannotClaim, class)
	}
	return newClaimClassRouteOver(class, graph), nil
}

// newClaimClassRouteOver builds the route value over an already-projected front
// door, with both per-invocation memos live. It is the one place they are
// created, so a route can never reach the escalation with a nil map.
//
// The topology it derives is PESSIMISTIC: one binding over the given store,
// serving every infrastructure class, censused by a nil relics func — which
// storeref.BuildBindings reads as HasLegacyResidents=true for every store,
// because a caller holding no censused routes is entitled to claim nothing more.
// That makes probeRetired() false, so a route built here probes on exactly the
// ids it probed on before this frame existed. Callers that DO hold a census —
// hookClaimClassRouteForCity, the only production one — overwrite it with the
// real thing.
//
// Deriving it through soleBindingResidency rather than assembling a ClassBinding
// literal is the point of doing it at all: Prefixes, Classes, Ref and both
// retirement bits then come from the one production derivation, so a route built
// from a bare store cannot disagree with a route built from a city about what
// the binding's namespaces are.
//
// class is what the topology's binding leg is, and graph must be the projection
// OF that same store — newHookClaimClassRoute is what guarantees it. A caller
// pairing a graph over one store with a topology over another would prove a bead
// absent in one engine and write through the other, which is the split this
// route exists to close, one level up.
func newClaimClassRouteOver(class beads.Store, graph storebinding.GraphStore) *hookClaimClassRoute {
	bindings, refused := soleBindingResidency(class)
	return &hookClaimClassRoute{
		class:    class,
		graph:    graph,
		topology: claimRouteTopology(bindings, refused),
		resident: map[string]bool{},
		workHeld: map[string]bool{},
	}
}

// claimRouteTopology frames the bindings for holds().
//
// The work leg is the unprobed residual, which is what makes the plan's answer
// readable as a yes/no about the BINDING alone: this route's work axis is the
// federated bd fan-out the caller already ran and found not-found, not a store,
// and a real work leg here would report those same beads owned by work again.
// No rig legs, for the reason cliByIDOwner passes none — a claim escalation has
// never read them, and starting to would change which beads it claims.
func claimRouteTopology(bindings []storeref.ClassBinding, refused error) storeref.Topology {
	return assembleResidencyTopology(nil, newUnprobedWorkResidual(), nil, bindings, refused)
}

// hookClaimClassesRelocated reports whether a class-route resolution found a
// city that relocates its coordination classes: a route opened, or a binding
// that cannot claim, which degrades the route to nil (hookClaimRouteVerdict)
// but still holds the relocated beads. The started key must not take a
// degraded city's leg for an adoption's proven residence unless its readback
// found the bead there (hookStartedKeyFor).
func hookClaimClassesRelocated(opened *hookClaimClassRoute, err error) bool {
	return opened != nil || errors.Is(err, errClaimRouteBindingCannotClaim)
}

// hookClaimRouteVerdict turns one class-route resolution into the claim ops the
// invocation runs with, and reports whether `gc hook --claim` may proceed.
//
// A binding that cannot claim is the degrade case: binding-resident beads stay
// unclaimable exactly as they were before this routing existed, every work store
// the agent CAN reach keeps being claimed from, and the operator is told once
// per invocation. Failing the tick instead would take a city whose graph claims
// were already impossible and stop it claiming anything at all.
//
// Every other resolution failure still fails closed. Claiming through the work
// store on a city that relocates a class writes ownership into a ledger that
// does not hold the bead, which is the wrong-answer lane this routing closes.
func hookClaimRouteVerdict(route *hookClaimClassRoute, err error, stderr io.Writer) (*hookClaimClassRoute, bool) {
	switch {
	case err == nil:
		return route, true
	case errors.Is(err, errClaimRouteBindingCannotClaim):
		fmt.Fprintf(stderr, "gc hook --claim: %v; claim-time class routing is off, so a bead resident in the binding stays unclaimable — every work store this agent reaches is unaffected\n", err) //nolint:errcheck // best-effort stderr
		return nil, true
	default:
		fmt.Fprintf(stderr, "gc hook --claim: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil, false
	}
}

// hookClaimBindingRefusedTheClaim reports whether a claim failed because the
// relocated binding refused the CAS capability itself, rather than because the
// write was attempted and failed.
//
// It is deliberately NOT folded into hookClaimBeadIsElsewhere. That predicate
// answers a question about the WORK store, where only beads.ErrNotFound proves
// the bead is absent and everything else may be an outstanding mutation; this
// one answers a question about the BINDING, reached only after a work store
// already proved this session owns nothing there. Both refusals it recognizes
// are returned before any write, so nothing is outstanding anywhere and the bead
// can be skipped exactly as the work-side not-found is:
//
//   - storebinding.ErrBeadsAdapterCapability: the graph adapter's type
//     assertion on the binding fails first.
//   - beads.ErrClaimUnsupported: a forwarding wrapper has the two-argument
//     Claim, so it passes newHookClaimClassRoute's door check, but the store it
//     wraps does not, and the wrapper refuses before forwarding.
func hookClaimBindingRefusedTheClaim(err error) bool {
	return errors.Is(err, storebinding.ErrBeadsAdapterCapability) ||
		errors.Is(err, beads.ErrClaimUnsupported)
}

// hookClaimBeadIsAWisp reports whether a routed claim failed because the
// relocated binding resolved id to a wisp row rather than to a claimable
// issue.
//
// A wisp is not an ownership conflict and not evidence of an outstanding
// mutation: beads.NativeDoltStore.Claim returns beads.ErrWispNotClaimable (see
// its doc comment) only for a wisp it could not claim through the update
// operation either, refused before any write was attempted, which is the same
// "refused before any write, so nothing is outstanding" shape
// hookClaimBindingRefusedTheClaim carries for a capability-less binding. Only
// a native-store binding produces this sentinel; a SQLite- or bd-backed
// binding never does.
//
// The ready tier's claimFirstReadyHookAssignment fails the whole hook invocation
// on any claim error it does not recognize, so an unrecognized wisp refusal
// would stop this session over one routed id that this binding cannot claim —
// the one outcome ErrWispNotClaimable's own doc comment says should instead be
// "log it once and move on."
func hookClaimBeadIsAWisp(err error) bool {
	return errors.Is(err, beads.ErrWispNotClaimable)
}

// hookClaimClassRouteForCity resolves the claim-time class front door for a
// city, or (nil, nil) when the city relocates no coordination class.
//
// The funnel is the same one cityQueryTopology already entered to decide whether
// this invocation's work query federates at all, and it is memoized per city
// (cli_storage_routes.go), so a hook that reaches here opens no second binding.
//
// It resolves the SOLE binding rather than the graph class's, for the reason
// spelled out on cliSoleClassBinding: a claim escalates to this route for any
// reserved-prefix id, so a city serving its classes from more than one store
// would have sessions-class claims routed at the graph binding, which would
// truthfully report the bead absent. That shape is refused upstream; asking for
// the sole binding is what makes the refusal load-bearing here too.
func hookClaimClassRouteForCity(cityPath string) (*hookClaimClassRoute, error) {
	binding, relocated, err := cliSoleClassBinding(cityPath)
	if err != nil {
		return nil, fmt.Errorf("resolving the claim-time class route: %w", err)
	}
	if !relocated {
		return nil, nil
	}
	route, err := newHookClaimClassRoute(binding.Store)
	if err != nil {
		return nil, err
	}
	// The census-bearing frame, read from the SAME memo cliSoleClassBinding just
	// took the store out of. That is what makes the probe handle and the write
	// handle one store by construction rather than by coincidence, and it is why
	// the topology is not resolved from cityPath later: a second derivation could
	// open a second engine on the same root.
	bindings, refused := cliResidencyBindings(cityPath)
	route.topology = claimRouteTopology(bindings, refused)
	return route, nil
}

// knownResident reports whether an earlier probe in THIS invocation already
// proved the binding holds id. It never probes: a false answer means "not proved
// yet", and every caller that gets one still runs its work-scope write first.
func (r *hookClaimClassRoute) knownResident(id string) bool {
	if r == nil {
		return false
	}
	return r.resident[strings.TrimSpace(id)]
}

// holds probes the binding for id and memoizes the answer.
//
// The judgement is the residency resolver's, over the frame captured at
// construction. It used to be spelled here — an unconditional Get, a not-found
// arm, a hand-rolled standing-refusal arm — and every clause of it was a
// restatement of one the by-id door already had. Two consequences follow from
// the collapse, and both are the point:
//
// An error is a read that FAILED, never absence, and the one non-fault is the
// one-shot funnel's standing refusal on a WORK-shaped id. That is now the
// resolver's per-leg policy rather than a predicate here: a residence probe for
// an id no relocated class could own tolerates the refusal, because a refused
// city still serves work from its work ledger, while the authority leg for an id
// inside a reserved namespace surfaces it, because there the refusal IS the
// answer.
//
// And a binding the boot census certified relic-free is not probed at all for a
// work-shaped id. The census is taken to retire exactly this read, and this seam
// was paying it once per escalated bead while the by-id door had already stopped.
func (r *hookClaimClassRoute) holds(id string) (bool, error) {
	id = strings.TrimSpace(id)
	if r == nil || id == "" {
		return false, nil
	}
	if known, ok := r.resident[id]; ok {
		return known, nil
	}
	_, resident, err := byIDBindingOwnerForTopology(r.topology, id)
	if err != nil {
		return false, fmt.Errorf("reading %q from the relocated class binding: %w", id, err)
	}
	r.resident[id] = resident
	return resident, nil
}

// routes reports whether a claim-time WRITE for id must run against the binding
// instead of the work store, given the error the work-scope write returned.
//
// Only a proven absence from the leg the write ran against opens the
// escalation (legProvedAbsent): nil, or an error that leaves the bead's
// residence there unproven, means the work store's answer is the answer.
//
// The absence must hold for the whole WORK fan-out, not for the one leg that
// answered. from is the leg the write ran against; every OTHER leg is read for
// the same id first, and a leg that holds it closes the escalation — the work
// store's own answer stands, the federated loop drops this leg, and the leg that
// holds the bead claims it on the next turn of the same invocation.
func (r *hookClaimClassRoute) routes(ctx context.Context, from hookStore, id, assignee string, workErr error) (bool, error) {
	if r == nil || !r.legProvedAbsent(ctx, from, id, assignee, workErr) {
		return false, nil
	}
	return r.escalates(ctx, from, id, assignee)
}

// routesRead is routes for a claim-time READ. The read's own not-found is the
// proof legProvedAbsent would otherwise ask for, so an unrecognized read error
// is never re-read: it is the answer.
func (r *hookClaimClassRoute) routesRead(ctx context.Context, from hookStore, id, assignee string, readErr error) (bool, error) {
	if r == nil || !hookClaimBeadIsElsewhere(readErr) {
		return false, nil
	}
	return r.escalates(ctx, from, id, assignee)
}

// escalates finishes the decision once the leg that answered is proved not to
// hold id: no other work leg may hold it, and the binding must.
func (r *hookClaimClassRoute) escalates(ctx context.Context, from hookStore, id, assignee string) (bool, error) {
	if r.anotherWorkLegHolds(ctx, from, id, assignee) {
		return false, nil
	}
	return r.holds(id)
}

// legProvedAbsent reports whether from, the leg a claim-time write for id ran
// against, is proved not to hold the bead, given the error the write returned.
//
// The write's own not-found is the proof. Any other error is settled by reading
// the same leg through the caller's read seam (the package comment says why): a
// not-found there proves it, and a read that finds the bead or fails proves
// nothing. Three errors skip the read because they already answer it: a named
// wisp refusal is the leg saying it HOLDS the bead; a store that did not answer
// (workQueryStoreUnavailable: a timeout, a refused connection, contention) would
// only be asked again while it is struggling; and a context that is done cannot
// carry a read that would mean anything.
func (r *hookClaimClassRoute) legProvedAbsent(ctx context.Context, from hookStore, id, assignee string, workErr error) bool {
	switch {
	case workErr == nil:
		return false
	case hookClaimBeadIsElsewhere(workErr):
		return true
	case r.readLeg == nil, hookClaimBeadIsAWisp(workErr), ctx.Err() != nil, workQueryStoreUnavailable(workErr):
		return false
	}
	_, err := r.readLeg(ctx, from.dir, from.env, id, assignee)
	return hookClaimBeadIsElsewhere(err)
}

// anotherWorkLegHolds reports whether some work leg OTHER than the one that
// answered can resolve id.
//
// Only the primary leg runs the city-wide reader (scopeFederatedHookStores), so
// it is the only leg whose query can serve a bead it does not hold — and for a
// rig-scoped agent, the shipped shape of every worker that claims, the primary
// leg is the RIG store while the city work store is appended last. Without this,
// the rig leg's not-found would send a claim to the binding for a co-resident
// bead the city work leg holds, which is the ledger the federated reader
// resolved it to and the copy it keeps re-serving.
//
// A read that FAILS counts as "may hold it". The consequence of guessing wrong
// in that direction is one skipped bead, reclaimed next tick; the consequence of
// guessing wrong in the other is an ownership write in a ledger that is not
// where the reader is looking.
func (r *hookClaimClassRoute) anotherWorkLegHolds(ctx context.Context, from hookStore, id, assignee string) bool {
	id = strings.TrimSpace(id)
	if id == "" || r.readLeg == nil || len(r.workLegs) == 0 {
		return false
	}
	if held, ok := r.workHeld[id]; ok {
		return held
	}
	held := false
	for _, leg := range r.workLegs {
		if sameHookStore(leg, from) {
			continue
		}
		if _, err := r.readLeg(ctx, leg.dir, leg.env, id, assignee); err == nil || !errors.Is(err, beads.ErrNotFound) {
			held = true
			break
		}
	}
	r.workHeld[id] = held
	return held
}

// observeWorkLegs records the fan-out the federated claim loop is about to run,
// so a not-found from one leg can be checked against the others before it opens
// the escalation. It is the loop's one input to the route.
func (r *hookClaimClassRoute) observeWorkLegs(stores []hookStore) {
	if r == nil {
		return
	}
	r.workLegs = stores
}

// claim acquires a binding-resident bead through the closed graph contract,
// applying the same post-mutation classification hookClaimWithBdStore applies to
// the work store so the two claim paths report a lost race, a stale projection
// and a canonical-readback failure identically.
func (r *hookClaimClassRoute) claim(beadID, assignee string) (beads.Bead, bool, error) {
	return hookClaimThroughStore(beadID, assignee,
		func() (beads.Bead, bool, error) { return r.graph.Claim(beadID, assignee) },
		r.graph.Get)
}

// listContinuation reads a continuation group out of the binding and records
// every member as resident, so the per-sibling assignment that follows does not
// re-probe (or re-fail) one bd subprocess at a time.
func (r *hookClaimClassRoute) listContinuation(rootID, group string) ([]beads.Bead, error) {
	siblings, err := r.graph.List(beads.ListQuery{
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:        rootID,
			beadmeta.ContinuationGroupMetadataKey: group,
		},
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, fmt.Errorf("listing continuation group %q of %q in the relocated class binding: %w", group, rootID, err)
	}
	for _, sibling := range siblings {
		if id := strings.TrimSpace(sibling.ID); id != "" {
			r.resident[id] = true
		}
	}
	return siblings, nil
}

// emitExecutionStepStarted records the durable lifecycle-start fact against the
// binding that owns the step.
//
// It mirrors hookEmitExecutionStepStarted, whose own comment asserts that "the
// hook's bd context owns both the claimed graph step and its workflow root" —
// true on a single-store city and false on a split one, where both live in the
// binding and the work-directory bd context can read neither. Routing it is what
// makes that sentence true again; EmitLifecycle still performs the authoritative
// graph.v2 root validation before recording anything.
func (r *hookClaimClassRoute) emitExecutionStepStarted(step beads.Bead) {
	var rec events.Recorder
	if r.recorder != nil {
		rec = r.recorder()
	} else {
		rec = openCityRecorder(io.Discard)
	}
	if closer, ok := rec.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // lifecycle events are best-effort
	}
	_ = executionevent.EmitLifecycle(rec, r.class, events.ExecutionStepStarted, step, eventActor())
}

// classRoutedHookClaimOps returns ops whose claim-time writes fall back to the
// relocated coordination-class binding for a bead the agent's work store does
// not hold.
//
// A nil route returns ops UNCHANGED — not a wrapper that always delegates — so a
// single-store city runs the identical function values it runs today and pays
// nothing for a seam it cannot use.
//
// Every wrapped seam applies the same rule: run the work-scope write; escalate
// only on the not-found that proves the bead is not there; take the binding only
// when it is proved to hold the id. The one exception is ListContinuation, which
// is a QUERY and has no not-found to escalate on — see below.
func classRoutedHookClaimOps(ops hookClaimOps, route *hookClaimClassRoute) hookClaimOps {
	if route == nil {
		return ops
	}
	ops.applyDefaults()
	base := ops
	// The route reads OTHER work legs through the caller's own read seam before
	// it escalates, so the residence question is asked with the same bd context
	// the claim would have used. Carrying the route on the ops is how
	// claimHookWorkWithRunner hands it the leg set.
	route.readLeg = base.ReadWorkMeta
	ops.ClassRoute = route

	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		if route.knownResident(beadID) {
			return route.claim(beadID, assignee)
		}
		// The work-scope claim is the first leg of the escalation: a
		// not-found here is the signal to probe the binding, so the call is
		// marked federated.
		var claimed beads.Bead
		var ok bool
		err := beads.FederatedRead(func() error {
			var claimErr error
			claimed, ok, claimErr = base.Claim(ctx, dir, env, beadID, assignee)
			return claimErr
		})
		leg := hookStore{dir: dir, env: env}
		if ok {
			// The work store's CAS COMMITTED. Its error, if any, is the
			// canonical readback failing on a bead this session now owns HERE —
			// the one signal claimFirstReadyHookAssignment and
			// claimFirstEligibleHookCandidate both stop on. Escalating it would
			// claim one logical bead in two ledgers and erase that signal, so
			// the work store's answer is returned exactly as it came back.
			return claimed, ok, err
		}
		routed, probeErr := route.routes(ctx, leg, beadID, assignee, err)
		switch {
		case probeErr != nil:
			return beads.Bead{}, false, probeErr
		case routed:
			return route.claim(beadID, assignee)
		default:
			return claimed, ok, err
		}
	}

	ops.StampWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error {
		write := func() error { return route.graph.Update(beadID, beads.UpdateOpts{Metadata: patch}) }
		if route.knownResident(beadID) {
			return write()
		}
		// First leg of the escalation; a not-found sends the write to the binding.
		err := beads.FederatedRead(func() error { return base.StampWorkMeta(ctx, dir, env, beadID, assignee, patch) })
		routed, probeErr := route.routes(ctx, hookStore{dir: dir, env: env}, beadID, assignee, err)
		switch {
		case probeErr != nil:
			return probeErr
		case routed:
			return write()
		default:
			return err
		}
	}

	ops.ReadWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, error) {
		if route.knownResident(beadID) {
			return route.graph.Get(beadID)
		}
		// First leg of the escalation; a not-found sends the read to the binding.
		var bead beads.Bead
		err := beads.FederatedRead(func() error {
			var readErr error
			bead, readErr = base.ReadWorkMeta(ctx, dir, env, beadID, assignee)
			return readErr
		})
		routed, probeErr := route.routesRead(ctx, hookStore{dir: dir, env: env}, beadID, assignee, err)
		switch {
		case probeErr != nil:
			return beads.Bead{}, probeErr
		case routed:
			return route.graph.Get(beadID)
		default:
			return bead, err
		}
	}

	// A continuation LIST is the one claim-time call with no not-found to
	// escalate on: a query against a store that holds no member of the group
	// returns an empty list, not an error. Escalating on EMPTY is what keeps
	// this honest — the work store's answer is never replaced, only an answer
	// it had nothing to say about is filled, and only when the binding is
	// proved to hold the group's root. A list error still fails loud: returning
	// an empty list from the wrong store because a read failed is precisely the
	// silent-empty this seam must not reproduce.
	//
	// It is also the one seam the fan-out arming does not gate, because its
	// proof is already stronger than a single leg's not-found: an EMPTY group
	// says this store holds no member at all, so no work leg's copy can be
	// preempted by filling it, and the preassignment that follows writes to the
	// store the list came from. It runs on the terminal path, after a claim has
	// already resolved which ledger owns the work.
	ops.ListContinuation = func(ctx context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
		if route.knownResident(rootID) {
			return route.listContinuation(rootID, group)
		}
		// First leg; an EMPTY answer sends the query to the binding.
		var siblings []beads.Bead
		err := beads.FederatedRead(func() error {
			var listErr error
			siblings, listErr = base.ListContinuation(ctx, dir, env, rootID, group)
			return listErr
		})
		if err != nil || len(siblings) > 0 {
			return siblings, err
		}
		held, probeErr := route.holds(rootID)
		switch {
		case probeErr != nil:
			return nil, probeErr
		case held:
			return route.listContinuation(rootID, group)
		default:
			return siblings, nil
		}
	}

	ops.AssignContinuation = func(ctx context.Context, dir string, env []string, beadID, assignee string) error {
		write := func() error { return route.graph.Update(beadID, beads.UpdateOpts{Assignee: &assignee}) }
		if route.knownResident(beadID) {
			return write()
		}
		// First leg of the escalation; a not-found sends the write to the binding.
		err := beads.FederatedRead(func() error { return base.AssignContinuation(ctx, dir, env, beadID, assignee) })
		routed, probeErr := route.routes(ctx, hookStore{dir: dir, env: env}, beadID, assignee, err)
		switch {
		case probeErr != nil:
			return probeErr
		case routed:
			return write()
		default:
			return err
		}
	}

	// The release of an undelivered claim (F-C) must run against the ledger the
	// claim landed in, or it would clear an assignee in a store that never held
	// one and leave the real claim parked. Like the lifecycle emission below it
	// routes on the MEMO alone and never probes: a bead this invocation did not
	// route is one the work store claimed, and the release is a consequence of
	// that claim rather than a second opinion about where the bead lives.
	ops.Release = func(ctx context.Context, dir string, env []string, beadID, assignee string) (bool, error) {
		if route.knownResident(beadID) {
			return route.graph.ReleaseIfCurrent(beadID, assignee)
		}
		return base.Release(ctx, dir, env, beadID, assignee)
	}

	// An adoption re-stamp of a bead resident in the relocated graph store has
	// no conditional-transfer primitive there, so it reports unsupported and
	// the bead is adopted as-is (the pre-re-stamp behavior). Anything else
	// is the work store's, like the release above.
	//
	// The unsupported answer is tagged with errRestampGraphResident, because the
	// caller's decision turns on WHERE the bead lives rather than on the error:
	// the identical beads.ErrConditionalTransferUnsupported from the work store
	// (a bd below the --if-assignee floor) means the opposite — a bead whose
	// close bd will fence on the spelling that could not be moved — and must
	// refuse adoption. See restampHookAdoption.
	ops.RestampAdopted = func(ctx context.Context, dir string, env []string, beadID, fromAssignee, toAssignee string) (bool, error) {
		if route.knownResident(beadID) {
			return false, errRestampGraphResident
		}
		return base.RestampAdopted(ctx, dir, env, beadID, fromAssignee, toAssignee)
	}

	// The lifecycle-start emission reads the step's workflow root, so it belongs
	// in the store the claim landed in. It routes on the MEMO alone and never
	// probes: a step this invocation did not route is one the work store
	// answered for, and emitting it anywhere else would be a second opinion
	// about ownership rather than a consequence of the claim.
	ops.EmitExecutionStepStarted = func(step beads.Bead, dir string, env []string, assignee string) {
		if !route.knownResident(step.ID) {
			base.EmitExecutionStepStarted(step, dir, env, assignee)
			return
		}
		route.emitExecutionStepStarted(step)
	}

	// The workflow stop gate reads a step's ancestry, and closes a stranded
	// step, in the ledger the step lives in. Like the emission above it routes
	// on the MEMO alone: before a claim nothing is known resident, so a graph
	// step's pre-claim read finds nothing in the work store and fails open, and
	// the post-claim re-check — after the route memoized the step — reads the
	// binding.
	ops.WorkflowStore = func(ctx context.Context, dir string, env []string, beadID, actor string) beads.Store {
		if route.knownResident(beadID) {
			return route.class
		}
		return base.WorkflowStore(ctx, dir, env, beadID, actor)
	}

	// Done-workflow retirement (R6, the #6475 treadmill). A graph.v2 root and
	// its steps live in the binding while the synthetic input convoy and the
	// work it tracks stay in the work store, so the check reads ACROSS the
	// two: the root and its step seats from the store that holds the root,
	// the convoy and its members from the work leg. Residence of the root is
	// decided by the same read the claim-time seams use — the work leg first,
	// the binding only on a proven absence — so a work-resident workflow
	// keeps the exact unrouted call.
	ops.InputDone = func(ctx context.Context, dir string, env []string, candidate beads.Bead, identityCandidates []string) (string, bool, error) {
		rootID := workflowRootIDForInputCheck(candidate)
		if rootID == "" {
			return base.InputDone(ctx, dir, env, candidate, identityCandidates)
		}
		resident, err := route.resolvesToBinding(ctx, base, dir, env, rootID)
		if err != nil {
			return rootID, false, err
		}
		if !resident {
			return base.InputDone(ctx, dir, env, candidate, identityCandidates)
		}
		work := base.WorkflowStore(ctx, dir, env, rootID, "")
		return workflowInputDoneAcross(route.class, work, candidate, identityCandidates)
	}

	ops.SkipDoneWorkflow = func(ctx context.Context, dir string, env []string, rootID string) error {
		resident, err := route.resolvesToBinding(ctx, base, dir, env, rootID)
		if err != nil {
			return err
		}
		if resident {
			return skipDoneWorkflow(route.class, rootID)
		}
		return base.SkipDoneWorkflow(ctx, dir, env, rootID)
	}

	// A stale-lease reclaim of a binding-resident bead belongs to the binding.
	// Beads, not gc, is the authority on staleness (ga-7rj87d NFR3), so a
	// binding whose engine exposes no stale-lease reclaim reclaims nothing:
	// the candidate is left untouched (FR4), and gc's dead-assignee release is
	// what recovers it, exactly as for every other binding-resident claim.
	ops.ReclaimStale = func(ctx context.Context, dir string, env []string, beadID string) (bool, string, error) {
		reclaim := func() (bool, string, error) {
			reclaimer, ok := route.class.(interface {
				ReclaimStale(id string) (bool, string, error)
			})
			if !ok {
				return false, "", nil
			}
			return reclaimer.ReclaimStale(beadID)
		}
		if route.knownResident(beadID) {
			return reclaim()
		}
		reclaimed, previousOwner, err := base.ReclaimStale(ctx, dir, env, beadID)
		routed, probeErr := route.routes(ctx, hookStore{dir: dir, env: env}, beadID, "", err)
		switch {
		case probeErr != nil:
			return false, "", probeErr
		case routed:
			return reclaim()
		default:
			return reclaimed, previousOwner, err
		}
	}

	return ops
}

// resolvesToBinding reports whether id is resident in the binding rather than
// in the work leg, by the claim-time read rule: a bead this invocation already
// proved resident is the binding's; otherwise the work leg is read first, and
// only its proven absence opens the binding probe. A work-leg read that finds
// the bead keeps it the work store's.
//
// An id inside a reserved class namespace skips the work read: no work store
// mints under those prefixes, so the read could only miss, and on the hook's
// latency budget it is one bd subprocess per candidate for nothing. The
// binding is the authority for those ids (holds surfaces its failures).
func (r *hookClaimClassRoute) resolvesToBinding(ctx context.Context, base hookClaimOps, dir string, env []string, id string) (bool, error) {
	if r.knownResident(id) {
		return true, nil
	}
	if base.ReadWorkMeta == nil || bdIDIsClassReserved(id) { // residency:allow — skips a work read no work store can answer; the binding probe below still decides
		return r.holds(id)
	}
	_, err := base.ReadWorkMeta(ctx, dir, env, id, "")
	if err == nil {
		return false, nil
	}
	return r.routesRead(ctx, hookStore{dir: dir, env: env}, id, "", err)
}

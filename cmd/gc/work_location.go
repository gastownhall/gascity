package main

// Typed work location (ARCH-RESTRUCTURE-2 R9, the K-b half).
//
// "Which stores hold this seat's work?" used to be answered from whatever
// beads.Store a caller held. The legacy tick and v2's L5 held the SESSIONS
// store, which on a split city is a binding, and read it as the work store:
// seats closed holding claims the city work store carried (mc-3ixn3.16,
// NEW2-1). #7480 fixed it by substitution: a registered city's work leg was
// swapped in whatever the caller passed, and a per-tick index was found by
// store pointer.
//
// Here the leg set is a value. A WorkLegs is minted only from a cityWorkLeg,
// and a cityWorkLeg only from a beads.WorkStore: a beads.SessionStore is a
// different type, and the worklegs analyzer (tools/nogo) refuses a WorkStore
// built from one, a WorkLegs or cityWorkLeg field set outside this file, any
// reference to the two mints outside their reviewed sites, and any workScope
// but the two polarity scopes. Every seat-work question takes the SeatWork
// read over a WorkLegs (seat_work.go): the tick mints one per pass, a v2
// effect one per effect, a one-shot command one per call.

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// cityWorkLeg is a city's work store, the one store a WorkLegs takes as its
// work leg. Its store sits under a named field, so no store type converts to
// it.
type cityWorkLeg struct{ store beads.Store }

// cityWorkLegOf is the city's work store as its work leg. It takes the typed
// work-class handle, so a sessions store reaches it only through a WorkStore
// built from one, which the worklegs analyzer refuses.
func cityWorkLegOf(work beads.WorkStore) cityWorkLeg {
	return cityWorkLeg{store: work.Store}
}

// WorkLegs is the leg set every seat-work question reads: the city work
// store, the serving rigs, then every relocated class binding, in plan order
// (storeref.AssignedWork). It is minted only by workLegsFromCensus.
type WorkLegs struct {
	cfg  *config.City
	work beads.Store
	rigs map[string]beads.Store // the serving rigs
	plan storeref.ResolvedPlan
	topo storeref.Topology // the residency topology plan was built from, for by-id reads
	err  error             // no leg set: a refused city
	// suspended names the declared, open rigs left out of rigs because they
	// are suspended (servingRigStores).
	suspended []string
	// cityName is the name a "city:<name>" store ref of this city carries.
	cityName string
}

// errNoWorkStore: a WorkLegs minted without a city work store, which answers
// every question unknown.
var errNoWorkStore = errors.New("no city work store to read work from")

// workLegsFromCensus is the one WorkLegs constructor, and its legs are the
// census's: the city work store, the SERVING rigs (declared in city.toml,
// open, not suspended: servingRigStores), then every relocated class binding
// this process serves for cityPath (residencyTopologyForCity). Every seat
// reads every leg, whatever its agent's scope: a gate that read fewer legs
// than the cascade released from decided "no work" on stores it never saw.
func workLegsFromCensus(cityPath string, cfg *config.City, work cityWorkLeg, rigs map[string]beads.Store) WorkLegs {
	l := WorkLegs{cfg: cfg, work: work.store}
	if work.store == nil {
		return l
	}
	if cfg != nil {
		l.cityName = loadedCityName(cfg, cityPath)
	}
	suspendedPaths := buildSuspendedRigPathsForCity(cfg, cityPath)
	l.rigs = servingRigStores(cfg, rigs, suspendedPaths)
	l.suspended = suspendedOpenRigs(cfg, rigs, suspendedPaths)
	l.topo = residencyTopologyForCity(cityPath, cfg, work.store, l.rigs)
	l.plan, l.err = storeref.Plan(storeref.AssignedWork{}, l.topo)
	return l
}

// workLegs is the controller's leg set for one tick, over the store it
// registers as the city's work leg (registerResidencyRoutes).
//
// It reads the published config snapshot rather than cr.cfg, so the wisp GC's
// work-arm lane, which runs off the tick, can take its legs from here too.
func (cr *CityRuntime) workLegs() WorkLegs {
	return workLegsFromCensus(cr.cityPath, cr.serviceConfigSnapshot(), cityWorkLegOf(cr.cityWorkStore()), cr.rigBeadStores()) // residency:allow — the census frame; workLegsFromCensus plans the legs (storeref.Plan)
}

// suspendedRigs names the open rigs this leg set leaves out because they are
// suspended.
func (l WorkLegs) suspendedRigs() []string { return l.suspended }

// storeForRef resolves a source store-ref ("city:<name>", "rig:<name>", a
// class binding ref) to the leg it names: the city work store for a city or
// binding ref, the rig's store for a serving rig. A suspended rig reports
// suspended=true (it is not a leg, and is not read); any other ref, ok=false.
func (l WorkLegs) storeForRef(ref string) (beads.Store, bool, bool) {
	ref = strings.TrimSpace(ref)
	rig, ok := storeref.ScopeRigContext(ref)
	if !ok {
		return nil, false, false
	}
	if rig == "" {
		// Only THIS city's ref names the city work store; a class binding
		// ref ("class:<token>") is city-scoped too, and fails this check.
		if l.cityName == "" || ref != "city:"+l.cityName {
			return nil, false, false
		}
		return l.work, false, l.work != nil
	}
	for _, name := range l.suspended {
		if name == rig {
			return nil, true, true
		}
	}
	store, ok := l.rigs[rig]
	return store, false, ok && store != nil
}

// suspendedOpenRigs names the declared rigs with an open store that are
// suspended: the complement of servingRigStores among the open rigs.
func suspendedOpenRigs(cfg *config.City, rigs map[string]beads.Store, suspendedRigPaths map[string]bool) []string {
	if cfg == nil || len(rigs) == 0 || len(suspendedRigPaths) == 0 {
		return nil
	}
	var out []string
	for _, rig := range cfg.Rigs {
		if !suspendedRigPaths[filepath.Clean(rig.Path)] {
			continue
		}
		if store, ok := rigs[rig.Name]; ok && store != nil {
			out = append(out, rig.Name)
		}
	}
	return out
}

// unusable is why l answers nothing: no leg set, or the zero WorkLegs,
// which no constructor minted. Every question on it is unknown.
func (l WorkLegs) unusable() error {
	if l.work == nil && l.err == nil {
		return errNoWorkStore
	}
	return l.err
}

// newSeatWork is the controller's read for one tick (or one dispatcher pass),
// raising session.stranded_claims through the controller's one alerter and
// logging close refusals through its one refusal log.
func (cr *CityRuntime) newSeatWork() *SeatWork {
	sw := newSeatWork(cr.workLegs())
	sw.alerts = cr.strandedClaimAlerter()
	sw.refusals = &cr.closeRefusals
	return sw
}

// strandedClaimAlerter is the controller's one strandedClaimAlerts.
func (cr *CityRuntime) strandedClaimAlerter() *strandedClaimAlerts {
	cr.strandedAlertsOnce.Do(func() { cr.strandedAlerts = &strandedClaimAlerts{rec: cr.rec} })
	return cr.strandedAlerts
}

// each visits every leg's store in plan order.
func (l WorkLegs) each(visit func(beads.Store)) {
	l.eachLeg(func(_ string, store beads.Store) { visit(store) })
}

// eachLeg visits every leg's store ref and store in plan order.
func (l WorkLegs) eachLeg(visit func(ref string, store beads.Store)) {
	if l.unusable() != nil {
		return
	}
	storeref.EachLeg(l.plan, func(leg storeref.Leg, _ storeref.Role, _ storeref.ErrPolicy) { // residency:allow — enumerates the plan's own legs
		visit(string(leg.Ref), leg.Store)
	})
}

// readOnly is l with every leg's store refusing writes: what a v2 effect's
// sections hold (capReadStores).
func (l WorkLegs) readOnly() WorkLegs {
	ro := func(s beads.Store) beads.Store { return readOnlyStore{blindWriteRefusingStore{inner: s}} }
	if l.work != nil {
		l.work = ro(l.work)
	}
	legs := make([]storeref.PlanLeg, len(l.plan.Legs))
	for i, leg := range l.plan.Legs { // residency:allow — wraps the plan's own legs in place; order, roles and policies kept
		leg.Leg.Store = ro(leg.Leg.Store) // residency:allow — the same leg, read-only
		legs[i] = leg
	}
	l.plan.Legs = legs
	return l
}

// workStore is l's city work store, for a by-ID residency topology (the
// live-claim veto, the confirmed-orphan release), which plans its own legs.
func (l WorkLegs) workStore() beads.Store { return l.work }

// readLive reads bead id live from the store that owns it by residency
// (byIDOwnerRowForTopology over l's topology): a relocated class binding
// before the work ledger, so a stale pre-migration copy co-resident in the
// ledger never answers, then the serving rigs and the work store. It reads
// through that store's Live handle, past any cache. No store ref is
// consulted: a slot's gc.trigger_bead_store_ref names the demand leg, which on
// a split city is the work ledger even for a step that lives only in the
// binding (ga-bmfmt, the warm-bind probe's trap). found is false on a clean
// miss; any other failure is err, which a caller needing proof reads as none.
func (l WorkLegs) readLive(id string) (beads.Bead, bool, error) {
	if err := l.unusable(); err != nil {
		return beads.Bead{}, false, err
	}
	if strings.TrimSpace(id) == "" {
		return beads.Bead{}, false, nil
	}
	owner, err := byIDOwnerRowForTopology(l.topo, id)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return beads.Bead{}, false, nil
	case err != nil:
		return beads.Bead{}, false, err
	case owner.Store == nil:
		return beads.Bead{}, false, nil
	}
	live := beads.HandlesFor(owner.Store).Live
	if live == nil {
		return beads.Bead{}, false, nil
	}
	b, err := live.Get(id)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return beads.Bead{}, false, nil
	case err != nil:
		return beads.Bead{}, false, err
	}
	return b, b.ID == id, nil
}

// isWork reports whether store is l's city work store.
func (l WorkLegs) isWork(store beads.Store) bool {
	return l.work != nil && unwrapClassStore(store) == unwrapClassStore(l.work)
}

// workScope is the identity set a seat-work question matches assignees
// against. Only the two polarity types implement it (pass-1 O4; the
// worklegs analyzer refuses any other), so a question names which set is
// safe for what it decides. refuses reports the wide polarity: a question
// that gates ending a live runtime, which no snapshot may answer.
type workScope interface {
	scopeIDs() []string
	refuses() bool
}

// ReleaseScope is the narrow set: the bead ID, session_name, the configured
// named identity and the stable alias (sessionAssignmentIdentifiersForConfig).
// It is the only set work is released under, and it gates closing a seat
// whose runtime is dead. A transient pool slot alias is never in it.
type ReleaseScope struct{ ids []string }

// RefuseScope is the wide set: ReleaseScope plus the current alias and the
// alias history. It gates acts that end a live runtime (the drain-ack
// escalation, the pool-slot retire): over-refusing wedges a row, under-
// refusing ends a live agent's turn.
type RefuseScope struct{ ids []string }

func (s ReleaseScope) scopeIDs() []string { return s.ids }
func (s RefuseScope) scopeIDs() []string  { return s.ids }
func (ReleaseScope) refuses() bool        { return false }
func (RefuseScope) refuses() bool         { return true }

// releaseScope is info's ReleaseScope.
func releaseScope(info sessionpkg.Info, cfg *config.City) ReleaseScope {
	return ReleaseScope{ids: sessionAssignmentIdentifiersForConfigInfo(info, cfg)}
}

// releaseScopeOfBead is releaseScope for a caller holding the raw bead.
func releaseScopeOfBead(b beads.Bead, cfg *config.City) ReleaseScope {
	return ReleaseScope{ids: sessionAssignmentIdentifiersForConfig(b, cfg)}
}

// releaseScopeOwn is b's own persisted identities (ID, session_name, the
// configured named identity), without the config-derived stable alias: the
// drain-ack (D5) and retired-session releases' set, a subset of
// releaseScopeOfBead.
func releaseScopeOwn(b beads.Bead) ReleaseScope {
	return ReleaseScope{ids: sessionAssignmentIdentifiers(b)}
}

// except drops the identities in preserve: work held under them is not this
// seat's to release (closeBeadPreservingAssignees).
func (s ReleaseScope) except(preserve map[string]struct{}) ReleaseScope {
	if len(preserve) == 0 {
		return s
	}
	var ids []string
	for _, id := range s.ids {
		if _, skip := preserve[strings.TrimSpace(id)]; !skip {
			ids = append(ids, id)
		}
	}
	return ReleaseScope{ids: ids}
}

// refuseScope is info's RefuseScope (O4): every identity the seat may hold
// work under, its alias and alias history included
// (drainAckAssigneeIdentities), but never a transient pool slot alias it no
// longer holds. The controller rebinds a slot to the next occupant, so a
// prior slot alias names a chair, and honoring it would let one seat's claim
// keep another alive (TestAssignmentGuardsIgnoreTransientPoolSlotAliases).
// The current alias stays: a legacy slot-aliased agent claims under it
// (AssigneeIdentifier is alias-first), so its own work keeps it. A seat
// whose agent does not resolve keeps its history too: wide, in front of a
// kill.
func refuseScope(info sessionpkg.Info, cfg *config.City) RefuseScope {
	if isPoolManagedSessionInfo(info) && strings.TrimSpace(info.PoolSlot) != "" {
		if agent := findAgentByTemplate(cfg, normalizedSessionTemplateInfo(info, cfg)); agent != nil && usesTransientPoolSlotIdentity(agent) {
			ids := append(sessionAssignmentIdentifiersForConfigInfo(info, cfg), strings.TrimSpace(info.Alias))
			return RefuseScope{ids: compactSessionAssignmentIdentifiers(ids)}
		}
	}
	return RefuseScope{ids: drainAckAssigneeIdentities(info, cfg)}
}

// legsTriggerResolver reads a bead by id from the store that owns it among
// legs (WorkLegs.readLive: the class binding, the serving rigs, the city work
// store), for the start path's trigger reads. nil when legs are unusable (a
// refused city, or no work store), which keeps the sessions-store read.
func legsTriggerResolver(legs WorkLegs) warmClaimTriggerResolver {
	if legs.unusable() != nil {
		return nil
	}
	return func(id string) (beads.Bead, error) {
		b, found, err := legs.readLive(id)
		switch {
		case err != nil:
			return beads.Bead{}, err
		case !found:
			return beads.Bead{}, fmt.Errorf("bead %q: %w", id, beads.ErrNotFound)
		}
		return b, nil
	}
}

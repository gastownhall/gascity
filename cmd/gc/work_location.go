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
	plan storeref.ResolvedPlan
	err  error // no leg set: a refused city
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
	serving := servingRigStores(cfg, rigs, buildSuspendedRigPathsForCity(cfg, cityPath))
	l.plan, l.err = storeref.Plan(storeref.AssignedWork{}, residencyTopologyForCity(cityPath, cfg, work.store, serving))
	return l
}

// workLegs is the controller's leg set for one tick, over the store it
// registers as the city's work leg (registerResidencyRoutes).
func (cr *CityRuntime) workLegs() WorkLegs {
	return workLegsFromCensus(cr.cityPath, cr.cfg, cityWorkLegOf(cr.cityWorkStore()), cr.rigBeadStores()) // residency:allow — the census frame; workLegsFromCensus plans the legs (storeref.Plan)
}

// unusable is why l answers nothing: no leg set, or the zero WorkLegs,
// which no constructor minted. Every question on it is unknown.
func (l WorkLegs) unusable() error {
	if l.work == nil && l.err == nil {
		return errNoWorkStore
	}
	return l.err
}

// each visits every leg's store in plan order.
func (l WorkLegs) each(visit func(beads.Store)) {
	if l.unusable() != nil {
		return
	}
	storeref.EachLeg(l.plan, func(leg storeref.Leg, _ storeref.Role, _ storeref.ErrPolicy) { // residency:allow — enumerates the plan's own legs
		visit(leg.Store)
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
// (drainAckAssigneeIdentities), but never a transient pool slot alias. The
// controller rebinds a slot to the next occupant, so slot-form work names a
// chair, and honoring it would let one seat's claim keep another alive
// (TestAssignmentGuardsIgnoreTransientPoolSlotAliases). A seat whose
// configured agent hands out transient slots has no other alias; one whose
// agent does not resolve keeps its aliases: wide, in front of a kill.
func refuseScope(info sessionpkg.Info, cfg *config.City) RefuseScope {
	if isPoolManagedSessionInfo(info) && strings.TrimSpace(info.PoolSlot) != "" {
		if agent := findAgentByTemplate(cfg, normalizedSessionTemplateInfo(info, cfg)); agent != nil && usesTransientPoolSlotIdentity(agent) {
			return RefuseScope{ids: sessionAssignmentIdentifiersForConfigInfo(info, cfg)}
		}
	}
	return RefuseScope{ids: drainAckAssigneeIdentities(info, cfg)}
}

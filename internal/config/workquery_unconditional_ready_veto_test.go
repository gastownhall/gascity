package config

import (
	"strings"
	"testing"
)

// This file is the RED step for ga-g4odhq: the default hook work_query's
// ready-reader must route through `gc ready` — which runs the already-shipped
// (ga-beg8uc / ga-a7v0ex) Go-side gc.work_outcome veto — even for a
// single-store city, not only when FederatedReady requires it. Today
// readyReaderCommand only takes the gc-ready branch when federated; a
// single-store city's work_query shells bare `bd ready`, which has no notion
// of gc.work_outcome and cannot see the veto at all.
//
// See ga-ez8vj8 for the architecture ruling and the confirmed live incident:
// ga-poygzp.2 closed gc.work_outcome=blocked at 21:47:39Z on 2026-09-22; its
// blocks-dependent ga-8noaen was claimed by gascity/builder about twenty
// minutes later, consistent with bd ready's ordinary closed-therefore-ready
// computation and not with a Go-side veto having run.

// fakeBDNaivelyServesBlocked is a `bd` stand-in for the single-store tiers:
// it answers `ready` the way bd's own native readiness computation does
// today — blind to gc.work_outcome — by serving a bead whose real blocker
// closed work_outcome=blocked, which a Go-side veto would have excluded.
const fakeBDNaivelyServesBlocked = `#!/bin/sh
case "$1" in
  ready) printf '[{"id":"ga-8noaen","status":"open","issue_type":"task"}]' ;;
  *) printf '[]' ;;
esac
`

// fakeGCVetoesBlocked is a `gc` stand-in representing the already-shipped
// Go-side work-outcome veto (ga-a7v0ex): it excludes the same bead
// fakeBDNaivelyServesBlocked naively serves, because the real `gc ready`
// would have seen the blocker's gc.work_outcome=blocked and vetoed the edge.
const fakeGCVetoesBlocked = `#!/bin/sh
case "$1" in
  ready) printf '[]' ;;
  *) printf '[]' ;;
esac
`

// fakeBDServesOrdinary and fakeGCServesOrdinary agree with each other: both
// serve the same bead, standing in for a blocker that did NOT close
// work_outcome=blocked (empty, shipped, no-op, ...), where no veto applies
// and either reader must still report the dependent ready (AC#3).
const fakeBDServesOrdinary = `#!/bin/sh
case "$1" in
  ready) printf '[{"id":"ga-ordinary-1","status":"open","issue_type":"task"}]' ;;
  *) printf '[]' ;;
esac
`

const fakeGCServesOrdinary = `#!/bin/sh
case "$1" in
  ready) printf '[{"id":"ga-ordinary-1","status":"open","issue_type":"task"}]' ;;
  *) printf '[]' ;;
esac
`

// TestSingleStoreWorkQueryAppliesWorkOutcomeVeto reproduces the incident
// shape from ga-ez8vj8 against the real generated single-store work_query
// (AC#1, AC#2). It must route through `gc ready` (which vetoes a
// work_outcome=blocked blocker) rather than bare `bd ready` (which cannot
// see gc.work_outcome at all and reports the dependent naively ready).
func TestSingleStoreWorkQueryAppliesWorkOutcomeVeto(t *testing.T) {
	a := &Agent{Name: "worker"}
	command := a.EffectiveWorkQueryFor(singleStoreTopology())
	res := runGeneratedQueryWithBD(t, command, map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
	}, fakeGCVetoesBlocked, fakeBDNaivelyServesBlocked)

	if strings.Contains(res.stdout, "ga-8noaen") {
		t.Fatalf("single-store work_query surfaced %q, a dependent whose blocker closed gc.work_outcome=blocked; the query must route through gc ready (which vetoes it) rather than bare bd ready (which cannot see gc.work_outcome): stdout=%q", "ga-8noaen", res.stdout)
	}
}

// TestSingleStoreWorkQueryStillServesAnOrdinaryClose is the AC#3 companion:
// a dependent whose blocker closed with work_outcome empty or non-blocked
// must still surface ready after the fix, exactly as it does today.
func TestSingleStoreWorkQueryStillServesAnOrdinaryClose(t *testing.T) {
	a := &Agent{Name: "worker"}
	command := a.EffectiveWorkQueryFor(singleStoreTopology())
	res := runGeneratedQueryWithBD(t, command, map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
	}, fakeGCServesOrdinary, fakeBDServesOrdinary)

	if !strings.Contains(res.stdout, "ga-ordinary-1") {
		t.Errorf("single-store work_query stdout = %q, want the bead the reader served — a blocker that did not close work_outcome=blocked must still satisfy the edge", res.stdout)
	}
}

// TestReadyReaderCommandIsAlwaysGCReady is the shared-function-level pin
// (AC#1, AC#5): every one of readyReaderCommand's call sites (Work,
// AssignedReady, RoutedPool, PoolDemand) must ask for gc ready, on every
// topology. Per ga-beg8uc's already-settled decision that the Go-side
// work-outcome veto must always run, "do I need to federate" and "does
// correctness require the veto" are no longer the same question, and only
// the answer to the second one is left deciding this.
func TestReadyReaderCommandIsAlwaysGCReady(t *testing.T) {
	for _, federated := range []bool{false, true} {
		if got := readyReaderCommand(federated); got != gcReadyCommand {
			t.Errorf("readyReaderCommand(%v) = %q, want %q — a single-store city must also apply the Go-side gc.work_outcome veto (ga-beg8uc)", federated, got, gcReadyCommand)
		}
	}
}

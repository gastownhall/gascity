package main

import (
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// detectStrandedClaims is the stranded-work detector (owner ruling O5,
// revised). A close that keeps a claim no lane serves stamps it with the
// closed seat (strand); on D6's cadence the detector lists the stamped claims
// on every work leg (beadmeta.StrandedMetadataKey, one cached query per
// leg), keeps those still assigned, held by no other open session and released by
// no sweep (the dead-assignee sweep's rule, laneDirect), and raises
// session.stranded_claims for each seat; a seat it no longer finds (its
// claims rerouted, released or closed: a release clears the stamp) ends its
// episode. The stamp covers what a seat's identities cannot find: a claim
// under an alias, on an archived row, or with no route of its own. A leg it
// cannot read ends no episode. A seat is read once per process, on first
// sight (sessions).
func detectStrandedClaims(legs WorkLegs, sessions beads.Store, open []session.Info, alerts *strandedClaimAlerts, now time.Time) {
	if alerts == nil || legs.unusable() != nil || !alerts.scanDue(now) {
		return
	}
	owners := make(map[string][]string) // assignee → the open sessions answering to it
	for _, info := range open {
		for _, id := range sessionBeadAssigneeIdentitiesInfo(info) {
			owners[id] = append(owners[id], info.ID)
		}
	}
	// heldElsewhere reports an open session other than seat answering to
	// assignee: the claim is that session's now. The seat itself may still be
	// open (a removed named session's archived row).
	heldElsewhere := func(assignee, seat string) bool {
		return slices.ContainsFunc(owners[assignee], func(id string) bool { return id != seat })
	}
	held := make(map[string][]strandedClaim)
	complete := true
	legs.each(func(store beads.Store) {
		rows, err := store.List(beads.ListQuery{Metadata: map[string]string{beadmeta.StrandedMetadataKey: "true"}, TierMode: beads.TierBoth})
		if err != nil {
			complete = false
			return
		}
		for _, b := range rows {
			assignee := strings.TrimSpace(b.Assignee)
			seat := strings.TrimSpace(b.Metadata[beadmeta.StrandedSessionMetadataKey])
			if seat == "" || assignee == "" || heldElsewhere(assignee, seat) || (b.Status != "open" && b.Status != "in_progress") {
				continue
			}
			if route, served := releaseRoute(legs.cfg, b, "", laneDirect); !served {
				held[seat] = append(held[seat], strandedClaim{id: b.ID, route: route, reason: keptUnservedRoute})
			}
		}
	})
	for id, claims := range held {
		slices.SortFunc(claims, func(a, b strandedClaim) int { return strings.Compare(a.id, b.id) })
		seat := alerts.seat(sessions, id)
		alerts.observe(seat.seat, seat.kind, claims, now)
	}
	if complete {
		alerts.sweep(held)
	}
}

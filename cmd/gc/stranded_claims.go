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
// leg), keeps those still assigned as they were kept, to a seat not open
// again, held by no other open session and released by
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
	live := make(map[string]bool)       // open seats not archived
	for _, info := range open {
		for _, id := range sessionBeadAssigneeIdentitiesInfo(info) {
			owners[id] = append(owners[id], info.ID)
		}
		if session.State(strings.TrimSpace(info.MetadataState)) != session.StateArchived {
			live[info.ID] = true
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
			switch {
			case b.Status != "open" && b.Status != "in_progress":
				continue
			case seat == "" || assignee == "" || assignee != strings.TrimSpace(b.Metadata[beadmeta.StrandedAssigneeMetadataKey]) || live[seat]:
				// Taken by hand, or the seat is open again: the stamp is stale.
				clearStrandedStamp(store, b.ID, seat, live[seat])
				continue
			case heldElsewhere(assignee, seat):
				continue
			}
			if route, served := releaseRoute(legs.cfg, b, "", laneDirect); !served {
				reason := keptUnservedRoute
				if _, carried := releaseRoute(legs.cfg, b, "", laneCarried); carried {
					reason = failedBudget // the close's own lane serves it: its release ran out of budget
				}
				held[seat] = append(held[seat], strandedClaim{id: b.ID, route: route, reason: reason})
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

// stampStranded marks claim id on store stranded on seat as assigned to
// assignee, while the live row is still assigned so.
func stampStranded(store beads.Store, id, seat, assignee string) error {
	return fencedClaimMeta(store, id, func(b beads.Bead) map[string]string {
		if strings.TrimSpace(b.Assignee) != assignee || b.Status == "closed" {
			return nil
		}
		return map[string]string{beadmeta.StrandedMetadataKey: "true", beadmeta.StrandedSessionMetadataKey: seat, beadmeta.StrandedAssigneeMetadataKey: assignee}
	})
}

// clearStrandedStamp clears claim id's stale stamp, best-effort: one the
// detector finds again next scan, and clears then. It judges staleness again
// on the live row, since the scan read a list a cache may lag: the stamp must
// still name seat, and the claim have left the assignee it stamped, or seat
// be open again (reopened). A fresh stamp a later close wrote survives.
func clearStrandedStamp(store beads.Store, id, seat string, reopened bool) {
	_ = fencedClaimMeta(store, id, func(b beads.Bead) map[string]string {
		stamped := strings.TrimSpace(b.Metadata[beadmeta.StrandedAssigneeMetadataKey])
		switch {
		case strings.TrimSpace(b.Metadata[beadmeta.StrandedMetadataKey]) == "",
			strings.TrimSpace(b.Metadata[beadmeta.StrandedSessionMetadataKey]) != seat,
			!reopened && seat != "" && stamped != "" && strings.TrimSpace(b.Assignee) == stamped:
			return nil
		}
		return map[string]string{beadmeta.StrandedMetadataKey: "", beadmeta.StrandedSessionMetadataKey: "", beadmeta.StrandedAssigneeMetadataKey: ""}
	})
}

// fencedClaimMeta writes decide's metadata on claim id, decided on the live
// row, by one write fenced on that row's revision; a store with no
// conditional writer writes after the read. A nil patch writes nothing.
func fencedClaimMeta(store beads.Store, id string, decide func(beads.Bead) map[string]string) error {
	b, err := liveBeadRead(store, id)
	if err != nil {
		return err
	}
	patch := decide(b)
	if len(patch) == 0 {
		return nil
	}
	writer, _, err := beads.ResolveConditionalWriter(store)
	switch {
	case err != nil:
		return err
	case writer == nil:
		return store.SetMetadataBatch(id, patch)
	}
	return writer.UpdateIfMatch(id, b.Revision, beads.UpdateOpts{Metadata: patch})
}

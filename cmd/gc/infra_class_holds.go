package main

// Holds the copy must carry (ga-1hncv0).
//
// The copy re-adds an infrastructure bead's edges to other infrastructure beads
// and nothing else, so two things that keep a bead from being ready in the work
// store do not reach the binding on their own:
//
//   - a blocking edge onto an unfinished WORK bead. A dependency row can only
//     name an id its own store holds, and the work bead stays in the work
//     store. Copied as a bare edge, SQLite would strand the bead forever and
//     bd would read the edge as external and never block.
//   - bd's indefinite deferral (status=deferred with no defer_until, or one
//     already past that bd's sweep has not lifted). Gas City reads it as
//     IndefinitelyDeferred, which no class binding can record.
//   - any other status bd parks a bead with: blocked (the sanctioned "I cannot
//     proceed" mark), pinned, hooked, or a custom status. A bd-backed read folds
//     each to "open" and keeps the raw status beside it (Bead.SourceStatus);
//     Gas City's demand reads and gc hook honor the raw status, so the bead is
//     parked in the work store, while the binding stores the folded "open".
//
// Either way the bead an operator or agent parked arrives open and ready in the
// binding, and is dispatched. The rehearsals found the first two on every
// unsplit city.
//
// The edge holds are the shared cross-boundary plan's (cross_boundary_plan.go,
// S5 §8.1): a `blocks` edge onto unfinished work crosses as a cross-store gate
// in the binding, exactly as the rig importer and the drain carry the same
// hold, and another blocking type onto unfinished work is refused by id. This
// file holds the status holds: an undated deferral or a parking status is
// refused by id, before anything is written, with what to do about it, and
// preflight names the same refusal so the operator fixes it before taking the
// window. A deferral with a future defer_until crosses as that date, which
// the binding honors.

import (
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// infraUncarriedHolds is the migration's refusal for holds the binding cannot
// express. Each entry names the bead, the hold, and the fix.
type infraUncarriedHolds struct {
	Holds []string
}

func (e *infraUncarriedHolds) Error() string {
	return fmt.Sprintf("%d infrastructure bead(s) are held in the work store by something the binding cannot express, so the copy would make them ready; nothing was copied. Resolve each, then run this again:\n  %s",
		len(e.Holds), strings.Join(e.Holds, "\n  "))
}

// infraCopyParkedRefusal refuses the copy when an open row of its plan is
// held by its status in a way the binding cannot record (an undated
// deferral, a parked status), naming every such row and its fix. The holds
// kept in edges are the cross-boundary plan's (planInfraCityBoundary). It
// reads only.
func infraCopyParkedRefusal(plan infraCopyPlan, now time.Time) error {
	var uncarried []string
	for _, b := range plan.rows {
		if b.Status == "closed" {
			continue
		}
		if why := infraParkedStatusRefusal(b, now); why != "" {
			uncarried = append(uncarried, why)
		}
	}
	if len(uncarried) > 0 {
		return &infraUncarriedHolds{Holds: uncarried}
	}
	return nil
}

// infraParkedStatusRefusal names the hold bd's raw status puts on b when the
// binding cannot carry it, or returns "". The binding stores Gas City's three
// statuses only, so a bead bd parks by status reaches it as the "open" a
// bd-backed read folds the status to, and is ready there. A deferral is carried
// only as a future defer_until; deferred is read from either signal a store
// reports it by (IndefinitelyDeferred, or the raw status alone).
func infraParkedStatusRefusal(b beads.Bead, now time.Time) string {
	if b.IndefinitelyDeferred || b.SourceStatus == "deferred" {
		if b.DeferUntil != nil && b.DeferUntil.After(now) {
			return ""
		}
		return fmt.Sprintf("%s is deferred in the work store with no future defer_until, which the binding cannot hold: give it a dated deferral (bd defer %s --until <date>), which the copy carries, or undefer it (bd undefer %s)", b.ID, b.ID, b.ID)
	}
	switch b.SourceStatus {
	case "", "open", "in_progress", "closed":
		return ""
	}
	return fmt.Sprintf("%s has bd status %q in the work store, which parks it there and which the binding cannot hold (it stores open, in_progress and closed), so it would arrive ready: keep it parked with a dated deferral (bd defer %s --until <date>), which the copy carries, release it (bd update %s --status open), or close it", b.ID, b.SourceStatus, b.ID, b.ID)
}

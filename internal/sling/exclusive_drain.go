package sling

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// ExclusiveDrainReservationError reports that a sling was refused because the
// target bead is held under an exclusive drain reservation
// (gc.exclusive_drain_reservation) by a live drain control.
//
// A drain declaring gc.drain_member_access: exclusive stamps its control ID on
// every member anchor it expands and clears it when the drain reaches a
// terminal state. Until then that member has exactly one dispatch authority —
// the drain — and the drain builds its own unit convoy and item root over it
// (internal/dispatch.ensureDrainUnitConvoy / ensureDrainItemRoot), never
// through DoSling. So a sling that names a reserved member is, by construction,
// a SECOND executor for work a drain is already running: the exact shape of the
// dip-tlbizd incident, where a drain unit and a fresh formula-backed dispatch
// both drove one anchor in one shared build tree, producing duplicate
// implementations and unattributable interleaved commits.
//
// Before this check the reservation was advisory: it was read only by other
// drains (reserveDrainMember's owner comparison), so nothing outside
// internal/dispatch consulted it and the fresh-dispatch entry point sailed past
// it. --force remains the deliberate double-dispatch escape hatch.
type ExclusiveDrainReservationError struct {
	BeadID string
	// ControlID is the drain control holding the reservation.
	ControlID string
	// ControlStatus/ControlTitle describe the holder when it could be read;
	// both are empty when the control bead is unreadable from here (a
	// cross-store holder, or a transient store error). They enrich the
	// diagnostic only — they do not gate the veto.
	ControlStatus string
	ControlTitle  string
}

func (e *ExclusiveDrainReservationError) Error() string {
	holder := e.ControlID
	if e.ControlTitle != "" {
		holder = fmt.Sprintf("%s (%q)", e.ControlID, e.ControlTitle)
	}
	suffix := ""
	if e.ControlStatus == "closed" || e.ControlStatus == "tombstone" {
		// A reservation on a terminated control is abnormal — a normal drain
		// releases before closing — so name it: this is a stuck reservation to
		// clear, not a lane to wait on.
		suffix = fmt.Sprintf(" (holder is %s — a stale reservation; clear it or --force)", e.ControlStatus)
	}
	return fmt.Sprintf(
		"bead %s is reserved for exclusive drain access by drain %s: routing it again would put a second executor on work that drain is already running (%s=%s)%s; wait for the drain, or re-run with --force to accept the double dispatch",
		e.BeadID, holder, beadmeta.ExclusiveDrainReservationMetadataKey, e.ControlID, suffix)
}

// CheckExclusiveDrainReservation reports whether beadID currently carries an
// exclusive drain reservation, returning a typed *ExclusiveDrainReservationError
// naming the holder, or nil.
//
// The presence of gc.exclusive_drain_reservation is itself the veto: a drain
// releases every reservation BEFORE it closes (releaseDrainReservations runs
// ahead of updateMetadataAndClose), so a normally-finished drain leaves an empty
// slot and no veto. A reservation that is still present therefore means either a
// live drain (holder open) or a drain that terminated WITHOUT releasing (holder
// closed/tombstoned) — and in the latter case an item-root worker the drain
// spawned may well still be executing the member. Neither is safe to
// double-dispatch, so the veto holds regardless of holder status; --force is the
// deliberate override, and the message flags a terminated holder as a stale
// reservation to clear rather than a lane to wait on. The holder is read only to
// enrich that message, so an unreadable (e.g. cross-store) control never
// downgrades the veto.
//
// An unreadable TARGET bead degrades to no veto: it carries no reservation we
// can see, and preflight's own existence check (validateExistingBead) owns the
// missing-bead diagnostic.
func CheckExclusiveDrainReservation(q BeadQuerier, store beads.Store, beadID string) error {
	b, ok := BeadFromGetters(beadID, q, store)
	if !ok {
		return nil
	}
	controlID := strings.TrimSpace(b.Metadata[beadmeta.ExclusiveDrainReservationMetadataKey])
	if controlID == "" {
		return nil
	}
	err := &ExclusiveDrainReservationError{BeadID: beadID, ControlID: controlID}
	if control, found := BeadFromGetters(controlID, q, store); found {
		err.ControlStatus = control.Status
		err.ControlTitle = control.Title
	}
	return err
}

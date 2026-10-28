package main

import (
	"errors"
	"log"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// A claim spelled with a session bead ID names exactly one row. When that row
// closes while the claim survives it (a cut-short close cascade, a claim
// written after the close), no successor row ever answers to the ID, so the
// dead-assignee sweep reopens work its seat's live successor may be running
// (gastownhall/gascity#6362). The same claim spelled with the row's seat
// identity — its configured named identity or stable alias, which
// `gc hook --claim` writes when GC_ALIAS is set — is already kept for the
// seat's next row. seatHandover gives the bead-ID spelling the same
// continuity: it moves the claim to the seat's unique live successor through
// the fenced ReassignWorkBead, exactly as duplicate repair does
// (reassignWorkAssignedToRetiredSessionBead). The successor's own next
// `gc hook --claim` adopts the bead and rebinds gc.session_id and
// gc.session_name to itself.
//
// A row with no seat identity (an unaliased pool row, or one whose only alias
// is a rebinding numeric pool slot) has no successor: its siblings never
// inherit its claims (ga-r22k2y), and the sweep releases them as before.

// seatHandover is one sweep's index of the open rows a closed seat row's
// bead-ID claim can be handed to, keyed by seat identity.
type seatHandover struct {
	cfg           *config.City
	sessionStore  beads.Store
	storeRefAware bool
	seats         map[string][]seatSuccessor
	predecessors  map[string]seatPredecessor
	handedOver    int
}

// seatSuccessor is an open row that can take over its seat's claims.
type seatSuccessor struct {
	id        string
	template  string
	storeRefs []string
}

// seatPredecessor is the closed session row a claim's assignee names, with
// the seat identities it held. ok is false when the assignee names no closed
// session row with a seat.
type seatPredecessor struct {
	id       string
	template string
	seats    []string
	ok       bool
}

func newSeatHandover(cityPath string, cfg *config.City, leading beads.Store, sessionStore beads.Store, openSessionInfos []session.Info, storeRefAware bool) *seatHandover {
	h := &seatHandover{
		cfg:           cfg,
		sessionStore:  sessionStore,
		storeRefAware: storeRefAware,
		seats:         make(map[string][]seatSuccessor),
		predecessors:  make(map[string]seatPredecessor),
	}
	var claimRefs []string
	if storeRefAware {
		claimRefs = assignedWorkClaimRefs(cityPath, cfg, leading)
	}
	for _, info := range openSessionInfos {
		if info.Closed || info.State == session.StateArchived || session.LifecycleIdentityReleasedInfo(info) {
			continue
		}
		row := seatSuccessor{id: info.ID, template: normalizedSessionTemplateInfo(info, cfg)}
		if storeRefAware {
			row.storeRefs = openSessionReachableStoreRefInfo(cityPath, cfg, claimRefs, info)
		}
		keys := append([]string{info.ConfiguredNamedIdentity, stableAssignmentAliasForConfigInfo(info, cfg)}, configuredNamedFallbackIdentitiesInfo(info, cfg)...)
		for _, seat := range compactSessionAssignmentIdentifiers(keys) {
			h.seats[seat] = append(h.seats[seat], row)
		}
	}
	return h
}

// handOver moves wb's claim from the closed session row its assignee names to
// that row's seat successor, and reports whether the sweep must keep wb: true
// once the claim was handed over or the hand-over write was fenced off or
// refused; false when the assignee names no closed seat row or the seat has
// no unique successor, so the release proceeds.
//
// The write is ReassignWorkBead's single assignee-only write, fenced on the
// snapshot's status and assignee, so a claim that lands after the snapshot is
// never overwritten (ReassignWorkBead logs that skip itself), and status and
// metadata are left for the successor's hook to rebind.
func (h *seatHandover) handOver(ownerStore beads.Store, wb beads.Bead, assignee, workStoreRef string) bool {
	pred := h.predecessor(assignee)
	if !pred.ok {
		return false
	}
	successor, seat, ok := h.successor(pred, workStoreRef)
	if !ok {
		return false
	}
	// The predecessor must stop naming the bead before it moves, as in the
	// duplicate-repair hand-over: `gc hook current` reads this stamp.
	if err := clearSessionCurrentClaim(h.sessionStore, pred.id); err != nil {
		log.Printf("releaseOrphanedPoolAssignments: clearing current claim on closed session %s: %v", pred.id, err)
	}
	if err := workAssignmentForStore(beads.WorkStore{Store: ownerStore}).ReassignWorkBead(wb, successor.id); err != nil {
		log.Printf("releaseOrphanedPoolAssignments: kept %s of closed session %s: handing it to seat %q successor %s: %v", wb.ID, pred.id, seat, successor.id, err)
		return true
	}
	h.handedOver++
	log.Printf("releaseOrphanedPoolAssignments: handed %s from closed session %s to seat %q successor %s", wb.ID, pred.id, seat, successor.id)
	return true
}

// predecessor resolves assignee to a closed session row with a seat identity,
// memoized per assignee for the sweep.
func (h *seatHandover) predecessor(assignee string) seatPredecessor {
	if pred, ok := h.predecessors[assignee]; ok {
		return pred
	}
	pred := h.lookupPredecessor(assignee)
	h.predecessors[assignee] = pred
	return pred
}

func (h *seatHandover) lookupPredecessor(assignee string) seatPredecessor {
	if h.sessionStore == nil {
		return seatPredecessor{}
	}
	sb, err := h.sessionStore.Get(assignee)
	if err != nil {
		if !errors.Is(err, beads.ErrNotFound) {
			log.Printf("releaseOrphanedPoolAssignments: reading session %q for a seat hand-over: %v", assignee, err)
		}
		return seatPredecessor{}
	}
	if sb.ID != assignee || !isSessionBead(sb) || sb.Status != "closed" {
		return seatPredecessor{}
	}
	seats := compactSessionAssignmentIdentifiers(append(
		[]string{predecessorNamedIdentity(sb), stableAssignmentAliasForConfig(sb, h.cfg)},
		configuredNamedFallbackIdentities(sb, h.cfg)...,
	))
	if len(seats) == 0 {
		return seatPredecessor{}
	}
	return seatPredecessor{id: sb.ID, template: normalizedSessionTemplate(sb, h.cfg), seats: seats, ok: true}
}

// predecessorNamedIdentity is the configured named identity a closed row
// held: configured_named_identity, or retired_named_identity when retirement
// (session.RetireNamedSessionPatch) recorded it there.
func predecessorNamedIdentity(sb beads.Bead) string {
	if identity := strings.TrimSpace(sb.Metadata[namedSessionIdentityMetadata]); identity != "" {
		return identity
	}
	return strings.TrimSpace(sb.Metadata["retired_named_identity"])
}

// successor returns the one open row of pred's template that answers to one
// of pred's seat identities and reaches the work's store, with the seat it
// answers to. Zero or several such rows is no successor: there is no
// tie-break.
func (h *seatHandover) successor(pred seatPredecessor, workStoreRef string) (seatSuccessor, string, bool) {
	var (
		found     seatSuccessor
		foundSeat string
		count     int
	)
	seen := make(map[string]struct{})
	for _, seat := range pred.seats {
		for _, row := range h.seats[seat] {
			if _, ok := seen[row.id]; ok {
				continue
			}
			seen[row.id] = struct{}{}
			if row.template != pred.template || !h.reachesWork(row, workStoreRef) {
				continue
			}
			found, foundSeat = row, seat
			count++
		}
	}
	if count != 1 {
		return seatSuccessor{}, "", false
	}
	return found, foundSeat, true
}

// reachesWork applies openSessionOwnsWork's store rule to a successor row.
func (h *seatHandover) reachesWork(row seatSuccessor, workStoreRef string) bool {
	if !h.storeRefAware {
		return true
	}
	for _, ref := range row.storeRefs {
		if ref == unresolvedOpenSessionStoreRef || ref == crossStoreOpenSessionStoreRef || ref == workStoreRef {
			return true
		}
	}
	return false
}

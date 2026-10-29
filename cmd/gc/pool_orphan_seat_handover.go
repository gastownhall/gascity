package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

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
// continuity: the release verb offers it each such claim the orphan release
// would otherwise reopen (ReleaseOpts.HandOver), only once the verb's own
// guards have passed, and it moves the claim to the seat's unique live
// successor in ReassignWorkBead's one assignee-only write, fenced on the
// snapshot, as duplicate repair does
// (reassignWorkAssignedToRetiredSessionBead). The successor's own next
// `gc hook --claim` adopts the bead and rebinds gc.session_id and
// gc.session_name to itself. A claim the verb keeps (no lane serves its
// route, its detached work reads alive) stays on the closed row, where the
// stranded detector reports it.
//
// A row with no seat identity (an unaliased pool row, or one whose only alias
// is a rebinding numeric pool slot) has no successor: its siblings never
// inherit its claims (ga-r22k2y), and the sweep releases them as before.

// seatRowReadErrorThreshold is how many consecutive sweeps keep the claims of
// a closed row that cannot be read, as detachedProbeErrorThreshold bounds the
// detached probe: the read is the hand-over's, and a row that stays
// unreadable must not pin claims the sweep released before it.
const seatRowReadErrorThreshold = 3

// seatRowReadErrors counts consecutive sweeps that could not read the session
// row an assignee names, by assignee.
var seatRowReadErrors = struct {
	sync.Mutex
	byAssignee map[string]int
}{
	byAssignee: make(map[string]int),
}

// seatHandover is one sweep's hand-over of closed seat rows' bead-ID claims
// to the open rows holding the same seats.
type seatHandover struct {
	cfg           *config.City
	cityPath      string
	leading       beads.Store
	sessionStore  beads.Store
	openSessions  []session.Info
	storeRefAware bool
	seats         map[string][]seatSuccessor // by seat identity, built on first use
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
// session row with a seat. unreadable, when set, is why the sweep keeps the
// claim this pass: the assignee's row could not be read, and uncertainty is
// not permission to reopen (gc-ft31x).
type seatPredecessor struct {
	id         string
	template   string
	seats      []string
	ok         bool
	unreadable string
}

func newSeatHandover(cityPath string, cfg *config.City, leading beads.Store, sessionStore beads.Store, openSessionInfos []session.Info, storeRefAware bool) *seatHandover {
	return &seatHandover{
		cfg:           cfg,
		cityPath:      cityPath,
		leading:       leading,
		sessionStore:  sessionStore,
		openSessions:  openSessionInfos,
		storeRefAware: storeRefAware,
		predecessors:  make(map[string]seatPredecessor),
	}
}

// offer is the ReleaseOpts.HandOver for a claim read from the store
// workStoreRef names.
func (h *seatHandover) offer(workStoreRef string) func(seatWorkHit) string {
	return func(c seatWorkHit) string { return h.handOver(c, workStoreRef) }
}

// handOver moves claim c from the closed session row its assignee names to
// that row's seat successor, and returns why the release keeps c: it was
// handed over, it moved since the snapshot, the closed row's current claim
// could not be cleared, the hand-over write failed, or the assignee's row
// could not be read. It returns "" to let the release proceed: the assignee
// names no closed seat row, the seat has no unique successor, the row stayed
// unreadable for seatRowReadErrorThreshold sweeps, or the store cannot fence
// the write (the release has fences of its own).
//
// The write is ReassignWorkBead's: Assignee only, fenced on the snapshot's
// status and assignee, so a claim that lands after the snapshot is never
// overwritten, and status and metadata are left for the successor's hook to
// rebind. It is made here so that only a write that landed is counted.
func (h *seatHandover) handOver(c seatWorkHit, workStoreRef string) string {
	pred := h.predecessor(strings.TrimSpace(c.bead.Assignee))
	if pred.unreadable != "" {
		return pred.unreadable
	}
	if !pred.ok {
		return ""
	}
	successor, seat, ok := h.successor(pred, workStoreRef)
	if !ok {
		return ""
	}
	// The predecessor must stop naming the bead before it moves, as in the
	// duplicate-repair hand-over: `gc hook current` reads this stamp. A clear
	// that fails keeps the claim here, so the next sweep retries the clear
	// and the move together: once the bead moved, the successor's ID would
	// keep every later sweep off it, and nothing would clear the stamp.
	if err := clearSessionCurrentClaim(h.sessionStore, pred.id); err != nil {
		return fmt.Sprintf("clearing current claim on closed session %s: %v", pred.id, err)
	}
	outcome, err := releaseAssignmentFenced(c.store, c.bead, beads.UpdateOpts{Assignee: &successor.id})
	switch {
	case err != nil:
		return fmt.Sprintf("handing it to seat %q successor %s: %v", seat, successor.id, err)
	case outcome == fencedReleaseApplied:
		h.handedOver++
		return fmt.Sprintf("handed to seat %q successor %s", seat, successor.id)
	case outcome == fencedReleaseRefused:
		return ""
	default:
		return keptMoved
	}
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
	if h.sessionStore == nil || assignee == "" {
		return seatPredecessor{}
	}
	sb, err := h.sessionStore.Get(assignee)
	// A relocated class's refusal proves the store holds no row under the
	// ID, as not-found does.
	if err != nil && !errors.Is(err, beads.ErrNotFound) && !errors.Is(err, beads.ErrBdSQLClassRelocated) {
		return unreadableSeatPredecessor(assignee, err)
	}
	clearSeatRowReadErrors(assignee)
	// The row must be one the session front door accepts: handOver clears
	// its current claim through it, and a row it refuses would keep its
	// claims every sweep.
	if err != nil || sb.ID != assignee || !session.IsSessionBeadOrRepairable(sb) || sb.Status != "closed" {
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

// unreadableSeatPredecessor keeps the claims of an assignee whose row could
// not be read, until seatRowReadErrorThreshold consecutive sweeps have
// failed to read it; then the release proceeds as for any dead assignee.
func unreadableSeatPredecessor(assignee string, err error) seatPredecessor {
	count := incrementSeatRowReadErrors(assignee)
	if count < seatRowReadErrorThreshold {
		return seatPredecessor{unreadable: fmt.Sprintf("its session row could not be read for a seat hand-over (error %d/%d): %v", count, seatRowReadErrorThreshold, err)}
	}
	clearSeatRowReadErrors(assignee)
	log.Printf("releaseOrphanedPoolAssignments: releasing work assigned to %q: its session row was unreadable for %d sweeps: %v", assignee, count, err)
	return seatPredecessor{}
}

func incrementSeatRowReadErrors(assignee string) int {
	seatRowReadErrors.Lock()
	defer seatRowReadErrors.Unlock()
	seatRowReadErrors.byAssignee[assignee]++
	return seatRowReadErrors.byAssignee[assignee]
}

func clearSeatRowReadErrors(assignee string) {
	seatRowReadErrors.Lock()
	defer seatRowReadErrors.Unlock()
	delete(seatRowReadErrors.byAssignee, assignee)
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
	index := h.seatIndex()
	seen := make(map[string]struct{})
	for _, seat := range pred.seats {
		for _, row := range index[seat] {
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

// seatIndex indexes the open rows that can take over a seat by seat
// identity. It is built on the sweep's first hand-over, not with the sweep:
// most sweeps hand nothing over, and the index reads the city's residency.
// A draining row is no successor: its hook's drain fence refuses to adopt
// work (doHookClaim), so a claim handed to it would sit out the whole drain,
// where the release reopens it at once.
func (h *seatHandover) seatIndex() map[string][]seatSuccessor {
	if h.seats != nil {
		return h.seats
	}
	h.seats = make(map[string][]seatSuccessor)
	var claimRefs []string
	if h.storeRefAware {
		claimRefs = assignedWorkClaimRefs(h.cityPath, h.cfg, h.leading)
	}
	for _, info := range h.openSessions {
		if info.Closed || info.State == session.StateArchived || info.State == session.StateDraining || session.LifecycleIdentityReleasedInfo(info) {
			continue
		}
		row := seatSuccessor{id: info.ID, template: normalizedSessionTemplateInfo(info, h.cfg)}
		if h.storeRefAware {
			row.storeRefs = openSessionReachableStoreRefInfo(h.cityPath, h.cfg, claimRefs, info)
		}
		keys := append([]string{info.ConfiguredNamedIdentity, stableAssignmentAliasForConfigInfo(info, h.cfg)}, configuredNamedFallbackIdentitiesInfo(info, h.cfg)...)
		for _, seat := range compactSessionAssignmentIdentifiers(keys) {
			h.seats[seat] = append(h.seats[seat], row)
		}
	}
	return h.seats
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

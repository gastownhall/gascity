package main

import (
	"errors"
	"log"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// retiredNamedIdentityMetadata is the marker RetireNamedSessionPatch stamps in
// place of configured_named_identity when a named session is retired, so the
// seat identity a closed row carried stays readable after the retire clears
// the live identity keys.
const retiredNamedIdentityMetadata = "retired_named_identity"

// seatHandover moves a closed seat row's surviving bead-ID claims to that
// seat's unique open successor during the dead-assignee release sweep
// (#6362, Gap 1).
//
// A bead-ID assignee names exactly one row, so when that row closes, no
// successor row ever answers to the claim and the sweep reopens work that the
// seat's live replacement is still running — the replacement keeps a stale
// gc.session_id and the reopened bead deadlocks the seat. The named-identity
// spelling of the same claim already survives via the named-route gate; this
// gives the bead-ID spelling the same seat continuity by moving it, exactly as
// duplicate repair already does (reassignWorkAssignedToRetiredSessionBead).
//
// It resolves the successor through the seat, never the row: the predecessor
// row's configured identity — or the retired marker the retire patch leaves —
// plus its stable alias, matched against open rows of the same template whose
// stores reach the work. A predecessor with no seat identity (an unaliased
// pool row) has no successor to find, and its claims release exactly as
// before: a live sibling must never mask a dead unaliased assignee
// (ga-r22k2y). Zero or multiple successors are equally no hand-over — there
// is no tie-break.
type seatHandover struct {
	cfg      *config.City
	cityPath string
	// sessionStore is the session-class store the predecessor rows live in.
	sessionStore beads.SessionStore
	// entries indexes the sweep's open snapshot by seat identity. Raw
	// identifiers — bead IDs, session names, instance tokens — are never keys:
	// they name one row, and one row closing is the failure mode.
	entries map[string][]seatHandoverEntry
	// predecessors memoizes the closed-row read per assignee per pass.
	predecessors map[string]seatPredecessor
	// readErrs records the first session-store read error per identity, so
	// log reports each once per pass.
	readErrs   map[string]error
	handedOver int
}

// seatHandoverEntry is one open row indexed under a seat identity.
type seatHandoverEntry struct {
	id       string
	template string
	// storeRefs are the row's reachable store refs
	// (openSessionReachableStoreRefInfo), matched against the work bead's own
	// ref with the same rule openSessionOwnsWork applies.
	storeRefs []string
}

// seatPredecessor is the memoized read of a work bead's bead-ID assignee.
type seatPredecessor struct {
	bead   beads.Bead
	status seatPredecessorStatus
	err    error
}

type seatPredecessorStatus int

const (
	// predecessorMissing: no row answers to the assignee. No hand-over; the
	// release decision proceeds exactly as before.
	predecessorMissing seatPredecessorStatus = iota
	// predecessorNotClosed: the row exists but is not a closed session bead
	// (open rows are kept by the liveness gates before this point; anything
	// else is not a hand-over shape). Release proceeds.
	predecessorNotClosed
	// predecessorClosed: a closed session bead, the hand-over's subject.
	predecessorClosed
	// predecessorUnknown: the read failed. The claim is kept this pass;
	// uncertainty is not permission to reopen (gc-ft31x).
	predecessorUnknown
)

// newSeatHandover indexes the sweep's open-session snapshot by seat identity.
// leading is the work store the pass runs over, used only to resolve the
// city's claim refs once; openSessionInfos is the same typed snapshot the
// sweep's keep gates already read.
func newSeatHandover(cityPath string, cfg *config.City, sessionStore beads.SessionStore, leading beads.Store, openSessionInfos []session.Info) *seatHandover {
	h := &seatHandover{
		cfg:          cfg,
		cityPath:     cityPath,
		sessionStore: sessionStore,
		entries:      make(map[string][]seatHandoverEntry),
		predecessors: make(map[string]seatPredecessor),
		readErrs:     make(map[string]error),
	}
	if cfg == nil || sessionStore.Store == nil || len(openSessionInfos) == 0 {
		return h
	}
	claimRefs := assignedWorkClaimRefs(cityPath, cfg, leading)
	for _, info := range openSessionInfos {
		if info.Closed || info.State == session.StateArchived || session.LifecycleIdentityReleasedInfo(info) {
			continue
		}
		id := strings.TrimSpace(info.ID)
		if id == "" {
			continue
		}
		entry := seatHandoverEntry{
			id:        id,
			template:  normalizedSessionTemplateInfo(info, cfg),
			storeRefs: openSessionReachableStoreRefInfo(cityPath, cfg, claimRefs, info),
		}
		for _, key := range seatIdentityKeysInfo(info, cfg) {
			h.entries[key] = append(h.entries[key], entry)
		}
	}
	return h
}

// seatIdentityKeysInfo returns the seat identities an open row answers to: its
// configured_named_identity, plus the stable alias and configured-named
// fallback sessionAssignmentIdentifiersForConfigInfo adds. The raw identifiers
// that function also reports — the row's bead ID and session name — are
// deliberately excluded: they name the row, not the seat, and indexing them
// would hand a closed row's claim to whichever row recycled its name.
func seatIdentityKeysInfo(info session.Info, cfg *config.City) []string {
	raw := rawAssignmentIdentifiersInfo(info)
	keys := []string{strings.TrimSpace(info.ConfiguredNamedIdentity)}
	for _, id := range sessionAssignmentIdentifiersForConfigInfo(info, cfg) {
		if _, isRaw := raw[id]; isRaw {
			continue
		}
		keys = append(keys, id)
	}
	return compactSessionAssignmentIdentifiers(keys)
}

// predecessorSeatIdentities returns the seat identities a closed row carried:
// its configured_named_identity — or, once the retire patch clears that, the
// retired_named_identity marker it stamps in the key's place — plus the stable
// alias and configured-named fallback sessionAssignmentIdentifiersForConfig
// reads off the bead. The raw bead ID and session name are excluded for the
// same reason as in seatIdentityKeysInfo. None of these means no seat, and the
// claims release exactly as before.
func predecessorSeatIdentities(p beads.Bead, cfg *config.City) []string {
	raw := rawAssignmentIdentifiers(p)
	identity := strings.TrimSpace(p.Metadata[namedSessionIdentityMetadata])
	if identity == "" {
		identity = strings.TrimSpace(p.Metadata[retiredNamedIdentityMetadata])
	}
	keys := []string{identity}
	for _, id := range sessionAssignmentIdentifiersForConfig(p, cfg) {
		if _, isRaw := raw[id]; isRaw {
			continue
		}
		keys = append(keys, id)
	}
	return compactSessionAssignmentIdentifiers(keys)
}

func rawAssignmentIdentifiers(b beads.Bead) map[string]struct{} {
	raw := make(map[string]struct{}, 3)
	for _, id := range sessionAssignmentIdentifierRaw(b) {
		if id != "" {
			raw[id] = struct{}{}
		}
	}
	return raw
}

func rawAssignmentIdentifiersInfo(info session.Info) map[string]struct{} {
	raw := make(map[string]struct{}, 3)
	for _, id := range sessionAssignmentIdentifierRawInfo(info) {
		if id != "" {
			raw[id] = struct{}{}
		}
	}
	return raw
}

// handOver reports whether the sweep must keep wb this pass: because its
// bead-ID assignee is a closed seat row whose claim moved to that seat's
// unique successor, or because the predecessor read could not be made. false
// means no hand-over applies and the release decision proceeds unchanged.
//
// ownerStore is the work bead's own owner store, the only store the reassign
// may write; workStoreRef is the bead's store ref when the caller is
// store-ref aware ("" otherwise, matching an agent with no configured rig).
func (h *seatHandover) handOver(ownerStore beads.Store, wb beads.Bead, assignee, workStoreRef string) bool {
	if h == nil || h.cfg == nil || h.sessionStore.Store == nil || ownerStore == nil {
		return false
	}
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return false
	}
	p := h.predecessor(assignee)
	switch p.status {
	case predecessorMissing, predecessorNotClosed:
		return false
	case predecessorUnknown:
		return true
	}
	seatKeys := predecessorSeatIdentities(p.bead, h.cfg)
	if len(seatKeys) == 0 {
		return false
	}
	successor, seat := h.uniqueSuccessor(seatKeys, p.bead, workStoreRef)
	if successor == "" {
		return false
	}
	// The closed predecessor must stop naming the bead before the successor
	// takes it: `gc hook current` closes whatever the row's current-claim
	// stamp names, and a stale stamp would close the successor's bead out from
	// under it. Best-effort, like every other release path that clears it —
	// the successor's next claim overwrites the back-channel anyway.
	if err := clearSessionCurrentClaim(h.sessionStore.Store, p.bead.ID); err != nil {
		log.Printf("releaseOrphanedPoolAssignments: clearing current claim on closed session %s before seat hand-over of %q: %v", p.bead.ID, wb.ID, err)
	}
	if err := workAssignmentForStore(beads.WorkStore{Store: ownerStore}).ReassignWorkBead(wb, successor); err != nil {
		// A lost fence or a store that cannot write conditionally keeps the
		// bead with the closed identity this pass (ReassignWorkBead never
		// writes blind); the next sweep decides again.
		log.Printf("releaseOrphanedPoolAssignments: seat hand-over of %q from closed session %s to %s (seat %q) deferred: %v", wb.ID, p.bead.ID, successor, seat, err)
		return true
	}
	h.handedOver++
	log.Printf("releaseOrphanedPoolAssignments: handed over %q from closed session %s to its seat successor %s (seat %q)", wb.ID, p.bead.ID, successor, seat)
	return true
}

// predecessor reads and classifies the assignee's own row, memoized per
// assignee per pass.
func (h *seatHandover) predecessor(assignee string) seatPredecessor {
	if p, ok := h.predecessors[assignee]; ok {
		return p
	}
	bead, err := h.sessionStore.Get(assignee)
	var p seatPredecessor
	switch {
	case err == nil:
		if isSessionBead(bead) && strings.EqualFold(strings.TrimSpace(bead.Status), "closed") {
			p = seatPredecessor{bead: bead, status: predecessorClosed}
		} else {
			p = seatPredecessor{bead: bead, status: predecessorNotClosed}
		}
	case errors.Is(err, beads.ErrNotFound):
		p = seatPredecessor{status: predecessorMissing}
	default:
		p = seatPredecessor{err: err, status: predecessorUnknown}
		if _, seen := h.readErrs[assignee]; !seen {
			h.readErrs[assignee] = err
		}
	}
	h.predecessors[assignee] = p
	return p
}

// uniqueSuccessor resolves the one open row that continues p's seat for work
// in workStoreRef, and the seat identity it answered to. The row must carry
// p's normalized template and reach the work's store — the same rule
// openSessionOwnsWork applies — and there must be exactly one; zero or
// several release the claim exactly as before, with no tie-break.
func (h *seatHandover) uniqueSuccessor(seatKeys []string, p beads.Bead, workStoreRef string) (successorID, seat string) {
	wantTemplate := normalizedSessionTemplate(p, h.cfg)
	seen := make(map[string]string, len(seatKeys))
	for _, key := range seatKeys {
		for _, entry := range h.entries[key] {
			if entry.template != wantTemplate || !seatReachesWorkStore(entry.storeRefs, workStoreRef) {
				continue
			}
			if _, dup := seen[entry.id]; dup {
				continue
			}
			seen[entry.id] = key
		}
	}
	if len(seen) != 1 {
		return "", ""
	}
	for id, key := range seen {
		return id, key
	}
	return "", ""
}

// seatReachesWorkStore reports whether a successor row's store refs reach the
// work bead's own store: the row's own ref, the unresolved marker a row with
// no resolvable agent carries, or the cross-store marker a city-scoped agent
// federates under. On a caller with no store-ref awareness every ref is ""
// and the match holds, which is the single-store answer.
func seatReachesWorkStore(refs []string, workStoreRef string) bool {
	for _, ref := range refs {
		if ref == workStoreRef || ref == unresolvedOpenSessionStoreRef || ref == crossStoreOpenSessionStoreRef {
			return true
		}
	}
	return false
}

// log reports the pass's predecessor read failures once each. Any read error
// other than not-found kept that identity's claims assigned this pass; the
// next sweep decides again against a fresh read.
func (h *seatHandover) log() {
	for assignee, err := range h.readErrs {
		log.Printf("releaseOrphanedPoolAssignments: reading closed session %q for a seat hand-over: %v (claims naming it stay assigned this pass)", assignee, err)
	}
}

package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// Session ownership: "does this live session own work, so that a demand-class
// decider must not stop it?"
//
// This file is the home of the shared never-stop-an-owner predicate. Today it
// holds one ownership signal — the claim the session stamped on its own bead
// through `gc hook --claim` (beadmeta.CurrentClaimBeadIDMetadataKey) — and is
// wired into the demand-class drain deciders that killed a fresh pool worker
// seconds after it claimed its first step:
//
//   - the not-desired "orphaned" drain (reconcileSessionBeads, !desired &&
//     providerAlive arm), including canceling an in-flight orphaned drain once
//     the claim becomes visible;
//   - the no-wake-reason / idle drain (reconcileSessionBeads, !shouldWake &&
//     alive arm).
//
// Intent-class stops (user suspend or kill, agent removed from config, city
// stop, config drift, max-age, the agent's own drain-ack, execution-stalled)
// are deliberately NOT gated: liveClaimVetoApplies refuses them.
//
// The ownership signal is read LIVE. The claim is written out of process by the
// worker's hook, so the controller's cache (and the demand snapshot built from
// it) can miss it for up to a cache-reconcile interval; that stale negative is
// exactly what drained the worker. The v1.6 epic (ga-uvzj6) grows this into a
// single ownership resolver (claim stamp + assignee identities + gc.session_id)
// enforced by one gate inside every destructive primitive, pinned by the
// decider table test in session_ownership_test.go. Until then, a new
// demand-class decider that can stop a live session should consult
// sessionOwnsLiveClaim and add a row to that table.

// liveClaimVetoApplies reports whether a demand-class drain of info may be
// vetoed by a live claim at all. Only pool-managed (ephemeral) sessions whose
// agent is still configured and not suspended qualify: removing the agent from
// config, or suspending it (agent, rig, or city), is operator intent and must
// still stop the session. Named sessions keep their own suspend-class handling.
func liveClaimVetoApplies(cityPath string, cfg *config.City, info sessionpkg.Info, suspState suspensionstate.State) bool {
	if cfg == nil || !isPoolManagedSessionInfo(info) || isNamedSessionInfo(info) {
		return false
	}
	agentCfg := sessionAgentConfigInfo(cfg, info)
	if agentCfg == nil {
		return false
	}
	return !isAgentEffectivelySuspendedWith(cfg, cityPath, agentCfg, suspState)
}

// sessionOwnsLiveClaim reports whether the session still owns the work bead it
// last claimed through `gc hook --claim`.
//
// It reads the session bead's current_claim_bead_id LIVE (not the tick
// snapshot, not the cache), then reads that work bead LIVE through the same
// residency legs the assigned-work guards use. The claim is held when the
// work bead exists and is not closed, unless the bead itself says the claim is
// gone:
//
//   - it was released back to the queue (status open, no assignee), or
//   - another session claimed it since (gc.session_id names a different
//     session) — the stale-stamp fence for a release path that could not clear
//     this session's stamp.
//
// Any read error reports the claim as held (owns=true with the error): a
// transient store failure must never be the reason a working session dies.
// claimID is the stamped work bead id ("" when nothing is stamped).
func sessionOwnsLiveClaim(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	sw *SeatWork,
	info sessionpkg.Info,
) (owns bool, claimID string, err error) {
	if store == nil || strings.TrimSpace(info.ID) == "" {
		return false, "", nil
	}
	sessionBead, err := liveBeadRead(store, info.ID)
	if err != nil {
		return true, "", fmt.Errorf("reading session bead %s: %w", info.ID, err)
	}
	claimID = strings.TrimSpace(sessionBead.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey])
	if claimID == "" {
		return false, "", nil
	}
	// The claimed bead is found by id, through the storeref ByID plan: a binding
	// answers alone for an id in its own namespace, so a graph-resident claim
	// (the common one on a split city) never costs a read of the remote work
	// store, even once it is closed and the stamp lingers. The first leg that
	// holds the bead answers for it — ids are unique, so its own state is final
	// — and only NotFound moves on.
	legs := sw.Legs()
	if err := legs.unusable(); err != nil {
		return true, claimID, fmt.Errorf("reading claimed bead %s: %w", claimID, err)
	}
	owner, err := byIDOwnerRowForTopology(byIDResidencyTopology(cityPath, cfg, legs.workStore(), legs.rigs, claimID), claimID)
	if errors.Is(err, beads.ErrNotFound) {
		return false, claimID, nil
	}
	if err != nil {
		return true, claimID, fmt.Errorf("reading claimed bead %s: %w", claimID, err)
	}
	// The resolver's read may be a cache's; the owner is re-read live.
	work, err := liveBeadRead(owner.Store, claimID)
	if errors.Is(err, beads.ErrNotFound) {
		return false, claimID, nil
	}
	if err != nil {
		return true, claimID, fmt.Errorf("reading claimed bead %s: %w", claimID, err)
	}
	held := claimedWorkStillHeldBy(work, info.ID)
	return held, claimID, nil
}

// claimedWorkStillHeldBy is the per-bead half of sessionOwnsLiveClaim.
func claimedWorkStillHeldBy(work beads.Bead, sessionID string) bool {
	if work.Status == "closed" {
		return false
	}
	if work.Status == "open" && strings.TrimSpace(work.Assignee) == "" {
		return false
	}
	if owner := strings.TrimSpace(work.Metadata[beadmeta.SessionIDMetadataKey]); owner != "" && owner != sessionID {
		return false
	}
	return true
}

// sessionHoldsAssignedWork reports the first held-work bead (heldWorkRow)
// assigned to info's ReleaseScope on sw's legs: work the seat holds but has no
// wake reason to run yet. SeatWork lists open work by its raw status, so
// dependency-blocked work is included and status-parked work (raw blocked or
// deferred) is excluded, as on the sibling seat-work gates. It fails closed: a
// leg that could not be read, with no confirmed hit elsewhere, reports held
// with the read error.
func sessionHoldsAssignedWork(sw *SeatWork, info sessionpkg.Info) (held bool, beadID string, err error) {
	b, ok, err := sw.first(heldWorkQuery(releaseScope(info, sw.Legs().cfg)))
	if !ok && err != nil {
		return true, "", err
	}
	return ok, b.ID, nil
}

// heldWorkQuery is the held-work keep's question about scope's work: its open
// or in-progress beads that heldWorkRow counts.
func heldWorkQuery(scope workScope) seatWorkQuery {
	return seatWorkQuery{scope: scope, statuses: seatWorkStatuses, row: heldWorkRow}
}

// heldWorkRow reports whether b, an open or in-progress bead assigned to a
// seat, is work the held-work keep counts, on both reconcilers: not a session
// or mail bead (as seatWorkQuery.accepts), and not open work parked on a
// dispatch hold, which the seat's hook never serves (assignedOpenWorkHeld).
// Held in-progress work still counts (ga-5736js).
func heldWorkRow(b beads.Bead) bool {
	return !sessionpkg.IsSessionBeadOrRepairable(b) && !beadmail.IsMessageBead(b) && !assignedOpenWorkHeld(b)
}

// heldWorkDrainReason reports whether reason is a timer drain that a seat
// holding assigned work is kept out of.
func heldWorkDrainReason(reason string) bool {
	return reason == "idle" || reason == "no-wake-reason"
}

// heldWorkIntent reports whether info's sleep intent leaves its seat to the
// held-work keep: none, or the idle probe's idle-stop-pending. An operator's
// suspend, an agent's wait and an intent no cell writes stop the seat on
// their own terms. Every keep site reads it, on both reconcilers.
func heldWorkIntent(info sessionpkg.Info) bool {
	d := sessionpkg.DispositionOfInfo(info)
	return d.Operator != sessionpkg.OperatorSuspended && !d.WaitIntent && d.Unknown&sessionpkg.UnknownIntent == 0
}

// heldWorkInBound reports whether the held-work keep may still hold info's
// seat at now: the seat woke (last_woke_at) less than parkedWorkMaxHold, the
// park's bound, before now. A held seat keeps its runtime, cap headroom and
// pool slot, which a parked seat gives back, so past the bound its timer
// drains run as they would without the keep, and the seat they sleep parks or
// strands as any drained seat does (parkSeats): a seat whose agent stopped
// answering gives them back within a day of its wake. A seat holds work live
// for no longer than it has been awake, so no hold outlasts the bound. A wake
// stamp that is missing, unreadable or more than parkedSleptAtFutureSkew past
// now proves nothing, and the seat drains as today. The bound runs from the
// wake, not from the pane's last activity, which an idle Claude Code TUI keeps
// fresh by repainting its status line (ga-07mi8). Every keep site reads it, on
// both reconcilers; the idle stop does not (idleStopSleepPatch).
func heldWorkInBound(info sessionpkg.Info, now time.Time) bool {
	until, ok := heldWorkHoldUntil(info, now)
	return ok && now.Before(until)
}

// heldWorkHoldUntil is when the held-work keep's bound on info's seat ends:
// its last_woke_at plus parkedWorkMaxHold. ok is false for a wake stamp that
// proves nothing (heldWorkInBound).
func heldWorkHoldUntil(info sessionpkg.Info, now time.Time) (until time.Time, ok bool) {
	woke, err := time.Parse(time.RFC3339, strings.TrimSpace(info.LastWokeAt))
	if err != nil || woke.Sub(now) >= parkedSleptAtFutureSkew {
		return time.Time{}, false
	}
	return woke.Add(parkedWorkMaxHold), true
}

// idleStopSleepPatch is the sleep legacy's idle-timeout stop (dec) writes on
// info's seat, and whether the seat holds work the held-work keep counts. The
// ordinary idle stop of a seat the keep covers (heldWorkIntent,
// liveClaimVetoApplies) that holds work (sessionHoldsAssignedWork, failing
// closed) finishes as its timer drain would have (finalizePatch with work:
// CompleteDrainPatch(idle)), which the park (parkableRow) accepts: inside the
// keep's bound the stop ends the hold that held that drain back, and past it
// (heldWorkInBound) the drain may still be running. Any other stop, the defer
// streak's forced stop included, takes the ladder's SleepPatch. v2's is
// idleTimeoutHeldWorkFinish.
func idleStopSleepPatch(cityPath string, cfg *config.City, info sessionpkg.Info, suspState suspensionstate.State, sw *SeatWork, dec sessionpkg.TimerDecision, now time.Time, stderr io.Writer) (sessionpkg.MetadataPatch, bool) {
	if dec.SleepReason == string(sessionpkg.SleepReasonIdleTimeout) && heldWorkIntent(info) && liveClaimVetoApplies(cityPath, cfg, info, suspState) {
		holds, _, err := sessionHoldsAssignedWork(sw, info)
		if err != nil {
			fmt.Fprintf(stderr, "session reconciler: idle timeout for %s: reading its held work: %v\n", info.SessionNameMetadata, err) //nolint:errcheck // best-effort stderr
		}
		if holds {
			return sessionpkg.CompleteDrainPatch(now, string(sessionpkg.SleepReasonIdle), info.WakeMode == "fresh"), true
		}
	}
	return sessionpkg.SleepPatch(now, dec.SleepReason), false
}

// liveBeadRead reads one bead through the store's authoritative live handle,
// bypassing a caching layer (liveReads): without the peel the read would
// silently fall back to the cache, as CachingStore.Get serves the cache first.
func liveBeadRead(store beads.Store, id string) (beads.Bead, error) {
	return liveReads(store).Get(id)
}

// liveRead is a read-only live handle on a store under its wrappers
// (liveReads). It offers reads only, so a wrapper that refuses writes
// (a v2 effect's read-only store) never hands its writable inner store on.
type liveRead struct{ store beads.Store }

// Get reads id through the peeled store's live reader (beads.LiveFor), past
// any cache and any class layer above it.
func (r liveRead) Get(id string) (beads.Bead, error) { return beads.LiveFor(r.store).Get(id) }

// Ready is beads.ReadyLive on the peeled store: its Backing() fast path
// where it has one.
func (r liveRead) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	return beads.ReadyLive(r.store, q...)
}

// liveReads is store's live read handle under v2's read-only wrappers
// (readOnlyStore, blindWriteRefusingStore: a v2 effect's stores), which do
// not promote their store's live capabilities (Handles, Backing). Each is
// found below any class layer (beads.As), so a class handle over one is
// peeled as the wrapper is, and a class handle over the store itself is read
// through LiveFor, which sees through every class layer: no handle type is
// named, so no handle a peel by hand forgot reads the cache.
func liveReads(store beads.Store) liveRead {
	for {
		if v, ok := beads.As[readOnlyStore](store); ok {
			store = v.Store
			continue
		}
		if v, ok := beads.As[blindWriteRefusingStore](store); ok {
			store = v.inner
			continue
		}
		return liveRead{store: store}
	}
}

// liveClaimVeto runs the ownership check for a demand-class drain decider and
// reports whether the drain must be skipped. It logs once per (session, claim)
// — the decider re-runs every tick while the veto holds — and records a
// kept_open trace each time it fires.
func liveClaimVeto(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	sw *SeatWork,
	info sessionpkg.Info,
	dt *drainTracker,
	name, reason string,
	stdout, stderr io.Writer,
) (vetoed bool, claimID string) {
	owns, claimID, err := sessionOwnsLiveClaim(cityPath, cfg, store, sw, info)
	if !owns {
		if dt != nil {
			dt.clearLiveClaimVeto(info.ID)
		}
		return false, ""
	}
	firstSighting := dt == nil || dt.noteLiveClaimVeto(info.ID, claimID)
	if firstSighting {
		if err != nil {
			fmt.Fprintf(stderr, "session reconciler: keeping '%s' (%s drain skipped): live claim check failed, treating claim as held: %v\n", name, reason, err) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "Skipping %s drain for '%s': session holds live claim %s\n", reason, name, claimID) //nolint:errcheck
		}
	}
	return true, claimID
}

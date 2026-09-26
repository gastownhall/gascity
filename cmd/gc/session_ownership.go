package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
	rigStores map[string]beads.Store,
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
	held, err := assignedWorkExistsForSession(cityPath, cfg, store, rigStores, info, func(s beads.Store) (bool, error) {
		work, err := liveBeadRead(s, claimID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		return claimedWorkStillHeldBy(work, info.ID), nil
	})
	if err != nil {
		return true, claimID, fmt.Errorf("reading claimed bead %s: %w", claimID, err)
	}
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

// liveBeadRead reads one bead through the store's authoritative live handle,
// bypassing a caching layer. The typed class wrappers (beads.SessionStore,
// beads.WorkStore) do not promote the Handles capability, so they are peeled
// first; without that the read would silently fall back to the cache.
func liveBeadRead(store beads.Store, id string) (beads.Bead, error) {
	switch v := store.(type) {
	case beads.SessionStore:
		store = v.Store
	case beads.WorkStore:
		store = v.Store
	}
	return beads.HandlesFor(store).Live.Get(id)
}

// liveClaimVeto runs the ownership check for a demand-class drain decider and
// reports whether the drain must be skipped. It logs once per (session, claim)
// — the decider re-runs every tick while the veto holds — and records a
// kept_open trace each time it fires.
func liveClaimVeto(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
	dt *drainTracker,
	name, reason string,
	stdout, stderr io.Writer,
) (vetoed bool, claimID string) {
	owns, claimID, err := sessionOwnsLiveClaim(cityPath, cfg, store, rigStores, info)
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

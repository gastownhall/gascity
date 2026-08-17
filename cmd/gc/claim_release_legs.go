package main

// Assigned-work release over the residency-resolved leg set (S0, #5264):
// upstream's sweep primitives, kept verbatim so demand, retirement, and
// drain-ack releases read identical store legs by construction.

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// resolved back to the leading store is deduped rather than scanned twice.
//
// These sweeps are VOID and their callers have no error channel, so a leg whose
// read fails is logged by visit and the pass continues — deliberately NOT the
// resolver's fail-loud policy, because the alternative on a best-effort
// idempotent sweep is releasing less on this tick and no diagnosis at all. The
// gates that DO have an error channel consume the policy
// (assignedWorkScanComplete).
//
// So be precise about what the bool means: visit returns nothing, so the
// executor never sees a leg error and res.Partial is always false here. It
// reports only that the city HAD a resolvable leg set — false is the refused
// city, where every infrastructure class answers with the refusal and a
// work-only sweep would release from the wrong ledger. Per-leg read failures
// are surfaced by visit's own stderr line and, for the one caller that reports
// a result, by unclaimResult.Failed; that is what stops the stranded-repair
// path from calling a repair clean.
func sweepAssignedWorkLegs(cityPath string, cfg *config.City, store beads.Store, rigStores map[string]beads.Store, identifiers []string, stderr io.Writer, visit func(index int, s beads.Store)) bool {
	if store == nil {
		return false
	}
	plan, err := assignedWorkSweepPlan(cityPath, cfg, store, rigStores, identifiers)
	if err != nil {
		// A refused city: its infrastructure classes answer nothing, and a
		// work-only sweep would release from the wrong ledger. Say so — the
		// refusal names the remedy, and a release pass that silently did
		// nothing is indistinguishable from one that found nothing.
		fmt.Fprintf(stderr, "session beads: no assigned-work leg set for this city: %v\n", err) //nolint:errcheck
		return false
	}
	index := 0
	res, walkErr := storeref.Walk(plan, func(leg storeref.Leg) (bool, error) {
		visit(index, leg.Store)
		index++
		return false, nil
	})
	return walkErr == nil && !res.Partial
}

// unclaimResult reports the outcome of one unassign sweep over a retired
// session bead's owned work: Released counts release attempts that completed
// without error, Failed counts ReleaseWorkBead errors (already logged per item
// to stderr). Released is deliberately NOT a count of writes — a release whose
// snapshot went stale (the work was re-claimed by a live worker before the
// write) correctly performs no write and reports no error, and is counted here
// with the ones that did write. Both mean the same thing to every caller: this
// session no longer holds that work. Only Failed distinguishes the case where
// that is still unknown. Void callers (named-session retirement, closed-
// session release) ignore it; the stranded-repair path reads Failed to avoid
// reporting a clean repair — or closing the session bead — when an unassign did
// not land, so a stale-assignee item is not masked behind a "repaired" close.

// unclaimWorkAssignedToRetiredSessionBead detaches every work bead a retired
// session still owns, across the leg set sweepAssignedWorkLegs resolves.
//
// The binding is one of those legs on every caller now, which it was not before:
// the reconciler leads with the sessions-class store (on a converged split, the
// binding itself), while `gc session close` and the stranded-repair sweep lead
// with the WORK store and used to hand the leg in by hand — or, in the Info
// twin's case, not at all. A claim claim_class_route.go routed into the binding
// was released by nothing on those paths, and a session that died without
// drain-ack stranded it until an operator ran `gc bd release-if-current`
// (ga-j4ob9).
func unclaimWorkAssignedToRetiredSessionBead(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	sessionBead beads.Bead,
	fallbackRoute string,
	stderr io.Writer,
) {
	if store == nil || strings.TrimSpace(sessionBead.ID) == "" {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	identifiers := sessionAssignmentIdentifiers(sessionBead)
	seen := make(map[string]struct{})
	sweepAssignedWorkLegs(cityPath, cfg, store, rigStores, identifiers, stderr, func(storeIndex int, ownerStore beads.Store) {
		wa := workAssignmentForStore(beads.WorkStore{Store: ownerStore})
		for _, status := range []string{"open", "in_progress"} {
			for _, assignee := range identifiers {
				work, err := wa.OpenAssignedTo(assignee, status, beads.TierBoth, true)
				if err != nil {
					fmt.Fprintf(stderr, "session beads: listing work assigned to retired session %s via %q: %v\n", sessionBead.ID, assignee, err) //nolint:errcheck
					continue
				}
				for _, item := range work {
					if session.IsSessionBeadOrRepairable(item) {
						continue
					}
					key := strconv.Itoa(storeIndex) + "\x00" + item.ID
					if _, ok := seen[key]; ok {
						continue
					}
					seen[key] = struct{}{}
					// The session owning this work is retired, so the work is fully
					// detached (not preserved to a new assignee). The release
					// primitive clears the assignee (empty-string) and stale
					// session-affinity metadata, resets in_progress to open
					// (otherwise the bead stays invisible to the work_query — Tier 1
					// needs an assignee match, Tiers 2/3 only match "ready"), and
					// stamps fallbackRoute run_target only when the bead is otherwise
					// unrouted — the same stale-affinity bug fixed on the retry,
					// reopen, orphan-pool, and closed-session release paths.
					if err := wa.ReleaseWorkBead(item, fallbackRoute); err != nil {
						fmt.Fprintf(stderr, "session beads: unclaiming work %s assigned to retired session %s: %v\n", item.ID, sessionBead.ID, err) //nolint:errcheck
					}
				}
			}
		}
	})
}

// releaseUnexecutedClaimsOnDrainAck gives back every in_progress WORK bead this
// session still holds when it acknowledges drain.
//
// Drain-ack means "I am done and I hold nothing." A session that reaches it
// still holding an in_progress claim never executed that claim — the correct
// worker contract orders drain-ack strictly after `gc bd close` — so the claim is
// parked work no one will run. Releasing it here makes "a session ends its last
// turn holding an unexecuted claim" structurally unrepresentable at the one place
// every ephemeral worker already terminates.
//
// It differs from unclaimWorkAssignedToRetiredSessionBead in exactly one way,
// and the difference is load-bearing: only in_progress is swept. Continuation
// preassignment writes an assignee onto OPEN siblings so they stay with the live
// context, and sweeping those would undo the preassignment the session's own
// claim just made. Everything else — the residency-correct leg set, the CAS,
// the session/mail exclusions — is deliberately the same machinery, because a
// second implementation of "release this session's work" is a second chance to
// disagree with the first.
//
// The leg set is sweepAssignedWorkLegs' — the same one the retired-session
// sweep reads, which is the point: "demand and sweep read the same stores" holds
// by construction, and a graph-resident claim is released here by the same leg
// that would have released it there. budget bounds the whole pass; see

// store.
func releaseUnexecutedClaimsOnDrainAck(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	sessionBead beads.Bead,
	budget time.Duration,
	stderr io.Writer,
) {
	if store == nil || strings.TrimSpace(sessionBead.ID) == "" {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	identifiers := sessionAssignmentIdentifiers(sessionBead)
	seen := make(map[string]struct{})
	deadline := time.Now().Add(budget)
	expired := false
	legsComplete := sweepAssignedWorkLegs(cityPath, cfg, store, rigStores, identifiers, stderr, func(storeIndex int, ownerStore beads.Store) {
		if expired {
			return
		}
		if time.Now().After(deadline) {
			expired = true
			fmt.Fprintf(stderr, "session beads: held-claim release for draining session %s ran out of its %s budget; remaining legs are left to the dead-assignee sweep\n", sessionBead.ID, budget) //nolint:errcheck
			return
		}
		wa := workAssignmentForStore(beads.WorkStore{Store: ownerStore})
		for _, assignee := range identifiers {
			if time.Now().After(deadline) {
				expired = true
				fmt.Fprintf(stderr, "session beads: held-claim release for draining session %s ran out of its %s budget; remaining identities are left to the dead-assignee sweep\n", sessionBead.ID, budget) //nolint:errcheck
				return
			}
			work, err := wa.OpenAssignedTo(assignee, "in_progress", beads.TierBoth, true)
			if err != nil {
				fmt.Fprintf(stderr, "session beads: listing in-progress work held by draining session %s via %q: %v\n", sessionBead.ID, assignee, err) //nolint:errcheck
				continue
			}
			for _, item := range work {
				if session.IsSessionBeadOrRepairable(item) {
					continue
				}
				key := strconv.Itoa(storeIndex) + "\x00" + item.ID
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				// No fallback route: a bead released here keeps whatever routing
				// it already carried, exactly as the close-release path does.
				// ReleaseWorkBead is compare-and-swap on the assignee, so a bead
				// that legitimately changed hands since the list is left alone.
				if err := wa.ReleaseWorkBead(item, ""); err != nil {
					fmt.Fprintf(stderr, "session beads: releasing unexecuted claim %s held by draining session %s: %v\n", item.ID, sessionBead.ID, err) //nolint:errcheck
				}
			}
		}
	})
	if !legsComplete {
		// The sweep already logged the refusal; say what it means here so the
		// drain-ack release is not mistaken for a clean sweep.
		fmt.Fprintf(stderr, "session beads: drain-ack release for %s ran on a partial leg set; the dead-assignee sweep will revisit\n", sessionBead.ID) //nolint:errcheck
	}
}

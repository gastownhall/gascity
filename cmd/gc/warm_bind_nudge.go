package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// warmBindNudgedForTriggerKey marks, on a pool slot's own session bead, the
// trigger bead id the warm-bind claim nudge was last delivered for. It is
// PERSISTED (not an in-memory grace map) so it survives a controller restart and
// cannot replay — the in-memory map of the reverted #312 idle nudger did not,
// which is precisely why that one re-nudge-stormed on every restart (test-5il). A
// fresh binding writes a different trigger id, so the marker mismatches and the
// nudge fires again: exactly once per binding.
const warmBindNudgedForTriggerKey = "warm_bind_nudged_for_trigger"

// warmBindNudgeIdleTimeout bounds how long the warm-bind claim nudge waits for
// the slot to reach an idle input prompt before delivering, so it never injects
// mid-turn. A warm slot with unclaimed bound work is normally already idle, so
// the wait returns at once; the bound only bites on a slot still finishing prior
// work, after which delivery is best-effort (mirroring cold Start's tolerance via
// startupNudgeIdleTimeout).
const warmBindNudgeIdleTimeout = 30 * time.Second

// warmClaimTriggerProbe reports whether a pool slot's bound trigger bead is still
// unclaimed — open and not yet claimed by this slot — read live from the store
// that holds it. It is built by the reconciler, where the city's opened routes and
// rig stores are in scope (buildWarmClaimTriggerProbe over
// CityRuntime.newWarmClaimTriggerResolver), and threaded to the warm-reuse branch
// of startPreparedStartCandidate via the start execution options. A nil probe
// (tests, and any start path that never builds one) suppresses the nudge entirely;
// a probe that returns false suppresses it for that session — the claim nudge fires
// only against a genuinely-unclaimed trigger, so it is structurally invisible to a
// slot that has already begun its work (fail-closed on any resolution uncertainty).
type warmClaimTriggerProbe func(session beads.Bead) bool

// warmClaimTriggerResolver reads one bound trigger bead by id, from wherever it
// lives. It is the residency contract's answer, handed to the probe already
// resolved: the probe's job is the claim question, not the store question.
type warmClaimTriggerResolver func(triggerID string) (beads.Bead, error)

// buildWarmClaimTriggerProbe returns a probe that reads a session bead's bound
// trigger through resolve and reports whether that trigger is still unclaimed.
//
// Reading the trigger live — rather than consulting the assignee-gated
// assigned-work snapshot the reverted poller used — is what lets the warm-bind
// path see a freshly bound, not-yet-claimed trigger at all: that snapshot omits an
// unassigned trigger, so the old poller was blind to exactly this case.
//
// The session's gc.trigger_bead_store_ref is NOT consulted. It records the demand
// LEG the row was counted under, which on a split city is a work ledger even when
// the bound step lives only in the class binding — so parsing it as a store
// selector resolved every such trigger to a store that misses, and the probe failed
// closed on work the slot was holding (ga-bmfmt). The stamp is still written, and
// other readers still use it; residency is the resolver's question.
//
// A read error — a resolution the topology cannot answer, a refused city, a dark
// leg — yields false: never nudge on uncertainty.
//
// That silence is safe but not free to operate: a declined nudge and a healthy
// fleet where every trigger happens to be claimed look identical from outside. So
// a fault — anything that is not the ordinary beads.ErrNotFound miss — is reported
// to faults ONCE per probe. The reconciler builds one probe per tick, which makes
// that once per tick rather than once per pool slot: a refused city or a dark
// binding suppresses the nudge for every slot at once, so a per-session line would
// be a fleet-sized burst of a single fact. A nil faults writer discards.
func buildWarmClaimTriggerProbe(cfg *config.City, resolve warmClaimTriggerResolver, faults io.Writer) warmClaimTriggerProbe {
	var reported sync.Once // the probe runs from async start goroutines too
	return func(session beads.Bead) bool {
		if resolve == nil {
			return false
		}
		triggerID := strings.TrimSpace(session.Metadata[beadmeta.TriggerBeadIDMetadataKey])
		if triggerID == "" {
			return false
		}
		wb, err := resolve(triggerID)
		if err != nil {
			if faults != nil && !errors.Is(err, beads.ErrNotFound) {
				reported.Do(func() {
					fmt.Fprintf(faults, "warm-bind claim nudge: resolving trigger %s failed, suppressing the nudge: %v\n", triggerID, err) //nolint:errcheck
				})
			}
			return false
		}
		return isUnclaimedTrigger(cfg, wb)
	}
}

// deliverWarmBindClaimNudge delivers a pool slot's claim nudge to an already
// running, idle slot that had on-demand work bound to it (bindPoolSessionTriggerBead)
// after it was last Started. Cold Provider.Start delivers this nudge as a
// side-effect of spawning the agent; a warm slot is never re-Started (the
// reconciler observes it running & alive and returns without a Start), so without
// this it sits idle at its prompt with unclaimed work it never began. This is the
// event-based counterpart, fired from startPreparedStartCandidate's warm-reuse
// branch — symmetric with cold Start, not a separate poll.
//
// Provider-agnostic by design (it does NOT gate on Capabilities().CanReportActivity).
// The bind edge — warm slot + unclaimed bound trigger + marker mismatch — is the
// same on every runtime, and coupling delivery to that capability flag would
// silently disable the nudge the moment herdr starts reporting activity. On herdr
// this is the ONLY path that closes the warm-bind gap (its GetLastActivity is
// zero, so the idle-timeout relaunch net never fires). On tmux it composes with
// that existing net as the fast primary nudge: the idle-timeout relaunch (which
// re-delivers the claim nudge via cold Start) remains the slower backstop, and the
// two gates below keep them from double-acting — a claimed bead stops matching the
// probe, and the persisted marker stops the re-fire.
//
// Churn-free by the same construction that inverts every failure mode of the
// reverted #312 idle nudger, but simpler — it keys on two independent gates, so
// even a wrong marker cannot disturb a working slot:
//   - Bead state: nudge only while the trigger is unclaimed. The instant the slot
//     claims, the bead flips to in_progress and the probe returns false — the
//     nudge is structurally invisible to a working slot.
//   - Persisted once-per-binding marker: fires exactly once per binding and
//     cannot replay across a controller restart.
//
// The cheap in-memory gates (pool slot, bound trigger, marker) short-circuit first
// so a steady warm fleet pays nothing; the store read (unclaimed probe) and the
// idle wait run only on the tick(s) right after a new binding, before the marker
// is set. Best-effort throughout: it never fails the (already-successful) warm start.
//
// When the controller's lane host executes message ops for store and a lane can
// type into the slot, the nudge is a planner notice op (warmBindNotice) the lane
// delivers at the slot's boundary, and the marker records that it was proposed,
// or given up on.
func deliverWarmBindClaimNudge(ctx context.Context, sp runtime.Provider, store beads.Store, session *beads.Bead, claimText string, probe warmClaimTriggerProbe) {
	if sp == nil || store == nil || session == nil || probe == nil {
		return
	}
	if strings.TrimSpace(session.Metadata["pool_managed"]) != "true" {
		return // pool slots only
	}
	name := strings.TrimSpace(session.Metadata["session_name"])
	if name == "" {
		return
	}
	triggerID := strings.TrimSpace(session.Metadata[beadmeta.TriggerBeadIDMetadataKey])
	if triggerID == "" {
		return // no bound work → nothing to claim
	}
	claimText = strings.TrimSpace(claimText)
	if claimText == "" {
		return // no claim instruction resolved → nothing to deliver
	}
	// Once-per-binding: the marker records the trigger we last nudged for. A match
	// means this binding was already handled (this tick, a prior tick, or before a
	// restart) — skip. A new/different binding mismatches and fires again.
	if strings.TrimSpace(session.Metadata[warmBindNudgedForTriggerKey]) == triggerID {
		return
	}
	// Churn invariant: only nudge while the trigger is genuinely unclaimed.
	if !probe(*session) {
		return
	}
	result := warmBindNoticeDirect
	if lane := plannerNoticeLaneFor(store); lane != nil {
		result = warmBindNotice(lane, *session, name, triggerID, claimText)
	}
	switch result {
	case warmBindNoticeRetry:
		return
	case warmBindNoticeDirect:
		if !nudgeWarmBindSlot(ctx, sp, name, triggerID, claimText) {
			return
		}
	}
	// Delivered, proposed or given up on: stamp the marker so this binding never nudges again. Persisted on
	// the session bead → restart-safe; mirrored in-memory so the rest of this tick
	// reads the just-written value.
	if err := sessionFrontDoor(store).SetMarker(session.ID, warmBindNudgedForTriggerKey, triggerID); err != nil {
		log.Printf("warm-bind claim nudge: marking %s failed: %v", session.ID, err)
		return
	}
	if session.Metadata == nil {
		session.Metadata = map[string]string{}
	}
	session.Metadata[warmBindNudgedForTriggerKey] = triggerID
	switch result {
	case warmBindNoticeProposed:
		log.Printf("warm-bind claim nudge: queued a claim notice for %s to claim %s", name, triggerID)
	case warmBindNoticeSpent:
		log.Printf("warm-bind claim nudge: gave up on the claim notice for %s to claim %s; the claim backstop owns re-delivery", name, triggerID)
	default:
		log.Printf("warm-bind claim nudge: nudged %s to claim %s", name, triggerID)
	}
}

// warmBindNoticeResult is what warmBindNotice did with a binding's claim
// nudge.
type warmBindNoticeResult int

const (
	// warmBindNoticeDirect: no lane can type into the slot; nudge it
	// directly. Nothing was written.
	warmBindNoticeDirect warmBindNoticeResult = iota
	// warmBindNoticeProposed: the notice op stands; stamp the marker.
	warmBindNoticeProposed
	// warmBindNoticeSpent: the notice was refused, or failed
	// plannerNoticeMaxFailures times in a row; stamp the marker, and the
	// claim backstop owns re-delivery.
	warmBindNoticeSpent
	// warmBindNoticeRetry: the notice failed; leave the marker unset so a
	// later tick retries.
	warmBindNoticeRetry
)

// warmBindNotice proposes the binding's claim nudge as a planner notice. The
// lane waits for the slot's boundary itself, so there is no idle wait here.
//
// The marker names only the trigger a slot was last nudged for, so a slot
// bound to A, then B, then A again in one incarnation is a new binding the
// key must tell apart from the first. Each binding takes the first episode
// (warmBindNoticeEpisode) whose op is not already written and past due: an
// op still due is this binding's own (a marker write that failed after its
// proposal), so the retry finds it pending and writes nothing new; an op no
// longer due belongs to an earlier binding. A trigger rebound more than
// warmBindNoticeMaxEpisodes times in one incarnation is given up on, and the
// claim backstop owns re-delivery. A failure is logged at most once per
// laneErrorLogInterval.
func warmBindNotice(lane plannerNoticeLane, session beads.Bead, name, triggerID, claimText string) warmBindNoticeResult {
	n := plannerNotice{
		Purpose:     warmBindNoticePurpose,
		Trigger:     triggerID,
		Causation:   triggerID,
		Attempt:     1,
		Content:     claimText,
		StartWithin: warmBindNoticeStartWithin,
	}
	failKey := plannerNoticeFailureKey(session.ID, n)
	for i := 1; i <= warmBindNoticeMaxEpisodes; i++ {
		n.Episode = warmBindNoticeEpisode(i)
		outcome, op, err := proposePlannerNotice(lane, session, n)
		switch {
		case err != nil && plannerNoticeRefused(err):
			lane.noticeRecovered(failKey)
			log.Printf("warm-bind claim nudge: notice for %s refused for trigger %s: %v", name, triggerID, err)
			return warmBindNoticeSpent
		case err != nil:
			failures, logNow := lane.noticeFailed(failKey)
			if failures >= plannerNoticeMaxFailures {
				lane.noticeRecovered(failKey)
				log.Printf("warm-bind claim nudge: notice for %s failed %d times in a row for trigger %s: %v", name, failures, triggerID, err)
				return warmBindNoticeSpent
			}
			if logNow {
				log.Printf("warm-bind claim nudge: notice for %s failed for trigger %s (%d of %d): %v", name, triggerID, failures, plannerNoticeMaxFailures, err)
			}
			return warmBindNoticeRetry
		case outcome == plannerNoticeAlreadyProposed && !plannerNoticeStillDue(op, lane.noticeNow()):
			continue // an earlier binding's notice; try the next episode
		}
		lane.noticeRecovered(failKey)
		if outcome == plannerNoticeDirect {
			return warmBindNoticeDirect
		}
		return warmBindNoticeProposed
	}
	lane.noticeRecovered(failKey)
	log.Printf("warm-bind claim nudge: %s was bound to %s more than %d times in one incarnation; no new notice", name, triggerID, warmBindNoticeMaxEpisodes)
	return warmBindNoticeSpent
}

// warmBindNoticeMaxEpisodes bounds how many bindings of one trigger in one
// incarnation get their own warm-bind notice (warmBindNotice).
const warmBindNoticeMaxEpisodes = 32

// warmBindNoticeEpisode is the Episode of a trigger's i-th warm-bind notice
// in one incarnation: none for the first, so a trigger bound once keys as
// <purpose>:<token>:<trigger>:1, then the binding's number.
func warmBindNoticeEpisode(i int) string {
	if i <= 1 {
		return ""
	}
	return strconv.Itoa(i)
}

// nudgeWarmBindSlot types the claim nudge into the slot directly, after
// waiting for its idle input prompt so it never injects mid-turn: a warm slot
// with unclaimed work is normally already idle (the wait returns at once);
// the bound only bites on a slot still finishing prior work. It reports
// whether the nudge was delivered. On failure (a TUI race, a transient) the
// marker stays unset so a later tick retries; the unclaimed gate keeps that
// retry safe (a claimed bead stops matching), and manual re-nudge remains the
// final escape hatch, as it was at the poller's give-up.
func nudgeWarmBindSlot(ctx context.Context, sp runtime.Provider, name, triggerID, claimText string) bool {
	if waiter, ok := sp.(runtime.IdleWaitProvider); ok {
		_ = waiter.WaitForIdle(ctx, name, warmBindNudgeIdleTimeout)
	}
	if err := sp.Nudge(name, runtime.TextContent(claimText)); err != nil {
		log.Printf("warm-bind claim nudge: %s failed for trigger %s: %v", name, triggerID, err)
		return false
	}
	return true
}

// warmBindNoticePurpose names the warm-bind claim nudge as a planner notice.
const warmBindNoticePurpose = "warm-bind-claim-nudge"

// warmBindNoticeStartWithin bounds how long the warm-bind notice waits for
// the slot's boundary: within the claim backstop's observe grace, after which
// that backstop owns re-delivery to a slot that still has not claimed.
const warmBindNoticeStartWithin = idleClaimNudgeGrace - plannerNoticeStartMargin

package main

// Demand/claim divergence diagnostics.
//
// The controller spawns a seat when it counts demand; the worker finds its own
// work. When those two reads disagree, the symptom is a seat that starts, reads
// empty, and drains — and from the outside that is indistinguishable from the
// HEALTHY case, where a sibling seat simply claimed the row first. One is the
// invariant breaking; the other is pull working exactly as designed.
//
// This file tells them apart, after the fact and only after the fact:
//
//   - It runs AFTER the drain result is written. The drain is not waiting on it,
//     cannot be changed by it, and is byte-identical whether it runs or not.
//   - It reads the trigger bead by id SOLELY to classify. That id is demand
//     bookkeeping, never an assignment: nothing on the claim path may consult it
//     to decide what to claim, and nothing here feeds back into that path.
//   - It is scoped to seats the controller spawned on demand evidence
//     (GC_SPAWN_ORIGIN=demand, presence only). A named session or a manual hook
//     draining empty is not evidence about anything.
//
// The divergence count is the rollout metric for the agreement fix: it should be
// zero. The benign count is expected to be nonzero forever, and a change that
// tried to drive IT to zero would be pathologizing correct pull.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// demandSpawnOriginValue is the presence-only marker the controller sets on a
// seat it minted from counted demand (build_desired_state.go).
const demandSpawnOriginValue = "demand"

// hookRecordDemandClaimDivergence is the emitter seam, replaced in tests.
var hookRecordDemandClaimDivergence = recordDemandClaimDivergence

// demandDivergenceRecorder opens the recorder this diagnostics event is written
// to. A var scoped to this emitter, so a test can read the event back without
// making the process-wide recorder constructor swappable.
var demandDivergenceRecorder = openCityRecorder

// recordDemandClaimDivergence publishes the diagnostics event for a
// demand-spawned seat that drained with no work. It is best-effort and silent on
// every failure: a diagnostics counter must never turn into a second failure
// mode on the drain path.
func recordDemandClaimDivergence(reason, dir string, opts hookClaimOptions, ops hookClaimOps, stderr io.Writer) {
	if hookClaimEnvValue(opts.Env, "GC_SPAWN_ORIGIN") != demandSpawnOriginValue {
		return
	}
	sessionID := hookClaimSessionID(opts.Env)
	if sessionID == "" {
		return
	}
	triggerID := hookClaimEnvValue(opts.Env, "GC_TRIGGER_WORK_BEAD_ID")
	status, classification := classifyDemandTrigger(triggerID, dir, opts, ops)
	payload, err := json.Marshal(events.SessionDemandClaimDivergencePayload{
		SessionID:            sessionID,
		Template:             hookClaimEnvValue(opts.Env, "GC_TEMPLATE"),
		DrainReason:          reason,
		TriggerBeadID:        triggerID,
		TriggerStatusAtDrain: status,
		Classification:       classification,
	})
	if err != nil {
		return
	}
	rec := demandDivergenceRecorder(io.Discard)
	if closer, ok := rec.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // diagnostics are best-effort
	}
	rec.Record(events.Event{
		Type:      events.SessionDemandClaimDivergence,
		Actor:     eventActor(),
		Subject:   triggerID,
		SessionID: sessionID,
		Message:   classification,
		Payload:   payload,
	})
	if classification == events.DemandClaimDivergence && stderr != nil {
		// The one classification worth a line on the worker's own stderr: the
		// row the controller counted is still sitting there, claimable, and this
		// seat could not see it.
		fmt.Fprintf(stderr, "gc hook --claim: demand/claim divergence: %s is still claimable but this seat's query did not serve it\n", triggerID) //nolint:errcheck
	}
}

// classifyDemandTrigger reads the trigger row and reports (status, verdict).
//
// A read that FAILS is never absence here either: it classifies as unknown
// rather than inventing a divergence, so a flaky store cannot manufacture the
// exact metric this counter exists to trust.
func classifyDemandTrigger(triggerID, dir string, opts hookClaimOptions, ops hookClaimOps) (status, classification string) {
	if strings.TrimSpace(triggerID) == "" {
		return "", events.DemandClaimUnknown
	}
	if ops.ReadWorkMeta == nil {
		return "", events.DemandClaimUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	bead, err := ops.ReadWorkMeta(ctx, dir, opts.Env, triggerID, opts.Assignee)
	if err != nil {
		return "unreadable", events.DemandClaimUnknown
	}
	status = strings.ToLower(strings.TrimSpace(bead.Status))
	// The invariant is about a row that is STILL claimable by a worker for this
	// template: open, unassigned, route-matching, and not excluded by the shared
	// serving rules. Anything else — a row that moved on, a sibling claim — is
	// correct pull.
	if status != "open" || !demandRowServable(bead) || !hookClaimMatchesRoute(bead, opts.RouteTargets) {
		return status, events.DemandClaimBenign
	}
	switch classifyDemandRowClaimability(bead, ops.nowOrWallClock()) {
	case demandRowClaimable:
		// Open, servable, route-matching, and claimable right now — the agreement
		// invariant breaking.
		return status, events.DemandClaimDivergence
	case demandRowBlockednessUnproven:
		// bd's is_blocked signal is a DENORMALIZED projection that can lag a
		// just-closed blocker, and production reads do not carry it at all — so
		// neither reading may decide this on its own. Bucketing every
		// no-projection row away from the counter would be worse than useless: it
		// is the reading production always hands us, so the divergence metric
		// would read zero forever and stop being the agreement signal it exists to
		// be. Settle it instead, against the same live deps the drain-ack open arm
		// settles on.
		return status, classifyDemandTriggerBlockedness(triggerID, dir, opts, ops)
	default:
		// Deferred: gated by defer_until (or bd's indefinite deferral), a fresh
		// bead-local field, so draining past it is correct pull.
		return status, events.DemandClaimBenign
	}
}

// classifyDemandTriggerBlockedness settles a trigger row whose blockedness the
// bead alone could not answer, by re-deriving it from live dependencies.
//
// A genuinely blocked row was never claimable, so it is not the agreement
// invariant breaking — but it is not folded into benign either: it gets its own
// bucket, because a routed row the controller keeps counting while no worker can
// take it is its own thing worth seeing. A row whose blocking deps turn out to be
// MET is claimable, so it counts as divergence exactly as if the projection had
// said so — a stale-true flag must not bury a real divergence.
//
// Every failure — no confirm seam wired, or a dep read that errors — reports
// unknown, never divergence and never blocked. Same discipline as the read above:
// a flaky store must not be able to manufacture the metric this counter exists to
// keep trustworthy.
func classifyDemandTriggerBlockedness(triggerID, dir string, opts hookClaimOptions, ops hookClaimOps) string {
	if ops.ConfirmBlocked == nil {
		return events.DemandClaimUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	blocked, err := ops.ConfirmBlocked(ctx, dir, opts.Env, triggerID, opts.Assignee)
	switch {
	case err != nil:
		return events.DemandClaimUnknown
	case blocked:
		return events.DemandClaimBlocked
	default:
		return events.DemandClaimDivergence
	}
}

// demandDivergenceOpsForBead is a tiny adapter so the classification read can be
// driven directly in tests without constructing a whole claim. The blockedness
// settlement answers "not blocked", the shape a row with no unmet plain `blocks`
// edge produces; demandDivergenceOpsForBlockedBead drives the other answers.
func demandDivergenceOpsForBead(bead beads.Bead, err error) hookClaimOps {
	return demandDivergenceOpsForBlockedBead(bead, err, false, nil)
}

// demandDivergenceOpsForBlockedBead is demandDivergenceOpsForBead with the
// live-dep blockedness settlement stubbed too, so a test can drive a confirmed
// blocker or a failed dependency read.
func demandDivergenceOpsForBlockedBead(bead beads.Bead, readErr error, blocked bool, confirmErr error) hookClaimOps {
	return hookClaimOps{
		ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
			return bead, readErr
		},
		ConfirmBlocked: func(context.Context, string, []string, string, string) (bool, error) {
			return blocked, confirmErr
		},
	}
}

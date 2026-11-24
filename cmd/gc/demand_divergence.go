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
//   - It reads the trigger bead by id, IN THE STORE THE CONTROLLER COUNTED IT
//     IN — resolved from the session row's gc.trigger_bead_store_ref stamp —
//     SOLELY to classify. That id is demand bookkeeping, never an assignment:
//     nothing on the claim path may consult it to decide what to claim, and
//     nothing here feeds back into that path. The id and the store ref come off
//     the seat's own session row, never its shell: no session env carries a
//     trigger id (gastownhall/gascity#5738). The seat's own store is never
//     consulted for the classification: re-reading it cannot see a
//     CROSS-STORE demand/claim disagreement, which is exactly the failure this
//     metric exists to catch (gastownhall/gascity#5858) — the production loop
//     there classified 2,658 such drains benign, divergence count zero.
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
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// demandSpawnOriginValue is the presence-only marker the controller sets on a
// seat it minted from counted demand (build_desired_state.go).
const demandSpawnOriginValue = "demand"

// demandTriggerUnreadable is the TriggerStatusAtDrain of a read that failed:
// the seat's session row, or the trigger row its stamp names in the store it
// was counted in.
const demandTriggerUnreadable = "unreadable"

// demandTriggerStoreUnresolved is the TriggerStatusAtDrain of a trigger whose
// counted store the seat could not place from its stamp: no stamp, another
// city, an unknown or suspended rig, or no resolver wired at all.
const demandTriggerStoreUnresolved = "store_unresolved"

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
//
// The second parameter is the drain's own store context, carried by the seam's
// fixed shape for the drain-ack fence ahead of it; the classification does NOT
// read through it. It reads the store the row's gc.trigger_bead_store_ref stamp
// names — which a stamp naming the drain's own store resolves back to anyway,
// so single-store verdicts are unchanged.
func recordDemandClaimDivergence(reason, _ string, opts hookClaimOptions, ops hookClaimOps, stderr io.Writer) {
	if hookClaimEnvValue(opts.Env, "GC_SPAWN_ORIGIN") != demandSpawnOriginValue {
		return
	}
	sessionID := hookClaimSessionID(opts.Env)
	if sessionID == "" {
		return
	}
	trigger, readErr := demandSessionTrigger(sessionID, ops)
	status, classification := demandTriggerUnreadable, events.DemandClaimUnknown
	if readErr == nil {
		status, classification = classifyDemandTrigger(trigger, opts, ops)
	}
	payload, err := json.Marshal(events.SessionDemandClaimDivergencePayload{
		SessionID:            sessionID,
		Template:             hookClaimEnvValue(opts.Env, "GC_TEMPLATE"),
		DrainReason:          reason,
		TriggerBeadID:        trigger.ID,
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
		Subject:   trigger.ID,
		SessionID: sessionID,
		Message:   classification,
		Payload:   payload,
	})
	if classification == events.DemandClaimDivergence && stderr != nil {
		// The one classification worth a line on the worker's own stderr: the
		// row the controller counted is still sitting there, claimable, and
		// this seat could not see it.
		fmt.Fprintf(stderr, "gc hook --claim: demand/claim divergence: %s is still claimable in %s, the store its demand was counted in, but this seat's query did not serve it\n", trigger.ID, trigger.StoreRef) //nolint:errcheck
	}
}

// demandSessionTrigger is the trigger stamp on sessionID's row: a zero
// demandTrigger when the row carries none, an error when it cannot be read.
// The caller records a failed read as demandTriggerUnreadable and unknown,
// never as the "no stamp" an empty id means, so a failed read neither
// manufactures a verdict nor passes for an unstamped seat.
func demandSessionTrigger(sessionID string, ops hookClaimOps) (demandTrigger, error) {
	if ops.ReadSessionTrigger == nil {
		return demandTrigger{}, nil
	}
	trigger, err := ops.ReadSessionTrigger(sessionID)
	if err != nil {
		return demandTrigger{}, err
	}
	trigger.ID = strings.TrimSpace(trigger.ID)
	trigger.StoreRef = strings.TrimSpace(trigger.StoreRef)
	return trigger, nil
}

// classifyDemandTrigger reads the trigger row in the store the controller
// counted it in — resolved from the session row's gc.trigger_bead_store_ref
// stamp — and reports (status, verdict).
//
// A read that FAILS, or a stamp the seat cannot place, is never evidence
// either way: both classify as unknown rather than inventing a divergence, so
// a flaky store or a foreign stamp cannot manufacture the exact metric this
// counter exists to keep trustworthy.
func classifyDemandTrigger(trigger demandTrigger, opts hookClaimOptions, ops hookClaimOps) (status, classification string) {
	if trigger.ID == "" {
		return "", events.DemandClaimUnknown
	}
	if ops.ReadWorkMeta == nil {
		return "", events.DemandClaimUnknown
	}
	leg, err := demandTriggerLeg(trigger.StoreRef, ops)
	if err != nil {
		return demandTriggerStoreUnresolved, events.DemandClaimUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	bead, err := ops.ReadWorkMeta(ctx, leg.dir, leg.env, trigger.ID, opts.Assignee)
	if err != nil {
		return demandTriggerUnreadable, events.DemandClaimUnknown
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
		// would read zero forever and stop being the agreement signal it exists
		// to be. Settle it instead, against the same live deps the drain-ack open arm
		// settles on.
		return status, classifyDemandTriggerBlockedness(trigger.ID, leg, opts, ops)
	case demandRowStatusBlocked:
		// bd's own blocked status is a fresh bead-local field and the ready query
		// excludes the row whatever its dependencies, so it is proof, not a
		// projection to settle: no live-dep read, and no way for a flaky dep read
		// to turn it into unknown or a met-deps one into divergence.
		return status, events.DemandClaimBlocked
	default:
		// Deferred: gated by a future defer_until or bd's deferred status, fresh
		// bead-local fields, so draining past it is correct pull. Held rows are
		// already benign through demandRowServable above; demandRowHeld lands
		// here for the same reason.
		return status, events.DemandClaimBenign
	}
}

// demandTriggerLeg resolves the leg that reads the store the stamp names. A nil
// seam (no city to resolve against) fails the same way an unplaceable ref does:
// neither is evidence, and neither may read anything.
func demandTriggerLeg(storeRef string, ops hookClaimOps) (hookStore, error) {
	if ops.DemandTriggerStore == nil {
		return hookStore{}, errors.New("no trigger-store resolver wired")
	}
	leg, err := ops.DemandTriggerStore(storeRef)
	if err != nil {
		return hookStore{}, err
	}
	if isZeroHookStore(leg) {
		return hookStore{}, fmt.Errorf("the trigger-store resolver returned no leg for %q", storeRef)
	}
	return leg, nil
}

// classifyDemandTriggerBlockedness settles a trigger row whose blockedness the
// bead alone could not answer, by re-deriving it from live dependencies — read
// in the counting store's leg, like the row itself.
//
// A genuinely blocked row was never claimable, so it is not the agreement
// invariant breaking — but it is not folded into benign either: it gets its own
// bucket, because a routed row the controller keeps counting while no worker can
// take it is its own thing worth seeing. A row whose blocking deps turn out to be
// MET is claimable, so it counts as divergence exactly as if the projection had
// said so — a stale-true flag must not bury a real divergence.
//
// Every failure — no confirm seam wired, or a dep read that errors — reports
// unknown, never divergence and never blocked. Same discipline as the read
// above: a flaky store must not be able to manufacture the metric this counter
// exists to keep trustworthy.
func classifyDemandTriggerBlockedness(triggerID string, leg hookStore, opts hookClaimOptions, ops hookClaimOps) string {
	if ops.ConfirmBlocked == nil {
		return events.DemandClaimUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	blocked, err := ops.ConfirmBlocked(ctx, leg.dir, leg.env, triggerID, opts.Assignee)
	switch {
	case err != nil:
		return events.DemandClaimUnknown
	case blocked:
		return events.DemandClaimBlocked
	default:
		return events.DemandClaimDivergence
	}
}

// demandTriggerStoreAt resolves a session row's gc.trigger_bead_store_ref stamp
// to the leg that reads the counted store, comparing the stamp the same way the
// census that wrote it compares store keys (normalizeDemandStoreRef), with the
// leg builders the hook fan-out already uses. It exists because the seat
// reading its own store cannot see a cross-store demand/claim disagreement
// (gastownhall/gascity#5858).
//
// A ref this seat cannot confidently place is an error, never a substitute leg:
// an unplaceable stamp classifies unknown, and reaching into a store the
// controller did not count in would classify on a row nobody demanded.
//
//   - "" is no stamp: nothing records where the row was counted.
//   - "city:<name>" with any name but this city's is another city's store. The
//     match is exact, as in namedTriggerRefIsSameStore.
//   - The "city" spellings — and a class ref, which is a city-scope store —
//     resolve onto the city leg (appendCityHookStore), the same co-residence
//     order the census counts by.
//   - "rig:<name>", or a bare name with no ":" (the assigned-work spelling),
//     resolve onto that rig's leg — unless the rig is unknown or suspended in
//     the frame the hook fan-out uses: a diagnostic must not reach toward a
//     stopped rig Dolt either.
//   - Anything else (the census's %p fallback key, unknown schemes) is not a
//     store a seat can read.
//
// The zero agent view carries no identity overrides on purpose: the read is by
// id, so there is no query identity to preserve, and the leg's store
// coordinates come from the view's dir alone.
func demandTriggerStoreAt(cityPath string, cfg *config.City, storeRef string) (hookStore, error) {
	storeRef = strings.TrimSpace(storeRef)
	if storeRef == "" {
		return hookStore{}, errors.New("resolving the trigger's store: the session row stamps no store")
	}
	if name, ok := strings.CutPrefix(storeRef, "city:"); ok && name != hookStoreCityName(cityPath, loadedCityName(cfg, cityPath)) {
		return hookStore{}, fmt.Errorf("resolving trigger store %q: another city's store", storeRef)
	}
	normalized := normalizeDemandStoreRef(storeRef)
	switch {
	case normalized == "city":
		if legs := appendCityHookStore(nil, cityPath, cfg, &config.Agent{}, nil); len(legs) == 1 {
			return legs[0], nil
		}
		return hookStore{}, fmt.Errorf("resolving trigger store %q: the city store's leg could not be built", storeRef)
	case strings.HasPrefix(normalized, "rig:"), !strings.Contains(normalized, ":"):
		return demandTriggerRigStoreAt(cityPath, cfg, storeRef, strings.TrimSpace(strings.TrimPrefix(normalized, "rig:")))
	default:
		return hookStore{}, fmt.Errorf("resolving trigger store %q: not a store a seat can read", storeRef)
	}
}

// demandTriggerRigStoreAt is demandTriggerStoreAt's rig arm: the named rig's
// leg, unless the rig is unknown, suspended in the frame the hook fan-out uses,
// or its leg cannot be built.
func demandTriggerRigStoreAt(cityPath string, cfg *config.City, storeRef, rig string) (hookStore, error) {
	st, err := loadSuspensionState(fsys.OSFS{}, cityPath)
	if err != nil {
		return hookStore{}, fmt.Errorf("resolving trigger store %q: loading rig suspension state: %w", storeRef, err)
	}
	if buildEffectiveSuspendedRigNames(cfg, st)[rig] {
		return hookStore{}, fmt.Errorf("resolving trigger store %q: rig %q is suspended", storeRef, rig)
	}
	if legs := appendOneRigHookStore(nil, cityPath, cfg, &config.Agent{}, rig, nil); len(legs) == 1 {
		return legs[0], nil
	}
	return hookStore{}, fmt.Errorf("resolving trigger store %q: no leg for rig %q", storeRef, rig)
}

// The divergence tests read and resolve through two canonical legs: the seat's
// own ("rig:rig", dir /rig) and the store the controller counted in ("city",
// dir /city). The double's city is named, so a "city:<name>" spelling resolves
// exactly the way the production resolver checks it.
const (
	demandDivergenceTestRigRef   = "rig:rig"
	demandDivergenceTestCityRef  = "city"
	demandDivergenceTestRigDir   = "/rig"
	demandDivergenceTestCityDir  = "/city"
	demandDivergenceTestCityName = "test-city"
)

// demandDivergenceTestTriggerStore is the resolver double the divergence test
// ops carry: the two canonical legs, and an error for any ref a seat cannot
// place — mirroring demandTriggerStoreAt's error arms without a city on disk.
func demandDivergenceTestTriggerStore(storeRef string) (hookStore, error) {
	storeRef = strings.TrimSpace(storeRef)
	if name, ok := strings.CutPrefix(storeRef, "city:"); ok && name != demandDivergenceTestCityName {
		return hookStore{}, fmt.Errorf("resolving trigger store %q: another city's store", storeRef)
	}
	normalized := normalizeDemandStoreRef(storeRef)
	switch {
	case normalized == demandDivergenceTestCityRef:
		return hookStore{dir: demandDivergenceTestCityDir}, nil
	case normalized == demandDivergenceTestRigRef:
		return hookStore{dir: demandDivergenceTestRigDir}, nil
	case strings.HasPrefix(normalized, "rig:"), !strings.Contains(normalized, ":"):
		return hookStore{}, fmt.Errorf("resolving trigger store %q: no leg for this rig", storeRef)
	default:
		return hookStore{}, fmt.Errorf("resolving trigger store %q: not a store a seat can read", storeRef)
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
// blocker or a failed dependency read. The session row reads as stamped with
// bead's id, counted in the city store.
func demandDivergenceOpsForBlockedBead(bead beads.Bead, readErr error, blocked bool, confirmErr error) hookClaimOps {
	return hookClaimOps{
		ReadSessionTrigger: func(string) (demandTrigger, error) {
			return demandTrigger{ID: bead.ID, StoreRef: demandDivergenceTestCityRef}, nil
		},
		DemandTriggerStore: demandDivergenceTestTriggerStore,
		ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
			return bead, readErr
		},
		ConfirmBlocked: func(context.Context, string, []string, string, string) (bool, error) {
			return blocked, confirmErr
		},
	}
}

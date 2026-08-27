package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// exactSessionProgressStallArm names which of D-STALL's two arms fired for one
// exact key. Both write the same effect — a fenced recycle — but they are
// separate decisions with separate thresholds and separate exemption gates, and
// the parity join needs to know which one legacy would have taken.
type exactSessionProgressStallArm string

const (
	exactSessionProgressStallArmNone        exactSessionProgressStallArm = ""
	exactSessionProgressStallArmClaimless   exactSessionProgressStallArm = "claimless"
	exactSessionProgressStallArmClaimHolder exactSessionProgressStallArm = "claim_holder"
)

// exactSessionProgressStallDecision is the whole D-STALL ladder's answer for one
// durable row.
type exactSessionProgressStallDecision struct {
	Arm          exactSessionProgressStallArm
	LastActivity time.Time
	Gap          time.Duration
	FloorExempt  bool
	HoldsClaim   bool
}

func (d exactSessionProgressStallDecision) recycles() bool {
	return d.Arm != exactSessionProgressStallArmNone
}

// decideExactSessionProgressStall runs legacy's progress-stall ladder
// (session_reconciler.go:2638-2739) over facts gathered per key. Every rung is
// legacy's, in legacy's order of precedence, with one deliberate reordering: the
// liveness observation moves BELOW the activity-gap check. Legacy already holds
// an `alive` bit for every row from its own fleet pass, so ordering it first
// costs it nothing; a keyed handler would have to pay a provider probe for every
// admission to answer the same question first. The rungs are ANDed either way,
// so the answer is identical and only already-stalled rows pay the probe — which
// is exactly the reason legacy gates its own store and health queries behind the
// cheap time check.
//
// The min-floor exemption is re-derived here, from one bounded row list, rather
// than threaded down from the sweep. It is fleet-shaped, and the sweep is where
// the ENQUEUE suppression lives (DETECTOR.md §3) — but a floor worker with a
// positive claim_holder_stall_timeout is deliberately enqueued, and without the
// bit the handler cannot reproduce legacy's `exempt || floorExempt` gate on the
// claim-less arm. Paying one list per candidate, after the cheap durable and
// activity rungs have already held, is D-DUP's precedent
// (exactSessionDuplicateNamedWinner) and keeps detector and handler answering
// from the same openPoolSessionCountForTemplate.
//
// Every gather failure fails SAFE — unreadable attachment or unreadable claim
// ownership suppresses the recycle rather than assuming the session is idle.
func decideExactSessionProgressStall(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) exactSessionProgressStallDecision {
	var none exactSessionProgressStallDecision
	if clk == nil {
		clk = clock.Real{}
	}
	// Revision 0 is the legacy-at-0 residual (DETECTOR.md §3b): a row no
	// conditional writer has ever fenced. Refuse it and let the first
	// unconditional write self-heal it.
	if params.Store == nil || params.Config == nil || params.Provider == nil || response.Revision == 0 {
		return none
	}
	claimless := params.Config.Session.ProgressStallTimeoutDuration()
	claimHolder := params.Config.Session.ClaimHolderStallTimeoutDuration()
	gate := minPositiveDuration(claimless, claimHolder)
	if gate <= 0 {
		return none
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" || info.Closed || !detectorBeadAwake(info) {
		return none
	}
	// A pinned configured named session is an operator-declared critical
	// conversation. Legacy's restart block refuses the abrupt kill for one
	// (session_reconciler.go:2766) unless continuation_reset_pending already
	// says an explicit controller reset is under way, so legacy's net effect on
	// such a row is: set the marker, decline the kill, clear the marker. The
	// keyed handler persists the marker PAIR before it stops anything, which a
	// crash could leave behind looking exactly like that explicit reset — and
	// legacy's next pass would then kill the session it is meant to protect.
	// Refusing the row here is net-identical to legacy with no such window.
	if pinnedConfiguredNamedSessionKillProtected(info) {
		return none
	}
	if !sessionActivityReportable(params.Provider, name) {
		return none
	}
	lastActivity, err := params.Provider.GetLastActivity(name)
	now := clk.Now().UTC()
	if err != nil || lastActivity.IsZero() || now.Sub(lastActivity) <= gate {
		return none
	}
	// Legacy's arm requires a live process. A durably-awake row with a dead
	// runtime is D-ORPHAN's and D-SLEEP's condition, not this family's — and this
	// rung is also what makes the recycle exactly-once: the incarnation this
	// handler just killed can never re-satisfy it.
	if _, alive := observeRuntimeProviderLiveness(params.Provider, name, drainAckStopPendingProcessNames(params.Config, info)); !alive {
		return none
	}

	decision := exactSessionProgressStallDecision{LastActivity: lastActivity, Gap: now.Sub(lastActivity)}
	exempt := pendingInteractionKeepsAwakeInfo(info, params.Provider, name, clk) ||
		pendingCreateStartInFlightInfo(info, clk, params.Config.Session.StartupTimeoutDuration())
	if !exempt {
		attached, attachErr := sessionAttachedForConfigDrift(info.ID, params.Provider, params.CityPath, params.Store, params.Config, name)
		if attachErr != nil || attached {
			exempt = true
		}
	}
	if !exempt {
		decision.FloorExempt = exactSessionMinFloorIdleWorker(params, info)
	}

	holdsClaim := false
	claimKnown := true
	if !exempt && (!decision.FloorExempt || claimHolder > 0) {
		has, claimErr := sessionHasInProgressAssignedWorkForConfig(params.CityPath, params.Config, params.Store, params.RigStores, info)
		if claimErr != nil {
			holdsClaim = true
			claimKnown = false
		} else {
			holdsClaim = has
		}
	}
	decision.HoldsClaim = holdsClaim

	providerHealthy := true
	if !exempt && (!decision.FloorExempt || holdsClaim) {
		if provider := exactSessionProgressStallProviderName(params, info); provider != "" {
			// The sweep's once-per-sweep snapshot, not a per-key file read
			// (WD.11): this guard runs on every candidate admission, so a file
			// read here is a read per key rather than per tick.
			if healthy, present := exactSessionProviderHealth(params).check(provider); present {
				providerHealthy = healthy
			}
		}
	}

	switch {
	case sessionProgressStalled(claimless, holdsClaim, providerHealthy, exempt || decision.FloorExempt, lastActivity, now):
		decision.Arm = exactSessionProgressStallArmClaimless
	case claimKnown && sessionClaimHolderStalled(claimHolder, holdsClaim, providerHealthy, exempt, lastActivity, now):
		decision.Arm = exactSessionProgressStallArmClaimHolder
	}
	return decision
}

// exactSessionMinFloorIdleWorker answers the fleet-shaped half of the ladder for
// one key: is this row part of its pool's always-warm contingent. It calls the
// same openPoolSessionCountForTemplate the sweep calls, over the rows the tick
// would have handed the sweep, so the two sides cannot drift. A store failure
// fails SAFE by exempting: an unreadable fleet must not recycle a floor worker.
func exactSessionMinFloorIdleWorker(params exactSessionStartParams, info sessionpkg.Info) bool {
	template := normalizedSessionTemplateInfo(info, params.Config)
	cfgAgent := findAgentByTemplate(params.Config, template)
	if cfgAgent == nil {
		return false
	}
	minFloor := cfgAgent.EffectiveMinActiveSessions()
	if minFloor <= 0 {
		return false
	}
	rows, err := sessionFrontDoor(params.Store).ListAllForReconcile(sessionpkg.ListAllOptions{})
	if err != nil {
		return true
	}
	infoByID := make(map[string]sessionpkg.Info, len(rows))
	for i := range rows {
		infoByID[rows[i].Info.ID] = rows[i].Info
	}
	return isMinFloorIdleWorker(minFloor, openPoolSessionCountForTemplate(infoByID, params.Config, template))
}

// exactSessionProgressStallProviderName resolves the row's provider name for the
// health gate. Legacy reads it off the tick's resolved TemplateParams, which a
// keyed guard cannot afford to rebuild for every admission; the durable row
// records the provider its live incarnation actually started under, which is the
// authority for a session that is running right now. It falls back to the
// fleet's own spelling (agent override, then the city default) for a row written
// before the mirror existed, and an unresolvable provider leaves the gate
// fail-open exactly as legacy's `tp.ResolvedProvider != nil` guard does.
func exactSessionProgressStallProviderName(params exactSessionStartParams, info sessionpkg.Info) string {
	var agentProvider string
	if cfgAgent := findAgentByTemplate(params.Config, normalizedSessionTemplateInfo(info, params.Config)); cfgAgent != nil {
		agentProvider = cfgAgent.Provider
	}
	return strings.TrimSpace(firstNonEmpty(info.Provider, agentProvider, params.Config.Session.Provider))
}

// exactSessionProgressStallCandidate is the seam's guard: a predicate over the
// durable row, never over admission.Source.
//
// It runs the WHOLE ladder, not just the trigger rung, and that is deliberate.
// Legacy evaluates the stall arm before its max-age and idle arms, and a firing
// stall `continue`s the row past them while a non-firing one falls through. A
// candidacy-only guard would claim every quiet row the ladder then declines and
// starve D-DEADLINE of it on every sweep; running the decision here reproduces
// legacy's precedence exactly. The handler re-runs the same decision against its
// own fresh re-read (A1), so the row — never the detector's reason — is the
// authority for the effect.
func exactSessionProgressStallCandidate(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) bool {
	return decideExactSessionProgressStall(params, info, response, clk).recycles()
}

// reconcileExactSessionProgressStallRecycle recycles ONE progress-stalled
// session by exact key. It adds no second recycle implementation: it persists
// legacy's own restart_requested marker — paired with continuation_reset_pending
// so the row is a reset .103's machinery recognizes — and then hands the key to
// commitExactSessionResetHandoff, which performs the token-bound unattended
// stop, confirms the death, and commits the same RestartRequestPatch legacy's
// restart block commits.
//
// The two writes are one effect, not two: the marker pair is the request and
// the handoff is its commit, and a crash between them leaves a row that the
// next admission re-drives through this same handler. (The commit body's own
// check-then-write is flagged for a WF-fold fence in ga-f7v2ft.133 item 2; it is
// reused here unchanged.)
//
// It always answers exactSessionStartKeyedOwner: the legacy yield here is an
// exclusion predicate, not an owner transfer, so there is no auto-mode handoff
// to fall back to.
func reconcileExactSessionProgressStallRecycle(
	admission sessionStartAdmission,
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	clk clock.Clock,
) (exactSessionStartOwner, error) { //nolint:unparam // owner is the detector-family seam's contract shape; this family never hands back to legacy
	if clk == nil {
		clk = clock.Real{}
	}
	stderr := params.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	decision := decideExactSessionProgressStall(params, info, response, clk)
	if !decision.recycles() {
		// Re-derived away between admission and dispatch, or a rung the sweep
		// could not see (claim, attachment, provider health, floor) declines it.
		// Release the key with zero effect; the condition is level-triggered.
		recordExactSessionProgressStallTrace(params, admission, info, decision, 0, false)
		return exactSessionStartKeyedOwner, nil
	}
	if _, ok := params.Provider.(runtime.FreshLivenessObserver); !ok {
		return exactSessionStartKeyedOwner, fmt.Errorf("exact progress-stalled session %q provider cannot prove fresh liveness", info.ID)
	}
	if _, ok := params.Provider.(runtime.UnattendedSessionStopper); !ok {
		return exactSessionStartKeyedOwner, fmt.Errorf("exact progress-stalled session %q provider cannot prove unattended stop", info.ID)
	}

	cfgAgent := findAgentByTemplate(params.Config, resolvedSessionTemplateInfo(info, params.Config))
	if cfgAgent == nil {
		return exactSessionStartKeyedOwner, nil
	}
	tp, resolvedInfo, err := resolveExactSessionStartTemplate(params, info, cfgAgent, clk, stderr)
	// Fold the resolver's Info back: the named path may have durably cleared a
	// stale trigger stamp, and the recycle below re-reads and fences off info.
	info = resolvedInfo
	if err != nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("recycling progress-stalled session %q: resolving template: %w", info.ID, err)
	}

	startedAt := time.Now()
	if writeErr := sessionFrontDoor(params.Store).ApplyPatch(info.ID, sessionpkg.MetadataPatch{
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
	}); writeErr != nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("requesting progress-stall recycle for %q: %w", info.ID, writeErr)
	}
	requested, requestedResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
	if readErr != nil {
		return exactSessionStartKeyedOwner, fmt.Errorf("re-reading progress-stalled session %q after the recycle request: %w", info.ID, readErr)
	}
	if !exactOrdinaryResetAuthorityMatches(requested, info) {
		return exactSessionStartKeyedOwner, nil
	}
	committed, _, resetErr := commitExactSessionResetHandoff(
		params, requested, requestedResponse, tp, clk, stderr, exactSessionProgressStallResetCurrent,
	)
	if resetErr != nil {
		recordExactSessionProgressStallTrace(params, admission, info, decision, time.Since(startedAt), false)
		return exactSessionStartKeyedOwner, fmt.Errorf("recycling progress-stalled session %q: %w", info.ID, resetErr)
	}
	if !exactOrdinaryResetCommitted(committed) {
		recordExactSessionProgressStallTrace(params, admission, info, decision, time.Since(startedAt), false)
		return exactSessionStartKeyedOwner, fmt.Errorf("recycling progress-stalled session %q: the restart handoff did not commit", info.ID)
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	gap := decision.Gap.Round(time.Second)
	fmt.Fprintf(stderr, "session reconciler: %s progress-stalled (%s arm, no progress for %s); requesting fresh restart\n", name, decision.Arm, gap) //nolint:errcheck // best-effort stderr
	recordExactSessionProgressStallTrace(params, admission, info, decision, time.Since(startedAt), true)
	return exactSessionStartKeyedOwner, nil
}

// exactSessionProgressStallResetCurrent is D-STALL's pre-stop authority for the
// shared reset machinery. It deliberately does NOT reuse exactOrdinaryResetCurrent:
// that predicate is the ordinary reset family's OWNERSHIP lattice, and it
// excludes named, pool-managed and dependency-bearing rows — which is precisely
// the population a progress-stall recycle targets. What this family needs proven
// between the admission and the stop is narrower and is the same thing legacy's
// restart block re-reads: the row is still open and still owes the reset this
// handler persisted a moment ago.
func exactSessionProgressStallResetCurrent(info sessionpkg.Info) bool {
	return !info.Closed && (exactOrdinaryResetRequested(info) || exactOrdinaryResetCommitted(info))
}

// recordExactSessionProgressStallTrace fires the SAME legacy site both D-STALL
// arms record at, with effect_owner=keyed and the honest effect_applied.
//
// The site is TraceSiteReconcilerProgressStallExempt for the recycle arm too:
// legacy traces only its exemption, so the recycle arm has no legacy site of its
// own (WD.1 delta 3), and the two arms are told apart by their detector reason
// rather than by their site. Legacy's own exempt record keeps its
// TraceReasonMinFloorIdleWorker / TraceOutcomeExempt pair, which the
// detector-shadow vocabulary deliberately does not reuse, so the WD.15 join can
// separate the legacy, keyed and detector-shadow populations on a shared cycle.
func recordExactSessionProgressStallTrace(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	decision exactSessionProgressStallDecision,
	duration time.Duration,
	applied bool,
) {
	if params.Trace == nil {
		return
	}
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_progress_stall_recycle", time.Now().UTC(), params.Config)
	if cycle == nil {
		return
	}
	template := normalizedSessionTemplateInfo(info, params.Config)
	reason := detectorReasonProgressStall
	outcome := TraceOutcomeStop
	if !applied {
		outcome = TraceOutcomeSkipped
		if decision.FloorExempt {
			reason = detectorReasonProgressStallExempt
		}
	}
	cycle.recordKeyedEffect(
		TraceSiteReconcilerProgressStallExempt,
		reason,
		outcome,
		"exact_session_progress_stall_recycle",
		template,
		info.ID,
		info.SessionNameMetadata,
		duration,
		map[string]any{
			"admission":         string(admission.Source),
			"admission_version": admission.Version,
			"generation":        params.Generation,
			"instance_token":    info.InstanceToken,
			"stall_arm":         string(decision.Arm),
			"floor_exempt":      decision.FloorExempt,
			"holds_claim":       decision.HoldsClaim,
			"idle_gap_seconds":  int64(decision.Gap.Seconds()),
			"effect_owner":      detectorKeyedEffectOwner,
			"effect_applied":    applied,
		},
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact progress-stall recycle trace: %v\n", err) //nolint:errcheck // tracing is observational
	}
}

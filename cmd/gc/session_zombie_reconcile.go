package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// exactSessionZombieCandidate is one row's zombie candidacy, carried from the
// seam guard into the handler.
type exactSessionZombieCandidate struct {
	SessionName  string
	Template     string
	ProcessNames []string
}

// exactSessionZombieMarkCandidate is the D-ZOMBIE seam's guard. It answers from
// the DURABLE ROW plus the tick's already-paid fleet liveness view — never from
// admission.Source, and never from a probe of its own.
//
// The last rung is the whole design problem of this family. `running ∧ !alive`
// has no durable shadow — it is pure provider I/O — while "awake, unmarked, with
// a live token" is the shape of every healthy session. A guard that stopped at
// the durable rungs would claim every admission in the fleet; a guard that
// probed would put one provider call on the ordinary start path for every key,
// which is a real regression against the exact-start cost bar
// (TestExactSessionStatusShadowOneKeyCostDoesNotGrowWithFleet asserts ZERO
// provider calls for one ordinary keyed start).
//
// So the fleet publishes what it already observed. The patrol sweep probes
// bead-awake rows once per tick — O(awake), a declared §2 input — and hands that
// view back through exactSessionStartParams.SessionLiveness, the same threading
// WD.3 used for DesiredSessionNames and for the same reason: the fact is
// fleet-shaped, and re-deriving it per key would turn an O(1) handler into a
// probing one. The view is a SCHEDULING filter, not authority — it decides only
// whether this key is worth a fresh look, and the handler re-observes before it
// writes anything (A1). An unpublished view declines the family, which is
// fail-safe and level-triggered: the next sweep re-detects.
//
// The durable rungs above it are legacy's own population: a fenced, open, named
// row with a live incarnation token, durably ACTIVE or AWAKE (legacy's arm runs
// on the desired fast path, so a creating/start_pending row — a start in flight,
// with no incarnation to declare dead — belongs to pending-create rollback), and
// not already carrying a terminal-provider mark, which is what makes the family
// exactly-once by key.
func exactSessionZombieMarkCandidate(
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
) (exactSessionZombieCandidate, bool) {
	// Revision 0 is the legacy-at-0 residual (DETECTOR.md §3b): a row no
	// conditional writer has ever fenced. Refuse it and let the first
	// unconditional write self-heal it.
	if params.Store == nil || params.Config == nil || params.Provider == nil || response.Revision == 0 {
		return exactSessionZombieCandidate{}, false
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if info.Closed || name == "" || strings.TrimSpace(info.InstanceToken) == "" || !isKnownStateInfo(info) {
		return exactSessionZombieCandidate{}, false
	}
	switch sessionpkg.State(strings.TrimSpace(info.MetadataState)) {
	case sessionpkg.StateActive, sessionpkg.StateAwake:
	default:
		return exactSessionZombieCandidate{}, false
	}
	if sessionHasProviderTerminalErrorInfo(info) {
		return exactSessionZombieCandidate{}, false
	}
	if !exactSessionObservedZombie(params, info.ID) {
		return exactSessionZombieCandidate{}, false
	}
	template := normalizedSessionTemplateInfo(info, params.Config)
	if template == "" {
		template = info.Template
	}
	return exactSessionZombieCandidate{
		SessionName:  name,
		Template:     template,
		ProcessNames: detectorProcessNames(params.Config, info),
	}, true
}

// exactSessionObservedZombie reports whether the tick's published fleet view saw
// this exact row running with a dead agent process. A missing view, or a row the
// view never probed, answers false.
func exactSessionObservedZombie(params exactSessionStartParams, sessionID string) bool {
	if params.SessionLiveness == nil {
		return false
	}
	bits, ok := params.SessionLiveness()[sessionID]
	return ok && bits.Probed && bits.Running && !bits.Alive
}

// reconcileExactSessionZombieMark marks one dead-process incarnation unhealthy
// by exact key. It adds no second classifier and no second write path: the
// scrollback peek, runtime.ProviderTerminalErrorReason and
// markProviderTerminalError are the fleet arm's own machinery, narrowed to one
// key.
//
// The order is legacy's, and each step is deliberate:
//
//	peek → classify → fence → mark → SessionCrashed → telemetry
//
// The classification LICENSES the mark: legacy stamps the health cluster only
// from a reason it recognized, so an unreadable or unrecognizable pane leaves
// the row untouched. The forensic event fires outside that check, exactly as
// legacy fires it, because a crash is worth reporting whether or not its cause
// has a name — with legacy's one exception, a pane showing a provider
// rate-limit screen, which is a throttle rather than a crash.
//
// The single fenced effect is the mark. It takes the conditional writer where
// the store has one and the front door where it does not — WD.2's delta 3
// shape, for WD.2's reason: conditional writes are an off-by-default per-store
// capability, so requiring one would make the family yield on most cities.
func reconcileExactSessionZombieMark(
	ctx context.Context,
	admission sessionStartAdmission,
	params exactSessionStartParams,
	info sessionpkg.Info,
	response sessionpkg.PersistedResponse,
	candidate exactSessionZombieCandidate,
	clk clock.Clock,
) error {
	if clk == nil {
		clk = clock.Real{}
	}
	if ctx != nil && ctx.Err() != nil {
		return nil
	}

	startedAt := time.Now()
	// A1: the guard's fleet view is up to one patrol old, so the handler makes
	// its OWN observation before anything else and refuses with zero effect if
	// the row has recovered — a replacement incarnation must never inherit the
	// dead one's mark. This is the single observation that licenses the effect;
	// the published view only decided the key was worth looking at.
	if running, alive := observeRuntimeProviderLiveness(params.Provider, candidate.SessionName, candidate.ProcessNames); !running || alive {
		recordExactSessionZombieTrace(params, admission, info, candidate, "", TraceOutcomeKeptOpen, time.Since(startedAt), false)
		return nil
	}
	peek := cachedSessionPeek(params.CityPath, params.Store, params.Provider, params.Config, info.ID, candidate.ProcessNames)
	output, err := peek(rateLimitPeekLines)
	if err != nil || output == "" {
		// No forensics to classify. Refuse with zero effect; the condition is
		// level-triggered, so the next sweep re-detects while the incarnation is
		// still dead.
		recordExactSessionZombieTrace(params, admission, info, candidate, "", TraceOutcomeKeptOpen, time.Since(startedAt), false)
		return nil
	}

	if reason := runtime.ProviderTerminalErrorReason(output); reason != "" {
		latest, latestResponse, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
		if readErr != nil {
			return fmt.Errorf("re-reading exact zombie session %q: %w", info.ID, readErr)
		}
		if latestResponse.Revision != response.Revision || latest.Closed ||
			strings.TrimSpace(latest.InstanceToken) != strings.TrimSpace(info.InstanceToken) ||
			strings.TrimSpace(latest.SessionNameMetadata) != candidate.SessionName ||
			sessionHasProviderTerminalErrorInfo(latest) {
			// The row moved between admission and the peek. Release the key with
			// zero effect: the row is the authority.
			return nil
		}
		if err := persistExactSessionZombieMark(params, latest, latestResponse.Revision, clk, reason); err != nil {
			return err
		}
		recordExactSessionZombieTrace(params, admission, latest, candidate, reason, TraceOutcomeUnhealthy, time.Since(startedAt), true)
	}

	// Forensics. Legacy emits these outside the reason check and suppresses them
	// for a provider rate-limit screen, which is a throttle rather than a crash.
	if !runtime.ContainsProviderRateLimitScreen(output) && params.Recorder != nil {
		displayName := candidate.SessionName
		if agent := findAgentByTemplate(params.Config, candidate.Template); agent != nil && strings.TrimSpace(agent.Name) != "" {
			displayName = strings.TrimSpace(agent.Name)
		}
		params.Recorder.Record(events.Event{
			Type:    events.SessionCrashed,
			Actor:   "gc",
			Subject: displayName,
			Message: output,
			Payload: api.SessionLifecyclePayloadJSON(info.ID, candidate.Template, "zombie process"),
		})
		telemetry.RecordAgentCrash(context.Background(), displayName, output)
	}
	return nil
}

// persistExactSessionZombieMark applies the terminal-provider-error cluster as
// ONE fenced write. The batch is markProviderTerminalError's own — the same
// keys in the same shape — so the keyed and fleet arms leave byte-identical
// rows; only the fence differs.
func persistExactSessionZombieMark(
	params exactSessionStartParams,
	info sessionpkg.Info,
	revision int64,
	clk clock.Clock,
	reason string,
) error {
	if params.StatusWriter == nil {
		if _, err := markProviderTerminalError(info, sessionFrontDoor(params.Store), clk, reason); err != nil {
			return fmt.Errorf("marking exact zombie session %q unhealthy: %w", info.ID, err)
		}
		return nil
	}
	now := clk.Now().UTC()
	//nolint:dupl // deliberately the same batch markProviderTerminalError writes.
	patch := map[string]string{
		"state":                                 string(sessionpkg.StateAsleep),
		"sleep_reason":                          string(sessionpkg.SleepReasonProviderTerminalError),
		"last_woke_at":                          "",
		"pending_create_claim":                  "",
		"pending_create_started_at":             "",
		sessionHealthStateMetadataKey:           "unhealthy",
		sessionHealthReasonMetadataKey:          reason,
		sessionDrainableMetadataKey:             boolMetadata(true),
		sessionProviderTerminalErrorMetadataKey: reason,
		sessionProviderTerminalErrorAtKey:       now.Format(time.RFC3339),
	}
	if err := params.StatusWriter.UpdateIfMatch(info.ID, revision, beads.UpdateOpts{Metadata: patch}); err != nil {
		// A lost fence here is not necessarily a lost race with a competing
		// intent. Legacy's exit-classification lane (checkRateLimitStability)
		// writes this SAME cluster for the same dead row from the same peek and
		// deliberately does not yield — it also owns rate-limit quarantine,
		// which the keyed arm does not replace. So re-read: if the row already
		// carries the terminal mark, the condition converged and this arm has
		// nothing left to do. Only a genuinely unmarked row is an error.
		latest, _, readErr := getAuthoritativeSessionStartPersistedRecord(params.Store, info.ID)
		if readErr == nil && sessionHasProviderTerminalErrorInfo(latest) {
			return nil
		}
		return fmt.Errorf("marking exact zombie session %q unhealthy: %w", info.ID, err)
	}
	return nil
}

// recordExactSessionZombieTrace fires the SAME legacy trace site the fleet arm
// fires (TraceSiteReconcilerTerminalProviderError, the site the sweep's shadow
// arm already records at), with effect_owner=keyed and the honest
// effect_applied, so the WD.15 parity join separates the legacy, keyed and
// detector-shadow populations on a shared cycle.
//
// The reason code follows legacy's: the classified provider reason where there
// is one, and the detector vocabulary's own code where the arm refused before
// classifying — a refusal has no provider reason to name, and inventing one
// would put a value in the join that legacy never emits.
func recordExactSessionZombieTrace(
	params exactSessionStartParams,
	admission sessionStartAdmission,
	info sessionpkg.Info,
	candidate exactSessionZombieCandidate,
	reason string,
	outcome TraceOutcomeCode,
	duration time.Duration,
	applied bool,
) {
	if params.Trace == nil {
		return
	}
	cycle := params.Trace.BeginCycle(TraceTickTriggerControl, "exact_session_zombie_mark", time.Now().UTC(), params.Config)
	if cycle == nil {
		return
	}
	template := candidate.Template
	if template == "" {
		template = normalizedSessionTemplateInfo(info, params.Config)
	}
	code := detectorReasonZombie
	if strings.TrimSpace(reason) != "" {
		code = TraceReasonCode(reason)
	}
	cycle.recordKeyedEffect(
		TraceSiteReconcilerTerminalProviderError,
		code,
		outcome,
		"exact_session_zombie_mark",
		template,
		info.ID,
		info.SessionNameMetadata,
		duration,
		map[string]any{
			"admission":         string(admission.Source),
			"admission_version": admission.Version,
			"generation":        params.Generation,
			"instance_token":    info.InstanceToken,
			"session_bead_id":   info.ID,
			"effect_owner":      detectorKeyedEffectOwner,
			"effect_applied":    applied,
		},
	)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil && params.Stderr != nil {
		fmt.Fprintf(params.Stderr, "session reconciler: recording exact zombie mark trace: %v\n", err) //nolint:errcheck // tracing is observational
	}
}

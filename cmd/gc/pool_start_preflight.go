package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

const (
	poolStartPreflightBypassed = "work_query_preflight_bypassed"
	poolStartPreflightWork     = "work_query_preflight_work"
	poolStartPreflightEmpty    = "work_query_preflight_empty"
	poolStartPreflightFailed   = "work_query_preflight_failed"
)

type poolStartPreflightResult struct {
	AllowStart bool
	Outcome    string
	Err        error
	Started    time.Time
	Finished   time.Time
}

// runPoolStartWorkQueryPreflight is the last zero-cost gate before an on-demand
// pool candidate mutates its wake metadata and reaches runtime.Provider.Start.
// It deliberately excludes configured pool floors: min_active_sessions is an
// operator request for resident capacity, not inferred work demand. Named,
// manual, and other non-pool sessions likewise retain their existing lifecycle.
//
// Query failures fail closed. Starting a paid model session from an incomplete
// read would make storage failure indistinguishable from useful demand.
func runPoolStartWorkQueryPreflight(
	ctx context.Context,
	candidate startCandidate,
	cfg *config.City,
	cityPath, cityName string,
	store beads.Store,
	now time.Time,
	runner ScaleCheckRunner,
	stderr io.Writer,
) poolStartPreflightResult {
	result := poolStartPreflightResult{AllowStart: true, Outcome: poolStartPreflightBypassed}
	if strings.TrimSpace(cityPath) == "" || cfg == nil ||
		!isPoolManagedSessionInfo(candidate.info) || isNamedSessionInfo(candidate.info) || candidate.tp.ManualSession {
		return result
	}

	agentCfg := findAgentByTemplate(cfg, candidate.logicalTemplate(cfg))
	if agentCfg == nil || isNonModelStartCandidate(candidate, agentCfg) ||
		!agentCfg.SupportsGenericEphemeralSessions() ||
		isConfiguredPoolFloorSession(candidate.info, candidate.logicalTemplate(cfg), cfg, store) {
		return result
	}
	// A restarting pool worker must re-adopt work already assigned to its
	// concrete session identity. The configured work_query remains the demand
	// gate for cold/unassigned capacity, but it may intentionally expose only
	// Ready-visible work and therefore omit an in-progress claim.
	identifiers := sessionAssignmentIdentifiersForConfigInfo(candidate.info, cfg)
	if hasAssigned, err := sessionHasOpenAssignedWorkInStoreByIdentifiers(store, identifiers); err == nil && hasAssigned {
		return result
	}
	if runner == nil {
		runner = shellScaleCheck
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if cityName == "" {
		cityName = config.EffectiveCityName(cfg, cityPath)
	}

	queryEnv, err := controllerWorkQueryEnv(cityPath, cfg, agentCfg)
	if err != nil {
		return finishPoolStartWorkQueryPreflight(ctx, result, candidate, cityPath, "", poolStartPreflightFailed, err, stderr)
	}
	runtimeInfo := candidate.info
	runtimeInfo.SessionName = candidate.name()
	identityEnv := sessionpkg.RuntimeEnvWithSessionContext(
		runtimeInfo,
		sessionpkg.DefaultGeneration,
		sessionpkg.DefaultContinuationEpoch,
		runtimeInfo.InstanceToken,
	)
	for key, value := range identityEnv {
		queryEnv[key] = value
	}
	// A pool instance's persisted template is authoritative. Repairable legacy
	// rows may omit it, so fall back to the configured template rather than
	// letting a stale inherited GC_TEMPLATE select another pool.
	if strings.TrimSpace(queryEnv["GC_TEMPLATE"]) == "" {
		queryEnv["GC_TEMPLATE"] = agentCfg.QualifiedName()
	}

	command := strings.TrimSpace(agentCfg.EffectiveWorkQueryFor(config.QueryTopology{Beads: cfg.Beads}))
	command = expandAgentCommandTemplate(cityPath, cityName, agentCfg, cfg.Rigs, "work_query", command, stderr)
	if command == "" {
		return result
	}

	result.Started = time.Now()
	out, err := runner(command, agentCommandDir(cityPath, agentCfg, cfg.Rigs), queryEnv)
	if err != nil {
		return finishPoolStartWorkQueryPreflight(ctx, result, candidate, cityPath, command, poolStartPreflightFailed, err, stderr)
	}
	normalized := normalizeWorkQueryOutput(strings.TrimSpace(out))
	normalized = filterUnreadyHookCandidates(normalized, now)
	if !workQueryHasReadyWork(normalized) {
		return finishPoolStartWorkQueryPreflight(ctx, result, candidate, cityPath, command, poolStartPreflightEmpty, nil, stderr)
	}
	result.AllowStart = true
	result.Outcome = poolStartPreflightWork
	result.Finished = time.Now()
	telemetry.RecordPoolStartPreflight(ctx, candidate.logicalTemplate(cfg), result.Outcome, result.Finished.Sub(result.Started), nil)
	return result
}

func isNonModelStartCandidate(candidate startCandidate, agentCfg *config.Agent) bool {
	if strings.EqualFold(strings.TrimSpace(agentCfg.PromptMode), "none") {
		return true
	}
	return candidate.tp.ResolvedProvider != nil &&
		strings.EqualFold(strings.TrimSpace(candidate.tp.ResolvedProvider.PromptMode), "none")
}

// isConfiguredPoolFloorSession reports whether info is one of the
// deterministic min_active_sessions residents. A failed lookup does not bypass
// the preflight: an unverified slot is safer to query than to start on a failed
// read.
func isConfiguredPoolFloorSession(info sessionpkg.Info, template string, cfg *config.City, store beads.Store) bool {
	infos, err := loadOpenSessionInfos(store)
	if err != nil {
		return false
	}
	byID := make(map[string]sessionpkg.Info, len(infos))
	for _, candidate := range infos {
		byID[candidate.ID] = candidate
	}
	return isMinFloorExemptIdleSession(byID, cfg, template, info.ID)
}

func finishPoolStartWorkQueryPreflight(
	ctx context.Context,
	result poolStartPreflightResult,
	candidate startCandidate,
	cityPath string,
	command string,
	outcome string,
	err error,
	stderr io.Writer,
) poolStartPreflightResult {
	result.AllowStart = false
	result.Outcome = outcome
	result.Err = err
	if result.Started.IsZero() {
		result.Started = time.Now()
	}
	result.Finished = time.Now()
	template := candidate.tp.TemplateName
	if template == "" {
		template = strings.TrimSpace(candidate.info.Template)
	}
	telemetry.RecordPoolStartPreflight(ctx, template, outcome, result.Finished.Sub(result.Started), err)
	if err != nil {
		emitCityWorkQueryFailure(cityPath, stderr, candidate.info.ID, template, command, err)
		fmt.Fprintf(stderr, "session reconciler: work_query preflight for %s failed closed: %v\n", candidate.name(), err) //nolint:errcheck
	}
	return result
}

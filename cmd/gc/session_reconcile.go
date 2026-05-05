// session_reconcile.go contains pure functions for the bead-driven session
// reconciler. Functions in this file assume single-threaded execution
// within one reconciler tick, with one intentional exception:
// computeWorkSet parallelizes its per-agent scale_check runner calls
// under a bounded semaphore (see bdProbeConcurrency in pool.go) so bd
// subprocess latency doesn't serialize the whole cycle. Any ScaleCheckRunner
// passed to computeWorkSet must therefore be safe to invoke from multiple
// goroutines concurrently — shellScaleCheck (the production implementation)
// is safe because it only reads its arguments and spawns an independent
// subprocess. Map mutations on beads.Bead.Metadata are visible to callers
// by design (maps are reference types).
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type wakeEvaluation struct {
	Reasons          []WakeReason
	Policy           resolvedSessionSleepPolicy
	ConfigSuppressed bool
}

// Deprecated: evaluateWakeReasons and wakeReasons are legacy functions
// superseded by ComputeAwakeSet (compute_awake_set.go). The production
// reconciler at session_reconciler.go:438 uses ComputeAwakeSet →
// awakeSetToWakeEvals for all wake/drain decisions. These functions are
// only called by computeWakeEvaluations (used as a nil-guard fallback
// in advanceSessionDrains, which never fires because the reconciler
// always passes non-nil wakeEvals) and by legacy tests.
//
// DO NOT add new wake logic here — it will have NO EFFECT on production
// behavior. All wake/sleep changes must go through ComputeAwakeSet.
//
// TODO: Remove these functions and migrate remaining tests to
// ComputeAwakeSet. Tracked as tech debt.

func wakeReasons(
	session beads.Bead,
	cfg *config.City,
	sp runtime.Provider,
	poolDesired map[string]int,
	workSet map[string]bool,
	readyWaitSet map[string]bool,
	clk clock.Clock,
) []WakeReason {
	return evaluateWakeReasons(session, cfg, sp, poolDesired, workSet, readyWaitSet, clk).Reasons
}

func evaluateWakeReasons(
	session beads.Bead,
	cfg *config.City,
	sp runtime.Provider,
	poolDesired map[string]int,
	workSet map[string]bool,
	readyWaitSet map[string]bool,
	clk clock.Clock,
) wakeEvaluation {
	policy := resolveSessionSleepPolicy(session, cfg, sp)

	// User hold suppresses all reasons.
	if held := session.Metadata["held_until"]; held != "" {
		if t, err := time.Parse(time.RFC3339, held); err == nil && clk.Now().Before(t) {
			return wakeEvaluation{Policy: policy}
		}
	}

	// Quarantine suppresses all reasons.
	if q := session.Metadata["quarantined_until"]; q != "" {
		if t, err := time.Parse(time.RFC3339, q); err == nil && clk.Now().Before(t) {
			return wakeEvaluation{Policy: policy}
		}
	}

	var reasons []WakeReason
	waitHold := session.Metadata["wait_hold"] != ""
	name := session.Metadata["session_name"]

	if readyWaitSet != nil && readyWaitSet[session.ID] {
		reasons = append(reasons, WakeWait)
	}
	if sessionStartRequested(session, clk) {
		reasons = append(reasons, WakeCreate)
	}

	template := normalizedSessionTemplate(session, cfg)
	agent, configEligible := sessionWithinDesiredConfig(session, cfg, poolDesired)
	if !waitHold && agent != nil && sessionMetadataState(session) == "active" && !policy.enabled() {
		reasons = append(reasons, WakeSession)
	}
	sleepSuppressed := configWakeSuppressed(session, policy, sp, clk)
	if configEligible {
		hasDemand := poolDesired[template] > 0
		isAlwaysNamed := isNamedSessionBead(session) && namedSessionMode(session) == "always"
		if !waitHold && (!sleepSuppressed || hasDemand || isAlwaysNamed) {
			reasons = append(reasons, WakeConfig)
		}
	}
	// WakeWork: the work_query reports pending work for this template.
	// This fires independently of poolDesired — if scale_check hasn't
	// caught up yet but work_query already sees routed beads, WakeWork
	// ensures the session wakes without waiting for the next tick.
	if !waitHold && workSet[template] {
		reasons = append(reasons, WakeWork)
	}
	if !waitHold && sessionKeepWarmEligible(session, policy, sp, clk) {
		reasons = append(reasons, WakeKeepWarm)
	}

	if !waitHold && sessionAttachedForWakeReason(sp, name) {
		reasons = append(reasons, WakeAttached)
	}

	if pendingInteractionReady(sp, name) {
		reasons = append(reasons, WakePending)
	}

	return wakeEvaluation{
		Reasons:          reasons,
		Policy:           policy,
		ConfigSuppressed: policy.enabled() && sleepSuppressed,
	}
}

func sessionWithinDesiredConfig(session beads.Bead, cfg *config.City, poolDesired map[string]int) (*config.Agent, bool) {
	template := normalizedSessionTemplate(session, cfg)
	agent := findAgentByTemplate(cfg, template)
	if agent == nil {
		return nil, false
	}
	if isDrainedSessionBead(session) {
		return agent, false
	}
	if session.Metadata["dependency_only"] == "true" {
		return agent, false
	}
	if isNamedSessionBead(session) {
		// Named sessions are config-eligible when they're "always" mode OR
		// when poolDesired > 0 (on_demand with active demand — e.g., work
		// assigned to their alias). buildDesiredState only adds on_demand
		// sessions when namedWorkReady is true.
		return agent, namedSessionMode(session) == "always" || poolDesired[template] > 0
	}
	if isManualSessionBead(session) {
		// Manual sessions on multi-session (implicit) agents are always
		// config-eligible — they were created by the user and should stay
		// alive until explicitly closed or idle-suspended.
		return agent, true
	}
	// Both pool and non-pool agents are config-eligible when demand exists.
	return agent, poolDesired[template] > 0
}

func sessionStartRequested(session beads.Bead, clk clock.Clock) bool {
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) == "true" {
		return true
	}
	if strings.TrimSpace(session.Metadata["state"]) != "creating" {
		return false
	}
	return !staleCreatingState(session, clk)
}

// staleCreatingStateTimeout bounds how long a state=creating bead may sit
// before generic creating metadata and corrupt start leases roll back. It is
// measured from the pending-create transition (see staleCreatingState below),
// not from the bead row's CreatedAt, so configured named-session reopens get a
// fresh window each time the bead is reopened. Pending creates that never
// reached preWakeCommit use pendingCreateNeverStartedTimeout instead.
const staleCreatingStateTimeout = time.Minute

func sessionMetadataState(session beads.Bead) string {
	switch state := strings.TrimSpace(session.Metadata["state"]); state {
	case "awake":
		return "active"
	case "drained":
		return "asleep"
	default:
		return state
	}
}

func computeWakeEvaluations(
	sessions []beads.Bead,
	cfg *config.City,
	sp runtime.Provider,
	poolDesired map[string]int,
	workSet map[string]bool,
	readyWaitSet map[string]bool,
	clk clock.Clock,
) map[string]wakeEvaluation {
	evals := make(map[string]wakeEvaluation, len(sessions))
	for _, session := range sessions {
		evals[session.ID] = evaluateWakeReasons(session, cfg, sp, poolDesired, workSet, readyWaitSet, clk)
	}
	applyDependencyWakeReasons(sessions, cfg, evals)
	capWakeConfigByDemand(sessions, cfg, evals, poolDesired)
	return evals
}

// capWakeConfigByDemand removes WakeConfig from excess sessions so that
// at most poolDesired[template] sessions get WakeConfig per template.
//
// Priority: sessions that are already alive or have resume-tier reasons
// (WakeSession, WakeAttached) keep their WakeConfig. Excess asleep
// sessions lose it. Sessions in creating/awake state that don't have
// assigned work count against the budget (they're "in-flight new"
// sessions that haven't claimed yet).
func capWakeConfigByDemand(sessions []beads.Bead, cfg *config.City, evals map[string]wakeEvaluation, poolDesired map[string]int) {
	// Group sessions by template and count how many already need to be awake.
	type templateBudget struct {
		desired int
		active  int      // creating/awake — already consuming a slot
		wakeIDs []string // sessions with WakeConfig that are asleep
	}
	budgets := make(map[string]*templateBudget)

	for _, session := range sessions {
		eval, ok := evals[session.ID]
		if !ok {
			continue
		}
		if !containsWakeReason(eval.Reasons, WakeConfig) {
			continue
		}
		// Named sessions with mode=always are not pool-managed — skip capping.
		if isNamedSessionBead(session) && namedSessionMode(session) == "always" {
			continue
		}
		// Manual sessions (user-created via API/UI) bypass pool demand — they
		// should stay alive until explicitly closed.
		if isManualSessionBead(session) {
			continue
		}
		template := normalizedSessionTemplate(session, cfg)
		if template == "" {
			continue
		}

		b := budgets[template]
		if b == nil {
			b = &templateBudget{desired: poolDesired[template]}
			budgets[template] = b
		}

		state := sessionMetadataState(session)
		switch state {
		case "active", "creating":
			// Already running or starting — counts against desired.
			b.active++
		default:
			// Asleep — candidate for wake, subject to budget.
			b.wakeIDs = append(b.wakeIDs, session.ID)
		}
	}

	// For each template, only allow enough asleep→wake transitions to
	// fill the gap between active and desired.
	for _, b := range budgets {
		slotsAvailable := b.desired - b.active
		if slotsAvailable < 0 {
			slotsAvailable = 0
		}
		// Keep the first slotsAvailable asleep sessions, strip WakeConfig from the rest.
		for i, id := range b.wakeIDs {
			if i >= slotsAvailable {
				eval := evals[id]
				eval.Reasons = removeWakeReason(eval.Reasons, WakeConfig)
				evals[id] = eval
			}
		}
	}
}

func removeWakeReason(reasons []WakeReason, remove WakeReason) []WakeReason {
	var result []WakeReason
	for _, r := range reasons {
		if r != remove {
			result = append(result, r)
		}
	}
	return result
}

func applyDependencyWakeReasons(sessions []beads.Bead, cfg *config.City, evals map[string]wakeEvaluation) {
	if cfg == nil || len(evals) == 0 {
		return
	}
	roots := make(map[string]bool)
	for _, session := range sessions {
		eval, ok := evals[session.ID]
		if !ok || !hasDependencyWakeRoot(eval.Reasons) {
			continue
		}
		template := normalizedSessionTemplate(session, cfg)
		if template != "" {
			roots[template] = true
		}
	}
	if len(roots) == 0 {
		return
	}
	preferred := preferredDependencySessions(sessions, cfg)
	visited := make(map[string]bool)
	var visit func(template string)
	visit = func(template string) {
		if template == "" || visited[template] {
			return
		}
		visited[template] = true
		agent := findAgentByTemplate(cfg, template)
		if agent == nil {
			return
		}
		for _, dep := range agent.DependsOn {
			if session, ok := preferred[dep]; ok {
				eval := evals[session.ID]
				if session.Metadata["held_until"] == "" && session.Metadata["quarantined_until"] == "" && !containsWakeReason(eval.Reasons, WakeDependency) {
					eval.Reasons = append(eval.Reasons, WakeDependency)
					evals[session.ID] = eval
				}
			}
			visit(dep)
		}
	}
	for template := range roots {
		visit(template)
	}
}

func preferredDependencySessions(sessions []beads.Bead, cfg *config.City) map[string]beads.Bead {
	preferred := make(map[string]beads.Bead)
	for _, session := range sessions {
		if isDrainedSessionBead(session) {
			continue
		}
		template := normalizedSessionTemplate(session, cfg)
		if template == "" {
			continue
		}
		existing, ok := preferred[template]
		if !ok || compareDependencyCandidate(session, existing) < 0 {
			preferred[template] = session
		}
	}
	return preferred
}

func compareDependencyCandidate(a, b beads.Bead) int {
	return strings.Compare(a.Metadata["session_name"], b.Metadata["session_name"])
}

func containsWakeReason(reasons []WakeReason, want WakeReason) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func hasDependencyWakeRoot(reasons []WakeReason) bool {
	return containsWakeReason(reasons, WakeConfig) ||
		containsWakeReason(reasons, WakeWork) ||
		containsWakeReason(reasons, WakeWait) ||
		containsWakeReason(reasons, WakeCreate) ||
		containsWakeReason(reasons, WakeSession) ||
		containsWakeReason(reasons, WakeAttached) ||
		containsWakeReason(reasons, WakePending) ||
		containsWakeReason(reasons, WakePin)
}

// computeWorkSet runs each agent's work_query command and returns the set
// of template names that have pending work. Called once per reconciler tick.
// Controller-side queries run from the canonical city/rig root so pack
// commands continue to operate on the real repo even when agent sessions use
// isolated work_dir sandboxes. Non-empty output means work exists. Agents
// without a work_query produce no WakeWork reason.
func computeWorkSet(cfg *config.City, runner ScaleCheckRunner, cityName, cityDir string, store beads.Store, sessionBeads *sessionBeadSnapshot, stderr io.Writer) map[string]bool { //nolint:unparam // cityName varies at runtime; tests use a fixed value
	if cfg == nil || runner == nil {
		return nil
	}
	// Collect the per-agent probe work first so the bd subprocess
	// calls can run concurrently. Each work_query shells out to `bd`,
	// which serializes on the shared dolt sql-server, so a sequential
	// loop over 40+ agents takes minutes per reconcile cycle. Bound
	// concurrency so overlapping probes don't stampede dolt.
	type probeWork struct {
		qn  string
		wq  string
		dir string
		env map[string]string
	}
	var probes []probeWork
	work := make(map[string]bool)
	seen := make(map[string]bool) // deduplicate pool instances
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		qn := a.QualifiedName()
		if seen[qn] {
			continue
		}
		seen[qn] = true
		probeEnv := controllerQueryRuntimeEnv(cityDir, cfg, a)
		wq := prefixedWorkQueryForProbeWithEnv(controllerQueryPrefixEnv(probeEnv), cfg, cityDir, cityName, store, sessionBeads, a, stderr)
		if wq == "" {
			continue
		}
		probes = append(probes, probeWork{qn: qn, wq: wq, dir: agentCommandDir(cityDir, a, cfg.Rigs), env: probeEnv})
	}

	sem := make(chan struct{}, cfg.Daemon.ProbeConcurrencyOrDefault())
	results := make([]bool, len(probes))
	var wg sync.WaitGroup
	for i := range probes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, err := runner(probes[idx].wq, probes[idx].dir, probes[idx].env)
			if err != nil {
				return // command failed — treat as no work
			}
			if workQueryHasReadyWork(strings.TrimSpace(out)) {
				results[idx] = true
			}
		}(i)
	}
	wg.Wait()

	for i, p := range probes {
		if results[i] {
			work[p.qn] = true
		}
	}
	return work
}

// findAgentByTemplate looks up a config agent by template name.
// Returns nil if not found.
func findAgentByTemplate(cfg *config.City, template string) *config.Agent {
	if cfg == nil || template == "" {
		return nil
	}
	for i := range cfg.Agents {
		if cfg.Agents[i].QualifiedName() == template {
			return &cfg.Agents[i]
		}
	}
	return nil
}

// healExpiredTimers clears expired held_until and quarantined_until.
// Separate from wakeReasons() to keep that function pure.
func healExpiredTimers(session *beads.Bead, store beads.Store, clk clock.Clock) {
	if h := session.Metadata["held_until"]; h != "" {
		if t, _ := time.Parse(time.RFC3339, h); !t.IsZero() && clk.Now().After(t) {
			batch := sessionpkg.ClearExpiredHoldPatch(session.Metadata["sleep_reason"])
			if err := store.SetMetadataBatch(session.ID, batch); err == nil {
				for k, v := range batch {
					session.Metadata[k] = v
				}
			}
		}
	}
	if q := session.Metadata["quarantined_until"]; q != "" {
		if t, _ := time.Parse(time.RFC3339, q); !t.IsZero() && clk.Now().After(t) {
			batch := sessionpkg.ClearExpiredQuarantinePatch(session.Metadata["sleep_reason"])
			if err := store.SetMetadataBatch(session.ID, batch); err == nil {
				for k, v := range batch {
					session.Metadata[k] = v
				}
			}
		}
	}
}

// checkStability detects dead sessions that still have last_woke_at. Provider
// rate-limit screens are retried until the hold metadata persists; ordinary
// crash wake failures are counted only inside stabilityThreshold.
//
// Production callers must run checkRateLimitStability before healState and
// pass nil here after healing. That ordering preserves continuation metadata
// for provider rate-limit screens while still letting crash recovery clear
// stale continuation identity after advisory state has been healed.
// Returns true if a stability event was recorded.
// Edge-triggered: clears last_woke_at after recording so the same crash
// is counted exactly once.
// Drain-aware: draining sessions died by request, not by crash.
func checkStability(session *beads.Bead, cfg *config.City, alive bool, dt *drainTracker, store beads.Store, clk clock.Clock, peek func(lines int) (string, error)) bool {
	if handled, err := checkRateLimitStability(session, cfg, alive, dt, store, clk, peek); handled || err != nil {
		return true
	}
	if !rapidExitWithinStabilityThreshold(session, cfg, alive, dt, clk) {
		return false
	}
	recordWakeFailure(session, store, clk)
	clearLastWokeAt(session, store)
	return true
}

func checkRateLimitStability(session *beads.Bead, cfg *config.City, alive bool, dt *drainTracker, store beads.Store, clk clock.Clock, peek func(lines int) (string, error)) (bool, error) {
	if !rateLimitStabilityCandidate(session, cfg, alive, dt, clk) {
		return false, nil
	}
	if peek == nil {
		return false, nil
	}
	content, err := peek(rateLimitPeekLines)
	if err != nil {
		return false, nil
	}
	if !runtime.ContainsProviderRateLimitScreen(content) {
		return false, nil
	}
	if err := recordRateLimitQuarantine(session, store, clk); err != nil {
		return false, err
	}
	return true, nil
}

func rateLimitStabilityCandidate(session *beads.Bead, cfg *config.City, alive bool, dt *drainTracker, clk clock.Clock) bool {
	if session == nil || alive {
		return false
	}
	if cfg != nil && cfg.Session.Provider == "subprocess" {
		return false
	}
	if dt != nil && dt.get(session.ID) != nil {
		return false
	}
	lastWoke := session.Metadata["last_woke_at"]
	if lastWoke == "" {
		return false
	}
	var startupTimeout time.Duration
	if cfg != nil {
		startupTimeout = cfg.Session.StartupTimeoutDuration()
	}
	if pendingCreateStartInFlight(*session, clk, startupTimeout) {
		return false
	}
	if _, err := time.Parse(time.RFC3339, lastWoke); err != nil {
		return false
	}
	return true
}

func rapidExitWithinStabilityThreshold(session *beads.Bead, cfg *config.City, alive bool, dt *drainTracker, clk clock.Clock) bool {
	if alive {
		return false
	}
	// Subprocess sessions are used for headless and deterministic workers that
	// intentionally exit after a unit of work. Treating those short-lived exits
	// as crashes quarantines valid one-shot workers like control-dispatcher.
	if cfg != nil && cfg.Session.Provider == "subprocess" {
		return false
	}
	// Don't count intentional drains as crashes.
	if dt != nil && dt.get(session.ID) != nil {
		return false
	}
	lastWoke := session.Metadata["last_woke_at"]
	if lastWoke == "" {
		return false
	}
	var startupTimeout time.Duration
	if cfg != nil {
		startupTimeout = cfg.Session.StartupTimeoutDuration()
	}
	if pendingCreateStartInFlight(*session, clk, startupTimeout) {
		return false
	}
	t, err := time.Parse(time.RFC3339, lastWoke)
	if err != nil {
		return false
	}
	return clk.Now().Sub(t) < stabilityThreshold
}

func clearLastWokeAt(session *beads.Bead, store beads.Store) {
	_ = store.SetMetadata(session.ID, "last_woke_at", "")
	session.Metadata["last_woke_at"] = ""
}

// recordRateLimitQuarantine backs off a session that exited into a provider
// rate-limit screen without treating the exit as a crash or resetting its
// conversation metadata.
func recordRateLimitQuarantine(session *beads.Bead, store beads.Store, clk clock.Clock) error {
	if session.Metadata == nil {
		session.Metadata = make(map[string]string)
	}
	qUntil := clk.Now().Add(defaultRateLimitQuarantineDuration).UTC().Format(time.RFC3339)
	batch := map[string]string{
		"state":                     string(sessionpkg.StateAsleep),
		"quarantined_until":         qUntil,
		"sleep_reason":              "rate_limit",
		"last_woke_at":              "",
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}
	if err := store.SetMetadataBatch(session.ID, batch); err != nil {
		fmt.Fprintf(os.Stderr, "recordRateLimitQuarantine: SetMetadataBatch %s: %v\n", session.ID, err) //nolint:errcheck
		return err
	}
	for k, v := range batch {
		session.Metadata[k] = v
	}
	return nil
}

// recordWakeFailure increments wake_attempts and quarantines if threshold exceeded.
func recordWakeFailure(session *beads.Bead, store beads.Store, clk clock.Clock) {
	attempts, _ := strconv.Atoi(session.Metadata["wake_attempts"])
	attempts++

	if session.Metadata == nil {
		session.Metadata = make(map[string]string)
	}
	// Clear session_key and started_config_hash so the next start gets a
	// fresh conversation. Clearing session_key triggers backfill of a new
	// UUID; clearing started_config_hash ensures resolveSessionCommand
	// treats the next wake as a first start (--session-id) rather than a
	// resume (--resume) of a conversation that no longer exists.
	//
	// checkStability runs after healState, and healState may already have
	// cleared session_key for an unexpected death before recordWakeFailure
	// runs. Clear started_config_hash whenever either field is set so the
	// recovery remains correct in that call order and for any skewed state
	// left behind by older builds.
	if session.Metadata["session_key"] != "" || session.Metadata["started_config_hash"] != "" {
		_ = store.SetMetadataBatch(session.ID, map[string]string{
			"session_key":                "",
			"started_config_hash":        "",
			"continuation_reset_pending": "true",
		})
		session.Metadata["session_key"] = ""
		session.Metadata["started_config_hash"] = ""
		session.Metadata["continuation_reset_pending"] = "true"
	}
	if attempts >= defaultMaxWakeAttempts {
		qUntil := clk.Now().Add(defaultQuarantineDuration).UTC().Format(time.RFC3339)
		batch := map[string]string{
			"wake_attempts":     strconv.Itoa(attempts),
			"quarantined_until": qUntil,
			"sleep_reason":      "quarantine",
		}
		if err := store.SetMetadataBatch(session.ID, batch); err == nil {
			for k, v := range batch {
				session.Metadata[k] = v
			}
		}
	} else {
		_ = store.SetMetadata(session.ID, "wake_attempts", strconv.Itoa(attempts))
		session.Metadata["wake_attempts"] = strconv.Itoa(attempts)
	}
}

// clearWakeFailures resets crash counter and quarantine for a stable session.
func clearWakeFailures(session *beads.Bead, store beads.Store) {
	batch := make(map[string]string, 2)
	if session.Metadata["wake_attempts"] != "" && session.Metadata["wake_attempts"] != "0" {
		batch["wake_attempts"] = "0"
	}
	if session.Metadata["quarantined_until"] != "" {
		batch["quarantined_until"] = ""
	}
	if len(batch) == 0 {
		return
	}
	if err := store.SetMetadataBatch(session.ID, batch); err == nil {
		if session.Metadata == nil {
			session.Metadata = make(map[string]string)
		}
		for k, v := range batch {
			session.Metadata[k] = v
		}
	}
}

// checkChurn detects repeated non-productive wake→die cycles (context
// exhaustion death spirals). Unlike checkStability which catches rapid
// crashes (< stabilityThreshold), this catches sessions that survive past
// the stability threshold but die before being productive.
//
// Returns true if a churn event was recorded (caller should skip further
// processing for this session).
func checkChurn(session *beads.Bead, cfg *config.City, alive bool, dt *drainTracker, store beads.Store, clk clock.Clock) bool {
	if alive {
		return false
	}
	// Subprocess sessions exit intentionally — not churn.
	if cfg != nil && cfg.Session.Provider == "subprocess" {
		return false
	}
	// Intentional drains are not churn.
	if dt != nil && dt.get(session.ID) != nil {
		return false
	}
	if isDeliberateSleepReason(session.Metadata["sleep_reason"]) {
		return false
	}
	lastWoke := session.Metadata["last_woke_at"]
	if lastWoke == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, lastWoke)
	if err != nil {
		return false
	}
	elapsed := clk.Now().Sub(t)
	// Only fires for sessions in the "churn band": survived past
	// stabilityThreshold (so checkStability didn't fire) but died
	// before churnProductivityThreshold (so not productive).
	if elapsed < stabilityThreshold {
		return false
	}
	if elapsed >= churnProductivityThreshold {
		// Session was productive — clear any stale churn count so it
		// doesn't carry over and cause premature quarantine next time.
		clearChurn(session, store)
		return false
	}

	recordChurn(session, store, clk)
	// Clear last_woke_at so this death is not re-counted next tick
	// (edge-triggered, same pattern as checkStability).
	_ = store.SetMetadata(session.ID, "last_woke_at", "")
	session.Metadata["last_woke_at"] = ""
	return true
}

func isDeliberateSleepReason(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "idle", "idle-timeout", "no-wake-reason", "config-drift", "drained",
		sleepReasonCityStop, "user-hold", "wait-hold", "rate_limit":
		return true
	default:
		return false
	}
}

// recordChurn increments the churn counter and clears session_key on
// every churn event to force a fresh conversation on next wake. When
// the counter reaches defaultMaxChurnCycles, the session is quarantined.
func recordChurn(session *beads.Bead, store beads.Store, clk clock.Clock) {
	count, _ := strconv.Atoi(session.Metadata["churn_count"])
	count++

	if session.Metadata == nil {
		session.Metadata = make(map[string]string)
	}

	// Always clear session_key on churn — context exhaustion means the
	// conversation itself is the problem. A fresh conversation avoids
	// re-hitting the same wall.
	clearBatch := map[string]string{
		"session_key":                "",
		"continuation_reset_pending": "true",
	}
	if session.Metadata["session_key"] != "" {
		_ = store.SetMetadataBatch(session.ID, clearBatch)
		for k, v := range clearBatch {
			session.Metadata[k] = v
		}
	}

	if count >= defaultMaxChurnCycles {
		qUntil := clk.Now().Add(defaultQuarantineDuration).UTC().Format(time.RFC3339)
		batch := map[string]string{
			"churn_count":       strconv.Itoa(count),
			"quarantined_until": qUntil,
			"sleep_reason":      "context-churn",
		}
		if err := store.SetMetadataBatch(session.ID, batch); err == nil {
			for k, v := range batch {
				session.Metadata[k] = v
			}
		}
		return
	}

	_ = store.SetMetadata(session.ID, "churn_count", strconv.Itoa(count))
	session.Metadata["churn_count"] = strconv.Itoa(count)
}

// clearChurn resets the churn counter for a productive session.
func clearChurn(session *beads.Bead, store beads.Store) {
	if session.Metadata["churn_count"] == "" || session.Metadata["churn_count"] == "0" {
		return
	}
	_ = store.SetMetadata(session.ID, "churn_count", "0")
	session.Metadata["churn_count"] = "0"
}

// productiveLongEnough returns true if the session has been alive past
// churnProductivityThreshold — long enough to have done useful work.
func productiveLongEnough(session beads.Bead, clk clock.Clock) bool {
	lastWoke := session.Metadata["last_woke_at"]
	if lastWoke == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, lastWoke)
	if err != nil {
		return false
	}
	return clk.Now().Sub(t) >= churnProductivityThreshold
}

// stableLongEnough returns true if the session has been alive past stabilityThreshold.
func stableLongEnough(session beads.Bead, clk clock.Clock) bool {
	lastWoke := session.Metadata["last_woke_at"]
	if lastWoke == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, lastWoke)
	if err != nil {
		return false
	}
	return clk.Now().Sub(t) >= stabilityThreshold
}

// sessionWakeAttempts returns the current wake attempt count.
func sessionWakeAttempts(session beads.Bead) int {
	n, _ := strconv.Atoi(session.Metadata["wake_attempts"])
	return n
}

// sessionIsQuarantined returns true if the session has an active quarantine.
func sessionIsQuarantined(session beads.Bead, clk clock.Clock) bool {
	q := session.Metadata["quarantined_until"]
	if q == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, q)
	if err != nil {
		return false
	}
	return clk.Now().Before(t)
}

// isPoolExcess returns true if this session is a pool instance whose slot
// exceeds the current desired count.
func isPoolExcess(session beads.Bead, cfg *config.City, poolDesired map[string]int) bool {
	template := normalizedSessionTemplate(session, cfg)
	agent := findAgentByTemplate(cfg, template)
	if agent == nil || !isEphemeralSessionBead(session) {
		return false
	}
	// A session is excess when demand is zero.
	return poolDesired[template] <= 0
}

// healState updates advisory state metadata only when changed (dirty check).
func healState(session *beads.Bead, alive bool, store beads.Store, clk clock.Clock) {
	if session == nil {
		return
	}
	batch := healStatePatch(*session, alive, clk)
	if len(batch) == 0 {
		return
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(batch))
	}
	if err := store.SetMetadataBatch(session.ID, batch); err != nil {
		fmt.Fprintf(os.Stderr, "healState: SetMetadataBatch %s: %v\n", session.ID, err) //nolint:errcheck
	}
	for k, v := range batch {
		session.Metadata[k] = v
	}
}

func healStatePatch(session beads.Bead, alive bool, clk clock.Clock) map[string]string {
	meta := session.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	var now time.Time
	var staleCreatingAfter time.Duration
	if clk != nil {
		now = clk.Now()
		staleCreatingAfter = staleCreatingStateTimeout
	}
	view := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInput{
		Status:             session.Status,
		Metadata:           meta,
		Runtime:            sessionpkg.RuntimeFacts{Observed: true, Alive: alive},
		CreatedAt:          session.CreatedAt,
		StaleCreatingAfter: staleCreatingAfter,
		Now:                now,
	})

	batch := make(map[string]string)
	if !alive && view.BaseState == sessionpkg.BaseStateDrained {
		if strings.TrimSpace(meta["state"]) != string(sessionpkg.StateAsleep) {
			batch["state"] = string(sessionpkg.StateAsleep)
		}
		if strings.TrimSpace(meta["sleep_reason"]) == "" {
			batch["sleep_reason"] = "drained"
		}
		return emptyNil(batch)
	}

	target := string(view.ReconciledState)
	if target == "" && view.BaseState == sessionpkg.BaseStateNone {
		target = string(sessionpkg.StateAsleep)
		if alive {
			target = string(sessionpkg.StateAwake)
		} else if sessionStartRequested(session, clk) {
			target = string(sessionpkg.StateCreating)
		}
	}
	if target == "" {
		return nil
	}
	if meta["state"] != target {
		batch["state"] = target
	}
	if target == string(sessionpkg.StateAsleep) && view.ResetContinuation {
		batch["session_key"] = ""
		batch["started_config_hash"] = ""
		batch["continuation_reset_pending"] = "true"
	}
	return emptyNil(batch)
}

func emptyNil(batch map[string]string) map[string]string {
	if len(batch) == 0 {
		return nil
	}
	return batch
}

// staleCreatingState returns true when a state=creating bead has been
// stuck in that state longer than staleCreatingStateTimeout.
//
// "How long" is measured from the most recent transition into the
// creating/pending-create state, NOT from the bead's original
// CreatedAt. Configured-named-session beads (e.g. beads/planner) get
// REOPENED on demand — the same bead row toggles closed→open with
// state→creating — so its CreatedAt is from when the bead row was
// first created (potentially hours/days/months ago) and is irrelevant
// to whether the current spawn attempt is stuck.
//
// Order of preference:
//  1. metadata["pending_create_started_at"] — set by createPoolSessionBead
//     and reopenClosedConfiguredNamedSessionBead at the moment the bead
//     enters state=creating with pending_create_claim=true.
//  2. session.CreatedAt — fallback for fresh pool beads minted before
//     this metadata key was introduced, and for any caller that creates
//     a bead in state=creating without going through the helpers above.
func staleCreatingState(session beads.Bead, clk clock.Clock) bool {
	if clk == nil {
		return false
	}
	if strings.TrimSpace(session.Metadata["state"]) != string(sessionpkg.StateCreating) {
		return false
	}
	now := clk.Now()
	if started, ok := parseRFC3339Metadata(session.Metadata["pending_create_started_at"]); ok {
		return !now.Before(started.Add(staleCreatingStateTimeout))
	}
	if session.CreatedAt.IsZero() {
		return true
	}
	return !now.Before(session.CreatedAt.Add(staleCreatingStateTimeout))
}

// pendingCreateStartedAtNow returns the timestamp string to write into
// metadata["pending_create_started_at"] when a bead transitions into
// state=creating with pending_create_claim=true. Must match the format
// staleCreatingState parses (RFC3339).
func pendingCreateStartedAtNow(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	return now.UTC().Format(time.RFC3339)
}

// topoOrder returns session beads in dependency order (dependencies first).
// deps maps template name -> list of dependency template names.
// If a cycle is detected (should not happen — validated at config load),
// falls back to original order.
func topoOrder(sessions []beads.Bead, deps map[string][]string) []beads.Bead {
	if len(deps) == 0 {
		return sessions
	}

	// Build template -> sessions index.
	templateSessions := make(map[string][]beads.Bead)
	for _, s := range sessions {
		template := s.Metadata["template"]
		templateSessions[template] = append(templateSessions[template], s)
	}

	// Collect unique templates present in sessions.
	var templates []string
	seen := make(map[string]bool)
	for _, s := range sessions {
		t := s.Metadata["template"]
		if !seen[t] {
			seen[t] = true
			templates = append(templates, t)
		}
	}

	// Topological sort via DFS with cycle detection.
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(templates))
	var order []string
	hasCycle := false

	var visit func(t string)
	visit = func(t string) {
		if hasCycle {
			return
		}
		color[t] = gray
		for _, dep := range deps[t] {
			switch color[dep] {
			case gray:
				hasCycle = true
				return
			case white:
				if seen[dep] { // only visit templates present in sessions
					visit(dep)
				}
			}
		}
		color[t] = black
		order = append(order, t)
	}

	for _, t := range templates {
		if color[t] == white {
			visit(t)
		}
	}

	if hasCycle {
		return sessions // fallback: unordered
	}

	// order is in reverse-finish order (dependencies come first).
	var result []beads.Bead
	for _, t := range order {
		result = append(result, templateSessions[t]...)
	}
	return result
}

// knownSessionStates is the set of bead metadata "state" values that the
// current reconciler understands. Beads with unrecognized states are skipped
// during reconciliation to allow forward-compatible rollback from newer
// versions that add states like "draining" or "archived".
var knownSessionStates = map[string]bool{
	"active":      true,
	"asleep":      true,
	"awake":       true,
	"stopped":     true,
	"suspended":   true,
	"orphaned":    true,
	"closed":      true,
	"quarantined": true,
	"creating":    true,
	"drained":     true,
	"":            true, // empty state is valid (legacy beads)
}

// isKnownState returns true if the bead's metadata state is recognized by
// the current reconciler. Unknown states (from a newer version) are skipped
// to prevent panics during rollback.
func isKnownState(session beads.Bead) bool {
	return knownSessionStates[session.Metadata["state"]]
}

// reverseBeads returns a reversed copy of the bead slice.
func reverseBeads(beadSlice []beads.Bead) []beads.Bead {
	n := len(beadSlice)
	result := make([]beads.Bead, n)
	for i, b := range beadSlice {
		result[n-1-i] = b
	}
	return result
}

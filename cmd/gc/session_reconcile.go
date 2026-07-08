// session_reconcile.go contains pure functions for the bead-driven session
// reconciler. Functions in this file assume single-threaded execution
// within one reconciler tick, with one intentional exception:
// computeWorkSet is the legacy controller-side work_query helper. It
// parallelizes runner calls under a bounded semaphore (see bdProbeConcurrency
// in pool.go), so any ScaleCheckRunner passed to it must be safe to invoke
// from multiple goroutines concurrently. shellScaleCheck is safe because it
// only reads its arguments and spawns an independent subprocess. Map mutations
// on beads.Bead.Metadata are visible to callers by design (maps are reference
// types).
package main

import (
	"context"
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
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

type wakeEvaluation struct {
	Reasons []WakeReason
	// Reason mirrors AwakeDecision.Reason on the ComputeAwakeSet bridge path.
	// It is only actionable when Reasons contains the matching effective wake.
	Reason           string
	Policy           resolvedSessionSleepPolicy
	ConfigSuppressed bool
	HasAssignedWork  bool
}

const (
	sessionHealthStateMetadataKey           = "session_health"
	sessionHealthReasonMetadataKey          = "session_health_reason"
	sessionDrainableMetadataKey             = "session_drainable"
	sessionProviderTerminalErrorMetadataKey = "provider_terminal_error"
	sessionProviderTerminalErrorAtKey       = "provider_terminal_error_at"
)

// wakeReasons and evaluateWakeReasons are the CLI `gc session` REASON-column
// display helpers ONLY. They compute the multi-reason, comma-joined cell shown
// to operators; their sole production caller is sessionReason in cmd_session.go.
// Production wake/sleep decisions come exclusively from ComputeAwakeSet
// (compute_awake_set.go) via awakeSetToWakeEvals — do NOT add wake logic here,
// it has no effect on reconciler behavior.

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
	if strings.TrimSpace(session.Metadata["state"]) == string(sessionpkg.StateStartPending) {
		return true
	}
	if strings.TrimSpace(session.Metadata["pending_create_claim"]) == "true" {
		return true
	}
	if strings.TrimSpace(session.Metadata["state"]) != "creating" {
		return false
	}
	return !staleCreatingState(session, clk)
}

// sessionStartRequestedInfo is the session.Info sibling of sessionStartRequested.
// Equivalence-proven. It reads the RAW metadata state (Info.MetadataState) and the
// projected pending-create claim flag (Info.PendingCreateClaim, which the codec
// derives as strings.TrimSpace(pending_create_claim) == "true" — identical to the
// raw read), and keeps the literal "creating" state compare the original uses.
func sessionStartRequestedInfo(i sessionpkg.Info, clk clock.Clock) bool {
	if strings.TrimSpace(i.MetadataState) == string(sessionpkg.StateStartPending) {
		return true
	}
	if i.PendingCreateClaim {
		return true
	}
	if strings.TrimSpace(i.MetadataState) != "creating" {
		return false
	}
	return !staleCreatingStateInfo(i, clk)
}

// staleCreatingStateTimeout bounds how long a state=creating bead may sit
// before generic creating metadata and corrupt start leases roll back. It is
// measured from the pending-create transition (see staleCreatingState below),
// not from the bead row's CreatedAt, so configured named-session reopens get a
// fresh window each time the bead is reopened. Pending creates that never
// reached preWakeCommit use pendingCreateNeverStartedTimeout instead.
const staleCreatingStateTimeout = time.Minute

// stalePendingCreateTimeout is the longer grace window applied by
// reapStaleSessionBeads to a started pending-create bead — one that holds
// pending_create_claim=true AND has a last_woke_at (it reached preWakeCommit).
// Such a bead may legitimately be mid-start or mid-rollback, so it is given
// more time than a plain creating bead before being reaped. Without an upper
// bound, a bead whose rollback never completes (e.g. a transient store error
// on closeBead) would stay open as a phantom forever — the leak tracked by
// gc-5tyf5. Never-started pending creates (no last_woke_at) instead defer to
// pendingCreateNeverStartedTimeout, so a slow provider.Start() is not reaped
// out from under the reconciler's still-active never-started lease.
const stalePendingCreateTimeout = 5 * time.Minute

func sessionMetadataState(session beads.Bead) string {
	switch state := strings.TrimSpace(session.Metadata["state"]); state {
	case "awake":
		return "active"
	case string(sessionpkg.StateStartPending):
		return "creating"
	case "drained":
		return "asleep"
	default:
		return state
	}
}

// sessionMetadataStateInfo is the session.Info mirror of sessionMetadataState. It
// reads the RAW metadata state (Info.MetadataState), not the normalized Info.State.
func sessionMetadataStateInfo(i sessionpkg.Info) string {
	switch state := strings.TrimSpace(i.MetadataState); state {
	case "awake":
		return "active"
	case string(sessionpkg.StateStartPending):
		return "creating"
	case "drained":
		return "asleep"
	default:
		return state
	}
}

func containsWakeReason(reasons []WakeReason, want WakeReason) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

// computeWorkSet runs legacy controller-side work_query commands and returns
// the set of template names that have pending work. The current CityRuntime
// demand snapshot keeps WorkSet empty and uses assigned-work scans plus
// scale_check for controller wake demand; keep this helper suspension-aware
// until the legacy WakeWork fallback is removed. Controller-side queries run
// from the canonical city/rig root so pack commands continue to operate on the
// real repo even when agent sessions use isolated work_dir sandboxes. Non-empty
// output means work exists. Agents without a work_query produce no WakeWork
// reason.
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
	// Load runtime suspension state once against the in-scope city
	// directory so the per-agent checks resolve suspension against the
	// controlled city rather than the process cwd.
	suspState, _ := loadSuspensionState(fsys.OSFS{}, cityDir)
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		qn := a.QualifiedName()
		if seen[qn] {
			continue
		}
		seen[qn] = true
		if isAgentEffectivelySuspendedWith(cfg, a, suspState) {
			continue
		}
		probeEnv, err := controllerQueryRuntimeEnv(cityDir, cfg, a)
		if err != nil {
			fmt.Fprintf(stderr, "session reconcile: building probe env for %s: %v\n", qn, err) //nolint:errcheck
			continue
		}
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

// findAgentByTemplate looks up a config agent by template name. Exact
// identity matches (canonical qualified name or V1 dir+name form, via
// config.AgentMatchesIdentity) win over all fallbacks; when nothing matches
// exactly, a legacy bound form ("dir/binding.name") resolves to the unbound
// agent "dir/name" so sessions and work persisted before a bound→unbound
// migration stay attributed. Callers that need strict exact-match lookup
// (e.g. uniqueness validation) must not use this resolver.
// Returns nil if not found.
func findAgentByTemplate(cfg *config.City, template string) *config.Agent {
	template = strings.TrimSpace(template)
	if cfg == nil || template == "" {
		return nil
	}
	for i := range cfg.Agents {
		if config.AgentMatchesIdentity(&cfg.Agents[i], template) {
			return &cfg.Agents[i]
		}
	}
	for i := range cfg.Agents {
		if legacyBoundTemplateMatchesUnboundAgent(&cfg.Agents[i], template) {
			return &cfg.Agents[i]
		}
	}
	return nil
}

func legacyBoundTemplateMatchesUnboundAgent(agent *config.Agent, template string) bool {
	if agent == nil || strings.TrimSpace(agent.BindingName) != "" {
		return false
	}
	dir, local := config.ParseQualifiedName(strings.TrimSpace(template))
	if strings.TrimSpace(dir) != strings.TrimSpace(agent.Dir) {
		return false
	}
	binding, unbound, ok := strings.Cut(local, ".")
	if !ok || strings.TrimSpace(binding) == "" {
		return false
	}
	return strings.TrimSpace(unbound) == strings.TrimSpace(agent.Name)
}

// normalizeAgentTemplateIdentity maps a persisted template identity to the
// matching agent's current canonical qualified name. It resolves through
// findAgentByTemplate, so a legacy bound form ("dir/binding.name") left by a
// bound→unbound migration normalizes to the unbound agent's canonical name.
// Identities that resolve to no configured agent pass through unchanged.
func normalizeAgentTemplateIdentity(cfg *config.City, template string) string {
	template = strings.TrimSpace(template)
	if template == "" {
		return ""
	}
	if agent := findAgentByTemplate(cfg, template); agent != nil {
		return agent.QualifiedName()
	}
	return template
}

// agentTemplateIdentitiesEquivalent reports whether two template identities
// name the same configured agent after normalization. Distinct configured
// agents stay distinct: each exact identity normalizes to itself, so a bound
// agent and a same-named unbound agent never merge while both exist.
func agentTemplateIdentitiesEquivalent(cfg *config.City, a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return normalizeAgentTemplateIdentity(cfg, a) == normalizeAgentTemplateIdentity(cfg, b)
}

// healExpiredTimers clears expired held_until and quarantined_until.
// Separate from wakeReasons() to keep that function pure.
func healExpiredTimers(session *beads.Bead, sessFront *sessionpkg.Store, clk clock.Clock) {
	if h := session.Metadata["held_until"]; h != "" {
		if t, _ := time.Parse(time.RFC3339, h); !t.IsZero() && clk.Now().After(t) {
			batch := sessionpkg.ClearExpiredHoldPatch(session.Metadata["sleep_reason"])
			if err := sessFront.ApplyPatch(session.ID, batch); err == nil {
				for k, v := range batch {
					session.Metadata[k] = v
				}
			}
		}
	}
	if q := session.Metadata["quarantined_until"]; q != "" {
		if t, _ := time.Parse(time.RFC3339, q); !t.IsZero() && clk.Now().After(t) {
			batch := sessionpkg.ClearExpiredQuarantinePatch(session.Metadata["sleep_reason"])
			if err := sessFront.ApplyPatch(session.ID, batch); err == nil {
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
// Edge-triggered: clears last_woke_at after recording so the same crash
// is counted exactly once. Drain-aware: draining sessions died by request,
// not by crash.
//
// Returns (true, batch) when a stability event was recorded, where batch is the
// union of every patch mirrored onto session.Metadata on that path so the
// forward-pass caller can fold it via ApplyPatch (front-door migration Step 6d,
// STEP6-PREPASS-AUDIT group 2). Returns (false, nil) otherwise; ApplyPatch(nil)
// is a no-op.
func checkStability(session *beads.Bead, cfg *config.City, alive bool, dt *drainTracker, sessFront *sessionpkg.Store, clk clock.Clock, peek func(lines int) (string, error)) (bool, map[string]string) {
	if handled, rlBatch, err := checkRateLimitStability(session, cfg, alive, dt, sessFront, clk, peek); handled || err != nil {
		return true, rlBatch
	}
	if sessionpkg.DecideSessionExit(sessionExitFacts(session, cfg, alive, dt, clk)) != sessionpkg.ExitRapidCrash {
		return false, nil
	}
	wfBatch := recordWakeFailure(session, sessFront, clk, sessionAgentMetricIdentity(*session, cfg))
	clearBatch := clearLastWokeAt(session, sessFront)
	return true, mergeMetadataPatch(wfBatch, clearBatch)
}

// checkRateLimitStability runs the provider-screen lane of the
// exit-classification decider for a dead crash candidate. It peeks the
// session's provider screen and records, without counting an ordinary crash:
//
//   - a terminal (non-retryable) provider error → mark the session
//     unhealthy + drainable so pool sizing excludes its slot. Terminal errors
//     take precedence over the rate-limit screen: they are non-retryable.
//   - otherwise a rate-limit screen → quarantine with a back-off and a
//     distinct sleep_reason, so the session is retried rather than crashed.
//
// Returns (handled, err, batch): handled=true when either was recorded, err
// when the write failed, and batch holding the mirrored patch on the hit path
// so the forward-pass caller can fold it onto the typed snapshot via
// ApplyPatch (front-door migration Step 6d, STEP6-PREPASS-AUDIT group 1).
// batch is nil on every path that mirrors nothing (no-hit, nil session, or
// persist error); ApplyPatch(nil) is a no-op.
func checkRateLimitStability(session *beads.Bead, cfg *config.City, alive bool, dt *drainTracker, sessFront *sessionpkg.Store, clk clock.Clock, peek func(lines int) (string, error)) (bool, map[string]string, error) {
	if session == nil {
		return false, nil, nil
	}
	facts := sessionExitFacts(session, cfg, alive, dt, clk)
	facts.ScreenAvailable = peek != nil
	dec := sessionpkg.DecideSessionExit(facts)
	for dec == sessionpkg.ExitGatherScreen {
		facts.Screen = sessionpkg.ScreenOther
		if content, err := peek(rateLimitPeekLines); err == nil {
			if reason := runtime.ProviderTerminalErrorReason(content); reason != "" {
				termBatch, markErr := markProviderTerminalError(session, sessFront, clk, reason)
				if markErr != nil {
					return false, nil, markErr
				}
				return true, termBatch, nil
			}
			if runtime.ContainsProviderRateLimitScreen(content) {
				facts.Screen = sessionpkg.ScreenRateLimit
			}
		}
		dec = sessionpkg.DecideSessionExit(facts)
	}
	if dec != sessionpkg.ExitRateLimitQuarantine {
		return false, nil, nil
	}
	rlBatch, err := recordRateLimitQuarantine(session, sessFront, clk)
	if err != nil {
		return false, nil, err
	}
	return true, rlBatch, nil
}

// sessionExitFacts gathers the cheap facts for the exit-classification
// decider. The provider-screen fact is gathered on demand by checkStability
// when the decider asks for it.
func sessionExitFacts(session *beads.Bead, cfg *config.City, alive bool, dt *drainTracker, clk clock.Clock) sessionpkg.ExitFacts {
	var startupTimeout time.Duration
	subprocess := false
	if cfg != nil {
		startupTimeout = cfg.Session.StartupTimeoutDuration()
		subprocess = cfg.Session.Provider == "subprocess"
	}
	return sessionpkg.ExitFacts{
		Alive:                      alive,
		SubprocessProvider:         subprocess,
		DrainPending:               dt != nil && dt.get(session.ID) != nil,
		PendingCreateClaim:         strings.TrimSpace(session.Metadata["pending_create_claim"]) == "true",
		PendingCreateStartInFlight: pendingCreateStartInFlight(*session, clk, startupTimeout),
		SleepReason:                session.Metadata["sleep_reason"],
		LastWokeAt:                 session.Metadata["last_woke_at"],
		Now:                        clk.Now(),
		StabilityThreshold:         stabilityThreshold,
		ProductivityThreshold:      churnProductivityThreshold,
	}
}

// clearLastWokeAt clears last_woke_at on the session bead and returns the
// mirrored batch {"last_woke_at": ""} so the caller can fold it onto the typed
// snapshot via ApplyPatch (front-door migration Step 6d).
func clearLastWokeAt(session *beads.Bead, sessFront *sessionpkg.Store) map[string]string {
	_ = sessFront.SetMarker(session.ID, "last_woke_at", "")
	session.Metadata["last_woke_at"] = ""
	return map[string]string{"last_woke_at": ""}
}

// recordRateLimitQuarantine backs off a session that exited into a provider
// rate-limit screen without treating the exit as a crash or resetting its
// conversation metadata. Returns (batch, nil) on success so the caller can
// fold the mirrored patch onto the typed snapshot via ApplyPatch (front-door
// migration Step 6d); returns (nil, err) on persist failure.
func recordRateLimitQuarantine(session *beads.Bead, sessFront *sessionpkg.Store, clk clock.Clock) (map[string]string, error) {
	if session.Metadata == nil {
		session.Metadata = make(map[string]string)
	}
	batch := sessionpkg.RateLimitQuarantinePatch(clk.Now().Add(defaultRateLimitQuarantineDuration))
	if err := sessFront.ApplyPatch(session.ID, batch); err != nil {
		fmt.Fprintf(os.Stderr, "recordRateLimitQuarantine: SetMetadataBatch %s: %v\n", session.ID, err) //nolint:errcheck
		return nil, err
	}
	for k, v := range batch {
		session.Metadata[k] = v
	}
	return batch, nil
}

// markProviderTerminalError records the terminal-provider-error health/sleep
// metadata on a zombie session bead. It returns the batch it mirrored onto
// session.Metadata (so the reconciler can fold it onto the typed Info snapshot
// via write-returns-Info, front-door migration Step 6d) and any persist error.
// The returned batch is nil on every path that mirrors nothing — a nil/empty
// argument, an empty reason, or a persist failure (the mirror below runs only
// after a successful ApplyPatch) — so ApplyPatch(returnedBatch) is a no-op
// exactly when the raw bead was left unchanged.
func markProviderTerminalError(session *beads.Bead, sessFront *sessionpkg.Store, clk clock.Clock, reason string) (map[string]string, error) {
	if session == nil || sessFront == nil {
		return nil, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, nil
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string)
	}
	now := time.Now().UTC()
	if clk != nil {
		now = clk.Now().UTC()
	}
	batch := map[string]string{
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
	if err := sessFront.ApplyPatch(session.ID, batch); err != nil {
		return nil, err
	}
	for k, v := range batch {
		session.Metadata[k] = v
	}
	return batch, nil
}

func sessionHasProviderTerminalError(session beads.Bead) bool {
	if strings.TrimSpace(session.Metadata[sessionProviderTerminalErrorMetadataKey]) != "" {
		return true
	}
	return strings.TrimSpace(session.Metadata[sessionHealthStateMetadataKey]) == "unhealthy" &&
		strings.TrimSpace(session.Metadata[sessionDrainableMetadataKey]) == boolMetadata(true) &&
		strings.TrimSpace(session.Metadata[sessionHealthReasonMetadataKey]) != ""
}

// sessionHasProviderTerminalErrorInfo is the session.Info sibling of
// sessionHasProviderTerminalError, reading the typed health/terminal-error
// mirrors instead of raw bead metadata. Equivalence-proven.
func sessionHasProviderTerminalErrorInfo(info sessionpkg.Info) bool {
	if strings.TrimSpace(info.ProviderTerminalError) != "" {
		return true
	}
	return strings.TrimSpace(info.HealthState) == "unhealthy" &&
		info.Drainable &&
		strings.TrimSpace(info.HealthReason) != ""
}

// recordWakeFailure increments wake_attempts and quarantines if threshold
// exceeded. Returns the merged batch of everything mirrored onto
// session.Metadata so the caller can fold it onto the typed snapshot via
// ApplyPatch (front-door migration Step 6d). The batch includes:
//   - the ConversationResetPatch if session_key or started_config_hash was set
//   - the WakeFailureAccrualPatch (quarantine or single-counter increment)
//
// Returns nil only when no keys were mirrored (a quarantined accrual whose
// persist failed is excluded from the batch). agentIdentity is the
// start-path-joinable agent label for gc.agent.quarantines.total.
func recordWakeFailure(session *beads.Bead, sessFront *sessionpkg.Store, clk clock.Clock, agentIdentity string) map[string]string {
	attempts, _ := strconv.Atoi(session.Metadata["wake_attempts"])

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
	var merged map[string]string
	if session.Metadata["session_key"] != "" || session.Metadata["started_config_hash"] != "" {
		reset := sessionpkg.ConversationResetPatch(true)
		_ = sessFront.ApplyPatch(session.ID, reset)
		for k, v := range reset {
			session.Metadata[k] = v
		}
		merged = mergeMetadataPatch(merged, reset)
	}
	accrual := sessionpkg.WakeFailureAccrualPatch(attempts, defaultMaxWakeAttempts, clk.Now().Add(defaultQuarantineDuration))
	if accrual.Quarantined {
		if err := sessFront.ApplyPatch(session.ID, accrual.Patch); err == nil {
			for k, v := range accrual.Patch {
				session.Metadata[k] = v
			}
			telemetry.RecordAgentQuarantine(context.Background(), agentIdentity)
			merged = mergeMetadataPatch(merged, accrual.Patch)
		}
	} else {
		next := accrual.Patch["wake_attempts"]
		_ = sessFront.SetMarker(session.ID, "wake_attempts", next)
		session.Metadata["wake_attempts"] = next
		merged = mergeMetadataPatch(merged, map[string]string{"wake_attempts": next})
	}
	return merged
}

// clearWakeFailures resets crash counter and quarantine for a stable session.
// Returns the mirrored batch on the persist path, nil when there is nothing to
// clear (both fields already absent/zero). The caller folds the returned batch
// onto the typed snapshot via ApplyPatch (nil is a no-op).
func clearWakeFailures(session *beads.Bead, sessFront *sessionpkg.Store) map[string]string {
	batch := make(map[string]string, 2)
	if session.Metadata["wake_attempts"] != "" && session.Metadata["wake_attempts"] != "0" {
		batch["wake_attempts"] = "0"
	}
	if session.Metadata["quarantined_until"] != "" {
		batch["quarantined_until"] = ""
	}
	if len(batch) == 0 {
		return nil
	}
	if err := sessFront.ApplyPatch(session.ID, batch); err == nil {
		if session.Metadata == nil {
			session.Metadata = make(map[string]string)
		}
		for k, v := range batch {
			session.Metadata[k] = v
		}
		return batch
	}
	return nil
}

// checkChurn detects repeated non-productive wake→die cycles (context
// exhaustion death spirals). Unlike checkStability which catches rapid
// crashes (< stabilityThreshold), this catches sessions that survive past
// the stability threshold but die before being productive.
//
// Returns (churned, batch): churned=true if a churn event was recorded
// (caller should skip further processing for this session), and batch is the
// union of all patches mirrored onto session.Metadata on either exit path,
// so the caller can fold it via ApplyPatch regardless of the bool return
// (front-door migration Step 6d, STEP6-PREPASS-AUDIT group 5).
// batch is nil when nothing was mirrored. ApplyPatch(nil) is a no-op.
func checkChurn(session *beads.Bead, cfg *config.City, alive bool, dt *drainTracker, sessFront *sessionpkg.Store, clk clock.Clock) (bool, map[string]string) {
	switch sessionpkg.DecideSessionExit(sessionExitFacts(session, cfg, alive, dt, clk)) {
	case sessionpkg.ExitChurn:
		churnBatch := recordChurn(session, sessFront, clk, sessionAgentMetricIdentity(*session, cfg))
		// Clear last_woke_at so this death is not re-counted next tick
		// (edge-triggered, same pattern as checkStability).
		clearBatch := clearLastWokeAt(session, sessFront)
		return true, mergeMetadataPatch(churnBatch, clearBatch)
	case sessionpkg.ExitProductiveDeath:
		// Session was productive — clear any stale churn count so it
		// doesn't carry over and cause premature quarantine next time.
		return false, clearChurn(session, sessFront)
	default:
		// Rapid crashes belong to checkStability, which ran first.
		return false, nil
	}
}

func isDeliberateSleepReason(reason string) bool {
	return sessionpkg.IsDeliberateSleepReason(reason)
}

// recordChurn increments the churn counter and clears session_key on every
// churn event to force a fresh conversation on next wake. When the counter
// reaches defaultMaxChurnCycles, the session is quarantined.
//
// Returns the merged batch of everything mirrored onto session.Metadata so
// the caller can fold it onto the typed snapshot via ApplyPatch (front-door
// migration Step 6d). The batch includes:
//   - the ConversationResetPatch (session_key/continuation_reset_pending) if
//     session_key was set
//   - the ChurnAccrualPatch (churn_count, and quarantined_until/sleep_reason
//     when quarantined) when the quarantined persist succeeded
//   - {"churn_count": next} on the non-quarantine path
//
// agentIdentity is the start-path-joinable agent label for gc.agent.quarantines.total.
func recordChurn(session *beads.Bead, sessFront *sessionpkg.Store, clk clock.Clock, agentIdentity string) map[string]string {
	count, _ := strconv.Atoi(session.Metadata["churn_count"])

	if session.Metadata == nil {
		session.Metadata = make(map[string]string)
	}

	// Always clear session_key on churn — context exhaustion means the
	// conversation itself is the problem. A fresh conversation avoids
	// re-hitting the same wall.
	var merged map[string]string
	if session.Metadata["session_key"] != "" {
		reset := sessionpkg.ConversationResetPatch(false)
		_ = sessFront.ApplyPatch(session.ID, reset)
		for k, v := range reset {
			session.Metadata[k] = v
		}
		merged = mergeMetadataPatch(merged, reset)
	}

	accrual := sessionpkg.ChurnAccrualPatch(count, defaultMaxChurnCycles, clk.Now().Add(defaultQuarantineDuration))
	if accrual.Quarantined {
		if err := sessFront.ApplyPatch(session.ID, accrual.Patch); err == nil {
			for k, v := range accrual.Patch {
				session.Metadata[k] = v
			}
			telemetry.RecordAgentQuarantine(context.Background(), agentIdentity)
			merged = mergeMetadataPatch(merged, accrual.Patch)
		}
		return merged
	}

	next := accrual.Patch["churn_count"]
	_ = sessFront.SetMarker(session.ID, "churn_count", next)
	session.Metadata["churn_count"] = next
	return mergeMetadataPatch(merged, map[string]string{"churn_count": next})
}

// clearChurn resets the churn counter for a productive session.
// Returns the mirrored batch {"churn_count":"0"} when a clear is persisted, nil
// when churn_count is already absent or zero (no-op). The caller folds the
// returned batch onto the typed snapshot via ApplyPatch (nil is a no-op).
func clearChurn(session *beads.Bead, sessFront *sessionpkg.Store) map[string]string {
	if session.Metadata["churn_count"] == "" || session.Metadata["churn_count"] == "0" {
		return nil
	}
	_ = sessFront.SetMarker(session.ID, "churn_count", "0")
	session.Metadata["churn_count"] = "0"
	return map[string]string{"churn_count": "0"}
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

// sessionWakeAttemptsInfo is the session.Info mirror of sessionWakeAttempts.
func sessionWakeAttemptsInfo(i sessionpkg.Info) int {
	return i.WakeAttempts
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

// sessionIsQuarantinedInfo is the session.Info mirror of sessionIsQuarantined.
func sessionIsQuarantinedInfo(i sessionpkg.Info, clk clock.Clock) bool {
	q := i.QuarantinedUntil
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

// mergeMetadataPatch merges src into dst and returns the result. Later (src)
// keys win. If dst is nil and src is non-nil, src is returned directly to
// avoid an extra allocation. Used by the stability/churn/rate-limit helpers to
// accumulate the complete mirrored batch for the write-returns-Info fold
// (front-door migration Step 6d).
func mergeMetadataPatch(dst, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}
	if len(dst) == 0 {
		return src
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// healState updates advisory state metadata only when changed (dirty check).
func healState(session *beads.Bead, alive bool, sessFront *sessionpkg.Store, clk clock.Clock) {
	healStateWithRollback(session, alive, sessFront, clk, 0, true)
}

// healStateWithRollback is the explicit-control variant of healState. When
// rollbackAvailable is false (e.g. the reconciler short-circuited the
// stale-pending-create rollback because storeQueryPartial=true) the heal path
// preserves pending_create_claim so the next non-partial tick can do the
// proper rollback. When true (default), healState clears the stale claim
// in-line after startupTimeout has elapsed to break the state=creating ↔
// state=asleep oscillation described in ga-mf1.
func healStateWithRollback(session *beads.Bead, alive bool, sessFront *sessionpkg.Store, clk clock.Clock, startupTimeout time.Duration, rollbackAvailable bool) map[string]string {
	if session == nil {
		return nil
	}
	// healState is the third writer in the closed-bead flap cycle. The
	// lifecycle projection still resolves to BaseStateDrained for closed
	// beads, so without this guard healState writes state=asleep on
	// every reconciler tick of a terminal bead — alternating with the
	// gc_swept / orphaned writes from the closeBead path. Closed beads
	// are terminal; their advisory state metadata should not move.
	if session.Status == "closed" {
		return nil
	}
	batch := healStatePatchWithRollback(*session, alive, clk, startupTimeout, rollbackAvailable)
	if len(batch) == 0 {
		return nil
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(batch))
	}
	if err := sessFront.ApplyPatch(session.ID, batch); err != nil {
		fmt.Fprintf(os.Stderr, "healState: SetMetadataBatch %s: %v\n", session.ID, err) //nolint:errcheck
	}
	for k, v := range batch {
		session.Metadata[k] = v
	}
	// S19 Stage 3 shadow: record the legacy compared-key writes this heal ACTUALLY
	// applied (no-op unless the shadow harness is enabled). Colocated with the
	// ApplyPatch + in-memory mirror so a pure builder (healStatePatch) invoked only
	// for inspection never records a write that never happened.
	recordLegacyCompareWrites(session.ID, "healStateWithRollback", batch)
	return batch
}

func healStatePatch(session beads.Bead, alive bool, clk clock.Clock) map[string]string {
	return healStatePatchWithRollback(session, alive, clk, 0, true)
}

func healStatePatchWithRollback(session beads.Bead, alive bool, clk clock.Clock, startupTimeout time.Duration, rollbackAvailable bool) map[string]string {
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
	lcInput := sessionpkg.LifecycleInputFromMetadata(session.Status, meta)
	lcInput.Runtime = sessionpkg.RuntimeFacts{Observed: true, Alive: alive}
	lcInput.CreatedAt = session.CreatedAt
	lcInput.StaleCreatingAfter = staleCreatingAfter
	lcInput.Now = now
	view := sessionpkg.ProjectLifecycle(lcInput)

	batch := make(map[string]string)
	if !alive && view.BaseState == sessionpkg.BaseStateDrained {
		if strings.TrimSpace(meta["state"]) != string(sessionpkg.StateAsleep) {
			batch["state"] = string(sessionpkg.StateAsleep)
		}
		if strings.TrimSpace(meta["sleep_reason"]) == "" {
			batch["sleep_reason"] = string(sessionpkg.SleepReasonDrained)
		}
		return emptyNil(batch)
	}

	target := string(view.ReconciledState)
	if target == "" && view.BaseState == sessionpkg.BaseStateNone {
		target = string(sessionpkg.StateAsleep)
		if alive {
			target = string(sessionpkg.StateAwake)
		} else if sessionStartRequested(session, clk) {
			target = string(sessionpkg.StateStartPending)
		}
	}
	stalePendingCreateRollback := false
	// failed-create is a terminal rollback marker written by
	// rollbackPendingCreate when a start attempt failed. A bead in this state
	// whose runtime is not alive must heal toward asleep, even if
	// pending_create_claim is still set from the failed attempt — otherwise
	// sessionStartRequested pulls the bead back to creating and the
	// reconciler ping-pongs forever. Clearing the stale claim in the same
	// batch finishes the rollback the lifecycle path started.
	if !alive && strings.TrimSpace(meta["state"]) == "failed-create" {
		if strings.TrimSpace(meta["pending_create_claim"]) == "true" && pendingCreateLeaseActive(session, clk, 0) {
			return nil
		}
		target = string(sessionpkg.StateAsleep)
		clearPendingCreateLease(meta, batch)
	}
	// ga-mf1: stale-creating projects to ReconciledState=asleep once the
	// pending_create lease has expired (creatingStateIsStale → true). Same
	// reasoning as failed-create above: if we leave pending_create_claim=true
	// in metadata, the next tick's projectWakeCauses re-emits
	// WakeCausePendingCreate and projectRuntimeProjection's post-creating
	// branch flips the projection back to StateCreating, ping-ponging the
	// bead forever between creating and asleep+runtime-missing. Clearing the
	// expired lease in the same heal batch lets the bead settle in asleep.
	//
	// Gate the clear on pendingCreateLeaseExpiredForRollback — the same
	// predicate the orphan rollback path uses — so we honor the longer
	// never-started lease (10 min) for beads that haven't yet had
	// last_woke_at recorded. creatingStateIsStale alone fires at 60s and
	// would race the rollback path's reservation.
	//
	// rollbackAvailable=false means the caller deferred the formal rollback
	// (e.g. storeQueryPartial); preserve the claim so the next complete tick
	// can drive attemptRollbackPendingCreate properly.
	if rollbackAvailable && !alive && strings.TrimSpace(meta["state"]) == "creating" {
		if pendingCreateLeaseExpiredForRollback(session, clk, startupTimeout) {
			target = string(sessionpkg.StateAsleep)
			stalePendingCreateRollback = true
			clearPendingCreateLease(meta, batch)
		}
	}
	if target == "" {
		return nil
	}
	if meta["state"] != target {
		batch["state"] = target
		if target == string(sessionpkg.StateAsleep) && (view.ResetContinuation || stalePendingCreateRollback) && strings.TrimSpace(meta["sleep_reason"]) == "" {
			batch["sleep_reason"] = string(sessionpkg.SleepReasonRuntimeMissing)
		}
	}
	if target == string(sessionpkg.StateAsleep) {
		if strings.TrimSpace(meta["sleep_reason"]) == "" && strings.TrimSpace(meta["state"]) == "failed-create" {
			batch["sleep_reason"] = string(sessionpkg.SleepReasonFailedCreate)
		}
		if view.ResetContinuation || stalePendingCreateRollback {
			if !isNamedSessionBead(session) || namedSessionMode(session) != "always" {
				batch["session_key"] = ""
				batch["started_config_hash"] = ""
				batch["continuation_reset_pending"] = "true"
				// Priming markers share started_config_hash's lifetime (S19
				// Stage 2): this asleep continuation reset re-primes.
				batch[sessionpkg.PrimedAtMetadataKey] = ""
				batch[sessionpkg.PrimingAttemptedAtMetadataKey] = ""
				batch[sessionpkg.PromptHashMetadataKey] = ""
			}
		}
	}
	return emptyNil(batch)
}

// clearPendingCreateLease writes empty-string clears for pending_create_claim
// and pending_create_started_at into the heal batch when the metadata
// currently carries a claim. Shared between the failed-create rollback path
// and the stale-creating heal path so both finish the rollback the lifecycle
// projection started, instead of letting the stale claim re-emit
// WakeCausePendingCreate on the next tick and re-enter state=creating.
func clearPendingCreateLease(meta, batch map[string]string) {
	if strings.TrimSpace(meta["pending_create_claim"]) != "true" {
		return
	}
	batch["pending_create_claim"] = ""
	batch["pending_create_started_at"] = ""
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
	return pendingCreateAttemptStale(session, clk)
}

// staleCreatingStateInfo is the session.Info sibling of staleCreatingState.
// Equivalence-proven. It reads the RAW metadata state (Info.MetadataState),
// matching staleCreatingState's session.Metadata["state"] read.
func staleCreatingStateInfo(i sessionpkg.Info, clk clock.Clock) bool {
	if clk == nil {
		return false
	}
	if strings.TrimSpace(i.MetadataState) != string(sessionpkg.StateCreating) {
		return false
	}
	return pendingCreateAttemptStaleInfo(i, clk)
}

// pendingCreateAttemptStale reports whether the current pending-create attempt
// has aged past staleCreatingStateTimeout, regardless of the bead's current
// projected state. This lets the reconciler keep never-started pending-create
// leases alive after healState has already rewritten state=creating to asleep.
func pendingCreateAttemptStale(session beads.Bead, clk clock.Clock) bool {
	if clk == nil {
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

// pendingCreateAttemptStaleInfo is the session.Info sibling of
// pendingCreateAttemptStale. Equivalence-proven.
func pendingCreateAttemptStaleInfo(i sessionpkg.Info, clk clock.Clock) bool {
	if clk == nil {
		return false
	}
	now := clk.Now()
	if started, ok := parseRFC3339Metadata(i.PendingCreateStartedAt); ok {
		return !now.Before(started.Add(staleCreatingStateTimeout))
	}
	if i.CreatedAt.IsZero() {
		return true
	}
	return !now.Before(i.CreatedAt.Add(staleCreatingStateTimeout))
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
	"active":                             true,
	"asleep":                             true,
	"awake":                              true,
	"stopped":                            true,
	"suspended":                          true,
	"orphaned":                           true,
	"closed":                             true,
	"quarantined":                        true,
	string(sessionpkg.StateStartPending): true,
	"creating":                           true,
	"drained":                            true,
	string(sessionpkg.StateFailedCreate): true, // processed so skip/orphan-close can release the slot
	"":                                   true, // empty state is valid (legacy beads)
}

// isKnownState returns true if the bead's metadata state is recognized by
// the current reconciler. Unknown states (from a newer version) are skipped
// to prevent panics during rollback.
func isKnownState(session beads.Bead) bool {
	return knownSessionStates[session.Metadata["state"]]
}

// isKnownStateInfo is the session.Info mirror of isKnownState. It keys off the
// RAW metadata state (Info.MetadataState, untrimmed), exactly as the bead form does.
func isKnownStateInfo(i sessionpkg.Info) bool {
	return knownSessionStates[i.MetadataState]
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

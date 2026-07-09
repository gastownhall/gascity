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

// wakeReasonsInfo and evaluateWakeReasonsInfo are the CLI `gc session`
// REASON-column display helpers ONLY. They compute the multi-reason, comma-joined
// cell shown to operators; their sole production caller is sessionReason in
// cmd_session.go. Production wake/sleep decisions come exclusively from
// ComputeAwakeSet (compute_awake_set.go) via awakeSetToWakeEvals — do NOT add wake
// logic here, it has no effect on reconciler behavior. They read the
// held/wait/quarantine/session-name metadata off the typed Info snapshot
// (Info.HeldUntil, Info.QuarantinedUntil, Info.WaitHold — untrimmed, matching the
// raw session.Metadata reads, Info.SessionNameMetadata, Info.ID) and route every
// classifier through its Info twin, while keeping the runtime probes
// (sessionAttachedForWakeReason, pendingInteractionReady) raw (§7 live edge).
func wakeReasonsInfo(
	info sessionpkg.Info,
	cfg *config.City,
	sp runtime.Provider,
	poolDesired map[string]int,
	workSet map[string]bool,
	readyWaitSet map[string]bool,
	clk clock.Clock,
) []WakeReason {
	return evaluateWakeReasonsInfo(info, cfg, sp, poolDesired, workSet, readyWaitSet, clk).Reasons
}

func evaluateWakeReasonsInfo(
	info sessionpkg.Info,
	cfg *config.City,
	sp runtime.Provider,
	poolDesired map[string]int,
	workSet map[string]bool,
	readyWaitSet map[string]bool,
	clk clock.Clock,
) wakeEvaluation {
	policy := resolveSessionSleepPolicyInfo(info, cfg, sp)

	// User hold suppresses all reasons.
	if held := info.HeldUntil; held != "" {
		if t, err := time.Parse(time.RFC3339, held); err == nil && clk.Now().Before(t) {
			return wakeEvaluation{Policy: policy}
		}
	}

	// Quarantine suppresses all reasons.
	if q := info.QuarantinedUntil; q != "" {
		if t, err := time.Parse(time.RFC3339, q); err == nil && clk.Now().Before(t) {
			return wakeEvaluation{Policy: policy}
		}
	}

	var reasons []WakeReason
	waitHold := info.WaitHold != ""
	name := info.SessionNameMetadata

	if readyWaitSet != nil && readyWaitSet[info.ID] {
		reasons = append(reasons, WakeWait)
	}
	if sessionStartRequestedInfo(info, clk) {
		reasons = append(reasons, WakeCreate)
	}

	template := normalizedSessionTemplateInfo(info, cfg)
	agent, configEligible := sessionWithinDesiredConfigInfo(info, cfg, poolDesired)
	if !waitHold && agent != nil && sessionMetadataStateInfo(info) == "active" && !policy.enabled() {
		reasons = append(reasons, WakeSession)
	}
	sleepSuppressed := configWakeSuppressedInfo(info, policy, sp, clk)
	if configEligible {
		hasDemand := poolDesired[template] > 0
		isAlwaysNamed := isNamedSessionInfo(info) && namedSessionModeInfo(info) == "always"
		if !waitHold && (!sleepSuppressed || hasDemand || isAlwaysNamed) {
			reasons = append(reasons, WakeConfig)
		}
	}
	// WakeWork: the work_query reports pending work for this template.
	if !waitHold && workSet[template] {
		reasons = append(reasons, WakeWork)
	}
	if !waitHold && sessionKeepWarmEligibleInfo(info, policy, sp, clk) {
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

// sessionWithinDesiredConfigInfo is the session.Info sibling of
// sessionWithinDesiredConfig. It routes the template resolution and the
// drained/named/manual classifiers through their Info twins, and compares
// dependency_only via Info.DependencyOnlyMetadata (the RAW, UNTRIMMED mirror) so
// the == "true" check stays byte-identical to the bead form on whitespace-padded
// input.
func sessionWithinDesiredConfigInfo(info sessionpkg.Info, cfg *config.City, poolDesired map[string]int) (*config.Agent, bool) {
	template := normalizedSessionTemplateInfo(info, cfg)
	agent := findAgentByTemplate(cfg, template)
	if agent == nil {
		return nil, false
	}
	if isDrainedSessionInfo(info) {
		return agent, false
	}
	if info.DependencyOnlyMetadata == "true" {
		return agent, false
	}
	if isNamedSessionInfo(info) {
		return agent, namedSessionModeInfo(info) == "always" || poolDesired[template] > 0
	}
	if isManualSessionInfo(info) {
		return agent, true
	}
	return agent, poolDesired[template] > 0
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

// sessionStartRequestedInfo reads the RAW metadata state (Info.MetadataState) and the
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

// sessionMetadataStateInfo normalizes the RAW persisted state metadata
// (Info.MetadataState, not the normalized Info.State) onto the display/decision
// vocabulary: awake→active, start_pending→creating, drained→asleep, everything
// else verbatim. The raw sessionMetadataState(beads.Bead) sibling was deleted in
// WI-6 R2 once its last caller (the wake-reason display lane) typed onto Info;
// the reference-implementation oracle in session_classifier_info_equiv_test.go
// pins this normalization.
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

// healExpiredTimersInfo is the fold form of the Phase-0 timer heal: it clears expired
// held_until / quarantined_until through the front door and returns the input
// Info advanced by each successful clear (write-returns-Info), with NO raw
// session.Metadata mirror. The reconciler's fold-then-build order (§2.3) applies
// this before the infoByID snapshot is built, so the returned Info is what the
// build projects — the mirror the raw form kept for the later re-projection is
// therefore unnecessary here.
//
// The hold-clear fold BEFORE the quarantine check is preserved from the raw body:
// ClearExpiredHoldPatch can blank sleep_reason, and ClearExpiredQuarantinePatch
// reads the post-hold sleep_reason (info.SleepReason), so the ordering is
// load-bearing. On a persist error the segment is returned unchanged, matching the
// raw `err == nil` mirror gate.
func healExpiredTimersInfo(info sessionpkg.Info, sessFront *sessionpkg.Store, clk clock.Clock) sessionpkg.Info {
	if h := info.HeldUntil; h != "" {
		if t, _ := time.Parse(time.RFC3339, h); !t.IsZero() && clk.Now().After(t) {
			batch := sessionpkg.ClearExpiredHoldPatch(info.SleepReason)
			if err := sessFront.ApplyPatch(info.ID, batch); err == nil {
				info = info.ApplyPatch(batch)
			}
		}
	}
	if q := info.QuarantinedUntil; q != "" {
		if t, _ := time.Parse(time.RFC3339, q); !t.IsZero() && clk.Now().After(t) {
			batch := sessionpkg.ClearExpiredQuarantinePatch(info.SleepReason)
			if err := sessFront.ApplyPatch(info.ID, batch); err == nil {
				info = info.ApplyPatch(batch)
			}
		}
	}
	return info
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
// Returns (info, true) when a stability event was recorded, where info is the
// snapshot advanced by every write on that path (front-door migration Step 6d,
// write-returns-Info, STEP6-PREPASS-AUDIT group 2). Returns (info, false)
// otherwise with the input Info unchanged (no write occurred).
func checkStability(info sessionpkg.Info, cfg *config.City, alive bool, dt *drainTracker, sessFront *sessionpkg.Store, clk clock.Clock, peek func(lines int) (string, error)) (sessionpkg.Info, bool) {
	if next, handled, err := checkRateLimitStability(info, cfg, alive, dt, sessFront, clk, peek); handled || err != nil {
		return next, true
	}
	if sessionpkg.DecideSessionExit(sessionExitFactsInfo(info, cfg, alive, dt, clk)) != sessionpkg.ExitRapidCrash {
		return info, false
	}
	info = recordWakeFailure(info, sessFront, clk, sessionAgentMetricIdentityInfo(info, cfg))
	info = clearLastWokeAt(info, sessFront)
	return info, true
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
// Returns (info, handled, err): handled=true when either was recorded, err when
// the write failed, and info the snapshot advanced by the hit-path write
// (front-door migration Step 6d, write-returns-Info, STEP6-PREPASS-AUDIT group 1).
// On every path that writes nothing (no-hit or persist error) the input Info is
// returned unchanged.
func checkRateLimitStability(info sessionpkg.Info, cfg *config.City, alive bool, dt *drainTracker, sessFront *sessionpkg.Store, clk clock.Clock, peek func(lines int) (string, error)) (sessionpkg.Info, bool, error) {
	facts := sessionExitFactsInfo(info, cfg, alive, dt, clk)
	facts.ScreenAvailable = peek != nil
	dec := sessionpkg.DecideSessionExit(facts)
	for dec == sessionpkg.ExitGatherScreen {
		facts.Screen = sessionpkg.ScreenOther
		if content, err := peek(rateLimitPeekLines); err == nil {
			if reason := runtime.ProviderTerminalErrorReason(content); reason != "" {
				next, markErr := markProviderTerminalError(info, sessFront, clk, reason)
				if markErr != nil {
					return info, false, markErr
				}
				return next, true, nil
			}
			if runtime.ContainsProviderRateLimitScreen(content) {
				facts.Screen = sessionpkg.ScreenRateLimit
			}
		}
		dec = sessionpkg.DecideSessionExit(facts)
	}
	if dec != sessionpkg.ExitRateLimitQuarantine {
		return info, false, nil
	}
	next, err := recordRateLimitQuarantine(info, sessFront, clk)
	if err != nil {
		return info, false, err
	}
	return next, true, nil
}

// sessionExitFactsInfo gathers the cheap facts for the exit-classification
// decider from the typed exit-decision mirrors. Info applies the identical
// TrimSpace=="true" for PendingCreateClaim, and its SleepReason/LastWokeAt fields
// are verbatim raw mirrors. The provider-screen fact is gathered on demand by
// checkStability when the decider asks for it.
func sessionExitFactsInfo(info sessionpkg.Info, cfg *config.City, alive bool, dt *drainTracker, clk clock.Clock) sessionpkg.ExitFacts {
	var startupTimeout time.Duration
	subprocess := false
	if cfg != nil {
		startupTimeout = cfg.Session.StartupTimeoutDuration()
		subprocess = cfg.Session.Provider == "subprocess"
	}
	return sessionpkg.ExitFacts{
		Alive:                      alive,
		SubprocessProvider:         subprocess,
		DrainPending:               dt != nil && dt.get(info.ID) != nil,
		PendingCreateClaim:         info.PendingCreateClaim,
		PendingCreateStartInFlight: pendingCreateStartInFlightInfo(info, clk, startupTimeout),
		SleepReason:                info.SleepReason,
		LastWokeAt:                 info.LastWokeAt,
		Now:                        clk.Now(),
		StabilityThreshold:         stabilityThreshold,
		ProductivityThreshold:      churnProductivityThreshold,
	}
}

// clearLastWokeAt clears last_woke_at on the session bead and returns the
// snapshot Info with that clear folded in (front-door migration Step 6d,
// write-returns-Info). It emits a single SetMetadata op (SetMarker) so the store
// write stays byte-identical to the raw single-key clear it replaces; the fold is
// applied unconditionally, exactly as the former raw session.Metadata mirror was.
func clearLastWokeAt(info sessionpkg.Info, sessFront *sessionpkg.Store) sessionpkg.Info {
	_ = sessFront.SetMarker(info.ID, "last_woke_at", "")
	return info.ApplyPatch(map[string]string{"last_woke_at": ""})
}

// recordRateLimitQuarantine backs off a session that exited into a provider
// rate-limit screen without treating the exit as a crash or resetting its
// conversation metadata. Returns (folded Info, nil) on success so the caller
// advances the typed snapshot with the quarantine write (front-door migration
// Step 6d, write-returns-Info); returns (info unchanged, err) on persist failure
// (ApplyPatchInfo leaves the snapshot pinned to the rejected write).
func recordRateLimitQuarantine(info sessionpkg.Info, sessFront *sessionpkg.Store, clk clock.Clock) (sessionpkg.Info, error) {
	batch := sessionpkg.RateLimitQuarantinePatch(clk.Now().Add(defaultRateLimitQuarantineDuration))
	next, err := sessFront.ApplyPatchInfo(info, batch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "recordRateLimitQuarantine: SetMetadataBatch %s: %v\n", info.ID, err) //nolint:errcheck
		return info, err
	}
	return next, nil
}

// markProviderTerminalError records the terminal-provider-error health/sleep
// metadata on a zombie session bead. It returns the snapshot Info with that write
// folded in (write-returns-Info, front-door migration Step 6d) and any persist
// error. On every path that writes nothing — a nil front door, an empty reason,
// or a persist failure — it returns the INPUT info unchanged, so an
// error-ignoring caller stays consistent with the store exactly when the bead was
// left untouched (ApplyPatchInfo guarantees the no-fold-on-error contract).
func markProviderTerminalError(info sessionpkg.Info, sessFront *sessionpkg.Store, clk clock.Clock, reason string) (sessionpkg.Info, error) {
	if sessFront == nil {
		return info, nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return info, nil
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
	return sessFront.ApplyPatchInfo(info, batch)
}

// sessionHasProviderTerminalErrorInfo reads the typed health/terminal-error
// mirrors to report whether a session recorded a non-retryable provider error.
func sessionHasProviderTerminalErrorInfo(info sessionpkg.Info) bool {
	if strings.TrimSpace(info.ProviderTerminalError) != "" {
		return true
	}
	return strings.TrimSpace(info.HealthState) == "unhealthy" &&
		info.Drainable &&
		strings.TrimSpace(info.HealthReason) != ""
}

// recordWakeFailure increments wake_attempts and quarantines if threshold
// exceeded. It returns the snapshot Info advanced by every write it made
// (front-door migration Step 6d, write-returns-Info):
//   - the ConversationResetPatch if session_key or started_config_hash was set
//   - the WakeFailureAccrualPatch (quarantine or single-counter increment)
//
// A quarantined accrual whose persist failed leaves the snapshot un-advanced for
// that write (ApplyPatchInfo folds only on success), so the returned Info matches
// what the raw bead carries. agentIdentity is the start-path-joinable agent label
// for gc.agent.quarantines.total.
func recordWakeFailure(info sessionpkg.Info, sessFront *sessionpkg.Store, clk clock.Clock, agentIdentity string) sessionpkg.Info {
	// Parse the raw wake_attempts mirror (not the pre-parsed info.WakeAttempts,
	// which zeroes on strconv.ErrRange) so an out-of-range counter yields the
	// same clamped value the old strconv.Atoi(session.Metadata[...]) path did —
	// byte-identical, mirroring how recordChurn treats info.ChurnCount.
	attempts, _ := strconv.Atoi(info.WakeAttemptsMetadata)

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
	// left behind by older builds. The store write is best-effort (its error is
	// intentionally ignored, as before) while the Info fold is unconditional.
	if info.SessionKey != "" || info.StartedConfigHash != "" {
		reset := sessionpkg.ConversationResetPatch(true)
		_ = sessFront.ApplyPatch(info.ID, reset)
		info = info.ApplyPatch(reset)
	}
	accrual := sessionpkg.WakeFailureAccrualPatch(attempts, defaultMaxWakeAttempts, clk.Now().Add(defaultQuarantineDuration))
	if accrual.Quarantined {
		if next, err := sessFront.ApplyPatchInfo(info, accrual.Patch); err == nil {
			telemetry.RecordAgentQuarantine(context.Background(), agentIdentity)
			info = next
		}
	} else {
		next := accrual.Patch["wake_attempts"]
		_ = sessFront.SetMarker(info.ID, "wake_attempts", next)
		info = info.ApplyPatch(map[string]string{"wake_attempts": next})
	}
	return info
}

// clearWakeFailures resets crash counter and quarantine for a stable session. It
// returns the snapshot Info advanced by the clear (write-returns-Info, front-door
// migration Step 6d), or the input Info unchanged when there is nothing to clear
// (both fields already absent/zero) or the persist failed.
func clearWakeFailures(info sessionpkg.Info, sessFront *sessionpkg.Store) sessionpkg.Info {
	batch := make(map[string]string, 2)
	// WakeAttemptsMetadata (the raw string mirror), not the parsed WakeAttempts int:
	// the != "0" distinction distinguishes an absent counter from a persisted "0".
	if info.WakeAttemptsMetadata != "" && info.WakeAttemptsMetadata != "0" {
		batch["wake_attempts"] = "0"
	}
	if info.QuarantinedUntil != "" {
		batch["quarantined_until"] = ""
	}
	if len(batch) == 0 {
		return info
	}
	next, err := sessFront.ApplyPatchInfo(info, batch)
	if err != nil {
		return info
	}
	return next
}

// checkChurn detects repeated non-productive wake→die cycles (context
// exhaustion death spirals). Unlike checkStability which catches rapid
// crashes (< stabilityThreshold), this catches sessions that survive past
// the stability threshold but die before being productive.
//
// Returns (info, churned): churned=true if a churn event was recorded (caller
// should skip further processing for this session), and info is the snapshot
// advanced by every write on either exit path (front-door migration Step 6d,
// write-returns-Info, STEP6-PREPASS-AUDIT group 5). The default (rapid-crash)
// path writes nothing and returns the input Info unchanged.
func checkChurn(info sessionpkg.Info, cfg *config.City, alive bool, dt *drainTracker, sessFront *sessionpkg.Store, clk clock.Clock) (sessionpkg.Info, bool) {
	switch sessionpkg.DecideSessionExit(sessionExitFactsInfo(info, cfg, alive, dt, clk)) {
	case sessionpkg.ExitChurn:
		info = recordChurn(info, sessFront, clk, sessionAgentMetricIdentityInfo(info, cfg))
		// Clear last_woke_at so this death is not re-counted next tick
		// (edge-triggered, same pattern as checkStability).
		info = clearLastWokeAt(info, sessFront)
		return info, true
	case sessionpkg.ExitProductiveDeath:
		// Session was productive — clear any stale churn count so it
		// doesn't carry over and cause premature quarantine next time.
		return clearChurn(info, sessFront), false
	default:
		// Rapid crashes belong to checkStability, which ran first.
		return info, false
	}
}

func isDeliberateSleepReason(reason string) bool {
	return sessionpkg.IsDeliberateSleepReason(reason)
}

// recordChurn increments the churn counter and clears session_key on every
// churn event to force a fresh conversation on next wake. When the counter
// reaches defaultMaxChurnCycles, the session is quarantined.
//
// It returns the snapshot Info advanced by every write it made (front-door
// migration Step 6d, write-returns-Info):
//   - the ConversationResetPatch (session_key/continuation_reset_pending) if
//     session_key was set
//   - the ChurnAccrualPatch (churn_count, and quarantined_until/sleep_reason
//     when quarantined) when the quarantined persist succeeded
//   - {"churn_count": next} on the non-quarantine path
//
// agentIdentity is the start-path-joinable agent label for gc.agent.quarantines.total.
func recordChurn(info sessionpkg.Info, sessFront *sessionpkg.Store, clk clock.Clock, agentIdentity string) sessionpkg.Info {
	count, _ := strconv.Atoi(info.ChurnCount)

	// Always clear session_key on churn — context exhaustion means the
	// conversation itself is the problem. A fresh conversation avoids
	// re-hitting the same wall. Best-effort store write (error ignored, as
	// before) with an unconditional Info fold.
	if info.SessionKey != "" {
		reset := sessionpkg.ConversationResetPatch(false)
		_ = sessFront.ApplyPatch(info.ID, reset)
		info = info.ApplyPatch(reset)
	}

	accrual := sessionpkg.ChurnAccrualPatch(count, defaultMaxChurnCycles, clk.Now().Add(defaultQuarantineDuration))
	if accrual.Quarantined {
		if next, err := sessFront.ApplyPatchInfo(info, accrual.Patch); err == nil {
			telemetry.RecordAgentQuarantine(context.Background(), agentIdentity)
			info = next
		}
		return info
	}

	next := accrual.Patch["churn_count"]
	_ = sessFront.SetMarker(info.ID, "churn_count", next)
	return info.ApplyPatch(map[string]string{"churn_count": next})
}

// clearChurn resets the churn counter for a productive session. It returns the
// snapshot Info with the {"churn_count":"0"} clear folded in (write-returns-Info),
// or the input Info unchanged when churn_count is already absent or zero (no-op).
// It emits a single SetMetadata op (SetMarker), byte-identical to the raw
// single-key clear it replaces.
func clearChurn(info sessionpkg.Info, sessFront *sessionpkg.Store) sessionpkg.Info {
	if info.ChurnCount == "" || info.ChurnCount == "0" {
		return info
	}
	_ = sessFront.SetMarker(info.ID, "churn_count", "0")
	return info.ApplyPatch(map[string]string{"churn_count": "0"})
}

// productiveLongEnoughInfo returns true if the session has been alive past
// churnProductivityThreshold — long enough to have done useful work — reading
// info.LastWokeAt (the verbatim raw last_woke_at mirror).
func productiveLongEnoughInfo(info sessionpkg.Info, clk clock.Clock) bool {
	lastWoke := info.LastWokeAt
	if lastWoke == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, lastWoke)
	if err != nil {
		return false
	}
	return clk.Now().Sub(t) >= churnProductivityThreshold
}

// stableLongEnoughInfo returns true if the session has been alive past
// stabilityThreshold, reading info.LastWokeAt (the verbatim raw last_woke_at
// mirror).
func stableLongEnoughInfo(info sessionpkg.Info, clk clock.Clock) bool {
	lastWoke := info.LastWokeAt
	if lastWoke == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, lastWoke)
	if err != nil {
		return false
	}
	return clk.Now().Sub(t) >= stabilityThreshold
}

// sessionWakeAttemptsInfo returns the current wake attempt count. It parses the
// raw WakeAttemptsMetadata string (rather than the pre-parsed i.WakeAttempts,
// which zeroes on strconv.ErrRange) so an out-of-range counter clamps identically
// to the historical strconv.Atoi(metadata) read.
func sessionWakeAttemptsInfo(i sessionpkg.Info) int {
	n, _ := strconv.Atoi(i.WakeAttemptsMetadata)
	return n
}

// sessionIsQuarantinedInfo returns true if the session has an active quarantine,
// reading info.QuarantinedUntil.
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

// healStatePatchWithRollbackInfo computes the advisory-state heal batch off the
// typed snapshot. It reads every state/lease key off Info (Info.MetadataState —
// the RAW state metadata; Info.SleepReason; Info.PendingCreateClaim; the
// lifecycle projection via LifecycleInputFromInfo; Info.CreatedAt) and routes
// each classifier through its equivalence-proven *Info twin
// (sessionStartRequestedInfo, pendingCreateLeaseActiveInfo,
// pendingCreateLeaseExpiredForRollbackInfo, isNamedSessionInfo,
// namedSessionModeInfo). Byte-identical to the bead form; the reconciler forward
// pass folds the returned batch onto its coherent infoByID snapshot.
func healStatePatchWithRollbackInfo(info sessionpkg.Info, alive bool, clk clock.Clock, startupTimeout time.Duration, rollbackAvailable bool) map[string]string {
	var now time.Time
	var staleCreatingAfter time.Duration
	if clk != nil {
		now = clk.Now()
		staleCreatingAfter = staleCreatingStateTimeout
	}
	lcInput := sessionpkg.LifecycleInputFromInfo(info)
	lcInput.Runtime = sessionpkg.RuntimeFacts{Observed: true, Alive: alive}
	lcInput.CreatedAt = info.CreatedAt
	lcInput.StaleCreatingAfter = staleCreatingAfter
	lcInput.Now = now
	view := sessionpkg.ProjectLifecycle(lcInput)

	batch := make(map[string]string)
	if !alive && view.BaseState == sessionpkg.BaseStateDrained {
		if strings.TrimSpace(info.MetadataState) != string(sessionpkg.StateAsleep) {
			batch["state"] = string(sessionpkg.StateAsleep)
		}
		if strings.TrimSpace(info.SleepReason) == "" {
			batch["sleep_reason"] = string(sessionpkg.SleepReasonDrained)
		}
		return emptyNil(batch)
	}

	target := string(view.ReconciledState)
	if target == "" && view.BaseState == sessionpkg.BaseStateNone {
		target = string(sessionpkg.StateAsleep)
		if alive {
			target = string(sessionpkg.StateAwake)
		} else if sessionStartRequestedInfo(info, clk) {
			target = string(sessionpkg.StateStartPending)
		}
	}
	stalePendingCreateRollback := false
	if !alive && strings.TrimSpace(info.MetadataState) == "failed-create" {
		if info.PendingCreateClaim && pendingCreateLeaseActiveInfo(info, clk, 0) {
			return nil
		}
		target = string(sessionpkg.StateAsleep)
		clearPendingCreateLeaseInfo(info.PendingCreateClaim, batch)
	}
	if rollbackAvailable && !alive && strings.TrimSpace(info.MetadataState) == "creating" {
		if pendingCreateLeaseExpiredForRollbackInfo(info, clk, startupTimeout) {
			target = string(sessionpkg.StateAsleep)
			stalePendingCreateRollback = true
			clearPendingCreateLeaseInfo(info.PendingCreateClaim, batch)
		}
	}
	if target == "" {
		return nil
	}
	if info.MetadataState != target {
		batch["state"] = target
		if target == string(sessionpkg.StateAsleep) && (view.ResetContinuation || stalePendingCreateRollback) && strings.TrimSpace(info.SleepReason) == "" {
			batch["sleep_reason"] = string(sessionpkg.SleepReasonRuntimeMissing)
		}
	}
	if target == string(sessionpkg.StateAsleep) {
		if strings.TrimSpace(info.SleepReason) == "" && strings.TrimSpace(info.MetadataState) == "failed-create" {
			batch["sleep_reason"] = string(sessionpkg.SleepReasonFailedCreate)
		}
		if view.ResetContinuation || stalePendingCreateRollback {
			if !isNamedSessionInfo(info) || namedSessionModeInfo(info) != "always" {
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

// healStateWithRollbackInfo is the session.Info sibling of healStateWithRollback:
// it reads its heal decision off the coherent infoByID snapshot entry instead of
// the raw *session bead, persists the batch through sessFront.ApplyPatch, and
// returns the batch for the reconciler to fold onto infoByID via ApplyPatchInfo.
// Unlike the raw form it does NOT mirror onto a raw bead — the snapshot fold is
// the single source of truth for the same-tick downstream readers (which now
// also read Info), so the two transitional W6 lockstep mirrors are gone.
func healStateWithRollbackInfo(info sessionpkg.Info, alive bool, sessFront *sessionpkg.Store, clk clock.Clock, startupTimeout time.Duration, rollbackAvailable bool) map[string]string {
	// Closed beads are terminal; their advisory state metadata should not move
	// (matches healStateWithRollback's session.Status == "closed" guard —
	// Info.Closed is the projected mirror).
	if info.Closed {
		return nil
	}
	batch := healStatePatchWithRollbackInfo(info, alive, clk, startupTimeout, rollbackAvailable)
	if len(batch) == 0 {
		return nil
	}
	if err := sessFront.ApplyPatch(info.ID, batch); err != nil {
		fmt.Fprintf(os.Stderr, "healState: SetMetadataBatch %s: %v\n", info.ID, err) //nolint:errcheck
	}
	// S19 Stage 3 shadow: record the legacy compared-key writes this heal ACTUALLY
	// applied (no-op unless the shadow harness is enabled). Colocated with the
	// ApplyPatch so a pure builder (healStatePatchWithRollbackInfo) invoked only for
	// inspection never records a write that never happened.
	recordLegacyCompareWrites(info.ID, "healStateWithRollback", batch)
	return batch
}

// clearPendingCreateLeaseInfo is the Info-form counterpart of
// clearPendingCreateLease. Info.PendingCreateClaim already carries
// strings.TrimSpace(pending_create_claim) == "true", so the gate is identical to
// the raw form's TrimSpace compare.
func clearPendingCreateLeaseInfo(pendingClaim bool, batch map[string]string) {
	if !pendingClaim {
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

// staleCreatingStateInfo returns true when a state=creating bead has been
// stuck in that state longer than staleCreatingStateTimeout. It reads the RAW
// metadata state (Info.MetadataState).
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
//     this metadata key was introduced.
func staleCreatingStateInfo(i sessionpkg.Info, clk clock.Clock) bool {
	if clk == nil {
		return false
	}
	if strings.TrimSpace(i.MetadataState) != string(sessionpkg.StateCreating) {
		return false
	}
	return pendingCreateAttemptStaleInfo(i, clk)
}

// pendingCreateAttemptStaleInfo reports whether the current pending-create
// attempt has aged past staleCreatingStateTimeout, regardless of the bead's
// current projected state. This lets the reconciler keep never-started
// pending-create leases alive after heal has rewritten state=creating to asleep.
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

// topoOrderRows is the ReconcileSession form of topoOrder: it orders the tick's
// rows by template dependency edges, reading each row's template off
// Info.Template (the verbatim raw mirror of b.Metadata["template"], so the
// grouping is byte-identical to the raw form). With no deps, or on a dependency
// cycle, it returns the input rows unchanged — identical fallback semantics to
// topoOrder. TestTopoOrderRowsMatchesTopoOrder pins the equivalence.
func topoOrderRows(rows []sessionpkg.ReconcileSession, deps map[string][]string) []sessionpkg.ReconcileSession {
	if len(deps) == 0 {
		return rows
	}

	templateRows := make(map[string][]sessionpkg.ReconcileSession)
	for _, r := range rows {
		template := r.Info.Template
		templateRows[template] = append(templateRows[template], r)
	}

	var templates []string
	seen := make(map[string]bool)
	for _, r := range rows {
		t := r.Info.Template
		if !seen[t] {
			seen[t] = true
			templates = append(templates, t)
		}
	}

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
				if seen[dep] {
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
		return rows
	}

	var result []sessionpkg.ReconcileSession
	for _, t := range order {
		result = append(result, templateRows[t]...)
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

// isKnownStateInfo returns true if the session's metadata state is recognized by
// the current reconciler. Unknown states (from a newer version) are skipped to
// prevent panics during rollback. It keys off the RAW metadata state
// (Info.MetadataState, untrimmed).
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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const (
	defaultMaxParallelStartsPerWave = 3
	defaultMaxParallelStopsPerWave  = 3
	defaultMaxParallelInterrupts    = 16

	// staleKeyDetectDelay is how long to wait after starting a session
	// before checking if it died immediately (stale resume key detection).
	// Matches the same constant in internal/session/chat.go.
	staleKeyDetectDelay = 2 * time.Second
)

type startCandidate struct {
	session *beads.Bead
	tp      TemplateParams
	order   int
}

func (c startCandidate) name() string {
	return c.session.Metadata["session_name"]
}

func (c startCandidate) logicalTemplate(cfg *config.City) string {
	if c.tp.TemplateName != "" {
		return c.tp.TemplateName
	}
	return normalizedSessionTemplate(*c.session, cfg)
}

type preparedStart struct {
	candidate     startCandidate
	cfg           runtime.Config
	coreHash      string
	coreBreakdown map[string]string
	liveHash      string
}

type startResult struct {
	prepared        preparedStart
	err             error
	outcome         string
	started         time.Time
	finished        time.Time
	rollbackPending bool
}

type stopTarget struct {
	name        string
	template    string
	subject     string
	order       int
	resolved    bool
	poolManaged bool
}

type stopResult struct {
	target   stopTarget
	err      error
	outcome  string
	started  time.Time
	finished time.Time
}

func logLifecycleOutcome(
	w io.Writer,
	op string,
	wave int,
	name, template, outcome string,
	started, finished time.Time,
	err error,
) {
	if w == nil {
		return
	}
	msg := fmt.Sprintf("session lifecycle: op=%s wave=%d session=%s template=%s outcome=%s", op, wave, name, template, outcome)
	if !started.IsZero() && !finished.IsZero() {
		msg += fmt.Sprintf(" duration=%s", finished.Sub(started).Round(time.Millisecond))
	}
	if err != nil {
		msg += fmt.Sprintf(" err=%s", formatLifecycleError(err))
	}
	fmt.Fprintln(w, msg) //nolint:errcheck // best-effort diagnostics
}

func formatLifecycleError(err error) string {
	if err == nil {
		return ""
	}
	return strings.ReplaceAll(err.Error(), "\n", "\\n")
}

func logLifecycleWave(w io.Writer, op string, wave int, started time.Time, count int) {
	if w == nil || count <= 1 {
		return
	}
	duration := time.Since(started).Round(time.Millisecond)
	fmt.Fprintf(w, "session lifecycle: op=%s wave=%d candidates=%d duration=%s\n", //nolint:errcheck // best-effort diagnostics
		op, wave, count, duration)
}

func dependencyTemplateWaveOrder(templatesInOrder []string, deps map[string][]string) (map[string]int, bool) {
	if len(templatesInOrder) == 0 {
		return map[string]int{}, true
	}
	present := make(map[string]bool, len(templatesInOrder))
	indegree := make(map[string]int, len(templatesInOrder))
	dependents := make(map[string][]string, len(templatesInOrder))
	emitted := make(map[string]bool, len(templatesInOrder))
	for _, template := range templatesInOrder {
		present[template] = true
	}
	for _, template := range templatesInOrder {
		for _, dep := range deps[template] {
			if !present[dep] {
				continue
			}
			indegree[template]++
			dependents[dep] = append(dependents[dep], template)
		}
	}
	waveByTemplate := make(map[string]int, len(templatesInOrder))
	emittedCount := 0
	wave := 0
	for emittedCount < len(templatesInOrder) {
		var ready []string
		for _, template := range templatesInOrder {
			if emitted[template] {
				continue
			}
			if indegree[template] == 0 {
				ready = append(ready, template)
			}
		}
		if len(ready) == 0 {
			return nil, false
		}
		for _, template := range ready {
			emitted[template] = true
			waveByTemplate[template] = wave
			emittedCount++
			for _, dependent := range dependents[template] {
				indegree[dependent]--
			}
		}
		wave++
	}
	return waveByTemplate, true
}

func strictSerialWaveOrder[T any](items []T) map[int]int {
	result := make(map[int]int, len(items))
	for i := range items {
		result[i] = i
	}
	return result
}

func dependencyTemplateAlive(
	template string,
	cfg *config.City,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	cityName string,
	store beads.Store,
) bool {
	if cfg == nil || template == "" {
		return false
	}
	cfgAgent := findAgentByTemplate(cfg, template)
	if cfgAgent == nil {
		return false
	}
	if isMultiSessionCfgAgent(cfgAgent) {
		for name, tp := range desiredState {
			if tp.TemplateName != template {
				continue
			}
			if sp.IsRunning(name) && sp.ProcessAlive(name, tp.Hints.ProcessNames) {
				return true
			}
		}
		return false
	}
	sessionName := lookupSessionNameOrLegacy(store, cityName, template, cfg.Workspace.SessionTemplate)
	depTP := desiredState[sessionName]
	return sp.IsRunning(sessionName) && sp.ProcessAlive(sessionName, depTP.Hints.ProcessNames)
}

func candidateWaveOrder(
	candidates []startCandidate,
	cfg *config.City,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	cityName string,
	store beads.Store,
) (map[int]int, bool) {
	if len(candidates) == 0 {
		return map[int]int{}, true
	}
	var templatesInOrder []string
	templateSeen := make(map[string]bool)
	candidateTemplates := make(map[string]bool)
	resolvedTemplates := make([]string, len(candidates))
	for idx, candidate := range candidates {
		template := candidate.logicalTemplate(cfg)
		resolvedTemplates[idx] = template
		candidateTemplates[template] = true
		if !templateSeen[template] {
			templateSeen[template] = true
			templatesInOrder = append(templatesInOrder, template)
		}
	}
	filteredDeps := make(map[string][]string)
	for _, template := range templatesInOrder {
		cfgAgent := findAgentByTemplate(cfg, template)
		if cfgAgent == nil {
			continue
		}
		for _, dep := range cfgAgent.DependsOn {
			if dependencyTemplateAlive(dep, cfg, desiredState, sp, cityName, store) {
				continue
			}
			if candidateTemplates[dep] {
				filteredDeps[template] = append(filteredDeps[template], dep)
			}
		}
	}
	templateWave, ok := dependencyTemplateWaveOrder(templatesInOrder, filteredDeps)
	if !ok {
		return strictSerialWaveOrder(candidates), false
	}
	candidateWave := make(map[int]int, len(candidates))
	for idx := range candidates {
		wave, ok := templateWave[resolvedTemplates[idx]]
		if !ok {
			continue
		}
		candidateWave[idx] = wave
	}
	return candidateWave, true
}

func prepareStartCandidate(
	candidate startCandidate,
	cfg *config.City,
	store beads.Store,
	clk clock.Clock,
) (*preparedStart, error) {
	session := candidate.session
	tp := candidate.tp
	if _, _, err := preWakeCommit(session, store, clk); err != nil {
		return nil, err
	}
	agentCfg := templateParamsToConfig(tp)

	// Apply template_overrides from bead metadata. These are per-session
	// schema option overrides (e.g., {"model":"opus","effort":"high"}) that
	// override the agent's default CLI flags for specific options.
	// Build complete options: effective defaults + explicit overrides so
	// unoverridden defaults are preserved when replaceSchemaFlags strips all
	// schema flags.
	if rawOverrides := session.Metadata["template_overrides"]; rawOverrides != "" {
		if tp.ResolvedProvider != nil && len(tp.ResolvedProvider.OptionsSchema) > 0 {
			var overrides map[string]string
			if err := json.Unmarshal([]byte(rawOverrides), &overrides); err != nil {
				log.Printf("session %s: invalid template_overrides JSON: %v", session.ID, err)
			} else if len(overrides) > 0 {
				fullOptions := make(map[string]string)
				for k, v := range tp.ResolvedProvider.EffectiveDefaults {
					fullOptions[k] = v
				}
				for k, v := range overrides {
					if k == "initial_message" {
						continue // handled separately below, not a schema option
					}
					fullOptions[k] = v
				}
				args, resolveErr := config.ResolveExplicitOptions(tp.ResolvedProvider.OptionsSchema, fullOptions)
				if resolveErr != nil {
					log.Printf("session %s: template_overrides resolution error: %v", session.ID, resolveErr)
				} else if len(args) > 0 {
					agentCfg.Command = replaceSchemaFlags(agentCfg.Command, tp.ResolvedProvider.OptionsSchema, args)
				}
			}
		}
	}

	coreHash := runtime.CoreFingerprint(agentCfg)
	coreBreakdown := runtime.CoreFingerprintBreakdown(agentCfg)
	liveHash := runtime.LiveFingerprint(agentCfg)
	if wd := resolveTaskWorkDir(store, session.ID, candidate.name(), strings.TrimSpace(session.Metadata["alias"]), candidate.logicalTemplate(cfg)); wd != "" {
		agentCfg.WorkDir = wd
	} else if wd := session.Metadata["work_dir"]; wd != "" {
		agentCfg.WorkDir = wd
	}
	if session.Metadata["session_key"] == "" && tp.ResolvedProvider != nil && tp.ResolvedProvider.SessionIDFlag != "" {
		sessionKey, err := sessionpkg.GenerateSessionKey()
		if err != nil {
			return nil, fmt.Errorf("generating session key: %w", err)
		}
		if store != nil && session.ID != "" {
			if err := store.SetMetadata(session.ID, "session_key", sessionKey); err != nil {
				return nil, fmt.Errorf("storing session key: %w", err)
			}
		}
		if session.Metadata == nil {
			session.Metadata = make(map[string]string)
		}
		session.Metadata["session_key"] = sessionKey
	}
	if sk := session.Metadata["session_key"]; sk != "" && tp.ResolvedProvider != nil {
		firstStart := session.Metadata["started_config_hash"] == ""
		forceFresh := session.Metadata["wake_mode"] == "fresh"
		agentCfg.Command = resolveSessionCommand(agentCfg.Command, sk, tp.ResolvedProvider, firstStart, forceFresh)
	}
	// Initial message: append to prompt on first start only.
	// Schema overrides were already applied in the block above (before coreHash).
	// resolveSessionCommand only adds --resume/--session-id which are not schema
	// flags, so the overrides don't need to be re-applied.
	if raw := session.Metadata["template_overrides"]; raw != "" {
		var overrides map[string]string
		if err := json.Unmarshal([]byte(raw), &overrides); err == nil {
			firstStart := session.Metadata["started_config_hash"] == ""
			forceFresh := session.Metadata["wake_mode"] == "fresh"
			if msg, ok := overrides["initial_message"]; ok && msg != "" && (firstStart || forceFresh) {
				if tp.ResolvedProvider != nil && tp.ResolvedProvider.PromptMode == "none" {
					if agentCfg.Nudge != "" {
						agentCfg.Nudge = agentCfg.Nudge + "\n\n---\n\nUser message:\n" + msg
					} else {
						agentCfg.Nudge = msg
					}
				} else {
					existing := ""
					if agentCfg.PromptSuffix != "" {
						parts := shellquote.Split(agentCfg.PromptSuffix)
						if len(parts) > 0 {
							existing = parts[0]
						}
					}
					if existing != "" {
						agentCfg.PromptSuffix = shellquote.Quote(existing + "\n\n---\n\nUser message:\n" + msg)
					} else {
						agentCfg.PromptSuffix = shellquote.Quote(msg)
					}
				}
			}
		}
	}
	generation, _ := strconv.Atoi(session.Metadata["generation"])
	if generation <= 0 {
		generation = sessionpkg.DefaultGeneration
	}
	continuationEpoch, _ := strconv.Atoi(session.Metadata["continuation_epoch"])
	if continuationEpoch <= 0 {
		continuationEpoch = sessionpkg.DefaultContinuationEpoch
	}
	instanceToken := session.Metadata["instance_token"]
	if instanceToken == "" {
		instanceToken = sessionpkg.NewInstanceToken()
		if err := store.SetMetadata(session.ID, "instance_token", instanceToken); err != nil {
			return nil, err
		}
		session.Metadata["instance_token"] = instanceToken
	}
	agentCfg.Env = mergeEnv(agentCfg.Env, sessionpkg.RuntimeEnvWithSessionContext(
		session.ID,
		candidate.name(),
		strings.TrimSpace(session.Metadata["alias"]),
		strings.TrimSpace(session.Metadata["template"]),
		strings.TrimSpace(session.Metadata["session_origin"]),
		generation,
		continuationEpoch,
		instanceToken,
	))
	if gcProvider := strings.TrimSpace(session.Metadata["provider_kind"]); gcProvider != "" {
		agentCfg.Env = mergeEnv(agentCfg.Env, map[string]string{"GC_PROVIDER": gcProvider})
	} else if gcProvider := strings.TrimSpace(session.Metadata["provider"]); gcProvider != "" {
		agentCfg.Env = mergeEnv(agentCfg.Env, map[string]string{"GC_PROVIDER": gcProvider})
	}
	agentCfg = runtime.SyncWorkDirEnv(agentCfg)
	return &preparedStart{
		candidate:     candidate,
		cfg:           agentCfg,
		coreHash:      coreHash,
		coreBreakdown: coreBreakdown,
		liveHash:      liveHash,
	}, nil
}

func executePreparedStartWave(
	ctx context.Context,
	prepared []preparedStart,
	sp runtime.Provider,
	startupTimeout time.Duration,
	maxParallel int,
) []startResult {
	if len(prepared) == 0 {
		return nil
	}
	if maxParallel <= 0 {
		maxParallel = 1
	}
	results := make([]startResult, len(prepared))
	sem := make(chan struct{}, maxParallel)
	done := make(chan int, len(prepared))
	for i, item := range prepared {
		i, item := i, item
		sem <- struct{}{}
		go func() {
			started := time.Now()
			defer func() {
				if recovered := recover(); recovered != nil {
					stack := debug.Stack()
					results[i] = startResult{
						prepared: item,
						err:      fmt.Errorf("panic during start: %v\n%s", recovered, stack),
						outcome:  "panic_recovered",
						started:  started,
						finished: time.Now(),
					}
				}
				<-sem
				done <- i
			}()
			startCtx := ctx
			cancel := func() {}
			if startupTimeout > 0 {
				startCtx, cancel = context.WithTimeout(ctx, startupTimeout)
			}
			defer cancel()
			err := sp.Start(startCtx, item.candidate.name(), item.cfg)
			// Stale session key detection: if the session was started
			// with a resume flag but dies immediately, the session key
			// likely references a conversation that no longer exists
			// (e.g., "No conversation found"). Report as a failure so
			// recordWakeFailure clears the key for the next attempt.
			if err == nil && item.candidate.session.Metadata["session_key"] != "" {
				time.Sleep(staleKeyDetectDelay)
				if !sp.IsRunning(item.candidate.name()) {
					err = fmt.Errorf("session %q died during startup", item.candidate.name())
				}
			}
			finished := time.Now()
			rollbackPending := err != nil && shouldRollbackPendingCreate(item.candidate.session)
			if err != nil && rollbackPending && runningSessionMatchesPendingCreate(item.candidate.session, item.candidate.name(), sp) {
				results[i] = startResult{
					prepared:        item,
					err:             nil,
					outcome:         "start_error_converged",
					started:         started,
					finished:        finished,
					rollbackPending: false,
				}
				return
			}
			var outcome string
			switch {
			case err == nil:
				outcome = "success"
			case startCtx.Err() == context.DeadlineExceeded:
				outcome = "deadline_exceeded"
			case startCtx.Err() == context.Canceled:
				outcome = "canceled"
			case errors.Is(err, runtime.ErrSessionInitializing):
				outcome = "session_initializing"
				err = nil
			case errors.Is(err, runtime.ErrSessionExists) && sp.IsRunning(item.candidate.name()):
				if rollbackPending && runningSessionMatchesPendingCreate(item.candidate.session, item.candidate.name(), sp) {
					outcome = "session_exists_converged"
					err = nil
					rollbackPending = false
				} else {
					outcome = "session_exists"
				}
			default:
				outcome = "provider_error"
			}
			results[i] = startResult{
				prepared:        item,
				err:             err,
				outcome:         outcome,
				started:         started,
				finished:        finished,
				rollbackPending: rollbackPending,
			}
		}()
	}
	for range prepared {
		<-done
	}
	return results
}

func commitStartResult(
	result startResult,
	store beads.Store,
	clk clock.Clock,
	rec events.Recorder,
	wave int, //nolint:unparam // always 0 here but passed through to commitStartResultTraced which uses it
	stdout, stderr io.Writer,
) bool {
	return commitStartResultTraced(result, store, clk, rec, wave, stdout, stderr, nil)
}

// confirmPendingStart reports whether a session in the given metadata
// state should be transitioned to "active" after a successful runtime
// spawn. Empty, "creating", "asleep", and "drained" all indicate the
// session was pending a spawn; "awake" is treated by the reconciler as
// equivalent to "active" and is intentionally NOT restamped (a no-op
// metadata write on every spawn). Any other state ("draining",
// "archived", "quarantined", ...) is left alone.
func confirmPendingStart(currentState string) bool {
	switch sessionpkg.State(strings.TrimSpace(currentState)) {
	case "", sessionpkg.StateCreating, sessionpkg.StateAsleep, sessionpkg.State("drained"):
		return true
	}
	return false
}

func commitStartResultTraced(
	result startResult,
	store beads.Store,
	clk clock.Clock,
	rec events.Recorder,
	wave int,
	stdout, stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
) bool {
	session := result.prepared.candidate.session
	name := result.prepared.candidate.name()
	tp := result.prepared.candidate.tp
	// Session still starting up — back off silently without recording failure.
	// The reconciler will retry on the next patrol tick.
	if result.outcome == "session_initializing" {
		logLifecycleOutcome(stderr, "start", wave, name, tp.TemplateName, result.outcome, result.started, result.finished, nil)
		return false
	}
	if result.err != nil {
		if result.rollbackPending {
			fmt.Fprintf(stderr, "session reconciler: starting %s: %s\n", name, formatLifecycleError(result.err)) //nolint:errcheck
			if trace != nil {
				trace.recordOperation("reconciler.start.rollback_pending", tp.TemplateName, name, "", "start", result.outcome, traceRecordPayload{
					"error": formatLifecycleError(result.err),
				}, "")
			}
			rollbackPendingCreate(session, store, clk.Now().UTC(), stderr)
			logLifecycleOutcome(stderr, "start", wave, name, tp.TemplateName, result.outcome, result.started, result.finished, result.err)
			return false
		}
		fmt.Fprintf(stderr, "session reconciler: starting %s: %s\n", name, formatLifecycleError(result.err)) //nolint:errcheck
		_ = store.SetMetadata(session.ID, "last_woke_at", "")
		session.Metadata["last_woke_at"] = ""
		recordWakeFailure(session, store, clk)
		if trace != nil {
			trace.recordOperation("reconciler.start.failed", tp.TemplateName, name, "", "start", result.outcome, traceRecordPayload{
				"error": formatLifecycleError(result.err),
			}, "")
		}
		logLifecycleOutcome(stderr, "start", wave, name, tp.TemplateName, result.outcome, result.started, result.finished, result.err)
		return false
	}
	fmt.Fprintf(stdout, "Woke session '%s'\n", tp.DisplayName()) //nolint:errcheck
	rec.Record(events.Event{
		Type:    events.SessionWoke,
		Actor:   "gc",
		Subject: tp.DisplayName(),
	})
	if err := clearPendingCreateClaim(session, store); err != nil {
		fmt.Fprintf(stderr, "session reconciler: clearing pending create claim for %s: %v\n", name, err) //nolint:errcheck
	}
	coreBreakdown := ""
	if bdj, err := json.Marshal(result.prepared.coreBreakdown); err == nil {
		coreBreakdown = string(bdj)
	}
	// Transition creating/asleep/drained beads to active once the runtime
	// spawn has confirmed. Folded into this metadata batch so the state
	// write is atomic with the hash writes and avoids a second round-trip
	// per spawn. See confirmPendingStart for the state gate.
	metadata := sessionpkg.CommitStartedPatch(sessionpkg.CommitStartedPatchInput{
		CoreHash:         result.prepared.coreHash,
		LiveHash:         result.prepared.liveHash,
		CoreBreakdown:    coreBreakdown,
		ConfirmState:     confirmPendingStart(session.Metadata["state"]),
		ClearSleepReason: session.Metadata["sleep_reason"] != "",
	})
	if err := store.SetMetadataBatch(session.ID, metadata); err != nil {
		fmt.Fprintf(stderr, "session reconciler: storing hashes for %s: %v\n", name, err) //nolint:errcheck
		if trace != nil {
			trace.recordMutation("bead_metadata", tp.TemplateName, name, "metadata_batch", session.ID, "started_config_hash", "", result.prepared.coreHash, "failed", traceRecordPayload{
				"wave":  wave,
				"error": err.Error(),
			}, "")
		}
		// The runtime started, but we failed to persist metadata
		// (including the state transition to active). Report failure so
		// the reconciler retries on the next tick rather than leaving
		// the session stuck in "creating" where it gets orphan-drained.
		logLifecycleOutcome(stderr, "start", wave, name, tp.TemplateName, "metadata_batch_failed", result.started, result.finished, err)
		return false
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string)
	}
	for key, value := range metadata {
		session.Metadata[key] = value
	}
	if trace != nil {
		trace.recordMutation("bead_metadata", tp.TemplateName, name, "metadata_batch", session.ID, "started_config_hash", "", result.prepared.coreHash, "success", traceRecordPayload{
			"wave": wave,
		}, "")
	}
	logLifecycleOutcome(stderr, "start", wave, name, tp.TemplateName, result.outcome, result.started, result.finished, nil)
	return true
}

func clearPendingCreateClaim(session *beads.Bead, store beads.Store) error {
	if !shouldRollbackPendingCreate(session) || store == nil {
		return nil
	}
	if err := store.SetMetadata(session.ID, "pending_create_claim", ""); err != nil {
		return err
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string)
	}
	session.Metadata["pending_create_claim"] = ""
	return nil
}

func shouldRollbackPendingCreate(session *beads.Bead) bool {
	if session == nil {
		return false
	}
	return strings.TrimSpace(session.Metadata["pending_create_claim"]) == "true"
}

func runningSessionMatchesPendingCreate(session *beads.Bead, sessionName string, sp runtime.Provider) bool {
	if session == nil || sp == nil {
		return false
	}
	if liveID, err := sp.GetMeta(sessionName, "GC_SESSION_ID"); err == nil {
		liveID = strings.TrimSpace(liveID)
		if liveID != "" {
			return liveID == session.ID
		}
	}
	expectedToken := strings.TrimSpace(session.Metadata["instance_token"])
	if expectedToken == "" {
		return false
	}
	liveToken, err := sp.GetMeta(sessionName, "GC_INSTANCE_TOKEN")
	if err != nil {
		return false
	}
	return strings.TrimSpace(liveToken) == expectedToken
}

func rollbackPendingCreate(session *beads.Bead, store beads.Store, now time.Time, stderr io.Writer) {
	if session == nil || store == nil {
		return
	}
	if strings.TrimSpace(session.Metadata["session_name_explicit"]) == "true" {
		if setMeta(store, session.ID, "session_name", "", stderr) == nil {
			if session.Metadata == nil {
				session.Metadata = make(map[string]string)
			}
			session.Metadata["session_name"] = ""
		}
	}
	closeBead(store, session.ID, "failed-create", now, stderr)
}

func executePlannedStarts(
	ctx context.Context,
	candidates []startCandidate,
	cfg *config.City,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	store beads.Store,
	cityName string,
	clk clock.Clock,
	rec events.Recorder,
	startupTimeout time.Duration,
	stdout, stderr io.Writer,
) int {
	return executePlannedStartsTraced(ctx, candidates, cfg, desiredState, sp, store, cityName, clk, rec, startupTimeout, stdout, stderr, nil)
}

func executePlannedStartsTraced(
	ctx context.Context,
	candidates []startCandidate,
	cfg *config.City,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	store beads.Store,
	cityName string,
	clk clock.Clock,
	rec events.Recorder,
	startupTimeout time.Duration,
	stdout, stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
) int {
	if len(candidates) == 0 {
		return 0
	}
	waveByCandidate, ok := candidateWaveOrder(candidates, cfg, desiredState, sp, cityName, store)
	if !ok {
		fmt.Fprintln(stderr, "session reconciler: dependency graph fallback to serial start order") //nolint:errcheck
	}
	maxWave := -1
	for _, wave := range waveByCandidate {
		if wave > maxWave {
			maxWave = wave
		}
	}
	wakeCount := 0
	for wave := 0; wave <= maxWave; wave++ {
		waveStarted := time.Now()
		var waveCandidates []startCandidate
		for idx, candidate := range candidates {
			if waveByCandidate[idx] == wave {
				waveCandidates = append(waveCandidates, candidate)
			}
		}
		if len(waveCandidates) == 0 {
			continue
		}
		if wakeCount >= defaultMaxWakesPerTick {
			for _, candidate := range waveCandidates {
				logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), "deferred_by_wake_budget", time.Time{}, time.Time{}, nil)
			}
			continue
		}
		var ready []startCandidate
		for _, candidate := range waveCandidates {
			if !allDependenciesAliveForTemplate(candidate.logicalTemplate(cfg), cfg, desiredState, sp, cityName, store) {
				logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), "blocked_on_dependencies", time.Time{}, time.Time{}, nil)
				continue
			}
			ready = append(ready, candidate)
		}
		for offset := 0; offset < len(ready); {
			if wakeCount >= defaultMaxWakesPerTick {
				for _, candidate := range ready[offset:] {
					logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), "deferred_by_wake_budget", time.Time{}, time.Time{}, nil)
				}
				break
			}
			batchSize := min(defaultMaxParallelStartsPerWave, defaultMaxWakesPerTick-wakeCount)
			end := min(offset+batchSize, len(ready))
			var prepared []preparedStart
			for _, candidate := range ready[offset:end] {
				if !allDependenciesAliveForTemplate(candidate.logicalTemplate(cfg), cfg, desiredState, sp, cityName, store) {
					logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), "blocked_on_dependencies", time.Time{}, time.Time{}, nil)
					continue
				}
				item, err := prepareStartCandidate(candidate, cfg, store, clk)
				if err != nil {
					fmt.Fprintf(stderr, "session reconciler: pre-wake %s: %s\n", candidate.name(), formatLifecycleError(err)) //nolint:errcheck
					logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), "failed", time.Time{}, time.Time{}, err)
					continue
				}
				prepared = append(prepared, *item)
			}
			offset = end
			results := executePreparedStartWave(ctx, prepared, sp, startupTimeout, defaultMaxParallelStartsPerWave)
			for _, result := range results {
				if trace != nil {
					trace.recordOperation("reconciler.start.execute", result.prepared.candidate.tp.TemplateName, result.prepared.candidate.name(), "", "start", result.outcome, traceRecordPayload{
						"rollback_pending": result.rollbackPending,
						"duration_ms":      result.finished.Sub(result.started).Milliseconds(),
					}, "")
				}
				if commitStartResultTraced(result, store, clk, rec, wave, stdout, stderr, trace) {
					wakeCount++
				}
			}
		}
		logLifecycleWave(stderr, "start", wave, waveStarted, len(waveCandidates))
	}
	return wakeCount
}

func stopWaveOrder(targets []stopTarget, cfg *config.City) (map[int]int, bool) {
	if len(targets) == 0 {
		return map[int]int{}, true
	}
	var templatesInOrder []string
	templateSeen := make(map[string]bool)
	for _, target := range targets {
		if templateSeen[target.template] {
			continue
		}
		templateSeen[target.template] = true
		templatesInOrder = append(templatesInOrder, target.template)
	}
	allDeps := buildDepsMap(cfg)
	selected := make(map[string]bool, len(templatesInOrder))
	for _, template := range templatesInOrder {
		selected[template] = true
	}
	deps := make(map[string][]string, len(templatesInOrder))
	for _, template := range templatesInOrder {
		deps[template] = reachableSelectedDependencies(template, allDeps, selected)
	}
	templateWave, ok := dependencyTemplateWaveOrder(templatesInOrder, deps)
	if !ok {
		return strictSerialWaveOrder(targets), false
	}
	maxWave := 0
	for _, wave := range templateWave {
		if wave > maxWave {
			maxWave = wave
		}
	}
	targetWave := make(map[int]int, len(targets))
	for idx, target := range targets {
		targetWave[idx] = maxWave - templateWave[target.template]
	}
	return targetWave, true
}

func reachableSelectedDependencies(
	template string,
	deps map[string][]string,
	selected map[string]bool,
) []string {
	var reachable []string
	seen := make(map[string]bool)
	added := make(map[string]bool)
	var visit func(string)
	visit = func(current string) {
		for _, dep := range deps[current] {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			if selected[dep] && !added[dep] {
				reachable = append(reachable, dep)
				added[dep] = true
			}
			visit(dep)
		}
	}
	visit(template)
	return reachable
}

func executeTargetWave(
	targets []stopTarget,
	maxParallel int,
	run func(stopTarget) error,
) []stopResult {
	if len(targets) == 0 {
		return nil
	}
	if maxParallel <= 0 {
		maxParallel = 1
	}
	results := make([]stopResult, len(targets))
	sem := make(chan struct{}, maxParallel)
	done := make(chan int, len(targets))
	for i, target := range targets {
		i, target := i, target
		sem <- struct{}{}
		go func() {
			started := time.Now()
			defer func() {
				if recovered := recover(); recovered != nil {
					stack := debug.Stack()
					results[i] = stopResult{
						target:   target,
						err:      fmt.Errorf("panic during lifecycle op: %v\n%s", recovered, stack),
						outcome:  "panic_recovered",
						started:  started,
						finished: time.Now(),
					}
				}
				<-sem
				done <- i
			}()
			err := run(target)
			finished := time.Now()
			outcome := "success"
			if err != nil {
				outcome = "provider_error"
			}
			results[i] = stopResult{
				target:   target,
				err:      err,
				outcome:  outcome,
				started:  started,
				finished: finished,
			}
		}()
	}
	for range targets {
		<-done
	}
	return results
}

func stopTargetsForNames(names []string, cfg *config.City, store beads.Store, stderr io.Writer) []stopTarget {
	sessionTemplates := make(map[string]string)
	sessionSubjects := make(map[string]string)
	sessionPoolManaged := make(map[string]bool)
	if store != nil {
		if sessionBeads, err := loadSessionBeads(store); err == nil {
			for _, bead := range sessionBeads {
				name := bead.Metadata["session_name"]
				template := normalizedSessionTemplate(bead, cfg)
				if name != "" && template != "" {
					sessionTemplates[name] = template
				}
				if name != "" {
					subject := sessionBeadAgentName(bead)
					if subject == "" && template != "" && bead.Metadata["pool_slot"] != "" {
						subject = template + "-" + bead.Metadata["pool_slot"]
					}
					if subject == "" {
						subject = template
					}
					if subject == "" {
						subject = name
					}
					sessionSubjects[name] = subject
					if isPoolManagedSessionBead(bead) {
						sessionPoolManaged[name] = true
					}
				}
			}
		} else if stderr != nil {
			fmt.Fprintf(stderr, "gc lifecycle: session bead lookup degraded to legacy session-name resolution: %v\n", err) //nolint:errcheck
		}
	}
	targets := make([]stopTarget, 0, len(names))
	for idx, name := range names {
		template := sessionTemplates[name]
		resolved := false
		if template == "" {
			candidate := resolveAgentTemplate(name, cfg)
			if candidate != "" && findAgentByTemplate(cfg, candidate) != nil {
				template = candidate
				resolved = true
			}
		} else if findAgentByTemplate(cfg, template) != nil {
			resolved = true
		}
		subject := sessionSubjects[name]
		if subject == "" {
			if template != "" {
				subject = template
			} else {
				subject = name
			}
		}
		targets = append(targets, stopTarget{
			name:        name,
			template:    template,
			subject:     subject,
			order:       idx,
			resolved:    resolved,
			poolManaged: sessionPoolManaged[name],
		})
	}
	return targets
}

func shouldLogStopOutcome(target stopTarget, cfg *config.City) bool {
	if cfg != nil {
		return true
	}
	return target.template != "" || target.resolved
}

func filterStopTargets(targets []stopTarget, names []string) []stopTarget {
	if len(names) == 0 {
		return nil
	}
	keep := make(map[string]bool, len(names))
	for _, name := range names {
		keep[name] = true
	}
	filtered := make([]stopTarget, 0, len(names))
	for _, target := range targets {
		if keep[target.name] {
			filtered = append(filtered, target)
		}
	}
	return filtered
}

func interruptTargetsBounded(targets []stopTarget, sp runtime.Provider, stderr io.Writer) int {
	// Pool-managed sessions have no human user, so Claude Code's
	// interactive "What should Claude do instead?" prompt would hang
	// them forever. Stop them immediately instead of interrupting —
	// no metadata to go stale if shutdown is aborted.
	interruptable := make([]stopTarget, 0, len(targets))
	for _, t := range targets {
		if t.poolManaged {
			started := time.Now()
			err := sp.Stop(t.name)
			outcome := "stopped_pool_managed"
			if err != nil {
				outcome = "stop_failed"
			}
			logLifecycleOutcome(stderr, "interrupt", 0, t.name, t.template, outcome, started, time.Now(), err)
			continue
		}
		interruptable = append(interruptable, t)
	}

	sent := 0
	waveStarted := time.Now()
	results := executeTargetWave(interruptable, min(len(interruptable), defaultMaxParallelInterrupts), func(target stopTarget) error {
		return sp.Interrupt(target.name)
	})
	for _, result := range results {
		logLifecycleOutcome(stderr, "interrupt", 0, result.target.name, result.target.template, result.outcome, result.started, result.finished, result.err)
		if result.err == nil {
			sent++
		}
	}
	logLifecycleWave(stderr, "interrupt", 0, waveStarted, len(interruptable))
	return sent
}

func interruptSessionsBounded(names []string, cfg *config.City, store beads.Store, sp runtime.Provider, stderr io.Writer) int {
	return interruptTargetsBounded(stopTargetsForNames(names, cfg, store, stderr), sp, stderr)
}

func stopTargetsBounded(
	targets []stopTarget,
	cfg *config.City,
	sp runtime.Provider,
	rec events.Recorder,
	actor string,
	stdout, stderr io.Writer,
) int {
	for _, target := range targets {
		if !target.resolved {
			if cfg != nil {
				fmt.Fprintln(stderr, "session lifecycle: unresolved stop target template; falling back to serial stop order") //nolint:errcheck
			}
			stopped := 0
			for wave, target := range targets {
				waveStarted := time.Now()
				results := executeTargetWave([]stopTarget{target}, 1, func(target stopTarget) error {
					return sp.Stop(target.name)
				})
				for _, result := range results {
					if shouldLogStopOutcome(result.target, cfg) {
						logLifecycleOutcome(stderr, "stop", wave, result.target.name, result.target.template, result.outcome, result.started, result.finished, result.err)
					}
					if result.err != nil {
						fmt.Fprintf(stderr, "gc stop: stopping %s: %s\n", result.target.name, formatLifecycleError(result.err)) //nolint:errcheck
						continue
					}
					fmt.Fprintf(stdout, "Stopped agent '%s'\n", result.target.name) //nolint:errcheck
					stopped++
					rec.Record(events.Event{
						Type: events.SessionStopped, Actor: actor, Subject: result.target.subject,
					})
				}
				logLifecycleWave(stderr, "stop", wave, waveStarted, 1)
			}
			return stopped
		}
	}

	waveByTarget, ok := stopWaveOrder(targets, cfg)
	if !ok {
		fmt.Fprintln(stderr, "session lifecycle: dependency graph fallback to serial stop order") //nolint:errcheck
	}
	maxWave := -1
	for _, wave := range waveByTarget {
		if wave > maxWave {
			maxWave = wave
		}
	}
	stopped := 0
	for wave := 0; wave <= maxWave; wave++ {
		waveStarted := time.Now()
		var waveTargets []stopTarget
		for idx, target := range targets {
			if waveByTarget[idx] == wave {
				waveTargets = append(waveTargets, target)
			}
		}
		results := executeTargetWave(waveTargets, defaultMaxParallelStopsPerWave, func(target stopTarget) error {
			return sp.Stop(target.name)
		})
		for _, result := range results {
			if shouldLogStopOutcome(result.target, cfg) {
				logLifecycleOutcome(stderr, "stop", wave, result.target.name, result.target.template, result.outcome, result.started, result.finished, result.err)
			}
			if result.err != nil {
				fmt.Fprintf(stderr, "gc stop: stopping %s: %s\n", result.target.name, formatLifecycleError(result.err)) //nolint:errcheck
				continue
			}
			fmt.Fprintf(stdout, "Stopped agent '%s'\n", result.target.name) //nolint:errcheck
			stopped++
			rec.Record(events.Event{
				Type: events.SessionStopped, Actor: actor, Subject: result.target.subject,
			})
		}
		logLifecycleWave(stderr, "stop", wave, waveStarted, len(waveTargets))
	}
	return stopped
}

func stopSessionsBounded(
	names []string,
	cfg *config.City,
	store beads.Store,
	sp runtime.Provider,
	rec events.Recorder,
	actor string,
	stdout, stderr io.Writer,
) int {
	return stopTargetsBounded(stopTargetsForNames(names, cfg, store, stderr), cfg, sp, rec, actor, stdout, stderr)
}

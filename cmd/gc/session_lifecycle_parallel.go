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
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/worker"
)

const (
	// Starts spend the configurable MaxWakesPerTick budget across ticks, but
	// preparation stays chunked so flapping dependencies are observed between batches.
	defaultStartDependencyRecheckBatchSize = 3

	// Stops and interrupts are teardown paths, so their parallelism is not
	// derived from the wake budget used for starts.
	defaultMaxParallelStopsPerWave = 3
	defaultMaxParallelInterrupts   = 16

	// staleKeyDetectDelay is how long to wait after starting a session
	// before checking if it died immediately (stale resume key detection).
	// Matches the same constant in internal/session/chat.go.
	staleKeyDetectDelay = 2 * time.Second
)

type asyncStartLimiter struct {
	mu       sync.Mutex
	limit    int
	inFlight int
}

func newAsyncStartLimiter(capacity int) *asyncStartLimiter {
	if capacity <= 0 {
		capacity = 1
	}
	return &asyncStartLimiter{limit: capacity}
}

func (l *asyncStartLimiter) resize(capacity int) {
	if l == nil {
		return
	}
	if capacity <= 0 {
		capacity = 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit = capacity
}

func (l *asyncStartLimiter) capacity() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

func (l *asyncStartLimiter) reserve(ctx context.Context) (func(), bool, string) {
	if l == nil {
		return func() {}, true, ""
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return nil, false, "context_canceled"
		default:
		}
	}
	l.mu.Lock()
	if l.inFlight >= l.limit {
		l.mu.Unlock()
		return nil, false, "deferred_by_async_start_limit"
	}
	l.inFlight++
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.inFlight > 0 {
			l.inFlight--
		}
	}, true, ""
}

func maxParallelStartsPerTick(cfg *config.City) int {
	if cfg == nil {
		return config.DefaultMaxWakesPerTick
	}
	return cfg.Daemon.MaxWakesPerTickOrDefault()
}

func startCandidateHasTemplateDependencies(candidate startCandidate, cfg *config.City) bool {
	cfgAgent := findAgentByTemplate(cfg, candidate.logicalTemplate(cfg))
	return cfgAgent != nil && len(cfgAgent.DependsOn) > 0
}

func asyncStartBatchNeedsFollowUp(candidates []startCandidate, cfg *config.City) bool {
	for _, candidate := range candidates {
		if startCandidateHasTemplateDependencies(candidate, cfg) {
			return true
		}
	}
	return false
}

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
	rateLimitScreen bool
}

type startExecutionOptions struct {
	async         bool
	asyncFollowUp func()
	asyncLimiter  *asyncStartLimiter
	asyncTracker  *asyncStartTracker
}

type startExecutionOption func(*startExecutionOptions)

func withAsyncStartExecution() startExecutionOption {
	return func(opts *startExecutionOptions) {
		opts.async = true
	}
}

func withAsyncStartFollowUp(fn func()) startExecutionOption {
	return func(opts *startExecutionOptions) {
		opts.asyncFollowUp = fn
	}
}

func withAsyncStartLimiter(limiter *asyncStartLimiter) startExecutionOption {
	return func(opts *startExecutionOptions) {
		opts.asyncLimiter = limiter
	}
}

func withAsyncStartTracker(tracker *asyncStartTracker) startExecutionOption {
	return func(opts *startExecutionOptions) {
		opts.asyncTracker = tracker
	}
}

type asyncStartTracker struct {
	mu       sync.Mutex
	wg       sync.WaitGroup
	stopping bool
}

func (t *asyncStartTracker) start() (func(), bool) {
	if t == nil {
		return func() {}, true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopping {
		return nil, false
	}
	t.wg.Add(1)
	return t.wg.Done, true
}

func (t *asyncStartTracker) wait(timeout time.Duration) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	t.stopping = true
	t.mu.Unlock()
	if timeout < 0 {
		t.wg.Wait()
		return true
	}
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	if timeout == 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

type asyncPreparedStart struct {
	item    preparedStart
	release func()
	done    func()
}

type stopTarget struct {
	sessionID   string
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
	clk clock.Clock,
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
			if dependencySessionStartInFlight(store, name, cfg, clk) {
				continue
			}
			if alive, err := workerSessionTargetAliveWithConfig(store, sp, cfg, name, tp.Hints.ProcessNames); err == nil && alive {
				return true
			}
		}
	}
	sessionName := lookupSessionNameOrLegacy(store, cityName, template, cfg.Workspace.SessionTemplate)
	if dependencySessionStartInFlight(store, sessionName, cfg, clk) {
		return false
	}
	depTP := desiredState[sessionName]
	alive, err := workerSessionTargetAliveWithConfig(store, sp, cfg, sessionName, depTP.Hints.ProcessNames)
	return err == nil && alive
}

func dependencySessionStartInFlight(store beads.Store, sessionName string, cfg *config.City, clk clock.Clock) bool {
	sessionName = strings.TrimSpace(sessionName)
	if store == nil || sessionName == "" {
		return false
	}
	matches, err := store.ListByMetadata(map[string]string{"session_name": sessionName}, 0)
	if err != nil {
		return true
	}
	for _, session := range matches {
		if session.Status == "closed" {
			continue
		}
		if !isSessionBead(session) {
			continue
		}
		var startupTimeout time.Duration
		if cfg != nil {
			startupTimeout = cfg.Session.StartupTimeoutDuration()
		}
		if pendingCreateStartInFlight(session, clk, startupTimeout) {
			return true
		}
	}
	return false
}

func isSessionBead(session beads.Bead) bool {
	if session.Type == sessionBeadType {
		return true
	}
	for _, label := range session.Labels {
		if label == sessionBeadLabel {
			return true
		}
	}
	return false
}

func candidateWaveOrder(
	candidates []startCandidate,
	cfg *config.City,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	cityName string,
	store beads.Store,
	clk clock.Clock,
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
			if dependencyTemplateAlive(dep, cfg, desiredState, sp, cityName, store, clk) {
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
	return prepareStartCandidateForCity(candidate, "", "", cfg, nil, store, clk, io.Discard)
}

func prepareStartCandidateForCity(
	candidate startCandidate,
	cityPath string,
	cityName string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	clk clock.Clock,
	stderr io.Writer,
) (*preparedStart, error) {
	session := candidate.session
	if _, _, err := preWakeCommit(session, store, clk); err != nil {
		return nil, err
	}
	candidate = refreshConfiguredNamedStartCandidate(candidate, cityPath, cityName, cfg, sp, store, clk, stderr)
	return buildPreparedStart(candidate, cfg, store)
}

func refreshConfiguredNamedStartCandidate(
	candidate startCandidate,
	cityPath string,
	cityName string,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	clk clock.Clock,
	stderr io.Writer,
) startCandidate {
	if candidate.session == nil || cfg == nil || store == nil || !isNamedSessionBead(*candidate.session) {
		return candidate
	}
	if cityName == "" {
		cityName = config.EffectiveCityName(cfg, "")
	}
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "session reconciler: refreshing named session start %s: listing sessions: %v\n", candidate.name(), err) //nolint:errcheck
		}
		return candidate
	}
	refreshed, err := resolvePreservedConfiguredNamedSessionTemplate(cityPath, cityName, cfg, sp, store, snapshot.Open(), *candidate.session, clk, stderr)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "session reconciler: refreshing named session start %s: %v\n", candidate.name(), err) //nolint:errcheck
		}
		return candidate
	}
	candidate.tp = refreshed
	return candidate
}

func buildPreparedStart(
	candidate startCandidate,
	cfg *config.City,
	store beads.Store,
) (*preparedStart, error) {
	session := candidate.session
	tp := candidate.tp
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
	if sk := session.Metadata["session_key"]; sk != "" && tp.ResolvedProvider != nil && !tp.IsACP {
		firstStart := session.Metadata["started_config_hash"] == ""
		forceFresh := session.Metadata["wake_mode"] == "fresh"
		agentCfg.Command = resolveSessionCommand(agentCfg.Command, sk, tp.ResolvedProvider, firstStart, forceFresh)
	}
	firstStart := session.Metadata["started_config_hash"] == ""
	forceFresh := session.Metadata["wake_mode"] == "fresh"
	if !firstStart && !forceFresh {
		agentCfg.PromptSuffix = ""
		agentCfg.PromptFlag = ""
		agentCfg.Nudge = tp.Hints.Nudge
		if agentCfg.Env != nil {
			delete(agentCfg.Env, startupPromptDeliveredEnv)
		}
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
	beadAlias := strings.TrimSpace(session.Metadata["alias"])
	runtimeEnv := sessionpkg.RuntimeEnvWithSessionContext(
		session.ID,
		candidate.name(),
		beadAlias,
		strings.TrimSpace(session.Metadata["template"]),
		strings.TrimSpace(session.Metadata["session_origin"]),
		generation,
		continuationEpoch,
		instanceToken,
	)
	// When the bead has no alias but the template was identity-stamped
	// (pool workers and dependency floors via setTemplateEnvIdentity),
	// don't let mergeEnv's override-wins semantics clobber the stamped
	// GC_ALIAS with the runtime's empty value. For ordinary sessions the
	// resolver-stamped GC_ALIAS is left to be overwritten by the empty
	// runtime value so the tmux runtime emits `env -u GC_ALIAS` and scrubs
	// any inherited GC_ALIAS from the tmux server.
	if beadAlias == "" && tp.EnvIdentityStamped {
		delete(runtimeEnv, "GC_ALIAS")
	}
	agentCfg.Env = mergeEnv(agentCfg.Env, runtimeEnv)
	if gcProvider := sessionProviderFamily(*session); gcProvider != "" {
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
	store beads.Store,
	startupTimeout time.Duration,
) []startResult {
	return executePreparedStartWaveForCity(ctx, prepared, "", sp, store, nil, startupTimeout, 1)
}

func executePreparedStartWaveForCity(
	ctx context.Context,
	prepared []preparedStart,
	cityPath string,
	sp runtime.Provider,
	store beads.Store,
	cfg *config.City,
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
			defer func() {
				<-sem
				done <- i
			}()
			results[i] = runPreparedStartCandidate(ctx, item, cityPath, sp, store, cfg, startupTimeout)
		}()
	}
	for range prepared {
		<-done
	}
	return results
}

func runPreparedStartCandidate(
	ctx context.Context,
	item preparedStart,
	cityPath string,
	sp runtime.Provider,
	store beads.Store,
	cfg *config.City,
	startupTimeout time.Duration,
) (result startResult) {
	started := time.Now()
	result = startResult{
		prepared: item,
		started:  started,
		finished: started,
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			stack := debug.Stack()
			result = startResult{
				prepared: item,
				err:      fmt.Errorf("panic during start: %v\n%s", recovered, stack),
				outcome:  "panic_recovered",
				started:  started,
				finished: time.Now(),
			}
		}
	}()

	startCtx := ctx
	cancel := func() {}
	if startupTimeout > 0 {
		startCtx, cancel = context.WithTimeout(ctx, startupTimeout)
	}
	defer cancel()
	_, err := startPreparedStartCandidate(startCtx, item, cityPath, store, sp, cfg)
	if err != nil && errors.Is(err, sessionpkg.ErrStateSync) {
		running, runningErr := workerSessionTargetRunningWithConfig(cityPath, store, sp, cfg, item.candidate.name())
		if runningErr == nil && running {
			err = nil
		}
	}
	// Stale session key detection: if the session was started
	// with a resume flag but dies immediately, the session key
	// likely references a conversation that no longer exists
	// (e.g., "No conversation found"). Report as a failure so
	// recordWakeFailure clears the key for the next attempt.
	if err == nil && item.candidate.session != nil && item.candidate.session.Metadata["session_key"] != "" {
		time.Sleep(staleKeyDetectDelay)
		running := false
		alive := false
		if store == nil || strings.TrimSpace(item.candidate.session.ID) == "" {
			running = sp != nil && sp.IsRunning(item.candidate.name())
			alive = running && (sp == nil || sp.ProcessAlive(item.candidate.name(), item.cfg.ProcessNames))
		} else {
			var obs worker.LiveObservation
			obs, err = workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, item.candidate.name(), item.cfg.ProcessNames)
			running = obs.Running
			alive = obs.Alive
		}
		if err != nil || !running || !alive {
			err = fmt.Errorf("session %q died during startup", item.candidate.name())
		}
	}
	finished := time.Now()
	rollbackPending := err != nil && shouldRollbackPendingCreate(item.candidate.session)
	rateLimitScreen := err != nil && startupRateLimitScreenDetected(item, cityPath, sp, store, cfg)
	if err != nil && rollbackPending && !rateLimitScreen && runningSessionMatchesPendingCreate(item.candidate.session, item.candidate.name(), sp) {
		return startResult{
			prepared:        item,
			err:             nil,
			outcome:         "start_error_converged",
			started:         started,
			finished:        finished,
			rollbackPending: false,
		}
	}
	var outcome string
	switch {
	case startCtx.Err() == context.DeadlineExceeded:
		outcome = "deadline_exceeded"
		if err == nil {
			err = fmt.Errorf("resuming session: %w", context.DeadlineExceeded)
		}
	case startCtx.Err() == context.Canceled:
		outcome = "canceled"
		if err == nil {
			err = fmt.Errorf("resuming session: %w", context.Canceled)
		}
	case err == nil:
		outcome = "success"
	case errors.Is(err, runtime.ErrSessionInitializing):
		outcome = "session_initializing"
		err = nil
	case errors.Is(err, runtime.ErrSessionExists):
		running, runningErr := workerSessionTargetRunningWithConfig(cityPath, store, sp, cfg, item.candidate.name())
		switch {
		case runningErr != nil || !running:
			outcome = "provider_error"
		case rollbackPending && !rateLimitScreen && runningSessionMatchesPendingCreate(item.candidate.session, item.candidate.name(), sp):
			outcome = "session_exists_converged"
			err = nil
			rollbackPending = false
		default:
			outcome = "session_exists"
		}
	default:
		outcome = "provider_error"
	}
	if err == nil {
		rateLimitScreen = false
	}
	return startResult{
		prepared:        item,
		err:             err,
		outcome:         outcome,
		started:         started,
		finished:        finished,
		rollbackPending: rollbackPending,
		rateLimitScreen: rateLimitScreen,
	}
}

func startupRateLimitScreenDetected(
	item preparedStart,
	cityPath string,
	sp runtime.Provider,
	store beads.Store,
	cfg *config.City,
) bool {
	if item.candidate.session == nil {
		return false
	}
	if cfg != nil && cfg.Session.Provider == "subprocess" {
		return false
	}
	lastWoke := item.candidate.session.Metadata["last_woke_at"]
	if lastWoke == "" {
		return false
	}
	if _, err := time.Parse(time.RFC3339, lastWoke); err != nil {
		return false
	}
	content, err := workerSessionTargetPeekWithConfig(
		cityPath,
		store,
		sp,
		cfg,
		item.candidate.name(),
		rateLimitPeekLines,
		item.cfg.ProcessNames,
	)
	return err == nil && runtime.ContainsProviderRateLimitScreen(content)
}

func enqueuePreparedStartWaveForCity(
	ctx context.Context,
	prepared []asyncPreparedStart,
	cityPath string,
	sp runtime.Provider,
	store beads.Store,
	cfg *config.City,
	clk clock.Clock,
	rec events.Recorder,
	startupTimeout time.Duration,
	wave int,
	stdout, stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
	asyncFollowUp func(),
) []startResult {
	if len(prepared) == 0 {
		return nil
	}
	results := make([]startResult, len(prepared))
	for i, reserved := range prepared {
		item := clonePreparedStartForAsync(reserved.item)
		release := reserved.release
		now := time.Now()
		results[i] = startResult{
			prepared: item,
			outcome:  "start_enqueued",
			started:  now,
			finished: now,
		}
		done := reserved.done
		go func(item preparedStart, release func(), done func()) {
			if done != nil {
				defer done()
			}
			if release != nil {
				defer release()
			}
			result := runPreparedStartCandidate(ctx, item, cityPath, sp, store, cfg, startupTimeout)
			commitAsyncStartResultWithContext(ctx, result, sp, store, clk, rec, wave, stdout, stderr, trace)
			if asyncFollowUp != nil {
				asyncFollowUp()
			}
		}(item, release, done)
	}
	return results
}

func reserveAsyncStartSlot(ctx context.Context, limiter *asyncStartLimiter) (func(), bool, string) {
	return limiter.reserve(ctx)
}

func commitAsyncStartResultWithContext(
	ctx context.Context,
	result startResult,
	sp runtime.Provider,
	store beads.Store,
	clk clock.Clock,
	rec events.Recorder,
	wave int,
	stdout, stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
) (committed bool) {
	name := result.prepared.candidate.name()
	template := result.prepared.candidate.tp.TemplateName
	defer func() {
		if trace != nil {
			_ = trace.flushCurrentBatch(TraceDurabilityDurable)
		}
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			err := fmt.Errorf("panic during async start commit: %v\n%s", recovered, debug.Stack())
			clearPendingStartInFlightLease(result.prepared.candidate.session, store, stderr)
			fmt.Fprintf(stderr, "session reconciler: committing async start %s: %s\n", name, formatLifecycleError(err)) //nolint:errcheck
			logLifecycleOutcome(stderr, "start", wave, name, template, "panic_recovered", result.started, time.Now(), err)
			committed = false
		}
	}()

	refreshed, ok, cleanupRuntime, releaseInFlight := refreshAsyncStartResult(result, store, stderr)
	if !ok {
		if cleanupRuntime {
			stopStaleAsyncStartRuntime(result, sp, stderr)
		}
		outcome := "stale_async_start"
		if releaseInFlight {
			clearPendingStartInFlightLease(result.prepared.candidate.session, store, stderr)
			outcome = "async_start_refresh_failed"
		}
		logLifecycleOutcome(stderr, "start", wave, name, template, outcome, result.started, time.Now(), nil)
		return false
	}
	if refreshed.err != nil && refreshed.rollbackPending && runningSessionMatchesPendingCreate(refreshed.prepared.candidate.session, refreshed.prepared.candidate.name(), sp) {
		refreshed.err = nil
		refreshed.outcome = "session_exists_converged"
		refreshed.rollbackPending = false
	}
	if ctx != nil && ctx.Err() != nil {
		if refreshed.err != nil && refreshed.rollbackPending {
			return commitStartResultTraced(refreshed, store, clk, rec, wave, stdout, stderr, trace)
		}
		if refreshed.err == nil && shouldRollbackPendingCreate(refreshed.prepared.candidate.session) {
			stopStaleAsyncStartRuntime(refreshed, sp, stderr)
			clearPendingStartInFlightLease(refreshed.prepared.candidate.session, store, stderr)
		}
		logLifecycleOutcome(stderr, "start", wave, name, template, "context_canceled", refreshed.started, time.Now(), ctx.Err())
		return false
	}
	if sp != nil && refreshed.err == nil && refreshed.outcome != "session_initializing" {
		clearReconcilerDrainAckMetadata(sp, refreshed.prepared.candidate.name())
	}
	return commitStartResultTraced(refreshed, store, clk, rec, wave, stdout, stderr, trace)
}

func refreshAsyncStartResult(result startResult, store beads.Store, stderr io.Writer) (startResult, bool, bool, bool) {
	session := result.prepared.candidate.session
	if store == nil || session == nil || strings.TrimSpace(session.ID) == "" {
		return result, true, false, false
	}
	current, err := store.Get(session.ID)
	if err != nil {
		fmt.Fprintf(stderr, "session reconciler: refreshing async start %s: %v\n", result.prepared.candidate.name(), err) //nolint:errcheck
		return result, false, false, true
	}
	if asyncStartPreparedCommandStale(result.prepared, current) {
		fmt.Fprintf(stderr, "session reconciler: ignoring stale async start result for %s: desired command changed during startup\n", result.prepared.candidate.name()) //nolint:errcheck
		return result, false, true, true
	}
	if !asyncStartSessionStillCurrent(*session, current) {
		fmt.Fprintf(stderr, "session reconciler: ignoring stale async start result for %s\n", result.prepared.candidate.name()) //nolint:errcheck
		return result, false, asyncStartStaleRuntimeCleanupAllowed(*session, current), false
	}
	result.prepared.candidate.session = &current
	return result, true, false, false
}

func asyncStartPreparedCommandStale(prepared preparedStart, current beads.Bead) bool {
	preparedCommand := strings.TrimSpace(prepared.candidate.tp.Command)
	currentCommand := strings.TrimSpace(current.Metadata["command"])
	return preparedCommand != "" && currentCommand != "" && preparedCommand != currentCommand
}

func clearPendingStartInFlightLease(session *beads.Bead, store beads.Store, stderr io.Writer) {
	if session == nil || store == nil {
		return
	}
	if setMeta(store, session.ID, "last_woke_at", "", stderr) == nil {
		if session.Metadata == nil {
			session.Metadata = make(map[string]string)
		}
		session.Metadata["last_woke_at"] = ""
	}
}

func stopStaleAsyncStartRuntime(result startResult, sp runtime.Provider, stderr io.Writer) {
	if sp == nil || result.prepared.candidate.session == nil {
		return
	}
	name := result.prepared.candidate.name()
	if !runningSessionMatchesPendingCreate(result.prepared.candidate.session, name, sp) {
		return
	}
	if err := sp.Stop(name); err != nil && !runtime.IsSessionGone(err) {
		fmt.Fprintf(stderr, "session reconciler: stopping stale async start runtime %s: %v\n", name, err) //nolint:errcheck
	}
}

// asyncStartSessionStillCurrent decides whether an async start result should
// commit against the current bead. Identity is established by instance_token:
// when the prepared and current tokens both exist and match, the bead is the
// same session we spawned for, even if the generation has been bumped by a
// concurrent reconciler phase (which is normal when a wave runs long enough
// for other phases to write metadata between enqueue and result completion).
//
// Rejecting on generation drift alone caused stuck-creating zombies: the
// process spawned successfully, but the result was discarded as "stale", so
// pending_create_claim never cleared and the session never advanced past
// state=creating. Falling back to generation only when the token is absent
// preserves the prior behavior for callers that pre-date instance_token.
func asyncStartSessionStillCurrent(prepared, current beads.Bead) bool {
	if strings.TrimSpace(current.Status) == "closed" {
		return false
	}
	if !asyncStartIdentityMatches(prepared, current) {
		return false
	}
	currentState := sessionpkg.State(strings.TrimSpace(current.Metadata["state"]))
	// If the bead has progressed to a live state (active or awake), the spawn
	// already succeeded and another phase (typically ensureRunning via attach)
	// has cleared pending_create_claim. The async result still carries useful
	// metadata (creation_complete_at, runtime_epoch, etc.) — commit it instead
	// of discarding as "stale", which leaves the bead missing fields the rest
	// of the system relies on.
	if currentState == sessionpkg.StateAwake || currentState == sessionpkg.StateActive {
		return true
	}
	// For sessions still mid-flight (creating/asleep/drained/empty), reject if
	// pending_create_claim was cleared from under us — that means a different
	// reconciler phase already rolled the create back, and our result would
	// stomp on its decision.
	if shouldRollbackPendingCreate(&prepared) && !shouldRollbackPendingCreate(&current) {
		return false
	}
	return confirmPendingStart(string(currentState))
}

func asyncStartStaleRuntimeCleanupAllowed(prepared, current beads.Bead) bool {
	if strings.TrimSpace(current.Status) == "closed" {
		return true
	}
	if !asyncStartIdentityMatches(prepared, current) {
		return true
	}
	currentState := sessionpkg.State(strings.TrimSpace(current.Metadata["state"]))
	if shouldRollbackPendingCreate(&prepared) && !shouldRollbackPendingCreate(&current) {
		return currentState != sessionpkg.StateAwake && currentState != sessionpkg.StateActive
	}
	return !confirmPendingStart(string(currentState)) &&
		currentState != sessionpkg.StateAwake &&
		currentState != sessionpkg.StateActive
}

// asyncStartIdentityMatches reports whether prepared and current describe the
// same session bead. instance_token is authoritative when both sides have one;
// only fall back to generation when the prepared bead has no token (legacy
// pre-instance_token snapshots). Generation drift with a matching token is a
// normal consequence of concurrent reconciler phases and must not invalidate
// an in-flight start result.
func asyncStartIdentityMatches(prepared, current beads.Bead) bool {
	preparedToken := strings.TrimSpace(prepared.Metadata["instance_token"])
	if preparedToken != "" {
		return strings.TrimSpace(current.Metadata["instance_token"]) == preparedToken
	}
	preparedGeneration := strings.TrimSpace(prepared.Metadata["generation"])
	if preparedGeneration == "" {
		return true
	}
	return strings.TrimSpace(current.Metadata["generation"]) == preparedGeneration
}

func clonePreparedStartForAsync(item preparedStart) preparedStart {
	if item.candidate.session == nil {
		return item
	}
	sessionCopy := *item.candidate.session
	if item.candidate.session.Labels != nil {
		sessionCopy.Labels = append([]string(nil), item.candidate.session.Labels...)
	}
	if item.candidate.session.Metadata != nil {
		sessionCopy.Metadata = make(map[string]string, len(item.candidate.session.Metadata))
		for key, value := range item.candidate.session.Metadata {
			sessionCopy.Metadata[key] = value
		}
	}
	item.candidate.session = &sessionCopy
	return item
}

func startPreparedStartCandidate(
	ctx context.Context,
	item preparedStart,
	cityPath string,
	store beads.Store,
	sp runtime.Provider,
	cfg *config.City,
) (bool, error) {
	if store == nil || item.candidate.session == nil || strings.TrimSpace(item.candidate.session.ID) == "" {
		handle, err := runtimeWorkerHandleWithConfig(
			cityPath,
			store,
			sp,
			cfg,
			item.candidate.name(),
			item.candidate.name(),
			"",
			nil,
		)
		if err != nil {
			return false, err
		}
		return true, handle.StartResolved(ctx, item.cfg.Command, item.cfg)
	}
	handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, item.candidate.session.ID)
	if err != nil {
		return true, err
	}
	return true, handle.StartResolved(ctx, item.cfg.Command, item.cfg)
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
		clearPendingStartInFlightLease(session, store, stderr)
		logLifecycleOutcome(stderr, "start", wave, name, tp.TemplateName, result.outcome, result.started, result.finished, nil)
		return false
	}
	if result.err != nil {
		fmt.Fprintf(stderr, "session reconciler: starting %s: %s\n", name, formatLifecycleError(result.err)) //nolint:errcheck
		if result.rateLimitScreen {
			if err := recordRateLimitQuarantine(session, store, clk); err != nil {
				fmt.Fprintf(stderr, "session reconciler: recording startup rate-limit hold for %s: %v\n", name, err) //nolint:errcheck
				if trace != nil {
					trace.recordOperation("reconciler.start.rate_limit_hold", tp.TemplateName, name, "", "start", "hold_deferred", traceRecordPayload{
						"error": formatLifecycleError(result.err),
						"cause": err.Error(),
					}, "")
				}
				logLifecycleOutcome(stderr, "start", wave, name, tp.TemplateName, result.outcome, result.started, result.finished, result.err)
				return false
			}
			if trace != nil {
				trace.recordOperation("reconciler.start.rate_limit_hold", tp.TemplateName, name, "", "start", "held", traceRecordPayload{
					"error": formatLifecycleError(result.err),
				}, "")
			}
			logLifecycleOutcome(stderr, "start", wave, name, tp.TemplateName, result.outcome, result.started, result.finished, result.err)
			return false
		}
		if result.rollbackPending {
			if trace != nil {
				trace.recordOperation("reconciler.start.rollback_pending", tp.TemplateName, name, "", "start", result.outcome, traceRecordPayload{
					"error": formatLifecycleError(result.err),
				}, "")
			}
			rollbackPendingCreate(session, store, clk.Now().UTC(), stderr)
			logLifecycleOutcome(stderr, "start", wave, name, tp.TemplateName, result.outcome, result.started, result.finished, result.err)
			return false
		}
		if err := store.SetMetadata(session.ID, "last_woke_at", ""); err != nil {
			fmt.Fprintf(stderr, "session reconciler: clearing last_woke_at for %s: %v\n", name, err) //nolint:errcheck
		} else {
			session.Metadata["last_woke_at"] = ""
		}
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
	coreBreakdown := ""
	if bdj, err := json.Marshal(result.prepared.coreBreakdown); err == nil {
		coreBreakdown = string(bdj)
	}
	// Transition creating/asleep/drained beads to active once the runtime
	// spawn has confirmed. Folded into this metadata batch so the state
	// write is atomic with the hash writes, the pending_create_claim
	// clear, and the creation_complete_at marker. This prevents the sweep
	// from observing a transient state where the claim is gone but the
	// post-create marker hasn't landed yet. See confirmPendingStart for
	// the state gate.
	metadata := sessionpkg.CommitStartedPatch(sessionpkg.CommitStartedPatchInput{
		CoreHash:                result.prepared.coreHash,
		LiveHash:                result.prepared.liveHash,
		CoreBreakdown:           coreBreakdown,
		ConfirmState:            confirmPendingStart(session.Metadata["state"]),
		ClearSleepReason:        session.Metadata["sleep_reason"] != "",
		ClearPendingCreateClaim: shouldRollbackPendingCreate(session),
		Now:                     clk.Now(),
	})
	storedMCPSnapshot, err := sessionpkg.EncodeMCPServersSnapshot(result.prepared.cfg.MCPServers)
	if err != nil {
		clearPendingStartInFlightLease(session, store, stderr)
		fmt.Fprintf(stderr, "session reconciler: encoding MCP snapshot for %s: %v\n", name, err) //nolint:errcheck
		logLifecycleOutcome(stderr, "start", wave, name, tp.TemplateName, "metadata_encode_failed", result.started, result.finished, err)
		return false
	}
	if storedMCPSnapshot != "" || session.Metadata[sessionpkg.MCPServersSnapshotMetadataKey] != "" {
		metadata[sessionpkg.MCPServersSnapshotMetadataKey] = storedMCPSnapshot
	}
	if err := sessionpkg.PersistRuntimeMCPServersSnapshot(result.prepared.cfg.Env["GC_CITY_PATH"], session.ID, result.prepared.cfg.MCPServers); err != nil {
		clearPendingStartInFlightLease(session, store, stderr)
		fmt.Fprintf(stderr, "session reconciler: storing runtime MCP snapshot for %s: %v\n", name, err) //nolint:errcheck
		logLifecycleOutcome(stderr, "start", wave, name, tp.TemplateName, "runtime_mcp_snapshot_failed", result.started, result.finished, err)
		return false
	}
	if result.prepared.candidate.tp.IsACP ||
		session.Metadata[sessionpkg.MCPIdentityMetadataKey] != "" ||
		session.Metadata[sessionpkg.MCPServersSnapshotMetadataKey] != "" {
		storedMCPIdentity := firstNonEmptyGCString(
			session.Metadata[sessionpkg.MCPIdentityMetadataKey],
			session.Metadata[sessionpkg.NamedSessionIdentityMetadata],
			session.Metadata["agent_name"],
		)
		if storedMCPIdentity != "" || session.Metadata[sessionpkg.MCPIdentityMetadataKey] != "" {
			metadata[sessionpkg.MCPIdentityMetadataKey] = storedMCPIdentity
		}
	}
	if err := store.SetMetadataBatch(session.ID, metadata); err != nil {
		clearPendingStartInFlightLease(session, store, stderr)
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

func recoverRunningPendingCreate(
	session *beads.Bead,
	tp TemplateParams,
	cfg *config.City,
	store beads.Store,
	clk clock.Clock,
	trace *sessionReconcilerTraceCycle,
) bool {
	if session == nil || store == nil {
		return false
	}
	prepared, err := buildPreparedStart(startCandidate{session: session, tp: tp}, cfg, store)
	if err != nil {
		if trace != nil {
			trace.recordDecision("reconciler.session.pending_create", tp.TemplateName, tp.SessionName, "pending_create_rebuild_failed", "failed", traceRecordPayload{
				"error": err.Error(),
			}, nil, "")
		}
		return false
	}
	coreBreakdown := ""
	if bdj, err := json.Marshal(prepared.coreBreakdown); err == nil {
		coreBreakdown = string(bdj)
	}
	// Fall back to wall clock if the caller didn't inject one — the marker
	// is load-bearing for the post-create sweep guard, so leaving it unset
	// would re-open the crash/recovery spin-loop window.
	var now time.Time
	if clk != nil {
		now = clk.Now()
	} else {
		now = time.Now()
	}
	metadata := sessionpkg.CommitStartedPatch(sessionpkg.CommitStartedPatchInput{
		CoreHash:      prepared.coreHash,
		LiveHash:      prepared.liveHash,
		CoreBreakdown: coreBreakdown,
		ConfirmState: confirmPendingStart(session.Metadata["state"]) ||
			sessionpkg.State(strings.TrimSpace(session.Metadata["state"])) == sessionpkg.StateAwake,
		ClearSleepReason: session.Metadata["sleep_reason"] != "",
		// recoverRunningPendingCreate's caller (session_reconciler.go)
		// already gates entry on shouldRollbackPendingCreate(session), so
		// at this point the claim is guaranteed to be set — hard-code the
		// clear rather than re-evaluating the same predicate.
		ClearPendingCreateClaim: true,
		Now:                     now,
	})
	if err := store.SetMetadataBatch(session.ID, metadata); err != nil {
		if trace != nil {
			trace.recordDecision("reconciler.session.pending_create", tp.TemplateName, tp.SessionName, "pending_create_commit_failed", "failed", traceRecordPayload{
				"error": err.Error(),
			}, nil, "")
		}
		return false
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(metadata))
	}
	for key, value := range metadata {
		session.Metadata[key] = value
	}
	if trace != nil {
		trace.recordDecision("reconciler.session.pending_create", tp.TemplateName, tp.SessionName, "pending_create_healed", "healed", nil, nil, "")
	}
	return true
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
	liveID := ""
	if value, err := sp.GetMeta(sessionName, "GC_SESSION_ID"); err == nil {
		liveID = strings.TrimSpace(value)
		if liveID != "" && liveID != session.ID {
			return false
		}
	}
	expectedToken := strings.TrimSpace(session.Metadata["instance_token"])
	liveToken := ""
	if value, err := sp.GetMeta(sessionName, "GC_INSTANCE_TOKEN"); err == nil {
		liveToken = value
		liveToken = strings.TrimSpace(liveToken)
		if liveToken != "" && liveToken != expectedToken {
			liveGeneration, _ := sp.GetMeta(sessionName, "GC_RUNTIME_EPOCH")
			expectedGeneration := strings.TrimSpace(session.Metadata["generation"])
			if strings.TrimSpace(liveGeneration) != "" && expectedGeneration != "" && strings.TrimSpace(liveGeneration) != expectedGeneration {
				return false
			}
			if liveID == "" {
				return false
			}
		}
	}
	if liveID != "" {
		return liveID == session.ID
	}
	if expectedToken == "" {
		return false
	}
	return expectedToken != "" && liveToken == expectedToken
}

func rollbackPendingCreate(session *beads.Bead, store beads.Store, now time.Time, stderr io.Writer) {
	if session == nil || store == nil {
		return
	}
	clearPendingStartInFlightLease(session, store, stderr)
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
	return executePlannedStartsTraced(ctx, candidates, cfg, desiredState, sp, store, cityName, "", clk, rec, startupTimeout, stdout, stderr, nil)
}

func executePlannedStartsTraced(
	ctx context.Context,
	candidates []startCandidate,
	cfg *config.City,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	store beads.Store,
	cityName string,
	cityPath string,
	clk clock.Clock,
	rec events.Recorder,
	startupTimeout time.Duration,
	stdout, stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
	options ...startExecutionOption,
) int {
	if len(candidates) == 0 {
		return 0
	}
	startOpts := startExecutionOptions{}
	for _, apply := range options {
		if apply != nil {
			apply(&startOpts)
		}
	}
	cbCfg, cbEnabled := sessionCircuitBreakerConfigFromCity(cfg)
	var cb *sessionCircuitBreaker
	if cbEnabled {
		cb = defaultSessionCircuitBreaker()
		cb.configure(cbCfg)
	}
	asyncLimiter := startOpts.asyncLimiter
	maxWakes := maxParallelStartsPerTick(cfg)
	if startOpts.async && asyncLimiter == nil {
		asyncLimiter = newAsyncStartLimiter(maxWakes)
	}
	waveByCandidate, ok := candidateWaveOrder(candidates, cfg, desiredState, sp, cityName, store, clk)
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
		asyncFollowUpRequired := false
		var waveCandidates []startCandidate
		for idx, candidate := range candidates {
			if waveByCandidate[idx] == wave {
				waveCandidates = append(waveCandidates, candidate)
			}
		}
		if len(waveCandidates) == 0 {
			continue
		}
		if wakeCount >= maxWakes {
			for _, candidate := range waveCandidates {
				logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), "deferred_by_wake_budget", time.Time{}, time.Time{}, nil)
			}
			continue
		}
		var ready []startCandidate
		for _, candidate := range waveCandidates {
			if !allDependenciesAliveForTemplateWithClock(candidate.logicalTemplate(cfg), cfg, desiredState, sp, cityName, store, clk) {
				logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), "blocked_on_dependencies", time.Time{}, time.Time{}, nil)
				continue
			}
			ready = append(ready, candidate)
		}
		for offset := 0; offset < len(ready); {
			if wakeCount >= maxWakes {
				for _, candidate := range ready[offset:] {
					logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), "deferred_by_wake_budget", time.Time{}, time.Time{}, nil)
				}
				break
			}
			batchSize := min(defaultStartDependencyRecheckBatchSize, maxWakes-wakeCount)
			end := min(offset+batchSize, len(ready))
			batchCandidates := ready[offset:end]
			var prepared []preparedStart
			var asyncPrepared []asyncPreparedStart
			for _, candidate := range batchCandidates {
				if !allDependenciesAliveForTemplateWithClock(candidate.logicalTemplate(cfg), cfg, desiredState, sp, cityName, store, clk) {
					logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), "blocked_on_dependencies", time.Time{}, time.Time{}, nil)
					continue
				}
				var release func()
				var done func()
				if startOpts.async {
					var tracking bool
					done, tracking = startOpts.asyncTracker.start()
					if !tracking {
						logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), "context_canceled", time.Time{}, time.Time{}, nil)
						continue
					}
					var reserved bool
					var outcome string
					release, reserved, outcome = reserveAsyncStartSlot(ctx, asyncLimiter)
					if !reserved {
						done()
						logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), outcome, time.Time{}, time.Time{}, nil)
						continue
					}
				}
				if cbEnabled {
					identity := ""
					if candidate.session != nil {
						identity = namedSessionIdentity(*candidate.session)
					}
					if identity != "" {
						cbNow := clk.Now().UTC()
						if cb.IsOpen(identity, cbNow) {
							if release != nil {
								release()
							}
							if done != nil {
								done()
							}
							if err := persistSessionCircuitBreakerMetadata(store, candidate.session, cb, identity, cbNow); err != nil {
								fmt.Fprintf(stderr, "session reconciler: %v\n", err) //nolint:errcheck // best-effort stderr
							}
							cb.LogOpenOnce(identity, stderr)
							if trace != nil {
								trace.recordDecision("reconciler.session.circuit_open", candidate.tp.TemplateName, candidate.name(), "circuit_open", "skipped", traceRecordPayload{
									"identity": identity,
								}, nil, "")
							}
							continue
						}
						state, err := recordSessionCircuitBreakerRestart(store, candidate.session, cb, identity, cbNow)
						if err != nil {
							if release != nil {
								release()
							}
							if done != nil {
								done()
							}
							fmt.Fprintf(stderr, "session reconciler: %v\n", err) //nolint:errcheck // best-effort stderr
							logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), "circuit_metadata_failed", time.Time{}, time.Time{}, err)
							continue
						}
						if state == circuitOpen {
							if release != nil {
								release()
							}
							if done != nil {
								done()
							}
							cb.LogOpenOnce(identity, stderr)
							if trace != nil {
								trace.recordDecision("reconciler.session.circuit_trip", candidate.tp.TemplateName, candidate.name(), "circuit_trip", "skipped", traceRecordPayload{
									"identity": identity,
								}, nil, "")
							}
							continue
						}
					}
				}
				item, err := prepareStartCandidateForCity(candidate, cityPath, cityName, cfg, sp, store, clk, stderr)
				if err != nil {
					clearPendingStartInFlightLease(candidate.session, store, stderr)
					if release != nil {
						release()
					}
					if done != nil {
						done()
					}
					fmt.Fprintf(stderr, "session reconciler: pre-wake %s: %s\n", candidate.name(), formatLifecycleError(err)) //nolint:errcheck
					logLifecycleOutcome(stderr, "start", wave, candidate.name(), candidate.logicalTemplate(cfg), "failed", time.Time{}, time.Time{}, err)
					continue
				}
				if startOpts.async {
					asyncPrepared = append(asyncPrepared, asyncPreparedStart{item: *item, release: release, done: done})
				} else {
					prepared = append(prepared, *item)
				}
			}
			offset = end
			var results []startResult
			if startOpts.async {
				results = enqueuePreparedStartWaveForCity(ctx, asyncPrepared, cityPath, sp, store, cfg, clk, rec, startupTimeout, wave, stdout, stderr, trace, startOpts.asyncFollowUp)
				if len(results) > 0 && asyncStartBatchNeedsFollowUp(batchCandidates, cfg) {
					asyncFollowUpRequired = true
				}
			} else {
				results = executePreparedStartWaveForCity(ctx, prepared, cityPath, sp, store, cfg, startupTimeout, batchSize)
			}
			for _, result := range results {
				if trace != nil {
					trace.recordOperation("reconciler.start.execute", result.prepared.candidate.tp.TemplateName, result.prepared.candidate.name(), "", "start", result.outcome, traceRecordPayload{
						"rollback_pending": result.rollbackPending,
						"duration_ms":      result.finished.Sub(result.started).Milliseconds(),
					}, "")
				}
				if result.outcome == "start_enqueued" {
					logLifecycleOutcome(stderr, "start", wave, result.prepared.candidate.name(), result.prepared.candidate.logicalTemplate(cfg), result.outcome, result.started, result.finished, nil)
					wakeCount++
					continue
				}
				if result.err == nil && result.outcome != "session_initializing" {
					clearReconcilerDrainAckMetadata(sp, result.prepared.candidate.name())
				}
				if commitStartResultTraced(result, store, clk, rec, wave, stdout, stderr, trace) {
					wakeCount++
				}
			}
			if startOpts.async && asyncFollowUpRequired {
				break
			}
		}
		logLifecycleWave(stderr, "start", wave, waveStarted, len(waveCandidates))
		if startOpts.async && asyncFollowUpRequired {
			// Dependency-sensitive async batches yield after enqueueing so the
			// next batch observes committed dependency and pending-create state.
			return wakeCount
		}
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
	sessionIDs := make(map[string]string)
	if store != nil {
		if sessionBeads, err := loadSessionBeads(store); err == nil {
			for _, bead := range sessionBeads {
				name := bead.Metadata["session_name"]
				template := normalizedSessionTemplate(bead, cfg)
				if name != "" && template != "" {
					sessionTemplates[name] = template
				}
				if name != "" {
					sessionIDs[name] = bead.ID
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
			sessionID:   sessionIDs[name],
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

func hydrateStopTargets(targets []stopTarget, cfg *config.City, store beads.Store, stderr io.Writer) []stopTarget {
	if store == nil || len(targets) == 0 {
		return targets
	}
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		if strings.TrimSpace(target.name) == "" {
			continue
		}
		names = append(names, target.name)
	}
	if len(names) == 0 {
		return targets
	}
	hydrated := stopTargetsForNames(names, cfg, store, stderr)
	byName := make(map[string]stopTarget, len(hydrated))
	for _, target := range hydrated {
		byName[target.name] = target
	}
	merged := make([]stopTarget, 0, len(targets))
	for _, target := range targets {
		if hydratedTarget, ok := byName[target.name]; ok {
			if strings.TrimSpace(target.sessionID) == "" {
				target.sessionID = hydratedTarget.sessionID
			}
			if strings.TrimSpace(target.template) == "" {
				target.template = hydratedTarget.template
			}
			if strings.TrimSpace(target.subject) == "" {
				target.subject = hydratedTarget.subject
			}
			if !target.resolved {
				target.resolved = hydratedTarget.resolved
			}
			if !target.poolManaged {
				target.poolManaged = hydratedTarget.poolManaged
			}
		}
		merged = append(merged, target)
	}
	return merged
}

func stopTargetThroughWorkerBoundary(target stopTarget, store beads.Store, sp runtime.Provider, cfg *config.City) error {
	targetID := strings.TrimSpace(target.sessionID)
	if targetID == "" {
		targetID = strings.TrimSpace(target.name)
	}
	if cityStopSessionMarked(store, target.sessionID) {
		if err := workerKillSessionTargetWithConfig("", store, sp, cfg, targetID); err != nil {
			return err
		}
		markCityStopSessionAsAsleep(store, target.sessionID, nil)
		return nil
	}
	return workerStopSessionTargetWithConfig("", store, sp, cfg, targetID)
}

func cityStopSessionMarked(store beads.Store, sessionID string) bool {
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return false
	}
	b, err := store.Get(sessionID)
	if err != nil {
		return false
	}
	return strings.TrimSpace(b.Metadata["sleep_reason"]) == sleepReasonCityStop
}

func markCityStopSessionAsAsleep(store beads.Store, sessionID string, stderr io.Writer) {
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	batch := sessionpkg.SleepPatch(time.Now().UTC(), sleepReasonCityStop)
	if err := store.SetMetadataBatch(sessionID, batch); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "gc stop: marking session %s asleep: %v\n", sessionID, err) //nolint:errcheck
	}
}

func interruptTargetsBounded(targets []stopTarget, cfg *config.City, store beads.Store, sp runtime.Provider, stderr io.Writer) int {
	targets = hydrateStopTargets(targets, cfg, store, stderr)
	// Pool-managed sessions have no human user, so Claude Code's
	// interactive "What should Claude do instead?" prompt would hang
	// them forever. Stop them immediately instead of interrupting —
	// no metadata to go stale if shutdown is aborted.
	interruptable := make([]stopTarget, 0, len(targets))
	for _, t := range targets {
		if t.poolManaged {
			started := time.Now()
			err := stopTargetThroughWorkerBoundary(t, store, sp, cfg)
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
		targetID := strings.TrimSpace(target.sessionID)
		if targetID == "" {
			targetID = strings.TrimSpace(target.name)
		}
		return workerInterruptSessionTargetWithConfig("", store, sp, cfg, targetID)
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
	return interruptTargetsBounded(stopTargetsForNames(names, cfg, store, stderr), cfg, store, sp, stderr)
}

func stopTargetsBounded(
	targets []stopTarget,
	cfg *config.City,
	store beads.Store,
	sp runtime.Provider,
	rec events.Recorder,
	actor string,
	stdout, stderr io.Writer,
) int {
	targets = hydrateStopTargets(targets, cfg, store, stderr)
	for _, target := range targets {
		if !target.resolved {
			if cfg != nil {
				fmt.Fprintln(stderr, "session lifecycle: unresolved stop target template; falling back to serial stop order") //nolint:errcheck
			}
			stopped := 0
			for wave, target := range targets {
				waveStarted := time.Now()
				results := executeTargetWave([]stopTarget{target}, 1, func(target stopTarget) error {
					return stopTargetThroughWorkerBoundary(target, store, sp, cfg)
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
			return stopTargetThroughWorkerBoundary(target, store, sp, cfg)
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
	return stopTargetsBounded(stopTargetsForNames(names, cfg, store, stderr), cfg, store, sp, rec, actor, stdout, stderr)
}

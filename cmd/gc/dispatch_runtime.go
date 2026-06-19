package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphroute"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// cliGraphrouteDeps builds the graphroute dependencies used by CLI
// graph-routing call sites.
func cliGraphrouteDeps(cityPath string) graphroute.Deps {
	return graphroute.Deps{
		CityPath:              cityPath,
		Resolver:              cliAgentResolver{},
		DirectSessionResolver: cliDirectSessionResolver,
		ControlDispatcherRuntimeMissing: func(qualifiedName string) bool {
			return controlDispatcherSessionRuntimeMissing(cityPath, qualifiedName)
		},
	}
}

// controlDispatcherSessionRuntimeMissing reports whether the control-dispatcher
// agent's session is asleep with reason runtime-missing. Session beads are
// city-scoped, so it reads the city store directly (the rig-scoped routing
// store cannot see them). It powers the rig→city control-dispatcher fallback
// (#3454); any lookup failure returns false so routing falls back to the normal
// rig-local binding rather than mis-routing on a transient store error.
func controlDispatcherSessionRuntimeMissing(cityPath, qualifiedName string) bool {
	if cityPath == "" || strings.TrimSpace(qualifiedName) == "" {
		return false
	}
	// Only consult the city store for a real, initialized city. Mirrors the
	// pool-nudge guard in doSling: a bare working dir (no city.toml) has no
	// session beads to read, and opening a store there would needlessly
	// spin up a managed Dolt backend on the routing hot path.
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		return false
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil || store == nil {
		return false
	}
	return sessionRuntimeMissingInStore(store, qualifiedName)
}

// sessionRuntimeMissingInStore reports whether any open session bead for the
// agent (selected by its agent:<qualified> label) projects the runtime-missing
// lifecycle reason in the given store. The projection lives in internal/session
// so the API sling path can share it without importing package main.
func sessionRuntimeMissingInStore(store beads.Store, qualifiedName string) bool {
	return sessionpkg.RuntimeMissingInStore(store, qualifiedName)
}

// applyGraphRouting delegates to graphroute.ApplyGraphRouting with CLI
// dependencies.
func applyGraphRouting(recipe *formula.Recipe, a *config.Agent, routedTo string, vars map[string]string, scopeKind, scopeRef, storeRef string, store beads.Store, cityName, cityPath string, cfg *config.City) error {
	return graphroute.ApplyGraphRouting(recipe, a, routedTo, vars, "", scopeKind, scopeRef, storeRef, store, cityName, cfg, cliGraphrouteDeps(cityPath))
}

var (
	workflowServeList               = nextWorkflowServeBeads
	controlDispatcherServe          = runControlDispatcherInStore
	workflowServeOpenEventsProvider = func(stderr io.Writer) (events.Provider, error) {
		ep, code := openCityEventsProvider(stderr, "gc convoy control --serve")
		if ep == nil {
			return nil, fmt.Errorf("opening events provider (exit %d)", code)
		}
		return ep, nil
	}
	workflowServeIdlePollInterval  = 100 * time.Millisecond
	workflowServeIdlePollAttempts  = 3
	workflowServeWakeSweepInterval = 1 * time.Second
	workflowServeMaxIdleSleep      = 30 * time.Second
	// workflowServeWakeDebounce is the coalescing window opened once the first
	// relevant event wakes the --follow loop. Additional buffered events that
	// arrive during the window are drained and folded into the same wake so a
	// burst of N bead.* events (e.g. an mc-wisp-* event storm) collapses into a
	// single work/ready re-scan instead of N heavy per-event Dolt scans. This is
	// a fixed (max-wait) window, so a lone relevant wake also waits out the
	// window before its drain; the delay is intentional and small relative to
	// the 1–30s idle sleeps it replaces. Set it to 0 to disable coalescing and
	// restore one-event-one-drain. Injectable so tests can shrink it. Fixes
	// gastownhall/gascity#3206.
	workflowServeWakeDebounce = 250 * time.Millisecond
	workflowServeWaitForWake  = waitForRelevantWorkflowWakeWithTrace
	workflowTraceNow          = time.Now
	// The trace helper is intentionally process-global because workflowTracef
	// does not carry per-invocation context. Nested installs (serve ->
	// runControlDispatcherWithStore) reuse the active dedup map so one bad trace
	// path warns once per command invocation instead of once per control bead.
	// The newest installed scope owns the active writer; the most recent scope
	// for a given writer reuses that writer's dedupe map, and out-of-order
	// restores reactivate the newest remaining scope instead of panicking.
	// This assumes top-level callers are nested, not concurrently active from
	// separate goroutines in the same process.
	workflowTraceWarnings = struct {
		mu     sync.Mutex
		writer io.Writer
		warned map[string]struct{}
		scopes []workflowTraceWarningScope
		nextID uint64
	}{
		writer: os.Stderr,
		warned: map[string]struct{}{},
	}
)

// followSleepDuration returns the sleep interval the --follow loop should use
// before its next drain, given how many consecutive idle sweeps have passed.
// The idle sweep count doubles the base interval on each step, capped at
// workflowServeMaxIdleSleep. Fixes gastownhall/gascity#1028.
func followSleepDuration(idleSweeps int) time.Duration {
	if idleSweeps <= 0 {
		return workflowServeWakeSweepInterval
	}
	const maxShift = 30
	shift := idleSweeps
	if shift > maxShift {
		shift = maxShift
	}
	d := workflowServeWakeSweepInterval << uint(shift)
	if d <= 0 || d > workflowServeMaxIdleSleep {
		return workflowServeMaxIdleSleep
	}
	return d
}

const workflowServeScanLimit = 20

// runConvoyControlServe is the entry point for `gc convoy control --serve`.
func runConvoyControlServe(args []string, stdout, stderr io.Writer) error {
	var agentName string
	if len(args) > 0 {
		agentName = args[0]
	}
	if err := runWorkflowServe(agentName, true, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "gc convoy control --serve: %v\n", err) //nolint:errcheck
		return errExit
	}
	return nil
}

type hookBead struct {
	ID       string           `json:"id"`
	Metadata hookBeadMetadata `json:"metadata"`
}

type workflowTraceWarningScope struct {
	id     uint64
	writer io.Writer
	warned map[string]struct{}
}

// hookBeadMetadata handles metadata where values may be JSON strings,
// numbers, or booleans (bd writes numbers for numeric-looking values).
// Normalizes everything to strings on unmarshal.
type hookBeadMetadata map[string]string

func (m *hookBeadMetadata) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = make(hookBeadMetadata, len(raw))
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) == nil {
			(*m)[k] = s
		} else {
			// Non-string (number, bool): use raw JSON text without quotes.
			(*m)[k] = strings.Trim(string(v), " ")
		}
	}
	return nil
}

func workflowTracef(format string, args ...any) {
	path := strings.TrimSpace(os.Getenv("GC_WORKFLOW_TRACE"))
	if path == "" {
		path = strings.TrimSpace(os.Getenv("GC_SLING_TRACE"))
	}
	if path == "" {
		return
	}
	maybeRotateWorkflowTrace(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		workflowTraceWarnOpenFailure(path, err)
		return
	}
	defer f.Close()                                                                                            //nolint:errcheck // best-effort trace log
	fmt.Fprintf(f, "%s %s\n", workflowTraceNow().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...)) //nolint:errcheck
}

func workflowTraceWarnOpenFailure(path string, err error) {
	if strings.TrimSpace(path) == "" || err == nil {
		return
	}
	workflowTraceWarnings.mu.Lock()
	writer := workflowTraceWarnings.writer
	workflowTraceWarnings.mu.Unlock()
	workflowTraceWarnf(writer, "trace-open:"+normalizePathForCompare(path), "gc convoy control --serve: warning: opening workflow trace %q: %v\n", path, err)
}

func workflowTraceWarnf(writer io.Writer, dedupeKey, format string, args ...any) {
	if writer == nil {
		return
	}
	workflowTraceWarnings.mu.Lock()
	warned := workflowTraceWarnings.warned
	if workflowTraceWarnings.writer != writer || warned == nil {
		warned = nil
		for i := len(workflowTraceWarnings.scopes) - 1; i >= 0; i-- {
			if workflowTraceWarnings.scopes[i].writer == writer {
				warned = workflowTraceWarnings.scopes[i].warned
				break
			}
		}
	}
	if warned != nil {
		if _, alreadyWarned := warned[dedupeKey]; alreadyWarned {
			workflowTraceWarnings.mu.Unlock()
			return
		}
		warned[dedupeKey] = struct{}{}
	}
	workflowTraceWarnings.mu.Unlock()
	fmt.Fprintf(writer, format, args...) //nolint:errcheck // best-effort stderr
}

// useWorkflowTraceWarnings installs a per-command warning sink. Nested callers
// that share a writer reuse the same dedupe map so a single command invocation
// warns once per path. Restores may arrive out of order; the newest remaining
// scope stays active so helper reuse cannot panic the process.
func useWorkflowTraceWarnings(writer io.Writer) func() {
	workflowTraceWarnings.mu.Lock()
	workflowTraceWarnings.nextID++
	restoreID := workflowTraceWarnings.nextID
	warned := map[string]struct{}{}
	for i := len(workflowTraceWarnings.scopes) - 1; i >= 0; i-- {
		if workflowTraceWarnings.scopes[i].writer == writer {
			warned = workflowTraceWarnings.scopes[i].warned
			break
		}
	}
	workflowTraceWarnings.scopes = append(workflowTraceWarnings.scopes, workflowTraceWarningScope{
		id:     restoreID,
		writer: writer,
		warned: warned,
	})
	workflowTraceWarnings.writer = writer
	workflowTraceWarnings.warned = warned
	workflowTraceWarnings.mu.Unlock()
	return func() {
		workflowTraceWarnings.mu.Lock()
		defer workflowTraceWarnings.mu.Unlock()
		restoreIdx := -1
		for i := len(workflowTraceWarnings.scopes) - 1; i >= 0; i-- {
			if workflowTraceWarnings.scopes[i].id == restoreID {
				restoreIdx = i
				break
			}
		}
		if restoreIdx < 0 {
			return
		}
		workflowTraceWarnings.scopes = append(workflowTraceWarnings.scopes[:restoreIdx], workflowTraceWarnings.scopes[restoreIdx+1:]...)
		if n := len(workflowTraceWarnings.scopes); n > 0 {
			top := workflowTraceWarnings.scopes[n-1]
			workflowTraceWarnings.writer = top.writer
			workflowTraceWarnings.warned = top.warned
			return
		}
		workflowTraceWarnings.writer = os.Stderr
		workflowTraceWarnings.warned = map[string]struct{}{}
	}
}

func runWorkflowServe(agentName string, follow bool, _ io.Writer, stderr io.Writer) error {
	restoreTraceWarnings := useWorkflowTraceWarnings(stderr)
	defer restoreTraceWarnings()

	if follow {
		if err := requireWorkflowServeFollowSessionEnv(); err != nil {
			return err
		}
	}

	cityPath, err := resolveCity()
	if err != nil {
		return err
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return err
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	warnLegacyWorkflowTracePath(cityPath, cfg.Rigs, stderr)
	if agentName == "" {
		agentName = os.Getenv("GC_ALIAS")
	}
	if agentName == "" {
		agentName = os.Getenv("GC_AGENT")
	}
	if agentName == "" || agentName == strings.TrimSpace(os.Getenv("GC_ALIAS")) || agentName == strings.TrimSpace(os.Getenv("GC_AGENT")) {
		template := strings.TrimSpace(os.Getenv("GC_TEMPLATE"))
		hasSessionContext := strings.TrimSpace(os.Getenv("GC_SESSION_NAME")) != "" ||
			strings.TrimSpace(os.Getenv("GC_SESSION_ID")) != ""
		if template != "" && hasSessionContext {
			agentName = template
		}
	}
	if agentName == "" {
		agentName = config.ControlDispatcherAgentName
	}
	agentCfg, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
	if !ok {
		return fmt.Errorf("agent %q not found in config", agentName)
	}
	workDir := agentCommandDir(cityPath, &agentCfg, cfg.Rigs)
	workEnv, err := controllerWorkQueryEnv(cityPath, cfg, &agentCfg)
	if err != nil {
		return fmt.Errorf("building work query env: %w", err)
	}
	cityName := loadedCityName(cfg, cityPath)
	// Expand {{.Rig}}/{{.AgentBase}} once so the long-poll drain reuses the
	// rig-scoped command instead of passing the literal template to the shell
	// on every iteration. #793.
	workQuery := expandAgentCommandTemplate(cityPath, cityName, &agentCfg, cfg.Rigs, "work_query", agentCfg.EffectiveWorkQueryForBeads(cfg.Beads), stderr)
	if agentCfg.WorkQuery == "" && isWorkflowServeControlDispatcherAgent(agentCfg) {
		workQuery = workflowServeControlReadyQueryForBeads(agentCfg, cfg.Beads, config.NamedSessionRuntimeName(cityName, cfg.Workspace, agentCfg.QualifiedName()))
	}
	workflowTracef("serve start agent=%s city=%s dir=%s", agentCfg.QualifiedName(), cityPath, workDir)
	if !follow {
		_, err := drainWorkflowServeWork(agentCfg, cityPath, workDir, workQuery, workEnv, stderr)
		return err
	}
	return runWorkflowServeFollow(agentCfg, cityPath, workDir, workQuery, workEnv, stderr)
}

func requireWorkflowServeFollowSessionEnv() error {
	var missing []string
	for _, key := range []string{"GC_SESSION_ID", "GC_SESSION_NAME"} {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("control dispatcher follow mode requires managed session env (%s not set)", strings.Join(missing, ", "))
	}
	return nil
}

func legacyWorkflowTracePaths(cityPath string, rigs []config.Rig) []string {
	paths := make([]string, 0, len(rigs)+1)
	seen := make(map[string]struct{}, len(rigs)+1)
	appendTracePath := func(root string) {
		root = strings.TrimSpace(root)
		if root == "" || !pathIsWithin(cityPath, root) {
			return
		}
		tracePath := filepath.Join(root, "control-dispatcher-trace.log")
		normalized := normalizePathForCompare(tracePath)
		if normalized == "" {
			return
		}
		if _, exists := seen[normalized]; exists {
			return
		}
		seen[normalized] = struct{}{}
		paths = append(paths, tracePath)
	}

	appendTracePath(cityPath)
	for _, rig := range rigs {
		appendTracePath(rig.Path)
	}
	appendTracePath(os.Getenv("GC_RIG_ROOT"))
	return paths
}

func warnLegacyWorkflowTracePath(cityPath string, rigs []config.Rig, stderr io.Writer) {
	if stderr == nil {
		return
	}
	legacyTracePaths := legacyWorkflowTracePaths(cityPath, rigs)
	nextTracePath := strings.TrimSpace(os.Getenv("GC_CONTROL_DISPATCHER_TRACE_DEFAULT"))
	if nextTracePath == "" {
		nextTracePath = citylayout.ControlDispatcherTraceDefaultPath(cityPath)
	}
	current := strings.TrimSpace(os.Getenv("GC_WORKFLOW_TRACE"))
	if current != "" {
		for _, legacyTracePath := range legacyTracePaths {
			if samePath(current, legacyTracePath) {
				workflowTraceWarnf(
					stderr,
					"legacy-trace-path:"+normalizePathForCompare(current),
					"gc convoy control --serve: warning: legacy control-dispatcher trace path %q matches a watcher-visible legacy location; change or unset GC_WORKFLOW_TRACE so this session adopts %q, or restart/recycle the session if this value was inherited before the upgrade\n",
					current,
					nextTracePath,
				)
				return
			}
		}
	}
	activeTracePath := current
	if activeTracePath == "" {
		activeTracePath = nextTracePath
	}
	for _, legacyTracePath := range legacyTracePaths {
		if samePath(activeTracePath, legacyTracePath) {
			continue
		}
		info, err := os.Stat(legacyTracePath)
		if err != nil || info.IsDir() {
			continue
		}
		workflowTraceWarnf(
			stderr,
			"legacy-trace-file:"+normalizePathForCompare(legacyTracePath),
			"gc convoy control --serve: warning: legacy control-dispatcher trace file %q still exists; writes to it can wake the city watcher. If it is still growing, restart or recycle the control-dispatcher session so it adopts %q.\n",
			legacyTracePath,
			nextTracePath,
		)
	}
}

type workflowServeDrainResult struct {
	processedAny bool
	pendingAny   bool
}

// drainWorkflowServeWork runs the control-dispatcher drain loop to completion
// for a single invocation. Returns whether it advanced a control bead and
// whether the queue still contains only pending work so the --follow caller
// can distinguish blocked work from genuine idle.
func drainWorkflowServeWork(agentCfg config.Agent, cityPath, storePath, workQuery string, workEnv map[string]string, stderr io.Writer) (workflowServeDrainResult, error) {
	result := workflowServeDrainResult{}
	idlePolls := 0
	for {
		serveQuery := workflowServeWorkQuery(agentCfg, workQuery)
		queue, err := workflowServeList(serveQuery, storePath, workEnv)
		if err != nil {
			workflowTracef("serve query-error agent=%s err=%v", agentCfg.QualifiedName(), err)
			// Surface a killed/timed-out control work query on the event
			// bus so the reconciler has a named cause to escalate on
			// rather than the session dying silently (issues #1496/#1497).
			emitCityWorkQueryFailure(cityPath, stderr,
				os.Getenv("GC_SESSION_ID"), os.Getenv("GC_TEMPLATE"), serveQuery, err)
			return result, fmt.Errorf("querying control work for %s: %w", agentCfg.QualifiedName(), err)
		}
		if len(queue) == 0 {
			if result.processedAny && idlePolls < workflowServeIdlePollAttempts {
				idlePolls++
				workflowTracef("serve idle-retry agent=%s attempt=%d", agentCfg.QualifiedName(), idlePolls)
				time.Sleep(workflowServeIdlePollInterval)
				continue
			}
			workflowTracef("serve idle-exit agent=%s", agentCfg.QualifiedName())
			return result, nil
		}
		idlePolls = 0
		processedThisCycle := false
		pendingCount := 0
		for _, candidate := range queue {
			beadID := candidate.ID
			kind := strings.TrimSpace(candidate.Metadata[beadmeta.KindMetadataKey])
			workflowTracef("serve process bead=%s kind=%s store=%s", beadID, kind, storePath)
			// controlDispatcherServe currently returns nil both when it
			// successfully advanced a control bead AND when ProcessControl
			// chose to no-op (e.g., status != "open"). The caller cannot
			// tell those apart without cross-referencing the store, so the
			// trace line just below was previously identical in both
			// cases. That masked a 20-minute stall on ga-ttn5z's retry
			// control ga-fw2fm. The silent no-op now emits a separate
			// `process-control ... skip reason=bead_not_open` line inside
			// ProcessControl itself; see runtime.go.
			if err := controlDispatcherServe(cityPath, storePath, beadID, io.Discard, stderr); err != nil {
				if errors.Is(err, dispatch.ErrControlPending) {
					pendingCount++
					result.pendingAny = true
					workflowTracef("serve pending bead=%s kind=%s", beadID, kind)
					continue
				}
				workflowTracef("serve process-error bead=%s kind=%s err=%v", beadID, kind, err)
				if dispatch.IsTransientControllerError(err) {
					pendingCount++
					result.pendingAny = true
					workflowTracef("serve transient-error-pending bead=%s kind=%s err=%v", beadID, kind, err)
					continue
				}
				return result, fmt.Errorf("processing control bead %s: %w", beadID, err)
			}
			workflowTracef("serve processed bead=%s kind=%s", beadID, kind)
			result.processedAny = true
			processedThisCycle = true
		}
		if processedThisCycle {
			// Signal workers to skip their poll sleep: new step beads may be ready.
			writeDispatchWakeFile(cityPath)
			continue
		}
		if pendingCount > 0 {
			workflowTracef("serve pending-queue agent=%s count=%d", agentCfg.QualifiedName(), pendingCount)
			return result, nil
		}
	}
}

func runWorkflowServeFollow(agentCfg config.Agent, cityPath, storePath, workQuery string, workEnv map[string]string, stderr io.Writer) error {
	ep, err := workflowServeOpenEventsProvider(stderr)
	if err != nil {
		return err
	}
	defer ep.Close() //nolint:errcheck // best-effort cleanup

	afterSeq, err := ep.LatestSeq()
	if err != nil {
		return fmt.Errorf("reading current event cursor: %w", err)
	}
	watcher, err := ep.Watch(context.Background(), afterSeq)
	if err != nil {
		return fmt.Errorf("watching city events: %w", err)
	}
	defer watcher.Close() //nolint:errcheck // best-effort cleanup
	done := make(chan struct{})
	defer close(done)

	eventCh := make(chan workflowWatchResult, 1)
	go pumpWorkflowEvents(done, watcher, eventCh)

	idleSweeps := 0
	var pendingWakeErr error
	for {
		drainResult, err := drainWorkflowServeWork(agentCfg, cityPath, storePath, workQuery, workEnv, stderr)
		if err != nil {
			// A transient work-query/store failure — most commonly the
			// work-query timeout (hookWorkQueryTimeout) when the bead store is
			// briefly saturated — must NOT terminate this long-running serve
			// loop. drainWorkflowServeWork already surfaced the failure on the
			// event bus for reconciler visibility (#1496/#1497); returning here
			// kills the dispatcher process (pane exits non-zero) and leaves the
			// rig un-dispatched while its session bead still reports "active".
			// Downgrade to a no-progress sweep so the idle backoff retries it;
			// only genuinely fatal errors end the loop.
			if !dispatch.IsTransientControllerError(err) {
				return err
			}
			workflowTracef("serve drain-transient-retry agent=%s err=%v", agentCfg.QualifiedName(), err)
			drainResult = workflowServeDrainResult{}
		}
		if pendingWakeErr != nil {
			// The previous wait observed a relevant event and then a fatal
			// watcher error in the same coalescing window. The drain above is
			// the one re-scan that wake promised, so the observed work is now
			// serviced; surface the watcher error to end the loop.
			return pendingWakeErr
		}
		if drainResult.processedAny || drainResult.pendingAny {
			idleSweeps = 0
		}
		sleepDur := followSleepDuration(idleSweeps)
		workflowTracef(
			"serve wait agent=%s idle_sweeps=%d sleep=%s processed=%t pending=%t",
			agentCfg.QualifiedName(),
			idleSweeps,
			sleepDur,
			drainResult.processedAny,
			drainResult.pendingAny,
		)
		eventWake, err := workflowServeWaitForWake(eventCh, sleepDur, idleSweeps)
		if err != nil {
			if !eventWake {
				// Fatal stream error with no relevant event observed: nothing to
				// re-scan, so terminate immediately.
				return err
			}
			// A relevant event was observed just before the fatal error. Loop
			// once more so the next drain services that wake, then surface the
			// error on the following iteration.
			pendingWakeErr = err
			idleSweeps = 0
			continue
		}
		switch {
		case eventWake, drainResult.pendingAny:
			idleSweeps = 0
		case !drainResult.processedAny:
			idleSweeps++
		}
	}
}

type workflowWatchResult struct {
	evt events.Event
	err error
}

func pumpWorkflowEvents(done <-chan struct{}, watcher events.Watcher, eventCh chan<- workflowWatchResult) {
	for {
		evt, err := watcher.Next()
		select {
		case eventCh <- workflowWatchResult{evt: evt, err: err}:
		case <-done:
			return
		}
		if err != nil {
			return
		}
	}
}

// waitForRelevantWorkflowWake blocks until either a relevant city event wakes
// the --follow loop or sleepDur elapses. Returns eventWake=true on the event
// path (so the caller can reset any idle-backoff counter), false when the
// timer fires.
func waitForRelevantWorkflowWake(eventCh <-chan workflowWatchResult, sleepDur time.Duration) (bool, error) {
	return waitForRelevantWorkflowWakeWithTrace(eventCh, sleepDur, -1)
}

func waitForRelevantWorkflowWakeWithTrace(eventCh <-chan workflowWatchResult, sleepDur time.Duration, idleSweeps int) (bool, error) {
	timer := time.NewTimer(sleepDur)
	defer timer.Stop()

	for {
		select {
		case res := <-eventCh:
			if res.err != nil {
				return false, res.err
			}
			if workflowEventRelevant(res.evt) {
				if idleSweeps >= 0 {
					workflowTracef("serve wake-event type=%s subject=%s idle_sweeps=%d sleep=%s", res.evt.Type, res.evt.Subject, idleSweeps, sleepDur)
				} else {
					workflowTracef("serve wake-event type=%s subject=%s", res.evt.Type, res.evt.Subject)
				}
				// Coalesce a burst: keep draining buffered events for a short
				// debounce window so N relevant events collapse into one drain.
				// runWorkflowServeFollow does exactly one drain per return=true,
				// so the trailing events are already covered by that single
				// re-scan — no event is dropped, only batched.
				coalesced, coalesceErr := coalesceWorkflowWakeBurst(eventCh)
				if coalesced > 0 {
					workflowTracef("serve wake-coalesce extra=%d debounce=%s", coalesced, workflowServeWakeDebounce)
				}
				// Report the wake even when a fatal stream error arrived during
				// the coalescing window: a relevant event was already observed,
				// so runWorkflowServeFollow must still perform the one re-scan it
				// promised for that wake before terminating. Surfacing
				// (true, err) lets the caller drain the observed wake and then
				// exit on the error, instead of stranding newly-ready work until
				// a dispatcher restart re-scans.
				return true, coalesceErr
			}
			workflowTracef("serve ignore-event type=%s subject=%s", res.evt.Type, res.evt.Subject)
		case <-timer.C:
			if idleSweeps >= 0 {
				workflowTracef("serve wake-sweep idle_sweeps=%d sleep=%s", idleSweeps, sleepDur)
			} else {
				workflowTracef("serve wake-sweep")
			}
			return false, nil
		}
	}
}

// coalesceWorkflowWakeBurst drains additional buffered events from eventCh for
// the workflowServeWakeDebounce window after a relevant event has already
// decided to wake the loop. It returns the number of extra events it folded
// into this wake so the caller emits a single drain for the whole burst, plus
// any watcher error encountered while draining. The caller pairs that error
// with the already-observed wake (returning true, err) so the serve loop still
// performs the one promised re-scan before terminating on a fatal stream
// failure. Events are only batched here, never dropped: the caller's single
// re-scan already reflects every drained event.
func coalesceWorkflowWakeBurst(eventCh <-chan workflowWatchResult) (int, error) {
	if workflowServeWakeDebounce <= 0 {
		return 0, nil
	}
	debounce := time.NewTimer(workflowServeWakeDebounce)
	defer debounce.Stop()

	coalesced := 0
	for {
		select {
		case res := <-eventCh:
			if res.err != nil {
				return coalesced, res.err
			}
			coalesced++
		case <-debounce.C:
			return coalesced, nil
		}
	}
}

func workflowEventRelevant(evt events.Event) bool {
	switch evt.Type {
	case events.BeadCreated, events.BeadClosed, events.BeadUpdated:
		return true
	default:
		return false
	}
}

func workflowServeQuery(workQuery string) string {
	const single = "--limit=1"
	scan := fmt.Sprintf("--limit=%d", workflowServeScanLimit)
	if strings.Contains(workQuery, single) {
		return strings.Replace(workQuery, single, scan, 1)
	}
	return workQuery
}

func workflowServeWorkQuery(agentCfg config.Agent, expandedWorkQuery ...string) string {
	if len(expandedWorkQuery) > 0 {
		return workflowServeQuery(expandedWorkQuery[0])
	}
	if agentCfg.WorkQuery == "" && isWorkflowServeControlDispatcherAgent(agentCfg) {
		return workflowServeControlReadyQuery(agentCfg)
	}
	workQuery := agentCfg.EffectiveWorkQuery()
	return workflowServeQuery(workQuery)
}

func isWorkflowServeControlDispatcherAgent(agentCfg config.Agent) bool {
	qualified := strings.TrimSpace(agentCfg.QualifiedName())
	return qualified == config.ControlDispatcherAgentName ||
		strings.HasSuffix(qualified, "/"+config.ControlDispatcherAgentName) ||
		strings.HasSuffix(qualified, "."+config.ControlDispatcherAgentName)
}

func workflowServeControlReadyQuery(agentCfg config.Agent, controlSessionNames ...string) string {
	return workflowServeControlReadyQueryForBeads(agentCfg, config.BeadsConfig{}, controlSessionNames...)
}

func workflowServeControlReadyQueryForBeads(agentCfg config.Agent, beadsCfg config.BeadsConfig, controlSessionNames ...string) string {
	target := strings.TrimSpace(agentCfg.QualifiedName())
	if target == "" {
		target = config.ControlDispatcherAgentName
	}
	limit := fmt.Sprintf("%d", workflowServeScanLimit)
	includeEphemeral := ""
	if beadsCfg.UsesBD105ReadySemantics() {
		includeEphemeral = " --include-ephemeral"
	}
	jqFilter := fmt.Sprintf(
		`reduce add[] as $item ([]; if (($item.metadata // {})[%q] // "") != "" then . elif any(.[]; .id == $item.id) then . else . + [$item] end)`,
		beadmeta.InstantiatingMetadataKey,
	)
	jqFilter = strings.ReplaceAll(jqFilter, `\`, `\\`)
	jqFilter = strings.ReplaceAll(jqFilter, `"`, `\"`)
	jqFilter = strings.ReplaceAll(jqFilter, `$`, `\$`)
	queryPrefix := `BD_EXPORT_AUTO=false GC_CONTROL_TARGET=` + shellquote.Quote(target)
	for _, name := range controlSessionNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		queryPrefix += ` GC_CONTROL_SESSION_NAME=` + shellquote.Quote(name)
		break
	}
	if legacy := workflowServeLegacyControlRoute(target); legacy != "" {
		queryPrefix += ` GC_CONTROL_LEGACY_TARGET=` + shellquote.Quote(legacy)
	}
	if bare := controlDispatcherBareRoute(target); bare != "" {
		queryPrefix += ` GC_CONTROL_BARE_TARGET=` + shellquote.Quote(bare)
	}
	query := queryPrefix + ` sh -c '` +
		`set -e; ` +
		`tmp=$(mktemp); seen="$tmp.seen"; err="$tmp.err"; : > "$seen"; trap "rm -f \"$tmp\" \"$seen\" \"$err\"" EXIT; ` +
		`emit_ready() { r=$("$@" 2>"$err") || { status=$?; [ -n "$r" ] && printf "%s\n" "$r" >&2; cat "$err" >&2; return "$status"; }; [ -n "$r" ] && [ "$r" != "[]" ] && printf "%s\n" "$r" >> "$tmp"; return 0; }; ` +
		`assignee_ready() { cand="$1"; [ -z "$cand" ] && return 0; if grep -Fxq "$cand" "$seen"; then return 0; fi; printf "%s\n" "$cand" >> "$seen"; ` +
		`emit_ready bd --readonly --sandbox ready` + includeEphemeral + ` --assignee="$cand" --exclude-type=epic --json --limit=` + limit + `; }; ` +
		`routed_ready() { route="$1"; [ -z "$route" ] && return 0; ` +
		`emit_ready bd --readonly --sandbox ready` + includeEphemeral + ` --metadata-field "gc.run_target=$route" --unassigned --exclude-type=epic --json --sort oldest --limit=` + limit + `; ` +
		`emit_ready bd --readonly --sandbox ready` + includeEphemeral + ` --metadata-field "gc.routed_to=$route" --unassigned --exclude-type=epic --json --sort oldest --limit=` + limit + `; ` +
		`}; ` +
		`for id in "$GC_CONTROL_SESSION_NAME" "$GC_SESSION_NAME" "$GC_ALIAS" "$GC_CONTROL_TARGET" "$GC_SESSION_ID"; do ` +
		`[ -z "$id" ] && continue; ` +
		`legacy=""; case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac; ` +
		`for cand in "$id" "$legacy"; do ` +
		`[ -z "$cand" ] && continue; ` +
		`assignee_ready "$cand"; ` +
		`done; ` +
		`done; ` +
		`routed_ready "$GC_CONTROL_TARGET"; ` +
		`routed_ready "${GC_CONTROL_LEGACY_TARGET:-}"; ` +
		`routed_ready "${GC_CONTROL_BARE_TARGET:-}"; ` +
		`if [ -s "$tmp" ]; then jq -s "` + jqFilter + `" "$tmp"; else printf "[]"; fi` + `'`
	return query
}

func workflowServeLegacyControlRoute(target string) string {
	target = strings.TrimSpace(target)
	if target == config.ControlDispatcherAgentName {
		return "workflow-control"
	}
	const suffix = "/" + config.ControlDispatcherAgentName
	if strings.HasSuffix(target, suffix) {
		return strings.TrimSuffix(target, suffix) + "/workflow-control"
	}
	return ""
}

// controlDispatcherBareRoute returns the binding-stripped alias of a control
// dispatcher's qualified name, e.g. "core.control-dispatcher" ->
// "control-dispatcher" and "rig/core.control-dispatcher" ->
// "rig/control-dispatcher". Pre-1.3 builds routed control beads to this bare
// form (see the pre-migration controlDispatcherTargetForExecutionTarget), so
// the qualified-name consumers must still claim/scale them after an upgrade.
// Returns "" when target is already bare (no distinct alias) or is not a
// control-dispatcher route.
func controlDispatcherBareRoute(target string) string {
	target = strings.TrimSpace(target)
	if target == "" || target == config.ControlDispatcherAgentName {
		return ""
	}
	dir, name := config.ParseQualifiedName(target)
	if name == config.ControlDispatcherAgentName {
		return "" // already bare (possibly rig-scoped); the target itself matches
	}
	if !strings.HasSuffix(name, "."+config.ControlDispatcherAgentName) {
		return ""
	}
	if dir != "" {
		return dir + "/" + config.ControlDispatcherAgentName
	}
	return config.ControlDispatcherAgentName
}

func nextWorkflowServeBeads(workQuery, dir string, env map[string]string) ([]hookBead, error) {
	if workQuery == "" {
		return nil, nil
	}
	output, err := shellWorkQueryWithEnv(workQuery, dir, mergeRuntimeEnv(os.Environ(), env))
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(output)
	if !workQueryHasReadyWork(trimmed) {
		return nil, nil
	}
	var beadsOut []hookBead
	if err := json.Unmarshal([]byte(trimmed), &beadsOut); err == nil {
		return beadsOut, nil
	}
	var bead hookBead
	if err := json.Unmarshal([]byte(trimmed), &bead); err == nil {
		return []hookBead{bead}, nil
	}
	return nil, fmt.Errorf("unexpected work query output: %s", trimmed)
}

// dispatchWakeFile returns the path of the dispatch-wake sentinel file.
// The control dispatcher touches it after each successful batch so workers
// can skip their poll sleep and call gc hook immediately.
func dispatchWakeFile(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "dispatch-wake")
}

// writeDispatchWakeFile updates the mtime of the dispatch-wake sentinel file.
// Best-effort: if the write fails the dispatch cycle continues normally;
// workers fall back to their standard poll interval.
func writeDispatchWakeFile(cityPath string) {
	path := dispatchWakeFile(cityPath)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_ = f.Close()
}

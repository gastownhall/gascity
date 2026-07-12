package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphroute"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
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
	// Session beads are session-class; thread the city store into the consumer
	// as a typed beads.SessionStore so the class stays statically visible. The
	// lazy open above is retained deliberately: it is the load-bearing guard that
	// avoids spinning up a managed Dolt backend on a bare working dir, and there
	// is no controllerState here to source a pre-opened session store from.
	return sessionRuntimeMissingInStore(beads.SessionStore{Store: store}, qualifiedName)
}

// sessionRuntimeMissingInStore reports whether any open session bead for the
// agent (selected by its agent:<qualified> label) projects the runtime-missing
// lifecycle reason in the given session store. The projection lives in
// internal/session so the API sling path can share it without importing package
// main; it takes the unwrapped beads.Store.
func sessionRuntimeMissingInStore(store beads.SessionStore, qualifiedName string) bool {
	return sessionpkg.RuntimeMissingInStore(store.Store, qualifiedName)
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
	// Cap the --follow idle backoff at 5s. A worker that closes a step bead
	// with a raw bd write does not publish a city BeadClosed event, so the
	// control-dispatcher only notices the next ready step on its idle re-poll.
	// At the former 30s cap each graph hop could wait up to ~30s, so a
	// multi-step workflow accumulated minutes of pure wake latency (the bulk of
	// the TestGraphWorkflowSuccessPath flake). 5s keeps the loop responsive
	// across hops while still backing off from the 1s base; the cost is one
	// serve loop polling every 5s rather than 30s when a city is fully idle.
	// (Complementary to the wake-debounce coalescing below, which only helps
	// the event-arrival path; a raw-bd-write close publishes no event.)
	workflowServeMaxIdleSleep = 5 * time.Second
	// workflowServeWakeDebounce is the coalescing window opened once the first
	// relevant event wakes the --follow loop. Additional buffered events that
	// arrive during the window are drained and folded into the same wake so a
	// burst of N bead.* events (e.g. an mc-wisp-* event storm) collapses into a
	// single work/ready re-scan instead of N heavy per-event Dolt scans. This is
	// a fixed (max-wait) window, so a lone relevant wake also waits out the
	// window before its drain; the delay is intentional and small relative to
	// the 1–5s idle sleeps it replaces. Set it to 0 to disable coalescing and
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

const (
	workflowServeScanLimit               = 20
	workflowServeControlReadyQueryPrefix = "gc-control-ready-v1:"
)

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
		workQuery = workflowServeControlReadyQuery(agentCfg, config.NamedSessionRuntimeName(cityName, cfg.Workspace, agentCfg.QualifiedName()))
	}
	targets, err := workflowServeTargets(cityPath, cfg, &agentCfg, workDir, workEnv)
	if err != nil {
		return err
	}
	workflowTracef("serve start agent=%s city=%s dir=%s stores=%d", agentCfg.QualifiedName(), cityPath, workDir, len(targets))
	if !follow {
		_, err := drainWorkflowServeWork(agentCfg, cityPath, targets, workQuery, stderr)
		return err
	}
	return runWorkflowServeFollow(agentCfg, cityPath, targets, workQuery, stderr)
}

// workflowServeTarget pairs a bead store root with the env that pins the
// serve loop's ready query and per-bead dispatch to that store.
type workflowServeTarget struct {
	storePath string
	workEnv   map[string]string
}

// workflowServeTargets returns every store the serve loop must scan for the
// resolved agent. A rig-scoped dispatcher instance watches only its own rig
// store. The city-singleton control dispatcher also owns rig-routed control
// beads — config.PreferredDeterministicControlDispatcher stamps rig-store
// control beads with the singleton's qualified name — so its loop fans
// across the city store plus every configured rig store, mirroring how the
// one-shot path locates beads with findBeadAcrossStores. A city-only scan
// strands rig control beads: their routed demand points at a loop that
// cannot see them, while the rig dispatcher sees no demand and never scales
// from zero (gastownhall/gascity#3872, incident 2). Agents with a custom
// work_query keep the single-store behavior: their query text may embed
// rig-specific template expansions that do not transfer across stores.
func workflowServeTargets(cityPath string, cfg *config.City, agentCfg *config.Agent, workDir string, workEnv map[string]string) ([]workflowServeTarget, error) {
	targets := []workflowServeTarget{{storePath: workDir, workEnv: workEnv}}
	if !isWorkflowServeControlDispatcherAgent(*agentCfg) ||
		strings.TrimSpace(agentCfg.Dir) != "" ||
		strings.TrimSpace(agentCfg.WorkQuery) != "" {
		return targets, nil
	}
	seen := map[string]struct{}{normalizePathForCompare(workDir): {}}
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		rigAgent := *agentCfg
		rigAgent.Dir = rig.Name
		storePath := agentCommandDir(cityPath, &rigAgent, cfg.Rigs)
		// A rig path aliasing the city root (or another rig) resolves to a
		// store already in the list; scanning it twice per sweep would
		// double the query cost for nothing.
		if _, dup := seen[normalizePathForCompare(storePath)]; dup {
			continue
		}
		seen[normalizePathForCompare(storePath)] = struct{}{}
		env, err := controllerWorkQueryEnv(cityPath, cfg, &rigAgent)
		if err != nil {
			return nil, fmt.Errorf("building work query env for rig %q: %w", rig.Name, err)
		}
		targets = append(targets, workflowServeTarget{
			storePath: storePath,
			workEnv:   env,
		})
	}
	return targets, nil
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
// for a single invocation. Each sweep takes one ready batch from every serve
// target in order — round-robin rather than drain-to-quiescence per store, so
// a busy city store cannot starve the rig stores. A failing target is traced,
// surfaced on the event bus, and skipped for the sweep while the healthy
// targets keep draining; the loop errors out only when every target fails,
// so one bad rig store cannot gate city-wide control dispatch or kill the
// --follow session. Returns whether it advanced a control bead and whether
// any store's queue still contains only pending work so the --follow caller
// can distinguish blocked work from genuine idle.
func drainWorkflowServeWork(agentCfg config.Agent, cityPath string, targets []workflowServeTarget, workQuery string, stderr io.Writer) (workflowServeDrainResult, error) {
	result := workflowServeDrainResult{}
	idlePolls := 0
	for {
		processedThisSweep := false
		pendingThisSweep := false
		failedTargets := 0
		var firstErr error
		for _, target := range targets {
			batch, err := drainWorkflowServeStoreBatch(agentCfg, cityPath, target, workQuery, stderr)
			if batch.processed > 0 {
				result.processedAny = true
				processedThisSweep = true
			}
			if batch.pending > 0 {
				result.pendingAny = true
				pendingThisSweep = true
			}
			if err != nil {
				failedTargets++
				if firstErr == nil {
					firstErr = err
				}
				workflowTracef("serve target-error agent=%s store=%s err=%v (isolating target for this sweep)",
					agentCfg.QualifiedName(), target.storePath, err)
			}
		}
		if failedTargets == len(targets) {
			return result, firstErr
		}
		if processedThisSweep {
			// Signal workers to skip their poll sleep: new step beads may be ready.
			writeDispatchWakeFile(cityPath)
			idlePolls = 0
			continue
		}
		if pendingThisSweep {
			workflowTracef("serve pending-queue agent=%s", agentCfg.QualifiedName())
			return result, nil
		}
		if result.processedAny && idlePolls < workflowServeIdlePollAttempts {
			idlePolls++
			workflowTracef("serve idle-retry agent=%s attempt=%d", agentCfg.QualifiedName(), idlePolls)
			time.Sleep(workflowServeIdlePollInterval)
			continue
		}
		workflowTracef("serve idle-exit agent=%s", agentCfg.QualifiedName())
		return result, nil
	}
}

// workflowServeBatchResult reports one target's single query-and-process pass.
type workflowServeBatchResult struct {
	processed int
	pending   int
}

// drainWorkflowServeStoreBatch queries one target store once and processes
// the ready control beads it returned.
func drainWorkflowServeStoreBatch(agentCfg config.Agent, cityPath string, target workflowServeTarget, workQuery string, stderr io.Writer) (workflowServeBatchResult, error) {
	result := workflowServeBatchResult{}
	serveQuery := workflowServeWorkQuery(agentCfg, workQuery)
	queue, err := workflowServeList(serveQuery, target.storePath, target.workEnv)
	if err != nil {
		workflowTracef("serve query-error agent=%s store=%s err=%v", agentCfg.QualifiedName(), target.storePath, err)
		// Surface a killed/timed-out control work query on the event
		// bus so the reconciler has a named cause to escalate on
		// rather than the session dying silently (issues #1496/#1497).
		emitCityWorkQueryFailure(cityPath, stderr,
			os.Getenv("GC_SESSION_ID"), os.Getenv("GC_TEMPLATE"), serveQuery, err)
		return result, fmt.Errorf("querying control work for %s: %w", agentCfg.QualifiedName(), err)
	}
	for _, candidate := range queue {
		beadID := candidate.ID
		kind := strings.TrimSpace(candidate.Metadata[beadmeta.KindMetadataKey])
		workflowTracef("serve process bead=%s kind=%s store=%s", beadID, kind, target.storePath)
		// controlDispatcherServe currently returns nil both when it
		// successfully advanced a control bead AND when ProcessControl
		// chose to no-op (e.g., status != "open"). The caller cannot
		// tell those apart without cross-referencing the store, so the
		// trace line just below was previously identical in both
		// cases. That masked a 20-minute stall on ga-ttn5z's retry
		// control ga-fw2fm. The silent no-op now emits a separate
		// `process-control ... skip reason=bead_not_open` line inside
		// ProcessControl itself; see runtime.go.
		if err := controlDispatcherServe(cityPath, target.storePath, beadID, io.Discard, stderr); err != nil {
			if errors.Is(err, dispatch.ErrControlPending) {
				result.pending++
				workflowTracef("serve pending bead=%s kind=%s", beadID, kind)
				continue
			}
			workflowTracef("serve process-error bead=%s kind=%s err=%v", beadID, kind, err)
			if dispatch.IsTransientControllerError(err) {
				result.pending++
				workflowTracef("serve transient-error-pending bead=%s kind=%s err=%v", beadID, kind, err)
				continue
			}
			return result, fmt.Errorf("processing control bead %s: %w", beadID, err)
		}
		workflowTracef("serve processed bead=%s kind=%s", beadID, kind)
		result.processed++
	}
	return result, nil
}

func runWorkflowServeFollow(agentCfg config.Agent, cityPath string, targets []workflowServeTarget, workQuery string, stderr io.Writer) error {
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
		drainResult, err := drainWorkflowServeWork(agentCfg, cityPath, targets, workQuery, stderr)
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

// workflowServeControlReadyQuery builds the internal control-ready spec the
// supervisor-cache consumer (api.Client.ListReadyBeads) filters client-side.
// It relies on an invariant: graph.v2 control beads are always durable
// (main-tier), never ephemeral/wisp-tier. The /beads/ready cache read this spec
// drives is a default-tier (TierIssues) read, which excludes ephemeral rows, so
// every control bead is reachable only because no dispatch/sling/formula/graph
// creation path materializes control beads in the ephemeral tier. If that ever
// changes, this scan would silently miss them and tier selection must be plumbed
// through here.
func workflowServeControlReadyQuery(agentCfg config.Agent, controlSessionNames ...string) string {
	target := strings.TrimSpace(agentCfg.QualifiedName())
	if target == "" {
		target = config.ControlDispatcherAgentName
	}
	spec := workflowServeControlReadySpec{
		Target:       target,
		LegacyTarget: workflowServeLegacyControlRoute(target),
		// Pre-1.3 builds routed control beads to the binding-stripped bare
		// name "control-dispatcher". Carrying it in the spec lets the
		// supervisor-cache consumer claim that in-flight work after an upgrade
		// scaled the qualified dispatcher, mirroring the pre-cache shell loop's
		// GC_CONTROL_BARE_TARGET routed scan.
		BareTarget: controlDispatcherBareRoute(target),
	}
	for _, name := range controlSessionNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		spec.ControlSessionName = name
		break
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return workflowServeControlReadyQueryPrefix + base64.RawURLEncoding.EncodeToString(data)
}

type workflowServeControlReadySpec struct {
	Target             string `json:"target"`
	ControlSessionName string `json:"control_session_name,omitempty"`
	LegacyTarget       string `json:"legacy_target,omitempty"`
	BareTarget         string `json:"bare_target,omitempty"`
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
	if spec, ok, err := parseWorkflowServeControlReadyQuery(workQuery); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return nextWorkflowServeControlReadyBeads(spec, env)
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

func parseWorkflowServeControlReadyQuery(workQuery string) (workflowServeControlReadySpec, bool, error) {
	encoded, ok := strings.CutPrefix(strings.TrimSpace(workQuery), workflowServeControlReadyQueryPrefix)
	if !ok {
		return workflowServeControlReadySpec{}, false, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return workflowServeControlReadySpec{}, true, fmt.Errorf("decode control ready query: %w", err)
	}
	var spec workflowServeControlReadySpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return workflowServeControlReadySpec{}, true, fmt.Errorf("parse control ready query: %w", err)
	}
	spec.Target = strings.TrimSpace(spec.Target)
	spec.ControlSessionName = strings.TrimSpace(spec.ControlSessionName)
	spec.LegacyTarget = strings.TrimSpace(spec.LegacyTarget)
	spec.BareTarget = strings.TrimSpace(spec.BareTarget)
	if spec.Target == "" {
		return workflowServeControlReadySpec{}, true, fmt.Errorf("control ready query missing target")
	}
	return spec, true, nil
}

// errControlReadyNoSupervisorClient signals that the control-ready reader could
// not build a supervisor API client (GC_NO_API escape hatch, or the supervisor
// process is down/restarting). The cache-backed scan has no direct-store
// fallback, so the caller maps it to a transient ErrControllerAPIUnavailable and
// the --follow loop retries instead of crash-looping.
var errControlReadyNoSupervisorClient = errors.New("control ready: no supervisor API client")

// workflowServeControlReadyList obtains the federated ready set the control-ready
// scan filters, reading through the supervisor API client. The filter is pushed
// into the request so the supervisor returns only this dispatcher's control work
// instead of the full federated ready set. It is a package var so tests can
// inject transient or partial cache responses without standing up the
// supervisor; production wiring reads through apiClient and returns
// errControlReadyNoSupervisorClient when no client can be built.
var workflowServeControlReadyList = func(cityPath string, filter beads.ControlReadyFilter) (api.ReadyBeads, error) {
	client := apiClient(cityPath)
	if client == nil {
		if disabled, _ := classifyGCNoAPI(os.Getenv("GC_NO_API")); !disabled && apiRouteControllerAliveHook(cityPath) != 0 {
			client = apiRouteSupervisorClientHook(cityPath)
		}
	}
	if client == nil {
		return api.ReadyBeads{}, errControlReadyNoSupervisorClient
	}
	return client.ListReadyBeads(filter)
}

func nextWorkflowServeControlReadyBeads(spec workflowServeControlReadySpec, env map[string]string) ([]hookBead, error) {
	cityPath := strings.TrimSpace(env["GC_CITY_PATH"])
	if cityPath == "" {
		cityPath = strings.TrimSpace(env["GC_CITY"])
	}
	if cityPath == "" {
		cityPath = strings.TrimSpace(env["GC_STORE_ROOT"])
	}
	if cityPath == "" {
		return nil, fmt.Errorf("control ready query missing city path")
	}
	filter := workflowServeControlReadyFilter(spec, env)
	ready, err := workflowServeControlReadyList(cityPath, filter)
	if err != nil {
		if errors.Is(err, errControlReadyNoSupervisorClient) {
			warnControlReadySupervisorAPIUnavailable()
			// The cache-backed control-ready scan has no direct-store fallback, so a
			// disabled (GC_NO_API escape hatch) or down/restarting controller API
			// must not be fatal here. Wrap ErrControllerAPIUnavailable so the
			// --follow serve loop classifies it transient and backs off/retries
			// instead of exiting and crash-looping the long-running dispatcher.
			return nil, fmt.Errorf("control ready query requires supervisor API (%s): %w", apiClientFallbackReason(cityPath), dispatch.ErrControllerAPIUnavailable)
		}
		// A non-nil supervisor client can still fail a ready read while the cache
		// is priming/reconciling (cache_not_live), the store is briefly saturated
		// (store_slow), the supervisor returns a generic 5xx, or the connection
		// drops. These are all retryable and the scan has no direct-store fallback,
		// so map them onto ErrControllerAPIUnavailable and let the --follow loop
		// back off and retry instead of exiting. A genuinely unexpected error stays
		// fatal so it surfaces loudly rather than spinning forever.
		if api.ShouldFallbackForRead(err) || api.IsStoreSlowError(err) {
			return nil, fmt.Errorf("list ready beads from supervisor cache (%w): %w", dispatch.ErrControllerAPIUnavailable, err)
		}
		return nil, fmt.Errorf("list ready beads from supervisor cache: %w", err)
	}
	if !ready.CityReadAuthoritative() {
		// A partial federated read that omitted the city store can hide graph
		// control beads (they live only in the city store) and make a non-idle
		// queue look idle. Treat it as a transient controller-API condition so the
		// follow loop retries instead of acting on an incomplete view. An unrelated
		// rig-store failure leaves the city store authoritative and does not trip
		// this.
		return nil, fmt.Errorf("control ready query got a partial ready set omitting the city store (%s): %w", strings.Join(ready.PartialErrors, "; "), dispatch.ErrControllerAPIUnavailable)
	}
	return filterWorkflowServeControlReadyBeads(ready.Body, spec, env), nil
}

// warnControlReadySupervisorAPIUnavailable emits a one-time operator warning
// when GC_NO_API disables the supervisor API that graph control dispatch now
// reads control-ready work from. GC_NO_API is the documented direct-bd escape
// hatch, but the cache-backed control-ready scan has no direct-store fallback,
// so the dispatcher cannot make progress until the API is reachable. The
// warning routes to the active serve stderr sink and dedupes per command
// invocation so the retry loop does not spam it. A transient controller-down
// blip is not warned (it self-heals on restart); only the persistent escape
// hatch is surfaced.
func warnControlReadySupervisorAPIUnavailable() {
	if disabled, _ := classifyGCNoAPI(os.Getenv("GC_NO_API")); !disabled {
		return
	}
	workflowTraceWarnings.mu.Lock()
	writer := workflowTraceWarnings.writer
	workflowTraceWarnings.mu.Unlock()
	workflowTraceWarnf(
		writer,
		"control-ready-no-api",
		"gc convoy control --serve: warning: GC_NO_API disables the supervisor API, but graph control dispatch reads control-ready work from it and has no direct-bd fallback; the control-dispatcher will retry until GC_NO_API is unset or the API is reachable.\n",
	)
}

// workflowServeControlReadyFilter builds the control-ready filter from the serve
// spec and live session identity: the assignees and routes this dispatcher
// claims, bounded by the per-scan limit. It is shared by the supervisor request
// (so the federated ready set is filtered server-side) and the client-side
// re-filter below, so the two predicates cannot drift. The assignee priority
// order, the control-dispatcher→workflow-control legacy alias, and the
// bare/legacy routed targets all live in the assignee/route builders it calls.
func workflowServeControlReadyFilter(spec workflowServeControlReadySpec, env map[string]string) beads.ControlReadyFilter {
	return beads.ControlReadyFilter{
		Assignees:     workflowServeControlReadyAssignees(spec, env),
		Routes:        workflowServeControlReadyRoutes(spec),
		PerGroupLimit: workflowServeScanLimit,
	}
}

// filterWorkflowServeControlReadyBeads reduces a federated ready set to the
// control beads this dispatcher should claim and converts them to hook beads.
// The supervisor already applies the same ControlReadyFilter server-side; this
// re-filter is idempotent (Apply(Apply(x)) == Apply(x)) and guards against an
// older supervisor (mixed-version rollout) that ignores the new control_* params
// and returns the full ready set.
func filterWorkflowServeControlReadyBeads(ready []beads.Bead, spec workflowServeControlReadySpec, env map[string]string) []hookBead {
	var out []hookBead
	for _, b := range workflowServeControlReadyFilter(spec, env).Apply(ready) {
		out = append(out, hookBead{ID: b.ID, Metadata: hookBeadMetadata(b.Metadata)})
	}
	return out
}

func workflowServeControlReadyAssignees(spec workflowServeControlReadySpec, env map[string]string) []string {
	var out []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range out {
			if existing == value {
				return
			}
		}
		out = append(out, value)
	}
	addWithLegacy := func(value string) {
		add(value)
		value = strings.TrimSpace(value)
		if strings.HasSuffix(value, config.ControlDispatcherAgentName) {
			add(strings.TrimSuffix(value, config.ControlDispatcherAgentName) + "workflow-control")
		}
	}
	addWithLegacy(spec.ControlSessionName)
	for _, key := range []string{"GC_CONTROL_SESSION_NAME", "GC_SESSION_NAME", "GC_ALIAS"} {
		addWithLegacy(controlReadyEnvIdentity(env, key))
	}
	addWithLegacy(spec.Target)
	addWithLegacy(controlReadyEnvIdentity(env, "GC_SESSION_ID"))
	return out
}

// controlReadyEnvIdentity resolves a live-session identity key for the
// control-ready assignee scan, preferring an explicit value in the serve env map
// and falling back to the live process environment. The pre-cache shell scan
// read GC_SESSION_NAME/GC_ALIAS/GC_SESSION_ID from os.Environ (via
// mergeRuntimeEnv), but the production controller serve env
// (controllerWorkQueryEnv) carries only store/bd coordinates, so without this
// fallback the live session identity is dropped and a control bead assigned to
// the live session name/alias/id is stranded. Mirrors the existing os.Getenv
// read of GC_NO_API in this path.
func controlReadyEnvIdentity(env map[string]string, key string) string {
	if v := strings.TrimSpace(env[key]); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(key))
}

func workflowServeControlReadyRoutes(spec workflowServeControlReadySpec) []string {
	var out []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range out {
			if existing == value {
				return
			}
		}
		out = append(out, value)
	}
	add(spec.Target)
	add(spec.LegacyTarget)
	// Pre-1.3 builds routed control work to the binding-stripped bare name;
	// scan it too so an upgraded qualified dispatcher claims that persisted
	// in-flight work instead of stranding it (mirrors the pre-cache shell
	// loop's GC_CONTROL_BARE_TARGET routed_ready scan).
	add(spec.BareTarget)
	return out
}

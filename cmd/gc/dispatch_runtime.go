package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/sling"
)

// graphExecutionRouteMetaKey is an alias for sling.GraphExecutionRouteMetaKey.
const graphExecutionRouteMetaKey = sling.GraphExecutionRouteMetaKey

// isControlDispatcherKind delegates to sling.IsControlDispatcherKind.
func isControlDispatcherKind(kind string) bool {
	return sling.IsControlDispatcherKind(kind)
}

// workflowExecutionRoute delegates to sling.WorkflowExecutionRoute.
func workflowExecutionRoute(bead beads.Bead) string {
	return sling.WorkflowExecutionRoute(bead)
}

// controlDispatcherBinding delegates to sling.ControlDispatcherBinding.
func controlDispatcherBinding(store beads.Store, cityName string, cfg *config.City, rigContext string) (sling.GraphRouteBinding, error) {
	deps := sling.SlingDeps{
		CityName: cityName,
		Store:    store,
		Cfg:      cfg,
		Resolver: cliAgentResolver{},
		Stderr:   os.Stderr,
	}
	return sling.ControlDispatcherBinding(store, cityName, cfg, rigContext, deps)
}

// assignGraphStepRoute delegates to sling.AssignGraphStepRoute.
func assignGraphStepRoute(step *formula.RecipeStep, executionBinding sling.GraphRouteBinding, controlBinding *sling.GraphRouteBinding) {
	sling.AssignGraphStepRoute(step, executionBinding, controlBinding)
}

// applyGraphRouting delegates to sling.ApplyGraphRouting with CLI interfaces.
func applyGraphRouting(recipe *formula.Recipe, a *config.Agent, routedTo string, vars map[string]string, sourceBeadID, scopeKind, scopeRef, storeRef string, store beads.Store, cityName, cityPath string, cfg *config.City) error {
	deps := sling.SlingDeps{
		CityName:              cityName,
		CityPath:              cityPath,
		Store:                 store,
		StoreRef:              storeRef,
		Cfg:                   cfg,
		Resolver:              cliAgentResolver{},
		DirectSessionResolver: cliDirectSessionResolver,
		Stderr:                os.Stderr,
	}
	return sling.ApplyGraphRouting(recipe, a, routedTo, vars, sourceBeadID, scopeKind, scopeRef, storeRef, store, cityName, cfg, deps)
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
	workflowServeWaitForWake       = waitForRelevantWorkflowWakeWithTrace
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
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()                                                                                //nolint:errcheck // best-effort trace log
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...)) //nolint:errcheck
}

func runWorkflowServe(agentName string, follow bool, _ io.Writer, stderr io.Writer) error {
	cityPath, err := resolveCity()
	if err != nil {
		return err
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return err
	}
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
	workEnv := controllerWorkQueryEnv(cityPath, cfg, &agentCfg)
	// Expand {{.Rig}}/{{.AgentBase}} once so the long-poll drain reuses the
	// rig-scoped command instead of passing the literal template to the shell
	// on every iteration. #793.
	workQuery := expandAgentCommandTemplate(cityPath, loadedCityName(cfg, cityPath), &agentCfg, cfg.Rigs, "work_query", agentCfg.EffectiveWorkQuery(), stderr)
	workflowTracef("serve start agent=%s city=%s dir=%s", agentCfg.QualifiedName(), cityPath, workDir)
	if !follow {
		_, err := drainWorkflowServeWork(agentCfg, cityPath, workDir, workQuery, workEnv, stderr)
		return err
	}
	return runWorkflowServeFollow(agentCfg, cityPath, workDir, workQuery, workEnv, stderr)
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
		queue, err := workflowServeList(workflowServeWorkQuery(agentCfg, workQuery), storePath, workEnv)
		if err != nil {
			workflowTracef("serve query-error agent=%s err=%v", agentCfg.QualifiedName(), err)
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
		legacyOversizedCount := 0
		for _, candidate := range queue {
			beadID := candidate.ID
			kind := strings.TrimSpace(candidate.Metadata["gc.kind"])
			if !isControlDispatcherKind(kind) {
				workflowTracef("serve unexpected-kind bead=%s kind=%s", beadID, kind)
				return result, fmt.Errorf("bead %s has unexpected non-control kind %q", beadID, kind)
			}
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
				if isLegacyOversizedControlEventError(err) {
					legacyOversizedCount++
					continue
				}
				return result, fmt.Errorf("processing control bead %s: %w", beadID, err)
			}
			workflowTracef("serve processed bead=%s kind=%s", beadID, kind)
			result.processedAny = true
			processedThisCycle = true
			break
		}
		if processedThisCycle {
			continue
		}
		if pendingCount > 0 {
			workflowTracef("serve pending-queue agent=%s count=%d", agentCfg.QualifiedName(), pendingCount)
			return result, nil
		}
		if legacyOversizedCount > 0 {
			workflowTracef("serve legacy-oversized-queue agent=%s count=%d", agentCfg.QualifiedName(), legacyOversizedCount)
			return result, nil
		}
	}
}

func isLegacyOversizedControlEventError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "recording attempt log") &&
		strings.Contains(msg, "old_value") &&
		strings.Contains(msg, "too large")
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
	for {
		drainResult, err := drainWorkflowServeWork(agentCfg, cityPath, storePath, workQuery, workEnv, stderr)
		if err != nil {
			return err
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
			return err
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
				return true, nil
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
	if agentCfg.WorkQuery == "" && isWorkflowServeControlDispatcherAgent(agentCfg) {
		return workflowServeControlReadyQuery(agentCfg)
	}
	workQuery := agentCfg.EffectiveWorkQuery()
	if len(expandedWorkQuery) > 0 {
		workQuery = expandedWorkQuery[0]
	}
	return workflowServeQuery(workQuery)
}

func isWorkflowServeControlDispatcherAgent(agentCfg config.Agent) bool {
	qualified := strings.TrimSpace(agentCfg.QualifiedName())
	return qualified == config.ControlDispatcherAgentName ||
		strings.HasSuffix(qualified, "/"+config.ControlDispatcherAgentName)
}

func workflowServeControlReadyQuery(agentCfg config.Agent) string {
	target := strings.TrimSpace(agentCfg.QualifiedName())
	if target == "" {
		target = config.ControlDispatcherAgentName
	}
	limit := fmt.Sprintf("%d", workflowServeScanLimit)
	query := `sh -c '` +
		`for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS" "` + target + `"; do ` +
		`[ -z "$id" ] && continue; ` +
		`legacy=""; case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac; ` +
		`for cand in "$id" "$legacy"; do ` +
		`[ -z "$cand" ] && continue; ` +
		`r=$(bd list --status in_progress --assignee="$cand" --json --limit=` + limit + ` 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`done; ` +
		`done; ` +
		`for id in "$GC_SESSION_ID" "$GC_SESSION_NAME" "$GC_ALIAS" "` + target + `"; do ` +
		`[ -z "$id" ] && continue; ` +
		`legacy=""; case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac; ` +
		`for cand in "$id" "$legacy"; do ` +
		`[ -z "$cand" ] && continue; ` +
		`r=$(bd ready --assignee="$cand" --json --limit=` + limit + ` 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; ` +
		`done; ` +
		`done; ` +
		`r=$(bd ready --metadata-field gc.routed_to=` + target +
		` --unassigned --json --limit=` + limit + ` 2>/dev/null); ` +
		`[ -n "$r" ] && [ "$r" != "[]" ] && printf "%s" "$r" && exit 0; `
	if legacy := workflowServeLegacyControlRoute(target); legacy != "" {
		query += `bd ready --metadata-field gc.routed_to=` + legacy +
			` --unassigned --json --limit=` + limit + ` 2>/dev/null'`
	} else {
		query += `printf "[]"` + `'`
	}
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

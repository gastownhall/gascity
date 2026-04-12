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
	}
	return sling.ApplyGraphRouting(recipe, a, routedTo, vars, sourceBeadID, scopeKind, scopeRef, storeRef, store, cityName, cfg, deps)
}

var (
	workflowServeList               = nextWorkflowServeBeads
	controlDispatcherServe          = runControlDispatcher
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
)

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
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		return err
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	if agentName == "" {
		agentName = os.Getenv("GC_ALIAS")
	}
	if agentName == "" {
		agentName = os.Getenv("GC_AGENT")
	}
	if agentName == "" {
		agentName = config.ControlDispatcherAgentName
	}
	agentCfg, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
	if !ok {
		return fmt.Errorf("agent %q not found in config", agentName)
	}
	workDir := agentCommandDir(cityPath, &agentCfg, cfg.Rigs)
	// Build rig-aware subprocess env for work queries (same pattern as
	// cmdHook) so rig-backed agents read the rig store, not an inherited
	// city-scoped BEADS_DIR. See issue #514.
	overrides := hookQueryEnv(cityPath, cfg, &agentCfg)
	queryEnv := mergeRuntimeEnv(os.Environ(), overrides)
	workflowTracef("serve start agent=%s city=%s dir=%s", agentCfg.QualifiedName(), cityPath, workDir)
	if !follow {
		return drainWorkflowServeWork(agentCfg, workDir, queryEnv, stderr)
	}
	return runWorkflowServeFollow(agentCfg, workDir, queryEnv, stderr)
}

func drainWorkflowServeWork(agentCfg config.Agent, workDir string, queryEnv []string, stderr io.Writer) error {
	processedAny := false
	idlePolls := 0
	for {
		queue, err := workflowServeList(workflowServeQuery(agentCfg.EffectiveWorkQuery()), workDir, queryEnv)
		if err != nil {
			workflowTracef("serve query-error agent=%s err=%v", agentCfg.QualifiedName(), err)
			return fmt.Errorf("querying control work for %s: %w", agentCfg.QualifiedName(), err)
		}
		if len(queue) == 0 {
			if processedAny && idlePolls < workflowServeIdlePollAttempts {
				idlePolls++
				workflowTracef("serve idle-retry agent=%s attempt=%d", agentCfg.QualifiedName(), idlePolls)
				time.Sleep(workflowServeIdlePollInterval)
				continue
			}
			workflowTracef("serve idle-exit agent=%s", agentCfg.QualifiedName())
			return nil
		}
		idlePolls = 0
		processedThisCycle := false
		pendingCount := 0
		for _, candidate := range queue {
			beadID := candidate.ID
			kind := strings.TrimSpace(candidate.Metadata["gc.kind"])
			if !isControlDispatcherKind(kind) {
				workflowTracef("serve unexpected-kind bead=%s kind=%s", beadID, kind)
				return fmt.Errorf("bead %s has unexpected non-control kind %q", beadID, kind)
			}
			workflowTracef("serve process bead=%s kind=%s", beadID, kind)
			if err := controlDispatcherServe(beadID, io.Discard, stderr); err != nil {
				if errors.Is(err, dispatch.ErrControlPending) {
					pendingCount++
					workflowTracef("serve pending bead=%s kind=%s", beadID, kind)
					continue
				}
				workflowTracef("serve process-error bead=%s kind=%s err=%v", beadID, kind, err)
				return fmt.Errorf("processing control bead %s: %w", beadID, err)
			}
			workflowTracef("serve processed bead=%s kind=%s", beadID, kind)
			processedAny = true
			processedThisCycle = true
			break
		}
		if processedThisCycle {
			continue
		}
		if pendingCount > 0 {
			workflowTracef("serve pending-queue agent=%s count=%d", agentCfg.QualifiedName(), pendingCount)
			return nil
		}
	}
}

func runWorkflowServeFollow(agentCfg config.Agent, workDir string, queryEnv []string, stderr io.Writer) error {
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

	for {
		if err := drainWorkflowServeWork(agentCfg, workDir, queryEnv, stderr); err != nil {
			return err
		}
		if err := waitForRelevantWorkflowWake(eventCh); err != nil {
			return err
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

func waitForRelevantWorkflowWake(eventCh <-chan workflowWatchResult) error {
	timer := time.NewTimer(workflowServeWakeSweepInterval)
	defer timer.Stop()

	for {
		select {
		case res := <-eventCh:
			if res.err != nil {
				return res.err
			}
			if workflowEventRelevant(res.evt) {
				workflowTracef("serve wake-event type=%s subject=%s", res.evt.Type, res.evt.Subject)
				return nil
			}
			workflowTracef("serve ignore-event type=%s subject=%s", res.evt.Type, res.evt.Subject)
		case <-timer.C:
			workflowTracef("serve wake-sweep")
			return nil
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

func nextWorkflowServeBeads(workQuery, dir string, env []string) ([]hookBead, error) {
	if workQuery == "" {
		return nil, nil
	}
	output, err := shellWorkQueryWithEnv(workQuery, dir, env)
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

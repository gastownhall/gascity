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
)

const graphExecutionRouteMetaKey = "gc.execution_routed_to"

func isControlDispatcherKind(kind string) bool {
	switch kind {
	case "check", "fanout", "retry-eval", "scope-check", "workflow-finalize", "retry", "ralph":
		return true
	default:
		return false
	}
}

func workflowExecutionRouteFromMeta(meta map[string]string) string {
	if meta == nil {
		return ""
	}
	if routedTo := strings.TrimSpace(meta[graphExecutionRouteMetaKey]); routedTo != "" {
		return routedTo
	}
	return strings.TrimSpace(meta["gc.routed_to"])
}

func workflowExecutionRoute(bead beads.Bead) string {
	return workflowExecutionRouteFromMeta(bead.Metadata)
}

func controlDispatcherBinding(store beads.Store, cityName string, cfg *config.City, rigContext string) (graphRouteBinding, error) {
	if cfg == nil {
		return graphRouteBinding{}, fmt.Errorf("control-dispatcher route requires config")
	}
	agentCfg, ok := resolveAgentIdentity(cfg, config.ControlDispatcherAgentName, rigContext)
	if !ok {
		return graphRouteBinding{}, fmt.Errorf("control-dispatcher agent %q not found", config.ControlDispatcherAgentName)
	}
	binding := graphRouteBinding{qualifiedName: agentCfg.QualifiedName()}
	if isMultiSessionCfgAgent(&agentCfg) {
		return binding, nil
	}
	sn := lookupSessionNameOrLegacy(store, cityName, agentCfg.QualifiedName(), cfg.Workspace.SessionTemplate)
	if sn == "" {
		return graphRouteBinding{}, fmt.Errorf("could not resolve session name for %q", agentCfg.QualifiedName())
	}
	binding.sessionName = sn
	return binding, nil
}

func applyGraphRouteBinding(step *formula.RecipeStep, binding graphRouteBinding) {
	step.Metadata["gc.routed_to"] = binding.qualifiedName
	if binding.label != "" {
		step.Labels = appendUniqueString(step.Labels, binding.label)
		step.Assignee = ""
		return
	}
	step.Assignee = binding.sessionName
}

func assignGraphStepRoute(step *formula.RecipeStep, executionBinding graphRouteBinding, controlBinding *graphRouteBinding) {
	if controlBinding != nil {
		if executionBinding.qualifiedName != "" {
			step.Metadata[graphExecutionRouteMetaKey] = executionBinding.qualifiedName
		} else {
			delete(step.Metadata, graphExecutionRouteMetaKey)
		}
		applyGraphRouteBinding(step, *controlBinding)
		return
	}
	delete(step.Metadata, graphExecutionRouteMetaKey)
	applyGraphRouteBinding(step, executionBinding)
}

// applyGraphRouting decorates a compiled recipe with routing metadata if it
// is a graph.v2 workflow. Sets gc.routed_to on all step beads so agents can
// discover routed work. No-op for non-graph recipes.
//
// Used by both the gc sling CLI path and the order dispatch path.
// For the sling path, pass the pre-resolved agent. For the order path,
// pass nil and the agent will be resolved from routedTo + config.
func applyGraphRouting(recipe *formula.Recipe, a *config.Agent, routedTo string, vars map[string]string, sourceBeadID, scopeKind, scopeRef, storeRef string, store beads.Store, cityName string, cfg *config.City) error {
	if !isCompiledGraphWorkflow(recipe) || cfg == nil {
		return nil
	}

	// Resolve agent if not provided (order dispatch path).
	if a == nil {
		rigContext := graphRouteRigContext(routedTo)
		baseName := routedTo
		if i := strings.LastIndex(routedTo, "/"); i >= 0 {
			baseName = routedTo[i+1:]
		}
		resolved, ok := resolveAgentIdentity(cfg, baseName, rigContext)
		if !ok {
			// Can't resolve agent — skip decoration rather than fail.
			return nil
		}
		a = &resolved
	}

	var sessionName string
	if !isMultiSessionCfgAgent(a) {
		sessionName = lookupSessionNameOrLegacy(store, cityName, a.QualifiedName(), cfg.Workspace.SessionTemplate)
		if sessionName == "" {
			return fmt.Errorf("could not resolve session name for %q", a.QualifiedName())
		}
	}
	routeVars := graphWorkflowRouteVars(recipe, vars)
	return decorateGraphWorkflowRecipe(recipe, routeVars, sourceBeadID, scopeKind, scopeRef, storeRef, routedTo, sessionName, store, cityName, cfg)
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
	workflowTracef("serve start agent=%s city=%s dir=%s", agentCfg.QualifiedName(), cityPath, workDir)
	if !follow {
		return drainWorkflowServeWork(agentCfg, workDir, stderr)
	}
	return runWorkflowServeFollow(agentCfg, workDir, stderr)
}

func drainWorkflowServeWork(agentCfg config.Agent, workDir string, stderr io.Writer) error {
	processedAny := false
	idlePolls := 0
	for {
		queue, err := workflowServeList(workflowServeQuery(agentCfg.EffectiveWorkQuery()), workDir)
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

func runWorkflowServeFollow(agentCfg config.Agent, workDir string, stderr io.Writer) error {
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
		if err := drainWorkflowServeWork(agentCfg, workDir, stderr); err != nil {
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

func nextWorkflowServeBeads(workQuery, dir string) ([]hookBead, error) {
	if workQuery == "" {
		return nil, nil
	}
	output, err := shellWorkQuery(workQuery, dir)
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

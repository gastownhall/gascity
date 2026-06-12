package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/user"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
)

// convergenceRequest is a command sent from the controller socket to the
// event loop for serialized processing.
type convergenceRequest struct {
	Command string            `json:"command"` // create, approve, iterate, stop, retry
	BeadID  string            `json:"bead_id"`
	User    string            `json:"user,omitempty"` // resolved client-side for audit attribution
	Params  map[string]string `json:"params"`         // command-specific parameters
	replyCh chan convergenceReply
}

// convergenceReply is the response from the event loop to a socket command.
type convergenceReply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// convergenceScope binds a convergence handler to one bead store — the
// city/HQ store or a single rig's store. The controller maintains one
// scope per store so a convergence loop is created, ticked, and reconciled
// in whichever store the operator's --rig context selected. The empty rig
// name ("") denotes the city/HQ scope.
type convergenceScope struct {
	rig       string // "" for the city/HQ store
	storePath string // city root for HQ, rig root for rig scopes
	store     beads.Store
	adapter   *convergenceStoreAdapter
	handler   *convergence.Handler
	// needsStartupReconcile keeps a scope eligible for retry when startup
	// reconciliation failed but the controller kept running with an empty
	// active index.
	needsStartupReconcile bool
}

// logSuffix returns a human-readable scope qualifier for log lines: empty
// for the city/HQ scope, " (rig <name>)" for a rig scope. Keeping the
// city-scope suffix empty leaves city-scope log lines byte-identical to
// the pre-rig-scoping output.
func (s *convergenceScope) logSuffix() string {
	if s.rig == "" {
		return ""
	}
	return " (rig " + s.rig + ")"
}

func (s *convergenceScope) triggerName(prefix string) string {
	if s.rig == "" {
		return prefix
	}
	return prefix + "-rig-" + s.rig
}

// initConvergenceHandler builds one convergence scope per available bead
// store: the city/HQ store plus every bound rig store. Called once during
// CityRuntime.run() initialization.
//
// TODO(#2403): rigs added by a later config reload are not picked up until
// the controller restarts — convScopes is not rebuilt on reload.
func (cr *CityRuntime) initConvergenceHandler() {
	cityStore := cr.cityBeadStore()
	if cityStore == nil {
		return
	}
	scopes := map[string]*convergenceScope{
		"": cr.newConvergenceScope("", cityStore, cr.cityPath, cr.cfg.FormulaLayers.City),
	}
	rigStorePaths := cr.convergenceRigStorePaths()
	for rigName, store := range cr.rigBeadStores() {
		if store == nil {
			continue
		}
		scopes[rigName] = cr.newConvergenceScope(
			rigName, store, rigStorePaths[rigName], cr.cfg.FormulaLayers.SearchPaths(rigName))
	}
	cr.convScopesMu.Lock()
	cr.convScopes = scopes
	cr.convScopesMu.Unlock()
}

// newConvergenceScope wires a store adapter and convergence handler for a
// single bead store. Each rig scope resolves formulas through that rig's
// formula search paths so rig-local formulas are honored.
func (cr *CityRuntime) newConvergenceScope(rig string, store beads.Store, storePath string, formulaSearchPaths []string) *convergenceScope {
	adapter := newConvergenceStoreAdapter(store, formulaSearchPaths)
	return &convergenceScope{
		rig:       rig,
		storePath: storePath,
		store:     store,
		adapter:   adapter,
		handler: &convergence.Handler{
			Store:     adapter,
			StorePath: storePath,
			Emitter:   &convergenceEventEmitter{rec: cr.rec},
		},
	}
}

func (cr *CityRuntime) convergenceRigStorePaths() map[string]string {
	rigs := cr.convergenceRigSnapshot()
	if len(rigs) == 0 {
		return nil
	}
	paths := make(map[string]string, len(rigs))
	for _, rig := range rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		paths[rig.Name] = resolveStoreScopeRoot(cr.cityPath, rig.Path)
	}
	return paths
}

func (cr *CityRuntime) convergenceRigSnapshot() []config.Rig {
	cr.serviceStateMu.RLock()
	defer cr.serviceStateMu.RUnlock()
	if cr.cfg == nil || len(cr.cfg.Rigs) == 0 {
		return nil
	}
	return append([]config.Rig(nil), cr.cfg.Rigs...)
}

func (cr *CityRuntime) convergenceMaxPerAgent() int {
	cr.serviceStateMu.RLock()
	defer cr.serviceStateMu.RUnlock()
	if cr.cfg == nil {
		return (config.ConvergenceConfig{}).MaxPerAgentOrDefault()
	}
	return cr.cfg.Convergence.MaxPerAgentOrDefault()
}

func (cr *CityRuntime) convergenceScopes() []*convergenceScope {
	if len(cr.convScopes) == 0 {
		return nil
	}
	keys := make([]string, 0, len(cr.convScopes))
	for key := range cr.convScopes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	scopes := make([]*convergenceScope, 0, len(keys))
	for _, key := range keys {
		if scope := cr.convScopes[key]; scope != nil {
			scopes = append(scopes, scope)
		}
	}
	return scopes
}

func unboundRigConvergenceError(rig string) error {
	return fmt.Errorf("rig %q is registered but has no bead store; "+
		"convergence loops require a bound rig", rig)
}

// convergenceScopeForRig returns the convergence scope for a rig name. An
// empty rig selects the city/HQ scope. An unknown or unbound rig is an
// error so a mistyped or unbound --rig fails loudly instead of silently
// writing the bead to HQ (the defect tracked in issue #2357). The error
// distinguishes a misspelled rig from a registered-but-unusable one.
func (cr *CityRuntime) convergenceScopeForRig(rig string) (*convergenceScope, error) {
	if cr.convScopes == nil {
		return nil, fmt.Errorf("convergence not available (no bead store)")
	}
	if scope, ok := cr.convScopes[rig]; ok {
		if rig == "" {
			return scope, nil
		}
		for _, candidate := range cr.convergenceRigSnapshot() {
			if candidate.Name != rig {
				continue
			}
			if strings.TrimSpace(candidate.Path) == "" {
				return nil, fmt.Errorf("rig %q became unbound after config reload but convergence scopes were not rebuilt; restart the controller (#2403)", rig)
			}
			currentPath := resolveStoreScopeRoot(cr.cityPath, candidate.Path)
			if currentPath != scope.storePath {
				return nil, fmt.Errorf("rig %q bead store changed after config reload from %q to %q; restart the controller (#2403)", rig, scope.storePath, currentPath)
			}
			return scope, nil
		}
		return nil, fmt.Errorf("rig %q was removed from city config but convergence scopes were not rebuilt; restart the controller (#2403)", rig)
	}
	for _, candidate := range cr.convergenceRigSnapshot() {
		if candidate.Name == rig {
			if strings.TrimSpace(candidate.Path) == "" {
				return nil, unboundRigConvergenceError(rig)
			}
			return nil, fmt.Errorf("rig %q is bound but convergence scopes were not rebuilt after config reload; restart the controller (#2403)", rig)
		}
	}
	return nil, fmt.Errorf("rig %q is not registered in this city", rig)
}

func (cr *CityRuntime) validateConvergenceScopeCurrent(scope *convergenceScope) error {
	if scope == nil {
		return fmt.Errorf("convergence scope is nil")
	}
	current, err := cr.convergenceScopeForRig(scope.rig)
	if err != nil {
		return err
	}
	if current != scope {
		if scope.rig == "" {
			return fmt.Errorf("city/HQ convergence scope was rebuilt; skipping stale cached scope")
		}
		return fmt.Errorf("rig %q convergence scope was rebuilt; skipping stale cached scope", scope.rig)
	}
	return nil
}

// convergenceTick processes active convergence loops in every scope — the
// city/HQ store and each bound rig store. Called from tick().
func (cr *CityRuntime) convergenceTick(ctx context.Context) {
	if cr.convScopes == nil || cr.convergenceReqCh == nil {
		return
	}
	for _, scope := range cr.convergenceScopes() {
		scope := scope
		cr.safeTick(func() {
			cr.convergenceTickScope(ctx, scope)
		}, scope.triggerName("convergence-tick"))
	}
}

// convergenceTickScope processes active convergence loops in a single
// scope by checking indexed beads for closed wisps and calling
// HandleWispClosed. Uses the scope's in-memory active index (O(active)
// instead of O(all beads)).
func (cr *CityRuntime) convergenceTickScope(ctx context.Context, scope *convergenceScope) {
	if scope == nil || scope.adapter == nil {
		return
	}
	if err := cr.validateConvergenceScopeCurrent(scope); err != nil {
		fmt.Fprintf(cr.stderr, "%s: convergence%s: skipping stale scope: %v\n", //nolint:errcheck
			cr.logPrefix, scope.logSuffix(), err)
		return
	}
	if scope.needsStartupReconcile {
		cr.convergenceStartupReconcileScope(ctx, scope)
		if scope.needsStartupReconcile {
			return
		}
	}
	if scope.adapter.activeIndex == nil {
		return
	}

	for _, beadID := range scope.adapter.activeBeadIDs() {
		meta, err := scope.adapter.GetMetadata(beadID)
		if err != nil {
			continue
		}
		// Trigger-gated loops are advanced by evaluating the trigger
		// condition; they have no active wisp to poll.
		if meta[convergence.FieldState] == convergence.StateWaitingTrigger {
			result, hErr := scope.handler.HandleTrigger(ctx, beadID)
			if hErr != nil {
				fmt.Fprintf(cr.stderr, "%s: convergence%s: HandleTrigger(%s): %v\n", //nolint:errcheck
					cr.logPrefix, scope.logSuffix(), beadID, hErr)
				continue
			}
			if result.Action != convergence.ActionSkipped {
				fmt.Fprintf(cr.stdout, "Convergence %s: %s (iteration %d)\n", //nolint:errcheck
					beadID, result.Action, result.Iteration)
			}
			continue
		}
		// Only process active beads; skip others like waiting_manual
		// that are indexed for CountActiveConvergenceLoops but not for tick.
		if meta[convergence.FieldState] != convergence.StateActive {
			continue
		}
		activeWisp := meta[convergence.FieldActiveWisp]
		if activeWisp == "" {
			continue
		}
		// Check if the active wisp is closed.
		wispInfo, wErr := scope.adapter.GetBead(activeWisp)
		if wErr != nil {
			if !errors.Is(wErr, beads.ErrNotFound) {
				continue
			}
			reconciler := &convergence.Reconciler{Handler: scope.handler}
			report, rErr := reconciler.ReconcileBeads(ctx, []string{beadID})
			if rErr != nil {
				fmt.Fprintf(cr.stderr, "%s: convergence%s: reconcile(%s): %v\n", //nolint:errcheck
					cr.logPrefix, scope.logSuffix(), beadID, rErr)
				continue
			}
			if len(report.Details) > 0 && report.Details[0].Error != nil {
				fmt.Fprintf(cr.stderr, "%s: convergence%s: reconcile(%s): %v\n", //nolint:errcheck
					cr.logPrefix, scope.logSuffix(), beadID, report.Details[0].Error)
			}
			continue
		}
		if wispInfo.Status != "closed" {
			continue
		}
		// Process the closed wisp.
		result, hErr := scope.handler.HandleWispClosed(ctx, beadID, activeWisp)
		if hErr != nil {
			fmt.Fprintf(cr.stderr, "%s: convergence%s: HandleWispClosed(%s, %s): %v\n", //nolint:errcheck
				cr.logPrefix, scope.logSuffix(), beadID, activeWisp, hErr)
			continue
		}
		if result.Action != convergence.ActionSkipped {
			fmt.Fprintf(cr.stdout, "Convergence %s: %s (iteration %d)\n", //nolint:errcheck
				beadID, result.Action, result.Iteration)
		}
	}
}

// processConvergenceRequests drains the convergence request channel and
// processes each command serially. Called from the event loop to serialize
// CLI commands with tick-based processing.
func (cr *CityRuntime) processConvergenceRequests(ctx context.Context) {
	if cr.convScopes == nil || cr.convergenceReqCh == nil {
		return
	}
	for {
		select {
		case req := <-cr.convergenceReqCh:
			reply := cr.safeHandleConvergenceRequest(ctx, req)
			req.replyCh <- reply
		default:
			return
		}
	}
}

// safeHandleConvergenceRequest wraps handleConvergenceRequest with panic
// recovery so a panicking handler doesn't leave replyCh unwritten and hang
// the socket handler goroutine.
func (cr *CityRuntime) safeHandleConvergenceRequest(ctx context.Context, req convergenceRequest) (reply convergenceReply) {
	defer func() {
		if r := recover(); r != nil {
			reply = convergenceReply{Error: fmt.Sprintf("internal error (panic): %v", r)}
			fmt.Fprintf(cr.stderr, "%s: convergence: panic handling %q for %s: %v\n", //nolint:errcheck
				cr.logPrefix, req.Command, req.BeadID, r)
		}
	}()
	reply = cr.handleConvergenceRequest(ctx, req)
	if reply.Error != "" {
		fmt.Fprintf(cr.stderr, "%s: convergence: %s %s: %s\n", //nolint:errcheck
			cr.logPrefix, req.Command, req.BeadID, reply.Error)
	}
	return reply
}

// handleConvergenceRequest dispatches a single convergence command to the
// scope selected by the request's "rig" parameter.
func (cr *CityRuntime) handleConvergenceRequest(ctx context.Context, req convergenceRequest) convergenceReply {
	if cr.convScopes == nil {
		return convergenceReply{Error: "convergence not available (no bead store)"}
	}

	// Use client-supplied username for audit attribution; fall back to
	// daemon user only if the client didn't provide one.
	username := req.User
	if username == "" {
		username = currentUsername()
	}

	switch req.Command {
	case "create":
		return cr.handleConvergenceCreate(ctx, req)
	case "approve", "iterate", "stop":
		scope, err := cr.convergenceScopeForRig(req.Params["rig"])
		if err != nil {
			return convergenceReply{Error: err.Error()}
		}
		return cr.handleConvergenceLifecycle(ctx, scope, req.Command, req.BeadID, username)
	case "retry":
		return cr.handleConvergenceRetry(ctx, req)
	default:
		return convergenceReply{Error: fmt.Sprintf("unknown convergence command: %q", req.Command)}
	}
}

// handleConvergenceLifecycle dispatches approve/iterate/stop to the
// convergence handler bound to the resolved scope's bead store.
func (cr *CityRuntime) handleConvergenceLifecycle(ctx context.Context, scope *convergenceScope, command, beadID, username string) convergenceReply {
	var (
		result convergence.HandlerResult
		err    error
	)
	switch command {
	case "approve":
		result, err = scope.handler.ApproveHandler(ctx, beadID, username, "")
	case "iterate":
		result, err = scope.handler.IterateHandler(ctx, beadID, username, "")
	case "stop":
		result, err = scope.handler.StopHandler(ctx, beadID, username, "")
	default:
		return convergenceReply{Error: fmt.Sprintf("unknown convergence command: %q", command)}
	}
	if err != nil {
		return convergenceReply{Error: err.Error()}
	}
	return marshalReply(result)
}

// handleConvergenceCreate processes a create command. The "rig" parameter
// selects the bead store: empty creates the loop in the city/HQ store, a
// rig name creates it in that rig's store.
func (cr *CityRuntime) handleConvergenceCreate(ctx context.Context, req convergenceRequest) convergenceReply {
	rig := req.Params["rig"]
	scope, err := cr.convergenceScopeForRig(rig)
	if err != nil {
		return convergenceReply{Error: err.Error()}
	}

	formula := req.Params["formula"]
	target := req.Params["target"]
	maxIter := 5
	if v, ok := convergence.DecodeInt(req.Params["max_iterations"]); ok && v > 0 {
		maxIter = v
	}

	gateMode := req.Params["gate_mode"]
	if gateMode == "" {
		gateMode = convergence.GateModeManual
	}

	// Concurrency checks are scoped to this store: each bead store accounts
	// for its own active convergence loops independently.
	maxPerAgent := cr.convergenceMaxPerAgent()
	if err := convergence.CheckConcurrencyLimits(scope.handler.Store, target, maxPerAgent); err != nil {
		return convergenceReply{Error: err.Error()}
	}
	if err := convergence.CheckNestedConvergence(scope.handler.Store, "", target); err != nil {
		return convergenceReply{Error: err.Error()}
	}

	// Build vars from params with "var." prefix.
	vars := make(map[string]string)
	for k, v := range req.Params {
		if len(k) > 4 && k[:4] == "var." {
			vars[k[4:]] = v
		}
	}

	params := convergence.CreateParams{
		Formula:           formula,
		Target:            target,
		MaxIterations:     maxIter,
		GateMode:          gateMode,
		GateCondition:     req.Params["gate_condition"],
		GateTimeout:       req.Params["gate_timeout"],
		GateTimeoutAction: req.Params["gate_timeout_action"],
		Title:             req.Params["title"],
		Vars:              vars,
		CityPath:          cr.cityPath,
		EvaluatePrompt:    req.Params["evaluate_prompt"],
		Trigger:           req.Params["trigger"],
		TriggerCondition:  req.Params["trigger_condition"],
		Rig:               rig,
	}

	result, err := scope.handler.CreateHandler(ctx, params)
	if err != nil {
		return convergenceReply{Error: err.Error()}
	}
	return marshalReply(result)
}

// handleConvergenceRetry processes a retry command. The retry loop is
// created in the same scope as the source loop, selected by the request's
// "rig" parameter.
func (cr *CityRuntime) handleConvergenceRetry(ctx context.Context, req convergenceRequest) convergenceReply {
	scope, err := cr.convergenceScopeForRig(req.Params["rig"])
	if err != nil {
		return convergenceReply{Error: err.Error()}
	}

	sourceBeadID := req.BeadID
	maxIter := 0
	if v, ok := convergence.DecodeInt(req.Params["max_iterations"]); ok && v > 0 {
		maxIter = v
	}

	// Read source bead metadata once for both max_iterations and target.
	meta, err := scope.handler.Store.GetMetadata(sourceBeadID)
	if err != nil {
		return convergenceReply{Error: fmt.Sprintf("reading source bead: %v", err)}
	}

	// If no max_iterations specified, read from source bead.
	if maxIter == 0 {
		if v, ok := convergence.DecodeInt(meta[convergence.FieldMaxIterations]); ok {
			maxIter = v
		}
		if maxIter == 0 {
			maxIter = 5
		}
	}

	target := meta[convergence.FieldTarget]

	// Concurrency checks.
	maxPerAgent := cr.convergenceMaxPerAgent()
	if err := convergence.CheckConcurrencyLimits(scope.handler.Store, target, maxPerAgent); err != nil {
		return convergenceReply{Error: err.Error()}
	}
	if err := convergence.CheckNestedConvergence(scope.handler.Store, "", target); err != nil {
		return convergenceReply{Error: err.Error()}
	}

	username := req.User
	if username == "" {
		username = currentUsername()
	}

	result, err := scope.handler.RetryHandler(ctx, sourceBeadID, username, maxIter)
	if err != nil {
		return convergenceReply{Error: err.Error()}
	}
	return marshalReply(result)
}

// convergenceStartupReconcile runs convergence bead reconciliation on
// startup for every scope and then populates each scope's in-memory
// active index.
func (cr *CityRuntime) convergenceStartupReconcile(ctx context.Context) {
	if cr.convScopes == nil || cr.convergenceReqCh == nil {
		return
	}
	for _, scope := range cr.convergenceScopes() {
		scope := scope
		run := func() {
			cr.convergenceStartupReconcileScope(ctx, scope)
		}
		trigger := scope.triggerName("convergence-startup-reconcile")
		if cr.safeTick(run, trigger) && scope.adapter.activeIndex == nil {
			cr.safeTick(run, trigger+"-retry")
		}
		if scope.adapter.activeIndex == nil {
			scope.needsStartupReconcile = true
			scope.adapter.activeIndex = map[string]string{}
		}
	}
}

// convergenceStartupReconcileScope reconciles interrupted convergence
// beads in one scope's store and then populates that scope's active index.
func (cr *CityRuntime) convergenceStartupReconcileScope(ctx context.Context, scope *convergenceScope) {
	if scope == nil {
		return
	}
	if err := cr.validateConvergenceScopeCurrent(scope); err != nil {
		fmt.Fprintf(cr.stderr, "%s: convergence reconcile%s: skipping stale scope: %v\n", //nolint:errcheck
			cr.logPrefix, scope.logSuffix(), err)
		return
	}
	// List() waits for CachingStore prime if not yet live, then serves
	// from memory. No subprocess stampede.
	all, err := scope.store.List(beads.ListQuery{Type: "convergence"})
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: convergence reconcile%s: listing beads: %v\n", //nolint:errcheck
			cr.logPrefix, scope.logSuffix(), err)
		scope.needsStartupReconcile = true
		scope.adapter.activeIndex = map[string]string{}
		return
	}

	var beadIDs []string
	for _, b := range all {
		beadIDs = append(beadIDs, b.ID)
	}

	if len(beadIDs) > 0 {
		reconciler := &convergence.Reconciler{Handler: scope.handler}
		report, err := reconciler.ReconcileBeads(ctx, beadIDs)
		if err != nil {
			fmt.Fprintf(cr.stderr, "%s: convergence reconciliation%s: %v\n", //nolint:errcheck
				cr.logPrefix, scope.logSuffix(), err)
			scope.needsStartupReconcile = true
			scope.adapter.activeIndex = map[string]string{}
			return
		}
		if report.Recovered > 0 || report.Errors > 0 {
			fmt.Fprintf(cr.stdout, "Convergence recovery%s: %d scanned, %d recovered, %d errors\n", //nolint:errcheck
				scope.logSuffix(), report.Scanned, report.Recovered, report.Errors)
		}
	}

	// Populate the active index after reconciliation so it reflects
	// post-recovery state.
	if err := scope.adapter.populateIndex(); err != nil {
		fmt.Fprintf(cr.stderr, "%s: convergence: populating active index%s: %v\n", //nolint:errcheck
			cr.logPrefix, scope.logSuffix(), err)
		scope.needsStartupReconcile = true
		scope.adapter.activeIndex = map[string]string{}
		return
	}
	scope.needsStartupReconcile = false
}

// sendConvergenceRequest sends a request through the controller socket and
// waits for a reply. Used by CLI commands.
func sendConvergenceRequest(cityPath string, req convergenceRequest) (convergenceReply, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return convergenceReply{}, fmt.Errorf("marshaling request: %w", err)
	}
	respBytes, err := sendControllerCommand(cityPath, "converge:"+string(data))
	if err != nil {
		return convergenceReply{}, err
	}
	var reply convergenceReply
	if err := json.Unmarshal(respBytes, &reply); err != nil {
		return convergenceReply{}, fmt.Errorf("parsing response: %w", err)
	}
	return reply, nil
}

func marshalReply(v any) convergenceReply {
	data, err := json.Marshal(v)
	if err != nil {
		return convergenceReply{Error: fmt.Sprintf("marshaling result: %v", err)}
	}
	return convergenceReply{Result: data}
}

func currentUsername() string {
	u, err := user.Current()
	if err != nil {
		return "unknown"
	}
	return u.Username
}

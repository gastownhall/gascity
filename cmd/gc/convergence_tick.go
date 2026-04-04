package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/user"

	"github.com/gastownhall/gascity/internal/beads"
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

// initConvergenceHandler creates the convergence handler if a bead store is
// available. Called once during CityRuntime.run() initialization.
func (cr *CityRuntime) initConvergenceHandler() {
	store := cr.cityBeadStore()
	if store == nil {
		return
	}
	adapter := newConvergenceStoreAdapter(store, cr.cfg.FormulaLayers.City)
	emitter := &convergenceEventEmitter{rec: cr.rec}
	cr.convStoreAdapter = adapter
	cr.convHandler = &convergence.Handler{
		Store:   adapter,
		Emitter: emitter,
	}
}

// convergenceTick processes active convergence loops by checking indexed
// beads for closed wisps and calling HandleWispClosed. Called from tick().
// Uses the in-memory active index (O(active) instead of O(all beads)).
func (cr *CityRuntime) convergenceTick(ctx context.Context) {
	if cr.convHandler == nil || cr.convergenceReqCh == nil {
		return
	}
	if cr.convStoreAdapter == nil || cr.convStoreAdapter.activeIndex == nil {
		return
	}

	for _, beadID := range cr.convStoreAdapter.activeBeadIDs() {
		meta, err := cr.convStoreAdapter.GetMetadata(beadID)
		if err != nil {
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
		wispInfo, wErr := cr.convStoreAdapter.GetBead(activeWisp)
		if wErr != nil {
			if !errors.Is(wErr, beads.ErrNotFound) {
				continue
			}
			reconciler := &convergence.Reconciler{Handler: cr.convHandler}
			report, rErr := reconciler.ReconcileBeads(ctx, []string{beadID})
			if rErr != nil {
				fmt.Fprintf(cr.stderr, "%s: convergence: reconcile(%s): %v\n", //nolint:errcheck
					cr.logPrefix, beadID, rErr)
				continue
			}
			if len(report.Details) > 0 && report.Details[0].Error != nil {
				fmt.Fprintf(cr.stderr, "%s: convergence: reconcile(%s): %v\n", //nolint:errcheck
					cr.logPrefix, beadID, report.Details[0].Error)
			}
			continue
		}
		if wispInfo.Status != "closed" {
			continue
		}
		// Process the closed wisp.
		result, hErr := cr.convHandler.HandleWispClosed(ctx, beadID, activeWisp)
		if hErr != nil {
			fmt.Fprintf(cr.stderr, "%s: convergence: HandleWispClosed(%s, %s): %v\n", //nolint:errcheck
				cr.logPrefix, beadID, activeWisp, hErr)
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
	if cr.convHandler == nil || cr.convergenceReqCh == nil {
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

// handleConvergenceRequest dispatches a single convergence command.
func (cr *CityRuntime) handleConvergenceRequest(ctx context.Context, req convergenceRequest) convergenceReply {
	if cr.convHandler == nil {
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
	case "approve":
		result, err := cr.convHandler.ApproveHandler(ctx, req.BeadID, username, "")
		if err != nil {
			return convergenceReply{Error: err.Error()}
		}
		return marshalReply(result)
	case "iterate":
		result, err := cr.convHandler.IterateHandler(ctx, req.BeadID, username, "")
		if err != nil {
			return convergenceReply{Error: err.Error()}
		}
		return marshalReply(result)
	case "stop":
		result, err := cr.convHandler.StopHandler(ctx, req.BeadID, username, "")
		if err != nil {
			return convergenceReply{Error: err.Error()}
		}
		return marshalReply(result)
	case "retry":
		return cr.handleConvergenceRetry(ctx, req)
	default:
		return convergenceReply{Error: fmt.Sprintf("unknown convergence command: %q", req.Command)}
	}
}

// handleConvergenceCreate processes a create command.
func (cr *CityRuntime) handleConvergenceCreate(ctx context.Context, req convergenceRequest) convergenceReply {
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

	// Concurrency checks.
	maxPerAgent := cr.cfg.Convergence.MaxPerAgentOrDefault()
	if err := convergence.CheckConcurrencyLimits(cr.convHandler.Store, target, maxPerAgent); err != nil {
		return convergenceReply{Error: err.Error()}
	}
	if err := convergence.CheckNestedConvergence(cr.convHandler.Store, "", target); err != nil {
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
	}

	result, err := cr.convHandler.CreateHandler(ctx, params)
	if err != nil {
		return convergenceReply{Error: err.Error()}
	}
	return marshalReply(result)
}

// handleConvergenceRetry processes a retry command.
func (cr *CityRuntime) handleConvergenceRetry(ctx context.Context, req convergenceRequest) convergenceReply {
	sourceBeadID := req.BeadID
	maxIter := 0
	if v, ok := convergence.DecodeInt(req.Params["max_iterations"]); ok && v > 0 {
		maxIter = v
	}

	// Read source bead metadata once for both max_iterations and target.
	meta, err := cr.convHandler.Store.GetMetadata(sourceBeadID)
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
	maxPerAgent := cr.cfg.Convergence.MaxPerAgentOrDefault()
	if err := convergence.CheckConcurrencyLimits(cr.convHandler.Store, target, maxPerAgent); err != nil {
		return convergenceReply{Error: err.Error()}
	}
	if err := convergence.CheckNestedConvergence(cr.convHandler.Store, "", target); err != nil {
		return convergenceReply{Error: err.Error()}
	}

	username := req.User
	if username == "" {
		username = currentUsername()
	}

	result, err := cr.convHandler.RetryHandler(ctx, sourceBeadID, username, maxIter)
	if err != nil {
		return convergenceReply{Error: err.Error()}
	}
	return marshalReply(result)
}

// convergenceStartupReconcile runs convergence bead reconciliation on startup
// and then populates the in-memory active index.
func (cr *CityRuntime) convergenceStartupReconcile(ctx context.Context) {
	if cr.convHandler == nil || cr.convergenceReqCh == nil {
		return
	}
	store := cr.cityBeadStore()
	if store == nil {
		return
	}

	// List() waits for CachingStore prime if not yet live, then serves
	// from memory. No subprocess stampede.
	all, err := store.ListOpen()
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: convergence reconcile: listing beads: %v\n", cr.logPrefix, err) //nolint:errcheck
		return
	}

	var beadIDs []string
	for _, b := range all {
		if b.Type == "convergence" && b.Status != "closed" {
			beadIDs = append(beadIDs, b.ID)
		}
	}

	if len(beadIDs) > 0 {
		reconciler := &convergence.Reconciler{Handler: cr.convHandler}
		report, err := reconciler.ReconcileBeads(ctx, beadIDs)
		if err != nil {
			fmt.Fprintf(cr.stderr, "%s: convergence reconciliation: %v\n", cr.logPrefix, err) //nolint:errcheck
			return
		}
		if report.Recovered > 0 || report.Errors > 0 {
			fmt.Fprintf(cr.stdout, "Convergence recovery: %d scanned, %d recovered, %d errors\n", //nolint:errcheck
				report.Scanned, report.Recovered, report.Errors)
		}
	}

	// Populate the active index after reconciliation so it reflects
	// post-recovery state.
	if cr.convStoreAdapter != nil {
		if err := cr.convStoreAdapter.populateIndex(); err != nil {
			fmt.Fprintf(cr.stderr, "%s: convergence: populating active index: %v\n", cr.logPrefix, err) //nolint:errcheck
		}
	}
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

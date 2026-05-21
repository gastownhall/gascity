package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/spf13/cobra"
)

func newConvergeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "converge",
		Short: "Manage convergence loops (bounded iterative refinement)",
		Long: `Convergence loops are bounded multi-step refinement cycles.

A root bead + formula + gate = repeat until the gate passes or max
iterations are reached. The controller processes wisp_closed events
and drives the loop automatically.`,
	}
	cmd.AddCommand(
		newConvergeCreateCmd(stdout, stderr),
		newConvergeStatusCmd(stdout, stderr),
		newConvergeApproveCmd(stdout, stderr),
		newConvergeIterateCmd(stdout, stderr),
		newConvergeStopCmd(stdout, stderr),
		newConvergeListCmd(stdout, stderr),
		newConvergeTestGateCmd(stdout, stderr),
		newConvergeRetryCmd(stdout, stderr),
	)
	return cmd
}

func newConvergeCreateCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		formula           string
		target            string
		maxIterations     int
		gateMode          string
		gateCondition     string
		gateTimeout       string
		gateTimeoutAction string
		title             string
		evaluatePrompt    string
		vars              []string
		jsonOutput        bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a convergence loop",
		RunE: func(_ *cobra.Command, _ []string) error {
			rctx, err := resolveContext()
			if err != nil {
				fmt.Fprintf(stderr, "gc converge create: %v\n", err) //nolint:errcheck
				return errExit
			}

			// Build params map. "rig" carries the resolved --rig context
			// so the controller creates the loop in the rig's bead store
			// instead of silently writing it to city/HQ (issue #2357).
			params := map[string]string{
				"formula":             formula,
				"target":              target,
				"max_iterations":      convergence.EncodeInt(maxIterations),
				"gate_mode":           gateMode,
				"gate_condition":      gateCondition,
				"gate_timeout":        gateTimeout,
				"gate_timeout_action": gateTimeoutAction,
				"title":               title,
				"evaluate_prompt":     evaluatePrompt,
				"rig":                 rctx.RigName,
			}
			for _, v := range vars {
				parts := strings.SplitN(v, "=", 2)
				if len(parts) != 2 {
					fmt.Fprintf(stderr, "gc converge create: invalid --var %q (expected key=value)\n", v) //nolint:errcheck
					return errExit
				}
				params["var."+parts[0]] = parts[1]
			}

			req := convergenceRequest{
				Command: "create",
				User:    currentUsername(),
				Params:  params,
			}
			reply, err := sendConvergenceRequest(rctx.CityPath, req)
			if err != nil {
				fmt.Fprintf(stderr, "gc converge create: %v\n", err) //nolint:errcheck
				return errExit
			}
			if reply.Error != "" {
				fmt.Fprintf(stderr, "gc converge create: %s\n", reply.Error) //nolint:errcheck
				return errExit
			}

			// Parse result for bead ID.
			var result convergence.CreateResult
			if err := json.Unmarshal(reply.Result, &result); err != nil {
				fmt.Fprintf(stderr, "gc converge create: parsing result: %v\n", err) //nolint:errcheck
				return errExit
			}
			if jsonOutput {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc converge create", convergeCreateJSONResult{
					SchemaVersion: "1",
					OK:            true,
					BeadID:        result.BeadID,
				})
			}
			fmt.Fprintln(stdout, result.BeadID) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVar(&formula, "formula", "", "Formula to use (required)")
	cmd.Flags().StringVar(&target, "target", "", "Target agent (required)")
	cmd.Flags().IntVar(&maxIterations, "max-iterations", 5, "Maximum iterations")
	cmd.Flags().StringVar(&gateMode, "gate", "manual", "Gate mode: manual, condition, hybrid")
	cmd.Flags().StringVar(&gateCondition, "gate-condition", "", "Path to gate condition script")
	cmd.Flags().StringVar(&gateTimeout, "gate-timeout", convergence.DefaultGateTimeout.String(), "Gate execution timeout")
	cmd.Flags().StringVar(&gateTimeoutAction, "gate-timeout-action", "iterate", "Action on gate timeout: iterate, retry, manual, terminate")
	cmd.Flags().StringVar(&title, "title", "", "Convergence loop title")
	cmd.Flags().StringVar(&evaluatePrompt, "evaluate-prompt", "", "Custom evaluate prompt (overrides formula default)")
	cmd.Flags().StringArrayVar(&vars, "var", nil, "Template variable (key=value, repeatable)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output JSONL summary")
	_ = cmd.MarkFlagRequired("formula")
	_ = cmd.MarkFlagRequired("target")
	return cmd
}

func newConvergeStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status <bead-id>",
		Short: "Show convergence loop status",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			beadID := args[0]
			store, _, _, code := openConvergeStore(stderr, "gc converge status")
			if code != 0 {
				return errExit
			}
			b, err := store.Get(beadID)
			if err != nil {
				fmt.Fprintf(stderr, "gc converge status: %v\n", err) //nolint:errcheck
				return errExit
			}
			if b.Type != "convergence" {
				fmt.Fprintf(stderr, "gc converge status: bead %s is type %q, not convergence\n", beadID, b.Type) //nolint:errcheck
				return errExit
			}

			meta := b.Metadata
			if meta == nil {
				meta = map[string]string{}
			}

			if jsonOutput {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc converge status", meta)
			}

			state := meta[convergence.FieldState]
			iteration, _ := convergence.DecodeInt(meta[convergence.FieldIteration])
			maxIter, _ := convergence.DecodeInt(meta[convergence.FieldMaxIterations])
			gateMode := meta[convergence.FieldGateMode]
			formula := meta[convergence.FieldFormula]
			target := meta[convergence.FieldTarget]
			rig := meta[convergence.FieldRig]
			gateOutcome := meta[convergence.FieldGateOutcome]
			waitingReason := meta[convergence.FieldWaitingReason]
			terminalReason := meta[convergence.FieldTerminalReason]
			activeWisp := meta[convergence.FieldActiveWisp]

			fmt.Fprintf(stdout, "ID:              %s\n", beadID)                //nolint:errcheck
			fmt.Fprintf(stdout, "Title:           %s\n", b.Title)               //nolint:errcheck
			fmt.Fprintf(stdout, "State:           %s\n", state)                 //nolint:errcheck
			fmt.Fprintf(stdout, "Iteration:       %d/%d\n", iteration, maxIter) //nolint:errcheck
			fmt.Fprintf(stdout, "Formula:         %s\n", formula)               //nolint:errcheck
			fmt.Fprintf(stdout, "Target:          %s\n", target)                //nolint:errcheck
			if rig != "" {
				fmt.Fprintf(stdout, "Rig:             %s\n", rig) //nolint:errcheck
			}
			fmt.Fprintf(stdout, "Gate:            %s\n", gateMode) //nolint:errcheck
			if gateOutcome != "" {
				fmt.Fprintf(stdout, "Gate Outcome:    %s\n", gateOutcome) //nolint:errcheck
			}
			if activeWisp != "" {
				fmt.Fprintf(stdout, "Active Wisp:     %s\n", activeWisp) //nolint:errcheck
			}
			if waitingReason != "" {
				fmt.Fprintf(stdout, "Waiting:         %s\n", waitingReason) //nolint:errcheck
			}
			if terminalReason != "" {
				fmt.Fprintf(stdout, "Terminal:        %s\n", terminalReason) //nolint:errcheck
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func newConvergeApproveCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "approve <bead-id>",
		Short: "Approve and close a convergence loop (manual gate)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return convergeSocketCmd(args[0], "approve", nil, jsonOutput, stdout, stderr)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output JSONL summary")
	return cmd
}

func newConvergeIterateCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "iterate <bead-id>",
		Short: "Force next iteration (manual gate)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return convergeSocketCmd(args[0], "iterate", nil, jsonOutput, stdout, stderr)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output JSONL summary")
	return cmd
}

func newConvergeStopCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "stop <bead-id>",
		Short: "Stop a convergence loop",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return convergeSocketCmd(args[0], "stop", nil, jsonOutput, stdout, stderr)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output JSONL summary")
	return cmd
}

func newConvergeListCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		all         bool
		allRigs     bool
		stateFilter string
		jsonOutput  bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List convergence loops",
		RunE: func(_ *cobra.Command, _ []string) error {
			type convEntry struct {
				ID        string `json:"id"`
				State     string `json:"state"`
				Iteration string `json:"iteration"`
				Gate      string `json:"gate"`
				Formula   string `json:"formula"`
				Target    string `json:"target"`
				Rig       string `json:"rig"`
				Title     string `json:"title"`
			}
			var entries []convEntry
			appendEntries := func(scopeRig string, store beads.Store) error {
				query := beads.ListQuery{Type: "convergence"}
				if all {
					query.IncludeClosed = true
				}
				beadList, err := store.List(query)
				if err != nil {
					return err
				}
				for _, b := range beadList {
					meta := b.Metadata
					if meta == nil {
						meta = map[string]string{}
					}
					state := meta[convergence.FieldState]
					if stateFilter != "" && state != stateFilter {
						continue
					}
					iter, _ := convergence.DecodeInt(meta[convergence.FieldIteration])
					maxIter, _ := convergence.DecodeInt(meta[convergence.FieldMaxIterations])
					rig := meta[convergence.FieldRig]
					if rig == "" {
						rig = scopeRig
					}
					entries = append(entries, convEntry{
						ID:        b.ID,
						State:     state,
						Iteration: fmt.Sprintf("%d/%d", iter, maxIter),
						Gate:      meta[convergence.FieldGateMode],
						Formula:   meta[convergence.FieldFormula],
						Target:    meta[convergence.FieldTarget],
						Rig:       rig,
						Title:     b.Title,
					})
				}
				return nil
			}

			hadScopeError := false
			if allRigs {
				rctx, err := resolveContext()
				if err != nil {
					fmt.Fprintf(stderr, "gc converge list: %v\n", err) //nolint:errcheck
					return errExit
				}
				store, err := openStoreAtForCity(rctx.CityPath, rctx.CityPath)
				if err != nil {
					fmt.Fprintf(stderr, "gc converge list: %v\n", err) //nolint:errcheck
					return errExit
				}
				if err := appendEntries("", store); err != nil {
					fmt.Fprintf(stderr, "gc converge list: %v\n", err) //nolint:errcheck
					return errExit
				}
				cfg, err := loadCityConfig(rctx.CityPath, io.Discard)
				if err != nil {
					fmt.Fprintf(stderr, "gc converge list: loading city config: %v\n", err) //nolint:errcheck
					return errExit
				}
				rigs := make([]string, 0, len(cfg.Rigs))
				rigPathByName := make(map[string]string, len(cfg.Rigs))
				for i := range cfg.Rigs {
					if strings.TrimSpace(cfg.Rigs[i].Path) == "" {
						continue
					}
					rigs = append(rigs, cfg.Rigs[i].Name)
					rigPathByName[cfg.Rigs[i].Name] = resolveStoreScopeRoot(rctx.CityPath, cfg.Rigs[i].Path)
				}
				sort.Strings(rigs)
				for _, rig := range rigs {
					store, err := openStoreAtForCity(rigPathByName[rig], rctx.CityPath)
					if err != nil {
						fmt.Fprintf(stderr, "gc converge list: rig %q: %v\n", rig, err) //nolint:errcheck
						hadScopeError = true
						continue
					}
					if err := appendEntries(rig, store); err != nil {
						fmt.Fprintf(stderr, "gc converge list: rig %q: %v\n", rig, err) //nolint:errcheck
						hadScopeError = true
						continue
					}
				}
			} else {
				store, _, _, code := openConvergeStore(stderr, "gc converge list")
				if code != 0 {
					return errExit
				}
				if err := appendEntries("", store); err != nil {
					fmt.Fprintf(stderr, "gc converge list: %v\n", err) //nolint:errcheck
					return errExit
				}
			}

			if jsonOutput {
				if err := writeCLIJSONLineOrErr(stdout, stderr, "gc converge list", struct {
					Entries []convEntry `json:"entries"`
				}{Entries: entries}); err != nil {
					return err
				}
				if hadScopeError {
					return errExit
				}
				return nil
			}

			if len(entries) == 0 {
				fmt.Fprintln(stdout, "No convergence loops found.") //nolint:errcheck
				if hadScopeError {
					return errExit
				}
				return nil
			}

			// Table output. The RIG column is empty for city/HQ-scoped loops.
			fmt.Fprintf(stdout, "%-14s %-10s %-10s %-10s %-26s %-16s %-16s %s\n", //nolint:errcheck
				"ID", "STATE", "ITERATION", "GATE", "FORMULA", "TARGET", "RIG", "TITLE")
			for _, e := range entries {
				fmt.Fprintf(stdout, "%-14s %-10s %-10s %-10s %-26s %-16s %-16s %s\n", //nolint:errcheck
					e.ID, e.State, e.Iteration, e.Gate, e.Formula, e.Target, e.Rig, e.Title)
			}
			if hadScopeError {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Include closed/terminated loops")
	cmd.Flags().BoolVar(&allRigs, "all-rigs", false, "List loops from city/HQ and every bound rig")
	cmd.Flags().StringVar(&stateFilter, "state", "", "Filter by state (active, waiting_manual, terminated)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func newConvergeTestGateCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "test-gate <bead-id>",
		Short: "Dry-run the gate condition (no state changes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			beadID := args[0]
			store, rctx, storePath, code := openConvergeStore(stderr, "gc converge test-gate")
			if code != 0 {
				return errExit
			}
			b, err := store.Get(beadID)
			if err != nil {
				fmt.Fprintf(stderr, "gc converge test-gate: %v\n", err) //nolint:errcheck
				return errExit
			}
			if b.Type != "convergence" {
				fmt.Fprintf(stderr, "gc converge test-gate: bead %s is type %q, not convergence\n", beadID, b.Type) //nolint:errcheck
				return errExit
			}
			meta := b.Metadata
			if meta == nil {
				meta = map[string]string{}
			}

			gateConfig, err := convergence.ParseGateConfig(meta)
			if err != nil {
				fmt.Fprintf(stderr, "gc converge test-gate: %v\n", err) //nolint:errcheck
				return errExit
			}

			if gateConfig.Mode == convergence.GateModeManual {
				if jsonOutput {
					return writeCLIJSONLineOrErr(stdout, stderr, "gc converge test-gate", convergeTestGateJSONResult{
						SchemaVersion: "1",
						OK:            true,
						BeadID:        beadID,
						Mode:          gateConfig.Mode,
						Skipped:       true,
						Reason:        "manual_gate",
					})
				}
				fmt.Fprintln(stdout, "Gate mode is manual — no condition to test.") //nolint:errcheck
				return nil
			}
			if gateConfig.Condition == "" {
				if jsonOutput {
					return writeCLIJSONLineOrErr(stdout, stderr, "gc converge test-gate", convergeTestGateJSONResult{
						SchemaVersion: "1",
						OK:            true,
						BeadID:        beadID,
						Mode:          gateConfig.Mode,
						Skipped:       true,
						Reason:        "missing_condition",
					})
				}
				fmt.Fprintln(stdout, "No gate condition configured.") //nolint:errcheck
				return nil
			}

			cityPath := rctx.CityPath
			iter, _ := convergence.DecodeInt(meta[convergence.FieldIteration])
			maxIter, _ := convergence.DecodeInt(meta[convergence.FieldMaxIterations])
			env := convergence.ConditionEnv{
				BeadID:        beadID,
				Iteration:     iter,
				MaxIterations: maxIter,
				WispID:        meta[convergence.FieldActiveWisp],
				CityPath:      cityPath,
				StorePath:     storePath,
				DocPath:       meta[convergence.VarPrefix+"doc_path"],
			}

			if !jsonOutput {
				fmt.Fprintf(stdout, "Testing gate: %s\n", gateConfig.Condition) //nolint:errcheck
			}
			result := convergence.RunCondition(
				context.TODO(),
				gateConfig.Condition, env, gateConfig.Timeout, 0,
			)
			if jsonOutput {
				payload := convergeTestGateJSONResult{
					SchemaVersion: "1",
					OK:            true,
					BeadID:        beadID,
					Mode:          gateConfig.Mode,
					Condition:     gateConfig.Condition,
					Outcome:       result.Outcome,
					ExitCode:      result.ExitCode,
					Stdout:        result.Stdout,
					Stderr:        result.Stderr,
				}
				return writeCLIJSONLineOrErr(stdout, stderr, "gc converge test-gate", payload)
			}
			fmt.Fprintf(stdout, "Outcome:  %s\n", result.Outcome) //nolint:errcheck
			if result.ExitCode != nil {
				fmt.Fprintf(stdout, "Exit:     %d\n", *result.ExitCode) //nolint:errcheck
			}
			if result.Stdout != "" {
				fmt.Fprintf(stdout, "Stdout:\n%s\n", result.Stdout) //nolint:errcheck
			}
			if result.Stderr != "" {
				fmt.Fprintf(stdout, "Stderr:\n%s\n", result.Stderr) //nolint:errcheck
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output JSONL summary")
	return cmd
}

func newConvergeRetryCmd(stdout, stderr io.Writer) *cobra.Command {
	var maxIterations int
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "retry <bead-id>",
		Short: "Retry a terminated convergence loop",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			rctx, err := resolveContext()
			if err != nil {
				fmt.Fprintf(stderr, "gc converge retry: %v\n", err) //nolint:errcheck
				return errExit
			}

			// "rig" routes the retry to the same bead store as the source
			// loop; the retry loop is created in that store.
			params := map[string]string{"rig": rctx.RigName}
			if maxIterations > 0 {
				params["max_iterations"] = convergence.EncodeInt(maxIterations)
			}

			req := convergenceRequest{
				Command: "retry",
				User:    currentUsername(),
				BeadID:  args[0],
				Params:  params,
			}
			reply, err := sendConvergenceRequest(rctx.CityPath, req)
			if err != nil {
				fmt.Fprintf(stderr, "gc converge retry: %v\n", err) //nolint:errcheck
				return errExit
			}
			if reply.Error != "" {
				fmt.Fprintf(stderr, "gc converge retry: %s\n", reply.Error) //nolint:errcheck
				return errExit
			}

			var result convergence.RetryResult
			if err := json.Unmarshal(reply.Result, &result); err != nil {
				fmt.Fprintf(stderr, "gc converge retry: parsing result: %v\n", err) //nolint:errcheck
				return errExit
			}
			if jsonOutput {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc converge retry", convergeRetryJSONResult{
					SchemaVersion: "1",
					OK:            true,
					SourceBeadID:  args[0],
					NewBeadID:     result.NewBeadID,
				})
			}
			fmt.Fprintln(stdout, result.NewBeadID) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().IntVar(&maxIterations, "max-iterations", 0, "Override max iterations (default: inherit from source)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output JSONL summary")
	return cmd
}

// convergeSocketCmd sends a simple convergence command (approve, iterate, stop)
// through the controller socket and prints the result. The resolved --rig
// context is forwarded as the "rig" parameter so the command targets the
// rig's bead store rather than always city/HQ.
func convergeSocketCmd(beadID, command string, params map[string]string, jsonOutput bool, stdout, stderr io.Writer) error {
	rctx, err := resolveContext()
	if err != nil {
		fmt.Fprintf(stderr, "gc converge %s: %v\n", command, err) //nolint:errcheck
		return errExit
	}
	if params == nil {
		params = map[string]string{}
	}
	params["rig"] = rctx.RigName

	req := convergenceRequest{
		Command: command,
		User:    currentUsername(),
		BeadID:  beadID,
		Params:  params,
	}
	reply, err := sendConvergenceRequest(rctx.CityPath, req)
	if err != nil {
		fmt.Fprintf(stderr, "gc converge %s: %v\n", command, err) //nolint:errcheck
		return errExit
	}
	if reply.Error != "" {
		fmt.Fprintf(stderr, "gc converge %s: %s\n", command, reply.Error) //nolint:errcheck
		return errExit
	}

	var result convergence.HandlerResult
	if err := json.Unmarshal(reply.Result, &result); err != nil {
		if jsonOutput {
			fmt.Fprintf(stderr, "gc converge %s: parsing result: %v\n", command, err) //nolint:errcheck
			return errExit
		}
		return nil
	}
	if jsonOutput {
		return writeCLIJSONLineOrErr(stdout, stderr, "gc converge "+command, convergeActionJSONResult{
			SchemaVersion: "1",
			OK:            true,
			BeadID:        beadID,
			Action:        string(result.Action),
		})
	}
	fmt.Fprintf(stdout, "%s: %s\n", beadID, result.Action) //nolint:errcheck
	return nil
}

type convergeCreateJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	BeadID        string `json:"bead_id"`
}

type convergeActionJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	BeadID        string `json:"bead_id"`
	Action        string `json:"action"`
}

type convergeRetryJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	SourceBeadID  string `json:"source_bead_id"`
	NewBeadID     string `json:"new_bead_id"`
}

type convergeTestGateJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	BeadID        string `json:"bead_id"`
	Mode          string `json:"mode"`
	Condition     string `json:"condition,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
	ExitCode      *int   `json:"exit_code,omitempty"`
	Stdout        string `json:"stdout,omitempty"`
	Stderr        string `json:"stderr,omitempty"`
	Skipped       bool   `json:"skipped,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// openConvergeStore opens the bead store for a read-side converge
// subcommand (status, list, test-gate), honoring --rig. With no rig
// context it opens the city/HQ store; with a rig context it opens that
// rig's store so rig-scoped convergence loops are visible. It also returns
// the resolved context for callers that need the city path.
func openConvergeStore(stderr io.Writer, cmdName string) (beads.Store, resolvedContext, string, int) {
	rctx, err := resolveContext()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck
		return nil, resolvedContext{}, "", 1
	}
	storePath, err := convergeStorePathForContext(rctx)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck
		return nil, resolvedContext{}, "", 1
	}
	store, err := openStoreAtForCity(storePath, rctx.CityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck
		return nil, resolvedContext{}, "", 1
	}
	return store, rctx, storePath, 0
}

func convergeStorePathForContext(rctx resolvedContext) (string, error) {
	if rctx.RigName == "" {
		return rctx.CityPath, nil
	}
	cfg, err := loadCityConfig(rctx.CityPath, io.Discard)
	if err != nil {
		return "", fmt.Errorf("loading city config: %w", err)
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name != rctx.RigName {
			continue
		}
		if strings.TrimSpace(cfg.Rigs[i].Path) == "" {
			return "", unboundRigConvergenceError(rctx.RigName)
		}
		return resolveStoreScopeRoot(rctx.CityPath, cfg.Rigs[i].Path), nil
	}
	return "", fmt.Errorf("rig %q is not registered in this city", rctx.RigName)
}

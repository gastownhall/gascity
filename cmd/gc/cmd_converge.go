package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a convergence loop",
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc converge create: %v\n", err) //nolint:errcheck
				return errExit
			}

			// Build params map.
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
			reply, err := sendConvergenceRequest(cityPath, req)
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
			fmt.Fprintln(stdout, result.BeadID) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVar(&formula, "formula", "", "Formula to use (required)")
	cmd.Flags().StringVar(&target, "target", "", "Target agent (required)")
	cmd.Flags().IntVar(&maxIterations, "max-iterations", 5, "Maximum iterations")
	cmd.Flags().StringVar(&gateMode, "gate", "manual", "Gate mode: manual, condition, hybrid")
	cmd.Flags().StringVar(&gateCondition, "gate-condition", "", "Path to gate condition script")
	cmd.Flags().StringVar(&gateTimeout, "gate-timeout", "30s", "Gate execution timeout")
	cmd.Flags().StringVar(&gateTimeoutAction, "gate-timeout-action", "iterate", "Action on gate timeout: iterate, retry, manual, terminate")
	cmd.Flags().StringVar(&title, "title", "", "Convergence loop title")
	cmd.Flags().StringVar(&evaluatePrompt, "evaluate-prompt", "", "Custom evaluate prompt (overrides formula default)")
	cmd.Flags().StringArrayVar(&vars, "var", nil, "Template variable (key=value, repeatable)")
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
			store, code := openCityStore(stderr, "gc converge status")
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
				data, _ := json.MarshalIndent(meta, "", "  ")
				fmt.Fprintln(stdout, string(data)) //nolint:errcheck
				return nil
			}

			state := meta[convergence.FieldState]
			iteration, _ := convergence.DecodeInt(meta[convergence.FieldIteration])
			maxIter, _ := convergence.DecodeInt(meta[convergence.FieldMaxIterations])
			gateMode := meta[convergence.FieldGateMode]
			formula := meta[convergence.FieldFormula]
			target := meta[convergence.FieldTarget]
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
			fmt.Fprintf(stdout, "Gate:            %s\n", gateMode)              //nolint:errcheck
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
	return &cobra.Command{
		Use:   "approve <bead-id>",
		Short: "Approve and close a convergence loop (manual gate)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return convergeSocketCmd(args[0], "approve", nil, stdout, stderr)
		},
	}
}

func newConvergeIterateCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "iterate <bead-id>",
		Short: "Force next iteration (manual gate)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return convergeSocketCmd(args[0], "iterate", nil, stdout, stderr)
		},
	}
}

func newConvergeStopCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <bead-id>",
		Short: "Stop a convergence loop",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return convergeSocketCmd(args[0], "stop", nil, stdout, stderr)
		},
	}
}

func newConvergeListCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		all         bool
		stateFilter string
		jsonOutput  bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List convergence loops",
		RunE: func(_ *cobra.Command, _ []string) error {
			store, code := openCityStore(stderr, "gc converge list")
			if code != 0 {
				return errExit
			}
			query := beads.ListQuery{Type: "convergence"}
			if all {
				query.IncludeClosed = true
			}
			beadList, err := store.List(query)
			if err != nil {
				fmt.Fprintf(stderr, "gc converge list: %v\n", err) //nolint:errcheck
				return errExit
			}

			type convEntry struct {
				ID        string `json:"id"`
				State     string `json:"state"`
				Iteration string `json:"iteration"`
				Gate      string `json:"gate"`
				Formula   string `json:"formula"`
				Target    string `json:"target"`
				Title     string `json:"title"`
			}
			var entries []convEntry
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
				entries = append(entries, convEntry{
					ID:        b.ID,
					State:     state,
					Iteration: fmt.Sprintf("%d/%d", iter, maxIter),
					Gate:      meta[convergence.FieldGateMode],
					Formula:   meta[convergence.FieldFormula],
					Target:    meta[convergence.FieldTarget],
					Title:     b.Title,
				})
			}

			if jsonOutput {
				data, _ := json.MarshalIndent(entries, "", "  ")
				fmt.Fprintln(stdout, string(data)) //nolint:errcheck
				return nil
			}

			if len(entries) == 0 {
				fmt.Fprintln(stdout, "No convergence loops found.") //nolint:errcheck
				return nil
			}

			// Table output.
			fmt.Fprintf(stdout, "%-14s %-10s %-10s %-10s %-26s %-16s %s\n", //nolint:errcheck
				"ID", "STATE", "ITERATION", "GATE", "FORMULA", "TARGET", "TITLE")
			for _, e := range entries {
				fmt.Fprintf(stdout, "%-14s %-10s %-10s %-10s %-26s %-16s %s\n", //nolint:errcheck
					e.ID, e.State, e.Iteration, e.Gate, e.Formula, e.Target, e.Title)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Include closed/terminated loops")
	cmd.Flags().StringVar(&stateFilter, "state", "", "Filter by state (active, waiting_manual, terminated)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	return cmd
}

func newConvergeTestGateCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "test-gate <bead-id>",
		Short: "Dry-run the gate condition (no state changes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			beadID := args[0]
			store, code := openCityStore(stderr, "gc converge test-gate")
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
				fmt.Fprintln(stdout, "Gate mode is manual — no condition to test.") //nolint:errcheck
				return nil
			}
			if gateConfig.Condition == "" {
				fmt.Fprintln(stdout, "No gate condition configured.") //nolint:errcheck
				return nil
			}

			cityPath, _ := resolveCity()
			iter, _ := convergence.DecodeInt(meta[convergence.FieldIteration])
			maxIter, _ := convergence.DecodeInt(meta[convergence.FieldMaxIterations])
			env := convergence.ConditionEnv{
				BeadID:        beadID,
				Iteration:     iter,
				MaxIterations: maxIter,
				WispID:        meta[convergence.FieldActiveWisp],
				CityPath:      cityPath,
				DocPath:       meta[convergence.VarPrefix+"doc_path"],
			}

			fmt.Fprintf(stdout, "Testing gate: %s\n", gateConfig.Condition) //nolint:errcheck
			result := convergence.RunCondition(
				context.TODO(),
				gateConfig.Condition, env, gateConfig.Timeout, 0,
			)
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
}

func newConvergeRetryCmd(stdout, stderr io.Writer) *cobra.Command {
	var maxIterations int
	cmd := &cobra.Command{
		Use:   "retry <bead-id>",
		Short: "Retry a terminated convergence loop",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc converge retry: %v\n", err) //nolint:errcheck
				return errExit
			}

			params := map[string]string{}
			if maxIterations > 0 {
				params["max_iterations"] = convergence.EncodeInt(maxIterations)
			}

			req := convergenceRequest{
				Command: "retry",
				User:    currentUsername(),
				BeadID:  args[0],
				Params:  params,
			}
			reply, err := sendConvergenceRequest(cityPath, req)
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
			fmt.Fprintln(stdout, result.NewBeadID) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().IntVar(&maxIterations, "max-iterations", 0, "Override max iterations (default: inherit from source)")
	return cmd
}

// convergeSocketCmd sends a simple convergence command (approve, iterate, stop)
// through the controller socket and prints the result.
func convergeSocketCmd(beadID, command string, params map[string]string, stdout, stderr io.Writer) error {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc converge %s: %v\n", command, err) //nolint:errcheck
		return errExit
	}

	req := convergenceRequest{
		Command: command,
		User:    currentUsername(),
		BeadID:  beadID,
		Params:  params,
	}
	reply, err := sendConvergenceRequest(cityPath, req)
	if err != nil {
		fmt.Fprintf(stderr, "gc converge %s: %v\n", command, err) //nolint:errcheck
		return errExit
	}
	if reply.Error != "" {
		fmt.Fprintf(stderr, "gc converge %s: %s\n", command, reply.Error) //nolint:errcheck
		return errExit
	}

	var result convergence.HandlerResult
	if err := json.Unmarshal(reply.Result, &result); err == nil {
		fmt.Fprintf(stdout, "%s: %s\n", beadID, result.Action) //nolint:errcheck
	}
	return nil
}

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/spf13/cobra"
)

const graphExecutionRouteMetaKey = "gc.execution_routed_to"

func isWorkflowControlKind(kind string) bool {
	switch kind {
	case "check", "fanout", "scope-check", "workflow-finalize":
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

func workflowControlBinding(store beads.Store, _, cityPath string, cfg *config.City) (graphRouteBinding, error) {
	if cfg == nil {
		return graphRouteBinding{}, fmt.Errorf("workflow-control route requires config")
	}
	agentCfg, ok := resolveAgentIdentity(cfg, config.WorkflowControlAgentName, "")
	if !ok {
		return graphRouteBinding{}, fmt.Errorf("workflow-control agent %q not found", config.WorkflowControlAgentName)
	}
	binding := graphRouteBinding{qualifiedName: agentCfg.QualifiedName()}
	if agentCfg.IsPool() {
		binding.label = "pool:" + agentCfg.QualifiedName()
		return binding, nil
	}
	sn, err := ensureSessionForTemplate(cityPath, cfg, store, agentCfg.QualifiedName(), io.Discard)
	if err != nil {
		return graphRouteBinding{}, err
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

var (
	workflowServeNext    = nextWorkflowServeBead
	workflowServeControl = runWorkflowControl
)

func newWorkflowServeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "serve [agent]",
		Short:  "Run one-shot graph.v2 workflow control work",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var agentName string
			if len(args) > 0 {
				agentName = args[0]
			}
			if err := runWorkflowServe(agentName, stdout, stderr); err != nil {
				fmt.Fprintf(stderr, "gc workflow serve: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	return cmd
}

type hookBead struct {
	ID       string            `json:"id"`
	Metadata map[string]string `json:"metadata"`
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

func runWorkflowServe(agentName string, _ io.Writer, stderr io.Writer) error {
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
		agentName = config.WorkflowControlAgentName
	}
	agentCfg, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
	if !ok {
		return fmt.Errorf("agent %q not found in config", agentName)
	}
	workDir := agentCommandDir(cityPath, &agentCfg, cfg.Rigs)
	workflowTracef("serve start agent=%s city=%s dir=%s", agentCfg.QualifiedName(), cityPath, workDir)

	for {
		beadID, kind, err := workflowServeNext(agentCfg.EffectiveWorkQuery(), workDir)
		if err != nil {
			workflowTracef("serve query-error agent=%s err=%v", agentCfg.QualifiedName(), err)
			return fmt.Errorf("querying control work for %s: %w", agentCfg.QualifiedName(), err)
		}
		if beadID == "" {
			workflowTracef("serve idle-exit agent=%s", agentCfg.QualifiedName())
			return nil
		}
		if !isWorkflowControlKind(kind) {
			workflowTracef("serve unexpected-kind bead=%s kind=%s", beadID, kind)
			return fmt.Errorf("bead %s has unexpected non-control kind %q", beadID, kind)
		}
		workflowTracef("serve process bead=%s kind=%s", beadID, kind)
		if err := workflowServeControl(beadID, io.Discard, stderr); err != nil {
			workflowTracef("serve process-error bead=%s kind=%s err=%v", beadID, kind, err)
			return fmt.Errorf("processing control bead %s: %w", beadID, err)
		}
		workflowTracef("serve processed bead=%s kind=%s", beadID, kind)
	}
}

func nextWorkflowServeBead(workQuery, dir string) (string, string, error) {
	if workQuery == "" {
		return "", "", nil
	}
	output, err := shellWorkQuery(workQuery, dir)
	if err != nil {
		return "", "", err
	}
	trimmed := strings.TrimSpace(output)
	if !workQueryHasReadyWork(trimmed) {
		return "", "", nil
	}
	var beadsOut []hookBead
	if err := json.Unmarshal([]byte(trimmed), &beadsOut); err == nil {
		if len(beadsOut) == 0 {
			return "", "", nil
		}
		return beadsOut[0].ID, strings.TrimSpace(beadsOut[0].Metadata["gc.kind"]), nil
	}
	var bead hookBead
	if err := json.Unmarshal([]byte(trimmed), &bead); err == nil {
		return bead.ID, strings.TrimSpace(bead.Metadata["gc.kind"]), nil
	}
	return "", "", fmt.Errorf("unexpected work query output: %s", trimmed)
}

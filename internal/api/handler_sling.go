package api

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type slingBody struct {
	Rig            string            `json:"rig"`
	Target         string            `json:"target"`
	Bead           string            `json:"bead"`
	Formula        string            `json:"formula"`
	AttachedBeadID string            `json:"attached_bead_id"`
	Title          string            `json:"title"`
	Vars           map[string]string `json:"vars"`
	ScopeKind      string            `json:"scope_kind"`
	ScopeRef       string            `json:"scope_ref"`
}

type slingResponse struct {
	Status         string `json:"status"`
	Target         string `json:"target"`
	Formula        string `json:"formula,omitempty"`
	Bead           string `json:"bead,omitempty"`
	WorkflowID     string `json:"workflow_id,omitempty"`
	RootBeadID     string `json:"root_bead_id,omitempty"`
	AttachedBeadID string `json:"attached_bead_id,omitempty"`
	Mode           string `json:"mode,omitempty"`
}

// slingCommandRunner is the function that executes gc sling as a subprocess.
// Replaceable in tests.
var slingCommandRunner = runSlingCommand

// execSling builds gc sling CLI args from the request body and shells out.
// Both plain-bead and workflow-backed launches use the same subprocess entry
// point so the HTTP API stays aligned with `gc sling`.
func (s *Server) execSling(
	ctx context.Context,
	body slingBody,
	defaultFormula string,
) (*slingResponse, int, string, string) {
	args := []string{"--city", s.state.CityPath(), "sling", body.Target}

	formulaName := strings.TrimSpace(body.Formula)
	attachedBeadID := strings.TrimSpace(body.AttachedBeadID)
	mode := "direct"
	workflowLaunch := false

	switch {
	case attachedBeadID != "":
		mode = "attached"
		workflowLaunch = true
		args = append(args, attachedBeadID, "--on", formulaName)
	case formulaName != "":
		mode = "standalone"
		workflowLaunch = true
		args = append(args, formulaName, "--formula")
	case strings.TrimSpace(body.Bead) != "" &&
		defaultFormula != "" &&
		(len(body.Vars) > 0 || body.Title != "" || body.ScopeKind != "" || body.ScopeRef != ""):
		mode = "attached"
		workflowLaunch = true
		attachedBeadID = strings.TrimSpace(body.Bead)
		formulaName = strings.TrimSpace(defaultFormula)
		args = append(args, attachedBeadID)
	default:
		args = append(args, body.Bead)
	}

	if workflowLaunch {
		if title := strings.TrimSpace(body.Title); title != "" {
			args = append(args, "--title", title)
		}
		if scopeKind := strings.TrimSpace(body.ScopeKind); scopeKind != "" {
			args = append(args, "--scope-kind", scopeKind)
		}
		if scopeRef := strings.TrimSpace(body.ScopeRef); scopeRef != "" {
			args = append(args, "--scope-ref", scopeRef)
		}
		if len(body.Vars) > 0 {
			keys := make([]string, 0, len(body.Vars))
			for key := range body.Vars {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				args = append(args, "--var", key+"="+body.Vars[key])
			}
		}
	}

	stdout, stderr, err := slingCommandRunner(ctx, s.state.CityPath(), args)
	if err != nil {
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = strings.TrimSpace(stdout)
		}
		if message == "" {
			message = err.Error()
		}
		return nil, http.StatusBadRequest, "invalid", message
	}

	resp := &slingResponse{
		Status: "slung",
		Target: body.Target,
		Bead:   body.Bead,
		Mode:   mode,
	}
	if !workflowLaunch {
		return resp, http.StatusOK, "", ""
	}

	resp.Formula = formulaName
	resp.AttachedBeadID = attachedBeadID
	workflowID := parseWorkflowIDFromSlingOutput(stdout)
	if workflowID == "" {
		workflowID = parseWorkflowIDFromSlingOutput(stderr)
	}
	if workflowID == "" {
		return nil, http.StatusInternalServerError, "internal", "gc sling did not report a workflow id"
	}
	resp.WorkflowID = workflowID
	resp.RootBeadID = workflowID
	return resp, http.StatusCreated, "", ""
}

func runSlingCommand(ctx context.Context, cityPath string, args []string) (string, string, error) {
	gcBin, err := os.Executable()
	if err != nil {
		return "", "", err
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, gcBin, args...)
	cmd.Dir = cityPath

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

func parseWorkflowIDFromSlingOutput(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"Started workflow ", "Attached workflow "} {
			if rest, ok := strings.CutPrefix(line, prefix); ok {
				workflowID, _, _ := strings.Cut(rest, " ")
				return strings.TrimSpace(workflowID)
			}
		}
		if rest, ok := strings.CutPrefix(line, "Slung formula "); ok {
			if _, afterRoot, found := strings.Cut(rest, "(wisp root "); found {
				workflowID, _, _ := strings.Cut(afterRoot, ")")
				return strings.TrimSpace(workflowID)
			}
		}
	}
	return ""
}

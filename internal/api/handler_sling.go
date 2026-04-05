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

func (s *Server) handleSling(w http.ResponseWriter, r *http.Request) {
	var body slingBody
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.Target == "" {
		writeError(w, http.StatusBadRequest, "invalid", "target agent or pool is required")
		return
	}

	cfg := s.state.Config()
	agentCfg, ok := findAgent(cfg, body.Target)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "target "+body.Target+" not found")
		return
	}

	if body.Bead == "" && body.Formula == "" {
		writeError(w, http.StatusBadRequest, "invalid", "bead or formula is required")
		return
	}
	if body.Bead != "" && body.Formula != "" {
		writeError(w, http.StatusBadRequest, "invalid", "bead and formula are mutually exclusive")
		return
	}
	if body.Bead != "" && body.AttachedBeadID != "" {
		writeError(w, http.StatusBadRequest, "invalid", "bead and attached_bead_id are mutually exclusive")
		return
	}

	body.ScopeKind = strings.TrimSpace(body.ScopeKind)
	body.ScopeRef = strings.TrimSpace(body.ScopeRef)
	workflowLaunchOptions := body.AttachedBeadID != "" ||
		len(body.Vars) > 0 ||
		body.Title != "" ||
		body.ScopeKind != "" ||
		body.ScopeRef != ""
	defaultFormulaLaunch := body.Formula == "" &&
		body.AttachedBeadID == "" &&
		body.Bead != "" &&
		agentCfg.DefaultSlingFormula != "" &&
		(len(body.Vars) > 0 || body.Title != "" || body.ScopeKind != "" || body.ScopeRef != "")
	if body.Formula == "" && body.AttachedBeadID != "" {
		writeError(w, http.StatusBadRequest, "invalid", "formula is required when attached_bead_id is provided")
		return
	}
	if body.Formula == "" && workflowLaunchOptions && !defaultFormulaLaunch {
		writeError(w, http.StatusBadRequest, "invalid", "formula or target default formula is required when vars, title, or scope are provided")
		return
	}
	if (body.ScopeKind == "") != (body.ScopeRef == "") {
		writeError(w, http.StatusBadRequest, "invalid", "scope_kind and scope_ref must be provided together")
		return
	}
	if body.ScopeKind != "" && body.ScopeKind != "city" && body.ScopeKind != "rig" {
		writeError(w, http.StatusBadRequest, "invalid", "scope_kind must be 'city' or 'rig'")
		return
	}

	resp, status, code, message := s.execSling(r.Context(), body, agentCfg.DefaultSlingFormula)
	if code != "" {
		writeError(w, status, code, message)
		return
	}
	writeJSON(w, status, resp)
}

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

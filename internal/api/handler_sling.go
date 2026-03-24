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
		writeError(w, http.StatusBadRequest, "invalid", "target agent is required")
		return
	}

	// Validate target agent exists in config.
	if _, ok := findAgent(s.state.Config(), body.Target); !ok {
		writeError(w, http.StatusNotFound, "not_found", "target agent "+body.Target+" not found")
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
	if body.Formula == "" && (body.AttachedBeadID != "" || len(body.Vars) > 0 || body.Title != "") {
		writeError(w, http.StatusBadRequest, "invalid", "formula is required when attached_bead_id, vars, or title are provided")
		return
	}

	// All sling paths go through `gc sling` CLI to ensure full semantics:
	// sling query execution, default formula application, auto-convoy,
	// controller poke, and nudge.
	resp, status, code, message := s.execSling(r.Context(), body)
	if code != "" {
		writeError(w, status, code, message)
		return
	}
	writeJSON(w, status, resp)
}

// execSling builds gc sling CLI args from the request body and shells out.
// Both plain-bead and formula paths use the same subprocess entry point so
// the HTTP API has identical semantics to the CLI.
func (s *Server) execSling(ctx context.Context, body slingBody) (*slingResponse, int, string, string) {
	args := []string{"--city", s.state.CityPath(), "sling", body.Target}

	isFormula := body.Formula != ""
	mode := "direct"

	if isFormula {
		if beadID := strings.TrimSpace(body.AttachedBeadID); beadID != "" {
			mode = "attached"
			args = append(args, beadID, "--on", body.Formula)
		} else {
			mode = "standalone"
			args = append(args, body.Formula, "--formula")
		}
		if title := strings.TrimSpace(body.Title); title != "" {
			args = append(args, "--title", title)
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
	} else {
		// Plain bead sling: gc sling <target> <bead>
		args = append(args, body.Bead)
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

	if isFormula {
		resp.Formula = body.Formula
		resp.AttachedBeadID = strings.TrimSpace(body.AttachedBeadID)
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

	return resp, http.StatusOK, "", ""
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
	}
	return ""
}

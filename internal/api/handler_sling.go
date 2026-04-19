package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
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
	Force          bool              `json:"force"`
}

type slingResponse struct {
	Status         string   `json:"status"`
	Target         string   `json:"target"`
	Formula        string   `json:"formula,omitempty"`
	Bead           string   `json:"bead,omitempty"`
	WorkflowID     string   `json:"workflow_id,omitempty"`
	RootBeadID     string   `json:"root_bead_id,omitempty"`
	AttachedBeadID string   `json:"attached_bead_id,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

// execSling calls the intent-based Sling API directly. The Huma handler
// humaHandleSling performs all validation before calling this.
//
// Return tuple:
//   - resp: the success body (nil when code != "")
//   - status: HTTP status for the success or error case
//   - code: short error code ("" on success)
//   - message: human-readable error message ("" on success)
//   - conflict: populated when code == "conflict"; carries the blocking
//     source_bead_id, workflow IDs, and cleanup hint the caller needs
//     to render a rich 409 Problem Details body. Returning it out-of-band
//     keeps Huma's structured error path available without widening the
//     (*slingResponse, int, string, string) shape every non-conflict
//     caller already consumes.
func (s *Server) execSling(ctx context.Context, body slingBody, _ string) (*slingResponse, int, string, string, *sourceworkflow.ConflictError) {
	cfg := s.state.Config()
	agentCfg, _ := findAgent(cfg, body.Target)

	formulaName := strings.TrimSpace(body.Formula)
	attachedBeadID := strings.TrimSpace(body.AttachedBeadID)

	// Build deps and construct Sling instance.
	store := s.findSlingStore(body.Rig, agentCfg)
	deps := sling.SlingDeps{
		CityName: s.state.CityName(),
		CityPath: s.state.CityPath(),
		Cfg:      s.state.Config(),
		SP:       s.state.SessionProvider(),
		Store:    store,
		StoreRef: s.slingStoreRef(body.Rig, agentCfg),
		SourceWorkflowStores: func() ([]sling.SourceWorkflowStore, error) {
			return s.sourceWorkflowStores(), nil
		},
		Runner:   s.slingRunner(),
		Resolver: apiAgentResolver{},
		Branches: apiBranchResolver{cityPath: s.state.CityPath()},
		Notify:   &apiNotifier{state: s.state},
	}
	sl, err := sling.New(deps)
	if err != nil {
		return nil, http.StatusInternalServerError, "internal", err.Error(), nil
	}

	// Build vars slice from map (sorted for determinism).
	var varSlice []string
	if len(body.Vars) > 0 {
		keys := make([]string, 0, len(body.Vars))
		for k := range body.Vars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			varSlice = append(varSlice, k+"="+body.Vars[k])
		}
	}

	formulaOpts := sling.FormulaOpts{
		Title:     strings.TrimSpace(body.Title),
		Vars:      varSlice,
		ScopeKind: body.ScopeKind,
		ScopeRef:  body.ScopeRef,
		Force:     body.Force,
	}

	// Dispatch to the right intent-based method.
	var result sling.SlingResult
	mode := "direct"
	workflowLaunch := false

	switch {
	case attachedBeadID != "":
		mode = "attached"
		workflowLaunch = true
		result, err = sl.AttachFormula(ctx, formulaName, attachedBeadID, agentCfg, formulaOpts)

	case formulaName != "":
		mode = "standalone"
		workflowLaunch = true
		result, err = sl.LaunchFormula(ctx, formulaName, agentCfg, formulaOpts)

	case strings.TrimSpace(body.Bead) != "" &&
		agentCfg.EffectiveDefaultSlingFormula() != "" &&
		(len(body.Vars) > 0 || body.Title != "" || body.ScopeKind != "" || body.ScopeRef != ""):
		mode = "attached"
		workflowLaunch = true
		attachedBeadID = strings.TrimSpace(body.Bead)
		formulaName = agentCfg.EffectiveDefaultSlingFormula()
		// Default formula: route the bead and let the domain apply the default.
		result, err = sl.RouteBead(ctx, attachedBeadID, agentCfg, sling.RouteOpts{Force: body.Force})

	default:
		result, err = sl.RouteBead(ctx, body.Bead, agentCfg, sling.RouteOpts{Force: body.Force})
	}

	if err != nil {
		var conflictErr *sourceworkflow.ConflictError
		if errors.As(err, &conflictErr) {
			return nil, http.StatusConflict, "conflict", err.Error(), conflictErr
		}
		return nil, http.StatusBadRequest, "invalid", err.Error(), nil
	}

	resp := &slingResponse{
		Status:   "slung",
		Target:   body.Target,
		Bead:     body.Bead,
		Mode:     mode,
		Warnings: result.MetadataErrors,
	}
	if !workflowLaunch {
		return resp, http.StatusOK, "", "", nil
	}

	resp.Formula = formulaName
	resp.AttachedBeadID = attachedBeadID
	// Use structured result fields directly -- no stdout parsing needed.
	resp.WorkflowID = result.WorkflowID
	resp.RootBeadID = result.BeadID
	if resp.WorkflowID == "" && resp.RootBeadID == "" {
		return nil, http.StatusInternalServerError, "internal", "sling did not produce a workflow or bead id", nil
	}
	return resp, http.StatusCreated, "", "", nil
}

// sourceWorkflowCleanupHint renders the CLI command that clears the blocking
// source workflow. Surfaced in the conflict response body so users can fix
// the state without grepping docs.
func sourceWorkflowCleanupHint(sourceBeadID, storeRef string) string {
	args := []string{"gc workflow delete-source", sourceBeadID}
	if storeRef = strings.TrimSpace(storeRef); storeRef != "" {
		args = append(args, "--store-ref", storeRef)
	}
	args = append(args, "--apply")
	return strings.Join(args, " ")
}

// findSlingStore returns the bead store for sling operations.
func (s *Server) findSlingStore(rig string, agentCfg config.Agent) beads.Store {
	if rig != "" {
		if store := s.state.BeadStore(rig); store != nil {
			return store
		}
	}
	if agentCfg.Dir != "" {
		if store := s.state.BeadStore(agentCfg.Dir); store != nil {
			return store
		}
	}
	return s.state.CityBeadStore()
}

// slingStoreRef returns a store ref string for the sling context.
func (s *Server) slingStoreRef(rig string, agentCfg config.Agent) string {
	if rig != "" {
		return "rig:" + rig
	}
	if agentCfg.Dir != "" {
		return "rig:" + agentCfg.Dir
	}
	return "city:" + s.state.CityName()
}

func (s *Server) sourceWorkflowStores() []sling.SourceWorkflowStore {
	stores := make([]sling.SourceWorkflowStore, 0, len(s.state.BeadStores())+1)
	if cityStore := s.state.CityBeadStore(); cityStore != nil {
		stores = append(stores, sling.SourceWorkflowStore{
			Store:    cityStore,
			StoreRef: "city:" + s.state.CityName(),
		})
	}
	for rigName, store := range s.state.BeadStores() {
		if store == nil {
			continue
		}
		stores = append(stores, sling.SourceWorkflowStore{
			Store:    store,
			StoreRef: "rig:" + rigName,
		})
	}
	return stores
}

// slingRunner returns the SlingRunner for the API context.
// Uses SlingRunnerFunc if set (for tests), otherwise a real shell runner.
func (s *Server) slingRunner() sling.SlingRunner {
	if s.SlingRunnerFunc != nil {
		return s.SlingRunnerFunc
	}
	return func(dir, command string, env map[string]string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		if dir != "" {
			cmd.Dir = dir
		}
		if len(env) > 0 {
			cmd.Env = mergeEnvForSling(env)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("running %q: %w", command, err)
		}
		return string(out), nil
	}
}

// mergeEnvForSling merges extra env vars into the current process env.
func mergeEnvForSling(extra map[string]string) []string {
	base := os.Environ()
	merged := make([]string, 0, len(base)+len(extra))
	merged = append(merged, base...)
	for k, v := range extra {
		merged = append(merged, k+"="+v)
	}
	return merged
}

// apiAgentResolver implements sling.AgentResolver for the API context.
// Mirrors the CLI's rig-context behavior for bare agent names while still
// delegating qualified and city-scoped lookups to findAgent.
type apiAgentResolver struct{}

func (apiAgentResolver) ResolveAgent(cfg *config.City, name, rigContext string) (config.Agent, bool) {
	if rigContext != "" && !strings.Contains(name, "/") {
		if a, ok := findAgent(cfg, rigContext+"/"+name); ok {
			return a, true
		}
	}
	return findAgent(cfg, name)
}

// qualifySlingTarget prepends a rig directory to a bare target when the
// caller supplied a rig context and the qualified form resolves.
func qualifySlingTarget(cfg *config.City, target, rigContext string) string {
	if rigContext == "" || strings.Contains(target, "/") {
		return target
	}
	qualified := rigContext + "/" + target
	if _, ok := findAgent(cfg, qualified); ok {
		return qualified
	}
	return target
}

// slingRigContext derives the effective rig context for target qualification.
// scope_ref wins for explicit rig scope; otherwise body.Rig is used for legacy
// dashboard dispatches that pass --rig without scope metadata.
func slingRigContext(body slingBody) string {
	if body.ScopeKind == "rig" && body.ScopeRef != "" {
		return body.ScopeRef
	}
	if body.ScopeKind == "" && body.Rig != "" {
		return body.Rig
	}
	return ""
}

// apiBranchResolver implements sling.BranchResolver for the API context.
// Uses the same git resolution as the CLI.
type apiBranchResolver struct {
	cityPath string
}

func (r apiBranchResolver) DefaultBranch(dir string) string {
	if dir == "" {
		dir = r.cityPath
	}
	// Best-effort: read git's origin/HEAD ref for the default branch.
	// Falls back to empty string if git is unavailable.
	out, err := exec.CommandContext(context.Background(), "git", "-C", dir,
		"symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/"))
}

// apiNotifier implements sling.Notifier for the API context.
type apiNotifier struct {
	state State
}

func (n *apiNotifier) PokeController(_ string) {
	n.state.Poke()
}

func (n *apiNotifier) PokeControlDispatch(_ string) {
	n.state.Poke()
}

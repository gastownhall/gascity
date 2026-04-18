package api

import (
	"context"
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

	// Scope fields are consulted for target qualification before the other
	// validation below, so trim them eagerly. The semantic checks happen
	// further down once all other fields are known.
	body.ScopeKind = strings.TrimSpace(body.ScopeKind)
	body.ScopeRef = strings.TrimSpace(body.ScopeRef)

	cfg := s.state.Config()

	// UI dispatches carry scope_kind/scope_ref but not the CLI's ambient
	// rig directory. Dashboard dispatches use body.Rig (from --rig=X).
	// Apply the same rig-prefixing convention for either so a bare
	// target resolves to the rig-scoped agent. Keeps formulas generic
	// (bare assignees) while routing to the correct rig-local beads
	// and worktree context.
	body.Target = qualifySlingTarget(cfg, body.Target, slingRigContext(body))

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

	workflowLaunchOptions := body.AttachedBeadID != "" ||
		len(body.Vars) > 0 ||
		body.Title != "" ||
		body.ScopeKind != "" ||
		body.ScopeRef != ""
	defaultFormulaLaunch := body.Formula == "" &&
		body.AttachedBeadID == "" &&
		body.Bead != "" &&
		agentCfg.EffectiveDefaultSlingFormula() != "" &&
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

	// Reject contradictory rig inputs. When scope_kind=rig, the
	// sling operation's effective rig must agree across every input
	// that drives routing or storage, because downstream:
	//   - findSlingStore picks the store from body.Rig → agentCfg.Dir → city
	//   - FormulaOpts.ScopeRef flows from body.ScopeRef
	// Silently accepting disagreement produces split-brain dispatches
	// (beads land in one rig, formula scope metadata references
	// another). This single guard covers three concrete classes:
	//   (a) qualified target in a different rig than scope_ref
	//       ("otherrig/worker" + scope_ref="myrig"): agentCfg.Dir="otherrig"
	//   (b) bare name fell through to a city-scoped agent while the
	//       caller requested rig scope ("mayor" + scope_ref="myrig"):
	//       agentCfg.Dir=""
	//   (c) body.Rig override disagreeing with scope_ref.
	if body.ScopeKind == "rig" && body.ScopeRef != "" {
		if agentCfg.Dir != body.ScopeRef {
			msg := "scope_ref " + body.ScopeRef + " conflicts with resolved target rig " + agentCfg.Dir
			if agentCfg.Dir == "" {
				msg = "scope_ref " + body.ScopeRef + " requires a rig-scoped target; resolved target " + body.Target + " is city-scoped"
			}
			writeError(w, http.StatusBadRequest, "invalid", msg)
			return
		}
		if body.Rig != "" && body.Rig != body.ScopeRef {
			writeError(w, http.StatusBadRequest, "invalid",
				"rig "+body.Rig+" conflicts with scope_ref "+body.ScopeRef)
			return
		}
	}

	resp, status, code, message := s.execSlingDirect(r.Context(), body, agentCfg)
	if code != "" {
		writeError(w, status, code, message)
		return
	}
	writeJSON(w, status, resp)
}

// execSlingDirect calls the intent-based Sling API directly.
func (s *Server) execSlingDirect(ctx context.Context, body slingBody, agentCfg config.Agent) (*slingResponse, int, string, string) {
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
		Runner:   s.slingRunner(),
		Resolver: apiAgentResolver{},
		Branches: apiBranchResolver{cityPath: s.state.CityPath()},
		Notify:   &apiNotifier{state: s.state},
	}
	sl, err := sling.New(deps)
	if err != nil {
		return nil, http.StatusInternalServerError, "internal", err.Error()
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
		result, err = sl.RouteBead(ctx, attachedBeadID, agentCfg, sling.RouteOpts{})

	default:
		result, err = sl.RouteBead(ctx, body.Bead, agentCfg, sling.RouteOpts{})
	}

	if err != nil {
		return nil, http.StatusBadRequest, "invalid", err.Error()
	}

	resp := &slingResponse{
		Status:   "slung",
		Target:   body.Target,
		Bead:     body.Bead,
		Mode:     mode,
		Warnings: result.MetadataErrors,
	}
	if !workflowLaunch {
		return resp, http.StatusOK, "", ""
	}

	resp.Formula = formulaName
	resp.AttachedBeadID = attachedBeadID
	// Use structured result fields directly -- no stdout parsing needed.
	resp.WorkflowID = result.WorkflowID
	resp.RootBeadID = result.BeadID
	if resp.WorkflowID == "" && resp.RootBeadID == "" {
		return nil, http.StatusInternalServerError, "internal", "sling did not produce a workflow or bead id"
	}
	return resp, http.StatusCreated, "", ""
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
// Mirrors the CLI's resolveAgentIdentity rig-context behavior (step 1)
// so formula child steps with bare assignees route to the same rig as
// the top-level target when dispatched through the API (e.g. gasworks-gui
// UI).
//
// Intentional divergence from cmd/gc/cmd_agent.go:resolveAgentIdentity:
//
//   - Step 3 (unambiguous bare-name fallback across all rigs) is
//     deliberately not implemented here. UI/API dispatches always carry
//     scope_kind+scope_ref when they want rig routing; without that
//     context, routing to a single rig-scoped agent by bare name would
//     be ambiguous in any multi-rig city. A bare name with no rig
//     context falls through to findAgent's qualified-or-city-scoped
//     lookup, which also retains findAgent's V2 BindingName pool-prefix
//     handling that agentutil.ResolveAgent does not currently implement.
//
//   - Unifying CLI and API onto agentutil.ResolveAgent is a follow-up;
//     doing it safely requires porting findAgent's V2 BindingName logic
//     into agentutil first. TestApiVsAgentutilResolverParity below
//     captures the current behavioral contract this resolver guarantees.
type apiAgentResolver struct{}

func (apiAgentResolver) ResolveAgent(cfg *config.City, name, rigContext string) (config.Agent, bool) {
	// Step 1: ambient rig match — if the caller supplied a rig context and
	// the name is bare, prefer the rig-scoped agent.
	if rigContext != "" && !strings.Contains(name, "/") {
		if a, ok := findAgent(cfg, rigContext+"/"+name); ok {
			return a, true
		}
	}
	// Step 2: literal lookup (qualified or city-scoped, plus V2
	// BindingName pool-member synthesis inside findAgent).
	return findAgent(cfg, name)
}

// qualifySlingTarget prepends a rig directory to a bare-name target
// when the caller supplies a non-empty rigContext. Returns the target
// unchanged if already qualified or if the qualified form does not
// resolve (the caller's final findAgent call will then surface a clean
// 404). This lets the API carry rig intent via scope_ref (UI path) or
// body.Rig (dashboard/--rig CLI path) without every caller composing
// the "<rig>/<name>" string manually.
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

// slingRigContext derives the effective rig context for target
// qualification. scope_ref takes precedence (explicit UI intent); when
// absent, body.Rig is used as the implicit rig for legacy dashboard
// dispatches that pass --rig= without scope_kind/scope_ref.
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

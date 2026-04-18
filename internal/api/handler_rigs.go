package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	gitpkg "github.com/gastownhall/gascity/internal/git"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/worker"
)

type rigResponse struct {
	Name         string     `json:"name"`
	Path         string     `json:"path"`
	Suspended    bool       `json:"suspended"`
	Prefix       string     `json:"prefix,omitempty"`
	AgentCount   int        `json:"agent_count"`
	RunningCount int        `json:"running_count"`
	LastActivity *time.Time `json:"last_activity,omitempty"`
	Git          *gitStatus `json:"git,omitempty"`
}

type gitStatus struct {
	Branch       string `json:"branch"`
	Clean        bool   `json:"clean"`
	ChangedFiles int    `json:"changed_files"`
	Ahead        int    `json:"ahead"`
	Behind       int    `json:"behind"`
}

func (s *Server) handleRigList(w http.ResponseWriter, r *http.Request) {
	bp := parseBlockingParams(r)
	if bp.isBlocking() {
		waitForChange(r.Context(), s.state.EventProvider(), bp)
	}

	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()
	wantGit := r.URL.Query().Get("git") == "true"
	store := s.state.CityBeadStore()

	rigs := make([]rigResponse, 0, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		resp := s.buildRigResponse(cfg, rig, store, sp, cityName, s.state.CityPath())
		if wantGit {
			resp.Git = fetchGitStatus(rig.Path)
		}
		rigs = append(rigs, resp)
	}
	writeListJSON(w, s.latestIndex(), rigs, len(rigs))
}

func (s *Server) handleRig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	wantGit := r.URL.Query().Get("git") == "true"
	store := s.state.CityBeadStore()

	for _, rig := range cfg.Rigs {
		if rig.Name == name {
			resp := s.buildRigResponse(cfg, rig, store, sp, s.state.CityName(), s.state.CityPath())
			if wantGit {
				resp.Git = fetchGitStatus(rig.Path)
			}
			writeIndexJSON(w, s.latestIndex(), resp)
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "rig "+name+" not found")
}

func (s *Server) handleRigAction(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	action := r.PathValue("action")

	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	var err error
	switch action {
	case "suspend":
		err = sm.SuspendRig(name)
	case "resume":
		err = sm.ResumeRig(name)
	case "restart":
		s.handleRigRestart(w, name)
		return
	default:
		writeError(w, http.StatusNotFound, "not_found", "unknown rig action: "+action)
		return
	}

	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "action": action, "rig": name})
}

// handleRigRestart kills all agents in a rig so the reconciler restarts them.
func (s *Server) handleRigRestart(w http.ResponseWriter, name string) {
	cfg := s.state.Config()
	sp := s.state.SessionProvider()
	cityName := s.state.CityName()
	store := s.state.CityBeadStore()

	// Verify rig exists.
	rigFound := false
	for _, rig := range cfg.Rigs {
		if rig.Name == name {
			rigFound = true
			break
		}
	}
	if !rigFound {
		writeError(w, http.StatusNotFound, "not_found", "rig "+name+" not found")
		return
	}

	// Best-effort kill: the agent set may change between config read and each
	// Stop call (pool scaling, config reload). The reconciler is the
	// convergence mechanism — survivors will be caught on its next tick.
	killed := make([]string, 0)
	failed := make([]string, 0)
	for _, a := range cfg.Agents {
		if workdirutil.ConfiguredRigName(s.state.CityPath(), a, cfg.Rigs) != name {
			continue
		}
		expanded := expandAgent(a, cityName, cfg.Workspace.SessionTemplate, sp)
		for _, ea := range expanded {
			sessionName := agentSessionName(cityName, ea.qualifiedName, cfg.Workspace.SessionTemplate)
			handle, err := s.workerHandleForSessionTarget(store, sessionName)
			if err == nil {
				err = handle.Kill(context.Background())
			}
			if err != nil {
				// "not found" / "not running" are benign — agent wasn't running.
				if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "not running") {
					failed = append(failed, ea.qualifiedName)
				}
			} else {
				killed = append(killed, ea.qualifiedName)
			}
		}
	}
	resp := map[string]any{
		"status": "ok",
		"action": "restart",
		"rig":    name,
		"killed": killed,
	}
	httpStatus := http.StatusOK
	if len(failed) > 0 {
		resp["failed"] = failed
		if len(killed) == 0 {
			// Total failure — no agents were killed.
			resp["status"] = "failed"
			httpStatus = http.StatusInternalServerError
		} else {
			resp["status"] = "partial"
		}
	}
	writeJSON(w, httpStatus, resp)
}

// buildRigResponse creates a rigResponse with agent counts and last activity.
func (s *Server) buildRigResponse(cfg *config.City, rig config.Rig, store beads.Store, sp sessionLister, cityName, cityPath string) rigResponse {
	tmpl := cfg.Workspace.SessionTemplate
	var agentCount, runningCount int
	var maxActivity time.Time

	for _, a := range cfg.Agents {
		if workdirutil.ConfiguredRigName(cityPath, a, cfg.Rigs) != rig.Name {
			continue
		}
		expanded := expandAgent(a, cityName, tmpl, sp)
		for _, ea := range expanded {
			agentCount++
			sessionName := agent.SessionNameFor(cityName, ea.qualifiedName, tmpl)
			handle, _ := s.workerHandleForSessionTarget(store, sessionName)
			obs, _ := worker.ObserveHandle(context.Background(), handle)
			if obs.Running {
				runningCount++
			}
			if obs.LastActivity != nil && obs.LastActivity.After(maxActivity) {
				maxActivity = *obs.LastActivity
			}
		}
	}

	resp := rigResponse{
		Name:         rig.Name,
		Path:         rig.Path,
		Suspended:    s.rigSuspended(cfg, rig, store, sp, cityName, cityPath),
		Prefix:       rig.Prefix,
		AgentCount:   agentCount,
		RunningCount: runningCount,
	}
	if !maxActivity.IsZero() {
		resp.LastActivity = &maxActivity
	}
	return resp
}

// rigSuspended computes effective suspended state for a rig by merging config
// and runtime session metadata. A rig is suspended if the config says so, or
// if all its agents are runtime-suspended via session metadata.
func (s *Server) rigSuspended(cfg *config.City, rig config.Rig, store beads.Store, sp sessionLister, cityName, cityPath string) bool {
	if rig.Suspended {
		return true
	}
	tmpl := cfg.Workspace.SessionTemplate
	var agentCount, suspendedCount int
	for _, a := range cfg.Agents {
		if workdirutil.ConfiguredRigName(cityPath, a, cfg.Rigs) != rig.Name {
			continue
		}
		expanded := expandAgent(a, cityName, tmpl, sp)
		for _, ea := range expanded {
			agentCount++
			sessionName := agent.SessionNameFor(cityName, ea.qualifiedName, tmpl)
			handle, _ := s.workerHandleForSessionTarget(store, sessionName)
			obs, _ := worker.ObserveHandle(context.Background(), handle)
			if obs.Suspended {
				suspendedCount++
			}
		}
	}
	return agentCount > 0 && suspendedCount == agentCount
}

// gitStatusTimeout bounds how long git operations can take per rig.
const gitStatusTimeout = 3 * time.Second

// fetchGitStatus uses internal/git to get branch/status/ahead-behind info.
// Returns nil on any error or timeout (rig may not be a git repo).
// The context-based timeout ensures that git subprocesses are killed on
// expiry, preventing goroutine and process leaks.
func fetchGitStatus(path string) *gitStatus {
	ctx, cancel := context.WithTimeout(context.Background(), gitStatusTimeout)
	defer cancel()
	return fetchGitStatusCtx(ctx, path)
}

func fetchGitStatusCtx(ctx context.Context, path string) *gitStatus {
	g := gitpkg.New(path)
	if !g.IsRepoCtx(ctx) {
		return nil
	}

	branch, err := g.CurrentBranchCtx(ctx)
	if err != nil {
		return nil
	}

	porcelain, err := g.StatusPorcelainCtx(ctx)
	if err != nil {
		return nil
	}

	var changedFiles int
	for _, line := range strings.Split(porcelain, "\n") {
		if strings.TrimSpace(line) != "" {
			changedFiles++
		}
	}

	gs := &gitStatus{
		Branch:       branch,
		Clean:        changedFiles == 0,
		ChangedFiles: changedFiles,
	}

	// Ahead/behind (best-effort — fails if no upstream set).
	ahead, behind, err := g.AheadBehindCtx(ctx)
	if err == nil {
		gs.Ahead = ahead
		gs.Behind = behind
	}

	return gs
}

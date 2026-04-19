package api

import (
	"context"
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

package api

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	gitpkg "github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

type rigResponse struct {
	Name          string     `json:"name"`
	Path          string     `json:"path"`
	Suspended     bool       `json:"suspended"`
	Prefix        string     `json:"prefix,omitempty"`
	DefaultBranch string     `json:"default_branch,omitempty"`
	AgentCount    int        `json:"agent_count"`
	RunningCount  int        `json:"running_count"`
	LastActivity  *time.Time `json:"last_activity,omitempty"`
	Git           *gitStatus `json:"git,omitempty"`
}

type gitStatus struct {
	Branch       string `json:"branch"`
	Clean        bool   `json:"clean"`
	ChangedFiles int    `json:"changed_files"`
	Ahead        int    `json:"ahead"`
	Behind       int    `json:"behind"`
}

// buildRigResponse creates a rigResponse with agent counts and last activity.
//
// Every configured session slot is observed exactly once per response: the
// running count, the last-activity high-water mark and the all-agents-
// suspended input to rigSuspended all come from that single pass. On the
// tmux provider each observation is several subprocesses, and the supervisor
// serves /rigs to every hook and order script in the city, so a second pass
// per field doubled the per-request fork count (sys-4za3nm).
func (s *Server) buildRigResponse(cfg *config.City, rig config.Rig, sp runtime.Provider, cityName, cityPath string) rigResponse {
	tmpl := cfg.Workspace.SessionTemplate
	var agentCount, runningCount, suspendedCount int
	var maxActivity time.Time

	for _, a := range cfg.Agents {
		if workdirutil.ConfiguredRigName(cityPath, a, cfg.Rigs) != rig.Name {
			continue
		}
		processNames := config.AgentProcessNames(cfg, a, exec.LookPath)
		expanded := expandAgent(a, cityName, tmpl, sp)
		for _, ea := range expanded {
			agentCount++
			sessionName := agent.SessionNameFor(cityName, ea.qualifiedName, tmpl)
			obs := observeProviderSession(sp, sessionName, processNames)
			if obs.Running {
				runningCount++
			}
			if obs.Suspended {
				suspendedCount++
			}
			if obs.LastActivity != nil && obs.LastActivity.After(maxActivity) {
				maxActivity = *obs.LastActivity
			}
		}
	}

	resp := rigResponse{
		Name:          rig.Name,
		Path:          rig.Path,
		Suspended:     rigSuspended(rig, cityPath, agentCount, suspendedCount),
		Prefix:        rig.Prefix,
		DefaultBranch: rig.DefaultBranch,
		AgentCount:    agentCount,
		RunningCount:  runningCount,
	}
	if !maxActivity.IsZero() {
		resp.LastActivity = &maxActivity
	}
	return resp
}

// rigSuspended computes the effective suspended state for a rig. A rig
// is suspended if the runtime state file records an explicit "suspended"
// preference, if the rig's SuspendedOnStart applies with no overriding
// runtime entry, or if all its agents are runtime-suspended via session
// metadata (agentCount/suspendedCount, observed by the caller). The
// deprecated `[[rig]] suspended` field in city.toml is intentionally NOT
// consulted — `gc doctor` surfaces it as a migration target.
func rigSuspended(rig config.Rig, cityPath string, agentCount, suspendedCount int) bool {
	if rs, err := suspensionstate.Load(fsys.OSFS{}, cityPath); err == nil &&
		suspensionstate.EffectiveRigSuspended(rs, rig.Name, rig.EffectiveSuspendedOnStart()) {
		return true
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

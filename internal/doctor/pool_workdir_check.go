package doctor

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/validation"
)

// PoolWorkDirIsolationCheck warns when multi-session agents would place
// parallel instances in one shared working directory (data-loss risk on
// branch switch / reset / stash).
type PoolWorkDirIsolationCheck struct {
	cfg      *config.City
	cityPath string
}

// NewPoolWorkDirIsolationCheck builds the doctor check for shared pool work dirs.
func NewPoolWorkDirIsolationCheck(cfg *config.City, cityPath string) *PoolWorkDirIsolationCheck {
	return &PoolWorkDirIsolationCheck{cfg: cfg, cityPath: cityPath}
}

// Name returns the check identifier.
func (c *PoolWorkDirIsolationCheck) Name() string { return "pool-workdir-isolation" }

// CanFix reports that this check is advisory only.
func (c *PoolWorkDirIsolationCheck) CanFix() bool { return false }

// Fix is a no-op; isolation is fixed by setting a per-instance work_dir.
func (c *PoolWorkDirIsolationCheck) Fix(_ *CheckContext) error { return nil }

// Run warns when any multi-session agent shares one work dir across instances.
func (c *PoolWorkDirIsolationCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if c.cfg == nil {
		r.Status = StatusOK
		r.Message = "no config loaded"
		return r
	}
	collisions := validation.ValidatePoolWorkDirIsolation(c.cityPath, c.cfg)
	if len(collisions) == 0 {
		r.Status = StatusOK
		r.Message = "multi-session agents use distinct work dirs per instance"
		return r
	}
	details := make([]string, 0, len(collisions))
	for _, coll := range collisions {
		details = append(details, validation.FormatSharedPoolWorkDir(coll))
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d multi-session agent(s) share one work dir across pool instances", len(collisions))
	r.FixHint = "set work_dir with {{.AgentBase}} (e.g. .gc/worktrees/{{.Rig}}/{{.AgentBase}}) and a pre_start that runs `gc internal ensure-worktree`; implicit rig pools get this by default"
	r.Details = details
	r.Message = r.Message + ": " + strings.Join(details, "; ")
	return r
}

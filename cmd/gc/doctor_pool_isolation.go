package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/git"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// poolIsolationDoctorCheck warns when the instances of a pool agent, an
// implicit agent or an agent with an explicit pool signal, would share one
// working directory, or when a rig that should give implicit pool instances
// their own worktree cannot. Sharing can be deliberate, so it only warns.
type poolIsolationDoctorCheck struct {
	cityPath string
	cfg      *config.City
}

func newPoolIsolationDoctorCheck(cityPath string, cfg *config.City) *poolIsolationDoctorCheck {
	return &poolIsolationDoctorCheck{cityPath: cityPath, cfg: cfg}
}

// Name implements doctor.Check.
func (*poolIsolationDoctorCheck) Name() string { return "pool-isolation" }

// CanFix implements doctor.Check. Each remedy is a config choice.
func (*poolIsolationDoctorCheck) CanFix() bool { return false }

// WarmupEligible implements doctor.Check. The unborn-HEAD probe runs git.
func (*poolIsolationDoctorCheck) WarmupEligible() bool { return false }

// Fix implements doctor.Check.
func (*poolIsolationDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run implements doctor.Check.
func (c *poolIsolationDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	const name = "pool-isolation"
	if c.cfg == nil {
		return okCheck(name, "no city config")
	}
	cityName := loadedCityName(c.cfg, c.cityPath)
	runtime := effectiveProviderName(c.cfg.Session.Provider)
	var agents, details []string
	for i := range c.cfg.Agents {
		a := &c.cfg.Agents[i]
		if !a.Implicit && !workdirutil.RequiresPoolWorkDirIsolationCheck(*a) {
			continue
		}
		reason, err := c.poolSharingReason(cityName, a, runtime)
		if err != nil {
			reason = err.Error()
		}
		if reason == "" {
			continue
		}
		agents = append(agents, a.QualifiedName())
		details = append(details, fmt.Sprintf("agent %q: %s", a.QualifiedName(), reason))
	}
	if len(agents) == 0 {
		return okCheck(name, "every pool agent's instances get their own working directory")
	}
	return warnCheck(name,
		fmt.Sprintf("pool instances share a working directory: %s", strings.Join(agents, ", ")),
		fmt.Sprintf("give each instance its own directory: make the rig a git repository with a commit, leave [workspace] implicit_isolation at %q, or set work_dir to a template that includes {{.AgentBase}}", config.ImplicitIsolationWorktree),
		details)
}

// poolSharingReason returns why a's first two instances would share a
// directory or, for a pool-slot agent, why its slots cannot be created; ""
// when they are isolated.
func (c *poolIsolationDoctorCheck) poolSharingReason(cityName string, a *config.Agent, runtime string) (string, error) {
	first := a.QualifiedInstanceName(a.Name + "-1")
	second := a.QualifiedInstanceName(a.Name + "-2")
	firstPath, err := resolveConfiguredWorkDirPathUnvalidated(c.cityPath, cityName, first, a, c.cfg.Rigs, runtime)
	if err != nil {
		return "", fmt.Errorf("resolving work_dir for instance %q: %w", first, err)
	}
	secondPath, err := resolveConfiguredWorkDirPathUnvalidated(c.cityPath, cityName, second, a, c.cfg.Rigs, runtime)
	if err != nil {
		return "", fmt.Errorf("resolving work_dir for instance %q: %w", second, err)
	}
	if firstPath != secondPath {
		if slot, ok := workdirutil.ImplicitSlotFor(c.cityPath, first, *implicitSlotAgentForRuntime(a, runtime), c.cfg.Rigs); ok {
			if _, err := git.New(slot.RigRoot).RevParseVerifyCommit(implicitSlotWorktreeBase); err != nil {
				return fmt.Sprintf("rig %q has no commit at %s, so its pool-slot worktrees cannot be created", slot.RigRoot, implicitSlotWorktreeBase), nil
			}
		}
		return "", nil
	}
	shared := fmt.Sprintf("instances %q and %q both run in %q", first, second, firstPath)
	switch {
	case !a.Implicit:
		return shared + "; work_dir does not vary per instance (include {{.AgentBase}})", nil
	case a.Dir == "":
		return shared, nil
	case strings.TrimSpace(c.cfg.Workspace.ImplicitIsolation) == config.ImplicitIsolationNone:
		return shared + `; workspace.implicit_isolation = "none"`, nil
	case !implicitSlotRuntimeIsHostLocal(effectiveSessionProvider(a.Session, runtime)):
		return shared + fmt.Sprintf("; session runtime %q is not host-local", effectiveSessionProvider(a.Session, runtime)), nil
	}
	if _, err := os.Lstat(filepath.Join(firstPath, ".git")); err != nil {
		return shared + fmt.Sprintf("; the rig root %q is not a git repository root", firstPath), nil
	}
	return shared, nil
}

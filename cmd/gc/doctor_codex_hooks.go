package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

type codexHooksDriftCheck struct {
	cityPath string
	dirs     []string
}

func newCodexHooksDriftCheck(cityPath string, dirs []string) *codexHooksDriftCheck {
	cityPath = strings.TrimSpace(cityPath)
	if cityPath != "" {
		cityPath = filepath.Clean(cityPath)
	}
	return &codexHooksDriftCheck{cityPath: cityPath, dirs: cleanCodexHookDirs(dirs)}
}

func codexHookWorkDirs(cityPath string, cfg *config.City) []string {
	var dirs []string
	addCodexHookDir(&dirs, cityPath)
	if cfg == nil {
		return dirs
	}
	suspState, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	suspendedRigPaths := map[string]bool{}
	for i := range cfg.Rigs {
		rig := &cfg.Rigs[i]
		suspended := suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart())
		if suspended || strings.TrimSpace(rig.Path) == "" {
			if suspended && strings.TrimSpace(rig.Path) != "" {
				suspendedRigPaths[filepath.Clean(rig.Path)] = true
			}
			continue
		}
		addCodexHookDir(&dirs, rig.Path)
	}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || agentInSuspendedRig(cityPath, agent, cfg.Rigs, suspendedRigPaths) {
			continue
		}
		if !agentUsesCodexHookSurface(cfg, agent) {
			continue
		}
		addCodexHookAgentWorkDirs(&dirs, cityPath, cfg, agent)
	}
	return dirs
}

func cleanCodexHookDirs(dirs []string) []string {
	var cleaned []string
	for _, dir := range dirs {
		addCodexHookDir(&cleaned, dir)
	}
	sort.Strings(cleaned)
	return cleaned
}

func addCodexHookDir(dirs *[]string, dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	dir = filepath.Clean(dir)
	for _, existing := range *dirs {
		if existing == dir {
			return
		}
	}
	*dirs = append(*dirs, dir)
}

func agentUsesCodexHookSurface(cfg *config.City, agent *config.Agent) bool {
	if cfg == nil || agent == nil {
		return false
	}
	if codexHookProviderName(codexHookEffectiveAgentProvider(cfg, agent), cfg.Providers) {
		return true
	}
	for _, provider := range config.ResolveInstallHooks(agent, &cfg.Workspace) {
		if codexHookProviderName(provider, cfg.Providers) {
			return true
		}
	}
	return false
}

func codexHookEffectiveAgentProvider(cfg *config.City, agent *config.Agent) string {
	if agent == nil {
		return ""
	}
	if provider := strings.TrimSpace(agent.Provider); provider != "" {
		return provider
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.Workspace.Provider)
	}
	return ""
}

func codexHookProviderName(name string, providers map[string]config.ProviderSpec) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	return name == "codex" || config.BuiltinFamily(name, providers) == "codex"
}

func addCodexHookAgentWorkDirs(dirs *[]string, cityPath string, cfg *config.City, agent *config.Agent) {
	addCodexHookAgentWorkDir(dirs, cityPath, cfg, agent, agent.QualifiedName())
	for _, slot := range codexHookPoolSlots(agent) {
		instanceAgent, qualifiedInstance, _ := poolDesiredRequestIdentity(agent, slot)
		if qualifiedInstance == agent.QualifiedName() {
			continue
		}
		addCodexHookAgentWorkDir(dirs, cityPath, cfg, instanceAgent, qualifiedInstance)
	}
}

func addCodexHookAgentWorkDir(dirs *[]string, cityPath string, cfg *config.City, agent *config.Agent, qualifiedName string) {
	workDir, err := resolveCodexHookAgentWorkDir(cityPath, cfg, agent, qualifiedName)
	if err != nil {
		return
	}
	addCodexHookDir(dirs, workDir)
}

func resolveCodexHookAgentWorkDir(cityPath string, cfg *config.City, agent *config.Agent, qualifiedName string) (string, error) {
	if agent == nil {
		return "", nil
	}
	cityName := loadedCityName(cfg, cityPath)
	var rigs []config.Rig
	if cfg != nil {
		rigs = cfg.Rigs
	}
	if strings.TrimSpace(qualifiedName) == "" {
		qualifiedName = agent.QualifiedName()
	}
	workDir, err := workdirutil.ResolveWorkDirPathStrict(cityPath, cityName, qualifiedName, *agent, rigs)
	if err != nil {
		return "", err
	}
	if err := workdirutil.ValidateAncestorWorktreesNotStale(workDir); err != nil {
		return "", err
	}
	return workDir, nil
}

func codexHookPoolSlots(agent *config.Agent) []int {
	if agent == nil || !agent.SupportsInstanceExpansion() {
		return nil
	}
	limit := 1
	if len(agent.NamepoolNames) > 0 {
		limit = len(agent.NamepoolNames)
	} else if maxSessions := agent.EffectiveMaxActiveSessions(); maxSessions != nil {
		if *maxSessions <= 1 {
			return nil
		}
		limit = *maxSessions
	} else if minSessions := agent.EffectiveMinActiveSessions(); minSessions > 1 {
		limit = minSessions
	}
	slots := make([]int, 0, limit)
	for slot := 1; slot <= limit; slot++ {
		slots = append(slots, slot)
	}
	return slots
}

func (c *codexHooksDriftCheck) Name() string { return "codex-hooks-drift" }

func (c *codexHooksDriftCheck) CanFix() bool { return true }

func (c *codexHooksDriftCheck) Fix(_ *doctor.CheckContext) error {
	for _, dir := range c.dirs {
		if !codexHooksNeedUpgrade(filepath.Join(dir, ".codex", "hooks.json"), c.cityPath) {
			continue
		}
		if err := hooks.Install(fsys.OSFS{}, c.cityPath, dir, []string{"codex"}); err != nil {
			return fmt.Errorf("upgrading Codex hooks in %s: %w", dir, err)
		}
	}
	return nil
}

func (c *codexHooksDriftCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	var stale []string
	for _, dir := range c.dirs {
		path := filepath.Join(dir, ".codex", "hooks.json")
		if codexHooksNeedUpgrade(path, c.cityPath) {
			stale = append(stale, path)
		}
	}
	if len(stale) == 0 {
		return okCheck(c.Name(), "Codex hooks are current or user-owned")
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d managed Codex hook file(s) need upgrade", len(stale)),
		"run `gc doctor --fix` or restart the city to upgrade managed Codex hooks",
		stale)
}

func codexHooksNeedUpgrade(path, cityPath string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return hooks.CodexHooksNeedManagedUpgrade(data, cityPath)
}

func codexHooksMissingPreCompact(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return hooks.CodexHooksMissingManagedPreCompact(data)
}

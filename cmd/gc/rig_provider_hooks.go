package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/runtime"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// installRigAgentHooks uses the composed post-registration config so a project
// can be reviewed before startup without a later writer changing its hooks.
// Codex always uses the shared lossless reconciler, including explicit hook
// installation. It must never first pass through the legacy JSON upgrader.
func installRigAgentHooks(fs fsys.FS, cityPath, rigName, rigPath string, cfg *config.City) error {
	if cfg == nil {
		return fmt.Errorf("composed rig config is unavailable")
	}
	resolver := func(name string) string { return config.BuiltinFamily(name, cfg.Providers) }
	codexRequested := resolver(cfg.Workspace.Provider) == "codex"
	for _, name := range cfg.Workspace.InstallAgentHooks {
		if name == "codex" || resolver(name) == "codex" {
			codexRequested = true
		}
	}
	ordinaryErr := installRigNonCodexHooks(fs, cityPath, rigPath, cfg)
	// The default coding provider applies even before a city defines explicit
	// agents. Agent-specific inputs below use the same projection as startup.
	var profiles []runtime.Config
	var profileNames []string
	cityName := workdirutil.CityName(cityPath, cfg)
	seen := make(map[string]bool)
	for i := range cfg.Agents {
		candidate := &cfg.Agents[i]
		identity := candidate.QualifiedName()
		if candidate.Scope == "rig" && candidate.Dir == "" {
			identity = rigName + "/" + candidate.BindingQualifiedName()
		}
		if seen[identity] {
			continue
		}
		seen[identity] = true
		// Use runtime lookup precedence: an explicit rig role overrides a
		// generic root role. Never mutate or prepare the raw template.
		agent := config.FindAgent(cfg, identity)
		if agent == nil {
			continue
		}

		home, err := resolveConfiguredWorkDirPath(cityPath, cityName, agent.QualifiedName(), agent, cfg.Rigs)
		if err != nil || !pathutil.SamePath(home, rigPath) {
			continue
		}
		provider := agent.Provider
		if provider == "" {
			provider = cfg.Workspace.Provider
		}
		wantsCodex := agent.StartCommand == "" && resolver(provider) == "codex"
		for _, name := range config.ResolveInstallHooks(agent, &cfg.Workspace) {
			if name == "codex" || resolver(name) == "codex" {
				wantsCodex = true
			}
		}
		if !wantsCodex {
			continue
		}
		resolved, err := config.ResolveProvider(agent, &cfg.Workspace, cfg.Providers, exec.LookPath)
		if err != nil {
			return errors.Join(ordinaryErr, fmt.Errorf("resolving hooks for %s: %w", agent.QualifiedName(), err))
		}
		hints := runtime.Config{}
		materialize.ApplyConfiguredSessionOverlayHints(cityPath, cfg, agent.QualifiedName(), resolved, &hints)
		profiles = append(profiles, hints)
		profileNames = append(profileNames, agent.QualifiedName())
	}
	if len(profiles) == 0 && codexRequested {
		profiles = append(profiles, runtime.Config{
			ProviderName: resolver(cfg.Workspace.Provider), ProviderOverlayName: cfg.Workspace.Provider,
			InstallAgentHooks: cfg.Workspace.InstallAgentHooks,
			PackOverlayDirs:   effectiveOverlayDirs(cfg.PackOverlayDirs, cfg.RigOverlayDirs, rigName),
		})
		profileNames = append(profileNames, "workspace")
	}
	if len(profiles) == 0 {
		return ordinaryErr
	}
	var layerProfiles [][][]byte
	for _, hints := range profiles {
		configured, err := materialize.ReadConfiguredCodexHookOverlayLayers(hints)
		if err != nil {
			return errors.Join(ordinaryErr, err)
		}
		layerProfiles = append(layerProfiles, configured)
	}
	digest, err := hooks.ReconcileCodexHookProfiles(fs, cityPath, rigPath, layerProfiles)
	if err == nil && strings.TrimSpace(digest) == "" {
		err = fmt.Errorf("codex hooks did not converge at %s", rigPath)
	}
	if err != nil {
		err = fmt.Errorf("preparing codex hook profiles [%s]: %w", strings.Join(profileNames, ", "), err)
	}
	return errors.Join(ordinaryErr, err)
}

// Other provider installers keep their existing behavior even if composing
// optional post-provision configuration fails. Codex must fail closed.
func installRigNonCodexHooks(fs fsys.FS, cityPath, rigPath string, cfg *config.City) error {
	if cfg == nil {
		return nil
	}
	resolver := func(name string) string { return config.BuiltinFamily(name, cfg.Providers) }
	var ordinary []string
	for _, name := range cfg.Workspace.InstallAgentHooks {
		if name != "codex" && resolver(name) != "codex" {
			ordinary = append(ordinary, name)
		}
	}
	return hooks.InstallWithResolver(fs, cityPath, rigPath, ordinary, resolver)
}

func rigNeedsCodexHookPreparation(cfg *config.City) bool {
	if cfg == nil {
		return false
	}
	if config.BuiltinFamily(cfg.Workspace.Provider, cfg.Providers) == "codex" || installHooksIncludeFamily(cfg.Workspace.InstallAgentHooks, "codex", cfg.Providers) {
		return true
	}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if (agent.StartCommand == "" && config.BuiltinFamily(agent.Provider, cfg.Providers) == "codex") || installHooksIncludeFamily(config.ResolveInstallHooks(agent, &cfg.Workspace), "codex", cfg.Providers) {
			return true
		}
	}
	return false
}

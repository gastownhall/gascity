package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/shellquote"
)

var managedMCPGitignoreEntries = []string{
	".mcp.json",
	filepath.ToSlash(filepath.Join(".gemini", "settings.json")),
	filepath.ToSlash(filepath.Join(".codex", "config.toml")),
}

type mcpTargetSpec struct {
	Root       string
	Projection materialize.MCPProjection
	Agents     []string
}

type resolvedMCPProjection struct {
	Agent        *config.Agent
	Identity     string
	WorkDir      string
	ScopeRoot    string
	ProviderKind string
	Delivery     string
	Catalog      materialize.MCPCatalog
	Projection   materialize.MCPProjection
}

func buildMCPTemplateData(cityPath, cityName, qualifiedName, workDir string, agent *config.Agent, rigs []config.Rig) map[string]string {
	rigName := configuredRigName(cityPath, agent, rigs)
	rigRoot := rigRootForName(rigName, rigs)
	return buildTemplateData(PromptContext{
		CityRoot:      cityPath,
		AgentName:     qualifiedName,
		TemplateName:  templateNameFor(agent, qualifiedName),
		RigName:       rigName,
		RigRoot:       rigRoot,
		WorkDir:       workDir,
		IssuePrefix:   findRigPrefix(rigName, rigs),
		DefaultBranch: defaultBranchFor(workDir),
		WorkQuery:     agent.EffectiveWorkQuery(),
		SlingQuery:    agent.EffectiveSlingQuery(),
		Env:           agent.Env,
	})
}

func supportsMCPProviderKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case materialize.MCPProviderClaude, materialize.MCPProviderCodex, materialize.MCPProviderGemini:
		return true
	default:
		return false
	}
}

func loadEffectiveMCPForAgent(
	cityPath, cityName string,
	cfg *config.City,
	agent *config.Agent,
	qualifiedName, workDir string,
) (materialize.MCPCatalog, error) {
	templateData := buildMCPTemplateData(cityPath, cityName, qualifiedName, workDir, agent, cfg.Rigs)
	catalog, err := materialize.EffectiveMCPForAgent(cfg, agent, templateData)
	if err != nil {
		return materialize.MCPCatalog{}, fmt.Errorf("loading effective MCP: %w", err)
	}
	return catalog, nil
}

func resolveAgentMCPProjection(
	cityPath, cityName string,
	cfg *config.City,
	agent *config.Agent,
	qualifiedName, workDir string,
	providerKind string,
) (materialize.MCPCatalog, materialize.MCPProjection, error) {
	catalog, err := loadEffectiveMCPForAgent(cityPath, cityName, cfg, agent, qualifiedName, workDir)
	if err != nil {
		return materialize.MCPCatalog{}, materialize.MCPProjection{}, err
	}
	if !supportsMCPProviderKind(providerKind) {
		if len(catalog.Servers) > 0 {
			return materialize.MCPCatalog{}, materialize.MCPProjection{}, fmt.Errorf(
				"effective MCP requires a supported provider family, got %q", providerKind)
		}
		return catalog, materialize.MCPProjection{}, nil
	}
	projection, err := materialize.BuildMCPProjection(providerKind, workDir, catalog.Servers)
	if err != nil {
		return materialize.MCPCatalog{}, materialize.MCPProjection{}, err
	}
	return catalog, projection, nil
}

func mergeMCPFingerprintEntry(fpExtra map[string]string, projection materialize.MCPProjection) map[string]string {
	if projection.Provider == "" {
		return fpExtra
	}
	if fpExtra == nil {
		fpExtra = make(map[string]string, 1)
	}
	fpExtra["mcp:"+projection.Provider] = projection.Hash()
	return fpExtra
}

func appendProjectMCPPreStart(prestart []string, agentName, identity, workDir string) []string {
	cmd := `"${GC_BIN:-gc}" internal project-mcp --agent ` +
		shellquote.Join([]string{agentName}) +
		` --identity ` + shellquote.Join([]string{identity}) +
		` --workdir ` + shellquote.Join([]string{workDir})
	return append(prestart, cmd)
}

func ensureMCPGitignoreBestEffort(root string, stderr io.Writer) {
	if strings.TrimSpace(root) == "" {
		return
	}
	if err := ensureGitignoreEntries(fsys.OSFS{}, root, managedMCPGitignoreEntries); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "gc: warning: updating %s/.gitignore for MCP: %v\n", root, err) //nolint:errcheck // best-effort stderr
	}
}

func buildStage1MCPTargets(cityPath string, cfg *config.City, lookPath config.LookPathFunc) ([]mcpTargetSpec, error) {
	if cfg == nil {
		return nil, nil
	}
	byKey := make(map[string]mcpTargetSpec)
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if !canStage1Materialize(cfg.Session.Provider, agent) {
			continue
		}
		view, err := resolveConfiguredAgentMCPProjection(cityPath, cfg, agent, lookPath)
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", agent.QualifiedName(), err)
		}
		if view.Delivery != "stage1" {
			continue
		}
		if view.Projection.Provider == "" && len(view.Catalog.Servers) == 0 {
			continue
		}
		key := view.Projection.Provider + "|" + view.Projection.Target
		existing, ok := byKey[key]
		if ok {
			if existing.Projection.Hash() != view.Projection.Hash() {
				return nil, fmt.Errorf(
					"MCP target conflict at %s (%s): %s projects %s but %s projects %s",
					view.Projection.Target,
					view.Projection.Provider,
					strings.Join(existing.Agents, ", "),
					existing.Projection.Hash(),
					agent.QualifiedName(),
					view.Projection.Hash(),
				)
			}
			existing.Agents = append(existing.Agents, agent.QualifiedName())
			sort.Strings(existing.Agents)
			byKey[key] = existing
			continue
		}
		byKey[key] = mcpTargetSpec{
			Root:       view.ScopeRoot,
			Projection: view.Projection,
			Agents:     []string{agent.QualifiedName()},
		}
	}

	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]mcpTargetSpec, 0, len(keys))
	for _, key := range keys {
		out = append(out, byKey[key])
	}
	return out, nil
}

func runStage1MCPProjection(cityPath string, cfg *config.City, lookPath config.LookPathFunc, stderr io.Writer) error {
	targets, err := buildStage1MCPTargets(cityPath, cfg, lookPath)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if err := target.Projection.Apply(fsys.OSFS{}); err != nil {
			return fmt.Errorf("reconciling %s: %w", target.Projection.Target, err)
		}
		if len(target.Projection.Servers) > 0 {
			ensureMCPGitignoreBestEffort(target.Root, stderr)
		}
	}
	return nil
}

func resolveDeterministicAgentMCPProjection(
	cityPath string,
	cfg *config.City,
	agent *config.Agent,
	lookPath config.LookPathFunc,
) (resolvedMCPProjection, error) {
	view, err := resolveConfiguredAgentMCPProjection(cityPath, cfg, agent, lookPath)
	if err != nil || agent == nil || !agent.SupportsMultipleSessions() || len(view.Catalog.Servers) == 0 {
		return view, err
	}

	altIdentity := agent.QualifiedName() + "-alt"
	altWorkDir, err := resolveWorkDirForQualifiedName(cityPath, cfg, agent, altIdentity)
	if err != nil {
		return resolvedMCPProjection{}, fmt.Errorf("agent %q has session-specific MCP targets; use --session", agent.QualifiedName())
	}
	altView, err := resolveProjectedMCPForTarget(cityPath, cfg, agent, altIdentity, altWorkDir, "", lookPath)
	if err != nil {
		return resolvedMCPProjection{}, fmt.Errorf("agent %q has session-specific MCP targets; use --session", agent.QualifiedName())
	}
	if view.Projection.Target != altView.Projection.Target || view.Projection.Hash() != altView.Projection.Hash() {
		return resolvedMCPProjection{}, fmt.Errorf("agent %q has session-specific MCP targets; use --session", agent.QualifiedName())
	}
	return view, nil
}

func resolveConfiguredAgentMCPProjection(
	cityPath string,
	cfg *config.City,
	agent *config.Agent,
	lookPath config.LookPathFunc,
) (resolvedMCPProjection, error) {
	if cfg == nil || agent == nil {
		return resolvedMCPProjection{}, fmt.Errorf("agent unavailable")
	}
	cityName := config.EffectiveCityName(cfg, filepath.Base(cityPath))
	identity := agent.QualifiedName()
	workDir, err := resolveWorkDirForQualifiedName(cityPath, cfg, agent, identity)
	if err != nil {
		catalog, catErr := loadEffectiveMCPForAgent(cityPath, cityName, cfg, agent, identity, agentScopeRoot(agent, cityPath, cfg.Rigs))
		if catErr != nil {
			return resolvedMCPProjection{}, fmt.Errorf("loading effective MCP: %w", catErr)
		}
		if len(catalog.Servers) == 0 {
			return resolvedMCPProjection{}, nil
		}
		return resolvedMCPProjection{}, fmt.Errorf("resolving workdir for agent %q: %w", identity, err)
	}
	return resolveProjectedMCPForTarget(cityPath, cfg, agent, identity, workDir, "", lookPath)
}

func resolveSessionMCPProjection(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	sessionID string,
	lookPath config.LookPathFunc,
) (resolvedMCPProjection, error) {
	if cfg == nil {
		return resolvedMCPProjection{}, fmt.Errorf("city config unavailable")
	}
	if store == nil {
		return resolvedMCPProjection{}, fmt.Errorf("session store unavailable")
	}
	id, err := resolveSessionIDAllowClosedWithConfig(cityPath, cfg, store, sessionID)
	if err != nil {
		return resolvedMCPProjection{}, err
	}
	bead, err := store.Get(id)
	if err != nil {
		return resolvedMCPProjection{}, fmt.Errorf("loading session %q: %w", sessionID, err)
	}
	template := normalizedSessionTemplate(bead, cfg)
	if template == "" {
		template = strings.TrimSpace(bead.Metadata["agent_name"])
	}
	template = resolveAgentTemplate(template, cfg)
	agent := findAgentByTemplate(cfg, template)
	if agent == nil {
		return resolvedMCPProjection{}, fmt.Errorf("session %q maps to unknown agent template %q", sessionID, template)
	}
	identity := strings.TrimSpace(bead.Metadata["agent_name"])
	if identity == "" {
		identity = agent.QualifiedName()
	}
	workDir := strings.TrimSpace(bead.Metadata["work_dir"])
	if workDir == "" {
		workDir, err = resolveWorkDirForQualifiedName(cityPath, cfg, agent, identity)
		if err != nil {
			return resolvedMCPProjection{}, fmt.Errorf("resolving workdir for session %q: %w", sessionID, err)
		}
	}
	providerKind := strings.TrimSpace(bead.Metadata["provider_kind"])
	if providerKind == "" {
		providerKind = strings.TrimSpace(bead.Metadata["provider"])
	}
	return resolveProjectedMCPForTarget(cityPath, cfg, agent, identity, workDir, providerKind, lookPath)
}

func resolveProjectedMCPForTarget(
	cityPath string,
	cfg *config.City,
	agent *config.Agent,
	identity, workDir, providerKind string,
	lookPath config.LookPathFunc,
) (resolvedMCPProjection, error) {
	if cfg == nil || agent == nil {
		return resolvedMCPProjection{}, fmt.Errorf("agent unavailable")
	}
	cityName := config.EffectiveCityName(cfg, filepath.Base(cityPath))
	if strings.TrimSpace(identity) == "" {
		identity = agent.QualifiedName()
	}
	scopeRoot := agentScopeRoot(agent, cityPath, cfg.Rigs)
	if strings.TrimSpace(providerKind) == "" {
		resolved, err := config.ResolveProvider(agent, &cfg.Workspace, cfg.Providers, lookPath)
		if err != nil {
			catalog, catErr := loadEffectiveMCPForAgent(cityPath, cityName, cfg, agent, identity, workDir)
			if catErr != nil {
				return resolvedMCPProjection{}, catErr
			}
			if len(catalog.Servers) == 0 {
				return resolvedMCPProjection{}, nil
			}
			return resolvedMCPProjection{}, err
		}
		providerKind = strings.TrimSpace(resolved.Kind)
	}
	catalog, projection, err := resolveAgentMCPProjection(cityPath, cityName, cfg, agent, identity, workDir, providerKind)
	if err != nil {
		return resolvedMCPProjection{}, err
	}
	canonWorkDir := canonicaliseFilePath(workDir, cityPath)
	stage1 := canStage1Materialize(cfg.Session.Provider, agent) && canonWorkDir == scopeRoot
	stage2 := isStage2EligibleSession(cfg.Session.Provider, agent) && canonWorkDir != scopeRoot
	if len(catalog.Servers) > 0 && !stage1 && !stage2 {
		return resolvedMCPProjection{}, fmt.Errorf(
			"effective MCP cannot be delivered to workdir %q with session provider %q",
			canonWorkDir,
			cfg.Session.Provider,
		)
	}
	delivery := ""
	switch {
	case stage1:
		delivery = "stage1"
	case stage2:
		delivery = "stage2"
	}
	return resolvedMCPProjection{
		Agent:        agent,
		Identity:     identity,
		WorkDir:      canonWorkDir,
		ScopeRoot:    scopeRoot,
		ProviderKind: providerKind,
		Delivery:     delivery,
		Catalog:      catalog,
		Projection:   projection,
	}, nil
}

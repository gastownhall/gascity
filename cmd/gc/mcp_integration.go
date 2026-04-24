package main

import (
	"fmt"
	"io"
	"os"
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

func buildMCPTemplateData(cityPath, qualifiedName, workDir string, agent *config.Agent, rigs []config.Rig) map[string]string {
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
	cityPath string,
	cfg *config.City,
	agent *config.Agent,
	qualifiedName, workDir string,
) (materialize.MCPCatalog, error) {
	templateData := buildMCPTemplateData(cityPath, qualifiedName, workDir, agent, cfg.Rigs)
	catalog, err := materialize.EffectiveMCPForAgent(cfg, agent, templateData)
	if err != nil {
		return materialize.MCPCatalog{}, fmt.Errorf("loading effective MCP: %w", err)
	}
	return catalog, nil
}

func resolveAgentMCPProjection(
	cityPath string,
	cfg *config.City,
	agent *config.Agent,
	qualifiedName, workDir string,
	providerKind string,
) (materialize.MCPCatalog, materialize.MCPProjection, error) {
	catalog, err := loadEffectiveMCPForAgent(cityPath, cfg, agent, qualifiedName, workDir)
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
	desired := make(map[string]bool, len(targets)) // provider+root keys we still own
	for _, target := range targets {
		if err := target.Projection.ApplyWithStderr(fsys.OSFS{}, stderr); err != nil {
			return fmt.Errorf("reconciling %s: %w", target.Projection.Target, err)
		}
		desired[managedKey(target.Projection.Provider, target.Root)] = true
		if len(target.Projection.Servers) > 0 {
			ensureMCPGitignoreBestEffort(target.Root, stderr)
		}
	}
	if err := cleanupOrphanStage1Targets(cityPath, cfg, desired, stderr); err != nil {
		return err
	}
	return nil
}

// managedKey combines provider and scope-root into the key used by stage-1
// reconcile to recognize which managed markers still have a live claimant.
func managedKey(provider, root string) string {
	return provider + "|" + filepath.Clean(root)
}

// cleanupOrphanStage1Targets walks .gc/mcp-managed/ under every scope root
// reachable from the current config and reconciles away managed markers
// that have no desired claimant. Covered: agents removed from city.toml,
// provider changes that move a target between provider subtrees, and
// managed markers under still-attached rig roots that no longer have an
// agent claiming them.
//
// Not covered: rigs detached from city.toml (their path is no longer in
// cfg.Rigs, so we cannot reach their .gc/mcp-managed/ to sweep it).
// Detached-rig cleanup is explicit work for an operator — `gc rig detach`
// or a future `gc mcp reconcile --root <path>` command. If needed, the
// operator can also delete .gc/mcp-managed/ under the detached rig root
// by hand; the managed files that remain outside GC's view are
// structurally equivalent to any other hand-authored MCP surface.
//
// Stage-2 workdirs are also not swept here: they are self-reconciled on
// every session start and have no stable root registry.
func cleanupOrphanStage1Targets(cityPath string, cfg *config.City, desired map[string]bool, stderr io.Writer) error {
	if cfg == nil {
		return nil
	}
	roots := collectStage1ScopeRoots(cityPath, cfg)
	for _, root := range roots {
		if err := cleanupOrphansAtRoot(root, desired, stderr); err != nil {
			return err
		}
	}
	return nil
}

func collectStage1ScopeRoots(cityPath string, cfg *config.City) []string {
	seen := make(map[string]bool)
	add := func(root string) {
		if strings.TrimSpace(root) == "" {
			return
		}
		clean := filepath.Clean(root)
		if !filepath.IsAbs(clean) {
			clean = filepath.Clean(filepath.Join(cityPath, clean))
		}
		seen[clean] = true
	}
	add(cityPath)
	for _, rig := range cfg.Rigs {
		add(rig.Path)
	}
	out := make([]string, 0, len(seen))
	for root := range seen {
		out = append(out, root)
	}
	sort.Strings(out)
	return out
}

func cleanupOrphansAtRoot(root string, desired map[string]bool, stderr io.Writer) error {
	markersDir := filepath.Join(root, ".gc", "mcp-managed")
	entries, err := fsys.OSFS{}.ReadDir(markersDir)
	if err != nil {
		// Missing directory is the steady state when no targets have ever
		// been adopted at this root — not an error.
		if os.IsNotExist(err) {
			return nil
		}
		// Permission or corruption problems must surface — silently
		// skipping would leave stale managed state unreconciled and
		// hide operator-facing diagnostics.
		return fmt.Errorf("scanning %s: %w", markersDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		provider := strings.TrimSuffix(name, ".json")
		if !supportsMCPProviderKind(provider) {
			continue
		}
		if desired[managedKey(provider, root)] {
			continue
		}
		orphan, err := materialize.BuildMCPProjection(provider, root, nil)
		if err != nil {
			return fmt.Errorf("building orphan projection for %s at %s: %w", provider, root, err)
		}
		if err := orphan.ApplyWithStderr(fsys.OSFS{}, stderr); err != nil {
			return fmt.Errorf("cleaning up orphan %s marker at %s: %w", provider, root, err)
		}
		if stderr != nil {
			fmt.Fprintf(stderr, "gc: cleaned up orphan MCP marker %s at %s\n", provider, root) //nolint:errcheck // best-effort stderr
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
	identity := agent.QualifiedName()
	workDir, err := resolveWorkDirForQualifiedName(cityPath, cfg, agent, identity)
	if err != nil {
		catalog, catErr := loadEffectiveMCPForAgent(cityPath, cfg, agent, identity, agentScopeRoot(agent, cityPath, cfg.Rigs))
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

// validateStage2TargetClaimants enforces that every configured agent
// that would actually land on the same provider-native MCP target as
// the caller projects an identical payload. Stage-1 runs the same check
// at build-time (buildStage1MCPTargets); stage-2 must run it at
// write-time because multiple agents can share a workdir and last-
// writer-wins would otherwise let any of them silently lose their MCP
// surface.
//
// Critically, each candidate agent must be resolved against its **own**
// provider kind and **own** workdir (template expansion against that
// agent's identity) rather than the caller's. Reusing the caller's
// providerKind mis-projects mixed-provider peers (a Codex peer
// projected as "claude" would appear to collide at the caller's
// `.mcp.json` even though at runtime the two write disjoint files);
// reusing the caller's workdir collapses every peer onto the caller's
// target regardless of the peer's own WorkDir template.
//
// The caller's projection is the reference; any other configured agent
// whose own workdir resolves to the same target with a different hash
// aborts the write with a conflict error naming both agents.
//
// Scope limitation: this check iterates *configured* agents, not live
// sessions. A pooled template that produces session-varying MCP
// payloads (e.g., MCP catalogs that expand `{{.AgentName}}`) whose
// concrete sessions share a workdir is not detected here — the
// validator sees only one configured agent and no conflict is raised.
// This is an operator-misconfiguration scenario: if two live sessions
// share a workdir they are already racing for many shared resources
// (git state, skill materialization, hook state) beyond MCP. A future
// enhancement could plumb the session store in and validate against
// live claimants; for now, stage-2 validation covers the
// multi-template same-workdir case and defers same-template live-
// session conflicts to stage-1's build-time check for non-pooled
// scope-root overlaps and to operator review of pool configuration.
func validateStage2TargetClaimants(
	cityPath string,
	cfg *config.City,
	caller *config.Agent,
	want materialize.MCPProjection,
	lookPath config.LookPathFunc,
) error {
	if cfg == nil || want.Provider == "" {
		return nil
	}
	callerName := ""
	if caller != nil {
		callerName = caller.QualifiedName()
	}
	wantHash := want.Hash()
	for i := range cfg.Agents {
		other := &cfg.Agents[i]
		if other.QualifiedName() == callerName {
			continue
		}
		// Implicit agents are synthetic provider-coverage entries
		// (config.InjectImplicitAgents) used for sling target
		// materialization, not real MCP writers. They never invoke
		// `gc internal project-mcp`, so they cannot race with the
		// caller for this target.
		if other.Implicit {
			continue
		}
		identity := other.QualifiedName()
		// Resolve the candidate's own provider BEFORE projecting.
		// Reusing the caller's providerKind would mis-project mixed-
		// provider peers into the caller's target shape — a Codex
		// peer's catalog projected as "claude" would appear to
		// collide at the caller's `.mcp.json` even though the peer
		// would actually write `.codex/config.toml` at runtime.
		otherResolved, err := config.ResolveProvider(other, &cfg.Workspace, cfg.Providers, lookPath)
		if err != nil {
			// Cannot resolve this peer's provider — it can't claim
			// any target. Not a conflict.
			continue
		}
		otherKind := resolvedProviderLaunchFamily(otherResolved)
		if otherKind != want.Provider {
			// Different provider family — targets live in disjoint
			// subtrees (`.mcp.json` / `.gemini/settings.json` /
			// `.codex/config.toml`), so no collision is possible.
			continue
		}
		// Resolve the candidate's own workdir. Failures here are
		// treated as "this agent can't claim any target in this city
		// right now" — not a conflict.
		otherWorkDir, err := resolveWorkDirForQualifiedName(cityPath, cfg, other, identity)
		if err != nil {
			continue
		}
		_, projection, err := resolveAgentMCPProjection(cityPath, cfg, other, identity, otherWorkDir, otherKind)
		if err != nil {
			continue
		}
		if projection.Provider != want.Provider || projection.Target != want.Target {
			// Different physical target, no conflict.
			continue
		}
		if projection.Hash() != wantHash {
			return fmt.Errorf(
				"MCP target conflict at %s (%s): agent %q projects %s but agent %q projects %s",
				want.Target,
				want.Provider,
				callerName,
				wantHash,
				other.QualifiedName(),
				projection.Hash(),
			)
		}
	}
	return nil
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
	if strings.TrimSpace(identity) == "" {
		identity = agent.QualifiedName()
	}
	scopeRoot := agentScopeRoot(agent, cityPath, cfg.Rigs)
	if strings.TrimSpace(providerKind) == "" {
		resolved, err := config.ResolveProvider(agent, &cfg.Workspace, cfg.Providers, lookPath)
		if err != nil {
			catalog, catErr := loadEffectiveMCPForAgent(cityPath, cfg, agent, identity, workDir)
			if catErr != nil {
				return resolvedMCPProjection{}, catErr
			}
			if len(catalog.Servers) == 0 {
				return resolvedMCPProjection{}, nil
			}
			return resolvedMCPProjection{}, err
		}
		providerKind = resolvedProviderLaunchFamily(resolved)
	}
	catalog, projection, err := resolveAgentMCPProjection(cityPath, cfg, agent, identity, workDir, providerKind)
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

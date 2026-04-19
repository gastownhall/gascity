// template_resolve.go extracts a value-producing function for resolving
// agent config into session parameters. Most of the work is pure (provider
// resolution, dir expansion, env merging, prompt rendering).
//
// One side effect lives here by necessity: managed Claude settings are
// projected to .gc/settings.json via ensureClaudeSettingsArgs so that the
// --settings path is on disk before runtime fingerprints are captured.
// This is the single chokepoint for Claude projection — installAgentSideEffects
// skips the "claude" entry in its hook list to avoid duplicate work.
//
// Other side effects (ACP route registration, non-Claude hook installation)
// are handled by the caller (buildOneAgent → installAgentSideEffects).
//
// resolveTemplate returns TemplateParams — a value type suitable for
// session.Manager.CreateFromParams or for constructing runtime.Config.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// TemplateParams holds all resolved values needed to start a session.
// This is a pure data type — no side effects, no provider references.
type TemplateParams struct {
	// Command is the resolved provider command string.
	Command string
	// Prompt is the fully rendered prompt (with beacon).
	Prompt string
	// Env is the merged environment (passthrough + provider + agent + passthrough vars).
	Env map[string]string
	// Hints contains startup behavior (pre_start, session_setup, etc.).
	Hints agent.StartupHints
	// WorkDir is the resolved absolute working directory.
	WorkDir string
	// SessionName is the computed tmux session name.
	SessionName string
	// Alias is the human-readable session identifier used for commands and mail.
	Alias string
	// ConfiguredNamedIdentity marks a canonical named session bead reserved in config.
	ConfiguredNamedIdentity string
	// ConfiguredNamedMode records the controller mode for canonical named sessions.
	ConfiguredNamedMode string
	// FPExtra carries additional fingerprint data (pool config, etc.).
	FPExtra map[string]string
	// ResolvedProvider is the resolved provider spec (for ACP routing, etc.).
	ResolvedProvider *config.ResolvedProvider
	// TemplateName is the config template name (pool base name or qualified name).
	// For pool instances this is the base template (e.g., "dog"), not the instance.
	TemplateName string
	// InstanceName is the qualified instance name used for display and events.
	// For non-expanding templates it equals TemplateName; for pool instances it's "dog-1".
	InstanceName string
	// RigName is the resolved rig association (empty if none).
	RigName string
	// RigRoot is the absolute path to the associated rig root (empty if none).
	RigRoot string
	// WakeMode controls whether the next wake resumes or starts fresh conversation state.
	WakeMode string
	// IsACP is true if session = "acp".
	IsACP bool
	// HookEnabled reports whether provider hooks are installed for this agent.
	// Hook-enabled providers receive startup context via their hook path
	// (for example gc prime --hook), so PromptMode=none should not also
	// fall back to a delayed startup nudge.
	HookEnabled bool
	// SessionOverride is the per-agent session provider override (e.g., "acp",
	// "tmux", "exec:..."). Empty means use the city-level default.
	SessionOverride string
	// DependencyOnly marks a realized cold slot kept only so dependency wake
	// has something concrete to wake even when pool check wants zero.
	DependencyOnly bool
	// ManualSession marks a discovered root created outside pool scale logic
	// (for example via `gc session new`). These sessions stay desired without
	// inflating poolDesired for config-managed slots.
	ManualSession bool
	// PoolSlot is the 1-based slot number within the pool. Set during
	// buildDesiredState for pool instances so syncSessionBeads can stamp
	// pool_slot metadata without reverse-engineering the slot from the name
	// (which fails for namepool-themed instances like "fenrir").
	PoolSlot int
	// EnvIdentityStamped reports whether setTemplateEnvIdentity has written
	// an authoritative GC_ALIAS/GC_AGENT identity into Env. resolveTemplate
	// always seeds GC_ALIAS=qualifiedName, so "Env has GC_ALIAS" is not a
	// sufficient signal on its own — callers use this flag to distinguish
	// identity-stamped templates (pool workers, dependency floors) from the
	// resolver's default stamping on ordinary sessions.
	EnvIdentityStamped bool
}

// DisplayName returns the name to use for log messages and event subjects.
// For pool instances this is the instance name (e.g., "dog-1"); for
// non-expanding templates it equals TemplateName.
func (tp TemplateParams) DisplayName() string {
	if tp.InstanceName != "" {
		return tp.InstanceName
	}
	return tp.TemplateName
}

// resolveTemplate computes all session parameters from a config.Agent.
// It also reconciles managed Claude settings before wiring the active
// --settings path so runtime fingerprinting sees the current projected file.
//
// qualifiedName is the agent's canonical identity. fpExtra carries additional
// fingerprint data (e.g., pool bounds); pass nil for pool instances.
func resolveTemplate(p *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, fpExtra map[string]string) (TemplateParams, error) {
	// Step 1: Resolve provider preset.
	resolved, err := config.ResolveProvider(cfgAgent, p.workspace, p.providers, p.lookPath)
	if err != nil {
		return TemplateParams{}, fmt.Errorf("agent %q: %w", qualifiedName, err)
	}
	// Step 2: Validate session vs provider compatibility.
	if cfgAgent.Session == "acp" && !resolved.SupportsACP {
		return TemplateParams{}, fmt.Errorf("agent %q: session = \"acp\" but provider %q does not support ACP (set supports_acp = true on the provider)", qualifiedName, resolved.Name)
	}

	// Step 3: Expand dir template.
	dirCtx := sessionSetupContextForAgent(p.cityPath, p.cityName, qualifiedName, cfgAgent, p.rigs)
	workDir, err := resolveConfiguredWorkDir(p.cityPath, p.cityName, qualifiedName, cfgAgent, p.rigs)
	if err != nil {
		return TemplateParams{}, fmt.Errorf("agent %q: %w", qualifiedName, err)
	}
	rigName := dirCtx.Rig
	rigRoot := dirCtx.RigRoot
	agentBase := dirCtx.AgentBase

	// Step 4: Resolve overlay directory.
	overlayDir := resolveOverlayDir(cfgAgent.OverlayDir, p.cityPath)

	// Step 5: Build copy_files and command with settings args + schema defaults.
	var copyFiles []runtime.CopyEntry
	command := resolved.CommandString()
	// Append schema-derived default args (e.g., --dangerously-skip-permissions
	// from EffectiveDefaults["permission_mode"] = "unrestricted").
	if defaultArgs := resolved.ResolveDefaultArgs(); len(defaultArgs) > 0 {
		command = command + " " + shellquote.Join(defaultArgs)
	}
	sa, err := ensureClaudeSettingsArgs(p.fs, p.cityPath, resolved.Name, p.stderr)
	if err != nil {
		return TemplateParams{}, fmt.Errorf("agent %q: %w", qualifiedName, err)
	}
	if sa != "" {
		command = command + " " + sa
		settingsFile, relDst := claudeSettingsSource(p.cityPath)
		if settingsFile != "" {
			copyFiles = append(copyFiles, runtime.CopyEntry{
				Src: settingsFile, RelDst: relDst,
				Probed: true, ContentHash: runtime.HashPathContent(settingsFile),
			})
		}
	}
	scriptsDir := citylayout.ScriptsPath(p.cityPath)
	if info, sErr := os.Stat(scriptsDir); sErr == nil && info.IsDir() {
		copyFiles = append(copyFiles, runtime.CopyEntry{
			Src: scriptsDir, RelDst: path.Join(".gc", "scripts"),
			Probed: true, ContentHash: runtime.HashPathContent(scriptsDir),
		})
	}
	copyFiles = stageHookFiles(copyFiles, p.cityPath, workDir)

	// Step 6: Compute session name.
	// Uses bead-derived naming ("s-{beadID}") when a bead store is available,
	// falling back to the legacy SessionNameFor for backward compatibility.
	tmplName := templateNameFor(cfgAgent, qualifiedName)
	sessName := p.resolveSessionName(qualifiedName, tmplName)

	// Step 7: Resolve session bead ID for traceability.
	// Look up the session bead by session_name to get the bead ID (e.g., mc-cnf).
	// This is what MC uses to link beads → session logs.
	sessionBeadID := ""
	if p.sessionBeads != nil {
		for _, b := range p.sessionBeads.Open() {
			if b.Metadata["session_name"] == sessName {
				sessionBeadID = b.ID
				break
			}
		}
	}
	if sessionBeadID == "" && p.beadStore != nil {
		if all, err := p.beadStore.List(beads.ListQuery{Label: "gc:session"}); err == nil {
			for _, b := range all {
				if !session.IsSessionBeadOrRepairable(b) || b.Status == "closed" {
					continue
				}
				if b.Metadata["session_name"] == sessName {
					sessionBeadID = b.ID
					break
				}
			}
		}
	}
	if sessionBeadID == "" {
		sessionBeadID = qualifiedName
	}

	// Step 8: Build agent environment.
	agentEnv := map[string]string{
		"GC_SESSION_NAME":   sessName,
		"GC_SESSION_ID":     sessionBeadID,
		"GC_TEMPLATE":       templateNameFor(cfgAgent, qualifiedName),
		"GC_SESSION_ORIGIN": "ephemeral",
		"GC_AGENT":          sessName,
		"GC_ALIAS":          qualifiedName,
		"BEADS_ACTOR":       sessName,
		"GC_DIR":            workDir,
		// Explicit empty values matter here. tmux session creation uses `env -u`
		// only for keys present with empty strings, which prevents stale rig
		// scope from leaking out of the tmux server's inherited environment.
		"GC_RIG":      "",
		"GC_RIG_ROOT": "",
		"BEADS_DIR":   "",
		// GT_ROOT stays city-scoped by default. bd formula discovery falls back
		// to $GT_ROOT/.beads/formulas when agents run outside the city/rig repo
		// roots (for example under .gc/agents/... or .gc/worktrees/...).
		// Rig-scoped agents override the rig-specific keys below.
		"GT_ROOT": p.cityPath,
	}
	for key, value := range citylayout.CityRuntimeEnvMap(p.cityPath) {
		agentEnv[key] = value
	}
	agentEnv["GC_BEADS"] = rawBeadsProvider(p.cityPath)
	if exe, err := os.Executable(); err == nil && exe != "" {
		agentEnv["GC_BIN"] = exe
	}
	for key, value := range sessionDoltEnv(p.cityPath, rigRoot, p.rigs) {
		agentEnv[key] = value
	}
	if p.apiURL != "" {
		agentEnv["GC_API_URL"] = p.apiURL
	}
	if rigName != "" {
		agentEnv["GC_RIG"] = rigName
		agentEnv["GC_RIG_ROOT"] = rigRoot
		agentEnv["BEADS_DIR"] = filepath.Join(rigRoot, ".beads")
	}

	// Step 9: Render prompt with beacon.
	var prompt string
	hasHooks := config.AgentHasHooks(cfgAgent, p.workspace, resolved.Name)
	// Merge fragment sources: V1 global_fragments + inject_fragments,
	// plus V2 append_fragments from agent defaults.
	fragments := mergeFragmentLists(p.globalFragments, cfgAgent.InjectFragments)
	if len(p.appendFragments) > 0 {
		fragments = mergeFragmentLists(fragments, p.appendFragments)
	}
	prompt = renderPrompt(p.fs, p.cityPath, p.cityName, cfgAgent.PromptTemplate, PromptContext{
		CityRoot:      p.cityPath,
		AgentName:     qualifiedName,
		TemplateName:  cfgAgent.Name,
		RigName:       rigName,
		RigRoot:       rigRoot,
		WorkDir:       workDir,
		IssuePrefix:   findRigPrefix(rigName, p.rigs),
		DefaultBranch: defaultBranchFor(workDir),
		WorkQuery:     cfgAgent.EffectiveWorkQuery(),
		SlingQuery:    cfgAgent.EffectiveSlingQuery(),
		Env:           cfgAgent.Env,
	}, p.sessionTemplate, p.stderr, p.packDirs, fragments, p.beadStore)
	beacon := runtime.FormatBeaconAt(p.cityName, qualifiedName, !hasHooks, p.beaconTime)
	if prompt != "" {
		prompt = beacon + "\n\n" + prompt
	} else {
		prompt = beacon
	}
	agentEnv["GC_PROVIDER"] = resolved.Name

	// Step 9b: Append the assigned-skills appendix when the agent
	// has a vendor sink, hasn't opted out, AND the runtime actually
	// delivers the skills to the session workdir. The appendix claims
	// "these skills are materialized in your provider's skill
	// directory and load automatically" — that claim has to match
	// reality, so we gate on the same availability conditions as
	// materialization itself:
	//
	//   - Stage-1-eligible runtime + workdir == scope root: stage 1
	//     wrote the sink into the scope root the agent sees.
	//   - Stage-2-eligible runtime (regardless of workdir): the
	//     session PreStart invokes `gc internal materialize-skills`
	//     into the session workdir before the agent starts.
	//
	// Agents for which neither path delivers (ACP, k8s, hybrid,
	// subprocess with WorkDir ≠ scope root — because subprocess
	// doesn't execute PreStart) get no appendix; we'd be lying to
	// them. Discovered via the pass-1 Codex review.
	if effectiveInjectAssignedSkills(cfgAgent) {
		wsProvider := ""
		if p.workspace != nil {
			wsProvider = p.workspace.Provider
		}
		provider := effectiveAgentProvider(cfgAgent, wsProvider)
		if _, ok := materialize.VendorSink(provider); ok {
			scopeRoot := agentScopeRoot(cfgAgent, p.cityPath, p.rigs)
			canonWorkDir := canonicaliseFilePath(workDir, p.cityPath)
			stage1Delivers := canStage1Materialize(p.sessionProvider, cfgAgent) && canonWorkDir == scopeRoot
			stage2Delivers := isStage2EligibleSession(p.sessionProvider, cfgAgent)
			if stage1Delivers || stage2Delivers {
				var agentCat materialize.AgentCatalog
				if cfgAgent.SkillsDir != "" {
					// Best-effort: a transient I/O failure loading the
					// agent catalog shouldn't break the prompt render.
					// The error is already surfaced via
					// effectiveSkillsForAgent's stderr path earlier in
					// the call graph.
					if c, err := materialize.LoadAgentCatalog(cfgAgent.SkillsDir); err == nil {
						agentCat = c
					}
				}
				if frag := buildAssignedSkillsPromptFragment(cfgAgent, p.skillCatalog, agentCat); frag != "" {
					prompt = prompt + "\n\n" + frag
				}
			}
		}
	}

	// Step 10: Merge environment layers.
	env := convergence.ScrubTokenEnv(mergeEnv(passthroughEnv(), expandEnvMap(resolved.Env), expandEnvMap(cfgAgent.Env), agentEnv))
	applyAssignedWorkContext(p.beadStore, SessionAssignmentLookup{
		SessionName: sessName,
		AgentName:   qualifiedName,
		Env:         env,
	})

	// Step 11: Expand session setup templates.
	configDir := p.cityPath
	if cfgAgent.SourceDir != "" {
		configDir = cfgAgent.SourceDir
	}
	setupCtx := SessionSetupContext{
		Session:   sessName,
		Agent:     qualifiedName,
		AgentBase: agentBase,
		Rig:       rigName,
		RigRoot:   rigRoot,
		CityRoot:  p.cityPath,
		CityName:  p.cityName,
		WorkDir:   workDir,
		ConfigDir: configDir,
	}
	if strings.Contains(command, "{{") {
		expanded := expandSessionSetup([]string{command}, setupCtx)
		command = expanded[0]
	}
	expandedSetup := expandSessionSetup(cfgAgent.SessionSetup, setupCtx)
	resolvedScript := resolveSetupScript(cfgAgent.SessionSetupScript, p.cityPath)
	expandedPreStart := expandSessionSetup(cfgAgent.PreStart, setupCtx)
	expandedLive := expandSessionSetup(cfgAgent.SessionLive, setupCtx)

	// Step 11b: Skill materialization integration (per engdocs
	// skill-materialization.md § "When FingerprintExtra[\"skills:*\"]
	// is populated" and § "Stage 2 runtime gate"). Stage-2 eligible
	// runtimes (tmux for v0.15.1) get a PreStart entry for per-session
	// materialization into non-scope-root workdirs, and every eligible
	// agent gets per-skill fingerprint entries so catalog edits drain.
	// Stage-2 ineligible runtimes (subprocess/acp/k8s/hybrid/...) get
	// neither — the materializer cannot reach them, so spurious
	// fingerprint drift would cause pointless drain-restart cycles.
	if isStage2EligibleSession(p.sessionProvider, cfgAgent) {
		scopeRoot := agentScopeRoot(cfgAgent, p.cityPath, p.rigs)
		canonWorkDir := canonicaliseFilePath(workDir, p.cityPath)
		wsProvider := ""
		if p.workspace != nil {
			wsProvider = p.workspace.Provider
		}
		desired := effectiveSkillsForAgent(p.skillCatalog, cfgAgent, wsProvider, p.stderr)
		if len(desired) > 0 {
			fpExtra = mergeSkillFingerprintEntries(fpExtra, desired)
			if canonWorkDir != scopeRoot {
				// Pool instances inherit their skill catalog from the
				// template, not the instance — namepool members (e.g.
				// repo/furiosa from polecat) are not resolvable as
				// standalone agents by `gc internal materialize-skills`.
				// templateNameFor returns cfgAgent.PoolName for pool
				// instances and qualifiedName for singletons.
				materializeAgent := templateNameFor(cfgAgent, qualifiedName)
				expandedPreStart = appendMaterializeSkillsPreStart(expandedPreStart, materializeAgent, workDir)
			}
		}
	}

	// Step 12: Build startup hints.
	hints := agent.StartupHints{
		ReadyPromptPrefix:      resolved.ReadyPromptPrefix,
		ReadyDelayMs:           resolved.ReadyDelayMs,
		ProcessNames:           resolved.ProcessNames,
		EmitsPermissionWarning: resolved.EmitsPermissionWarning,
		Nudge:                  cfgAgent.Nudge,
		PreStart:               expandedPreStart,
		SessionSetup:           expandedSetup,
		SessionSetupScript:     resolvedScript,
		SessionLive:            expandedLive,
		ProviderName:           resolved.Kind,
		InstallAgentHooks:      config.ResolveInstallHooks(cfgAgent, p.workspace),
		PackOverlayDirs:        effectiveOverlayDirs(p.packOverlayDirs, p.rigOverlayDirs, rigName),
		OverlayDir:             overlayDir,
		CopyFiles:              copyFiles,
	}

	return TemplateParams{
		Command:          command,
		Prompt:           prompt,
		Env:              env,
		Hints:            hints,
		WorkDir:          workDir,
		SessionName:      sessName,
		Alias:            qualifiedName,
		FPExtra:          fpExtra,
		ResolvedProvider: resolved,
		TemplateName:     templateNameFor(cfgAgent, qualifiedName),
		InstanceName:     qualifiedName,
		RigName:          rigName,
		RigRoot:          rigRoot,
		WakeMode:         cfgAgent.WakeMode,
		IsACP:            cfgAgent.Session == "acp",
		HookEnabled:      hasHooks,
		SessionOverride:  cfgAgent.Session,
	}, nil
}

func sessionDoltEnv(cityPath, rigRoot string, rigs []config.Rig) map[string]string {
	env := map[string]string{
		// Explicit empty values let tmux unset stale Dolt vars inherited from
		// the server environment when the current city/rig does not use them.
		"GC_DOLT_HOST":             "",
		"GC_DOLT_PORT":             "",
		"GC_DOLT_USER":             "",
		"GC_DOLT_PASSWORD":         "",
		"BEADS_DOLT_SERVER_HOST":   "",
		"BEADS_DOLT_SERVER_PORT":   "",
		"BEADS_DOLT_SERVER_USER":   "",
		"BEADS_DOLT_PASSWORD":      "",
		"BEADS_DOLT_SERVER_MODE":   "1",
		"BEADS_DOLT_SHARED_SERVER": "",
		// Suppress bd's built-in Dolt auto-start. The gc controller manages
		// the server; bd's CLI auto-start launches rogue servers from the
		// agent's cwd with the wrong data_dir.
		"BEADS_DOLT_AUTO_START": "0",
	}
	// Explicit empty values let tmux unset stale Dolt vars inherited from
	// the server environment when the current city/rig does not use them.
	setProjectedDoltEnvEmpty(env)

	// Session env projection must not trigger provider recovery. Session setup
	// only publishes the currently resolved target; store operations use the
	// bd runtime env when recovery is allowed.
	if rigRoot == "" {
		if err := applyResolvedCityDoltEnv(env, cityPath, false); err != nil {
			mirrorBeadsDoltEnv(env)
		}
		return env
	}

	if err := applyResolvedRigDoltEnv(env, cityPath, rigRoot, rigConfigForScopeRoot(cityPath, rigRoot, rigs), false); err != nil {
		mirrorBeadsDoltEnv(env)
	}
	return env
}

// templateParamsToConfig converts TemplateParams to the runtime.Config
// needed by Provider.Start. This mirrors managed.SessionConfig() at
// internal/agent/agent.go:292-315 — for the same inputs, both must
// produce identical output.
func templateParamsToConfig(tp TemplateParams) runtime.Config {
	var promptSuffix string
	var promptFlag string
	nudge := tp.Hints.Nudge
	deliverStartupViaHooks := tp.HookEnabled && tp.ResolvedProvider != nil && tp.ResolvedProvider.SupportsHooks
	if tp.Prompt != "" {
		// Hook-enabled providers prime themselves on SessionStart via
		// gc prime --hook, so the rendered role prompt must not also be
		// replayed as argv or a delayed startup nudge.
		if !deliverStartupViaHooks {
			if tp.ResolvedProvider != nil && tp.ResolvedProvider.PromptMode == "none" {
				if nudge != "" {
					nudge = tp.Prompt + "\n\n---\n\n" + nudge
				} else {
					nudge = tp.Prompt
				}
			} else {
				promptSuffix = shellquote.Quote(tp.Prompt)
				if tp.ResolvedProvider != nil && tp.ResolvedProvider.PromptMode == "flag" && tp.ResolvedProvider.PromptFlag != "" {
					promptFlag = tp.ResolvedProvider.PromptFlag
				}
			}
		}
	}
	startupEnvelope := buildStartupEnvelope(tp, tp.Prompt)
	return runtime.Config{
		Command:                tp.Command,
		PromptSuffix:           promptSuffix,
		PromptFlag:             promptFlag,
		Env:                    tp.Env,
		StartupEnvelope:        startupEnvelope,
		WorkDir:                tp.WorkDir,
		ReadyPromptPrefix:      tp.Hints.ReadyPromptPrefix,
		ReadyDelayMs:           tp.Hints.ReadyDelayMs,
		ProcessNames:           tp.Hints.ProcessNames,
		EmitsPermissionWarning: tp.Hints.EmitsPermissionWarning,
		Nudge:                  nudge,
		PreStart:               tp.Hints.PreStart,
		SessionSetup:           tp.Hints.SessionSetup,
		SessionSetupScript:     tp.Hints.SessionSetupScript,
		SessionLive:            tp.Hints.SessionLive,
		ProviderName:           tp.Hints.ProviderName,
		InstallAgentHooks:      tp.Hints.InstallAgentHooks,
		PackOverlayDirs:        tp.Hints.PackOverlayDirs,
		OverlayDir:             tp.Hints.OverlayDir,
		CopyFiles:              tp.Hints.CopyFiles,
		FingerprintExtra:       tp.FPExtra,
	}
}

type SessionAssignmentLookup struct {
	SessionName string
	AgentName   string
	Env         map[string]string
}

func applyAssignedWorkContext(store beads.Store, lookup SessionAssignmentLookup) {
	if store == nil || lookup.Env == nil {
		return
	}
	workBead, ok := findAssignedWorkBead(store, lookup.SessionName, lookup.AgentName)
	if !ok {
		return
	}
	lookup.Env["GC_BEAD"] = workBead.ID
	lookup.Env["GC_BEAD_TITLE"] = workBead.Title

	if workBead.Ref != "" {
		lookup.Env["GC_FORMULA"] = workBead.Ref
	}
	if formula := workBead.Metadata["formula"]; formula != "" {
		lookup.Env["GC_FORMULA"] = formula
	}

	current := workBead
	for depth := 0; depth < 32; depth++ {
		if current.ParentID == "" {
			return
		}
		parent, err := store.Get(current.ParentID)
		if err != nil {
			return
		}
		if parent.Type == "convoy" && lookup.Env["GC_CONVOY"] == "" {
			lookup.Env["GC_CONVOY"] = parent.ID
			lookup.Env["GC_CONVOY_TITLE"] = parent.Title
			if total, closed, ok := convoyProgress(store, parent.ID); ok {
				lookup.Env["GC_CONVOY_TOTAL_COUNT"] = fmt.Sprintf("%d", total)
				lookup.Env["GC_CONVOY_CLOSED_COUNT"] = fmt.Sprintf("%d", closed)
				if total > 0 && closed >= total {
					lookup.Env["GC_CONVOY_STATUS"] = "closed"
				} else {
					lookup.Env["GC_CONVOY_STATUS"] = "open"
				}
			}
		}
		if beads.IsMoleculeType(parent.Type) && lookup.Env["GC_MOLECULE"] == "" {
			lookup.Env["GC_MOLECULE"] = parent.ID
			if lookup.Env["GC_FORMULA"] == "" && parent.Ref != "" {
				lookup.Env["GC_FORMULA"] = parent.Ref
			}
			if lookup.Env["GC_FORMULA"] == "" {
				if formula := parent.Metadata["formula"]; formula != "" {
					lookup.Env["GC_FORMULA"] = formula
				}
			}
		}
		current = parent
	}
}

func convoyProgress(store beads.Store, convoyID string) (total int, closed int, ok bool) {
	children, err := store.Children(convoyID)
	if err != nil {
		return 0, 0, false
	}
	for _, child := range children {
		total++
		if child.Status == "closed" {
			closed++
		}
	}
	return total, closed, true
}

func findAssignedWorkBead(store beads.Store, sessionName, agentName string) (beads.Bead, bool) {
	assignees := make([]string, 0, 2)
	if sessionName != "" {
		assignees = append(assignees, sessionName)
	}
	if agentName != "" && agentName != sessionName {
		assignees = append(assignees, agentName)
	}
	for _, status := range []string{"in_progress", "open"} {
		for _, assignee := range assignees {
			matches, err := store.ListByAssignee(assignee, status, 1)
			if err != nil || len(matches) == 0 {
				continue
			}
			return matches[0], true
		}
	}
	return beads.Bead{}, false
}

func buildStartupEnvelope(tp TemplateParams, startupPrompt string) json.RawMessage {
	envelope := map[string]any{
		"version": 1,
		"gc": map[string]any{
			"cityPath":    tp.Env["GC_CITY_PATH"],
			"cityName":    filepath.Base(tp.Env["GC_CITY_PATH"]),
			"rigName":     tp.RigName,
			"rigPath":     tp.RigRoot,
			"agent":       tp.TemplateName,
			"template":    tp.TemplateName,
			"sessionName": tp.SessionName,
		},
		"runtime": map[string]any{
			"provider":         tp.Env["GC_PROVIDER"],
			"model":            startupEnvelopeModel(tp),
			"sessionTransport": tp.SessionOverride,
			"runtimeMode":      "full-access",
			"interactionMode":  "default",
			"workDir":          tp.WorkDir,
			"command":          tp.Command,
		},
		"startup": map[string]any{
			"promptTemplate": tp.TemplateName,
			"startupPrompt":  startupPrompt,
			"initialNudge":   tp.Hints.Nudge,
		},
		"assignment": map[string]any{
			"beadId":            tp.Env["GC_BEAD"],
			"beadTitle":         tp.Env["GC_BEAD_TITLE"],
			"convoyId":          tp.Env["GC_CONVOY"],
			"convoyTitle":       tp.Env["GC_CONVOY_TITLE"],
			"convoyStatus":      tp.Env["GC_CONVOY_STATUS"],
			"convoyClosedCount": tp.Env["GC_CONVOY_CLOSED_COUNT"],
			"convoyTotalCount":  tp.Env["GC_CONVOY_TOTAL_COUNT"],
			"moleculeId":        tp.Env["GC_MOLECULE"],
			"formula":           tp.Env["GC_FORMULA"],
		},
		"context": map[string]any{
			"gcEnv": map[string]any{
				"GC_AGENT":               tp.Env["GC_AGENT"],
				"GC_PROVIDER":            tp.Env["GC_PROVIDER"],
				"GC_TEMPLATE":            tp.Env["GC_TEMPLATE"],
				"GC_BEAD":                tp.Env["GC_BEAD"],
				"GC_BEAD_TITLE":          tp.Env["GC_BEAD_TITLE"],
				"GC_CONVOY":              tp.Env["GC_CONVOY"],
				"GC_CONVOY_TITLE":        tp.Env["GC_CONVOY_TITLE"],
				"GC_CONVOY_STATUS":       tp.Env["GC_CONVOY_STATUS"],
				"GC_CONVOY_CLOSED_COUNT": tp.Env["GC_CONVOY_CLOSED_COUNT"],
				"GC_CONVOY_TOTAL_COUNT":  tp.Env["GC_CONVOY_TOTAL_COUNT"],
				"GC_MOLECULE":            tp.Env["GC_MOLECULE"],
				"GC_FORMULA":             tp.Env["GC_FORMULA"],
				"GC_CITY_PATH":           tp.Env["GC_CITY_PATH"],
				"GC_RIG":                 tp.Env["GC_RIG"],
				"GC_SESSION_NAME":        tp.Env["GC_SESSION_NAME"],
			},
		},
		"resume": map[string]any{
			"policy":                 "match-or-recreate",
			"allowThreadReuse":       true,
			"requiredThreadProvider": tp.Env["GC_PROVIDER"],
			"requiredThreadModel":    startupEnvelopeModel(tp),
		},
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil
	}
	return json.RawMessage(data)
}

func startupEnvelopeModel(tp TemplateParams) string {
	if strings.Contains(tp.Command, "--model ") {
		parts := strings.Fields(tp.Command)
		for i := 0; i < len(parts)-1; i++ {
			if parts[i] == "--model" {
				return parts[i+1]
			}
		}
	}
	if tp.Env["GC_PROVIDER"] == "codex" {
		return "gpt-5-codex"
	}
	return "claude-opus-4-6"
}

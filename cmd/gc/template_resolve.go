// template_resolve.go extracts a pure function for resolving agent config
// into session parameters. This is the data-only half of buildOneAgent:
// all steps that compute values (provider resolution, dir expansion, env
// merging, prompt rendering) without side effects.
//
// Side effects (ACP route registration, hook installation) are handled
// by the caller (buildOneAgent).
//
// resolveTemplate returns TemplateParams — a value type suitable for
// session.Manager.CreateFromParams or for constructing runtime.Config.
package main

import (
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
	// For singletons it equals TemplateName; for pool instances it's "dog-1".
	InstanceName string
	// RigName is the resolved rig association (empty if none).
	RigName string
	// RigRoot is the absolute path to the associated rig root (empty if none).
	RigRoot string
	// WakeMode controls whether the next wake resumes or starts fresh conversation state.
	WakeMode string
	// IsACP is true if session = "acp".
	IsACP bool
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
}

// DisplayName returns the name to use for log messages and event subjects.
// For pool instances this is the instance name (e.g., "dog-1"); for
// singletons it equals TemplateName.
func (tp TemplateParams) DisplayName() string {
	if tp.InstanceName != "" {
		return tp.InstanceName
	}
	return tp.TemplateName
}

// resolveTemplate computes all session parameters from a config.Agent without
// side effects. This is a pure extraction of steps 1-13 and 15-16 from
// buildOneAgent. The only side effect excluded is ACP route registration
// (step 14), which the caller handles.
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
	workDir, err := resolveConfiguredWorkDir(p.cityPath, p.cityName, cfgAgent, p.rigs)
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
	if sa := settingsArgs(p.cityPath, resolved.Name); sa != "" {
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
		"GC_SESSION_NAME": sessName,
		"GC_SESSION_ID":   sessionBeadID,
		"GC_TEMPLATE":     templateNameFor(cfgAgent, qualifiedName),
		"GC_AGENT":        sessionBeadID,
		"GC_ALIAS":        qualifiedName,
		"BEADS_ACTOR":     sessName,
		"GC_DIR":          workDir,
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
	if exe, err := os.Executable(); err == nil && exe != "" {
		agentEnv["GC_BIN"] = exe
	}
	for key, value := range sessionDoltEnv(p.cityPath, rigRoot, p.rigs) {
		agentEnv[key] = value
	}
	if rigName != "" {
		agentEnv["GC_RIG"] = rigName
		agentEnv["GC_RIG_ROOT"] = rigRoot
		agentEnv["BEADS_DIR"] = filepath.Join(rigRoot, ".beads")
	}

	// Step 9: Render prompt with beacon.
	var prompt string
	fragments := mergeFragmentLists(p.globalFragments, cfgAgent.InjectFragments)
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
	hasHooks := config.AgentHasHooks(cfgAgent, p.workspace, resolved.Name)
	beacon := runtime.FormatBeaconAt(p.cityName, qualifiedName, !hasHooks, p.beaconTime)
	if prompt != "" {
		prompt = beacon + "\n\n" + prompt
	} else {
		prompt = beacon
	}

	// Step 10: Merge environment layers.
	env := convergence.ScrubTokenEnv(mergeEnv(passthroughEnv(), expandEnvMap(resolved.Env), expandEnvMap(cfgAgent.Env), agentEnv))

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
	}, nil
}

func sessionDoltEnv(cityPath, rigRoot string, rigs []config.Rig) map[string]string {
	env := map[string]string{
		// Explicit empty values let tmux unset stale Dolt vars inherited from
		// the server environment when the current city/rig does not use them.
		"GC_DOLT_HOST":           "",
		"GC_DOLT_PORT":           "",
		"GC_DOLT_USER":           "",
		"GC_DOLT_PASSWORD":       "",
		"BEADS_DOLT_SERVER_HOST": "",
		"BEADS_DOLT_SERVER_PORT": "",
		"BEADS_DOLT_SERVER_USER": "",
		"BEADS_DOLT_PASSWORD":    "",
		// Suppress bd's built-in Dolt auto-start. The gc controller manages
		// the server; bd's CLI auto-start launches rogue servers from the
		// agent's cwd with the wrong data_dir.
		"BEADS_DOLT_AUTO_START": "0",
	}

	if host := doltHostForCity(cityPath); host != "" {
		env["GC_DOLT_HOST"] = host
		env["BEADS_DOLT_SERVER_HOST"] = host
	}
	if user := os.Getenv("GC_DOLT_USER"); user != "" {
		env["GC_DOLT_USER"] = user
		env["BEADS_DOLT_SERVER_USER"] = user
	}
	if pass := os.Getenv("GC_DOLT_PASSWORD"); pass != "" {
		env["GC_DOLT_PASSWORD"] = pass
		env["BEADS_DOLT_PASSWORD"] = pass
	}
	if isExternalDolt(cityPath) {
		if port := doltPortForCity(cityPath); port != "" {
			env["GC_DOLT_PORT"] = port
			env["BEADS_DOLT_SERVER_PORT"] = port
		}
	} else if port := currentDoltPort(cityPath); port != "" {
		env["GC_DOLT_PORT"] = port
		env["BEADS_DOLT_SERVER_PORT"] = port
	}
	if rigRoot == "" {
		return env
	}

	for _, r := range rigs {
		rp := r.Path
		if !filepath.IsAbs(rp) {
			rp = filepath.Join(cityPath, rp)
		}
		if filepath.Clean(rp) != filepath.Clean(rigRoot) {
			continue
		}
		if r.DoltHost != "" {
			env["GC_DOLT_HOST"] = r.DoltHost
			env["BEADS_DOLT_SERVER_HOST"] = r.DoltHost
		}
		if r.DoltPort != "" {
			env["GC_DOLT_PORT"] = r.DoltPort
			env["BEADS_DOLT_SERVER_PORT"] = r.DoltPort
		}
		if r.DoltHost != "" || r.DoltPort != "" {
			return env
		}
		break
	}

	if port := currentDoltPort(rigRoot); port != "" {
		env["GC_DOLT_HOST"] = ""
		env["BEADS_DOLT_SERVER_HOST"] = ""
		env["GC_DOLT_PORT"] = port
		env["BEADS_DOLT_SERVER_PORT"] = port
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
	if tp.Prompt != "" {
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
	return runtime.Config{
		Command:                tp.Command,
		PromptSuffix:           promptSuffix,
		PromptFlag:             promptFlag,
		Env:                    tp.Env,
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
		PackOverlayDirs:        tp.Hints.PackOverlayDirs,
		OverlayDir:             tp.Hints.OverlayDir,
		CopyFiles:              tp.Hints.CopyFiles,
		FingerprintExtra:       tp.FPExtra,
	}
}

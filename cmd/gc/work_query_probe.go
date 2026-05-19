package main

import (
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func controllerQueryRuntimeEnv(cityPath string, cfg *config.City, agentCfg *config.Agent) map[string]string {
	if strings.TrimSpace(cityPath) == "" || cfg == nil || agentCfg == nil {
		return nil
	}
	var source map[string]string
	if rigName := configuredRigName(cityPath, agentCfg, cfg.Rigs); rigName != "" {
		if rigRoot := rigRootForName(rigName, cfg.Rigs); rigRoot != "" {
			if !scopeUsesManagedBdStoreContract(cityPath, rigRoot) {
				return nil
			}
			source = bdRuntimeEnvForRig(cityPath, cfg, rigRoot)
		} else {
			if !scopeUsesManagedBdStoreContract(cityPath, cityPath) {
				return nil
			}
			source = bdRuntimeEnv(cityPath)
		}
	} else {
		if !scopeUsesManagedBdStoreContract(cityPath, cityPath) {
			return nil
		}
		source = bdRuntimeEnv(cityPath)
	}
	if len(source) == 0 {
		return nil
	}
	env := make(map[string]string, len(source))
	for key, value := range source {
		env[key] = value
	}
	return env
}

func controllerWorkQueryEnv(cityPath string, cfg *config.City, agentCfg *config.Agent) map[string]string {
	if strings.TrimSpace(cityPath) == "" || cfg == nil || agentCfg == nil {
		return nil
	}
	env := cityRuntimeEnvMapForCity(cityPath)
	env["GC_STORE_ROOT"] = cityPath
	env["GC_STORE_SCOPE"] = "city"
	env["GC_BEADS_PREFIX"] = config.EffectiveHQPrefix(cfg)
	env["GC_RIG"] = ""
	env["GC_RIG_ROOT"] = ""
	if rigName := configuredRigName(cityPath, agentCfg, cfg.Rigs); rigName != "" {
		if rigRoot := rigRootForName(rigName, cfg.Rigs); rigRoot != "" {
			env["GC_STORE_ROOT"] = rigRoot
			env["GC_STORE_SCOPE"] = "rig"
			env["GC_RIG"] = rigName
			env["GC_RIG_ROOT"] = rigRoot
			if rig, ok := rigByName(cfg, rigName); ok {
				env["GC_BEADS_PREFIX"] = rig.EffectivePrefix()
				env["GC_RIG"] = rig.Name
			}
		}
	}
	for key, value := range controllerQueryRuntimeEnv(cityPath, cfg, agentCfg) {
		env[key] = value
	}
	return env
}

func controllerQueryPrefixEnv(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	env := map[string]string{}
	// Only include connection coordinates (host/port) in the shell prefix —
	// NOT credentials. Passwords serialized into the command string would be
	// visible in process listings. Full canonical probe env is supplied via the
	// subprocess environment by the controller probe runners.
	for _, key := range []string{
		"GC_DOLT_HOST", "GC_DOLT_PORT",
	} {
		if value := strings.TrimSpace(source[key]); value != "" {
			env[key] = value
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

func controllerQueryEnv(cityPath string, cfg *config.City, agentCfg *config.Agent) map[string]string {
	return controllerQueryPrefixEnv(controllerQueryRuntimeEnv(cityPath, cfg, agentCfg))
}

func prefixedWorkQueryForProbe(
	cfg *config.City,
	cityPath string,
	cityName string,
	store beads.Store,
	sessionBeads *sessionBeadSnapshot,
	agentCfg *config.Agent,
	stderr io.Writer,
) string {
	return prefixedWorkQueryForProbeWithEnv(controllerQueryEnv(cityPath, cfg, agentCfg), cfg, cityPath, cityName, store, sessionBeads, agentCfg, stderr)
}

func prefixedWorkQueryForProbeWithEnv(
	queryEnv map[string]string,
	cfg *config.City,
	cityPath string,
	cityName string,
	store beads.Store,
	sessionBeads *sessionBeadSnapshot,
	agentCfg *config.Agent,
	stderr io.Writer,
) string {
	if agentCfg == nil {
		return ""
	}
	command := strings.TrimSpace(agentCfg.EffectiveWorkQuery())
	// Expand {{.Rig}}/{{.AgentBase}} so rig-scoped agents probe with
	// rig-specific metadata. Mirrors the scale_check expansion in
	// build_desired_state.go; #793. Malformed templates are logged to
	// stderr (when supplied) and fall back to the raw command.
	command = expandAgentCommandTemplate(cityPath, cityName, agentCfg, cfg.Rigs, "work_query", command, stderr)
	if command == "" || agentCfg.SupportsMultipleSessions() {
		return prefixShellEnv(queryEnv, command)
	}
	sessionName := probeSessionNameForTemplate(cfg, cityName, store, sessionBeads, agentCfg.QualifiedName())
	if sessionName == "" {
		return prefixShellEnv(queryEnv, command)
	}
	env := cloneStringMap(queryEnv)
	if env == nil {
		env = map[string]string{}
	}
	env["GC_AGENT"] = agentCfg.QualifiedName()
	env["GC_SESSION_NAME"] = sessionName
	env["GC_TEMPLATE"] = agentCfg.QualifiedName()
	return prefixShellEnv(env, command)
}

func probeSessionNameForTemplate(
	cfg *config.City,
	cityName string,
	store beads.Store,
	sessionBeads *sessionBeadSnapshot,
	identity string,
) string {
	identity = normalizeNamedSessionTarget(identity)
	if identity == "" {
		return ""
	}
	if cfg != nil {
		if spec, ok := findNamedSessionSpec(cfg, cityName, identity); ok {
			if sessionBeads != nil {
				if bead, ok := findCanonicalNamedSessionBead(sessionBeads, spec); ok {
					if sn := strings.TrimSpace(bead.Metadata["session_name"]); sn != "" {
						return sn
					}
				}
			}
			return spec.SessionName
		}
	}
	if sessionBeads != nil {
		if sn := sessionBeads.FindSessionNameByTemplate(identity); sn != "" {
			return sn
		}
	}
	if store != nil {
		if sn, ok := lookupSessionName(store, identity); ok {
			return sn
		}
	}
	sessionTemplate := ""
	if cfg != nil {
		sessionTemplate = cfg.Workspace.SessionTemplate
	}
	return agent.SessionNameFor(cityName, identity, sessionTemplate)
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func prefixShellEnv(env map[string]string, command string) string {
	command = strings.TrimSpace(command)
	if command == "" || len(env) == 0 {
		return command
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		if strings.TrimSpace(key) == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return command
	}
	parts := make([]string, 0, len(keys)+1)
	for _, key := range keys {
		parts = append(parts, key+"="+shellquote.Quote(env[key]))
	}
	parts = append(parts, command)
	return strings.Join(parts, " ")
}

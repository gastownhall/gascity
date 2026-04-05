package main

import (
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func controllerQueryEnv(cityPath string, cfg *config.City, agentCfg *config.Agent) map[string]string {
	if strings.TrimSpace(cityPath) == "" || cfg == nil || agentCfg == nil {
		return nil
	}
	if rawBeadsProvider(cityPath) != "bd" {
		return nil
	}
	var source map[string]string
	if agentCfg.Dir != "" {
		source = bdRuntimeEnvForRig(cityPath, cfg, agentCommandDir(cityPath, agentCfg, cfg.Rigs))
	} else {
		source = bdRuntimeEnv(cityPath)
	}
	if len(source) == 0 {
		return nil
	}
	env := map[string]string{}
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_HOST", "BEADS_DOLT_PORT"} {
		if value, ok := source[key]; ok {
			env[key] = value
		}
	}
	if env["BEADS_DOLT_HOST"] == "" {
		env["BEADS_DOLT_HOST"] = env["GC_DOLT_HOST"]
	}
	if env["BEADS_DOLT_PORT"] == "" {
		env["BEADS_DOLT_PORT"] = env["GC_DOLT_PORT"]
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

func prefixControllerQueryEnv(cityPath string, cfg *config.City, agentCfg *config.Agent, command string) string {
	return prefixShellEnv(controllerQueryEnv(cityPath, cfg, agentCfg), command)
}

func prefixedWorkQueryForProbe(
	cfg *config.City,
	cityPath string,
	cityName string,
	store beads.Store,
	sessionBeads *sessionBeadSnapshot,
	agentCfg *config.Agent,
) string {
	if agentCfg == nil {
		return ""
	}
	command := strings.TrimSpace(agentCfg.EffectiveWorkQuery())
	if command == "" || isMultiSessionCfgAgent(agentCfg) {
		return prefixControllerQueryEnv(cityPath, cfg, agentCfg, command)
	}
	sessionName := probeSessionNameForTemplate(cfg, cityName, store, sessionBeads, agentCfg.QualifiedName())
	if sessionName == "" {
		return prefixControllerQueryEnv(cityPath, cfg, agentCfg, command)
	}
	env := controllerQueryEnv(cityPath, cfg, agentCfg)
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
				if bead, ok := findCanonicalNamedSessionBead(sessionBeads, spec.Identity); ok {
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

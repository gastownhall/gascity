package main

import (
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func prefixedWorkQueryForProbe(
	cfg *config.City,
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
		return command
	}
	sessionName := probeSessionNameForTemplate(cfg, cityName, store, sessionBeads, agentCfg.QualifiedName())
	if sessionName == "" {
		return command
	}
	return prefixShellEnv(map[string]string{
		"GC_AGENT":        agentCfg.QualifiedName(),
		"GC_SESSION_NAME": sessionName,
		"GC_TEMPLATE":     agentCfg.QualifiedName(),
	}, command)
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

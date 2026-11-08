package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func controllerQueryRuntimeEnv(cityPath string, cfg *config.City, agentCfg *config.Agent) (map[string]string, error) {
	return queryRuntimeEnvFrom(cityPath, cfg, agentCfg, func(scopeRoot string, rig bool) (map[string]string, error) {
		if rig {
			return bdRuntimeEnvForRigWithError(cityPath, cfg, scopeRoot)
		}
		return bdRuntimeEnvWithError(cityPath)
	})
}

// observationQueryRuntimeEnv is controllerQueryRuntimeEnv for a verify-only
// caller: it classifies the scope first, so a stopped or stale store answers
// with a typed unavailable error, and builds the env with managed-dolt
// recovery off (a literal false, which the census guard reads).
func observationQueryRuntimeEnv(cityPath string, cfg *config.City, agentCfg *config.Agent) (map[string]string, error) {
	return queryRuntimeEnvFrom(cityPath, cfg, agentCfg, func(scopeRoot string, rig bool) (map[string]string, error) {
		if err := classifyObservationScope(cityPath, scopeRoot, observationScopeLabel(cityPath, scopeRoot, cfg)); err != nil {
			return nil, err
		}
		if rig {
			return bdRuntimeEnvForRigWithErrorRecovery(cityPath, cfg, scopeRoot, false)
		}
		return bdRuntimeEnvWithErrorRecovery(cityPath, false)
	})
}

// queryRuntimeEnvFrom resolves the query scope and copies the env its builder
// returns. The builder decides whether managed-dolt recovery may run.
func queryRuntimeEnvFrom(cityPath string, cfg *config.City, agentCfg *config.Agent, build func(scopeRoot string, rig bool) (map[string]string, error)) (map[string]string, error) {
	if strings.TrimSpace(cityPath) == "" || cfg == nil || agentCfg == nil {
		return nil, nil
	}
	var source map[string]string
	var err error
	if rigName := configuredRigName(cityPath, agentCfg, cfg.Rigs); rigName != "" {
		if rigRoot := rigRootForName(rigName, cfg.Rigs); rigRoot != "" {
			if !scopeUsesManagedBdStoreContract(cityPath, rigRoot) {
				return nil, nil
			}
			source, err = build(rigRoot, true)
		} else {
			if !scopeUsesManagedBdStoreContract(cityPath, cityPath) {
				return nil, nil
			}
			source, err = build(cityPath, false)
		}
	} else {
		if !scopeUsesManagedBdStoreContract(cityPath, cityPath) {
			return nil, nil
		}
		source, err = build(cityPath, false)
	}
	if err != nil {
		return nil, err
	}
	if len(source) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(source))
	for key, value := range source {
		env[key] = value
	}
	return env, nil
}

func controllerWorkQueryEnv(cityPath string, cfg *config.City, agentCfg *config.Agent) (map[string]string, error) {
	return workQueryEnvFrom(cityPath, cfg, agentCfg, controllerQueryRuntimeEnv)
}

// observationWorkQueryEnv is controllerWorkQueryEnv over the verify-only
// runtime env.
func observationWorkQueryEnv(cityPath string, cfg *config.City, agentCfg *config.Agent) (map[string]string, error) {
	return workQueryEnvFrom(cityPath, cfg, agentCfg, observationQueryRuntimeEnv)
}

// unavailableWorkQueryEnv is the static work-query env of a scope that
// classified as stopped or stale: its identity and scope coordinates without a
// store connection, and managed-dolt auto-start off, so a custom query an
// operator wrote cannot start the server the observation found down.
func unavailableWorkQueryEnv(cityPath string, cfg *config.City, agentCfg *config.Agent) (map[string]string, error) {
	env, err := workQueryEnvFrom(cityPath, cfg, agentCfg, func(string, *config.City, *config.Agent) (map[string]string, error) {
		return nil, nil
	})
	if err != nil || env == nil {
		return env, err
	}
	env["BEADS_DOLT_AUTO_START"] = "0"
	return env, nil
}

// workQueryEnvFrom builds the work-query env; runtimeEnv supplies the
// store-connection half, and decides whether recovery may run.
func workQueryEnvFrom(cityPath string, cfg *config.City, agentCfg *config.Agent, runtimeEnv func(string, *config.City, *config.Agent) (map[string]string, error)) (map[string]string, error) {
	if strings.TrimSpace(cityPath) == "" || cfg == nil || agentCfg == nil {
		return nil, nil
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
	queryEnv, err := runtimeEnv(cityPath, cfg, agentCfg)
	if err != nil {
		return nil, err
	}
	for key, value := range queryEnv {
		env[key] = value
	}
	return env, nil
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

func controllerQueryEnv(cityPath string, cfg *config.City, agentCfg *config.Agent) (map[string]string, error) {
	runtimeEnv, err := controllerQueryRuntimeEnv(cityPath, cfg, agentCfg)
	if err != nil {
		return nil, err
	}
	return controllerQueryPrefixEnv(runtimeEnv), nil
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
	queryEnv, err := controllerQueryEnv(cityPath, cfg, agentCfg)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "work_query probe env: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		return ""
	}
	return prefixedWorkQueryForProbeWithEnv(queryEnv, cfg, cityPath, cityName, store, sessionBeads, agentCfg, stderr)
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
	beadsCfg := config.BeadsConfig{}
	var rigs []config.Rig
	if cfg != nil {
		beadsCfg = cfg.Beads
		rigs = cfg.Rigs
	}
	// Controller-owned: config.QueryTopology{Beads: ...} and NOT
	// cityQueryTopology. Resolving the federation fact means asking the
	// storage routes, and the one-shot funnel that answers for a CLI command
	// (cliStorageRoutes) is explicitly for the half of the program that
	// "never builds a CityRuntime" — a controller reaching it would open the
	// city's binding a second time in a process that already holds it open.
	// The controller's own routes are the right source; threading them into
	// this plumbing is a change to controller wiring, not part of swapping
	// the reader, so it stays with the claim-routing slice (ga-601v2).
	command := strings.TrimSpace(agentCfg.EffectiveWorkQueryFor(config.QueryTopology{Beads: beadsCfg}))
	// Expand {{.Rig}}/{{.AgentBase}} so rig-scoped agents probe with
	// rig-specific metadata. Mirrors the scale_check expansion in
	// build_desired_state.go; #793. Malformed templates are logged to
	// stderr (when supplied) and fall back to the raw command.
	command = expandAgentCommandTemplate(cityPath, cityName, agentCfg, rigs, "work_query", command, stderr)
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
	env["GC_TEMPLATE"] = agentutil.RoutedToIdentity(agentCfg)
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
				if info, ok := findCanonicalNamedSessionInfo(sessionBeads, spec); ok {
					if sn := strings.TrimSpace(info.SessionNameMetadata); sn != "" {
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

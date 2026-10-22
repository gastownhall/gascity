package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
)

// crossStoreClaimDir resolves the working directory a cross-store-eligible
// agent's bd write (claim / update) must run in so it targets the bead's OWN
// store rather than the agent's default store. This is the WRITE counterpart to
// the gc-hook read federation (vp-kvp stage iii): a city-scoped singleton sees
// rig-store work via the federated work query, so its claim must land in that
// rig store.
//
// Returns ("", false, nil) when no redirection applies — a rig-scoped agent
// (byte-for-byte unchanged), or a bead whose prefix does not resolve to a
// configured rig (HQ/city-owned work stays in the agent's own store). The pure
// signature keeps the per-bead routing decision unit-testable; the impure
// store-env edge lives in agentScriptCrossStoreBeadEnv.
//
// It returns an error, for every agent and before any redirection, when the
// bead's prefix maps to a rig with federation = "rig" (or to two guarded rigs,
// which admit nothing) and actor is not admitted by it: the write would run
// as an identity outside the rig's namespace. sessions is this city's own
// session resolver (nil admits no pool session id).
func crossStoreClaimDir(cfg *config.City, agentCfg *config.Agent, beadID, actor string, sessions ownSessionResolver) (string, bool, error) {
	if cfg == nil {
		return "", false, nil
	}
	beadID = strings.TrimSpace(beadID)
	if ns, guarded := rigNamespaceForBeadID(cfg, beadID, sessions); guarded && !rigNamespacePinAdmitted(ns, actor) {
		return "", false, newRigNamespacePathError("rig %q (federation = \"rig\") serves only %s; claim actor %q is outside it%s", rigNamespaceLabel(ns), rigNamespaceShown(ns), strings.TrimSpace(actor), rigNamespaceActorHint(ns, actor))
	}
	if !agentIsCrossStoreEligible(agentCfg) {
		return "", false, nil
	}
	rigDir := sling.RigDirForBead(cfg, beadID)
	if rigDir == "" {
		return "", false, nil
	}
	return rigDir, true, nil
}

// agentScriptCrossStoreBeadEnv is the impure edge wired into the agent-script
// bd_claim / bd_update path: it resolves the current agent identity and city
// config from the environment and, for a cross-store-eligible runner, returns
// the rig store directory + bd subprocess env a write against beadID must use.
// Best-effort — a resolution failure returns ok=false so the write falls back
// to the default (inherited) environment, preserving rig-agent behavior.
//
// The rig namespace guard is not best-effort: once the city resolves, a write
// against a bead of a guarded rig whose actor the rig does not admit is an
// error, whether or not the redirection could be resolved, and so is a city
// config that cannot be loaded to tell. No bd process runs for it.
func agentScriptCrossStoreBeadEnv(beadID string) (string, []string, bool, error) {
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return "", nil, false, nil
	}
	cityPath, err := resolveCity()
	if err != nil {
		return "", nil, false, nil
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return "", nil, false, fmt.Errorf("loading city config to check bead %s: %w", beadID, err)
	}
	// Normalize relative rig paths to absolute so RigDirForBead returns a usable
	// command dir, matching cmd_hook's resolveRigPaths call after loadCityConfig.
	resolveRigPaths(cityPath, cfg.Rigs)

	var sessions ownSessionResolver
	if cityHasGuardedRig(cfg) {
		sessions = newCityCLISessionResolver(cityPath, cfg, func() (beads.SessionStore, error) {
			cityStore, err := openCityStoreAtWithConfig(cityPath, cfg)
			if err != nil {
				return beads.SessionStore{}, err
			}
			return cliStores(cityPath, cityStore).Sessions(), nil
		})
	}
	var agentCfg *config.Agent
	if agentName := agentScriptAgentName(); agentName != "" {
		if a, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg)); ok {
			agentCfg = &a
		}
	}
	rigDir, ok, err := crossStoreClaimDir(cfg, agentCfg, beadID, agentScriptClaimActor(), sessions)
	if err != nil || !ok {
		return "", nil, false, err
	}
	overrides, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	if err != nil {
		return "", nil, false, nil
	}
	return rigDir, mergeRuntimeEnv(os.Environ(), overrides), true, nil
}

// agentScriptAgentName is the agent identity the runner's config is resolved
// by: its alias, else GC_AGENT.
func agentScriptAgentName() string {
	if agentName := strings.TrimSpace(os.Getenv("GC_ALIAS")); agentName != "" {
		return agentName
	}
	return strings.TrimSpace(os.Getenv("GC_AGENT"))
}

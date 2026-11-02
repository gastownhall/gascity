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
// store the write lands in is a rig with federation = "rig" (or the bead's
// prefix maps to a guarded rig) and actor is not admitted by it
// (agentScriptWriteNamespace). runnerRig is the runner's own rig, whose
// store an unredirected write lands in. sessions is this city's own session
// resolver (nil admits no pool session id).
func crossStoreClaimDir(cfg *config.City, agentCfg *config.Agent, beadID, actor, runnerRig string, sessions ownSessionResolver) (string, bool, error) {
	if cfg == nil {
		return "", false, nil
	}
	beadID = strings.TrimSpace(beadID)
	rigDir := ""
	if agentIsCrossStoreEligible(agentCfg) {
		rigDir = sling.RigDirForBead(cfg, beadID)
	}
	// Rig paths are absolute here (agentScriptCrossStoreBeadEnv resolves
	// them), so no city path is needed to find the runner's store.
	if ns := agentScriptWriteNamespace(cfg, "", beadID, rigDir != "", runnerRig, sessions); ns != nil && !rigNamespacePinAdmitted(ns, actor) {
		return "", false, newRigNamespacePathError("rig %s (federation = \"rig\") serves only %s; claim actor %q is outside it%s", rigNamespaceLabel(ns), rigNamespaceShown(ns), strings.TrimSpace(actor), rigNamespaceActorHint(ns, actor))
	}
	if rigDir == "" {
		return "", false, nil
	}
	return rigDir, true, nil
}

// agentScriptWriteNamespace returns the guarded namespace an agent-script
// write against beadID is judged by, or nil. A redirected write lands in the
// store the bead's prefix names, so the prefix decides. An unredirected one
// lands in the runner's own store, so the runner's rig is the leg and the
// prefix only adds a deny (rigNamespaceForLeg).
func agentScriptWriteNamespace(cfg *config.City, cityPath, beadID string, redirected bool, runnerRig string, sessions ownSessionResolver) *rigNamespace {
	var leg *rigNamespace
	if !redirected {
		leg = guardedRigNamespaceForRigName(cfg, cityPath, runnerRig, sessions)
	}
	return rigNamespaceForLeg(leg, cfg, beadID, sessions)
}

// agentScriptCrossStoreBeadEnv is the impure edge wired into the agent-script
// bd_claim / bd_update path: it resolves the current agent identity and city
// config from the environment and, for a cross-store-eligible runner, returns
// the rig store directory + bd subprocess env a write against beadID must use.
// Best-effort — a resolution failure returns ok=false so the write falls back
// to the default (inherited) environment, preserving rig-agent behavior.
//
// The rig namespace guard is not best-effort: once the city resolves, a write
// that lands in a guarded rig's store is checked like `gc bd` checks it (the
// actor, the row's class, the assignee, routes and the creation stamp; see
// bdRigNamespaceCheck), whether or not the redirection could be resolved, and
// a city config that cannot be loaded to tell is an error. No bd process runs
// for a refused write.
func agentScriptCrossStoreBeadEnv(beadID string, bdArgs []string) (string, []string, bool, error) {
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
	return agentScriptBeadEnvForCity(cityPath, cfg, beadID, bdArgs)
}

// agentScriptBeadEnvForCity is agentScriptCrossStoreBeadEnv once the city
// and its config are resolved.
func agentScriptBeadEnvForCity(cityPath string, cfg *config.City, beadID string, bdArgs []string) (string, []string, bool, error) {
	var sessions ownSessionResolver
	guarded := cityHasGuardedRig(cfg)
	if guarded {
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
	runnerRig := configuredRigName(cityPath, agentCfg, cfg.Rigs)
	if runnerRig == "" && agentCfg == nil {
		runnerRig = agentScriptRig()
	}
	rigDir, ok, err := crossStoreClaimDir(cfg, agentCfg, beadID, agentScriptClaimActor(), runnerRig, sessions)
	if err != nil {
		return "", nil, false, err
	}
	if guarded {
		if ns := agentScriptWriteNamespace(cfg, cityPath, beadID, ok, runnerRig, sessions); ns != nil {
			scope := rigDir
			if !ok {
				scope = rigRootForName(runnerRig, cfg.Rigs)
			}
			if err := agentScriptRigNamespaceCheck(ns, agentScriptRowReader(cityPath, cfg, scope), bdArgs); err != nil {
				return "", nil, false, err
			}
		}
	}
	if !ok {
		return "", nil, false, nil
	}
	overrides, err := bdRuntimeEnvForRigWithError(cityPath, cfg, rigDir)
	if err != nil {
		return "", nil, false, nil
	}
	return rigDir, mergeRuntimeEnv(os.Environ(), overrides), true, nil
}

// agentScriptRowReader reads rows of the guarded rig's store at scope
// through its bd front door. A scope it cannot name is an error, so the row
// cannot be classified and the write is refused.
func agentScriptRowReader(cityPath string, cfg *config.City, scope string) func(string) (beads.Bead, error) {
	var reader beads.Store
	return func(id string) (beads.Bead, error) {
		if strings.TrimSpace(scope) == "" {
			return beads.Bead{}, fmt.Errorf("no store for the guarded rig")
		}
		if reader == nil {
			reader = bdWriteGuardStore(scope, cityPath, cfg)
		}
		return reader.Get(id)
	}
}

// agentScriptRigNamespaceCheck runs the `gc bd` check over an agent-script
// bd argv against a guarded rig's store, reading rows through get.
func agentScriptRigNamespaceCheck(ns *rigNamespace, get func(string) (beads.Bead, error), bdArgs []string) error {
	check := &bdRigNamespaceCheck{
		ns:    ns,
		actor: bdEffectiveActorFromEnv(os.Getenv),
		get:   get,
	}
	if _, msg, refused := check.check(bdArgs); refused {
		return newRigNamespacePathError("%s", strings.TrimPrefix(strings.TrimSuffix(msg, "\n"), "gc bd: "))
	}
	return nil
}

// agentScriptAgentName is the agent identity the runner's config is resolved
// by: its alias, else GC_AGENT.
func agentScriptAgentName() string {
	if agentName := strings.TrimSpace(os.Getenv("GC_ALIAS")); agentName != "" {
		return agentName
	}
	return strings.TrimSpace(os.Getenv("GC_AGENT"))
}

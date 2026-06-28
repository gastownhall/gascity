package main

import (
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// hookStore is one store the hook work_query runs against: a working dir and
// the rig/city-scoped subprocess env that points bd at that store.
type hookStore struct {
	dir string
	env []string
}

// hookIdentityEnvKeys are the identity overrides that must stay constant across
// every federated store attempt — the query always matches the agent's OWN
// identity (gc.routed_to / assignee == this identity) regardless of which store
// it reads.
var hookIdentityEnvKeys = []string{
	"GC_AGENT", "GC_SESSION_NAME", "GC_ALIAS",
	"GC_SESSION_ID", "GC_SESSION_ORIGIN", "GC_TEMPLATE",
}

// appendRigHookStores adds one hookStore per rig for a cross-store-eligible
// (city-scoped) agent — vp-kvp stage iii read federation. Each entry reuses the
// rig's store env (built the same way controller probes build it, via a per-rig
// agent view) while keeping the city agent's identity overrides, so the query
// reads the RIG store but still matches work routed/assigned to the city agent.
// Best-effort: a rig whose env cannot be built is skipped (the agent's own store
// is always queried first by the caller).
func appendRigHookStores(stores []hookStore, cityPath string, cfg *config.City, a *config.Agent, identityOverrides map[string]string) []hookStore {
	if cfg == nil || a == nil {
		return stores
	}
	for i := range cfg.Rigs {
		stores = appendOneRigHookStore(stores, cityPath, cfg, a, cfg.Rigs[i].Name, identityOverrides)
	}
	return stores
}

// appendOneRigHookStore appends the hookStore for a single named rig, reusing
// the per-rig env machinery (a per-rig agent view whose store env points bd at
// that rig, with the agent's identity overrides preserved so the query still
// matches work routed/assigned to this agent). Best-effort: returns stores
// unchanged if the rig is unknown or its env cannot be built. Shared by
// appendRigHookStores (city-scoped read federation, #2877, which keeps the
// agent's own store first) and the rig-scoped hook path (which puts the rig
// store first, as the agent's primary store).
func appendOneRigHookStore(stores []hookStore, cityPath string, cfg *config.City, a *config.Agent, rigName string, identityOverrides map[string]string) []hookStore {
	rigName = strings.TrimSpace(rigName)
	if cfg == nil || a == nil || rigName == "" {
		return stores
	}
	known := false
	for i := range cfg.Rigs {
		if strings.TrimSpace(cfg.Rigs[i].Name) == rigName {
			known = true
			break
		}
	}
	if !known {
		return stores
	}
	view := *a
	view.Dir = rigName
	rigEnv, err := hookQueryEnv(cityPath, cfg, &view)
	if err != nil || rigEnv == nil {
		return stores
	}
	for _, k := range hookIdentityEnvKeys {
		if v, ok := identityOverrides[k]; ok {
			rigEnv[k] = v
		}
	}
	return append(stores, hookStore{
		dir: agentCommandDir(cityPath, &view, cfg.Rigs),
		env: mergeRuntimeEnv(os.Environ(), rigEnv),
	})
}

// appendCityHookStore appends the CITY store as a best-effort federated entry
// for a rig-scoped agent — the mirror of the #2877 city→rig read federation in
// the opposite direction. Root-only (city-store) beads can be assigned to a
// rig-scoped agent (e.g. singleton patrol wisps created at city scope for a
// rig witness), and none of the agent's other entries reach them: the rig
// store is its primary, and a rig-backed agent's own work-query env is ALSO
// rig-scoped (controllerWorkQueryEnv switches to rig coordinates whenever the
// agent has a configured rig). A city view of the agent (Dir cleared) keeps
// controllerWorkQueryEnv at city coordinates; identity overrides are preserved
// so the query still matches work routed/assigned to this agent. Best-effort:
// returns stores unchanged when the city env cannot be built, and the city
// entry is appended LAST so the rig store keeps firstStoreWithWork's
// emit-on-timeout contract as the primary entry.
func appendCityHookStore(stores []hookStore, cityPath string, cfg *config.City, a *config.Agent, identityOverrides map[string]string) []hookStore {
	if cfg == nil || a == nil {
		return stores
	}
	view := *a
	view.Dir = ""
	cityEnv, err := hookQueryEnv(cityPath, cfg, &view)
	if err != nil || cityEnv == nil {
		return stores
	}
	for _, k := range hookIdentityEnvKeys {
		if v, ok := identityOverrides[k]; ok {
			cityEnv[k] = v
		}
	}
	return append(stores, hookStore{
		dir: cityPath,
		env: mergeRuntimeEnv(os.Environ(), cityEnv),
	})
}

// rigScopedHookRig returns the rig whose store a rig-scoped agent must ALSO
// query, or "" if none applies. A rig-scoped agent's identity is "<rig>/<name>"
// (its GC_AGENT) and its routed work lives in the <rig> store, which the agent's
// own (city-scoped) work-query env never reaches — so without this the hook
// returns empty, the session spawns, finds nothing, and exits (churn).
// Returns "" for a city-scoped identity (no "/") or an unknown rig, so a caller
// only adds a real rig store. City-scoped agents already federate every rig via
// appendRigHookStores and must not use this path.
func rigScopedHookRig(cfg *config.City, agentIdentity string) string {
	if cfg == nil {
		return ""
	}
	rig, _, ok := strings.Cut(strings.TrimSpace(agentIdentity), "/")
	if !ok || rig == "" {
		return ""
	}
	for i := range cfg.Rigs {
		if strings.TrimSpace(cfg.Rigs[i].Name) == rig {
			return rig
		}
	}
	return ""
}

// firstStoreWithWork runs command against each store in order and returns the
// output and store of the FIRST store that reports ready work (applying the same
// normalize + unready-filter that doHook uses, so a store with only
// deferred/blocked rows is not treated as a hit). run is injectable for tests.
//
// When no store has ready work, an error on the agent's OWN store (the first
// entry) is surfaced so emitCityWorkQueryFailure can classify it — preserving
// the single-store emit-on-timeout contract (a work-query timeout must reach
// the reconciler, not be silently downgraded to "no work"). Errors from
// federated rig stores are best-effort discovery (like appendRigHookStores)
// and are not surfaced, so one flaky rig store can't wedge the hook.
func firstStoreWithWork(command string, stores []hookStore, run func(command, dir string, env []string) (string, error)) (string, hookStore, error) {
	var lastOut string
	var ownStoreOut string
	var ownStoreErr error
	for i, st := range stores {
		out, err := run(command, st.dir, st.env)
		if err == nil {
			ready := filterUnreadyHookCandidates(normalizeWorkQueryOutput(strings.TrimSpace(out)), time.Now())
			if workQueryHasReadyWork(ready) {
				return out, st, nil
			}
			lastOut = out
			continue
		}
		if i == 0 {
			ownStoreOut, ownStoreErr = out, err
		}
	}
	if ownStoreErr != nil {
		return ownStoreOut, hookStore{}, ownStoreErr
	}
	return lastOut, hookStore{}, nil
}

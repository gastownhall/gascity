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
		rigName := strings.TrimSpace(cfg.Rigs[i].Name)
		if rigName == "" {
			continue
		}
		view := *a
		view.Dir = rigName
		rigEnv, err := hookQueryEnv(cityPath, cfg, &view)
		if err != nil || rigEnv == nil {
			continue
		}
		for _, k := range hookIdentityEnvKeys {
			if v, ok := identityOverrides[k]; ok {
				rigEnv[k] = v
			}
		}
		stores = append(stores, hookStore{
			dir: agentCommandDir(cityPath, &view, cfg.Rigs),
			env: mergeRuntimeEnv(os.Environ(), rigEnv),
		})
	}
	return stores
}

// firstStoreWithWork runs command against each store in order and returns the
// output of the FIRST store that reports ready work (applying the same
// normalize + unready-filter that doHook uses, so a store with only
// deferred/blocked rows is not treated as a hit). run is injectable for tests.
//
// When no store has ready work, an error on the agent's OWN store (the first
// entry) is surfaced so emitCityWorkQueryFailure can classify it — preserving
// the single-store emit-on-timeout contract (a work-query timeout must reach
// the reconciler, not be silently downgraded to "no work"). Errors from
// federated rig stores are best-effort discovery (like appendRigHookStores)
// and are not surfaced, so one flaky rig store can't wedge the hook.
func firstStoreWithWork(command string, stores []hookStore, run func(command, dir string, env []string) (string, error)) (string, error) {
	var lastOut string
	var ownStoreOut string
	var ownStoreErr error
	for i, st := range stores {
		out, err := run(command, st.dir, st.env)
		if err == nil {
			ready := filterUnreadyHookCandidates(normalizeWorkQueryOutput(strings.TrimSpace(out)), time.Now())
			if workQueryHasReadyWork(ready) {
				return out, nil
			}
			lastOut = out
			continue
		}
		if i == 0 {
			ownStoreOut, ownStoreErr = out, err
		}
	}
	if ownStoreErr != nil {
		return ownStoreOut, ownStoreErr
	}
	return lastOut, nil
}

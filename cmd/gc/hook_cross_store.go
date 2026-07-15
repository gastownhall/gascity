package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// hookStore is one store the hook work_query runs against: a working dir and
// the rig/city-scoped subprocess env that points bd at that store.
type hookStore struct {
	dir string
	env []string
}

// hookStoreRunner runs a work query against one federated store's dir and env.
// Injectable so the cross-store selection and claim paths can be tested without
// a real bd subprocess.
type hookStoreRunner func(command, dir string, env []string) (string, error)

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
// When no store has ready work, an error on the agent's OWN store (identified by
// primary, not by slice position) is surfaced so emitCityWorkQueryFailure can
// classify it — preserving the single-store emit-on-timeout contract (a
// work-query timeout must reach the reconciler, not be silently downgraded to
// "no work"). Errors from federated rig stores are best-effort discovery (like
// appendRigHookStores) and are not surfaced, so one flaky rig store can't wedge
// the hook. primary is matched by identity rather than position because the
// federated claim loop reselects over a shrinking store set: once the primary
// store has been dropped it is no longer in stores, so no later federated store
// may inherit its emit-on-timeout semantics.
func firstStoreWithWork(command string, stores []hookStore, primary hookStore, run hookStoreRunner) (string, hookStore, error) {
	var lastOut string
	var ownStoreOut string
	var ownStoreErr error
	for _, st := range stores {
		out, err := run(command, st.dir, st.env)
		if err == nil {
			ready := filterUnreadyHookCandidates(normalizeWorkQueryOutput(strings.TrimSpace(out)), time.Now())
			if workQueryHasReadyWork(ready) {
				return out, st, nil
			}
			lastOut = out
			continue
		}
		if sameHookStore(st, primary) {
			ownStoreOut, ownStoreErr = out, err
		}
	}
	if ownStoreErr != nil {
		return ownStoreOut, hookStore{}, ownStoreErr
	}
	return lastOut, hookStore{}, nil
}

type hookClaimCandidateRank struct {
	tier int
	bead beads.Bead
}

func bestClaimStoreWithWork(command string, stores []hookStore, primary hookStore, opts hookClaimOptions, requiredFreshPriority *int, run hookStoreRunner) (string, hookStore, hookClaimCandidateRank, error) {
	return bestClaimStoreWithSeed(command, stores, primary, opts, requiredFreshPriority, hookStore{}, "", false, run)
}

func bestClaimStoreWithSeed(command string, stores []hookStore, primary hookStore, opts hookClaimOptions, requiredFreshPriority *int, seedStore hookStore, seedOutput string, hasSeed bool, run hookStoreRunner) (string, hookStore, hookClaimCandidateRank, error) {
	var bestOut string
	var bestStore hookStore
	var bestRank hookClaimCandidateRank
	foundBest := false
	var primaryOut string
	var primaryErr error
	for _, store := range stores {
		out := seedOutput
		var err error
		if !hasSeed || !sameHookStore(store, seedStore) {
			out, err = run(command, store.dir, store.env)
		}
		if err != nil {
			if sameHookStore(store, primary) {
				primaryOut, primaryErr = out, err
			}
			continue
		}
		rank, ok, err := bestHookClaimCandidate(out, opts, requiredFreshPriority)
		if err != nil {
			if sameHookStore(store, primary) {
				primaryOut = out
				primaryErr = fmt.Errorf("decoding claim candidates: %w", err)
			}
			continue
		}
		if !ok || (foundBest && !hookClaimRankLess(rank, bestRank)) {
			continue
		}
		bestOut, bestStore, bestRank = out, store, rank
		foundBest = true
	}
	if foundBest {
		return bestOut, bestStore, bestRank, nil
	}
	if primaryErr != nil {
		return primaryOut, hookStore{}, hookClaimCandidateRank{}, primaryErr
	}
	return "", hookStore{}, hookClaimCandidateRank{}, nil
}

func bestHookClaimCandidate(output string, opts hookClaimOptions, requiredFreshPriority *int) (hookClaimCandidateRank, bool, error) {
	normalized := filterUnreadyHookCandidates(normalizeWorkQueryOutput(strings.TrimSpace(output)), time.Now())
	if !workQueryHasReadyWork(normalized) {
		return hookClaimCandidateRank{}, false, nil
	}
	candidates, err := decodeHookClaimBeads(normalized)
	if err != nil {
		return hookClaimCandidateRank{}, false, err
	}
	var best hookClaimCandidateRank
	found := false
	for _, candidate := range candidates {
		rank, ok := hookClaimRank(candidate, opts, requiredFreshPriority)
		if !ok || (found && !hookClaimRankLess(rank, best)) {
			continue
		}
		best, found = rank, true
	}
	return best, found, nil
}

func hookClaimRank(candidate beads.Bead, opts hookClaimOptions, requiredFreshPriority *int) (hookClaimCandidateRank, bool) {
	status := strings.ToLower(strings.TrimSpace(candidate.Status))
	switch {
	case status == "in_progress" && hookClaimHasIdentity(candidate.Assignee, opts.IdentityCandidates):
		return hookClaimCandidateRank{tier: 0, bead: candidate}, true
	case status == "open" && hookClaimHasIdentity(candidate.Assignee, opts.IdentityCandidates):
		return hookClaimCandidateRank{tier: 1, bead: candidate}, true
	case hookCandidateClaimable(candidate, opts.RouteTargets):
		priority := beads.PriorityValue(candidate.Priority)
		if requiredFreshPriority != nil && priority != *requiredFreshPriority {
			return hookClaimCandidateRank{}, false
		}
		return hookClaimCandidateRank{tier: 2, bead: candidate}, true
	default:
		return hookClaimCandidateRank{}, false
	}
}

func hookClaimRankLess(left, right hookClaimCandidateRank) bool {
	if left.tier != right.tier {
		return left.tier < right.tier
	}
	return beads.ReadyLess(left.bead, right.bead)
}

// isZeroHookStore reports whether s is the zero hookStore that firstStoreWithWork
// returns when no store has ready work (no dir and no env).
func isZeroHookStore(s hookStore) bool {
	return strings.TrimSpace(s.dir) == "" && len(s.env) == 0
}

// removeHookStore returns stores with the first entry equal to target removed.
// The federated claim loop uses it to drop a store whose ready work was lost to
// another claimant before reselecting across the remaining stores, which also
// guarantees the loop makes progress (the working set strictly shrinks).
func removeHookStore(stores []hookStore, target hookStore) []hookStore {
	out := make([]hookStore, 0, len(stores))
	removed := false
	for _, s := range stores {
		if !removed && sameHookStore(s, target) {
			removed = true
			continue
		}
		out = append(out, s)
	}
	return out
}

// sameHookStore reports whether two stores address the same dir and env, so the
// federated claim loop can drop the exact store it just exhausted.
func sameHookStore(a, b hookStore) bool {
	if a.dir != b.dir || len(a.env) != len(b.env) {
		return false
	}
	for i := range a.env {
		if a.env[i] != b.env[i] {
			return false
		}
	}
	return true
}

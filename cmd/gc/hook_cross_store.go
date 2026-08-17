package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// hookStore is one store the hook work_query runs against: a working dir and
// the rig/city-scoped subprocess env that points bd at that store.
type hookStore struct {
	dir string
	env []string
	// command overrides the shared work query for this store, and is empty on
	// every store production builds: one command is run against each leg in
	// turn. It was set by scopeFederatedHookStores when that pinned the
	// city-wide reader to the primary and left the extras on the single-store
	// command; the extras are dropped now, so nothing sets it. Retired with the
	// rest of that mechanism in ga-gu4xg.
	command string
}

// hookStoreCommand returns the work query st actually runs: its own command when
// it carries one, else the caller's shared command.
func hookStoreCommand(st hookStore, command string) string {
	if own := strings.TrimSpace(st.command); own != "" {
		return own
	}
	return command
}

// scopeFederatedHookStores collapses a city-wide work query onto ONE leg.
//
// # Leg relevance, taken from the plan rather than from the store list
//
// The hook's legs are bd WORKSPACES, and it fans out across them because
// `bd ready` reads exactly one store. On a federated city that premise is gone:
// the primary leg runs `gc ready`, whose own legs are Plan(RoutedWork) — the
// city work store, the rigs ascending, the relocated binding last — so its
// answer is a SUPERSET of every extra leg's, tier by tier. An extra can
// therefore never contribute a candidate the primary did not already return; it
// can only re-open every store and pay the federated legs again.
//
// So the relevant leg set for a federated claim read is exactly one leg, and
// that is what this returns. On mc's topology it takes a hook tick from six
// full work-query runs (one federated, five single-store) to one, and the
// dropped five were each a strictly narrower view of the same city.
//
// # Why the extras are no longer coverage
//
// They used to be: the crash-recovery tier ran `bd list --status in_progress`
// and the wisp probes ran `bd query`, neither of which the city-wide reader
// answered, so collapsing the loop would have stripped rig coverage from crash
// recovery. Both are closed now. The crash-recovery tier swapped to `gc ready
// --status in_progress`, and every federated leg is read at
// beads.FederatedReadTier, which spans the wisp tier — so the ephemeral rows a
// per-store `bd query` used to find are in the federated answer.
// TestTheFederatedReaderAnswersEveryTierTheExtrasUsedTo is the guard: if either
// tier regresses to a single-store read, the extras have to come back.
//
// # The signal
//
// federatedCommand and singleStoreCommand are the two forms of the same agent's
// query. They are EQUAL for a custom (verbatim) work_query and on a city that
// relocates nothing, and the call is then a no-op returning stores unchanged.
// That is not an optimization detail: a custom work_query reads one store
// whatever the topology, so the fan-out is its only coverage and must survive.
func scopeFederatedHookStores(stores []hookStore, federatedCommand, singleStoreCommand string) []hookStore {
	singleStoreCommand = strings.TrimSpace(singleStoreCommand)
	if len(stores) < 2 || singleStoreCommand == "" || singleStoreCommand == strings.TrimSpace(federatedCommand) {
		return stores
	}
	return stores[:1:1]
}

// hookStoreRunner runs a work query against one federated store's dir and env.
// Injectable so the cross-store selection and claim paths can be tested without
// a real bd subprocess.
type hookStoreRunner func(command, dir string, env []string) (string, error)

// hookWorkQueryStores is the whole fan-out `gc hook` queries, in probe order.
//
// A cross-store-eligible (city-scoped) agent federates its work query across all
// stores — its own first, then every rig store — matched on its own identity
// (vp-kvp stage iii). A rig-scoped agent ("<rig>/<name>") instead queries its own
// <rig> store FIRST: its routed work lives there, but its city-scoped
// work-query env does not reach it, so without this the hook returns empty and
// the spawned session exits with nothing to do. The rig store goes first (as the
// primary entry, not a best-effort federated extra) so a rig-store work-query
// timeout still surfaces to the reconciler via bestStoreWithWork's
// emit-on-timeout contract — the agent's (work-less) city-scoped env stays as a
// best-effort secondary. This extends the #2877 city-scoped cross-store delivery
// to rig-scoped agents.
//
// Every leg is a bd WORKSPACE: a directory plus the env that points bd at it.
// That is the shape of the I5 known gap — a relocated coordination class is not
// a bd workspace, so no leg of this list can reach the binding on its own. The
// federated `gc ready` reader (ga-bvdha) now runs AS the primary leg's command
// and covers the binding in-process, so the hook does see class ids; what it
// still cannot do is claim one, because the claim is a bd subprocess rooted in
// this leg's workspace. This function is the seam that pins it
// (conformanceClaimRouting); closing the gap makes the claim stop being a bd
// subprocess call, and I15 pins the see-but-cannot-claim asymmetry until it does.
func hookWorkQueryStores(cityPath string, cfg *config.City, a *config.Agent, agentForQuery, workDir string, queryEnv []string, identityOverrides map[string]string) []hookStore {
	stores := []hookStore{{dir: workDir, env: queryEnv}}
	if agentIsCrossStoreEligible(a) {
		return appendRigHookStores(stores, cityPath, cfg, a, identityOverrides)
	}
	rig := rigScopedHookRig(cfg, agentForQuery)
	if rig == "" {
		return stores
	}
	if rigStores := appendOneRigHookStore(nil, cityPath, cfg, a, rig, identityOverrides); len(rigStores) > 0 {
		stores = append(rigStores, stores...)
	}
	// A rig-backed agent's own env above is ALSO rig-scoped, so without this no
	// entry reaches the CITY store and root-only beads assigned to the agent
	// stay invisible. Best-effort tertiary; see appendCityHookStore.
	return appendCityHookStore(stores, cityPath, cfg, a, identityOverrides)
}

// hookIdentityEnvKeys are the identity overrides that must stay constant across
// every federated store attempt — the query always matches the agent's OWN
// identity (gc.routed_to / assignee == this identity) regardless of which store
// it reads.
var hookIdentityEnvKeys = []string{
	"GC_AGENT", "GC_SESSION_NAME", "GC_ALIAS",
	"GC_SESSION_ID", "GC_SESSION_ORIGIN", "GC_TEMPLATE",
}

// appendRigHookStores adds one hookStore per non-suspended rig for a
// cross-store-eligible (city-scoped) agent — vp-kvp stage iii read
// federation. Each entry reuses the rig's store env (built the same way
// controller probes build it, via a per-rig agent view) while keeping the
// city agent's identity overrides, so the query reads the RIG store but
// still matches work routed/assigned to the city agent. Best-effort: a rig
// whose env cannot be built is skipped (the agent's own store is always
// queried first by the caller). A suspended rig is excluded outright via
// buildSuspendedRigPathsForCity, the same helper buildDesiredStateWithSessionBeads
// uses to build activeStores for scale_check — reusing it here keeps
// work_query and scale_check agreeing on which stores exist for a
// city-scoped agent (ga-drb140 AC3).
func appendRigHookStores(stores []hookStore, cityPath string, cfg *config.City, a *config.Agent, identityOverrides map[string]string) []hookStore {
	if cfg == nil || a == nil {
		return stores
	}
	suspendedRigPaths := buildSuspendedRigPathsForCity(cfg, cityPath)
	for i := range cfg.Rigs {
		if suspendedRigPaths[filepath.Clean(cfg.Rigs[i].Path)] {
			continue
		}
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
// entry is appended LAST so the rig store keeps bestStoreWithWork's
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

// bestStoreWithWork runs command against each store and returns the output and
// store holding the work the agent should pick up NEXT — the best-ranked ready
// candidate across every federated store, not merely the first store that has
// anything (applying the same normalize + unready-filter that doHook uses, so a
// store with only deferred/blocked rows is not a hit). run is injectable for
// tests.
//
// Why ranking rather than first-hit: the store list puts a city-scoped agent's
// OWN store ahead of the federated rig stores, so returning the first hit meant
// a rig-routed P0 stayed invisible behind a city P2 for as long as the city
// store had anything at all. Priority was never compared across the store
// boundary, so priority could not rescue it — for a crew agent that always has
// some city work, rig-routed beads were not deprioritized but unreachable.
// Ranking compares candidates the way the three-tier work_query intends them to
// be consumed (see hookCandidateRank), which makes the ordering hold ACROSS
// stores as well as within one. This changes only WHICH already-matching store
// is selected: every store's query still matches this agent's own identity, so
// no bead becomes visible that was not already routed or assigned to it.
//
// An exact tie rotates across every store tied for the best rank, using
// hookTieBreakClock rather than always keeping the first-seen store. Always
// preferring slice order (in practice, the agent's own store, since it is
// stores[0]) meant a tie that persisted across calls — the common case for a
// steady stream of same-tier, same-priority work — starved every other tied
// store forever: the comparison is deterministic and re-runs identically on
// every hook invocation (ga-kbbg9a). Rotating only changes WHICH tied store
// wins; a candidate that is not tied for the best rank is never affected.
//
// Two deliberate carve-outs from ranking:
//   - A tier-0 (in_progress) candidate in the PRIMARY store short-circuits.
//     Resuming this session's own interrupted work is unconditional, nothing may
//     preempt it, and short-circuiting keeps the hot resume path at one query.
//   - If any hit's output cannot be ranked (non-JSON, or JSON that is not an
//     array of objects), selection degrades to first-hit for the whole call.
//     Reordering on a comparison we could not actually make would be worse than
//     the behavior it replaces.
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
func bestStoreWithWork(command string, stores []hookStore, primary hookStore, run hookStoreRunner) (string, hookStore, error) {
	var lastOut string
	var ownStoreOut string
	var ownStoreErr error

	var firstHitOut string
	var firstHitStore hookStore
	firstHit := false
	unrankable := false

	var bestRank hookCandidateRank
	var bestTied []hookRankedStore
	haveBest := false

	// When the federated reader is pinned to the primary leg, that leg is the
	// only one whose answer covers the whole city; the extras run the
	// single-store form and are structurally blind to the relocated binding. So
	// a primary error is not one leg's bad luck — it is the loss of the
	// federation itself, and the extras must not answer for it. See the
	// federatedPrimaryFailed handling below.
	federationPinnedToPrimary := hookFederationPinnedToPrimary(stores, primary)
	federatedPrimaryFailed := false

	now := time.Now()
	for _, st := range stores {
		out, err := run(hookStoreCommand(st, command), st.dir, st.env)
		if err != nil {
			if sameHookStore(st, primary) {
				ownStoreOut, ownStoreErr = out, err
				federatedPrimaryFailed = federationPinnedToPrimary
			}
			continue
		}
		if federatedPrimaryFailed {
			// Everything after a failed federated primary is a partial view. The
			// ONE exception is this session's own in-progress work: a resume row
			// is already claimed by this identity, so no federated view could
			// overturn it, and refusing it would regress crash recovery whenever
			// the primary is down. Take it and stop; discard every other answer.
			if resumeOut, ok := hookOwnResumeAnswer(out, now); ok {
				return resumeOut, st, nil
			}
			continue
		}
		ready := filterUnreadyHookCandidates(normalizeWorkQueryOutput(strings.TrimSpace(out)), now)
		if !workQueryHasReadyWork(ready) {
			lastOut = out
			continue
		}
		if !firstHit {
			firstHitOut, firstHitStore, firstHit = out, st, true
		}
		rank, id, ok := bestHookCandidateRank(ready)
		if !ok {
			unrankable = true
			continue
		}
		// Resuming this session's own in-progress work is unconditional.
		if rank.tier == hookTierInProgress && sameHookStore(st, primary) {
			return out, st, nil
		}
		switch {
		case !haveBest || rank.less(bestRank):
			bestRank, haveBest = rank, true
			bestTied = append(bestTied[:0], hookRankedStore{out: out, store: st, id: id})
		case rank == bestRank && !hookTiedIDSeen(bestTied, id):
			// A migrated bead (`gc storage migrate` copies, never deletes) can be
			// ready from more than one store under the SAME id: the same row, not
			// two tied pieces of work. Rotating across a duplicate would put a
			// later store ahead of the primary for no reason — nothing about the
			// work differs — and breaks the rig-first-city-last fan-out order the
			// class-escalation claim loop depends on. An empty id (unidentifiable
			// row) is never treated as a duplicate, so unranked-id fixtures keep
			// rotating exactly as before.
			bestTied = append(bestTied, hookRankedStore{out: out, store: st, id: id})
		}
	}

	// A failed federated primary is terminal for the invocation: the surviving
	// legs' single-store answers are a partial view of the city, and the worst of
	// them — a nil-error empty — would be written out as a no_work drain-ack,
	// reaping a seat whose demand still exists. A visible failure is better than
	// a confident wrong answer, and the caller's bounded retry gets first refusal
	// on it.
	if federatedPrimaryFailed {
		return ownStoreOut, hookStore{}, ownStoreErr
	}
	if unrankable && firstHit {
		return firstHitOut, firstHitStore, nil
	}
	if haveBest {
		winner := bestTied[0]
		if len(bestTied) > 1 {
			winner = bestTied[hookTieBreakIndex(len(bestTied), hookTieBreakClock())]
		}
		return winner.out, winner.store, nil
	}
	if firstHit {
		return firstHitOut, firstHitStore, nil
	}
	if ownStoreErr != nil {
		return ownStoreOut, hookStore{}, ownStoreErr
	}
	return lastOut, hookStore{}, nil
}

// hookRankedStore pairs one store's work-query output with the store it came
// from and the winning candidate's id. bestStoreWithWork accumulates one per
// store tied for the best rank, held only long enough to resolve the tie.
type hookRankedStore struct {
	out   string
	store hookStore
	id    string
}

// hookTiedIDSeen reports whether id already has an entry in tied. An empty id
// (a row bestHookCandidateRank could not read an "id" field from) never
// counts as seen, so unidentifiable rows fall back to the pre-existing
// rotate-every-entry behavior rather than being silently collapsed.
func hookTiedIDSeen(tied []hookRankedStore, id string) bool {
	if id == "" {
		return false
	}
	for _, t := range tied {
		if t.id == id {
			return true
		}
	}
	return false
}

// hookTieBreakClock returns the time bestStoreWithWork uses to rotate an
// exact-rank tie across stores (see its doc comment). A package-level var so
// tests can pin it; production always uses the real clock.
var hookTieBreakClock = time.Now

// hookTieBreakIndex picks which of n exact-rank-tied stores wins, rotating
// with t so the same tie does not resolve to the same store on every call. A
// fresh process (gc hook's normal invocation shape) has no in-memory state to
// round-robin with, so the rotation source has to be something that varies
// between invocations on its own — wall-clock time — rather than a counter.
// n must be > 0.
func hookTieBreakIndex(n int, t time.Time) int {
	return int(uint64(t.UnixNano()) % uint64(n))
}

// hookFederationPinnedToPrimary reports whether this store set is the shape
// scopeFederatedHookStores USED to produce: the primary runs the city-wide
// federated command and every extra carries its own single-store override. That
// is exactly when a primary failure costs the federation rather than one leg, so
// it is read off the store set rather than passed down as a flag.
//
// NO PRODUCTION CALLER PRODUCES THAT SHAPE ANY MORE. scopeFederatedHookStores
// now drops the extras outright, so a federated fan-out is one leg and this
// answers false — the single leg's failure is surfaced by bestStoreWithWork's
// own ownStoreErr path instead, with the same outcome. The mechanism is left
// standing rather than deleted here because removing it also removes
// bestStoreWithWork's federated-primary-failure semantics and their tests, which
// is a provable deletion of its own and does not belong in the same commit as a
// census leg-set change (ga-gu4xg).
func hookFederationPinnedToPrimary(stores []hookStore, primary hookStore) bool {
	if len(stores) < 2 {
		return false
	}
	for _, st := range stores {
		if sameHookStore(st, primary) {
			continue
		}
		if strings.TrimSpace(st.command) != "" {
			return true
		}
	}
	return false
}

// hookOwnResumeAnswer returns out when it carries this session's own in-progress
// work — the crash-recovery resume tier. Every query in the fan-out matches this
// agent's own identity (see the tier constants), so an in_progress row in any leg
// is a row this session already owns; nothing a wider read could say would change
// that. It is the only answer trusted from a leg that is blind to the federation.
func hookOwnResumeAnswer(out string, now time.Time) (string, bool) {
	ready := filterUnreadyHookCandidates(normalizeWorkQueryOutput(strings.TrimSpace(out)), now)
	if !workQueryHasReadyWork(ready) {
		return "", false
	}
	rank, _, ok := bestHookCandidateRank(ready)
	if !ok || rank.tier != hookTierInProgress {
		return "", false
	}
	return out, true
}

// Work-query tiers, ordered most-urgent first. They mirror the three tiers of
// the default work_query (config.Agent.WorkQuery): in_progress work assigned to
// this session (crash recovery), then ready work already assigned to it, then
// ready unassigned work routed to it. Every store's query matches this agent's
// own identity, so a row carrying an assignee is assigned to THIS agent and an
// unassigned row is a routed one — which is what lets the tier be read off the
// row without re-plumbing the caller's identity down here.
const (
	hookTierInProgress = iota
	hookTierAssigned
	hookTierRouted
)

// hookDefaultCandidatePriority is the priority assumed for a row whose priority
// field is absent. bd's priority is a *int and omitempty, so "no priority" and
// "priority 0" are different states on the wire; treating an absent priority as
// P0 would let a field that simply was not serialized outrank a real P0.
const hookDefaultCandidatePriority = 2

// hookCandidateRank orders one ready work-query candidate: lower is more urgent,
// tier first and priority second. Comparing this across stores is the whole
// point — within a single store bd already applies the same ordering.
type hookCandidateRank struct {
	tier     int
	priority int
}

// less reports whether r should be picked ahead of other. Equal ranks report
// false in both directions: less alone never breaks a tie, so each caller
// decides how to. bestHookCandidateRank (below) only replaces its incumbent
// on a strict improvement, so it keeps the first row seen. bestStoreWithWork
// instead accumulates every candidate tied for the best rank and rotates
// among them (see its doc comment) — the tie itself is the same regardless of
// caller; only what happens with it differs.
func (r hookCandidateRank) less(other hookCandidateRank) bool {
	if r.tier != other.tier {
		return r.tier < other.tier
	}
	return r.priority < other.priority
}

// bestHookCandidateRank returns the rank of the most urgent candidate in one
// store's ready work-query output. ok is false when the output cannot be ranked
// — not JSON, not a JSON array, or an array holding a non-object — which the
// caller treats as "do not reorder on a comparison that was not made" rather
// than as an absence of work.
func bestHookCandidateRank(ready string) (hookCandidateRank, string, bool) {
	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(ready)), &rows); err != nil {
		return hookCandidateRank{}, "", false
	}
	best := hookCandidateRank{}
	bestID := ""
	found := false
	for _, row := range rows {
		rank := hookRankCandidate(row)
		if !found || rank.less(best) {
			best, found = rank, true
			bestID, _ = row["id"].(string)
		}
	}
	return best, bestID, found
}

// hookRankCandidate reads one row's tier and priority. An unrecognized status or
// a missing priority falls back to the least-urgent defensible reading, so a row
// gc cannot classify never preempts one it can.
func hookRankCandidate(row map[string]any) hookCandidateRank {
	rank := hookCandidateRank{tier: hookTierRouted, priority: hookDefaultCandidatePriority}
	if status, ok := row["status"].(string); ok && strings.EqualFold(strings.TrimSpace(status), "in_progress") {
		rank.tier = hookTierInProgress
	} else if assignee, ok := row["assignee"].(string); ok && strings.TrimSpace(assignee) != "" {
		rank.tier = hookTierAssigned
	}
	if p, ok := row["priority"].(float64); ok {
		rank.priority = int(p)
	}
	return rank
}

// claimStoreWithFallback re-validates the discovery-selected store for
// claim-time freshness, then falls back to federated re-selection across all
// stores when that store has emptied since discovery. It exists because
// gc hook --claim selects the first store with ready work, then must commit to
// one store for the claim mutation. Re-running only the selected store would
// drain as "no work" whenever its claimable row was taken between discovery and
// claim — even though a later federated store still has ready routed work. The
// returned output is the work-query result the claim should act on, paired with
// the store it came from so the mutation runs against that store's bd context.
// (The narrow window between this re-validation and the bd update --claim is
// still handled by the claim itself skipping rows it cannot take.)
//
// A re-validation error on the selected store is surfaced only when that store
// is the primary (own) store; a federated store erroring at claim time is
// best-effort and falls through to re-selection, mirroring bestStoreWithWork's
// emit-on-timeout contract so a flaky rig store can't wedge the claim.
//
// # One leg means nothing to fall back TO, so there is nothing to re-validate
//
// The whole reason to re-read is the LATER store: draining as "no work" when a
// federated peer still has ready routed work is the strand this exists to
// prevent. A single-leg fan-out — every federated city after S3, where the
// primary's reader answers for the whole plan — has no later store, so the
// second read can only pay another full city-wide query for a row the claim
// itself re-checks anyway (`bd update --claim` skips rows it cannot take). On
// mc's topology that read is the more expensive half of the claim.
//
// discovered is the output selection already read from this store, moments ago.
func claimStoreWithFallback(command string, stores []hookStore, selected, primary hookStore, discovered string, run hookStoreRunner) (string, hookStore, error) {
	if len(stores) == 1 {
		return discovered, selected, nil
	}
	selectedOut, err := run(hookStoreCommand(selected, command), selected.dir, selected.env)
	if err != nil {
		if sameHookStore(selected, primary) {
			return "", hookStore{}, err
		}
		return bestStoreWithWork(command, stores, primary, run)
	}
	ready := filterUnreadyHookCandidates(normalizeWorkQueryOutput(strings.TrimSpace(selectedOut)), time.Now())
	if workQueryHasReadyWork(ready) {
		return selectedOut, selected, nil
	}
	return bestStoreWithWork(command, stores, primary, run)
}

// hookClaimReadsPerTick counts the work-query runs one claim attempt performs
// over a given leg set: one to select, and one to re-validate unless there is
// nothing to fall back to. It exists so the cost this slice removes is asserted
// rather than described (TestFederatedClaimTickIssuesOneWorkQueryRun).
//
// It is a CEILING, not a measurement. A tier-0 hit in the primary store
// short-circuits bestStoreWithWork before the later legs run, and each RUN is
// itself a tier ladder that exits on its first hit — so a real tick reads this
// many times or fewer, never more. Do not quote it as an observed number.
func hookClaimReadsPerTick(stores []hookStore) int {
	if len(stores) == 1 {
		return 1
	}
	return len(stores) + 1
}

// isZeroHookStore reports whether s is the zero hookStore that bestStoreWithWork
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

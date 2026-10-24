package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/config"
)

// hookStore is one store the hook work_query runs against: a working dir and
// the rig/city-scoped subprocess env that points bd at that store.
type hookStore struct {
	dir string
	env []string
	// rig is the rig whose store this leg reads, "" for the agent's own store
	// and the city store. A failed read is reported under it.
	rig string
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
//
// # Callers
//
// Only tests call it. The hook takes the same decision before it builds the
// fan-out (hookStoresScopeToPrimary) and builds only the entry this keeps
// (hookWorkQueryPrimaryStore), so the legs this drops are never built;
// TestHookWorkQueryPrimaryStoreIsTheScopedStore pins that entry.
func scopeFederatedHookStores(stores []hookStore, federatedCommand, singleStoreCommand string) []hookStore {
	if len(stores) < 2 || !hookStoresScopeToPrimary(federatedCommand, singleStoreCommand) {
		return stores
	}
	return stores[:1:1]
}

// hookStoresScopeToPrimary reports whether a claim running federatedCommand
// reads only the primary entry of its store fan-out: the federated form of the
// agent's query differs from its single-store form. Decided before the fan-out
// is built, so only that entry need be (hookWorkQueryPrimaryStore).
func hookStoresScopeToPrimary(federatedCommand, singleStoreCommand string) bool {
	singleStoreCommand = strings.TrimSpace(singleStoreCommand)
	return singleStoreCommand != "" && singleStoreCommand != strings.TrimSpace(federatedCommand)
}

// hookStoreQueryEnv builds one fan-out leg's work-query environment, the
// agent's own leg included. A variable so tests can count the legs built.
var hookStoreQueryEnv = hookQueryEnv

// hookStoreRunner runs a work query against one federated store's dir and env.
// Injectable so the cross-store selection and claim paths can be tested without
// a real bd subprocess.
type hookStoreRunner func(command, dir string, env []string) (string, error)

// hookWorkQueryStores is the whole fan-out `gc hook` queries, in probe order.
//
// A cross-store-eligible (city-scoped) agent federates its work query across all
// stores — its own first, then every rig store that is not suspended
// (suspendedRigs, keyed by rig name) — matched on its own identity (vp-kvp
// stage iii). A rig-scoped agent ("<rig>/<name>") instead queries its own
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
func hookWorkQueryStores(cityPath string, cfg *config.City, a *config.Agent, agentForQuery, workDir string, queryEnv []string, identityOverrides map[string]string, suspendedRigs map[string]bool) []hookStore {
	return hookWorkQueryStoreFanOut(cityPath, cfg, a, agentForQuery, workDir, queryEnv, identityOverrides, suspendedRigs, false)
}

// hookWorkQueryPrimaryStore is the first entry of hookWorkQueryStores, built
// without building any other: what a federated claim reads (see
// scopeFederatedHookStores).
func hookWorkQueryPrimaryStore(cityPath string, cfg *config.City, a *config.Agent, agentForQuery, workDir string, queryEnv []string, identityOverrides map[string]string) hookStore {
	return hookWorkQueryStoreFanOut(cityPath, cfg, a, agentForQuery, workDir, queryEnv, identityOverrides, nil, true)[0]
}

// hookWorkQueryStoreFanOut is hookWorkQueryStores, stopping after the first
// entry when primaryOnly.
func hookWorkQueryStoreFanOut(cityPath string, cfg *config.City, a *config.Agent, agentForQuery, workDir string, queryEnv []string, identityOverrides map[string]string, suspendedRigs map[string]bool, primaryOnly bool) []hookStore {
	stores := []hookStore{{dir: workDir, env: queryEnv}}
	if agentIsCrossStoreEligible(a) {
		if primaryOnly {
			return stores
		}
		return appendRigHookStores(stores, cityPath, cfg, a, identityOverrides, suspendedRigs)
	}
	rig := rigScopedHookRig(cfg, agentForQuery)
	if rig == "" {
		return stores
	}
	if rigStores := appendOneRigHookStore(nil, cityPath, cfg, a, rig, identityOverrides); len(rigStores) > 0 {
		if primaryOnly {
			return rigStores
		}
		stores = append(rigStores, stores...)
	}
	if primaryOnly {
		return stores
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

// appendRigHookStores adds one hookStore per rig for a cross-store-eligible
// (city-scoped) agent — vp-kvp stage iii read federation. Each entry reuses the
// rig's store env (built the same way controller probes build it, via a per-rig
// agent view) while keeping the city agent's identity overrides, so the query
// reads the RIG store but still matches work routed/assigned to the city agent.
// Best-effort: a rig whose env cannot be built is skipped (the agent's own store
// is always queried first by the caller).
//
// A rig named in suspendedRigs is not searched, the frame `gc ready` reads
// (readyServingRigs) and the controller's census counts demand in: a suspended
// rig's agents claim nothing, the controller stops the Dolt it owns, and its
// work comes back when the rig is resumed. Searched anyway, its store would be
// one every city-scoped seat fails to read for as long as the rig stays
// suspended.
func appendRigHookStores(stores []hookStore, cityPath string, cfg *config.City, a *config.Agent, identityOverrides map[string]string, suspendedRigs map[string]bool) []hookStore {
	if cfg == nil || a == nil {
		return stores
	}
	for i := range cfg.Rigs {
		if suspendedRigs[cfg.Rigs[i].Name] {
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
	rigEnv, err := hookStoreQueryEnv(cityPath, cfg, &view)
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
		rig: rigName,
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
	cityEnv, err := hookStoreQueryEnv(cityPath, cfg, &view)
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
// Ranking is applied in two passes, grouped by store dir (see
// bestRankedCandidate): raw tier stays meaningful WITHIN a dir — more than one
// hookStore entry can share a dir when they are different query legs over the
// SAME backing store (e.g. the claim-time class-routing front door's assigned
// and routed legs), and there the assigned tier must still win deterministically
// or a relocated graph step flip-flops onto a consolation bead every other tick,
// recreating the re-serve/re-skip treadmill ga-601v2 closed. Only ACROSS
// distinct dirs does tier collapse so priority decides (crossStoreRank) — that
// cross-dir collapse is what ga-t922vm fixed. In real deployments every
// hookWorkQueryStores entry already has a distinct dir, so the two passes
// coincide with a flat cross-store comparison; the dir grouping only matters for
// same-dir multi-leg callers like the class-routing front door's test harness.
//
// # Failed reads
//
// A store whose read failed never contributes a candidate, and a store that did
// answer with work is served regardless — a failed read elsewhere is not a
// reason to withhold work this agent can see. What a failed read changes is the
// "no work" answer: with any store unread, "no store has work" is not known,
// and answering it anyway is how a seat drains as idle while its work sits in a
// store the hook could not reach.
//
// When no store has ready work, an error on the agent's OWN store (identified by
// primary, not by slice position) takes precedence and is surfaced as itself so
// emitCityWorkQueryFailure can classify it — preserving the single-store
// emit-on-timeout contract (a work-query timeout must reach the reconciler, not
// be silently downgraded to "no work"). Failing that, an error on a secondary
// store is surfaced as a *hookSecondaryStoreFailure naming every store that
// failed, so plain `gc hook` exits with its work-query-failed code and `gc hook
// --claim` reports the failed read (reportClaimWorkQueryFailure) without a
// drain result.
//
// One secondary failure does not hold the answer up: a rig leg whose store
// resolves to a managed Dolt runtime that is not running, which the runner
// marks errHookRigRuntimeUnavailable (hookRigLegRunner). Its store cannot
// answer until that server is back, the controller cannot count demand in it
// either, and holding the answer on it would keep every seat that searches the
// rig from draining for as long as the server is down. It is passed to noted
// and the answer is given without it.
//
// When secondary failures fail the query, each is recorded as its own
// work-query-failure event naming the store (hookStoreFailureReport), never as
// this session's own read failing, and none outranks the primary's own error.
// primary is matched by identity rather than position because the federated
// claim loop reselects over a shrinking store set: once the primary store has
// been dropped it is no longer in stores, so no later federated store may
// inherit its emit-on-timeout semantics.
//
// When work IS served, each secondary store that failed is passed to noted (when
// given) so the caller can report it; the answer is right, but a store was still
// unreadable and someone should hear about it.
func bestStoreWithWork(command string, stores []hookStore, primary hookStore, run hookStoreRunner, noted ...hookStoreFailureNote) (string, hookStore, error) {
	var lastOut string
	var ownStoreOut string
	var ownStoreErr error
	// secondaryFailures hold the "no work" answer up; downFailures (rigs whose
	// managed Dolt runtime is not running) do not.
	var secondaryFailures, downFailures []*hookStoreReadError
	note := func(failures []*hookStoreReadError) {
		for _, n := range noted {
			if n == nil {
				continue
			}
			for _, failure := range failures {
				n(failure)
			}
		}
	}
	served := func(out string, st hookStore) (string, hookStore, error) {
		note(secondaryFailures)
		note(downFailures)
		return out, st, nil
	}

	var firstHitOut string
	var firstHitStore hookStore
	firstHit := false
	unrankable := false

	var ranked []hookRankedCandidate

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
				continue
			}
			failure := &hookStoreReadError{dir: st.dir, rig: st.rig, err: err}
			if errors.Is(err, errHookRigRuntimeUnavailable) {
				downFailures = append(downFailures, failure)
			} else {
				secondaryFailures = append(secondaryFailures, failure)
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
				return served(resumeOut, st)
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
			return served(out, st)
		}
		// Collected raw (uncollapsed) so bestRankedCandidate can compare same-dir
		// candidates on full tier before collapsing across dirs. See its doc
		// comment and the package doc comment above.
		ranked = append(ranked, hookRankedCandidate{out: out, store: st, id: id, rank: rank})
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
		return served(firstHitOut, firstHitStore)
	}
	if winner, ok := bestRankedCandidate(ranked); ok {
		return served(winner.out, winner.store)
	}
	if firstHit {
		return served(firstHitOut, firstHitStore)
	}
	if ownStoreErr != nil {
		return ownStoreOut, hookStore{}, ownStoreErr
	}
	note(downFailures)
	// No output alongside the error: an empty array next to a failure still
	// reads as an empty queue to any caller that looks at stdout first.
	if len(secondaryFailures) > 0 {
		return "", hookStore{}, &hookSecondaryStoreFailure{failures: secondaryFailures}
	}
	return lastOut, hookStore{}, nil
}

// errHookRigRuntimeUnavailable marks a failed read of a secondary rig leg whose
// store resolves to a managed Dolt runtime that is not running
// (hookRigLegRunner). bestStoreWithWork reports such a read without letting it
// hold the answer up.
var errHookRigRuntimeUnavailable = errors.New("the rig's managed Dolt runtime is not running")

// hookRigRuntimeUnavailable reports whether rig's store resolves to a managed
// Dolt runtime that is not running (rigManagedRuntimeUnavailable). Only a rig
// whose leg env is the managed bd runtime env (controllerQueryRuntimeEnv) can
// be in that state. A variable so tests can stand in for the runtime.
var hookRigRuntimeUnavailable = func(cityPath string, cfg *config.City, rig string) bool {
	if cfg == nil {
		return false
	}
	rigRoot := rigRootForName(rig, cfg.Rigs)
	if rigRoot == "" || !scopeUsesManagedBdStoreContract(cityPath, rigRoot) {
		return false
	}
	return rigManagedRuntimeUnavailable(cityPath, cfg, rigRoot)
}

// hookRigLegRunner is run for the fan-out stores, with one addition: when a
// secondary rig leg's read fails and that rig's store resolves to a managed
// Dolt runtime that is not running (hookRigRuntimeUnavailable), the error is
// marked errHookRigRuntimeUnavailable. The runtime is asked only after a read
// failed, and once per rig per invocation, since the claim loop reads its
// stores more than once. The first store is the agent's own and is never
// marked: its failure is this session's read failing, whatever the cause.
func hookRigLegRunner(cityPath string, cfg *config.City, stores []hookStore, run hookStoreRunner) hookStoreRunner {
	if len(stores) < 2 {
		return run
	}
	secondary := stores[1:]
	down := make(map[string]bool, len(secondary))
	return func(command, dir string, env []string) (string, error) {
		out, err := run(command, dir, env)
		if err == nil {
			return out, nil
		}
		rig := hookLegRig(secondary, dir, env)
		if rig == "" {
			return out, err
		}
		unavailable, asked := down[rig]
		if !asked {
			unavailable = hookRigRuntimeUnavailable(cityPath, cfg, rig)
			down[rig] = unavailable
		}
		if !unavailable {
			return out, err
		}
		return out, fmt.Errorf("%w: %w", errHookRigRuntimeUnavailable, err)
	}
}

// hookLegRig returns the rig of the leg in stores that reads dir with env, or
// "" when no rig leg does.
func hookLegRig(stores []hookStore, dir string, env []string) string {
	leg := hookStore{dir: dir, env: env}
	for _, st := range stores {
		if st.rig != "" && sameHookStore(st, leg) {
			return st.rig
		}
	}
	return ""
}

// hookStoreReadError is one store's failed work-query read, named by its rig
// and store directory so an operator can tell WHICH leg of the fan-out could
// not be read. Its message is bounded (workQueryFailureSummary); err keeps the
// whole failure for errors.Is and for the store-unavailable classification.
type hookStoreReadError struct {
	dir string
	rig string
	err error
}

func (e *hookStoreReadError) Error() string {
	store := "store " + e.dir
	if e.rig != "" {
		store = fmt.Sprintf("rig %q store %s", e.rig, e.dir)
	}
	return fmt.Sprintf("work query against %s failed: %s", store, workQueryFailureSummary(e.err))
}

func (e *hookStoreReadError) Unwrap() error { return e.err }

// hookStoreFailureSummaryMax caps workQueryFailureSummary, in bytes.
const hookStoreFailureSummaryMax = 200

// workQueryFailureSummary is the one-line form of a failed read that a fan-out
// report quotes: how the read ended and the first line the query wrote to
// stderr. The command line shellWorkQueryWithEnv quotes into its error after
// workQueryErrorPrefix is cut out: a generated work query runs to kilobytes,
// and a report repeats it for every store that failed.
func workQueryFailureSummary(err error) string {
	msg := strings.TrimSpace(err.Error())
	if i := strings.Index(msg, workQueryErrorPrefix+" "); i >= 0 {
		rest := msg[i+len(workQueryErrorPrefix)+1:]
		if quoted, qerr := strconv.QuotedPrefix(rest); qerr == nil {
			msg = msg[:i] + strings.TrimPrefix(rest[len(quoted):], ": ")
		}
	}
	if line, _, found := strings.Cut(msg, "\n"); found {
		msg = strings.TrimSpace(line)
	}
	if len(msg) <= hookStoreFailureSummaryMax {
		return msg
	}
	end := hookStoreFailureSummaryMax
	for end > 0 && !utf8.RuneStart(msg[end]) {
		end--
	}
	return msg[:end] + "…"
}

// hookStoreFailureNote receives a secondary store's failed read that the
// answer did not stop on: work was served from another store, or the store's
// rig runtime is down (errHookRigRuntimeUnavailable).
type hookStoreFailureNote func(*hookStoreReadError)

// hookSecondaryStoreFailure is bestStoreWithWork's error when no store had work
// and only secondary stores failed: "no work" could not be established. It is
// a distinct type so the caller reports it as a failed query without treating
// it as the agent's own store failing (hookStoreFailureReport), and so the
// store-unavailable classification reads each store's whole failure rather
// than the bounded message (classifyWorkQueryStoreUnavailable).
type hookSecondaryStoreFailure struct {
	failures []*hookStoreReadError
}

func (e *hookSecondaryStoreFailure) Error() string {
	msgs := make([]string, len(e.failures))
	for i, failure := range e.failures {
		msgs[i] = failure.Error()
	}
	return strings.Join(msgs, "; ")
}

func (e *hookSecondaryStoreFailure) Unwrap() []error {
	errs := make([]error, len(e.failures))
	for i, failure := range e.failures {
		errs[i] = failure
	}
	return errs
}

// hookStoreFailureReport reports the failed reads of one hook invocation. The
// claim loop reads its stores more than once (paced retries, the claim-time
// re-validation, a reselection after a lost race), and one unreadable store is
// one fact, so each store is reported once per invocation.
//
// A failed query is recorded through emit, the work-query-failure event: the
// agent's own store's failure as itself, and a fan-out that failed on
// secondary stores (hookSecondaryStoreFailure) as one event per failed store,
// naming it (classifyWorkQueryFailure). A secondary failure the answer did not
// stop on (work served from another store, or a rig whose managed Dolt runtime
// is not running) is not a failed query: it is a line on stderr.
type hookStoreFailureReport struct {
	label   string
	stderr  io.Writer
	emit    func(command string, err error)
	logged  map[string]bool
	emitted map[string]bool
}

func newHookStoreFailureReport(label string, stderr io.Writer, emit func(command string, err error)) *hookStoreFailureReport {
	return &hookStoreFailureReport{
		label:   label,
		stderr:  stderr,
		emit:    emit,
		logged:  map[string]bool{},
		emitted: map[string]bool{},
	}
}

// note is the hookStoreFailureNote bestStoreWithWork calls for a store whose
// failed read the answer did not stop on.
func (r *hookStoreFailureReport) note(failure *hookStoreReadError) {
	if r.logged[failure.dir] {
		return
	}
	r.logged[failure.dir] = true
	consequence := "serving work from the stores that answered"
	if errors.Is(failure.err, errHookRigRuntimeUnavailable) {
		consequence = "answering without that store"
	}
	fmt.Fprintf(r.stderr, "%s: %v; %s\n", r.label, failure, consequence) //nolint:errcheck // best-effort stderr
}

// failed records a work query that failed (err is the query's error, nil when
// it did not fail).
func (r *hookStoreFailureReport) failed(command string, err error) {
	if err == nil || r.emit == nil {
		return
	}
	var secondary *hookSecondaryStoreFailure
	if !errors.As(err, &secondary) {
		r.emit(command, err)
		return
	}
	for _, failure := range secondary.failures {
		if r.emitted[failure.dir] {
			continue
		}
		r.emitted[failure.dir] = true
		r.emit(command, failure)
	}
}

// hookRankedCandidate is one store's best candidate together with its RAW
// (uncollapsed) rank, gathered by bestStoreWithWork's per-store loop before
// bestRankedCandidate's dir-grouped reduction.
type hookRankedCandidate struct {
	out   string
	store hookStore
	id    string
	rank  hookCandidateRank
}

// bestRankedCandidate reduces every store's ranked candidate to the one
// bestStoreWithWork should return. See bestStoreWithWork's doc comment for
// why this is a two-pass reduction rather than a flat cross-store comparison:
// candidates are grouped by store dir first, reduced to one winner per dir
// using RAW (uncollapsed) rank — the same comparison bestHookCandidateRank
// already applies within one store's row array, because a shared dir means
// these ARE, in effect, rows of the same store — and only then compared
// ACROSS dirs via crossStoreRank, where an exact tie rotates using
// hookTieBreakClock rather than always keeping the first tied dir (ga-kbbg9a).
//
// Reports ok=false when ranked is empty (no store had a rankable candidate),
// letting the caller fall through to its first-hit/own-error/last-output
// fallbacks.
func bestRankedCandidate(ranked []hookRankedCandidate) (hookRankedStore, bool) {
	groups := make(map[string][]hookRankedCandidate, len(ranked))
	var dirOrder []string
	for _, c := range ranked {
		if _, seen := groups[c.store.dir]; !seen {
			dirOrder = append(dirOrder, c.store.dir)
		}
		groups[c.store.dir] = append(groups[c.store.dir], c)
	}

	var bestRank hookCandidateRank
	var bestTied []hookRankedStore
	haveBest := false
	for _, dir := range dirOrder {
		group := groups[dir]
		winner := group[0]
		for _, c := range group[1:] {
			if c.rank.less(winner.rank) {
				winner = c
			}
		}
		crossRank := winner.rank.crossStoreRank()
		switch {
		case !haveBest || crossRank.less(bestRank):
			bestRank, haveBest = crossRank, true
			bestTied = append(bestTied[:0], hookRankedStore{out: winner.out, store: winner.store, id: winner.id})
		case crossRank == bestRank && !hookTiedIDSeen(bestTied, winner.id):
			// A migrated bead (`gc storage migrate` copies, never deletes) can be
			// ready from more than one store under the SAME id: the same row, not
			// two tied pieces of work. Rotating across a duplicate would put a
			// later store ahead of the primary for no reason — nothing about the
			// work differs — and breaks the rig-first-city-last fan-out order the
			// class-escalation claim loop depends on. An empty id (unidentifiable
			// row) is never treated as a duplicate, so unranked-id fixtures keep
			// rotating exactly as before.
			bestTied = append(bestTied, hookRankedStore{out: winner.out, store: winner.store, id: winner.id})
		}
	}
	if !haveBest {
		return hookRankedStore{}, false
	}
	winner := bestTied[0]
	if len(bestTied) > 1 {
		winner = bestTied[hookTieBreakIndex(len(bestTied), hookTieBreakClock())]
	}
	return winner, true
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

// crossStoreRank collapses r for comparison AGAINST A DIFFERENT STORE. Tier
// alone is not comparable across stores: hookTierAssigned only means "this
// agent's identity is already on the row" — a bookkeeping fact of the store
// that happens to hold it — not that the work is more urgent than routed
// work sitting in another store. Left uncollapsed, an assigned-but-open row
// in the primary store permanently outranked every routed rig row on tier
// alone regardless of priority, and #5491's tie rotation never got a chance
// to fire because the two candidates were never at equal rank (ga-t922vm).
// hookTierInProgress is different: it is a live, unconditional resume claim
// (see the short-circuit in bestStoreWithWork below), so it passes through
// unchanged. Within a single store, tier stays fully meaningful — this is
// only for ranks being compared against a rank from another store.
func (r hookCandidateRank) crossStoreRank() hookCandidateRank {
	if r.tier == hookTierInProgress {
		return r
	}
	return hookCandidateRank{tier: hookTierRouted, priority: r.priority}
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
// A re-validation error on the selected store is returned as itself only when
// that store is the primary (own) store. A secondary store erroring at claim
// time falls through to re-selection, which reads every store again under
// bestStoreWithWork's rules: work in a store that answers is taken, and with no
// work anywhere that store's failed read is the answer.
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
//
// noted is forwarded to bestStoreWithWork for its failed-secondary reports.
func claimStoreWithFallback(command string, stores []hookStore, selected, primary hookStore, discovered string, run hookStoreRunner, noted ...hookStoreFailureNote) (string, hookStore, error) {
	if len(stores) == 1 {
		return discovered, selected, nil
	}
	selectedOut, err := run(hookStoreCommand(selected, command), selected.dir, selected.env)
	if err != nil {
		if sameHookStore(selected, primary) {
			return "", hookStore{}, err
		}
		return bestStoreWithWork(command, stores, primary, run, noted...)
	}
	ready := filterUnreadyHookCandidates(normalizeWorkQueryOutput(strings.TrimSpace(selectedOut)), time.Now())
	if workQueryHasReadyWork(ready) {
		return selectedOut, selected, nil
	}
	return bestStoreWithWork(command, stores, primary, run, noted...)
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

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// This file cuts the gc->bd read-storm documented on ga-ak6rt1: the
// control-dispatcher's per-tick readiness scan (workflowServeControlReadyQueryForBeads,
// dispatch_runtime.go) builds a shell script that fork-execs up to ~9
// bd/jq processes per agent per tick. Wire that same readiness evaluation to
// answer from an in-process CachingStore snapshot first, falling back to
// exactly one batched `bd ready --json` call when the snapshot can't answer,
// instead of the shell script's N separate `bd` invocations.
//
// Why this hooks into nextWorkflowServeBeads (the default workflowServeList
// implementation) rather than drainWorkflowServeWork: workflowServeList is a
// package var every existing serve-loop test overrides wholesale to fake the
// ready queue, so changing drainWorkflowServeWork's call site to bypass it
// for control-dispatcher agents would silently stop exercising ~25 existing
// tests' fakes. nextWorkflowServeBeads is never called directly by any
// existing test (they all replace workflowServeList outright), so extending
// its body here is additive: the exact query-string shape from
// workflowServeControlReadyQueryForBeads is unchanged (still asserted upon by
// TestWorkflowServeControlReadyQuery* tests), and any non-control-ready query
// -- or any failure standing up the cache -- falls straight through to the
// original shell exec, unchanged.

// controlReadyQueryMarkerPrefix identifies a workQuery produced by
// workflowServeControlReadyQueryForBeads. That function always writes this
// exact literal prefix (BD_EXPORT_AUTO=false plus a non-empty
// GC_CONTROL_TARGET, dispatch_runtime.go:788); no other work_query shape
// produces it.
const controlReadyQueryMarkerPrefix = "BD_EXPORT_AUTO=false GC_CONTROL_TARGET="

// controlReadyExcludeType mirrors the shell script's --exclude-type=epic.
const controlReadyExcludeType = "epic"

// controlReadyFallbackLimit bounds the single batched bd ready call issued
// when the cache can't answer. It must be generous enough that per-candidate/
// per-route filtering in Go (each capped at workflowServeScanLimit) is never
// starved by an earlier truncation at the bd layer -- unlike the shell script
// this replaces (which ran each candidate/route's own independently-capped bd
// call), this single batched call's cap is shared across every candidate and
// route, so it must hold a whole city's ready set even during the write
// bursts that make the cache dirty in the first place. It costs one bd call
// regardless of value, so err on the generous side; controlReadyFallbackReady
// also logs if a response ever comes back exactly at this limit, so silent
// truncation is at least observable.
const controlReadyFallbackLimit = 5000

// controlReadyCacheTTL bounds how long a primed control-ready snapshot is
// reused before the next tick re-primes it. A fresh CachingStore is built
// per drain invocation's first tick and reused for every ready bead
// processed in that invocation without any further bd calls; the TTL just
// caps how stale that snapshot can get across invocations (e.g. across the
// --follow loop's wake cycles) without needing a persistent, event-fed cache
// for the life of the process.
const controlReadyCacheTTL = 3 * time.Second

// controlReadyCacheMaxAge hard-caps how long one built snapshot may keep being
// served on the strength of a matching fingerprint alone (see
// revalidateControlReadyCache). The fingerprint is a three-scalar summary, not
// a hash of every field, so it can in principle alias -- two offsetting edits
// inside the same second. Rebuilding unconditionally past this age bounds that
// exposure to a known interval instead of letting a single aliased comparison
// pin a stale snapshot for the life of the process.
const controlReadyCacheMaxAge = 60 * time.Second

// parsedControlReadyQuery holds the values workflowServeControlReadyQueryForBeads
// bakes into its generated shell command as env-var prefix assignments.
type parsedControlReadyQuery struct {
	target             string
	controlSessionName string
	legacyTarget       string
	bareTarget         string
	includeEphemeral   bool
}

// parseControlReadyQuery recognizes a workQuery built by
// workflowServeControlReadyQueryForBeads and recovers the values it encoded
// as shell-quoted env-var prefix assignments, using shellquote.Split (the
// same package the query was built with) rather than hand-rolled parsing.
func parseControlReadyQuery(workQuery string) (parsedControlReadyQuery, bool) {
	if !strings.HasPrefix(workQuery, controlReadyQueryMarkerPrefix) {
		return parsedControlReadyQuery{}, false
	}
	parsed := parsedControlReadyQuery{
		includeEphemeral: strings.Contains(workQuery, "--include-ephemeral"),
	}
	for _, tok := range shellquote.Split(workQuery) {
		if tok == "sh" {
			break
		}
		switch {
		case strings.HasPrefix(tok, "GC_CONTROL_TARGET="):
			parsed.target = strings.TrimPrefix(tok, "GC_CONTROL_TARGET=")
		case strings.HasPrefix(tok, "GC_CONTROL_SESSION_NAME="):
			parsed.controlSessionName = strings.TrimPrefix(tok, "GC_CONTROL_SESSION_NAME=")
		case strings.HasPrefix(tok, "GC_CONTROL_LEGACY_TARGET="):
			parsed.legacyTarget = strings.TrimPrefix(tok, "GC_CONTROL_LEGACY_TARGET=")
		case strings.HasPrefix(tok, "GC_CONTROL_BARE_TARGET="):
			parsed.bareTarget = strings.TrimPrefix(tok, "GC_CONTROL_BARE_TARGET=")
		}
	}
	return parsed, parsed.target != ""
}

// envListValue looks up key in a KEY=VALUE environment list such as the one
// mergeRuntimeEnv produces, preferring the last match (matching os/exec's own
// last-wins semantics for duplicate keys).
func envListValue(environ []string, key string) string {
	prefix := key + "="
	for i := len(environ) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(environ[i], prefix); ok {
			return v
		}
	}
	return ""
}

// candidateLegacyVariant mirrors the shell loop's per-candidate legacy
// expansion: `case "$id" in *control-dispatcher) legacy="${id%control-dispatcher}workflow-control";; esac`.
// This is a plain suffix rewrite of whatever raw session/alias/id string is
// being checked, distinct from workflowServeLegacyControlRoute (which only
// matches a qualified-name-shaped target).
func candidateLegacyVariant(id string) string {
	const suffix = "control-dispatcher"
	if !strings.HasSuffix(id, suffix) {
		return ""
	}
	return strings.TrimSuffix(id, suffix) + "workflow-control"
}

// controlReadyCandidates returns the deduped, precedence-ordered assignee
// candidates the shell script would have checked: GC_CONTROL_SESSION_NAME,
// GC_SESSION_NAME, GC_ALIAS, GC_CONTROL_TARGET, GC_SESSION_ID, each paired
// with its control-dispatcher -> workflow-control legacy variant.
func controlReadyCandidates(parsed parsedControlReadyQuery, envList []string) []string {
	sources := []string{
		parsed.controlSessionName,
		envListValue(envList, "GC_SESSION_NAME"),
		envListValue(envList, "GC_ALIAS"),
		parsed.target,
		envListValue(envList, "GC_SESSION_ID"),
	}

	seen := make(map[string]struct{}, len(sources)*2)
	var candidates []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		candidates = append(candidates, id)
	}
	for _, id := range sources {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		add(id)
		add(candidateLegacyVariant(id))
	}
	return candidates
}

// controlReadyRoutes returns the routes routed_ready would have checked, in
// order: the target itself, its legacy alias, its bare alias.
func controlReadyRoutes(parsed parsedControlReadyQuery) []string {
	var routes []string
	for _, route := range []string{parsed.target, parsed.legacyTarget, parsed.bareTarget} {
		route = strings.TrimSpace(route)
		if route != "" {
			routes = append(routes, route)
		}
	}
	return routes
}

// filterReadyByAssignee mirrors `bd ready --assignee=$cand --exclude-type=epic --limit=N`.
// ready is expected to already be in canonical ready order (CachedReady/
// SortBeadsReadyOrder), matching bd's own default (no --sort) ready order.
func filterReadyByAssignee(ready []beads.Bead, assignee string, limit int) []beads.Bead {
	var out []beads.Bead
	for _, b := range ready {
		if b.Assignee != assignee || b.Type == controlReadyExcludeType {
			continue
		}
		out = append(out, b)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// filterReadyByRoute mirrors `bd ready --metadata-field $metadataKey=$route --unassigned --exclude-type=epic --sort oldest --limit=N`.
func filterReadyByRoute(ready []beads.Bead, metadataKey, route string, limit int) []beads.Bead {
	var matched []beads.Bead
	for _, b := range ready {
		if b.Assignee != "" || b.Type == controlReadyExcludeType {
			continue
		}
		if b.Metadata[metadataKey] != route {
			continue
		}
		matched = append(matched, b)
	}
	beads.SortBeads(matched, beads.SortCreatedAsc)
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	return matched
}

// mergeControlReadyGroups flattens the per-candidate/per-route result groups
// in the order they were checked, dropping beads still mid-instantiation and
// deduping by ID on first occurrence -- mirroring the shell script's closing
// `jq -s 'reduce add[] as $item (...)'` filter exactly, including its
// specific quirk: an instantiating-tagged occurrence of an ID is skipped
// WITHOUT being marked seen, so a later non-instantiating occurrence of the
// same ID still gets admitted.
func mergeControlReadyGroups(groups ...[]beads.Bead) []beads.Bead {
	seen := make(map[string]struct{})
	var merged []beads.Bead
	for _, group := range groups {
		for _, b := range group {
			if _, ok := seen[b.ID]; ok {
				continue
			}
			if strings.TrimSpace(b.Metadata[beadmeta.InstantiatingMetadataKey]) != "" {
				continue
			}
			seen[b.ID] = struct{}{}
			merged = append(merged, b)
		}
	}
	return merged
}

// evaluateControlReady answers a control-dispatcher readiness scan against an
// already-fetched ready set (from CachedReady or the single batched
// fallback), applying the exact candidate precedence, legacy/bare route
// aliasing, and instantiating-metadata dedup that
// workflowServeControlReadyQueryForBeads encodes as shell.
func evaluateControlReady(ready []beads.Bead, parsed parsedControlReadyQuery, envList []string) []beads.Bead {
	var groups [][]beads.Bead
	for _, cand := range controlReadyCandidates(parsed, envList) {
		groups = append(groups, filterReadyByAssignee(ready, cand, workflowServeScanLimit))
	}
	for _, route := range controlReadyRoutes(parsed) {
		groups = append(groups, filterReadyByRoute(ready, beadmeta.RunTargetMetadataKey, route, workflowServeScanLimit))
		groups = append(groups, filterReadyByRoute(ready, beadmeta.RoutedToMetadataKey, route, workflowServeScanLimit))
	}
	return mergeControlReadyGroups(groups...)
}

func beadsToHookBeads(items []beads.Bead) []hookBead {
	out := make([]hookBead, 0, len(items))
	for _, b := range items {
		out = append(out, hookBead{ID: b.ID, Metadata: hookBeadMetadata(b.Metadata)})
	}
	return out
}

// controlReadyFallbackReady issues exactly one batched `bd ready --json`
// call covering the whole active ready set (no --assignee/--metadata-field
// filter), for evaluateControlReady to filter in Go. Used when the in-process
// cache can't answer: dirty, still priming, or the rig's bd compatibility
// mode requires --include-ephemeral (a tier CachedReady can't serve).
func controlReadyFallbackReady(dir string, env map[string]string, includeEphemeral bool) ([]beads.Bead, error) {
	query := fmt.Sprintf("bd --readonly --sandbox ready --json --exclude-type=%s --limit=%d", controlReadyExcludeType, controlReadyFallbackLimit)
	if includeEphemeral {
		query += " --include-ephemeral"
	}
	output, err := shellWorkQueryWithEnv(query, dir, mergeRuntimeEnv(os.Environ(), env))
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(output)
	if !workQueryHasReadyWork(trimmed) {
		return nil, nil
	}
	var result []beads.Bead
	if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
		return nil, fmt.Errorf("control-ready fallback: unexpected bd ready output: %s", trimmed)
	}
	if len(result) == controlReadyFallbackLimit {
		log.Printf("control-ready fallback: bd ready for %s returned exactly the %d-item limit -- city-wide ready set may be truncated, some candidates/routes could see fewer beads than are actually ready", dir, controlReadyFallbackLimit)
	}
	beads.SortBeadsReadyOrder(result)
	return result, nil
}

var controlReadyCacheRegistry = struct {
	mu    sync.Mutex
	byDir map[string]*controlReadyCacheEntry
}{byDir: make(map[string]*controlReadyCacheEntry)}

type controlReadyCacheEntry struct {
	cache *beads.CachingStore
	// backing is retained so an expired entry can be revalidated with a single
	// cheap probe instead of opening a second store just to ask whether a
	// rebuild is needed.
	backing beads.Store
	// primedAt is when the snapshot was last known-good, and is the base for
	// controlReadyCacheTTL. A successful revalidation advances it without
	// rebuilding.
	primedAt time.Time
	// builtAt is when the snapshot was actually constructed, and is the base
	// for controlReadyCacheMaxAge. Revalidation never advances it.
	builtAt time.Time
	// fingerprint is the active-surface summary taken just before the prime
	// that produced cache. Empty means "never reuse this entry".
	fingerprint string
}

// reusableBacking returns the store a rebuild can build a fresh snapshot on
// instead of opening its own, or nil when this entry has none to offer. The
// nil receiver is the "no entry registered for this dir yet" case and is
// deliberately supported so the caller needs no separate presence check.
func (e *controlReadyCacheEntry) reusableBacking() beads.Store {
	if e == nil {
		return nil
	}
	return e.backing
}

// openControlReadyStore is a seam so tests can supply a store with known
// fingerprint behaviour; production always uses openControlStoreAtForCity.
var openControlReadyStore = openControlStoreAtForCity

// controlReadyCacheFor returns a short-lived, best-effort in-process ready
// snapshot for dir, reusing one primed within controlReadyCacheTTL instead of
// re-priming on every drain-loop tick. Past the TTL it does not rebuild
// immediately: revalidateControlReadyCache first asks, in one cheap probe,
// whether the active surface changed at all, and only rebuilds when it did or
// when the answer cannot be trusted. Returns nil whenever the cache cannot
// be built or trusted; callers must treat nil as "fall back to a live bd
// query", not as an error -- an unopenable store here is possible in scopes
// this readiness scan does not normally run against (e.g. test fixtures with
// no rig configured) and the sibling control-bead-processing path
// (runControlDispatcherInStore) would already be failing loudly if it were a
// real production gap.
//
// Known limitation (low-impact, not fixed here): concurrent callers racing a
// stale/missing entry for the same dir each independently open+prime their
// own store rather than coalescing behind one in-flight prime -- last writer
// into controlReadyCacheRegistry wins. Same class of gap already accepted
// for CachingStore.List/Ready cache-miss reads; worth revisiting with a
// singleflight if overlapping invocations against the same city/dir become
// common (e.g. a restart handoff window), but the control-dispatcher serve
// loop's typical call pattern is sequential-per-tick per dir.
//
// cfgFn is called at most once, and only on the branch that actually opens a
// store (no entry yet, or an entry with no retained backing to reuse) -- the
// hot within-TTL path and the revalidate-and-reuse path never touch it. A
// full config load re-validates every builtin pack's file manifest
// (EnsureBuiltinRuntimeAssets), so paying it on ticks this cache already
// answers from a warm snapshot was the dominant cost of the control-dispatcher
// serve loop's steady-state CPU (gcy pending, sample-profiled: 86/87 samples
// under loadCityConfig). cfgFn lets the caller defer that cost to the ticks
// that truly need it.
func controlReadyCacheFor(dir, cityPath string, cfgFn func() *config.City) *beads.CachingStore {
	controlReadyCacheRegistry.mu.Lock()
	entry, ok := controlReadyCacheRegistry.byDir[dir]
	fresh := ok && time.Since(entry.primedAt) < controlReadyCacheTTL
	controlReadyCacheRegistry.mu.Unlock()
	if fresh {
		return entry.cache
	}
	if ok {
		if cache := revalidateControlReadyCache(dir, entry); cache != nil {
			return cache
		}
	}

	// A rebuild replaces the SNAPSHOT, not the store beneath it. BdStore holds
	// no bead data -- every read still shells out to bd, so a retained store
	// cannot serve a stale bead -- and caches only capability verdicts: the
	// ready-projection version gate and the conditional-write probe. Opening a
	// fresh store per rebuild therefore buys no freshness and costs a cold
	// version gate, i.e. a whole `bd version` process spawn (measured ~0.5s,
	// as much as a real query) on every rebuild. That is the contradiction
	// bdReadyProjectionEnabled's own doc comment describes when it says it
	// probes "once per process": reusing the retained store is what makes that
	// comment true, without package-global state that would leak across tests
	// injecting runners with different bd versions.
	//
	// The staleness trap this cache guards lives in CachingStore, which absorbs
	// and never evicts (see revalidateControlReadyCache); that is still built
	// from scratch below, so the snapshot remains an exact point-in-time read.
	//
	// Consequence worth knowing: the store's scope resolution is done once, at
	// first open, so a city.toml edit relocating this dir's scope is not picked
	// up until the process restarts -- the same restart requirement the version
	// gate already documents.
	store := entry.reusableBacking()
	if store == nil {
		var cfg *config.City
		if cfgFn != nil {
			cfg = cfgFn()
		}
		opened, err := openControlReadyStore(dir, cityPath, cfg)
		if err != nil {
			return nil
		}
		store = opened
	}
	// Take the fingerprint BEFORE priming, not after. Priming is several
	// seconds of bd calls, and a write landing inside that window must not be
	// able to produce an entry whose fingerprint already describes state the
	// snapshot does not contain -- that would let the next tick match and
	// serve a snapshot known to be stale. Sampling first means such a write
	// instead shows up as a mismatch and forces a rebuild: a wasted prime,
	// which is exactly what happens today anyway, rather than a stale answer.
	fingerprint := ""
	if fp, isFingerprinter := store.(beads.ActiveFingerprinter); isFingerprinter {
		if current, fpErr := fp.ActiveFingerprint(); fpErr == nil {
			fingerprint = current
		}
	}
	cs := beads.NewCachingStore(store, nil)
	if err := cs.PrimeActive(); err != nil {
		log.Printf("control-ready cache: pre-prime failed for %s: %v (falling back to a live bd query)", dir, err)
		return nil
	}

	now := time.Now()
	controlReadyCacheRegistry.mu.Lock()
	controlReadyCacheRegistry.byDir[dir] = &controlReadyCacheEntry{
		cache:       cs,
		backing:     store,
		primedAt:    now,
		builtAt:     now,
		fingerprint: fingerprint,
	}
	controlReadyCacheRegistry.mu.Unlock()
	return cs
}

// revalidateControlReadyCache tries to extend an expired entry's life without
// rebuilding it, returning the reusable cache or nil meaning "rebuild".
//
// Rebuilding costs several bd subprocesses -- a List per active status over
// TierBoth plus the ready-projection union scan -- and on this workload each
// subprocess pays a ~0.5s process-spawn floor regardless of how trivial its
// query is. The fingerprint probe costs one. (Rebuilds no longer re-probe the
// bd version gate: controlReadyCacheFor reuses the retained backing store,
// whose capability verdicts are already memoized.) On a city where nothing changed since the last tick, which
// is the overwhelmingly common case for a control-dispatcher wake, the
// existing snapshot is not merely fresh enough: it is exactly correct, because
// there is nothing for it to be stale about.
//
// This is deliberately NOT the "reuse the store and re-prime it" fix that
// looks equivalent. CachingStore.PrimeActive absorbs rows and never evicts, so
// re-priming a retained store would leave beads that have since closed sitting
// in the cache and the dispatcher would keep seeing them as ready. Reuse here
// is gated on the surface provably not having changed at all, so no eviction
// question arises.
//
// Every uncertain outcome -- no fingerprinter, probe error, empty result,
// mismatch, or an entry too old to trust a summary comparison -- returns nil
// and rebuilds. Reuse requires a positive, matching answer.
func revalidateControlReadyCache(dir string, entry *controlReadyCacheEntry) *beads.CachingStore {
	if entry == nil || entry.cache == nil || entry.backing == nil || entry.fingerprint == "" {
		return nil
	}
	if time.Since(entry.builtAt) >= controlReadyCacheMaxAge {
		return nil
	}
	fp, ok := entry.backing.(beads.ActiveFingerprinter)
	if !ok {
		return nil
	}
	current, err := fp.ActiveFingerprint()
	if err != nil || current == "" || current != entry.fingerprint {
		return nil
	}

	controlReadyCacheRegistry.mu.Lock()
	// Only advance the entry still registered for dir: a concurrent rebuild
	// may have replaced it while the probe was in flight, and that newer
	// snapshot must keep its own primedAt.
	if live, present := controlReadyCacheRegistry.byDir[dir]; present && live == entry {
		entry.primedAt = time.Now()
	}
	controlReadyCacheRegistry.mu.Unlock()
	return entry.cache
}

// loadCityConfigForControlReady is a seam so tests can observe (or stub out)
// the config load tryControlReadyFromCacheOrFallback pays for on a cold-start
// or expired-and-changed cache. Production loads the real config, at the same
// cost EnsureBuiltinRuntimeAssets always charges the first caller into a city.
var loadCityConfigForControlReady = func(cityPath string) *config.City {
	cfg, _ := loadCityConfig(cityPath, io.Discard)
	return cfg
}

// tryControlReadyFromCacheOrFallback answers a control-dispatcher readiness
// scan in-process instead of running workflowServeControlReadyQueryForBeads's
// shell script. handled reports whether workQuery was even recognized as a
// control-ready query; when handled is false the caller must run workQuery
// as a shell command exactly as before. This changes the DATA SOURCE for
// control-dispatcher readiness, not the decision logic (ga-ak6rt1): candidate
// precedence, legacy/bare route aliasing, and the instantiating-metadata
// dedup filter are reproduced exactly by evaluateControlReady.
func tryControlReadyFromCacheOrFallback(workQuery, dir string, env map[string]string) (queue []hookBead, handled bool, err error) {
	parsed, ok := parseControlReadyQuery(workQuery)
	if !ok {
		return nil, false, nil
	}

	cityPath := cityForStoreDir(dir)
	envList := mergeRuntimeEnv(os.Environ(), env)

	if !parsed.includeEphemeral {
		cfgFn := func() *config.City { return loadCityConfigForControlReady(cityPath) }
		if cache := controlReadyCacheFor(dir, cityPath, cfgFn); cache != nil {
			if ready, ok := cache.CachedReady(); ok {
				return beadsToHookBeads(evaluateControlReady(ready, parsed, envList)), true, nil
			}
		}
	}

	ready, err := controlReadyFallbackReady(dir, env, parsed.includeEphemeral)
	if err != nil {
		return nil, true, err
	}
	return beadsToHookBeads(evaluateControlReady(ready, parsed, envList)), true, nil
}

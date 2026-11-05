package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// This file cuts the gc->bd read-storm documented on ga-ak6rt1: the
// control-dispatcher's per-tick readiness scan (workflowServeControlReadyQueryForBeads,
// dispatch_runtime.go) builds a shell script that fork-execs up to ~9
// bd/jq processes per agent per tick. This file answers that same readiness
// evaluation from exactly one Ready read per ledger leg (one `bd ready` call on
// a bd leg, in-process elsewhere) instead of the shell script's N separate `bd`
// invocations. There is no shell reader behind it: the gc.work_outcome=blocked
// veto lives in the Go Store.Ready implementations and an external `bd ready`
// is blind to it, so a scope whose legs cannot be opened fails the scan loudly
// rather than reading a queue that would release vetoed control beads.
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
// falls straight through to the original shell exec, unchanged.

// controlReadyQueryMarkerPrefix identifies a workQuery produced by
// workflowServeControlReadyQueryForBeads. That function always writes this
// exact literal prefix (BD_EXPORT_AUTO=false plus a non-empty
// GC_CONTROL_TARGET, dispatch_runtime.go:788); no other work_query shape
// produces it.
const controlReadyQueryMarkerPrefix = "BD_EXPORT_AUTO=false GC_CONTROL_TARGET="

// controlReadyExcludeType mirrors the shell script's --exclude-type=epic.
const controlReadyExcludeType = "epic"

// controlReadyScanLimit bounds the rows one leg's ready read contributes to the
// scan. It must be generous enough that per-candidate/per-route filtering in Go
// (each capped at workflowServeScanLimit) is never starved by an earlier
// truncation here -- unlike the shell script this replaces (which ran each
// candidate/route's own independently-capped bd call), one leg's cap is shared
// across every candidate and route, so it must hold a whole city's ready set
// even during write bursts. It costs nothing to read past it, so err on the
// generous side; controlReadyLegReady also logs if a leg ever comes back at this
// limit, so silent truncation is at least observable.
const controlReadyScanLimit = 5000

// parsedControlReadyQuery holds the values workflowServeControlReadyQueryForBeads
// bakes into its generated shell command as env-var prefix assignments.
type parsedControlReadyQuery struct {
	target             string
	controlSessionName string
	legacyTarget       string
	bareTarget         string
}

// parseControlReadyQuery recognizes a workQuery built by
// workflowServeControlReadyQueryForBeads and recovers the values it encoded
// as shell-quoted env-var prefix assignments, using shellquote.Split (the
// same package the query was built with) rather than hand-rolled parsing.
func parseControlReadyQuery(workQuery string) (parsedControlReadyQuery, bool) {
	if !strings.HasPrefix(workQuery, controlReadyQueryMarkerPrefix) {
		return parsedControlReadyQuery{}, false
	}
	var parsed parsedControlReadyQuery
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
// ready is expected to already be in canonical ready order
// (SortBeadsReadyOrder), matching bd's own default (no --sort) ready order.
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

// filterReadyByRoute mirrors `bd ready --metadata-field $metadataKey=$route --unassigned --exclude-type=epic --exclude-label <label>... --sort oldest --limit=N`,
// where the excluded labels are config.PoolDemandServeRulesForQuery().ExcludeLabels:
// the dispatch holds and bd's native human label.
// This is a route-scoped, unassigned tier (Tier 3 pool-demand/control-dispatcher
// routing), so beads parked for a hold or a person must be excluded (ga-5736js):
// filterReadyByAssignee (Tier 1/2, assignee-scoped) stays transparent to both
// by design and must not gain this filter.
func filterReadyByRoute(ready []beads.Bead, metadataKey, route string) []beads.Bead {
	excludedLabels := config.PoolDemandServeRulesForQuery().ExcludeLabels
	var matched []beads.Bead
	for _, b := range ready {
		if b.Assignee != "" || b.Type == controlReadyExcludeType {
			continue
		}
		if b.Metadata[metadataKey] != route {
			continue
		}
		excluded := false
		for _, label := range excludedLabels {
			if beadLabelsContain(b.Labels, label) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		matched = append(matched, b)
	}
	beads.SortBeads(matched, beads.SortCreatedAsc)
	if len(matched) > workflowServeScanLimit {
		matched = matched[:workflowServeScanLimit]
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
// already-fetched ready set (the per-leg Ready reads), applying the exact candidate precedence, legacy/bare route
// aliasing, and instantiating-metadata dedup that
// workflowServeControlReadyQueryForBeads encodes as shell.
func evaluateControlReady(ready []beads.Bead, parsed parsedControlReadyQuery, envList []string) []beads.Bead {
	var groups [][]beads.Bead
	for _, cand := range controlReadyCandidates(parsed, envList) {
		groups = append(groups, filterReadyByAssignee(ready, cand, workflowServeScanLimit))
	}
	for _, route := range controlReadyRoutes(parsed) {
		groups = append(groups, filterReadyByRoute(ready, beadmeta.RunTargetMetadataKey, route))
		groups = append(groups, filterReadyByRoute(ready, beadmeta.RoutedToMetadataKey, route))
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

// mergeControlReadyLegs unions the legs in order, first leg winning on a
// duplicate id, then restores canonical ready order over the whole set.
//
// The global re-sort is a deliberate divergence from `gc ready`'s federation,
// which preserves per-leg order. evaluateControlReady's inputs are documented as
// canonical (see filterReadyByAssignee), and its assignee tiers cap at
// workflowServeScanLimit by truncating the head of that order — so leaving the
// graph leg's rows appended after the scope leg's would let leg membership, not
// readiness, decide which beads survive the cap.
func mergeControlReadyLegs(legs ...[]beads.Bead) []beads.Bead {
	var merged []beads.Bead
	seen := make(map[string]struct{})
	for _, leg := range legs {
		for _, b := range leg {
			if _, ok := seen[b.ID]; ok {
				continue
			}
			seen[b.ID] = struct{}{}
			merged = append(merged, b)
		}
	}
	beads.SortBeadsReadyOrder(merged)
	return merged
}

// controlReadyLegReady is one leg's batched ready read, taken through the
// leg's store: a bd leg answers with one `bd ready` call, every other leg
// in-process.
//
// It reproduces the shell script's filters rather than approximating them: the
// read spans both tiers (TierBoth), the tier the dispatcher's queue has always
// been drawn from and the one that includes wisps; --exclude-type is applied in
// Go because ReadyQuery carries no type selector; and the limit is taken after
// that exclusion.
func controlReadyLegReady(dir string, binding beads.Store) ([]beads.Bead, error) {
	ready, err := binding.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		return nil, fmt.Errorf("control-ready scan: reading the ready leg for %s: %w", dir, err)
	}
	result := make([]beads.Bead, 0, len(ready))
	for _, bead := range ready {
		if bead.Type == controlReadyExcludeType {
			continue
		}
		result = append(result, bead)
		if len(result) == controlReadyScanLimit {
			log.Printf("control-ready scan: the ready leg for %s returned at least the %d-item limit -- city-wide ready set may be truncated, some candidates/routes could see fewer beads than are actually ready", dir, controlReadyScanLimit)
			break
		}
	}
	beads.SortBeadsReadyOrder(result)
	return result, nil
}

// controlReadyLegsReady answers one readiness scan with exactly one Ready read
// per leg for dir, and closes every leg it opened before returning. Every
// failure is loud: legs that cannot be opened and a leg whose Ready read fails
// both fail the whole scan, because a work query has nowhere to say "this
// answer is short" and a partial union must not read as a complete queue.
// Nothing reads the ledger another way when the legs will not open: the only
// other reader was an external `bd ready`, which is blind to the
// gc.work_outcome=blocked veto.
//
// Every call reads afresh; nothing is reused by a later scan. The serve loop
// re-scans only after it has written (processed a control bead) or after it has
// waited for another writer (idle re-poll, event wake, follow sweep), so a
// reused answer is stale on both arms: it re-offers the control bead the loop
// just closed, and it hides the control bead a worker's close just made ready
// (ga-vnycm2.9).
//
// One Ready read per leg is the whole cost of a scan (ga-vnycm2.19): one
// `bd ready` call on a bd leg, in-process on file, SQLite and native legs. Each
// read is TierBoth, the tier the dispatcher's queue has always been drawn from
// (the primed snapshot this replaces held ephemeral rows too), and it applies
// the same work-outcome veto the snapshot applied.
//
// The legs are the SAME ledgers runControlDispatcherWithStoreAndConfig
// dispatches against -- controlReadyLegSources applies the routing rule
// controlBeadLedger resolves against -- so the queue and the mutation are the
// same set. They must not diverge. A queue drawn from the work store while the
// dispatch closes the binding's copy re-offers the same id every sweep --
// ProcessControl no-ops on the already-closed copy, so drainWorkflowServeWork
// pays a full dispatch on every sweep to skip it; and the beads the dispatch
// CREATES (fanout fragments, retry attempts, drain units) would land in a
// ledger the scan never reads, stalling the workflow at its first hop.
func controlReadyLegsReady(dir, cityPath string, cfg *config.City) (ready []beads.Bead, err error) {
	sources, owned, err := controlReadyLegSourcesFn(dir, cityPath, cfg)
	if err != nil {
		return nil, fmt.Errorf("control-ready scan: opening the ready legs for %s: %w", dir, err)
	}
	// Release the scoped backing handles as soon as the read returns, on the
	// success and the error path alike, so no read mark on a shared WAL outlives
	// the scan (#6255). Only owned legs are closed; the graph binding is
	// process-shared (see controlReadyLegSources).
	defer func() {
		for _, s := range owned {
			if err := closeBeadStoreHandle(s); err != nil {
				log.Printf("control-ready scan: closing scope store for %s: %v", dir, err)
			}
		}
	}()
	legs := make([][]beads.Bead, 0, len(sources))
	for _, source := range sources {
		// Read the engine, not the one-shot emitter around a binding leg: the
		// scan never writes, so the emitter adds nothing.
		leg, err := controlReadyLegReady(dir, bindingEngine(source))
		if err != nil {
			return nil, err
		}
		legs = append(legs, leg)
	}
	if len(legs) == 1 {
		return legs[0], nil
	}
	return mergeControlReadyLegs(legs...), nil
}

// controlReadyLegSources returns the ordered ledgers to read, applying the
// routing rule controlGraphBinding and controlGraphRelicLeg answer between
// them:
//
//   - A CITY scope whose graph class relocated reads the binding INSTEAD of its
//     own store. The control beads in the work store are the copies the
//     migration retained, which no longer receive the workflow's mutations;
//     enumerating those would hand the drain loop a queue of ids the dispatch
//     then no-ops on forever.
//   - A RIG scope on that same city reads the binding FIRST, where every tree
//     minted since S4 lives (city-scoped molecules routed here by name, and the
//     rig's own new trees), and its own store second while the rig may still
//     hold pre-S4 trees. Once the rig is proven relic-free it reads the binding
//     only.
//
// owned is the subset of sources this call constructed and is therefore
// responsible for closing once the scan's read has returned. It is
// deliberately NOT every source. The graph-class binding legs
// (controlGraphBinding) resolve through the per-city
// cliStorageRoutes memo, so the binding store is process-shared and closed only
// by closeCLIStorageRoutes at process exit; CloseStore is a one-way latch, so
// closing a binding leg here would poison every later graph-class read and write
// in the process with ErrStoreClosed. Only the scoped leg
// (openControlStoreAtForCity) is freshly constructed per call, so only it is
// returned as owned.
//
// An error return must not hand back opened stores. controlReadyLegsReady
// registers its closing defer only after this call succeeds, so a store opened
// before an error return would have nothing to close it. Today the sole error
// return IS the failed open, so nothing is open on that path; an arm added
// later that can fail after a successful open must close what it opened before
// returning. The same obligation binds any controlReadyLegSourcesFn seam.
func controlReadyLegSources(dir, cityPath string, cfg *config.City) (sources, owned []beads.Store, err error) {
	// A relocated scope with no relic leg does not open its scope store at all
	// — that would be a bd process this scan never reads. The binding is
	// process-shared, so nothing here owns it. A rig scope that may still hold
	// pre-S4 trees reads its own store second.
	if binding, relocated := controlGraphBinding(cityPath, dir); relocated {
		if !controlGraphRelicLeg(cityPath, dir) {
			return []beads.Store{binding}, nil, nil
		}
		scoped, err := openControlStoreAtForCity(dir, cityPath, cfg)
		if err != nil {
			return nil, nil, err
		}
		return []beads.Store{binding, scoped}, []beads.Store{scoped}, nil
	}
	scoped, err := openControlStoreAtForCity(dir, cityPath, cfg)
	if err != nil {
		return nil, nil, err
	}
	return []beads.Store{scoped}, []beads.Store{scoped}, nil
}

// controlReadyLegSourcesFn is the test seam for controlReadyLegSources,
// following the package's own var-seam idiom (cf. controlDispatcherServe,
// dispatch_runtime.go). Production always uses controlReadyLegSources.
var controlReadyLegSourcesFn = controlReadyLegSources

// tryControlReadyScan answers a control-dispatcher readiness scan in-process
// instead of running workflowServeControlReadyQueryForBeads's shell script.
// handled reports whether workQuery was a control-ready query at all; when it is
// false the caller must run workQuery as a shell command exactly as before.
// A query carrying the control-ready marker is always handled: one the scan
// cannot parse is an error, never a query to hand to the shell, because that
// script's `bd ready` calls are blind to the gc.work_outcome=blocked veto. This
// changes the DATA SOURCE for control-dispatcher readiness, not the decision
// logic (ga-ak6rt1): candidate precedence, legacy/bare route aliasing, and the
// instantiating-metadata dedup filter are reproduced exactly by
// evaluateControlReady.
func tryControlReadyScan(workQuery, dir string, env map[string]string) (queue []hookBead, handled bool, err error) {
	parsed, ok := parseControlReadyQuery(workQuery)
	if !ok {
		if strings.HasPrefix(workQuery, controlReadyQueryMarkerPrefix) {
			return nil, true, fmt.Errorf("control-ready scan: work query carries the control-ready marker but names no control target: %q", workQuery)
		}
		return nil, false, nil
	}

	cityPath := cityForStoreDir(dir)
	cfg, _ := loadCityConfig(cityPath, io.Discard)
	envList := mergeRuntimeEnv(os.Environ(), env)

	start := time.Now()
	ready, err := controlReadyLegsReady(dir, cityPath, cfg)
	if err != nil {
		return nil, true, err
	}
	queue = beadsToHookBeads(evaluateControlReady(ready, parsed, envList))
	workflowTracef("control-ready scan ready=%d queue=%d dur=%s", len(ready), len(queue), time.Since(start))
	return queue, true, nil
}

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
)

// wispStepInjectionContent resolves the agent's current in-progress formula
// step bead and returns it formatted as a <system-reminder> block, or "" if
// none is found or any error occurs. Designed for best-effort use in hook
// injection paths — callers must never fail hard on an empty return.
//
// Store priority: if GC_RIG_ROOT is set the rig store is queried (where
// rig-scoped polecat work beads live), otherwise the city store at cityPath.
// When cityPath is empty the function falls back to GC_CITY from the env.
func wispStepInjectionContent(cityPath string) string {
	effective := cityPath
	if effective == "" {
		effective = strings.TrimSpace(os.Getenv("GC_CITY"))
	}
	store := openWispStepStore(effective)
	if store == nil {
		return ""
	}
	assignees := wispStepAssignees()
	if len(assignees) == 0 {
		return ""
	}
	routeCfg, _ := loadCityConfigWithoutBuiltinPackRefresh(effective, io.Discard)
	b, err := resolveActiveWispStep(store, cliGraphStore(store, routeCfg, effective).Store, assignees)
	if err != nil || b == nil {
		return ""
	}
	return formatWispStepReminder(b)
}

// openWispStepStore opens the bead store to query for active wisp steps.
// If GC_RIG_ROOT is set it opens that rig's store (where rig-scoped polecat
// work lives); otherwise it opens the city store at cityPath.
// Returns nil on any error — callers treat nil as "no store available".
// This is the WORK store and stays unrouted; see resolveActiveWispStep.
func openWispStepStore(cityPath string) beads.Store {
	if rigRoot := strings.TrimSpace(os.Getenv("GC_RIG_ROOT")); rigRoot != "" {
		store, err := openStoreAtForCity(rigRoot, cityPath)
		if err == nil {
			return store
		}
	}
	if cityPath == "" {
		return nil
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil
	}
	return store
}

// wispStepAssignees returns the deduped set of identity strings to match
// against bead assignees. Uses GC_ALIAS (primary), GC_SESSION_NAME, and
// GC_SESSION_ID in that priority order.
func wispStepAssignees() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	add(os.Getenv("GC_ALIAS"))
	add(os.Getenv("GC_SESSION_NAME"))
	add(os.Getenv("GC_SESSION_ID"))
	return out
}

// resolveActiveWispStep returns the agent's current formula step bead.
//
// Resolution order:
//  1. Find the agent's in-progress molecule bead (type=molecule or type=wisp).
//  2. Find that molecule's in-progress type=step child — the current step.
//  3. If no in-progress step child exists, fall back to the entry step: the
//     first open type=step child whose blockers are all satisfied (see
//     resolveEntryStepChild).
//  4. If no molecule bead is assigned to the agent, follow the molecule_id
//     bridge: an attached (v1) formula routes only the source work bead and
//     stamps its molecule_id with the (unrouted, unassigned) root, so resolve
//     the root's active step through that bridge. Two bridge sources are
//     tried, claimed state first: (a) the source bead is already
//     in_progress+assigned — the state that exists once the agent has claimed
//     it; (b) the source bead is still open+unassigned but carries
//     gc.routed_to for one of the agent's identities — the state that exists
//     at the exact wake (gc prime / nudge) the bridge is meant to serve,
//     before any claim has happened. (b) is tried only when the agent has
//     claimed nothing at all, and only for a source bead a claim could take
//     now: claimed work, molecule or not, is the agent's assignment, and a
//     routed bead bd ready would not offer is not (ga-qi3glw4).
//  5. If no molecule_id bridge exists either, fall back to any in-progress bead
//     with a non-empty Description (legacy behavior for agents not running a
//     formula).
//
// Returns nil, nil when no bead can be resolved. Never returns an error for
// not-found conditions — callers treat nil as "nothing to inject".
//
// Both stores, because the resolution spans two classes: roots and type=step
// children are ClassGraph, the assignee-owned beads it starts from are work.
func resolveActiveWispStep(workStore, graphStore beads.Store, assignees []string) (*beads.Bead, error) {
	if workStore == nil || len(assignees) == 0 {
		return nil, nil
	}
	if graphStore == nil {
		graphStore = workStore
	}

	// One store holding both classes answers every "what is in progress for
	// these identities" question below from one listing; split classes ask
	// each store their own.
	var claimed *claimedWork
	if sameStoreHandle(workStore, graphStore) {
		rows, err := listClaimedWork(workStore, assignees)
		if err != nil {
			return nil, fmt.Errorf("listing in-progress work: %w", err)
		}
		claimed = &claimedWork{rows: rows}
	}

	var molecule *beads.Bead
	var err error
	if claimed != nil {
		molecule = claimed.activeMolecule()
	} else if molecule, err = resolveActiveMolecule(graphStore, assignees); err != nil {
		return nil, err
	}
	if molecule == nil {
		// No molecule root is assigned to the agent. Attached (v1) formulas
		// leave the root unrouted and stamp molecule_id on the routed source
		// bead, so follow that bridge to the root's active step before the
		// legacy description fallback. Best-effort: a resolution error or no
		// bridge drops to legacy. The routed bridge waits behind any claimed
		// work, which the legacy fallback presents instead.
		var root *beads.Bead
		if claimed != nil {
			root = firstMoleculeRootFrom(graphStore, claimed.newest(claimedBridgeSample))
			if root == nil && len(claimed.rows) == 0 {
				root = resolveMoleculeRootViaRoutedBridge(workStore, graphStore, assignees)
			}
		} else {
			root = resolveMoleculeRootViaBridge(workStore, graphStore, assignees)
		}
		if root != nil {
			step, stepErr := resolveInProgressStepChild(graphStore, root.ID)
			if stepErr != nil {
				log.Printf("wisp step inject: error resolving in-progress step for bridged molecule %s: %v", root.ID, stepErr)
				return nil, nil
			}
			if step != nil {
				return step, nil
			}
			return resolveEntryStepChild(graphStore, root.ID)
		}
		// No molecule bridge; fall back to legacy: any in-progress bead with a description.
		if claimed != nil {
			return firstWithDescription(claimed.newest(claimedDescriptionSample)), nil
		}
		return resolveBeadWithDescription(workStore, assignees)
	}

	// Prefer the in-progress step child (the agent is mid-step).
	step, err := resolveInProgressStepChild(graphStore, molecule.ID)
	if err != nil {
		log.Printf("wisp step inject: error resolving in-progress step children for molecule %s: %v", molecule.ID, err)
		return nil, nil
	}
	if step != nil {
		return step, nil
	}

	// Fall back to the entry step: first open step child.
	log.Printf("wisp step inject: no in-progress step for molecule %s; resolving entry step", molecule.ID)
	return resolveEntryStepChild(graphStore, molecule.ID)
}

// Sample sizes of the step-reminder resolution: each question looks at the
// newest claimed beads only, cut in created order, newest first.
const (
	// activeMoleculeSample is how many of the newest in-progress roots of one
	// type are compared for the most recently updated.
	activeMoleculeSample = 5
	// claimedBridgeSample is how many of the newest claimed beads are searched
	// for a molecule_id bridge.
	claimedBridgeSample = 10
	// claimedDescriptionSample is how many of the newest claimed beads are
	// searched for one with a description.
	claimedDescriptionSample = 10
)

// activeMoleculeTypes are the root types searched for an active molecule, in
// preference order.
var activeMoleculeTypes = []string{"molecule", "wisp"}

// claimedWork is every in-progress bead assigned to the hook's identities,
// newest created first: the one listing the step reminder's in-progress
// questions are projections of when one store holds work and graph beads.
type claimedWork struct {
	rows []beads.Bead
}

// listClaimedWork lists every in-progress bead assigned to any of assignees,
// both tiers, newest created first.
func listClaimedWork(store beads.Store, assignees []string) ([]beads.Bead, error) {
	return store.List(beads.ListQuery{
		Status:    "in_progress",
		Assignees: assignees,
		TierMode:  beads.TierBoth,
		Sort:      beads.SortCreatedDesc,
	})
}

// newest returns the n newest claimed beads.
func (c *claimedWork) newest(n int) []beads.Bead {
	if len(c.rows) <= n {
		return c.rows
	}
	return c.rows[:n]
}

// activeMolecule is resolveActiveMolecule answered from the claimed work: of
// each root type in turn, the newest activeMoleculeSample roots, the most
// recently updated of them.
func (c *claimedWork) activeMolecule() *beads.Bead {
	for _, molType := range activeMoleculeTypes {
		var sample []beads.Bead
		for _, b := range c.rows {
			if b.Type != molType {
				continue
			}
			sample = append(sample, b)
			if len(sample) == activeMoleculeSample {
				break
			}
		}
		if best := mostRecentlyUpdatedMolecule(sample, molType); best != nil {
			return best
		}
	}
	return nil
}

// sameStoreHandle reports whether a and b are the same store handle.
func sameStoreHandle(a, b beads.Store) bool {
	if a == nil || b == nil {
		return false
	}
	ta := reflect.TypeOf(a)
	return ta == reflect.TypeOf(b) && ta.Kind() == reflect.Pointer && a == b
}

// resolveActiveMolecule returns the agent's in-progress molecule bead.
// When multiple molecules are found, the most recently updated of the five
// newest is returned and the ambiguity is logged. The sample is cut in created
// order, newest first, so which five are compared does not depend on the
// store's default listing order. Returns nil, nil when none is found.
func resolveActiveMolecule(store beads.Store, assignees []string) (*beads.Bead, error) {
	for _, molType := range activeMoleculeTypes {
		results, err := store.List(beads.ListQuery{
			Status:    "in_progress",
			Type:      molType,
			Assignees: assignees,
			TierMode:  beads.TierBoth,
			Limit:     activeMoleculeSample,
			Sort:      beads.SortCreatedDesc,
		})
		if err != nil {
			return nil, fmt.Errorf("listing in-progress %s beads: %w", molType, err)
		}
		if best := mostRecentlyUpdatedMolecule(results, molType); best != nil {
			return best, nil
		}
	}
	return nil, nil
}

// mostRecentlyUpdatedMolecule returns the most recently updated of the sampled
// roots of one type (the first on a tie), logging the ambiguity when there is
// more than one; nil for an empty sample.
func mostRecentlyUpdatedMolecule(results []beads.Bead, molType string) *beads.Bead {
	if len(results) == 0 {
		return nil
	}
	if len(results) > 1 {
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.ID
		}
		log.Printf("wisp step inject: %d in-progress %s beads found (%s); using most recent", len(results), molType, strings.Join(ids, ", "))
	}
	best := results[0]
	for _, r := range results[1:] {
		if r.UpdatedAt.After(best.UpdatedAt) {
			best = r
		}
	}
	return &best
}

// resolveMoleculeRootViaBridge finds the molecule root reachable from an
// attached (v1) source work bead. Attached formulas route only the source bead
// and stamp its molecule_id metadata with the (unrouted, unassigned) molecule
// root, so resolveActiveMolecule — which filters molecule roots by assignee —
// never matches. This bridges from the source bead to its root via the
// molecule_id metadata key, trying the claimed (in_progress+assigned) source
// state first and falling back to the pre-claim (open, gc.routed_to) state
// only when the agent has claimed nothing — see resolveActiveWispStep's doc
// comment, step 4. A claimed listing that fails proves nothing about claimed
// work, so it does not fall back either.
//
// Returns nil on any error or when no bridge bead is found — callers treat nil
// as "no bridge available" and fall through to the legacy path.
//
// The bridge crosses a class boundary: the bead carrying the stamp is work
// class, the root it names is ClassGraph.
//
// The ten candidates are the newest claimed work, cut in created order so the
// sample does not depend on the store's default listing order.
func resolveMoleculeRootViaBridge(workStore, graphStore beads.Store, assignees []string) *beads.Bead {
	results, err := workStore.List(beads.ListQuery{
		Status:    "in_progress",
		Assignees: assignees,
		TierMode:  beads.TierBoth,
		Limit:     claimedBridgeSample,
		Sort:      beads.SortCreatedDesc,
	})
	if err != nil {
		return nil
	}
	if root := firstMoleculeRootFrom(graphStore, results); root != nil {
		return root
	}
	if len(results) > 0 {
		return nil
	}
	return resolveMoleculeRootViaRoutedBridge(workStore, graphStore, assignees)
}

// resolveMoleculeRootViaRoutedBridge finds the molecule root reachable from a
// source work bead that has been routed but not yet claimed: still
// status=open and unassigned, carrying gc.routed_to for one of the agent's
// identities. cliBeadRouter.Route (cmd/gc/cmd_sling.go) stamps gc.routed_to
// alone at sling time, deliberately leaving status and assignee untouched so
// the tier-3 pool query can select on --unassigned — this is the bridge
// source's state at the exact wake (gc prime / nudge) that is supposed to
// carry the agent into the molecule, before any claim has happened.
//
// Only a source bead a claim could take now bridges (claimableBridgeSources).
// The work query hands out no other routed bead, so a routed bead still
// waiting on a dependency is not the agent's assignment, and no step of its
// molecule is either.
//
// ListQuery.Metadata is single-valued, so identities are tried one at a time.
// Best-effort: a list error for one identity is skipped, not fatal, so a
// later identity still gets a chance.
//
// Returns nil when no routed bridge bead is found for any identity.
func resolveMoleculeRootViaRoutedBridge(workStore, graphStore beads.Store, assignees []string) *beads.Bead {
	for _, identity := range assignees {
		results, err := workStore.List(beads.ListQuery{
			Status:   "open",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: identity},
			TierMode: beads.TierBoth,
			Limit:    10,
		})
		if err != nil {
			continue
		}
		if root := firstMoleculeRootFrom(graphStore, claimableBridgeSources(workStore, results)); root != nil {
			return root
		}
	}
	return nil
}

// claimableBridgeSources returns, in order, the open routed candidates that
// carry a molecule_id and that a claim could take now: not deferred, not on a
// dispatch hold (beadmeta.HasDispatchHold, the hook's isHeldHookCandidate), and
// with every ready-blocking dependency satisfied. Readiness is read only for
// the candidates that would bridge.
func claimableBridgeSources(store beads.Store, candidates []beads.Bead) []beads.Bead {
	now := time.Now()
	var sources []beads.Bead
	for _, c := range candidates {
		if strings.TrimSpace(c.Metadata[beadmeta.MoleculeIDMetadataKey]) == "" || beads.IsDeferred(c, now) || beadmeta.HasDispatchHold(c.Labels) {
			continue
		}
		sources = append(sources, c)
	}
	return unblockedBeads(store, sources, 0)
}

// firstMoleculeRootFrom returns the molecule root reachable via molecule_id
// from the first candidate bead that carries one. Returns nil when no
// candidate carries a resolvable molecule_id.
func firstMoleculeRootFrom(graphStore beads.Store, candidates []beads.Bead) *beads.Bead {
	for i := range candidates {
		rootID := strings.TrimSpace(candidates[i].Metadata[beadmeta.MoleculeIDMetadataKey])
		if rootID == "" {
			continue
		}
		root, err := graphStore.Get(rootID)
		if err != nil {
			log.Printf("wisp step inject: molecule_id %q on bead %s did not resolve: %v", rootID, candidates[i].ID, err)
			continue
		}
		return &root
	}
	return nil
}

// resolveInProgressStepChild returns the in-progress type=step child of moleculeID.
// When multiple are found, the most recently updated one is returned.
func resolveInProgressStepChild(store beads.Store, moleculeID string) (*beads.Bead, error) {
	results, err := store.List(beads.ListQuery{
		Status:   "in_progress",
		Type:     "step",
		ParentID: moleculeID,
		TierMode: beads.TierBoth,
		Limit:    5,
	})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	if len(results) > 1 {
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.ID
		}
		log.Printf("wisp step inject: %d in-progress steps for molecule %s (%s); using most recent", len(results), moleculeID, strings.Join(ids, ", "))
	}
	best := results[0]
	for _, r := range results[1:] {
		if r.UpdatedAt.After(best.UpdatedAt) {
			best = r
		}
	}
	return &best, nil
}

// resolveEntryStepChild returns the entry step of moleculeID: the first open
// type=step child, in created order, whose ready-blocking dependencies are all
// satisfied. This is the fallback when no step is in-progress: the formula's
// entry position — where execution should (re)start, and the step bd mol
// current calls ready. Created order alone does not find it: a molecule's
// steps are created together, created_at is stored to the second, and a tie
// breaks by id, so the first open step in created order can be one still
// waiting on an earlier step (ga-qi3glw4). Returns nil when every open step is
// blocked.
func resolveEntryStepChild(store beads.Store, moleculeID string) (*beads.Bead, error) {
	results, err := store.List(beads.ListQuery{
		Status:   "open",
		Type:     "step",
		ParentID: moleculeID,
		TierMode: beads.TierBoth,
		Sort:     beads.SortCreatedAsc,
	})
	if err != nil {
		return nil, fmt.Errorf("resolving entry step for molecule %s: %w", moleculeID, err)
	}
	ready := unblockedBeads(store, results, 1)
	if len(ready) == 0 {
		return nil, nil
	}
	return &ready[0], nil
}

// unblockedBeads returns, in order, the open candidates whose ready-blocking
// dependencies are all satisfied (beads.DependencySatisfied) — the dependency
// half of bd ready's readiness — stopping after limit of them when limit > 0.
// A blocker that is itself a candidate is open, so it blocks without a read;
// any other is read once, and only while a candidate still needs its verdict.
// A read that fails counts as unsatisfied: the reminder is advisory, and
// naming work the agent cannot start as its assignment is what this check
// prevents.
func unblockedBeads(store beads.Store, candidates []beads.Bead, limit int) []beads.Bead {
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]string, len(candidates))
	satisfied := make(map[string]bool, len(candidates))
	for i, c := range candidates {
		ids[i] = c.ID
		satisfied[c.ID] = false
	}
	edges, err := downEdges(store, ids)
	if err != nil {
		log.Printf("wisp step inject: reading the dependencies of %s: %v", strings.Join(ids, ", "), err)
		return nil
	}
	var out []beads.Bead
	for _, c := range candidates {
		if !blockersSatisfied(store, edges[c.ID], satisfied) {
			continue
		}
		out = append(out, c)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// blockersSatisfied reports whether every ready-blocking edge in deps points at
// a bead that satisfies it. satisfied holds the verdicts known so far; a
// blocker it does not hold yet is read, and its verdict recorded.
func blockersSatisfied(store beads.Store, deps []beads.Dep, satisfied map[string]bool) bool {
	for _, dep := range deps {
		if !beads.IsReadyBlockingDependencyType(dep.Type) {
			continue
		}
		ok, known := satisfied[dep.DependsOnID]
		if !known {
			blocker, err := store.Get(dep.DependsOnID)
			ok = err == nil && beads.DependencySatisfied(blocker.Status, beads.ReadinessWorkOutcome(blocker.Metadata))
			satisfied[dep.DependsOnID] = ok
		}
		if !ok {
			return false
		}
	}
	return true
}

// downEdges reads the DOWN edges of every id: in one round trip when there is
// more than one and the store reads them in a batch, one DepList per id
// otherwise. An id the batch answer leaves out is read on its own, because an
// absent anchor is not proof of no edges: some stores omit an edge-free
// anchor, and bd answers a batch it refuses with an empty map
// (sourceDepReader.prefetch).
func downEdges(store beads.Store, ids []string) (map[string][]beads.Dep, error) {
	edges := make(map[string][]beads.Dep, len(ids))
	if batch, ok := beads.DepListBatchFor(store); ok && len(ids) > 1 {
		got, err := batch.DepListBatch(ids)
		if err != nil && !errors.Is(err, beads.ErrDepListBatchUnsupported) {
			return nil, err
		}
		for id, deps := range got {
			edges[id] = deps
		}
	}
	for _, id := range ids {
		if _, ok := edges[id]; ok {
			continue
		}
		deps, err := store.DepList(id, "down")
		if err != nil {
			return nil, err
		}
		edges[id] = deps
	}
	return edges, nil
}

// resolveBeadWithDescription returns the first in-progress bead assigned to any
// of the given identities that has a non-empty Description. This is the legacy
// resolution path used when no molecule bead is assigned to the agent. The
// ten candidates are the newest claimed work, first in created order newest
// first, so the answer does not depend on the store's default listing order.
func resolveBeadWithDescription(store beads.Store, assignees []string) (*beads.Bead, error) {
	results, err := store.List(beads.ListQuery{
		Status:    "in_progress",
		Assignees: assignees,
		TierMode:  beads.TierBoth,
		Limit:     claimedDescriptionSample,
		Sort:      beads.SortCreatedDesc,
	})
	if err != nil {
		return nil, err
	}
	return firstWithDescription(results), nil
}

// firstWithDescription returns the first bead with a non-empty description.
func firstWithDescription(results []beads.Bead) *beads.Bead {
	for i := range results {
		if strings.TrimSpace(results[i].Description) != "" {
			b := results[i]
			return &b
		}
	}
	return nil
}

// formatWispStepReminder formats a formula step bead as a <system-reminder>
// block for injection into agent context.
func formatWispStepReminder(b *beads.Bead) string {
	title := extmsg.SanitizeForSystemReminder(strings.TrimSpace(b.Title))
	desc := extmsg.SanitizeForSystemReminder(strings.TrimSpace(b.Description))
	return fmt.Sprintf(
		"<system-reminder>\nYour current active work assignment:\n\n## %s (%s)\n\n%s\n</system-reminder>\n",
		title, b.ID, desc,
	)
}

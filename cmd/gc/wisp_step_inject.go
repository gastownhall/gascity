package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"

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
//
// The controller holding those stores open is asked first, and any failure
// reads them in process, exactly as before (wispStepLookup). A hook that can
// start the lookup earlier, so it overlaps the hook's other work, holds a
// wispStepLookup instead of calling this.
func wispStepInjectionContent(cityPath string) string {
	return startWispStepLookup(cityPath, os.Stderr).content()
}

// wispStepCityPath is the city the step reminder reads: cityPath, else GC_CITY.
func wispStepCityPath(cityPath string) string {
	if cityPath != "" {
		return cityPath
	}
	return strings.TrimSpace(os.Getenv("GC_CITY"))
}

// wispStepInjectionContentInProcess is wispStepInjectionContent read from
// stores this process opens itself. effective is the city wispStepCityPath
// resolved.
func wispStepInjectionContentInProcess(effective string) string {
	assignees := wispStepAssignees()
	if len(assignees) == 0 {
		return ""
	}
	store := openWispStepStore(effective)
	if store == nil {
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
//     first open type=step child (deterministic formula start position).
//  4. If no molecule bead is assigned to the agent, follow the molecule_id
//     bridge: an attached (v1) formula routes only the source work bead and
//     stamps its molecule_id with the (unrouted, unassigned) root, so resolve
//     the root's active step through that bridge. Two bridge sources are
//     tried, claimed state first: (a) the source bead is already
//     in_progress+assigned — the state that exists once the agent has claimed
//     it; (b) the source bead is still open+unassigned but carries
//     gc.routed_to for one of the agent's identities — the state that exists
//     at the exact wake (gc prime / nudge) the bridge is meant to serve,
//     before any claim has happened. Trying (a) first prefers an
//     already-claimed molecule over a merely-routed one when both exist.
//  5. If no molecule_id bridge exists either, fall back to any in-progress bead
//     with a non-empty Description (legacy behavior for agents not running a
//     formula).
//
// Returns nil, nil when no bead can be resolved. Never returns an error for
// not-found conditions — callers treat nil as "nothing to inject".
//
// Both stores, because the resolution spans two classes: roots and type=step
// children are ClassGraph, the assignee-owned beads it starts from are work.
// Ambiguities are logged to the process log, which is the hook's stderr.
func resolveActiveWispStep(workStore, graphStore beads.Store, assignees []string) (*beads.Bead, error) {
	if workStore == nil || len(assignees) == 0 {
		return nil, nil
	}
	if graphStore == nil {
		graphStore = workStore
	}
	r := stepResolution{work: workStore, graph: graphStore, logf: log.Printf}
	return r.activeStep(sameStoreHandle(workStore, graphStore), assignees)
}

// activeStepReader is the read surface the step resolution needs from a store.
// The hook passes the stores it opened; the controller passes the live handles
// of the stores it holds open, so a read never answers from its cache.
type activeStepReader interface {
	Get(id string) (beads.Bead, error)
	List(query beads.ListQuery) ([]beads.Bead, error)
}

// stepResolution is one step resolution: the work store the identities' beads
// are read from, the graph store roots and steps are read from, and where an
// ambiguity is reported.
type stepResolution struct {
	work  activeStepReader
	graph activeStepReader
	logf  func(format string, args ...any)
	// molecules are the readers a molecule root and its steps are looked up
	// in, in order: the graph store, then — when the two are distinct stores —
	// the work store, because a v1 POURED molecule is work class and lives on
	// the work ledger. activeStep sets it; nil means the graph store alone.
	molecules []activeStepReader
}

// moleculeReaders returns the readers a molecule root and its steps may live
// in (see stepResolution.molecules).
func (r stepResolution) moleculeReaders() []activeStepReader {
	if len(r.molecules) > 0 {
		return r.molecules
	}
	return []activeStepReader{r.graph}
}

// activeStep is resolveActiveWispStep over r. oneStore says whether the work
// and graph stores are one store, which lets one listing answer every
// in-progress question; it is decided by the caller, which holds the stores
// themselves rather than readers over them.
func (r stepResolution) activeStep(oneStore bool, assignees []string) (*beads.Bead, error) {
	r.molecules = []activeStepReader{r.graph}
	if !oneStore && r.work != nil {
		r.molecules = append(r.molecules, r.work)
	}
	// One store holding both classes answers every "what is in progress for
	// these identities" question below from one listing; split classes ask
	// each store their own.
	var claimed *claimedWork
	if oneStore {
		rows, err := listClaimedWork(r.work, assignees)
		if err != nil {
			return nil, fmt.Errorf("listing in-progress work: %w", err)
		}
		claimed = &claimedWork{rows: rows}
	}

	var molecule *beads.Bead
	var err error
	molReader := r.graph
	if claimed != nil {
		molecule = claimed.activeMolecule(r.logf)
	} else if molecule, molReader, err = r.activeMoleculeIn(assignees); err != nil {
		return nil, err
	}
	if molecule == nil {
		// No molecule root is assigned to the agent. Attached (v1) formulas
		// leave the root unrouted and stamp molecule_id on the routed source
		// bead, so follow that bridge to the root's active step before the
		// legacy description fallback. Best-effort: a resolution error or no
		// bridge drops to legacy.
		var root *beads.Bead
		var rootReader activeStepReader
		if claimed != nil {
			root, rootReader = r.firstMoleculeRootIn(claimed.newest(claimedBridgeSample))
			if root == nil {
				root, rootReader = r.rootViaRoutedBridgeIn(assignees)
			}
		} else {
			root, rootReader = r.rootViaBridgeIn(assignees)
		}
		if root != nil {
			step, stepErr := r.inProgressStepChildIn(rootReader, root.ID)
			if stepErr != nil {
				r.logf("wisp step inject: error resolving in-progress step for bridged molecule %s: %v", root.ID, stepErr)
				return nil, nil
			}
			if step != nil {
				return step, nil
			}
			return r.entryStepChildIn(rootReader, root.ID)
		}
		// No molecule bridge; fall back to legacy: any in-progress bead with a description.
		if claimed != nil {
			return firstWithDescription(claimed.newest(claimedDescriptionSample)), nil
		}
		return r.beadWithDescription(assignees)
	}

	// Prefer the in-progress step child (the agent is mid-step).
	step, err := r.inProgressStepChildIn(molReader, molecule.ID)
	if err != nil {
		r.logf("wisp step inject: error resolving in-progress step children for molecule %s: %v", molecule.ID, err)
		return nil, nil
	}
	if step != nil {
		return step, nil
	}

	// Fall back to the entry step: first open step child.
	r.logf("wisp step inject: no in-progress step for molecule %s; resolving entry step", molecule.ID)
	return r.entryStepChildIn(molReader, molecule.ID)
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
func listClaimedWork(store activeStepReader, assignees []string) ([]beads.Bead, error) {
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

// activeMolecule is stepResolution.activeMolecule answered from the claimed
// work: of each root type in turn, the newest activeMoleculeSample roots, the
// most recently updated of them.
func (c *claimedWork) activeMolecule(logf func(format string, args ...any)) *beads.Bead {
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
		if best := mostRecentlyUpdatedMolecule(sample, molType, logf); best != nil {
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

// activeMolecule returns the agent's in-progress molecule bead.
// When multiple molecules are found, the most recently updated of the five
// newest is returned and the ambiguity is logged. The sample is cut in created
// order, newest first, so which five are compared does not depend on the
// store's default listing order. Returns nil, nil when none is found.
func (r stepResolution) activeMolecule(assignees []string) (*beads.Bead, error) {
	b, _, err := r.activeMoleculeIn(assignees)
	return b, err
}

// activeMoleculeIn is activeMolecule across every molecule reader, returning
// the reader that holds the molecule it picked. The sample per type is the
// newest activeMoleculeSample roots across the readers.
func (r stepResolution) activeMoleculeIn(assignees []string) (*beads.Bead, activeStepReader, error) {
	readers := r.moleculeReaders()
	for _, molType := range activeMoleculeTypes {
		var sample []beads.Bead
		owner := map[string]activeStepReader{}
		for _, reader := range readers {
			results, err := reader.List(beads.ListQuery{
				Status:    "in_progress",
				Type:      molType,
				Assignees: assignees,
				TierMode:  beads.TierBoth,
				Limit:     activeMoleculeSample,
				Sort:      beads.SortCreatedDesc,
			})
			if err != nil {
				return nil, nil, fmt.Errorf("listing in-progress %s beads: %w", molType, err)
			}
			for _, b := range results {
				if _, dup := owner[b.ID]; dup {
					continue
				}
				owner[b.ID] = reader
				sample = append(sample, b)
			}
		}
		if len(readers) > 1 {
			sort.SliceStable(sample, func(i, j int) bool { return sample[i].CreatedAt.After(sample[j].CreatedAt) })
			if len(sample) > activeMoleculeSample {
				sample = sample[:activeMoleculeSample]
			}
		}
		if best := mostRecentlyUpdatedMolecule(sample, molType, r.logf); best != nil {
			return best, owner[best.ID], nil
		}
	}
	return nil, nil, nil
}

// mostRecentlyUpdatedMolecule returns the most recently updated of the sampled
// roots of one type (the first on a tie), logging the ambiguity when there is
// more than one; nil for an empty sample.
func mostRecentlyUpdatedMolecule(results []beads.Bead, molType string, logf func(format string, args ...any)) *beads.Bead {
	if len(results) == 0 {
		return nil
	}
	if len(results) > 1 {
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.ID
		}
		logf("wisp step inject: %d in-progress %s beads found (%s); using most recent", len(results), molType, strings.Join(ids, ", "))
	}
	best := results[0]
	for _, r := range results[1:] {
		if r.UpdatedAt.After(best.UpdatedAt) {
			best = r
		}
	}
	return &best
}

// rootViaBridge finds the molecule root reachable from an attached (v1) source
// work bead. Attached formulas route only the source bead and stamp its
// molecule_id metadata with the (unrouted, unassigned) molecule root, so
// activeMolecule — which filters molecule roots by assignee — never matches.
// This bridges from the source bead to its root via the molecule_id metadata
// key, trying the claimed (in_progress+assigned) source state first and
// falling back to the pre-claim (open, gc.routed_to) state — see
// resolveActiveWispStep's doc comment, step 4.
//
// Returns nil on any error or when no bridge bead is found — callers treat nil
// as "no bridge available" and fall through to the legacy path.
//
// The bridge crosses a class boundary: the bead carrying the stamp is work
// class, the root it names is ClassGraph.
//
// The ten candidates are the newest claimed work, cut in created order so the
// sample does not depend on the store's default listing order.
func (r stepResolution) rootViaBridge(assignees []string) *beads.Bead {
	root, _ := r.rootViaBridgeIn(assignees)
	return root
}

// rootViaBridgeIn is rootViaBridge returning the reader that holds the root.
func (r stepResolution) rootViaBridgeIn(assignees []string) (*beads.Bead, activeStepReader) {
	results, err := r.work.List(beads.ListQuery{
		Status:    "in_progress",
		Assignees: assignees,
		TierMode:  beads.TierBoth,
		Limit:     claimedBridgeSample,
		Sort:      beads.SortCreatedDesc,
	})
	if err == nil {
		if root, reader := r.firstMoleculeRootIn(results); root != nil {
			return root, reader
		}
	}
	return r.rootViaRoutedBridgeIn(assignees)
}

// rootViaRoutedBridge finds the molecule root reachable from a source work
// bead that has been routed but not yet claimed: still status=open and
// unassigned, carrying gc.routed_to for one of the agent's identities.
// cliBeadRouter.Route (cmd/gc/cmd_sling.go) stamps gc.routed_to alone at sling
// time, deliberately leaving status and assignee untouched so the tier-3 pool
// query can select on --unassigned — this is the bridge source's state at the
// exact wake (gc prime / nudge) that is supposed to carry the agent into the
// molecule, before any claim has happened.
//
// ListQuery.Metadata is single-valued, so each identity is its own listing, and
// on a native store each is a scan of the open beads (about 0.37 s on
// maintainer-city's largest rig). The listings are independent reads, so they
// are asked at once; the roots are then taken in identity order, the first
// that resolves winning, exactly as one listing after another would take them.
// Best-effort: a list error for one identity is skipped, not fatal, so a later
// identity still gets a chance.
//
// Returns nil when no routed bridge bead is found for any identity.
func (r stepResolution) rootViaRoutedBridge(assignees []string) *beads.Bead {
	root, _ := r.rootViaRoutedBridgeIn(assignees)
	return root
}

// rootViaRoutedBridgeIn is rootViaRoutedBridge returning the reader that holds
// the root.
func (r stepResolution) rootViaRoutedBridgeIn(assignees []string) (*beads.Bead, activeStepReader) {
	type routedCandidates struct {
		rows []beads.Bead
		err  error
	}
	listed := make([]routedCandidates, len(assignees))
	var wg sync.WaitGroup
	for i, identity := range assignees {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := r.work.List(beads.ListQuery{
				Status:   "open",
				Metadata: map[string]string{beadmeta.RoutedToMetadataKey: identity},
				TierMode: beads.TierBoth,
				Limit:    10,
			})
			listed[i] = routedCandidates{rows: rows, err: err}
		}()
	}
	wg.Wait()
	for _, candidates := range listed {
		if candidates.err != nil {
			continue
		}
		if root, reader := r.firstMoleculeRootIn(candidates.rows); root != nil {
			return root, reader
		}
	}
	return nil, nil
}

// firstMoleculeRootIn returns the molecule root reachable via molecule_id from
// the first candidate bead that carries one, read across every molecule reader
// (graph first, then work), and the reader that holds it. Returns nil when no
// candidate carries a resolvable molecule_id.
func (r stepResolution) firstMoleculeRootIn(candidates []beads.Bead) (*beads.Bead, activeStepReader) {
	for i := range candidates {
		rootID := strings.TrimSpace(candidates[i].Metadata[beadmeta.MoleculeIDMetadataKey])
		if rootID == "" {
			continue
		}
		var lastErr error
		for _, reader := range r.moleculeReaders() {
			root, err := reader.Get(rootID)
			if err == nil {
				return &root, reader
			}
			lastErr = err
			if !errors.Is(err, beads.ErrNotFound) {
				break
			}
		}
		r.logf("wisp step inject: molecule_id %q on bead %s did not resolve: %v", rootID, candidates[i].ID, lastErr)
	}
	return nil, nil
}

// inProgressStepChildIn returns the in-progress type=step child of
// moleculeID, read from reader, the store that holds the molecule. When
// multiple are found, the most recently updated one is returned.
func (r stepResolution) inProgressStepChildIn(reader activeStepReader, moleculeID string) (*beads.Bead, error) {
	if reader == nil {
		reader = r.graph
	}
	results, err := reader.List(beads.ListQuery{
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
		r.logf("wisp step inject: %d in-progress steps for molecule %s (%s); using most recent", len(results), moleculeID, strings.Join(ids, ", "))
	}
	best := results[0]
	for _, r := range results[1:] {
		if r.UpdatedAt.After(best.UpdatedAt) {
			best = r
		}
	}
	return &best, nil
}

// entryStepChildIn returns the first open type=step child of moleculeID, read
// from reader, the store that holds the molecule. This is the deterministic
// fallback when no step is in-progress: the formula's entry position — where
// execution should (re)start.
func (r stepResolution) entryStepChildIn(reader activeStepReader, moleculeID string) (*beads.Bead, error) {
	if reader == nil {
		reader = r.graph
	}
	results, err := reader.List(beads.ListQuery{
		Status:   "open",
		Type:     "step",
		ParentID: moleculeID,
		TierMode: beads.TierBoth,
		Limit:    1,
		Sort:     beads.SortCreatedAsc,
	})
	if err != nil {
		return nil, fmt.Errorf("resolving entry step for molecule %s: %w", moleculeID, err)
	}
	if len(results) == 0 {
		return nil, nil
	}
	b := results[0]
	return &b, nil
}

// beadWithDescription returns the first in-progress bead assigned to any of
// the given identities that has a non-empty Description. This is the legacy
// resolution path used when no molecule bead is assigned to the agent. The
// ten candidates are the newest claimed work, first in created order newest
// first, so the answer does not depend on the store's default listing order.
func (r stepResolution) beadWithDescription(assignees []string) (*beads.Bead, error) {
	results, err := r.work.List(beads.ListQuery{
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

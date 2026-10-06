package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// executorIdentityResidueCheck is a gc doctor check that finds and clears
// stale executor-identity stamp residue left on beads: gc.session_name,
// gc.work_dir, and the legacy work_dir metadata keys can survive after a
// bead moves on from the executor that stamped them (design ref
// ga-cm2o5t.1, PR #6099; amended scope ga-6af29d decision 1).
type executorIdentityResidueCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newExecutorIdentityResidueCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *executorIdentityResidueCheck {
	return &executorIdentityResidueCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *executorIdentityResidueCheck) Name() string { return "executor-identity-residue" }

func (c *executorIdentityResidueCheck) CanFix() bool { return true }

type executorIdentityResidueFinding struct {
	label  string
	store  beads.Store
	beadID string
	// keys is the set of metadata keys the triggering condition(s) actually
	// named. Fix() clears exactly these keys -- never a fixed superset --
	// so a trigger that did not fire can never lose state it never
	// inspected.
	keys []string
	// identities is the executor-identity index this finding was judged
	// against, carried so Fix() can re-run the same predicate on the live
	// bead without rebuilding a different index and reaching a different
	// verdict than the one that produced the finding.
	identities executorRouteIdentityIndex
}

func (f executorIdentityResidueFinding) describe() string {
	return fmt.Sprintf("%s bead %s carries stale executor-identity stamp residue (%s)", f.label, f.beadID, strings.Join(f.keys, ", "))
}

func (c *executorIdentityResidueCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	findings, skipped := c.collect()
	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no stale executor-identity stamp residue found")
	}
	details := make([]string, 0, len(findings)+len(skipped))
	for _, f := range findings {
		details = append(details, f.describe())
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("executor-identity-residue check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	if len(skipped) > 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("%d bead(s) carry stale executor-identity stamp residue; %d scope(s) skipped", len(findings), len(skipped)),
			"run gc doctor --fix to clear the stale stamps, fix skipped store access, then rerun gc doctor",
			details)
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d bead(s) carry stale executor-identity stamp residue", len(findings)),
		"run gc doctor --fix to clear the stale gc.session_name/gc.work_dir/work_dir stamps, then rerun gc doctor",
		details)
}

func (c *executorIdentityResidueCheck) Fix(_ *doctor.CheckContext) error {
	findings, skipped := c.collect()
	var errs []error
	for _, f := range findings {
		if len(f.keys) == 0 {
			continue
		}
		// Re-read the bead immediately before writing. collect()'s listing is
		// a snapshot, and a worker -- usually in another process -- may have
		// claimed, closed, or re-routed the bead in the window since. A claim
		// atomically flips it open->in_progress and consumes gc.routed_to, so
		// clearing the snapshot's keys would strip identity metadata off work
		// in flight. Recompute the predicate on the live row and clear only
		// the keys that still fire; a bead that no longer qualifies is skipped
		// silently, the same guard sweepDetachedHandoffOrphans applies before
		// its own write.
		live, getErr := f.store.Get(f.beadID)
		if getErr != nil {
			errs = append(errs, fmt.Errorf("%s bead %s: re-read before clearing executor-identity stamp: %w", f.label, f.beadID, getErr))
			continue
		}
		liveKeys := staleExecutorIdentityStampKeys(c.cfg, live, f.identities)
		if len(liveKeys) == 0 {
			continue
		}
		clearKVs := make(map[string]string, len(liveKeys))
		for _, key := range liveKeys {
			clearKVs[key] = ""
		}
		if err := f.store.SetMetadataBatch(f.beadID, clearKVs); err != nil {
			errs = append(errs, fmt.Errorf("%s bead %s: clear executor-identity stamp: %w", f.label, f.beadID, err))
		}
	}
	if len(skipped) > 0 {
		errs = append(errs, fmt.Errorf("executor-identity-residue skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; ")))
	}
	return errors.Join(errs...)
}

func (c *executorIdentityResidueCheck) collect() (findings []executorIdentityResidueFinding, skipped []string) {
	if c.newStore == nil {
		return nil, nil
	}
	type residueScope struct {
		label string
		store beads.Store
	}
	var scopes []residueScope
	var cityStore beads.Store
	if strings.TrimSpace(c.cityPath) != "" {
		store, err := c.newStore(c.cityPath)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("city skipped: opening bead store: %v", err))
		} else {
			cityStore = store
			scopes = append(scopes, residueScope{label: "city", store: store})
		}
	}
	if c.cfg != nil {
		suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
		for _, rig := range c.cfg.Rigs {
			if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			label := "rig " + rig.Name
			store, err := c.newStore(rig.Path)
			if err != nil {
				skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", label, err))
				continue
			}
			scopes = append(scopes, residueScope{label: label, store: store})
		}
	}
	if len(scopes) == 0 {
		return nil, skipped
	}

	// Session beads belong to the session coordination class, which resolves to
	// the city store by default and follows a [beads.classes.sessions]
	// relocation otherwise -- never to a rig store. Build the executor-identity
	// index from that one store and share it with every scope, because a rig
	// store holds the claimed WORK beads this check inspects but none of the
	// session beads that say which identities legitimately act for a route.
	// Indexing each scope from its own listing leaves every rig with an empty
	// index, so every pool-slot and alias stamp on rig work reads as a re-route
	// hazard -- exactly the false positive the pool-instance and alias
	// stand-downs exist to prevent.
	//
	// Without a usable index this check cannot tell residue from a legitimate
	// identity, and its Fix() deletes state. So an index that cannot be built
	// stands every scope down rather than scanning blind.
	if cityStore == nil {
		skipped = append(skipped, "all scopes skipped: session-identity index unavailable: city bead store did not open")
		return nil, skipped
	}
	sessionStore := cliSessionStore(cityStore, c.cfg, c.cityPath)
	sessionIdentities, err := buildExecutorRouteIdentityIndex(sessionStore)
	if err != nil {
		skipped = append(skipped, fmt.Sprintf("all scopes skipped: building session-identity index: %v", err))
		return nil, skipped
	}

	for _, sc := range scopes {
		identities := sessionIdentities
		// Union in a DISTINCT scope store's own session beads, so a rig that
		// does hold session records still contributes them. Interface identity
		// is the right test -- production stores are pointer-backed
		// CachingStores -- and it keeps the default single-store city from
		// scanning the same rows twice. The union is built into the scope's own
		// index rather than into the shared one, so one scope's session beads
		// can never leak into the next scope's verdict.
		if sc.store != sessionStore {
			scopeIdentities, idxErr := buildExecutorRouteIdentityIndex(sc.store)
			if idxErr != nil {
				skipped = append(skipped, fmt.Sprintf("%s skipped: listing session beads: %v", sc.label, idxErr))
				continue
			}
			scopeIdentities.backfill(sessionIdentities)
			identities = scopeIdentities
		}
		scopeFindings, listErr := c.collectStoreFindings(sc.store, sc.label, identities)
		findings = append(findings, scopeFindings...)
		if listErr != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", sc.label, listErr))
		}
	}
	return findings, skipped
}

func (c *executorIdentityResidueCheck) collectStoreFindings(store beads.Store, label string, routeIdentities executorRouteIdentityIndex) ([]executorIdentityResidueFinding, error) {
	items, err := store.List(beads.ListQuery{Status: "open", AllowScan: true, Live: true})
	if err != nil {
		return nil, err
	}
	var findings []executorIdentityResidueFinding
	for _, bd := range items {
		keys := staleExecutorIdentityStampKeys(c.cfg, bd, routeIdentities)
		if len(keys) == 0 {
			continue
		}
		findings = append(findings, executorIdentityResidueFinding{label: label, store: store, beadID: bd.ID, keys: keys, identities: routeIdentities})
	}
	return findings, nil
}

// executorRouteIdentityIndex maps a route to the full set of executor
// identities that legitimately act for it. The base session name a plain
// SessionNameFor(route) encoding would produce is only one member of that
// set when the route runs a pool — each pool slot mints its own concrete
// session name (sessionBeadIdentifier semantics) while still belonging to
// the same route (retiredSessionFallbackRoute semantics). Built fresh from
// the session-class store's session beads on every check run, never
// persisted, so it always reflects the pool's current membership.
type executorRouteIdentityIndex map[string]map[string]struct{}

// add records identity as a legitimate executor for route, ignoring a pair
// with an empty half.
func (idx executorRouteIdentityIndex) add(route, identity string) {
	if route == "" || identity == "" {
		return
	}
	if idx[route] == nil {
		idx[route] = make(map[string]struct{})
	}
	idx[route][identity] = struct{}{}
}

// backfill copies every route/identity pair from other that idx does not
// already carry, mirroring pool_detached_orphan_sweep.go's
// detachedOrphanRouteIndex.backfill. Membership here is a set per route
// rather than a single value, so the union is taken pair by pair: a route
// both indexes know keeps both their identities instead of one shadowing
// the other.
func (idx executorRouteIdentityIndex) backfill(other executorRouteIdentityIndex) {
	for route, identities := range other {
		for identity := range identities {
			idx.add(route, identity)
		}
	}
}

// buildExecutorRouteIdentityIndex indexes store's session beads by route
// (retiredSessionFallbackRoute) and identity (sessionBeadIdentifier), the
// inverse direction of pool_detached_orphan_sweep.go's
// detachedOrphanRouteIndex (session_name -> route, single-valued).
//
// Rows come from the session front door's ListAll, the canonical enumeration:
// it unions the type leg with the label leg, so a crash- or migration-damaged
// session bead that kept its gc:session label but lost its type still
// contributes its identity. Closed session beads are included for the same
// reason the sibling index includes them — the worker session is usually
// already gone by the time a sweep runs, and dropping its bead would make
// every stamp it left read as residue. Route and identity are read off the
// typed Info projection via the retiredSessionFallbackRouteInfo /
// sessionBeadIdentifierInfo mirrors, which are byte-identical to their raw-bead
// forms, so no session bead is cracked open here.
//
// A partial listing yields usable rows and is used as-is; only a hard error
// is returned, because an empty index cannot distinguish residue from a
// legitimate identity and this check's Fix() deletes state.
func buildExecutorRouteIdentityIndex(store beads.Store) (executorRouteIdentityIndex, error) {
	idx := make(executorRouteIdentityIndex)
	all, listErr := sessionFrontDoor(store).ListAll(session.ListAllOptions{IncludeClosed: true})
	if listErr != nil && !beads.IsPartialResult(listErr) {
		return nil, fmt.Errorf("listing session beads: %w", listErr)
	}
	for _, info := range all {
		idx.add(strings.TrimSpace(retiredSessionFallbackRouteInfo(info)), strings.TrimSpace(sessionBeadIdentifierInfo(info)))
	}
	return idx, nil
}

func (idx executorRouteIdentityIndex) legitimate(route, identity string) bool {
	if route == "" || identity == "" {
		return false
	}
	_, ok := idx[route][identity]
	return ok
}

// staleExecutorIdentityStampKeys reports which metadata keys on bd carry
// stale executor-identity stamp residue, or nil if none. Closed and
// in_progress beads are always out of scope, as is workflow topology (a run
// root, scope latch, or formula spec is never itself claimed — only its
// descendant steps are — so a completed step's visibility stamp copied onto
// the root must not read as residue even when it no longer matches the
// root's own gc.routed_to).
//
// Two independent triggers follow, each contributing only the key(s) it
// actually fired on to the result — see staleWorkDirStamp and
// staleSessionNameStamp for the trigger conditions and stand-downs. Keeping
// the two disjoint is load-bearing: Fix() clears exactly the returned keys,
// so a trigger that did not fire can never lose state it never inspected.
func staleExecutorIdentityStampKeys(cfg *config.City, bd beads.Bead, routeIdentities executorRouteIdentityIndex) []string {
	if bd.Status == "closed" {
		return nil
	}
	if bd.Status == "in_progress" {
		return nil
	}
	if graphroute.IsWorkflowTopologyKind(bd.Metadata[beadmeta.KindMetadataKey]) {
		return nil
	}

	var keys []string
	if staleWorkDirStamp(cfg, bd) {
		keys = append(keys, beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey)
	}
	if staleSessionNameStamp(cfg, bd, routeIdentities) {
		keys = append(keys, beadmeta.SessionNameMetadataKey)
	}
	return keys
}

// staleWorkDirStamp reports whether bd's legacy work_dir disagrees with its
// canonical gc.work_dir in a way that is genuine residue, rather than a
// repair candidate or an actively worktree-owning bead.
//
// Two stand-downs guard against clearing state a downstream fail-closed
// check relies on:
//
//   - hasWorktreeOwnershipEvidence: worktreeSpecForBead treats a bead
//     carrying worktree ownership metadata as actively managed and fails
//     closed on a canonical/legacy disagreement for it. Clearing both keys
//     here would erase the very evidence that check inspects, silently
//     downgrading its fail-closed conflict error into a "no spec, unmanaged"
//     no-op — the ga-6af29d/#6135 round-2 regression this stand-down closes.
//   - poolSlotWorkDirRepairFor(cfg, bd) != nil: this shape (canonical
//     clobbered with a pool-slot label, legacy still holding real per-bead
//     evidence) is a repair candidate the reconciler's own one-shot sweep
//     restores from legacy. Flagging it here races that repair for the same
//     keys and, if this check's Fix() wins, blanks both instead of
//     restoring the canonical.
func staleWorkDirStamp(cfg *config.City, bd beads.Bead) bool {
	workDir := strings.TrimSpace(bd.Metadata[beadmeta.WorkDirMetadataKey])
	legacyWorkDir := strings.TrimSpace(bd.Metadata[beadmeta.LegacyWorkDirMetadataKey])
	if workDir == "" || legacyWorkDir == "" || workDir == legacyWorkDir {
		return false
	}
	if hasWorktreeOwnershipEvidence(bd) {
		return false
	}
	if poolSlotWorkDirRepairFor(cfg, bd) != nil {
		return false
	}
	return true
}

// hasWorktreeOwnershipEvidence reports whether bd publishes any of the
// worktree-ownership metadata keys worktreeSpecForBead inspects to tell a
// managed per-bead worktree apart from an unmanaged spawn.
func hasWorktreeOwnershipEvidence(bd beads.Bead) bool {
	for _, key := range []string{
		beadmeta.WorktreeRootMetadataKey,
		beadmeta.WorktreeRepoMetadataKey,
		beadmeta.WorktreeOwnerMetadataKey,
	} {
		if strings.TrimSpace(bd.Metadata[key]) != "" {
			return true
		}
	}
	return false
}

// staleSessionNameStamp reports whether bd carries a stale gc.session_name:
// a non-empty stamp on a bead with a non-empty gc.routed_to (an empty route
// is the ordinary post-claim/detached-orphan state, not residue) whose
// stamped session name is neither what the bead's CURRENT gc.routed_to would
// mint today — via agent.SessionNameFor, honoring any configured
// session_template, forward-encoded on every call, never against a fixed
// snapshot — nor a member of that route's legitimate executor-identity set
// (routeIdentities; a pool slot's own concrete session name is a legitimate
// stamp against its base route).
func staleSessionNameStamp(cfg *config.City, bd beads.Bead, routeIdentities executorRouteIdentityIndex) bool {
	sessionName := strings.TrimSpace(bd.Metadata[beadmeta.SessionNameMetadataKey])
	if sessionName == "" {
		return false
	}
	routedTo := strings.TrimSpace(bd.Metadata[beadmeta.RoutedToMetadataKey])
	if routedTo == "" {
		return false
	}
	var sessionTemplate string
	var cityName string
	if cfg != nil {
		sessionTemplate = cfg.Workspace.SessionTemplate
		cityName = cfg.EffectiveCityName()
	}
	expected := agent.SessionNameFor(cityName, routedTo, sessionTemplate)
	if expected == sessionName {
		return false
	}
	return !routeIdentities.legitimate(routedTo, sessionName)
}

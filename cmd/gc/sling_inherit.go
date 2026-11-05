package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sling"
)

// slingCallerIdentityEnv names the calling session the way the default
// work_query does: its bead id, its session name, and its alias. A claim
// assigned to any of them is one of the caller's claims.
var slingCallerIdentityEnv = []string{"GC_SESSION_ID", "GC_SESSION_NAME", "GC_ALIAS"}

// slingInheritReader is the read surface inheritance needs from a store that
// can hold a claim, the convoys that track it, or a run root.
type slingInheritReader interface {
	Get(id string) (beads.Bead, error)
	List(query beads.ListQuery) ([]beads.Bead, error)
	DepList(id, direction string) ([]beads.Dep, error)
}

// slingInheritSources is what initiator inheritance reads: the caller's
// assignee identities, the session bead's current-claim pointer, and the
// stores claims and run roots can live in.
type slingInheritSources struct {
	// sessionID is the caller's session bead id (GC_SESSION_ID), "" when the
	// caller has none.
	sessionID string
	// identities are the assignees whose in_progress beads are the caller's
	// claims.
	identities []string
	// currentClaim returns the session bead's current_claim_bead_id ("" when
	// unset or when the session has no bead). Nil means no session bead.
	currentClaim func() (string, error)
	// stores are searched in order for claims and run roots: the relocated
	// graph binding first (when there is one), then the city and rig stores.
	stores []slingInheritReader
}

// slingClaimStores opens the stores a session's claims and their run roots can
// live in: the relocated graph binding first, then the city and rig stores.
// skipped lists every candidate store that did not open.
type slingClaimStores func() (stores []sling.SourceWorkflowStore, skipped []sourceWorkflowStoreSkip, err error)

// slingClaimStoresFor turns openViews, the opener of a local sling's own
// source-workflow stores, into its claim stores: the relocated graph binding
// first, then the city and rig stores that opened. The singleton scan over the
// same stores tolerates one that did not open; this opener hands every such
// store on, because a claim in it could name another run.
func slingClaimStoresFor(cfg *config.City, cityPath, cityName string, openViews func() ([]convoyStoreView, []sourceWorkflowStoreSkip, error)) slingClaimStores {
	return func() ([]sling.SourceWorkflowStore, []sourceWorkflowStoreSkip, error) {
		views, skipped, err := openViews()
		if err != nil {
			return nil, skipped, err
		}
		stores, err := sourceWorkflowStoresFromViews(cfg, cityPath, cityName, views)
		return stores, skipped, err
	}
}

// inheritSlingInitiator gives a local `gc sling` run from inside a session the
// gc.initiator of the work that session has claimed, so follow-up work an agent
// launches stays attributed to whoever launched that work. It never overrides
// an initiator already on opts and is a no-op outside a session. It inherits
// only when every claim the caller holds carries the same initiator, read from
// the open run the claim is the input of when the caller also holds that run's
// work, else from the claim itself when it carries one, else from its run root
// while that run is open.
// Otherwise it stamps nothing and leaves a one-line warning on stderr: when
// the claims carry different initiators, when one carries an initiator and
// another none, when a read fails, and when a claim store did not open.
// claimStores opens the stores it reads (slingClaimStoresFor).
func inheritSlingInitiator(opts *slingOpts, claimStores slingClaimStores, getenv func(string) string, stderr io.Writer) {
	if strings.TrimSpace(opts.Initiator) != "" {
		return
	}
	sessionID := strings.TrimSpace(getenv("GC_SESSION_ID"))
	if sessionID == "" && strings.TrimSpace(getenv("GC_SESSION_NAME")) == "" {
		return
	}
	src := slingInheritSources{sessionID: sessionID, currentClaim: slingSessionCurrentClaim(sessionID)}
	for _, k := range slingCallerIdentityEnv {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			src.identities = append(src.identities, v)
		}
	}
	if claimStores == nil {
		warnSlingInitiatorNotInherited(stderr, "no claim stores are available")
		return
	}
	stores, skipped, err := claimStores()
	if err != nil {
		warnSlingInitiatorNotInherited(stderr, fmt.Sprintf("opening the claim stores: %v", err))
		return
	}
	if len(skipped) > 0 {
		warnSlingInitiatorNotInherited(stderr, slingClaimStoreSkipsReason(skipped))
		return
	}
	for _, s := range stores {
		src.stores = append(src.stores, s.Store)
	}
	opts.Initiator = resolveInheritedInitiator(src, stderr)
}

// slingClaimStoreSkipsReason says why inheritance read nothing when some claim
// stores did not open: the claims in them are invisible, and one could name a
// run whose initiator contradicts the claims that are visible.
func slingClaimStoreSkipsReason(skipped []sourceWorkflowStoreSkip) string {
	parts := make([]string, 0, len(skipped))
	for _, skip := range skipped {
		parts = append(parts, fmt.Sprintf("%s (%v)", skip.path, skip.err))
	}
	return fmt.Sprintf("%d claim store(s) did not open, and a claim there could name another run: %s", len(skipped), strings.Join(parts, "; "))
}

// slingSessionCurrentClaim reads the session bead's current_claim_bead_id
// through the same session front door `gc hook current` reads it through. A
// session id that names no session bead has no current claim.
func slingSessionCurrentClaim(sessionID string) func() (string, error) {
	if sessionID == "" {
		return nil
	}
	return func() (string, error) {
		front, err := sessionCurrentClaimFrontDoor()
		if err != nil {
			return "", err
		}
		id, err := front.CurrentClaimBeadID(sessionID)
		if errors.Is(err, beads.ErrNotFound) || errors.Is(err, session.ErrSessionNotFound) {
			return "", nil
		}
		return id, err
	}
}

// slingInheritClaim is one of the caller's claims and the store it was read from.
type slingInheritClaim struct {
	bead beads.Bead
	home slingInheritReader
}

// slingInheritRoot is the bead a claim's gc.initiator is read from, and its id.
type slingInheritRoot struct {
	id   string
	bead beads.Bead
}

// resolveInheritedInitiator collects the caller's claims, resolves each to its
// run root (resolveSlingInheritRoots), and returns the gc.initiator those
// roots carry when they all carry the same one. It returns "" when none
// carries one, and "" with a warning when a read fails or the roots disagree.
// A root that carries no initiator disagrees with one that does: the session
// may be doing that run's work, so it inherits no other run's initiator over
// it.
func resolveInheritedInitiator(src slingInheritSources, stderr io.Writer) string {
	claims, err := collectSlingInheritClaims(src)
	if err != nil {
		warnSlingInitiatorNotInherited(stderr, err.Error())
		return ""
	}
	resolved, err := resolveSlingInheritRoots(src.stores, claims)
	if err != nil {
		warnSlingInitiatorNotInherited(stderr, err.Error())
		return ""
	}
	rootsByInitiator := map[string][]string{}
	var unattributed []string
	for _, root := range resolved {
		initiator := strings.TrimSpace(root.bead.Metadata[beadmeta.InitiatorMetadataKey])
		if initiator == "" {
			if !containsString(unattributed, root.id) {
				unattributed = append(unattributed, root.id)
			}
			continue
		}
		if !containsString(rootsByInitiator[initiator], root.id) {
			rootsByInitiator[initiator] = append(rootsByInitiator[initiator], root.id)
		}
	}
	switch {
	case len(rootsByInitiator) == 0:
		return ""
	case len(rootsByInitiator) == 1 && len(unattributed) == 0:
		for initiator := range rootsByInitiator {
			return initiator
		}
	}
	parts := make([]string, 0, len(rootsByInitiator)+1)
	for initiator, roots := range rootsByInitiator {
		parts = append(parts, fmt.Sprintf("%s=%s", strings.Join(roots, ","), initiator))
	}
	sort.Strings(parts)
	if len(unattributed) > 0 {
		parts = append(parts, strings.Join(unattributed, ",")+"=(none)")
	}
	warnSlingInitiatorNotInherited(stderr, "the session's claimed runs have different initiators ("+strings.Join(parts, "; ")+")")
	return ""
}

// collectSlingInheritClaims returns the caller's claims, each with the store it
// was read from: the session's current claim while the session still holds it
// (claimedWorkStillHeldBy), then every in_progress bead assigned to one of its
// identities in each claim store. A current-claim pointer outlives a release
// or a reclaim by another session until the session's next claim.
func collectSlingInheritClaims(src slingInheritSources) ([]slingInheritClaim, error) {
	var claims []slingInheritClaim
	seen := map[string]bool{}
	add := func(b beads.Bead, home slingInheritReader) {
		if seen[b.ID] {
			return
		}
		seen[b.ID] = true
		claims = append(claims, slingInheritClaim{bead: b, home: home})
	}
	if src.currentClaim != nil {
		id, err := src.currentClaim()
		if err != nil {
			return nil, fmt.Errorf("reading the session's current claim: %w", err)
		}
		if id != "" {
			b, home, found, err := getSlingInheritBead(src.stores, nil, id)
			if err != nil {
				return nil, err
			}
			if found && claimedWorkStillHeldBy(b, src.sessionID) {
				add(b, home)
			}
		}
	}
	if len(src.identities) == 0 {
		return claims, nil
	}
	for _, store := range src.stores {
		// FederatedReadTier on every leg: a relocated class store answers at
		// exactly the queried tier, so the zero value would drop its ephemeral
		// claims while the policy-wrapped legs still return theirs.
		listed, err := store.List(beads.ListQuery{Status: "in_progress", Assignees: src.identities, TierMode: beads.FederatedReadTier})
		if err != nil {
			return nil, fmt.Errorf("listing the session's claims: %w", err)
		}
		for _, b := range listed {
			add(b, store)
		}
	}
	return claims, nil
}

// resolveSlingInheritRoots resolves each claim to the bead its gc.initiator is
// read from: its run root (slingInheritRunRoot), except that a claim that
// resolves to itself there counts as the open run whose input it is when the
// caller also holds work that resolves to that run. Core mol-do-work claims
// the work bead a graph.v2 --on launch tracks through its input convoy, and
// an item formula can claim the member its drain item tracks the same way.
// Neither launch chains that bead to its run or stamps it, and a stamp it
// does carry is usually its creator's, so the run's initiator wins over it.
func resolveSlingInheritRoots(stores []slingInheritReader, claims []slingInheritClaim) ([]slingInheritRoot, error) {
	roots := make([]slingInheritRoot, 0, len(claims))
	for _, c := range claims {
		id, bead, err := slingInheritRunRoot(stores, c)
		if err != nil {
			return nil, err
		}
		roots = append(roots, slingInheritRoot{id: id, bead: bead})
	}
	runs := slingInheritRunsByInputConvoy(claims, roots)
	if len(runs) == 0 {
		return roots, nil
	}
	for i, c := range claims {
		if roots[i].id != c.bead.ID {
			continue
		}
		run, found, err := slingInheritInputRun(c, runs)
		if err != nil {
			return nil, err
		}
		if found {
			roots[i] = run
		}
	}
	return roots, nil
}

// slingInheritRunsByInputConvoy indexes the open runs the claims resolved to
// through a run chain (roots[i] is claims[i]'s) by each run's input convoy
// (gc.input_convoy_id): the convoy that tracks the bead a graph.v2 --on launch
// was slung on, or the member a drain item works on.
func slingInheritRunsByInputConvoy(claims []slingInheritClaim, roots []slingInheritRoot) map[string]slingInheritRoot {
	runs := map[string]slingInheritRoot{}
	for i, c := range claims {
		convoyID := strings.TrimSpace(roots[i].bead.Metadata[beadmeta.InputConvoyIDMetadataKey])
		if roots[i].id != c.bead.ID && convoyID != "" {
			runs[convoyID] = roots[i]
		}
	}
	return runs
}

// slingInheritInputRun returns the run in runs whose input convoy tracks claim
// c. The tracking edges are read from c's own store, which holds every convoy
// that tracks c (convoycore.TrackItemIn keeps a convoy beside its members).
func slingInheritInputRun(c slingInheritClaim, runs map[string]slingInheritRoot) (slingInheritRoot, bool, error) {
	deps, err := c.home.DepList(c.bead.ID, "up")
	if err != nil {
		return slingInheritRoot{}, false, fmt.Errorf("listing the convoys tracking %s: %w", c.bead.ID, err)
	}
	for _, dep := range deps {
		if run, ok := runs[dep.IssueID]; ok && dep.Type == convoycore.TrackingDepType {
			return run, true, nil
		}
	}
	return slingInheritRoot{}, false, nil
}

// slingInheritRunRoot resolves claim c, by its own metadata, to the bead its
// initiator is read from. That is c itself when c carries its own
// gc.initiator: an attributed sling stamps the bead it routes before the bead
// can be claimed, while a re-slung bead keeps the run chain of an earlier,
// closed attachment. It is also c itself when c names no run, and when the run
// c names is closed: a closed run no longer holds c, and a sling with no
// initiator neither stamps the bead it re-slings nor clears that stale chain,
// so c counts as unattributed. Otherwise it is c's run root
// (beadmeta.ResolveRunID), read from c's own store first, then from each claim
// store; graph and drain steps carry no stamp of their own. A root that no
// store holds is an error, like a failed read. resolveSlingInheritRoots can
// still count a claim this resolves to itself as the run it is the input of.
func slingInheritRunRoot(stores []slingInheritReader, c slingInheritClaim) (string, beads.Bead, error) {
	if strings.TrimSpace(c.bead.Metadata[beadmeta.InitiatorMetadataKey]) != "" {
		return c.bead.ID, c.bead, nil
	}
	rootID := beadmeta.ResolveRunID(c.bead.Metadata, c.bead.ID, "")
	if rootID == c.bead.ID {
		return rootID, c.bead, nil
	}
	root, _, found, err := getSlingInheritBead(stores, c.home, rootID)
	if err != nil {
		return "", beads.Bead{}, err
	}
	if !found {
		return "", beads.Bead{}, fmt.Errorf("run root %s of claim %s was not found", rootID, c.bead.ID)
	}
	if root.Status == "closed" {
		return c.bead.ID, c.bead, nil
	}
	return rootID, root, nil
}

// getSlingInheritBead reads id from home first, then from each store in order,
// reporting found=false when every store cleanly misses. Any other read error
// is returned: it is a failed read, not an absence.
func getSlingInheritBead(stores []slingInheritReader, home slingInheritReader, id string) (beads.Bead, slingInheritReader, bool, error) {
	order := stores
	if home != nil {
		order = append([]slingInheritReader{home}, stores...)
	}
	for _, store := range order {
		b, err := store.Get(id)
		if err == nil {
			return b, store, true, nil
		}
		if !errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, nil, false, fmt.Errorf("reading %s: %w", id, err)
		}
	}
	return beads.Bead{}, nil, false, nil
}

// warnSlingInitiatorNotInherited reports, on one stderr line, why an in-session
// sling carries no inherited initiator.
func warnSlingInitiatorNotInherited(stderr io.Writer, reason string) {
	fmt.Fprintf(stderr, "gc sling: warning: not inheriting %s: %s\n", beadmeta.InitiatorMetadataKey, reason) //nolint:errcheck // best-effort stderr
}

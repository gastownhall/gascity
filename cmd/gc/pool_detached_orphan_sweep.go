package main

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// sweepDetachedHandoffOrphans restores gc.routed_to on work beads that were
// fully detached by a failed done sequence. When a worker clears both the
// assignee and gc.routed_to in a single atomic update (e.g. because
// $REFINERY_TARGET resolved empty), the bead is left open+unassigned+unrouted
// with a branch already on origin — invisible to both the pool demand probe
// (which keys on gc.routed_to) and releaseOrphanedPoolAssignments (which only
// processes assigned work). This sweep finds such beads via gc.session_name →
// session bead → template and re-stamps gc.routed_to, returning each bead to
// pool demand. Returns the count of restored beads.
//
// Recovery is judgment-free (ZFC): it reads the original pool route from the
// session bead's own template metadata and re-stamps gc.routed_to. The bead
// re-enters pool demand; the formula re-evaluates it from there. No role names
// appear in this function.
func sweepDetachedHandoffOrphans(store beads.Store) (int, error) {
	result, err := sweepDetachedHandoffOrphansWithRouteStore(store, nil)
	return result.restored, err
}

// detachedOrphanSweepResult is one leg's convergence outcome, in the terms the
// pass's operator line reports: what it found, what it repaired, and what the
// answer cost in store round trips.
type detachedOrphanSweepResult struct {
	candidates int
	restored   int
	reads      int
}

// sweepDetachedHandoffOrphansWithRouteStore is sweepDetachedHandoffOrphans that
// additionally resolves pool routes from routeStore. Session beads (which carry
// the template/route) are class-stored, while a detached orphan can live in a rig
// store — so when sweeping a rig store, routeStore is the session-class store,
// and a rig-stored orphan whose closing session bead lives there is recovered.
// routeStore may be nil to resolve routes from store alone. Beads are only
// re-stamped in store; routeStore is read-only.
//
// This is the CONVERGENCE form: it finds its own candidates with a full live
// open-corpus scan. It is one leg of the off-tick backstop pass
// (detachedOrphanLane.backstopPass), never the tick — that scan was 180.8s of a
// 373s tick (ga-l7jdg). It reports what it found and what the answer cost, so a
// pass that repaired nothing can be told from one that looked at nothing.
func sweepDetachedHandoffOrphansWithRouteStore(store, routeStore beads.Store) (detachedOrphanSweepResult, error) {
	var result detachedOrphanSweepResult
	if store == nil {
		return result, nil
	}
	// Scan open beads for detached handoff orphans. Live is what makes
	// Status:"open" mean open: mapBdStatus folds bd's blocked/deferred/review/
	// testing into Gas City's three statuses, so such a bead decodes with Status
	// "open", and a cached List (which filters the collapsed status via
	// ListQuery.Matches) hands it back as if it were ready. Only the backing store
	// filters on the raw status, so without Live a bead parked in bd review/
	// testing with a pushed branch and a consumed gc.routed_to — an ordinary
	// post-work state — is re-stamped every tick and respawns a worker that drains
	// no-op (the sibling restoreCarriedWorkRoutes gates the same gc-4zb hazard with
	// Live). In steady state there are no candidates, so the expensive session-
	// index lookup is skipped entirely.
	items, err := store.List(beads.ListQuery{Status: "open", AllowScan: true, Live: true})
	result.reads++
	if err != nil {
		return result, fmt.Errorf("listing open beads: %w", err)
	}

	type candidate struct {
		id, sessionID, sessionName string
	}
	var candidates []candidate
	for _, b := range items {
		if !isDetachedHandoffOrphanCandidate(b) {
			continue
		}
		candidates = append(candidates, candidate{
			id:          b.ID,
			sessionID:   strings.TrimSpace(b.Metadata[beadmeta.SessionIDMetadataKey]),
			sessionName: strings.TrimSpace(b.Metadata[beadmeta.SessionNameMetadataKey]),
		})
	}
	result.candidates = len(candidates)
	if len(candidates) == 0 {
		return result, nil
	}

	routeIndex, indexReads, indexErr := detachedOrphanRoutesFor(store, routeStore)
	result.reads += indexReads
	if indexErr != nil {
		return result, indexErr
	}

	// Resolve the authoritative, cache-bypassing read handle once. Production
	// stores are CachingStore-wrapped, so a plain store.Get can return a bead that
	// predates a cross-process claim/close; handles.Live reads the backing store
	// directly. For a plain store this degrades to store.Get.
	handles := beads.HandlesFor(store)
	var errs []error
	for _, c := range candidates {
		route := routeIndex.route(c.sessionID, c.sessionName)
		if route == "" {
			log.Printf("sweepDetachedHandoffOrphans: no recoverable route for bead %s (gc.session_id=%q / gc.session_name=%q not found in any session bead, the session carries no template, or the session name is ambiguous)", c.id, c.sessionID, c.sessionName)
			continue
		}
		// Re-read the live bead immediately before writing, through the cache-
		// bypassing handle. The open-bead List is a snapshot; a worker — often in
		// another process — may have claimed, closed, or re-routed this bead in the
		// window since. A claim atomically flips it open->in_progress and consumes
		// gc.routed_to (ga-sa0), so a blind SetMetadata keyed on the stale snapshot
		// would resurrect gc.routed_to on the now-claimed bead and hand the
		// dispatcher a phantom pool-demand bead that flaps open<->in_progress
		// (ga-bgu). Skip unless the live bead is still a detached-orphan candidate
		// resolving to the same recovered route. (A block collapses to "open" on
		// this Get too, so the Live candidate List above — not this re-read — is
		// what excludes blocked/review/testing work; gc-4zb.)
		live, getErr := handles.Live.Get(c.id)
		result.reads++
		if getErr != nil {
			errs = append(errs, fmt.Errorf("bead %s: re-reading before route restore: %w", c.id, getErr))
			continue
		}
		if !isDetachedHandoffOrphanCandidate(live) ||
			routeIndex.route(strings.TrimSpace(live.Metadata[beadmeta.SessionIDMetadataKey]), strings.TrimSpace(live.Metadata[beadmeta.SessionNameMetadataKey])) != route {
			continue // claimed, closed, or re-routed since the snapshot — don't clobber
		}
		result.reads++
		if setErr := store.SetMetadata(c.id, beadmeta.RoutedToMetadataKey, route); setErr != nil {
			errs = append(errs, fmt.Errorf("bead %s: restoring gc.routed_to=%q: %w", c.id, route, setErr))
			continue
		}
		log.Printf("sweepDetachedHandoffOrphans: restored gc.routed_to=%q on detached handoff orphan %s", route, c.id)
		result.restored++
	}
	return result, errors.Join(errs...)
}

// detachedOrphanRoutesFor builds the session route index one pass over one leg
// uses, from that leg's own session beads unioned with routeStore's.
//
// Both lanes resolve routes through here, so the tick's delta pass and the
// off-tick scan can never disagree about which session bead answers for an
// orphan — the reader-agreement rule that makes the delta pass a cheaper form of
// the same question rather than a second, quieter one.
//
// It returns the store round trips it made. ListAllSessionBeads is a two-leg
// union (by type, by label), so each store consulted costs two.
func detachedOrphanRoutesFor(store, routeStore beads.Store) (detachedOrphanRouteIndex, int, error) {
	reads := 2
	routeIndex, indexErr := buildDetachedOrphanRouteIndex(store)
	if indexErr != nil {
		return detachedOrphanRouteIndex{}, reads, fmt.Errorf("building session route index: %w", indexErr)
	}
	// Only union a DISTINCT cross-store index. The city leg passes the city store
	// as both store and routeStore; its routes are already in routeIndex, so
	// rebuilding the same full ListAllSessionBeads scan and unioning it into
	// itself is pure waste. Interface identity is the right test here — production
	// stores are pointer-backed CachingStores.
	if routeStore != nil && routeStore != store {
		reads += 2
		crossIndex, crossErr := buildDetachedOrphanRouteIndex(routeStore)
		if crossErr != nil {
			return detachedOrphanRouteIndex{}, reads, fmt.Errorf("building cross-store session route index: %w", crossErr)
		}
		routeIndex.backfill(crossIndex)
	}
	return routeIndex, reads, nil
}

// isDetachedHandoffOrphanCandidate reports whether b has the signature of a
// fully-detached handoff orphan: open, unassigned, no pool route (neither
// gc.routed_to nor a legacy gc.run_target), no gc.kind, branch set (indicating
// work was done and pushed), and a session back-reference — gc.session_id or
// gc.session_name — from which the pool route can be recovered. This sweep's
// novel domain is exactly work that carries *no* self-declared route: a bead
// that still has gc.run_target is recovered earlier in the same tick by
// restoreCarriedWorkRoutes from its own declared route, and any non-empty
// gc.kind is a workflow-root/control/topology bead that carriedPoolRoute
// deliberately keeps out of pool demand.
func isDetachedHandoffOrphanCandidate(b beads.Bead) bool {
	if b.Status != "open" {
		return false
	}
	if strings.TrimSpace(b.Assignee) != "" {
		return false // still assigned — releaseOrphanedPoolAssignments covers this path
	}
	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
		return false // already has a pool route
	}
	if strings.TrimSpace(b.Metadata[beadmeta.RunTargetMetadataKey]) != "" {
		return false // carries its own legacy route — restoreCarriedWorkRoutes recovers it from gc.run_target
	}
	if strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey]) != "" {
		return false // any non-empty kind is workflow-root/control/topology work, not fully-detached pool work
	}
	if strings.TrimSpace(b.Metadata[beadmeta.WorkBranchMetadataKey]) == "" {
		return false // no work branch → not a completed-work handoff bead
	}
	// Accept either session back-reference. The claim path stamps gc.session_id
	// whenever GC_SESSION_ID is set and adds gc.session_name only when
	// GC_SESSION_NAME is also present (hookClaimIdentityPatch), so a valid
	// session-ID-only orphan exists. route() resolves either, preferring the
	// unique gc.session_id, so requiring gc.session_name here would strand a
	// session-ID-only orphan the exact-ID resolver could recover.
	if strings.TrimSpace(b.Metadata[beadmeta.SessionIDMetadataKey]) != "" {
		return true
	}
	return strings.TrimSpace(b.Metadata[beadmeta.SessionNameMetadataKey]) != ""
}

// detachedOrphanRouteIndex resolves a detached orphan's pool route from its
// session back-reference. It prefers an exact session-bead ID match — gc.session_id
// is the unique bead ID of the claiming session, stamped next to gc.session_name at
// claim time — and only falls back to gc.session_name when that name resolves
// unambiguously: every session bead carrying it that has a route agrees on one
// route. This mirrors internal/session/resolve.go's refusal to act on an ambiguous
// session_name match, so a duplicated session name never restores work to the wrong
// pool.
type detachedOrphanRouteIndex struct {
	byID   map[string]string // session-bead ID → pool route
	byName map[string]string // session_name → pool route, only when unambiguous
}

// route resolves the pool route for a detached orphan, preferring the exact
// session-bead ID over the session_name fallback. It returns "" when neither
// resolves — including when the session_name was dropped as ambiguous.
func (idx detachedOrphanRouteIndex) route(sessionID, sessionName string) string {
	if sessionID != "" {
		if r := idx.byID[sessionID]; r != "" {
			return r
		}
	}
	if sessionName != "" {
		if r := idx.byName[sessionName]; r != "" {
			return r
		}
	}
	return ""
}

// backfill copies entries from other for keys idx does not already own, so the
// primary store wins on conflict and the cross store only fills gaps. Both the ID
// and the (already ambiguity-pruned) name maps are unioned this way.
func (idx detachedOrphanRouteIndex) backfill(other detachedOrphanRouteIndex) {
	for id, route := range other.byID {
		if _, exists := idx.byID[id]; !exists {
			idx.byID[id] = route
		}
	}
	for sn, route := range other.byName {
		if _, exists := idx.byName[sn]; !exists {
			idx.byName[sn] = route
		}
	}
}

// buildDetachedOrphanRouteIndex indexes every session bead (open or closed) that
// carries a template, keyed both by the session bead's ID (matched against a work
// bead's gc.session_id) and by session_name. Closed session beads are included
// because the worker session is typically gone by the time this sweep runs. A
// session_name shared by session beads with conflicting routes is dropped from the
// name index so an ambiguous name never resolves to an arbitrary route; the unique
// per-ID entry still resolves such an orphan exactly.
func buildDetachedOrphanRouteIndex(store beads.Store) (detachedOrphanRouteIndex, error) {
	idx := detachedOrphanRouteIndex{byID: map[string]string{}, byName: map[string]string{}}
	all, listErr := session.ListAllSessionBeads(store, beads.ListQuery{IncludeClosed: true})
	// Hard errors return nil rows; surface them to the caller.
	partial := beads.IsPartialResult(listErr)
	if listErr != nil && !partial {
		return detachedOrphanRouteIndex{}, fmt.Errorf("listing session beads: %w", listErr)
	}
	// A partial list still yields usable rows, but it may be MISSING rows — so it
	// cannot prove a session_name is unambiguous: a conflicting same-name session
	// bead could sit in the unlisted partition and make byName silently resolve to
	// an arbitrary pool route. Session-bead IDs are unique, so a partial list can
	// only omit a byID entry (the orphan is simply not recovered this tick and
	// retries next tick), never make an existing one ambiguous. So byID stays safe
	// on a partial list while byName does not: populate byID always and skip byName
	// entirely when the list is partial, degrading to exact-gc.session_id recovery.
	ambiguousNames := map[string]bool{}
	for _, sb := range all {
		route := retiredSessionFallbackRoute(sb)
		if route == "" {
			continue // no template/agent_name → carries no recoverable route
		}
		if id := strings.TrimSpace(sb.ID); id != "" {
			idx.byID[id] = route // session-bead IDs are unique; exact gc.session_id match
		}
		if partial {
			continue // partial list can't prove name uniqueness — exact-ID recovery only
		}
		sn := strings.TrimSpace(sb.Metadata["session_name"])
		if sn == "" {
			continue
		}
		if existing, seen := idx.byName[sn]; seen {
			if existing != route {
				ambiguousNames[sn] = true // duplicate name resolving to conflicting routes
			}
			continue // keep first route; a matching duplicate is not a conflict
		}
		idx.byName[sn] = route
	}
	for sn := range ambiguousNames {
		delete(idx.byName, sn) // refuse to guess a route for an ambiguous name
	}
	return idx, nil
}

package api

// The T7e readers as they stood on main at 607e3570e3, before T7e typed their
// stores, frozen (t7-plan.md §2.3; T7d's workflow_legs_reference_test.go
// precedent). Each declaration is copied, doc comment and all, renamed to its
// frozen form; methods take the Server as their first argument, after a context. Every helper
// they call that reads or writes a store and that T7e edits is frozen too
// (relocatedGraphStore); sortedRigNames is T7d's frozen copy, identical to
// main's; the helpers T7e leaves byte-identical are called live.
// The equivalence table (bead_owner_legs_equivalence_test.go) runs them beside
// the converted readers. Do not edit them to follow production.

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/readywork"
	"github.com/gastownhall/gascity/internal/workrecord"
)

// frozenHumaHandleBeadReady is the Huma-typed handler for GET /v0/beads/ready.
//
// FEDERATION CONTRACT — the follow-on CLI federation (`gc ready`, ga-oxsyu) is
// specified against this handler, so its conformance test can assert CLI == API
// instead of inventing an oracle. The rules it has to match:
//
//   - Legs, in order: the city store, then the rigs by name ascending, then the
//     relocated graph store. The CLI composite's legs are work-then-infra, the
//     same relative order, so on a rig-less city the two sequences agree.
//     GET /v0/beads federates the same legs in the same order.
//   - Frame: every rig BeadStores holds, suspended ones included. `gc ready`
//     deliberately does not match this: it federates the serving rigs only,
//     the frame the controller's demand census reads, so a suspended rig's own
//     rows are in this answer and absent from the CLI's (ga-h07n0z; see
//     readyServingRigs in cmd/gc/ready_federation.go). CLI == API holds over a
//     city with no suspended rig.
//   - Within a leg: whatever order that leg's own Ready reader emits. For a
//     work store (CachingStore.Ready) and for the canonical relocated binding
//     (beads.SQLiteStore.Ready) alike that is the canonical (priority ASC,
//     created_at ASC, id ASC) order, so each leg is sorted on its own.
//   - Dedupe: first leg to return an id wins. The graph leg runs last, so a bead
//     co-resident in the work store and the binding — the documented steady
//     state of a migrated city, where the migration preserves ids and never
//     deletes back — resolves to the work store's row on both endpoints.
//   - Merged order: leg concatenation, deliberately NOT re-sorted. A global
//     re-sort would change the bytes a multi-rig single-store city already
//     serves, and the graph leg must be free for such a city. Both sides are
//     therefore compared after normalizing with beads.SortBeadsReadyOrder. That
//     normalization is load-bearing rather than cosmetic, because a
//     concatenation of sorted legs is not itself sorted; it is well-defined
//     because SortBeadsReadyOrder is a total order over the merged set.
//   - Failure: a rig degrades (Partial 200 + partial_errors); the graph leg
//     does not (503, carrying any work-leg errors recorded before it). See
//     graphPlaneUnavailable.
//   - Source: each leg is answered by its ready cache when the cache is warm
//     and can vouch for the answer (beads.ReadyPreferCache: the freshness and
//     invalidation rules of the zero-query CachingStore.Ready), and read live
//     only when it is cold or invalidated. Two limits follow from that. A cache
//     over a backing with no ready projection (MemStore, FileStore, and
//     DoltliteReadStore, whose projection is a no-op) treats a blocker it does
//     not hold as satisfied, where that store's own Ready can hold the
//     dependent back (ga-gmf8r). And a blocking request (?index=N) wakes on
//     its own watch, which can run ahead of the cache absorbing the event that
//     woke it: the body can miss that change while the index moves past it,
//     and the client sees the change on the next event or when its next wait
//     runs out (30s by default, 5m at most). DoltliteReadStore's cache also
//     ignores a closed blocker's gc.work_outcome, but so does its live Ready,
//     so the two agree there (ga-e3zi0s).
//   - Tier: every leg is read at beads.FederatedReadTier, stated explicitly.
//     A no-argument Ready() left TierMode at its zero value, which the work
//     legs' bead-policy layer rewrote to TierBoth while the unwrapped graph leg
//     took literally — so the relocated store's whole ephemeral tier fell out of
//     a 200 that named no failure (ga-8lyxc).
func frozenHumaHandleBeadReady(ctx context.Context, s *Server, input *BeadReadyInput) (*ListOutput[beads.Bead], error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}
	index := s.latestIndex()

	stores := s.state.BeadStores()
	rigNames := frozenSortedRigNames(stores)
	var pa partialAggregator
	// Each leg's rows are kept by leg so a split city can pass them through the
	// cross-store rule before they merge (below).
	type readyLeg struct {
		leg  readywork.Leg
		rows []beads.Bead
	}
	var legs []readyLeg
	federate := func(label string, store beads.Store) {
		if store == nil {
			return
		}
		pa.attempt()
		ready, err := beads.ReadyPreferCache(store, readyFederationQuery())
		if err != nil {
			if beads.IsPartialResult(err) && len(ready) > 0 {
				pa.record(label, err)
				pa.success()
			} else {
				pa.record(label, err)
				return
			}
		} else {
			pa.success()
		}
		legs = append(legs, readyLeg{leg: readywork.Leg{Label: label, Store: store}, rows: ready})
	}
	// City-scope ready work (graph.v2 molecules in a single-HQ city, control
	// beads) lives in the city store, so federate it explicitly first or HTTP
	// `bd ready` would never surface it. In production BeadStores() also returns
	// the city store keyed by CityName() (cmd/gc/api_state.go), so skip that
	// duplicate key in the rig loop below to avoid querying it twice.
	federate("city", s.state.CityBeadStore())
	cityName := s.state.CityName()
	for _, rigName := range rigNames {
		if rigName == cityName {
			continue // city store already federated explicitly above; production
			// BeadStores() also returns it under cityName (cmd/gc/api_state.go)
		}
		federate("rig "+rigName, stores[rigName])
	}
	// Split city: graph-class ready work (gcg- steps, control beads, molecule
	// roots) lives in the relocated graph store, which BeadStores() does not
	// include — federate it or the whole execution DAG is invisible behind an
	// authoritative-looking 200. It runs LAST so the merged order stays a leg
	// concatenation whose prefix is exactly what a legacy city serves, and it
	// does NOT go through federate(): the graph leg has no partial tier.
	if graph, ok := frozenRelocatedGraphStore(s.state); ok {
		pa.attempt()
		ready, err := beads.ReadyPreferCache(graph, readyFederationQuery())
		if err != nil {
			return nil, graphPlaneUnavailable("ready", err, pa.messages()...)
		}
		pa.success()
		legs = append(legs, readyLeg{leg: readywork.Leg{Label: "graph", Store: graph, ClassBinding: true, ReservedPrefixes: graphBindingPrefixes(s.state.Stores())}, rows: ready})
	}
	if pa.totalOutage() {
		return nil, pa.outageError()
	}
	// On a split city every leg's rows pass through the same cross-store rule
	// `gc ready` and pool demand apply (readywork.LegGate): a row a blocker
	// held in another store still holds is not served, and a row a store
	// strands behind a blocker that has closed elsewhere is. One gate judges
	// every leg, so a blocker several legs' rows wait on is read once. A city
	// with no binding keeps every store's own answer.
	federated := make([]readywork.Leg, len(legs))
	for i, l := range legs {
		federated[i] = l.leg
	}
	if readywork.IsSplit(federated) {
		gate := readywork.NewLegGate(federated, readyFederationQuery())
		for i := range legs {
			gated, err := gate.Gate(ctx, i, legs[i].rows)
			if err != nil {
				// A leg whose blockers could not be judged serves nothing: an
				// ungated row may be one a blocker still holds. The response
				// says it is short, as it does for a leg that failed to read.
				pa.record(legs[i].leg.Label, fmt.Errorf("cross-store blockers: %w", err))
				legs[i].rows = nil
				continue
			}
			legs[i].rows = gated
		}
	}
	var all []beads.Bead
	seen := make(map[string]bool)
	for _, l := range legs {
		for _, b := range l.rows {
			if seen[b.ID] {
				// An id is one bead: legacy file mode can alias the city and rig
				// stores, and a migrated split city holds the same infrastructure
				// row in both the work store and the binding.
				continue
			}
			seen[b.ID] = true
			all = append(all, b)
		}
	}
	if all == nil {
		all = []beads.Bead{}
	}

	return &ListOutput[beads.Bead]{
		Index: index,
		Body: ListBody[beads.Bead]{
			Items:         all,
			Total:         len(all),
			Partial:       pa.partial(),
			PartialErrors: pa.messages(),
		},
	}, nil
}

// frozenRelocatedGraphStore returns the graph-class handle when — and only when —
// the city has actually relocated it.
//
// Routing is keyed on STORE IDENTITY (beads.SameStore), the same rule
// Stores.Plan and handler_convoy_dispatch.go's workflow scan use: on a default
// city Stores().Graph() wraps the city's work store itself, so there is
// nothing to federate and every caller's extra arm stays dead. That identity
// guard is what keeps a single-store city byte-identical. Under the
// split-strict gate the graph handle reads the work store's class view, a
// store of its own, so a gate fixture keeps its split shape.
func frozenRelocatedGraphStore(state State) (beads.GraphStore, bool) {
	st := state.Stores()
	graph := st.Graph()
	if beads.IsNilStore(graph) || beads.SameStore(graph, st.Work()) {
		return beads.GraphStore{}, false
	}
	return graph, true
}

func frozenAppendMetadataAttachedChildren(store beads.Store, parent beads.Bead, children []beads.Bead) []beads.Bead {
	if store == nil {
		return children
	}
	seen := make(map[string]struct{}, len(children))
	for _, child := range children {
		seen[child.ID] = struct{}{}
	}
	// NOTE: "workflow_id" is the bare (non-prefixed) metadata key, distinct
	// from beadmeta.WorkflowIDMetadataKey ("gc.workflow_id") — do NOT substitute
	// the prefixed constant here or this would surface a different key.
	for _, key := range []string{beadmeta.MoleculeIDMetadataKey, "workflow_id"} {
		attachedID := strings.TrimSpace(parent.Metadata[key])
		if attachedID == "" {
			continue
		}
		if _, ok := seen[attachedID]; ok {
			continue
		}
		attached, err := store.Get(attachedID)
		if err != nil {
			continue
		}
		seen[attached.ID] = struct{}{}
		children = append(children, attached)
	}
	return children
}

// frozenCollectBeadGraph resolves the member set of the graph rooted at root and the
// parent-child edges within it. The membership rule it applied is returned
// alongside, so the handler reports it on the wire instead of the caller
// inferring it: beads.MembershipRootIDAndParentClosure, widened to
// beads.MembershipRootIDParentClosureAndConvoy when the root is a convoy.
//
// The root-id arm is what makes this NOT beads.MembershipDepReachable, and the
// difference is not cosmetic — on the measured live molecule gcg-arn a
// dependency walk returns 48 of the 61 beads this returns, dropping every
// gc.kind=spec sidecar, because spec steps are built with no dependency edges.
// See beads.Membership.
//
// Both list arms read beads.TierBoth because the declared rule has no tier
// axis. beads.DirectMembers reads both tiers (HandlesFor(store).Live forces
// it), so a tier-scoped read here would answer a strictly different question
// from the one the wire names: a wisp molecule materializes with every node in
// ephemeral storage (the wisp bead policy maps to ephemeral under bd-1.0.5
// semantics, and molecule instantiation stamps gc.root_bead_id on every node),
// so the default TierIssues would drop every member of one while still
// answering 200 with an unqualified "direct-root-id+parent-closure" — an empty
// wisp molecule is indistinguishable from a finished one. The read handle is
// deliberately still the store's own, not the LIVE handle beads.DirectMembers
// uses: this is a hot dashboard path and bypassing the cache is a separate
// change (see beads.Membership's note on the duplicated implementations).
//
// The parent-child walk is seeded from the whole accumulated member set, so
// the closure covers the convoy members too and a convoy member brings its own
// subtree. That is what the "+parent-closure" half of both spellings means; it
// is not the closure of the root alone.
func frozenCollectBeadGraph(store beads.Store, root beads.Bead) ([]beads.Bead, []workflowDepResponse, beads.Membership, error) {
	graphBeads := make([]beads.Bead, 0, 1)
	beadIndex := make(map[string]beads.Bead)

	upsert := func(b beads.Bead) {
		if b.ID == "" {
			return
		}
		if existing, ok := beadIndex[b.ID]; ok {
			if existing.ParentID == "" && b.ParentID != "" {
				existing.ParentID = b.ParentID
				beadIndex[b.ID] = existing
				for i := range graphBeads {
					if graphBeads[i].ID == b.ID {
						graphBeads[i].ParentID = b.ParentID
						break
					}
				}
			}
			return
		}
		beadIndex[b.ID] = b
		graphBeads = append(graphBeads, b)
	}
	upsert(root)

	membership := beads.MembershipRootIDAndParentClosure

	metadataChildren, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return nil, nil, "", fmt.Errorf("listing metadata children for bead %q: %w", root.ID, err)
	}
	for _, child := range metadataChildren {
		upsert(child)
	}

	if root.Type == "convoy" {
		membership = beads.MembershipRootIDParentClosureAndConvoy
		members, err := convoycore.MembersIn(convoycore.MemberClasses{Convoy: beads.WorkStore{Store: store}}, root.ID, true)
		if err != nil {
			return nil, nil, "", fmt.Errorf("listing convoy members for bead %q: %w", root.ID, err)
		}
		for _, member := range members {
			upsert(member)
		}
	}

	parentEdges := make([]workflowDepResponse, 0)
	seenEdges := make(map[string]bool)
	addParentEdge := func(parentID, childID string) {
		if parentID == "" || childID == "" {
			return
		}
		edge := workflowDepResponse{From: parentID, To: childID, Kind: "parent-child"}
		key := edge.From + "|" + edge.To + "|" + edge.Kind
		if seenEdges[key] {
			return
		}
		seenEdges[key] = true
		parentEdges = append(parentEdges, edge)
	}

	// Discover parent-linked descendants and their parent-child edges by walking
	// the tree one BFS level at a time, fetching each level's children with a
	// single batched store.List(ParentIDs=...) call. This replaces the former
	// per-bead store.List(ParentID=...) fan-out — an N+1 (one round-trip per bead)
	// that serialized on a single read connection and dominated graph-read latency
	// under load. The returned beads are filtered in memory against the level's
	// parent set, so the result is correct even on a backend that does not honor
	// ParentIDs (such a backend returns a superset, which the filter narrows).
	frontier := make([]string, 0, len(graphBeads))
	for _, b := range graphBeads {
		frontier = append(frontier, b.ID)
	}
	for len(frontier) > 0 {
		frontierSet := make(map[string]bool, len(frontier))
		for _, id := range frontier {
			frontierSet[id] = true
		}
		children, err := store.List(beads.ListQuery{
			ParentIDs:     frontier,
			IncludeClosed: true,
			AllowScan:     true,
			Sort:          beads.SortCreatedAsc,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			return nil, nil, "", fmt.Errorf("listing child beads for graph %q: %w", root.ID, err)
		}
		var next []string
		for _, child := range children {
			if !frontierSet[child.ParentID] {
				continue
			}
			addParentEdge(child.ParentID, child.ID)
			if _, seen := beadIndex[child.ID]; !seen {
				upsert(child)
				next = append(next, child.ID)
			}
		}
		frontier = next
	}

	return graphBeads, parentEdges, membership, nil
}

// frozenGateWorkRecordClose checks a bead the caller is about to close against the
// work-record contract, returning a 409 when enforcement is on and the record
// does not satisfy it (nil otherwise — warn-only is the default, so a violation
// is logged and the close proceeds).
//
// stored is the row the residency resolver read and store is the store that
// holds it. submitted carries the metadata of the same request, which the
// documented atomic close uses to stamp the record and close in one call:
// beads.UpdateOpts.Metadata is an additive merge, so validating only the stored
// row would refuse a request that supplies a perfectly good record. Coverage is
// decided on the stored row — Type, gc.kind, and workflow-step metadata, read
// before any submitted field is applied — so a request cannot escape the gate
// by stamping gc.kind on its way out, nor by retyping a gated task away from
// task. The same choice means the reverse direction is NOT covered: a closing
// update that converts a non-gated bead into a task closes ungated, because the
// type it would be gated on is the one the request is about to write. That is
// the boundary the CLI door has at isWorkRecordGatedBead in cmd/gc/work_record_gate.go,
// which decides coverage on the stored row and then projects metadata only;
// moving it belongs in internal/workrecord so both doors move together rather
// than asking different questions of different populations.
//
// ctx is the request's, and it reaches the reachability clause because that
// clause shells out to git: a client that hangs up has to be able to stop the
// subprocess, or a wedged repository leaves one blocking call per retry.
//
// Known limit — the check and the write are not atomic. The row validated here
// is the one resolveBeadOwner read, and the caller applies its close or update
// afterwards without re-reading it, so a concurrent write landing in that window
// is neither seen by the gate nor refused by the write. A close that races an
// edit stripping gc.work_outcome can therefore pass a check the final row would
// have failed. The CLI door has the same shape at evaluateWorkRecordCloseGate in
// cmd/gc/work_record_gate.go, which validates a stored (or pre-fetched) bead and
// then lets the bd invocation write.
//
// The remedy is to fence the write on the revision the gate read —
// beads.ConditionalWriter already spells it (CloseIfMatch/UpdateIfMatch, via
// beads.ResolveConditionalWriter) — so this is a change to the close paths, not
// to the store contract. It is left for a follow-up because the fence has to be
// threaded through both doors together and only capable stores carry it: a store
// that resolves as legacy has no revision to fence on, so the gate would need a
// degraded path there rather than a refusal. The window is small and the losing
// outcome is a close that recorded slightly less than it should, not a corrupted
// row.
func frozenGateWorkRecordClose(ctx context.Context, s *Server, id string, store beads.Store, stored beads.Bead, submitted map[string]string) error {
	if !workrecord.Gated(stored) {
		return nil
	}
	prospective := projectSubmittedWorkRecord(stored, submitted)
	repoDir := frozenWorkRecordRepoDir(s, store, prospective)
	unverified := false
	violations := workrecord.ValidateOnClose(prospective, func(commit, branch string) bool {
		if repoDir == "" {
			// The scope that owns this bead names no checkout (see
			// workRecordRepoDir for which shapes produce that, and why the
			// current State implementations do not), so there is no repository to
			// ask. The clause degrades to a warning and says so; see
			// workrecord.ReachabilityUnverifiedNote for why only this one does.
			unverified = true
			return true
		}
		return workRecordCommitReachable(ctx, repoDir, commit, branch)
	})

	enforce := workrecord.EnforceEnabled()
	mode := "warn-only"
	if enforce {
		mode = "enforced"
	}
	if unverified {
		workRecordGateLogf("work-record gate (%s): close of %s: %s", mode, id, workrecord.ReachabilityUnverifiedNote)
	}
	for _, violation := range violations {
		workRecordGateLogf("work-record gate (%s): close of %s: %s", mode, id, violation)
	}
	if enforce && len(violations) > 0 {
		return apierr.ConflictWrongState.Msg("conflict: bead " + id + " does not satisfy the work-record close contract: " + strings.Join(violations, "; "))
	}
	return nil
}

// frozenWorkRecordRepoDir names the repository a shipped bead's commit must be
// reachable in. The rule is workrecord.RepoDirFor's — gc.work_dir, else the
// bead's OWNER — so this door and the CLI's judge one bead against one
// repository; all this plane supplies is the checkout table and, for a bead that
// names no owner, the answer it gave before owners were recorded.
//
// An empty result means "unknown" and must stay distinguishable from a path,
// because falling back to a default would run git in whatever directory the
// server happens to occupy and answer the reachability question about the wrong
// repository.
func frozenWorkRecordRepoDir(s *Server, store beads.Store, bead beads.Bead) string {
	dirs := s.workRecordScopeDirs()
	dir, kind := workrecord.RepoDirFor(bead, dirs)
	if kind == workrecord.ScopeUnrooted {
		return frozenWorkRecordLegacyRepoDir(s, store, dirs)
	}
	return dir
}

// frozenWorkRecordLegacyRepoDir is the answer this plane gave before a bead could name
// its owner: the checkout of the scope the row was READ through — the rig's path
// when a configured rig's store answered, the city directory otherwise. It stays
// for the beads that record no gc.root_store_ref, which is every city that
// predates the residency census, so their closes are unchanged.
//
// The store is matched by asking each CONFIGURED rig whether it is the one that
// answered (beads.SameStore, so a class handle over a rig's store is that rig,
// and a rig with no store matches nothing), rather than by enumerating the
// loaded stores: an enumeration is a
// list of work stores, which is blind to a binding by construction, and the
// question is not "which stores exist" but "does the store that answered belong
// to a rig with a checkout".
//
// A store no configured rig claims is the city store or a class binding, and
// both answered to the city checkout before this rule existed. buildStores keys
// only from cfg.Rigs and swaps the config and the store map under one lock, so
// there is no third population to mistake for one.
//
// The converse population is real and this answer is a guess for it: in legacy
// shared-file mode buildStores aliases every rig backed by that store to one
// handle, so MANY rigs claim it and the identity match selects whichever is
// configured first — an ownerless bead whose work happened on a different rig
// is judged against that first rig's checkout. The loop predates this rule and
// RepoDirFor strictly narrows it, since a bead that names an owner no longer
// reaches here; what is left is the pre-census population, where no answer is
// better than a guess because the bead records nothing to resolve.
//
// Two shapes still produce "unknown" here: a configured rig that names no
// checkout, and a city with no path at all. Both are defensive against a State
// implementation the current ones do not produce — buildStores skips a rig with
// an empty path outright, so BeadStore answers nil for one and the identity
// match never selects it, and cityPath is set once at construction from an
// already-resolved directory and never reassigned. State is an interface and the
// branches are cheap, so they stay; a refactor that changes either fact should
// update this note rather than find it silently wrong.
func frozenWorkRecordLegacyRepoDir(s *Server, store beads.Store, dirs workRecordScopeDirs) string {
	for _, rig := range dirs.rigs {
		if !beads.SameStore(s.state.BeadStore(rig.Name), store) {
			continue
		}
		// The path comes off the rig that MATCHED, not from a second lookup by
		// name: the match here is store identity, and re-deriving it from a name
		// would answer for whichever rig the name resolves to instead.
		if strings.TrimSpace(rig.Path) == "" {
			return ""
		}
		return resolveScopeRoot(dirs.cityPath, rig.Path)
	}
	return dirs.CityDir()
}

package beads

import "sync/atomic"

// inlineDependencyProjection is what a BdStore has learned, from bd's own
// answers, about the dependency rows bd's list JSON carries beside each issue.
//
// bd projects those rows from the same table, in the same query, as the
// dependency_count it puts on every row: sqlbuild.SearchCountsSQL selects
// `d.deps_json = JSON_ARRAYAGG(...)` over tables.Dependencies alongside
// `dc.cnt = COUNT(*) ... WHERE type = 'blocks'` over that same table, for the
// issues family and the wisps family alike, and ScanReadyWorkRowWithCounts
// unmarshals deps_json onto types.Issue.Dependencies. Both `bd list` and the
// `bd query` this store uses for the wisp tier go through that one builder.
//
// So the count and the rows are two views of one read, and a disagreement
// between them is the only way the inline projection can be short. That makes
// the completeness claim MEASURABLE per listing rather than assumed from a
// reading of bd's source — which matters, because an adapter that populated the
// field for nobody would answer "no edges" for every bead and a cache that
// believed it would flatten the whole topology into "everything is ready".
type inlineDependencyProjection struct {
	// witnessed latches on the first listed row bd said had blocking edges AND
	// carried them. Nothing short of a row with edges proves the projection is
	// live, so a store whose ledger has no dependencies at all never claims
	// completeness — it keeps today's behavior, which is correct and merely
	// slower.
	witnessed atomic.Bool
	// contradicted latches on the first listed row whose dependency_count and
	// inline blocking edges disagree, and is authoritative over witnessed. It
	// is one-way: a projection that was short once is not one a later page can
	// vouch for, and the cost of being wrong is offering the control dispatcher
	// work whose gate has not opened.
	contradicted atomic.Bool
}

// noteInlineDependencyProjection records what one page of bd output proved
// about the inline dependency projection. rows and beads are parallel:
// beads[i] must be rows[i].toBead(), so the comparison is against the
// NORMALIZED edges a cache would actually consume rather than against the raw
// JSON.
//
// It is called with what bd RETURNED, before any client-side filtering, for the
// same reason noteServerRows is: a filter reducing a real answer to nothing says
// nothing about the ledger.
func (s *BdStore) noteInlineDependencyProjection(rows []bdIssue, beads []Bead) {
	if s == nil || len(rows) != len(beads) || s.inlineDeps.contradicted.Load() {
		return
	}
	witnessed := false
	for i := range rows {
		// A bd that does not project the count cannot testify either way. Skip
		// the row rather than reading its silence as a disagreement.
		if rows[i].DependencyCount == nil {
			continue
		}
		blocking := 0
		for _, dep := range beads[i].Dependencies {
			// dependency_count is COUNT(*) WHERE type = 'blocks' exactly — not
			// the wider set gascity treats as ready-blocking — so the comparison
			// is against that one type.
			if dep.Type == "blocks" {
				blocking++
			}
		}
		if blocking != *rows[i].DependencyCount {
			s.inlineDeps.contradicted.Store(true)
			return
		}
		if blocking > 0 {
			witnessed = true
		}
	}
	if witnessed {
		s.inlineDeps.witnessed.Store(true)
	}
}

// listIncludesCompleteDependencies reports whether the rows this store's List
// hands back carry their complete "down" dependency set inline, so a
// CachingStore over it can serve DepList and readiness from the snapshot it
// already has instead of spending a second read.
//
// This used to be a flat false, and that cost every BdStore-backed scope its
// complete-ready cache read permanently: CachingStore.cachedReadyCompleteOnly
// gates on depsComplete, so ReadyContext answered "bead cache unavailable"
// forever and the control dispatcher took a live `bd ready` on every tick
// (ga-tgpfm). On maintainer-city, whose work class is cross-region hosted
// Postgres, that live read costs ~4.4 s.
//
// The verdict is now the witnessed one, and the witness is free: it is read off
// the rows bd already returned (noteInlineDependencyProjection), so this adds
// ZERO subprocesses for any N. That is the whole reason it is not a
// dependencySnapshotForCache over DepListBatch, which the two other stores use:
// DepListBatch is `bd dep list`, and the bd/Postgres backend answers that with
// `operation "IssueRelations" not supported by the postgres backend` (measured
// on maintainer-city, ga-7i7ts). On the one city this defect is about, the
// batched read would have bought a guaranteed-failing subprocess per prime and
// per reconcile and left depsComplete false. The same inline fallback is what
// cmd/gc/infra_class_recover.go's sourceDepReader already relies on for that
// adapter, and for the same reason.
//
// Completeness of the dep SET is not the same claim as readiness equalling bd's.
// bd's is_blocked propagates transitively down parent-child edges, which no
// down-dep set can reproduce, so the cache's readiness answer equals bd's only
// while enrichReadyProjectionForCache is filling that column — and when it
// cannot, it reports ErrReadyProjectionUnsupported and the cache declines every
// readiness handle (readyReadsMustGoLive). Those two are separate gates on
// purpose; this one only says the rows carry their own edges.
func (s *BdStore) listIncludesCompleteDependencies() bool {
	if s == nil || s.inlineDeps.contradicted.Load() {
		return false
	}
	return s.inlineDeps.witnessed.Load()
}

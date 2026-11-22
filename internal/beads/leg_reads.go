package beads

import "context"

// This file is the role-carrying read side below a leg (typed-stores T7b, the
// T7a review's carry-forward). A leg layer's own bead reads carry its role on
// the stack (leg.go), but a read made BELOW the layer does not: the live and
// cached readers HandlesFor and LiveFor answer, the exact batch read GetMany
// takes through the live reader (and the Gets it fans out to its own
// goroutines), and the cache-first list and count capabilities As hands out.
// Each of those reaches the store beneath the leg directly, so the gate would
// police it as strict.
//
// So a leg answers them through adapters whose reads are their own frames,
// one non-generic type per role (generic stenciling would merge the frame
// names), never inlined, and registered in legReadFrames: the federated and
// relic read adapters (the cached and live readers, with GetExactBatch), the
// cache-first list adapters (CachedLister) and the count adapters (Counter).
// Any other capability a leg hands out (DependencyBatchLister,
// ChildrenBatchLister, AbsentBlockedReader, ...) is still read below the leg
// and policed; a caller that needs its role takes it through one of these.

// legSideReader is the read side of a cached or a live reader: CachedReader
// and LiveReader have one method set.
type legSideReader interface {
	Get(id string) (Bead, error)
	List(query ListQuery) ([]Bead, error)
	Ready(query ...ReadyQuery) ([]Bead, error)
	DepList(id, direction string) ([]Dep, error)
}

// federatedReads is a reader below a federated leg.
type federatedReads struct{ r legSideReader }

// relicReads is a reader below a relic leg.
type relicReads struct{ r legSideReader }

var (
	_ LiveReader       = (*federatedReads)(nil)
	_ CachedReader     = (*federatedReads)(nil)
	_ ExactBatchGetter = (*federatedReads)(nil)
	_ LiveReader       = (*relicReads)(nil)
	_ CachedReader     = (*relicReads)(nil)
	_ ExactBatchGetter = (*relicReads)(nil)
)

// Get reads below the federated leg.
//
//go:noinline
func (a *federatedReads) Get(id string) (Bead, error) { return a.r.Get(id) }

// List reads below the federated leg.
//
//go:noinline
func (a *federatedReads) List(query ListQuery) ([]Bead, error) { return a.r.List(query) }

// Ready reads below the federated leg.
//
//go:noinline
func (a *federatedReads) Ready(query ...ReadyQuery) ([]Bead, error) { return a.r.Ready(query...) }

// DepList reads below the federated leg.
//
//go:noinline
func (a *federatedReads) DepList(id, direction string) ([]Dep, error) {
	return a.r.DepList(id, direction)
}

// GetExactBatch is the wrapped reader's exact batch read below the federated
// leg, or ErrExactBatchGetUnsupported for a reader without one.
//
//go:noinline
func (a *federatedReads) GetExactBatch(ids []string) (map[string]Bead, []string, error) {
	return exactBatchOf(a.r, ids)
}

// Get reads below the relic leg.
//
//go:noinline
func (a *relicReads) Get(id string) (Bead, error) { return a.r.Get(id) }

// List reads below the relic leg.
//
//go:noinline
func (a *relicReads) List(query ListQuery) ([]Bead, error) { return a.r.List(query) }

// Ready reads below the relic leg.
//
//go:noinline
func (a *relicReads) Ready(query ...ReadyQuery) ([]Bead, error) { return a.r.Ready(query...) }

// DepList reads below the relic leg.
//
//go:noinline
func (a *relicReads) DepList(id, direction string) ([]Dep, error) { return a.r.DepList(id, direction) }

// GetExactBatch is the wrapped reader's exact batch read below the relic leg;
// see federatedReads'.
//
//go:noinline
func (a *relicReads) GetExactBatch(ids []string) (map[string]Bead, []string, error) {
	return exactBatchOf(a.r, ids)
}

// exactBatchOf is r's exact batch read, or ErrExactBatchGetUnsupported, which
// GetMany answers by reading one id at a time.
func exactBatchOf(r legSideReader, ids []string) (map[string]Bead, []string, error) {
	getter, ok := r.(ExactBatchGetter)
	if !ok {
		return nil, nil, ErrExactBatchGetUnsupported
	}
	return getter.GetExactBatch(ids)
}

// federatedCachedList is a cache-first list below a federated leg.
type federatedCachedList struct{ c CachedLister }

// relicCachedList is a cache-first list below a relic leg.
type relicCachedList struct{ c CachedLister }

// CachedList lists from the cache below the federated leg.
//
//go:noinline
func (a *federatedCachedList) CachedList(q ListQuery) ([]Bead, bool) { return a.c.CachedList(q) }

// CachedList lists from the cache below the relic leg.
//
//go:noinline
func (a *relicCachedList) CachedList(q ListQuery) ([]Bead, bool) { return a.c.CachedList(q) }

// federatedCount is a count below a federated leg.
type federatedCount struct{ c Counter }

// relicCount is a count below a relic leg.
type relicCount struct{ c Counter }

// Count counts below the federated leg.
//
//go:noinline
func (a *federatedCount) Count(ctx context.Context, query ListQuery, excludeTypes ...string) (int, error) {
	return a.c.Count(ctx, query, excludeTypes...)
}

// Count counts below the relic leg.
//
//go:noinline
func (a *relicCount) Count(ctx context.Context, query ListQuery, excludeTypes ...string) (int, error) {
	return a.c.Count(ctx, query, excludeTypes...)
}

// legRoleOf is the role of the innermost leg layer among s's class layers
// (the leg nearest the store, as LegRoleOnStack answers the innermost leg
// frame), or NoLegRole.
func legRoleOf(s StoreReader) LegRole {
	role := NoLegRole
	for range classLayerMaxDepth {
		if leg, ok := s.(legLayer); ok && leg.legRole() != NoLegRole {
			role = leg.legRole()
		}
		layer, ok := s.(classLayer)
		if !ok {
			return role
		}
		s = layer.unwrapClassLayer()
	}
	return role
}

// roleReads is r with its reads carrying role: a reader adapter, or r itself
// for NoLegRole or a nil reader.
func roleReads(role LegRole, r legSideReader) legSideReader {
	if r == nil {
		return nil
	}
	switch role {
	case FederatedLegRole:
		return &federatedReads{r: r}
	case RelicLegRole:
		return &relicReads{r: r}
	default:
		return r
	}
}

// legHandles is h with its cached and live readers carrying role; the writer
// is unchanged, since a write carries no role.
func legHandles(role LegRole, h StoreHandles) StoreHandles {
	if h.Cached != nil {
		h.Cached = roleReads(role, h.Cached)
	}
	if h.Live != nil {
		h.Live = roleReads(role, h.Live)
	}
	return h
}

// legCapability is v, a capability As found below a leg of role, with its
// reads carrying the role where an adapter exists for T: CachedLister and
// Counter. Any other T is returned as is.
func legCapability[T any](role LegRole, v T) T {
	if role == NoLegRole {
		return v
	}
	switch any((*T)(nil)).(type) {
	case *CachedLister:
		c := any(v).(CachedLister)
		if role == RelicLegRole {
			return any(&relicCachedList{c: c}).(T)
		}
		return any(&federatedCachedList{c: c}).(T)
	case *Counter:
		c := any(v).(Counter)
		if role == RelicLegRole {
			return any(&relicCount{c: c}).(T)
		}
		return any(&federatedCount{c: c}).(T)
	}
	return v
}

// LegAdapterBase is the reader or capability a role-carrying adapter wraps,
// and v itself for anything else. It is for conformance tests that compare
// what a leg hands out with what its store does.
func LegAdapterBase(v any) any {
	switch a := v.(type) {
	case *federatedReads:
		return a.r
	case *relicReads:
		return a.r
	case *federatedCachedList:
		return a.c
	case *relicCachedList:
		return a.c
	case *federatedCount:
		return a.c
	case *relicCount:
		return a.c
	}
	return v
}

// legAdapterFrames are the read frames the adapters leave, by role.
func legAdapterFrames(pkg string, frames map[string]LegRole) {
	for _, read := range []string{"Get", "List", "Ready", "DepList", "GetExactBatch"} {
		frames[pkg+"(*federatedReads)."+read] = FederatedLegRole
		frames[pkg+"(*relicReads)."+read] = RelicLegRole
	}
	frames[pkg+"(*federatedCachedList).CachedList"] = FederatedLegRole
	frames[pkg+"(*relicCachedList).CachedList"] = RelicLegRole
	frames[pkg+"(*federatedCount).Count"] = FederatedLegRole
	frames[pkg+"(*relicCount).Count"] = RelicLegRole
}

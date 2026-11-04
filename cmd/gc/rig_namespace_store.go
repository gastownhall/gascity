package main

// The guarded rig store: what a rig with federation = "rig" looks like to the
// rest of this city.
//
// Several cities can attach one bead store. A rig that opts in with
// federation = "rig" restricts this city to its share of that store: rows
// whose owners are all inside the rig's namespace, rows a session of this
// city's own pools of the rig's agents claimed, and rows this city's guard
// created (rigNamespace decides which; rig_namespace.go). Everything else in
// the store belongs to another city and must stay out of reach.
//
// # Where it sits
//
// The chain is policy -> guard -> cache -> engine in the controller and
// policy -> guard -> engine for a one-shot open. Under the cache, the cache's
// reconcile would re-absorb filtered rows through the backing's Get and
// synthesize closes for rows that leave the namespace; folded into the policy
// layer, the policy layer's resolve-target peel and its re-wrap sites would
// step around it. So the cache stays unfiltered and the guard filters what the
// city reads from it and fences what the city writes through it.
//
// # Reads
//
// Discovery reads (List, Ready and their helpers, and every read through the
// guard's Handles) drop Foreign rows. A discovery read that selects by
// identity (an assignee, or an owner key or gc.run_target in its metadata
// equality) returns nothing without reading when the identity is not
// admitted. Reads by id, by parent and of dependencies pass through
// unchanged: hiding a row there would make a molecule look finished or a
// dependency look gone. A bounded read re-reads with a growing window until
// it fills its page or the inner store runs dry, so a short page still means
// "exhausted". A row whose classification failed (this city's session records
// could not be read) is dropped and the read is reported as a partial result
// (beads.IsPartialResult), so no caller concludes from the smaller set.
//
// # Writes
//
// Every mutation of a Foreign row is refused with a *rigNamespaceError (which
// wraps errRigNamespace). A create is stamped with the creation stamp
// (gc.owner_namespace = the namespace); no other write may set, change or
// remove that stamp. Setting an assignee requires an admitted identity; a
// routing value must be rig-qualified, and one outside the namespace is
// accepted only on an Owned row (a hand-off of this city's own work).
// Every guarded write costs one extra live read of the row it writes. The
// revision-fenced writes (UpdateIfMatch, CloseIfMatch, DeleteIfMatch,
// CloseWithMetadataIfMatch, and the guarded batch, whose unguarded updates
// are pinned to the revision judged) are exact: a row no longer at the
// expected revision is a precondition failure, so the decision is about the
// row written. Every other write (Update, SetMetadata, Close, Reopen,
// Delete, dep edges, Tx operations, the metadata CAS, ReleaseIfCurrent,
// TransferIfCurrent, UpdateIfAssignment, Claim) reads the row and then
// writes it, which leaves a window in which another writer can change the
// row's owner between the two. That is a documented residual: closing it
// needs those call sites converted to revision-fenced writes.
//
// # Capabilities
//
// Optional capabilities are discovered by type assertion, so a method the
// guard did not carry would silently downgrade its caller, and one it carried
// as a plain forward would hand the caller the inner store's unfenced write.
// The guard therefore forwards every capability the reference stores declare,
// through handles that answer false when the inner store lacks it, and never
// carries ConditionalWritesResolveTarget, Backing or the plain
// ConditionalWriter methods. TestRigNamespaceStoreCapabilityParity holds it
// to that.
//
// The guard reaches the inner store's capabilities with beads.As. The inner
// store is physical (an engine or a cache, never a class handle), so As is a
// plain assertion there, except for a capability with a resolver helper:
// UpdateIfAssignment resolves through AssignmentGuardedUpdaterFor, which
// follows a declared resolve target, so over a proxied engine the guard
// forwards it to the write leaf after its own checks, as an unguarded rig
// would.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// rigNamespaceError is a guarded store's refusal of a write.
type rigNamespaceError struct {
	// Rig and Namespace name the guarded rig ("beta", "beta/").
	Rig       string
	Namespace string
	// Op is the refused operation ("update", "close", "create", ...).
	Op string
	// ID is the row the operation targets; empty for a create.
	ID string
	// Reason says what made the write foreign.
	Reason string
	// Err is a session-record lookup failure behind the refusal, if any.
	Err error
}

// Error reports the refusal.
func (e *rigNamespaceError) Error() string {
	if e == nil {
		return "<nil>"
	}
	target := e.Op
	if e.ID != "" {
		target += " of " + e.ID
	}
	msg := fmt.Sprintf("rig %q (federation = \"rig\") refuses %s: %s", e.Rig, target, e.Reason)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap exposes errRigNamespace and the lookup failure, if any.
func (e *rigNamespaceError) Unwrap() []error {
	if e == nil {
		return nil
	}
	if e.Err != nil {
		return []error{errRigNamespace, e.Err}
	}
	return []error{errRigNamespace}
}

// rigNamespaceStore is the guard over one guarded rig's store. inner is the
// cache (controller) or the engine (one-shot open); ns decides which rows
// and identities are this city's.
type rigNamespaceStore struct {
	inner beads.Store
	ns    *rigNamespace
}

// newRigNamespaceStore returns the guard over inner. A nil namespace is
// guarded as deny-all, never left open.
func newRigNamespaceStore(inner beads.Store, ns *rigNamespace) *rigNamespaceStore {
	if ns == nil {
		ns = denyAllRigNamespace(nil)
	}
	return &rigNamespaceStore{inner: inner, ns: ns}
}

// ---------------------------------------------------------------------------
// Read gating and filtering.

// rigNamespaceRefillRounds bounds how often a bounded read grows its window
// (by rigNamespaceRefillFactor) before it reads the rest unbounded.
const (
	rigNamespaceRefillRounds = 4
	rigNamespaceRefillFactor = 4
)

// errRigNamespaceCacheDeclined carries a declined CachedList through refill.
var errRigNamespaceCacheDeclined = errors.New("cache declined the read")

// isStructuralListQuery reports whether q selects by id or by parent: such a
// read passes through the guard unchanged.
func isStructuralListQuery(q beads.ListQuery) bool {
	return len(q.IDs) > 0 || strings.TrimSpace(q.ParentID) != "" || len(q.ParentIDs) > 0
}

// admitPredicate decides one identity-predicate value. An empty value
// selects rows by the absence of an identity, not by one, and is not gated.
func (s *rigNamespaceStore) admitPredicate(v string) (bool, error) {
	if strings.TrimSpace(v) == "" {
		return true, nil
	}
	a := s.ns.admission(v)
	return a.Admitted(), a.Err
}

// gateMetadata decides a metadata equality's identity predicates: the owner
// keys and gc.run_target, keys compared trimmed as the backends trim them.
// Keys are visited in sorted order, so the verdict and the error that names
// the first failing key are the same on every call.
func (s *rigNamespaceStore) gateMetadata(filters map[string]string) (bool, error) {
	for _, k := range slices.Sorted(maps.Keys(filters)) {
		if !isRigNamespaceIdentityPredicateKey(k) {
			continue
		}
		ok, err := s.admitPredicate(filters[k])
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// gateListQuery decides q's identity predicates. It returns the query to read
// (Assignees narrowed to the admitted ones) and readable=false when the read
// must return nothing without touching the inner store. err is a session
// lookup failure that kept a value unadmitted.
func (s *rigNamespaceStore) gateListQuery(q beads.ListQuery) (beads.ListQuery, bool, error) {
	if ok, err := s.admitPredicate(q.Assignee); err != nil || !ok {
		return q, false, err
	}
	if len(q.Assignees) > 0 {
		var kept []string
		var firstErr error
		for _, a := range q.Assignees {
			ok, err := s.admitPredicate(a)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			if ok {
				kept = append(kept, a)
			}
		}
		if len(kept) == 0 {
			return q, false, firstErr
		}
		q.Assignees = kept
		if firstErr != nil {
			// Some assignees could not be decided: read the others, and
			// report the read as partial.
			if ok, err := s.gateMetadata(q.Metadata); err != nil || !ok {
				return q, false, err
			}
			return q, true, firstErr
		}
	}
	if ok, err := s.gateMetadata(q.Metadata); err != nil || !ok {
		return q, false, err
	}
	return q, true, nil
}

// gateReadyQuery is gateListQuery for a ready read.
func (s *rigNamespaceStore) gateReadyQuery(q beads.ReadyQuery) (bool, error) {
	if ok, err := s.admitPredicate(q.Assignee); err != nil || !ok {
		return false, err
	}
	return s.gateMetadata(q.MetadataEquals)
}

// partial reports a read the guard could not answer completely.
func (s *rigNamespaceStore) partial(op string, err error) error {
	if err == nil {
		return nil
	}
	return &beads.PartialResultError{
		Op:  "rig namespace " + s.ns.rigName() + ": " + op,
		Err: err,
	}
}

// keep drops the Foreign rows of rows. A row whose classification failed is
// dropped too, and the first such failure is returned for the caller to
// report the read as partial.
func (s *rigNamespaceStore) keep(rows []beads.Bead) ([]beads.Bead, error) {
	if rows == nil {
		return nil, nil
	}
	out := make([]beads.Bead, 0, len(rows))
	var firstErr error
	for _, b := range rows {
		c := s.ns.classifyRow(b)
		if c.Class == rowForeign {
			if c.Err != nil && firstErr == nil {
				firstErr = c.Err
			}
			continue
		}
		out = append(out, b)
	}
	return out, firstErr
}

// filtered applies keep to an inner read's answer. The inner store's own
// error wins; a classification failure on an otherwise clean read is a
// partial result.
func (s *rigNamespaceStore) filtered(op string, rows []beads.Bead, err error) ([]beads.Bead, error) {
	kept, cerr := s.keep(rows)
	if err != nil {
		return kept, err
	}
	return kept, s.partial(op, cerr)
}

// refill answers a read bounded to limit rows: it reads with a window of
// limit, keeps the admitted rows, and grows the window until limit rows are
// kept or the inner store returns fewer rows than asked (exhausted). Kept
// rows are the inner store's order with Foreign rows removed, so the page is
// a correct prefix of what an unbounded read would keep. limit <= 0 reads
// unbounded once. read is called with the window to use; 0 means unbounded.
func (s *rigNamespaceStore) refill(op string, limit int, read func(window int) ([]beads.Bead, error)) ([]beads.Bead, error) {
	if limit <= 0 {
		rows, err := read(0)
		return s.filtered(op, rows, err)
	}
	window := limit
	for round := 0; ; round++ {
		rows, err := read(window)
		kept, ferr := s.filtered(op, rows, err)
		if err != nil {
			return kept, ferr
		}
		if len(kept) >= limit {
			return kept[:limit], ferr
		}
		if window == 0 || len(rows) < window {
			return kept, ferr
		}
		if round+1 >= rigNamespaceRefillRounds || window > (1<<30)/rigNamespaceRefillFactor {
			window = 0
		} else {
			window *= rigNamespaceRefillFactor
		}
	}
}

// listGated answers a discovery List read through read (the inner store's
// List, or one of its handles').
func (s *rigNamespaceStore) listGated(op string, q beads.ListQuery, read func(beads.ListQuery) ([]beads.Bead, error)) ([]beads.Bead, error) {
	q, readable, gerr := s.gateListQuery(q)
	if isStructuralListQuery(q) {
		// By id or parent: never filtered, but an identity it also selects by
		// is gated like any other, so the read cannot be used to find another
		// city's rows by holder.
		if !readable {
			return []beads.Bead{}, s.partial(op, gerr)
		}
		rows, err := read(q)
		if err == nil && gerr != nil {
			err = s.partial(op, gerr)
		}
		return rows, err
	}
	if !readable {
		return []beads.Bead{}, s.partial(op, gerr)
	}
	rows, err := s.refill(op, q.Limit, func(window int) ([]beads.Bead, error) {
		q2 := q
		q2.Limit = window
		return read(q2)
	})
	if err == nil && gerr != nil {
		err = s.partial(op, gerr)
	}
	return rows, err
}

// readyGated answers a discovery Ready read through read.
func (s *rigNamespaceStore) readyGated(op string, q beads.ReadyQuery, read func(beads.ReadyQuery) ([]beads.Bead, error)) ([]beads.Bead, error) {
	readable, gerr := s.gateReadyQuery(q)
	if !readable {
		return []beads.Bead{}, s.partial(op, gerr)
	}
	return s.refill(op, q.Limit, func(window int) ([]beads.Bead, error) {
		q2 := q
		q2.Limit = window
		return read(q2)
	})
}

func firstReadyQuery(query []beads.ReadyQuery) beads.ReadyQuery {
	if len(query) == 0 {
		return beads.ReadyQuery{}
	}
	return query[0]
}

// ---------------------------------------------------------------------------
// beads.Store reads.

// Get passes through: a read by id is never filtered.
func (s *rigNamespaceStore) Get(id string) (beads.Bead, error) { return s.inner.Get(id) }

// List filters a discovery read; a read by id or parent passes through.
func (s *rigNamespaceStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	return s.listGated("list", query, s.inner.List)
}

// ListOpen filters the open rows.
func (s *rigNamespaceStore) ListOpen(status ...string) ([]beads.Bead, error) {
	rows, err := s.inner.ListOpen(status...)
	return s.filtered("list open", rows, err)
}

// Ready filters the ready rows.
func (s *rigNamespaceStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return s.readyGated("ready", firstReadyQuery(query), func(q beads.ReadyQuery) ([]beads.Bead, error) {
		return s.inner.Ready(q)
	})
}

// Children passes through: a structural read.
func (s *rigNamespaceStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.inner.Children(parentID, opts...)
}

// ChildrenOfAny passes the batched children read through, like Children.
func (s *rigNamespaceStore) ChildrenOfAny(parentIDs []string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if batch, ok := beads.As[beads.ChildrenBatchLister](s.inner); ok {
		return batch.ChildrenOfAny(parentIDs, opts...)
	}
	return beads.ChildrenOfAnyPerParent(s, parentIDs, opts...)
}

// ListByLabel filters a label read.
func (s *rigNamespaceStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return s.refill("list by label", limit, func(window int) ([]beads.Bead, error) {
		return s.inner.ListByLabel(label, window, opts...)
	})
}

// ListByAssignee returns nothing for an unadmitted assignee and filters
// otherwise.
func (s *rigNamespaceStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	const op = "list by assignee"
	if ok, err := s.admitPredicate(assignee); err != nil || !ok {
		return []beads.Bead{}, s.partial(op, err)
	}
	return s.refill(op, limit, func(window int) ([]beads.Bead, error) {
		return s.inner.ListByAssignee(assignee, status, window)
	})
}

// ListByMetadata returns nothing for an unadmitted identity predicate and
// filters otherwise.
func (s *rigNamespaceStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	const op = "list by metadata"
	if ok, err := s.gateMetadata(filters); err != nil || !ok {
		return []beads.Bead{}, s.partial(op, err)
	}
	return s.refill(op, limit, func(window int) ([]beads.Bead, error) {
		return s.inner.ListByMetadata(filters, window, opts...)
	})
}

// DepList passes through: dependency edges are structural.
func (s *rigNamespaceStore) DepList(id, direction string) ([]beads.Dep, error) {
	return s.inner.DepList(id, direction)
}

// GetLocalString passes through: clone-local values are this store's own.
func (s *rigNamespaceStore) GetLocalString(id, key string) (string, error) {
	return s.inner.GetLocalString(id, key)
}

// SetLocalString passes through: a clone-local value is never synced to the
// shared store, so it touches no other city's row.
func (s *rigNamespaceStore) SetLocalString(id, key, value string) error {
	return s.inner.SetLocalString(id, key, value)
}

// Ping passes through.
func (s *rigNamespaceStore) Ping() error { return s.inner.Ping() }

// ---------------------------------------------------------------------------
// Optional reads.

// ReadyContext filters the deadline-bound ready read.
func (s *rigNamespaceStore) ReadyContext(ctx context.Context, query ...beads.ReadyQuery) ([]beads.Bead, error) {
	reader, ok := beads.As[beads.ContextReadyReader](s.inner)
	if !ok {
		return nil, fmt.Errorf("reading ready beads through the rig namespace guard: %w", beads.ErrReadyContextUnsupported)
	}
	return s.readyGated("ready", firstReadyQuery(query), func(q beads.ReadyQuery) ([]beads.Bead, error) {
		return reader.ReadyContext(ctx, q)
	})
}

// StrandsAbsentTargets answers for the store the guard wraps
// (beads.AbsentBlockedReader): the guard strands what that store strands.
func (s *rigNamespaceStore) StrandsAbsentTargets() bool {
	_, ok := beads.ReadyBlockedByAbsentFor(s.inner)
	return ok
}

// ReadyBlockedByAbsent filters the inner store's absent-blocker listing as
// Ready filters its rows: the listing is a discovery read of ready candidates,
// so another city's rows are dropped and a read the guard cannot classify is
// partial. An inner store without the listing strands nothing and reports
// none. The listing ignores Limit (the caller re-judges every row), so it is
// read once, unbounded.
func (s *rigNamespaceStore) ReadyBlockedByAbsent(query beads.ReadyQuery) ([]beads.Bead, error) {
	reader, ok := beads.ReadyBlockedByAbsentFor(s.inner)
	if !ok {
		return nil, nil
	}
	const op = "ready blocked by absent"
	readable, gerr := s.gateReadyQuery(query)
	if !readable {
		return []beads.Bead{}, s.partial(op, gerr)
	}
	rows, err := reader.ReadyBlockedByAbsent(query)
	return s.filtered(op, rows, err)
}

// ReadyPreferCache filters the cache-preferred ready read.
func (s *rigNamespaceStore) ReadyPreferCache(query beads.ReadyQuery) ([]beads.Bead, error) {
	return s.readyGated("ready prefer cache", query, func(q beads.ReadyQuery) ([]beads.Bead, error) {
		return beads.ReadyPreferCache(s.inner, q)
	})
}

// ReadyOrdered answers a bounded ordered ready read: it re-reads with a
// growing window until the page is full or the inner store is exhausted, and
// reports ordered=true only when the page is a correct prefix of the order.
func (s *rigNamespaceStore) ReadyOrdered(query beads.ReadyQuery, order beads.ReadyOrder) ([]beads.Bead, bool, error) {
	const op = "ready ordered"
	readable, gerr := s.gateReadyQuery(query)
	if !readable {
		return []beads.Bead{}, query.Limit > 0, s.partial(op, gerr)
	}
	limit := query.Limit
	if limit <= 0 {
		rows, ordered, err := beads.ReadyOrdered(s.inner, query, order)
		kept, ferr := s.filtered(op, rows, err)
		return kept, ordered, ferr
	}
	window := limit
	for round := 0; ; round++ {
		q := query
		q.Limit = window
		rows, ordered, err := beads.ReadyOrdered(s.inner, q, order)
		kept, ferr := s.filtered(op, rows, err)
		if err != nil || !ordered {
			// An error, or the inner store's whole ready set in its own
			// order: either way not a cut page.
			return kept, false, ferr
		}
		if len(kept) >= limit {
			return kept[:limit], true, ferr
		}
		if len(rows) < window {
			return kept, true, ferr
		}
		if round+1 >= rigNamespaceRefillRounds || window > (1<<30)/rigNamespaceRefillFactor {
			// beads.ReadyOrdered with no limit reads the whole set.
			q.Limit = 0
			rows, _, err := beads.ReadyOrdered(s.inner, q, order)
			kept, ferr := s.filtered(op, rows, err)
			return kept, false, ferr
		}
		window *= rigNamespaceRefillFactor
	}
}

// CachedList filters the cache-only list read. A read the guard cannot
// answer completely (a classification failure) is declined, so the caller
// takes its live path, which reports the failure.
func (s *rigNamespaceStore) CachedList(query beads.ListQuery) ([]beads.Bead, bool) {
	rows, ok, err := s.cachedListChecked(query)
	if err != nil || !ok {
		return nil, false
	}
	return rows, true
}

// cachedListChecked is CachedList that tells a declined read (ok=false, nil
// error) from one the guard could not answer completely (a partial-result
// error): a caller with no live fallback must not read the latter as "no
// rows".
func (s *rigNamespaceStore) cachedListChecked(query beads.ListQuery) ([]beads.Bead, bool, error) {
	cache, ok := beads.As[interface {
		CachedList(beads.ListQuery) ([]beads.Bead, bool)
	}](s.inner)
	if !ok {
		return nil, false, nil
	}
	rows, err := s.listGated("cached list", query, func(q beads.ListQuery) ([]beads.Bead, error) {
		rows, ok := cache.CachedList(q)
		if !ok {
			return nil, errRigNamespaceCacheDeclined
		}
		return rows, nil
	})
	switch {
	case errors.Is(err, errRigNamespaceCacheDeclined):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return rows, true, nil
}

// Count answers 0 for an unadmitted identity predicate without reading and
// ErrCountUnsupported otherwise, so callers count what List returns, which
// the guard filters. A read by id or parent is counted by the inner store,
// as List passes it through.
func (s *rigNamespaceStore) Count(ctx context.Context, query beads.ListQuery, excludeTypes ...string) (int, error) {
	q, readable, gerr := s.gateListQuery(query)
	if gerr != nil {
		return 0, fmt.Errorf("counting beads through the rig namespace guard: %w", errors.Join(beads.ErrCountUnsupported, gerr))
	}
	if !readable {
		return 0, nil
	}
	if isStructuralListQuery(q) {
		if counter, ok := beads.As[beads.Counter](s.inner); ok {
			return counter.Count(ctx, q, excludeTypes...)
		}
	}
	return 0, fmt.Errorf("counting beads through the rig namespace guard: %w", beads.ErrCountUnsupported)
}

// GetExactBatch passes the batched read by id through.
func (s *rigNamespaceStore) GetExactBatch(ids []string) (map[string]beads.Bead, []string, error) {
	getter, ok := beads.As[beads.ExactBatchGetter](s.inner)
	if !ok {
		return nil, nil, beads.ErrExactBatchGetUnsupported
	}
	return getter.GetExactBatch(ids)
}

// DepListBatch passes the batched dependency read through.
func (s *rigNamespaceStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	batch, ok := beads.DepListBatchFor(s.inner)
	if !ok {
		return nil, beads.ErrDepListBatchUnsupported
	}
	return batch.DepListBatch(ids)
}

// DepMetadata passes the edge-payload read through. An inner store without
// it is an error, not "no payload".
func (s *rigNamespaceStore) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	reader, ok := beads.As[beads.DepMetadataReader](s.inner)
	if !ok {
		return "", false, fmt.Errorf("reading dependency metadata %s -> %s: guarded store over %T: %w", issueID, dependsOnID, s.inner, beads.ErrDepMetadataUnsupported)
	}
	return reader.DepMetadata(issueID, dependsOnID)
}

// DepMetadataAbsentByProtocol forwards what the inner store declares about
// its protocol, as the bead-policy wrapper above the guard does. It is a
// declaration about the protocol, not a read, so it names no row.
func (s *rigNamespaceStore) DepMetadataAbsentByProtocol() (string, bool) {
	return beads.DepMetadataAbsent(s.inner)
}

// ReadyGraphOnlyHandle exposes the inner store's graph-only ready read,
// filtered, when it has one.
func (s *rigNamespaceStore) ReadyGraphOnlyHandle() (beads.GraphOnlyReadyStore, bool) {
	inner, ok := beads.GraphOnlyReadyFor(s.inner)
	if !ok {
		return nil, false
	}
	return rigNamespaceGraphOnlyReady{s: s, inner: inner}, true
}

type rigNamespaceGraphOnlyReady struct {
	s     *rigNamespaceStore
	inner beads.GraphOnlyReadyStore
}

func (g rigNamespaceGraphOnlyReady) ReadyGraphOnly(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return g.s.readyGated("ready graph only", firstReadyQuery(query), func(q beads.ReadyQuery) ([]beads.Bead, error) {
		return g.inner.ReadyGraphOnly(q)
	})
}

// Handles returns filtered cached and live readers over the inner store's
// handles and the guard as the writer. The inner store's own Handles are
// never forwarded: their Writer would bypass the guard.
func (s *rigNamespaceStore) Handles() beads.StoreHandles {
	inner := beads.HandlesFor(s.inner)
	return beads.StoreHandles{
		Cached: rigNamespaceReader{s: s, inner: inner.Cached},
		Live:   rigNamespaceReader{s: s, inner: inner.Live},
		Writer: s,
	}
}

// rigNamespaceReader filters one of the inner store's read handles.
// beads.CachedReader and beads.LiveReader have one method set, so one type
// serves both.
type rigNamespaceReader struct {
	s     *rigNamespaceStore
	inner beads.CachedReader
}

func (r rigNamespaceReader) Get(id string) (beads.Bead, error) { return r.inner.Get(id) }

func (r rigNamespaceReader) List(query beads.ListQuery) ([]beads.Bead, error) {
	return r.s.listGated("list", query, r.inner.List)
}

func (r rigNamespaceReader) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return r.s.readyGated("ready", firstReadyQuery(query), func(q beads.ReadyQuery) ([]beads.Bead, error) {
		return r.inner.Ready(q)
	})
}

func (r rigNamespaceReader) DepList(id, direction string) ([]beads.Dep, error) {
	return r.inner.DepList(id, direction)
}

// HasResidentOutside forwards the id-namespace census unfiltered: a filtered
// census could report a store clean while it still holds rows. A store with
// no census gets ErrNamespaceCensusUnsupported, never a verdict.
func (s *rigNamespaceStore) HasResidentOutside(prefixes []string) (bool, error) {
	census, ok := beads.NamespaceCensusFor(s.inner)
	if !ok {
		return false, fmt.Errorf("censusing the id namespaces of the guarded store over %T: %w", s.inner, beads.ErrNamespaceCensusUnsupported)
	}
	return census.HasResidentOutside(prefixes)
}

// NamespaceCensusHandle exposes the inner store's census only when it has
// one, so the structural HasResidentOutside above never advertises a census
// nobody can answer.
func (s *rigNamespaceStore) NamespaceCensusHandle() (beads.NamespaceCensus, bool) {
	return beads.NamespaceCensusFor(s.inner)
}

// ---------------------------------------------------------------------------
// Pass-through store facts.

// IDPrefix forwards the inner store's id prefix (cache prefix keying, the
// operation journal's prefix discovery).
func (s *rigNamespaceStore) IDPrefix() string {
	if p, ok := beads.As[interface{ IDPrefix() string }](s.inner); ok {
		return p.IDPrefix()
	}
	return ""
}

// AtomicTx forwards whether the inner store's Tx is atomic.
func (s *rigNamespaceStore) AtomicTx() bool {
	a, ok := beads.As[interface{ AtomicTx() bool }](s.inner)
	return ok && a.AtomicTx()
}

// ReadOnly forwards whether the inner store refuses writes.
func (s *rigNamespaceStore) ReadOnly() bool {
	r, ok := beads.As[interface{ ReadOnly() bool }](s.inner)
	return ok && r.ReadOnly()
}

// SawRows forwards the inner store's row witness.
func (s *rigNamespaceStore) SawRows() bool {
	w, ok := beads.As[beads.RowWitness](s.inner)
	return ok && w.SawRows()
}

// SupportsEphemeralGraphApply forwards the inner store's answer.
func (s *rigNamespaceStore) SupportsEphemeralGraphApply() bool {
	e, ok := beads.As[beads.EphemeralGraphApplyStore](s.inner)
	return ok && e.SupportsEphemeralGraphApply()
}

// ProxiedStore hands out nothing: the proxied split store's view reaches its
// leaves (BdLeaf), which would bypass the guard. Diagnostics of a guarded rig
// report it as not proxied.
func (s *rigNamespaceStore) ProxiedStore() (beads.ProxiedStoreView, bool) {
	return nil, false
}

// WaitForParentProjection forwards the inner store's projection wait; an
// inner store without one does not wait.
func (s *rigNamespaceStore) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	waiter, ok := beads.As[beads.ParentProjectionWaiter](s.inner)
	if !ok {
		return nil
	}
	return waiter.WaitForParentProjection(ctx, id, oldParentID, newParentID)
}

// ConditionalWritesModeSource points the conditional-writes resolver at the
// inner store for the mode, liveness, capability and degrade latch, while the
// writer it resolves stays the guard. A resolve TARGET would hand back the
// inner store as the writer and every fenced write would bypass the guard.
func (s *rigNamespaceStore) ConditionalWritesModeSource() beads.Store { return s.inner }

// CloseStore closes the inner store: the cache's reconciler is stopped and
// its backing closed, as closeBeadStoreHandle does for an unguarded store.
func (s *rigNamespaceStore) CloseStore() error { return closeBeadStoreHandle(s.inner) }

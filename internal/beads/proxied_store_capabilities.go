package beads

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// The ProxiedStore capability surface.
//
// # Why this is a file of its own, and a compile-time contract
//
// Production code discovers optional store capabilities by DIRECT type
// assertion. A wrapper that embedded the Store interface would compile, pass
// every Store-shaped test, and silently strip every one of them — which is the
// hazard splittest/strict_store.go's doc calls out and the reason that type
// carries the same block of `var _ ...` lines. The list below is the executable
// statement of what a caller holding a *ProxiedStore may discover, and removing a
// method from it fails the build rather than a distant behavior test.
//
// # The three rules the routing follows
//
//  1. A capability that READS is forwarded to the CURRENT read leaf — native
//     while it serves, bd after a stand-down. That includes CachingStore's
//     package-private discovery interfaces, which is the whole reason the split is
//     a wrapper and not a hand-written leaf.
//  2. A capability that WRITES resolves on the bd leaf, always. PR2's native
//     handle is read-only latched (P2-06), so even a mis-routed write refuses —
//     but the routing is what makes the refusal unreachable rather than merely
//     caught.
//  3. A capability the wrapper cannot honestly claim is NOT implemented. Two of
//     those are load-bearing and are documented at their (absent) place below:
//     graph-apply (H4) and CachingStore's dependency-snapshot shortcut.
var (
	_ Store                                 = (*ProxiedStore)(nil)
	_ AtomicTxStore                         = (*ProxiedStore)(nil)
	_ BatchDeleter                          = (*ProxiedStore)(nil)
	_ ConditionalAssignmentReleaser         = (*ProxiedStore)(nil)
	_ ConditionalWriterHandleProvider       = (*ProxiedStore)(nil)
	_ ConditionalWritesResolveTargeter      = (*ProxiedStore)(nil)
	_ Counter                               = (*ProxiedStore)(nil)
	_ DepMetadataReader                     = (*ProxiedStore)(nil)
	_ ForeignIDCreator                      = (*ProxiedStore)(nil)
	_ GraphApplyHandleProvider              = (*ProxiedStore)(nil)
	_ MetadataCASWriterHandleProvider       = (*ProxiedStore)(nil)
	_ AtomicConditionalCloserHandleProvider = (*ProxiedStore)(nil)
	_ ParentProjectionWaiter                = (*ProxiedStore)(nil)
	_ RowWitness                            = (*ProxiedStore)(nil)
	_ StorageCreateStore                    = (*ProxiedStore)(nil)
	_ conditionalWritesModeCarrier          = (*ProxiedStore)(nil)
	_ listDependencyCompletenessStore       = (*ProxiedStore)(nil)
	_ readyProjectionEnrichmentStore        = (*ProxiedStore)(nil)
	_ interface{ IDPrefix() string }        = (*ProxiedStore)(nil)
	_ interface{ Backing() Store }          = (*ProxiedStore)(nil)
	_ interface{ CloseStore() error }       = (*ProxiedStore)(nil)
	_ interface {
		DepListBatch(ids []string) (map[string][]Dep, error)
	} = (*ProxiedStore)(nil)
)

// IDPrefix reports the id namespace this store mints under.
//
// It comes from the READ leaf, which is the one that read it out of the database
// (native_dolt_store.go reads the issue_prefix config row at open) rather than
// out of gc's own config. The two cannot disagree on an admitted scope: H10's
// bound is that admission refuses with prefix_mismatch when they do, so by the
// time a ProxiedStore exists the question has already been settled.
func (s *ProxiedStore) IDPrefix() string {
	if prefix, ok := s.readLeaf().(interface{ IDPrefix() string }); ok {
		return prefix.IDPrefix()
	}
	return ""
}

// Backing returns the READ leaf, which is what beads.ReadyLive asks for
// (live_ready.go calls .Ready() on whatever this returns). Answering with the
// write leaf would send every live readiness read through a bd fork while a
// perfectly good native pool sat idle.
func (s *ProxiedStore) Backing() Store {
	return s.readLeaf()
}

// Count answers a bounded count from the read leaf.
//
// Both leaves' answers are honest, but only one is cheap: *NativeDoltStore has a
// Count (native_dolt_store_count.go) and *BdStore has none, so a demoted store
// reports ErrCountUnsupported and its callers fall back to List — which is
// exactly what a proxied scope does today.
func (s *ProxiedStore) Count(ctx context.Context, query ListQuery, excludeTypes ...string) (int, error) {
	counter, ok := s.readLeaf().(Counter)
	if !ok {
		return 0, fmt.Errorf("counting beads: proxied store read leaf: %w", ErrCountUnsupported)
	}
	n, err := counter.Count(ctx, query, excludeTypes...)
	return n, s.classifyReadError(err)
}

// DepMetadata reads an edge's payload from the read leaf. Id-scoped, so H5's
// relocated-class guard runs first.
//
// A leaf without the capability gets an ERROR rather than ("", false, nil): a
// caller that refuses on uncertainty — the infra-class migration is the live one
// — reads a stripped capability as UNABLE TO ANSWER, and conflating that with
// "carries nothing" is the bug dep_metadata.go's doc comment records.
func (s *ProxiedStore) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	leaf := s.readLeaf()
	if s.nativeLeaf() != nil {
		if err := s.guardRelocatedIDs("dep metadata "+issueID, issueID, dependsOnID); err != nil {
			return "", false, err
		}
	}
	reader, ok := leaf.(DepMetadataReader)
	if !ok {
		return "", false, fmt.Errorf("reading dependency metadata %s -> %s: proxied store read leaf %T exposes no edge-payload read", issueID, dependsOnID, leaf)
	}
	payload, carried, err := reader.DepMetadata(issueID, dependsOnID)
	return payload, carried, s.classifyReadError(err)
}

// DepListBatch reads several beads' "down" edges.
//
// The native leaf has no batched read, so the fallback loops DepList THROUGH THIS
// STORE rather than through a leaf: that keeps the relocated-class guard and the
// demotion routing live on every id, and on the native lane it costs zero
// subprocesses whatever the batch size.
func (s *ProxiedStore) DepListBatch(ids []string) (map[string][]Dep, error) {
	if batch, ok := s.readLeaf().(interface {
		DepListBatch(ids []string) (map[string][]Dep, error)
	}); ok {
		deps, err := batch.DepListBatch(ids)
		return deps, s.classifyReadError(err)
	}
	result := make(map[string][]Dep, len(ids))
	for _, id := range ids {
		deps, err := s.DepList(id, "down")
		if err != nil {
			return nil, fmt.Errorf("listing deps for %q: %w", id, err)
		}
		result[id] = deps
	}
	return result, nil
}

// SawRows implements RowWitness, and it ORs the two leaves.
//
// The capability is one-directional by contract: true proves the ledger holds
// rows, false proves nothing. Both leaves read the SAME ledger, so a row either
// of them has already been handed is proof for the store as a whole — and a
// wrapper that reported only the current read leaf would forget, on demotion,
// that it had been proven populated a second earlier.
func (s *ProxiedStore) SawRows() bool {
	if native := s.nativeLeaf(); native != nil && native.SawRows() {
		return true
	}
	witness, ok := s.writeLeaf().(RowWitness)
	return ok && witness.SawRows()
}

// listIncludesCompleteDependencies is the first of CachingStore's package-private
// discovery interfaces (caching_store.go 1440-1450), and the reason the split is
// a wrapper.
//
// It answers whether the rows List returns already carry their complete "down"
// dependency set, which decides whether the cache can serve DepList and readiness
// from the snapshot it already has or must spend a read per bead. The native leaf
// answers true unconditionally; the bd leaf answers with a WITNESSED value it
// derives from rows bd already returned. Either way the answer belongs to the leaf
// that produced the rows, so it follows the read leaf.
func (s *ProxiedStore) listIncludesCompleteDependencies() bool {
	completeness, ok := s.readLeaf().(listDependencyCompletenessStore)
	return ok && completeness.listIncludesCompleteDependencies()
}

// enrichReadyProjectionForCache is the second, and it is the one whose loss is
// silent and expensive: it fills bd's denormalized is_blocked column on a cached
// snapshot, and a cache that cannot get it declines EVERY readiness read to a
// live backing store. It follows the read leaf, which is the leaf that can answer
// it without a subprocess.
func (s *ProxiedStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	enricher, ok := s.readLeaf().(readyProjectionEnrichmentStore)
	if !ok {
		return items, nil
	}
	enriched, err := enricher.enrichReadyProjectionForCache(items)
	return enriched, s.classifyReadError(err)
}

// The third — cacheDependencySnapshotStore (dependencySnapshotForCache) — is
// DELIBERATELY NOT IMPLEMENTED, and this comment is the reason.
//
// CachingStore.fetchDepsForBeads tries the three in strict order
// (caching_store.go): a backing that implements the SNAPSHOT interface preempts
// the completeness interface entirely. Neither of this store's leaves implements
// it — only DoltliteReadStore does — so a wrapper that claimed it would have
// nothing to forward to, would preempt the completeness answer above, and would
// therefore convert a complete inline dependency snapshot into a fabricated
// empty one. Claiming a capability in order to answer "no" is strictly worse than
// not claiming it, and this is the interface where that is measurable.

// WaitForParentProjection waits for the WRITE leaf's parent-child view to reflect
// a reparent.
//
// It is asserted after a successful Update, and the Update ran on the bd leaf, so
// the projection that has to converge is bd's. Asking the native leaf would be
// waiting on the wrong view of the right database.
func (s *ProxiedStore) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	waiter, ok := s.writeLeaf().(ParentProjectionWaiter)
	if !ok {
		return nil
	}
	return waiter.WaitForParentProjection(ctx, id, oldParentID, newParentID)
}

// AtomicTx reports the WRITE leaf's transactional guarantee. This is H3.
//
// *NativeDoltStore.AtomicTx is true and BdStore's Tx is staged and non-atomic, and
// Tx runs on the bd leaf. Reporting true because one of the two leaves can roll
// back would promise callers an all-or-nothing multi-write swap that this store
// cannot perform — and the Store.Tx contract says a caller who needs one must
// either require such a store or sequence its writes to stay recoverable, so the
// lie would be acted on.
func (s *ProxiedStore) AtomicTx() bool {
	return StoreSupportsAtomicTx(s.writeLeaf())
}

// GraphApplyHandle hands out the WRITE leaf's applier, and this is H4 — the one
// capability whose shape is a direct PR2 constraint.
//
// *NativeDoltStore declares GraphApplyStore, StorageGraphApplyStore AND
// EphemeralGraphApplyStore, and cmd/gc's wrapStoreWithBeadPolicies resolves
// beads.GraphApplyFor(store) at WRAP time and keeps the answer. So the wrapper
// implementing any of those three would route every policy-selected ephemeral
// graph create — every molecule pour — into a native WRITE on a database bd owns.
//
// Implementing the HANDLE PROVIDER instead of the interfaces is what keeps
// beads.GraphApplyFor working through the wrapper without the wrapper being an
// applier: the caller gets the bd leaf's applier, with the bd leaf's own storage
// and ephemeral claims intact, and a type assertion for StorageGraphApplyStore on
// the *store* fails — which is the outcome the plan requires.
func (s *ProxiedStore) GraphApplyHandle() (GraphApplyStore, bool) {
	return GraphApplyFor(s.writeLeaf())
}

// ConditionalWriterHandle forwards the write leaf's conditional-write capability
// the same way, so beads.ConditionalWriterFor resolves through the wrapper
// without a false claim.
//
// H9 (the revision token a CAS sends came from a NATIVE Get, and the write that
// carries it runs on bd) is pinned by an integration row rather than by code
// here: the two are the same column by documentation, and the plan's owner
// question Q2 is a beads-side ask. Nothing in this file assumes an answer.
func (s *ProxiedStore) ConditionalWriterHandle() (ConditionalWriter, bool) {
	return ConditionalWriterFor(s.writeLeaf())
}

// MetadataCASWriterHandle forwards the write leaf's metadata compare-and-swap.
func (s *ProxiedStore) MetadataCASWriterHandle() (MetadataCASWriter, bool) {
	return MetadataCASWriterFor(s.writeLeaf())
}

// AtomicConditionalCloserHandle forwards the write leaf's atomic terminal write.
func (s *ProxiedStore) AtomicConditionalCloserHandle() (AtomicConditionalCloser, bool) {
	return AtomicConditionalCloserFor(s.writeLeaf())
}

// ConditionalWritesResolveTarget declares the WRITE leaf as the
// conditional-writes resolution target.
//
// Without it a resolve through this wrapper would collapse to unset -> legacy
// silently, which conditional_writes_resolve.go names as the one optional
// capability whose loss does not fail loudly — and under `require` it is the
// exact silent fallback the seam exists to make inexpressible.
func (s *ProxiedStore) ConditionalWritesResolveTarget() Store {
	return s.writeLeaf()
}

// ReleaseIfCurrent releases an assignment on the write leaf, conditionally.
func (s *ProxiedStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := s.writeLeaf().(ConditionalAssignmentReleaser)
	if !ok {
		return false, ErrConditionalReleaseUnsupported
	}
	var released bool
	err := s.withMutation("release-if-current "+id, func(Store) error {
		var err error
		released, err = releaser.ReleaseIfCurrent(id, expectedAssignee)
		return err
	})
	return released, err
}

// DeleteBatch removes a batch on the write leaf.
//
// A leaf without the capability ERRORS rather than falling back to per-id Delete:
// the orphan-preserving contract is the point of the batch, and the per-id
// fallback does not have it. Same rule as beadPolicyStore's.
func (s *ProxiedStore) DeleteBatch(ids []string) error {
	deleter, ok := s.writeLeaf().(BatchDeleter)
	if !ok {
		return fmt.Errorf("proxied store: write leaf %T does not support orphan-preserving batch delete: %w",
			s.writeLeaf(), ErrBatchDeleteUnsupported)
	}
	return s.withMutation("delete-batch", func(Store) error { return deleter.DeleteBatch(ids) })
}

// CreateWithForeignID creates a bead KEEPING another store's id, on the write
// leaf. It is the class-store migration's copy path, and it is a write like any
// other.
func (s *ProxiedStore) CreateWithForeignID(b Bead) (Bead, error) {
	creator, ok := s.writeLeaf().(ForeignIDCreator)
	if !ok {
		return s.Create(b)
	}
	var out Bead
	err := s.withMutation("create-foreign-id", func(Store) error {
		created, err := creator.CreateWithForeignID(b)
		out = created
		return err
	})
	return out, err
}

// CreateWithStorage creates a bead in a policy-selected storage tier, on the
// write leaf.
//
// Claimed because *BdStore implements it, so a proxied scope has this capability
// today: dropping it would send wisp and no-history creates back through the
// flag-based Create fallback and silently change which table a molecule's steps
// land in.
func (s *ProxiedStore) CreateWithStorage(b Bead, storage StorageClass) (Bead, error) {
	creator, ok := s.writeLeaf().(StorageCreateStore)
	if !ok {
		return s.Create(b)
	}
	var out Bead
	err := s.withMutation("create-with-storage", func(Store) error {
		created, err := creator.CreateWithStorage(b, storage)
		out = created
		return err
	})
	return out, err
}

// The conditional-writes stamp surface.
//
// The factory stamps the resolved city-global mode onto the store it returns, and
// this wrapper is that store. The stamp lands on BOTH leaves rather than being
// held here: the bd leaf is where conditional writes resolve (see
// ConditionalWritesResolveTarget) and must carry the mode, and the native leaf
// must carry it too because PR3 moves writes onto it and a leaf that was never
// stamped would resolve unset -> legacy at exactly the moment enforcement was
// supposed to start.
//
// stampConditionalWritesMode reports whether the stamp LANDED. It reports the
// WRITE leaf's answer, because a stamp that reached only the read leaf has not
// reached the store that writes, and the factory has to be told that rather than
// believing it took.
func (s *ProxiedStore) stampConditionalWritesMode(mode gate.Mode, defaulted bool) bool {
	landed := false
	if carrier, ok := s.writeLeaf().(conditionalWritesModeCarrier); ok {
		landed = carrier.stampConditionalWritesMode(mode, defaulted)
	}
	if native := s.nativeLeaf(); native != nil {
		native.stampConditionalWritesMode(mode, defaulted)
	}
	return landed
}

// conditionalWritesMode reads the WRITE leaf's stamp, for the same reason the
// stamp reports its answer.
func (s *ProxiedStore) conditionalWritesMode() (gate.Mode, bool) {
	if carrier, ok := s.writeLeaf().(conditionalWritesModeCarrier); ok {
		return carrier.conditionalWritesMode()
	}
	return gate.ModeUnset, false
}

// noteConditionalDegradeOnce forwards the once-per-store latch to the write leaf,
// so the degrade is counted once for the store that would have degraded.
func (s *ProxiedStore) noteConditionalDegradeOnce() bool {
	if carrier, ok := s.writeLeaf().(conditionalWritesModeCarrier); ok {
		return carrier.noteConditionalDegradeOnce()
	}
	return false
}

// setConditionalWritesDegradeCallback installs the factory's emission callback on
// both leaves, so whichever one degrades reaches the same bus.
func (s *ProxiedStore) setConditionalWritesDegradeCallback(cb func(ConditionalWritesDegrade)) {
	if carrier, ok := s.writeLeaf().(conditionalWritesModeCarrier); ok {
		carrier.setConditionalWritesDegradeCallback(cb)
	}
	if native := s.nativeLeaf(); native != nil {
		native.setConditionalWritesDegradeCallback(cb)
	}
}

// fireConditionalWritesDegradeOnce forwards the emission to the write leaf, whose
// latch noteConditionalDegradeOnce consumes.
func (s *ProxiedStore) fireConditionalWritesDegradeOnce(d ConditionalWritesDegrade) {
	if carrier, ok := s.writeLeaf().(conditionalWritesModeCarrier); ok {
		carrier.fireConditionalWritesDegradeOnce(d)
	}
}

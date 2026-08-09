// Package splittest provides strict, prefix-disjoint bead-store test doubles
// for the work/coordination-class store split.
//
// # Why this package exists
//
// The split-store bug class — code resolving "which store owns this class of
// bead" differently on different paths — is the one this repo keeps paying
// for. beads.MemStore hides it, because MemStore is lenient exactly where the
// production bd/Dolt and SQLite backends are strict:
//
//   - MemStore.DepAdd appends an edge without resolving either endpoint, so a
//     cross-store dependency that hard-fails in production ("no issue found")
//     silently succeeds in a test.
//   - MemStore.Create mints over whatever id it was handed, so a graph-prefixed
//     bead can be "created" inside a work store without a peep — and the test
//     never learns that the row it thinks it made does not exist.
//
// A test written against those leniencies passes while the production path it
// models hard-fails. StrictStore closes both gaps at the LEAF store, so a
// policy or class wrapper layered on top keeps the checks live on every path
// (cmd/gc's beadPolicyStore does not override DepAdd or Create's id handling —
// the embedded Store interface delegates them straight down).
//
// # Tier transparency is a requirement, not a bonus
//
// Production molecules materialize as ephemeral wisps carrying pinned
// <prefix>-wisp-<suffix> ids, not store-minted main-tier ids. A double that
// clobbers explicit ids cannot express the wisp tier at all, so the kit's
// leaves honor a pinned in-prefix id (beads.MemStore.HonorExplicitIDs) and the
// strict wrapper is otherwise tier-transparent: ephemeral beads create, read,
// and dep-link through it exactly as through the leaf. Fixtures still have to
// ASK for the wisp tier the way tier-aware production code does
// (ListQuery.TierMode / ReadyQuery.TierMode set to beads.TierWisps or
// beads.TierBoth); the kit is a leaf-level pair with no policy wrapper
// expanding reads.
package splittest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// StrictStore wraps a LEAF beads.Store and makes the leniencies of in-process
// stores fail loudly, the way the bd/Dolt and SQLite backends fail in
// production:
//
//   - DepAdd resolves BOTH endpoints in this store first and rejects a missing
//     one with a bd-shaped "no issue found" error, preserving the parent-child
//     short-circuit exactly as beads.BdStore.DepAdd does.
//   - Create (including Tx creates and CreateWithStorage on storage-capable
//     leaves) rejects an explicit id whose prefix segment differs from the
//     store's declared id prefix, mirroring bd's rejection of a mismatched
//     --id without --force, and fails loudly when the leaf hands back an id
//     other than the one that was asked for.
//
// Reads are untouched. Optional leaf capabilities that production code
// discovers by type-assertion are forwarded (see the method set and the
// "deliberately dropped" notes on Strict).
type StrictStore struct {
	beads.Store
	// prefix is the normalized id-prefix segment this store mints under
	// ("gcg" for a graph-class store). Empty means no declared namespace: the
	// create guard is inert and IDPrefix reports "", which storeref skips —
	// the same routing behavior as a store without the accessor.
	prefix string
}

// Compile-time capability contracts.
var (
	_ beads.Store                            = (*StrictStore)(nil)
	_ beads.ConditionalAssignmentReleaser    = (*StrictStore)(nil)
	_ beads.ConditionalWriterHandleProvider  = (*StrictStore)(nil)
	_ beads.ConditionalWritesResolveTargeter = (*StrictStore)(nil)
	_ beads.BatchDeleter                     = (*StrictStore)(nil)
	_ beads.ForeignIDCreator                 = (*StrictStore)(nil)
	_ beads.Counter                          = (*StrictStore)(nil)
	_ beads.GraphApplyHandleProvider         = (*StrictStore)(nil)
	_ beads.AtomicTxStore                    = (*StrictStore)(nil)
	_ beads.ParentProjectionWaiter           = (*StrictStore)(nil)
	_ storeref.HasIDPrefix                   = (*StrictStore)(nil)
)

// Strict wraps a leaf store in the strict split-store checks. The store's id
// prefix is taken from the leaf's own storeref.HasIDPrefix accessor when it
// exposes one; use StrictWithPrefix for leaves that do not (beads.MemStore).
//
// Wrap the LEAF store, not a policy wrapper: cmd/gc's beadPolicyStore does not
// override DepAdd, so a strict leaf keeps the dependency check live on every
// path through the policy stack, while a strict wrapper AROUND the policy store
// would be bypassed by any code holding the inner store.
//
// Capability forwarding: production code discovers optional store capabilities
// by direct type-assertion, and an interface-embedding wrapper silently strips
// everything outside beads.Store. StrictStore forwards Handles (with a strict
// Writer), IDPrefix, graph-apply and conditional writes (via the
// beads.GraphApplyHandleProvider / beads.ConditionalWriterHandleProvider
// handles, so beads.GraphApplyFor and beads.ConditionalWriterFor keep working
// without a false claim), the conditional-writes resolve target, Counter,
// ConditionalAssignmentReleaser, BatchDeleter, ForeignIDCreator, DepListBatch,
// CloseStore, AtomicTx, Backing, and WaitForParentProjection.
// StorageCreateStore is forwarded only when the leaf implements it (a
// capability-preserving variant type), so the wrapper never falsely claims
// CreateWithStorage for leaves without it — the storage-policy fallback in
// cmd/gc must keep firing for MemStore leaves.
//
// Deliberately dropped: beads.StorageGraphApplyStore (asserted on the
// graph-apply HANDLE, which is forwarded verbatim, never on the store itself)
// and any bd-only unexported surfaces. Graph-apply plans bypass the strict
// DepAdd guard by construction: a plan's nodes and edges land atomically in ONE
// store, and real appliers validate edges internally; MemStore leaves have no
// applier at all. Create-time deps (beads.Bead.Needs) are also left
// unstrictened: molecule step Needs carry formula step refs, not bead ids, on
// some fixture paths, and bd's --deps validation behavior is not pinned by a
// contract test, so enforcing here would reject valid fixtures rather than
// catch real cross-store bugs.
func Strict(s beads.Store) beads.Store {
	prefix := ""
	if p, ok := s.(storeref.HasIDPrefix); ok {
		prefix = p.IDPrefix()
	}
	return newStrict(s, prefix)
}

// StrictWithPrefix wraps a leaf store like Strict, additionally declaring the
// id-prefix segment the store mints under (e.g. "gcg" for a graph-class store).
// The declared prefix arms the foreign-prefix create guard and is reported
// through IDPrefix for storeref prefix routing. Use it for leaves that do not
// expose storeref.HasIDPrefix themselves (beads.MemStore).
func StrictWithPrefix(s beads.Store, prefix string) beads.Store {
	return newStrict(s, prefix)
}

// newStrict builds the wrapper, choosing the StorageCreateStore-preserving
// variant when (and only when) the leaf implements CreateWithStorage.
func newStrict(s beads.Store, prefix string) beads.Store {
	if s == nil {
		return nil
	}
	strict := &StrictStore{Store: s, prefix: normalizePrefix(prefix)}
	if storage, ok := s.(beads.StorageCreateStore); ok {
		return &strictStorageStore{StrictStore: strict, storage: storage}
	}
	return strict
}

// normalizePrefix mirrors the beads package's internal id-prefix normalization
// (CachingStore): lowercase, trimmed, no trailing dashes, so "GCG-" and "gcg"
// declare the same namespace.
func normalizePrefix(prefix string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(prefix)), "-")
}

// Create rejects an explicit id outside the store's declared namespace before
// delegating, and fails loudly if the leaf did not return the id that was
// asked for. A post-check failure leaves the offending row in the leaf — this
// is a test double, and loud beats tidy.
func (s *StrictStore) Create(b beads.Bead) (beads.Bead, error) {
	if err := s.guardExplicitID(b.ID); err != nil {
		return beads.Bead{}, err
	}
	created, err := s.Store.Create(b)
	if err != nil {
		return beads.Bead{}, err
	}
	if err := s.checkCreatedID(b.ID, created); err != nil {
		return beads.Bead{}, err
	}
	return created, nil
}

// CreateWithForeignID creates a bead KEEPING an id from another store's
// namespace. It DELIBERATELY bypasses the foreign-prefix guard: this capability
// IS the forced path (beads.BdStore passes --force), used by the class-store
// migration to keep a legacy id when copying a bead into a relocated-class
// store. Leaves with their own forced create serve it; the rest get a guard-free
// Create, with the id round-trip still verified so a clobbering leaf cannot pass
// this off as a success.
func (s *StrictStore) CreateWithForeignID(b beads.Bead) (beads.Bead, error) {
	if strings.TrimSpace(b.ID) == "" {
		return beads.Bead{}, errors.New("creating bead with foreign id: empty id")
	}
	create := s.Store.Create
	if creator, ok := s.Store.(beads.ForeignIDCreator); ok {
		create = creator.CreateWithForeignID
	}
	created, err := create(b)
	if err != nil {
		return beads.Bead{}, err
	}
	if created.ID != b.ID {
		return beads.Bead{}, fmt.Errorf("creating bead with foreign id %q: leaf store %T returned id %q instead; it does not honor an explicit id and cannot model a forced foreign-prefix create", b.ID, s.Store, created.ID)
	}
	return created, nil
}

// DepAdd resolves both endpoints in THIS store before delegating, mirroring the
// bd backend, which hard-fails `bd dep add` when either id does not resolve in
// the target database. beads.MemStore.DepAdd appends unconditionally — the
// exact leniency that lets a cross-store dependency (work bead → graph bead or
// vice versa) succeed in-process while production wedges on "no issue found".
//
// The parent-child short-circuit is preserved exactly as beads.BdStore.DepAdd
// has it: a parent-child dep that merely restates the bead's own ParentID
// returns nil BEFORE endpoint resolution — on a split store the parent may
// legitimately live elsewhere, and bd never sees the call.
//
// The missing-endpoint error is shaped like the bd backend's output
// ("resolving issue ID <id>: no issue found", wrapped in BdStore's "adding dep"
// context) and intentionally does NOT wrap beads.ErrNotFound: bd's real failure
// is a subprocess stderr string that callers can only classify textually, so a
// typed error here would let in-process tests pass on errors.Is checks that
// production could never satisfy.
func (s *StrictStore) DepAdd(issueID, dependsOnID, depType string) error {
	if depType == "parent-child" {
		bead, err := s.Get(issueID)
		if err == nil && bead.ParentID == dependsOnID {
			return nil
		}
	}
	for _, id := range []string{issueID, dependsOnID} {
		if _, err := s.Get(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return fmt.Errorf("adding dep %s→%s: resolving issue ID %s: no issue found (endpoint not in this store — cross-store dependency?)", issueID, dependsOnID, id)
			}
			return fmt.Errorf("adding dep %s→%s: resolving issue ID %s: %w", issueID, dependsOnID, id, err)
		}
	}
	return s.Store.DepAdd(issueID, dependsOnID, depType)
}

// Tx wraps the leaf transaction so creates inside the callback go through the
// same explicit-id guard as Create — without this, Tx.Create would be an
// unguarded side door for foreign-prefix rows.
func (s *StrictStore) Tx(commitMsg string, fn func(beads.Tx) error) error {
	return s.Store.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&strictTx{tx: tx, store: s})
	})
}

// Handles returns explicit read/write handles with this strict store as the
// Writer, so HandlesFor-discovered write paths (Writer.DepAdd, Writer.Create)
// keep the strict checks. Readers keep the leaf's native handle guarantees.
func (s *StrictStore) Handles() beads.StoreHandles {
	handles := beads.HandlesFor(s.Store)
	handles.Writer = s
	return handles
}

// IDPrefix implements storeref.HasIDPrefix, reporting the declared id-prefix
// segment ("" when none was declared or inferred — storeref.PrefixOwner skips
// empty prefixes, matching a store without the accessor).
func (s *StrictStore) IDPrefix() string {
	return s.prefix
}

// GraphApplyHandle forwards the leaf's graph-apply capability when it has one.
// Implementing beads.GraphApplyHandleProvider (instead of claiming
// beads.GraphApplyStore outright) keeps beads.GraphApplyFor working on the
// wrapper without a false claim for leaves that cannot graph-apply.
func (s *StrictStore) GraphApplyHandle() (beads.GraphApplyStore, bool) {
	return beads.GraphApplyFor(s.Store)
}

// ConditionalWriterHandle forwards the leaf's conditional-write capability the
// same way GraphApplyHandle forwards graph-apply: beads.ConditionalWriterFor
// keeps resolving through the wrapper without the wrapper claiming
// beads.ConditionalWriter for a leaf that lacks it.
func (s *StrictStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(s.Store)
}

// ConditionalWritesResolveTarget declares the wrapped leaf as the
// conditional-writes resolution target, exactly as the typed class wrappers do.
// Without it a resolve through this wrapper collapses to unset→legacy silently,
// which is the one optional capability whose loss does not fail loudly.
func (s *StrictStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

// Count forwards the leaf's beads.Counter capability. Leaves without one report
// beads.ErrCountUnsupported, signaling callers to fall back to List — the same
// contract cmd/gc's beadPolicyStore forwards.
func (s *StrictStore) Count(ctx context.Context, query beads.ListQuery, excludeTypes ...string) (int, error) {
	counter, ok := s.Store.(beads.Counter)
	if !ok {
		return 0, fmt.Errorf("counting beads: strict-wrapped store: %w", beads.ErrCountUnsupported)
	}
	return counter.Count(ctx, query, excludeTypes...)
}

// ReleaseIfCurrent forwards the leaf's conditional assignment release, or
// reports beads.ErrConditionalReleaseUnsupported when the leaf lacks it,
// matching the beadPolicyStore forwarding contract.
func (s *StrictStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := s.Store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return false, beads.ErrConditionalReleaseUnsupported
	}
	return releaser.ReleaseIfCurrent(id, expectedAssignee)
}

// DeleteBatch forwards the leaf's orphan-preserving batch delete. A leaf without
// the capability errors — never a per-id fallback, which would defeat the
// orphan-preserving contract (same rule as beadPolicyStore).
func (s *StrictStore) DeleteBatch(ids []string) error {
	deleter, ok := s.Store.(beads.BatchDeleter)
	if !ok {
		return fmt.Errorf("strict store: leaf store %T does not support orphan-preserving batch delete", s.Store)
	}
	return deleter.DeleteBatch(ids)
}

// DepListBatch forwards the leaf's batched "down" dep listing (asserted by
// internal/dispatch's scope-skip walk and the class-store migration). Leaves
// without it fall back to per-id DepList — byte-identical to the fallback those
// callers run themselves.
func (s *StrictStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	if batch, ok := s.Store.(interface {
		DepListBatch(ids []string) (map[string][]beads.Dep, error)
	}); ok {
		return batch.DepListBatch(ids)
	}
	result := make(map[string][]beads.Dep, len(ids))
	for _, id := range ids {
		deps, err := s.DepList(id, "down")
		if err != nil {
			return nil, fmt.Errorf("listing deps for %q: %w", id, err)
		}
		result[id] = deps
	}
	return result, nil
}

// CloseStore releases the leaf's backing handle when it has one (asserted by
// cmd/gc store shutdown). Leaves without one hold nothing to release.
func (s *StrictStore) CloseStore() error {
	if closer, ok := s.Store.(interface{ CloseStore() error }); ok {
		return closer.CloseStore()
	}
	return nil
}

// AtomicTx reports the LEAF's transactional guarantee — wrapping neither adds
// nor removes atomicity. False matches the conservative contract for stores that
// never implemented beads.AtomicTxStore.
func (s *StrictStore) AtomicTx() bool {
	return beads.StoreSupportsAtomicTx(s.Store)
}

// Backing forwards the leaf's live-read backing store (asserted by
// beads.ReadyLive). Nil matches a leaf without a caching layer: ReadyLive then
// falls back to the store's own Ready, which is read-only and therefore
// unaffected by strictness either way.
func (s *StrictStore) Backing() beads.Store {
	if backed, ok := s.Store.(interface{ Backing() beads.Store }); ok {
		return backed.Backing()
	}
	return nil
}

// WaitForParentProjection forwards the leaf's projection wait when it has one.
// In-process leaves apply parent updates synchronously, so their projection has
// already converged by the time a caller could ask.
func (s *StrictStore) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	if waiter, ok := s.Store.(beads.ParentProjectionWaiter); ok {
		return waiter.WaitForParentProjection(ctx, id, oldParentID, newParentID)
	}
	return nil
}

// guardExplicitID rejects a caller-supplied id outside the store's declared
// namespace, mirroring bd's rejection of a mismatched --id without --force. An
// empty id (store-minted) or an undeclared namespace passes.
func (s *StrictStore) guardExplicitID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" || s.prefix == "" || s.ownsID(id) {
		return nil
	}
	return fmt.Errorf("creating bead %q: explicit id prefix does not match store id prefix %q (bd rejects a mismatched --id without --force; use CreateWithForeignID for the forced foreign-prefix create)", id, s.prefix)
}

// checkCreatedID fails loudly when the leaf did not produce the row the caller
// asked for: a pinned id silently replaced by the leaf's own sequence (a leaf
// that does not honor explicit ids, so every wisp-shaped fixture id is a lie),
// or a minted id outside the declared namespace (a foreign-prefix row inside a
// split store — exactly the residence-invariant violation this wrapper exists
// to catch).
func (s *StrictStore) checkCreatedID(requestedID string, created beads.Bead) error {
	if requested := strings.TrimSpace(requestedID); requested != "" && created.ID != requested {
		return fmt.Errorf("store returned bead %q for an explicit create of %q: the leaf store %T clobbers pinned ids, so it cannot model a store that round-trips them (production wisps carry pinned <prefix>-wisp-<suffix> ids)", created.ID, requested, s.Store)
	}
	if s.prefix == "" || s.ownsID(created.ID) {
		return nil
	}
	return fmt.Errorf("store minted bead %q outside its declared id namespace %q: the leaf is minting under a different prefix than the one this store was declared with", created.ID, s.prefix)
}

// ownsID reports whether id sits in the declared prefix namespace, using the
// same case-insensitive segment match as storeref.PrefixOwner and CachingStore.
// Only meaningful when a prefix is declared.
func (s *StrictStore) ownsID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return strings.HasPrefix(id, s.prefix+"-")
}

// strictStorageStore is the StorageCreateStore-preserving variant of
// StrictStore, returned by the constructors only when the leaf implements
// CreateWithStorage. Keeping the claim conditional matters: production
// storage-policy code type-asserts beads.StorageCreateStore and only falls back
// to flag-based Create when the assertion fails, so an unconditional claim on a
// MemStore leaf would break wisp/no-history tier routing instead of preserving
// it.
type strictStorageStore struct {
	*StrictStore
	storage beads.StorageCreateStore
}

var _ beads.StorageCreateStore = (*strictStorageStore)(nil)

// CreateWithStorage applies the same explicit-id guard and id post-check as
// Create, then forwards the policy-selected storage tier to the leaf.
func (s *strictStorageStore) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	if err := s.guardExplicitID(b.ID); err != nil {
		return beads.Bead{}, err
	}
	created, err := s.storage.CreateWithStorage(b, storage)
	if err != nil {
		return beads.Bead{}, err
	}
	if err := s.checkCreatedID(b.ID, created); err != nil {
		return beads.Bead{}, err
	}
	return created, nil
}

// strictTx applies the strict create checks inside a beads.Store.Tx callback.
// Update, SetMetadataBatch, and Close mutate existing rows only and delegate
// verbatim.
type strictTx struct {
	tx    beads.Tx
	store *StrictStore
}

// Create guards and post-checks exactly like StrictStore.Create, against the
// transaction's write surface.
func (t *strictTx) Create(b beads.Bead) (beads.Bead, error) {
	if err := t.store.guardExplicitID(b.ID); err != nil {
		return beads.Bead{}, err
	}
	created, err := t.tx.Create(b)
	if err != nil {
		return beads.Bead{}, err
	}
	if err := t.store.checkCreatedID(b.ID, created); err != nil {
		return beads.Bead{}, err
	}
	return created, nil
}

// Update delegates to the leaf transaction.
func (t *strictTx) Update(id string, opts beads.UpdateOpts) error {
	return t.tx.Update(id, opts)
}

// SetMetadataBatch delegates to the leaf transaction.
func (t *strictTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return t.tx.SetMetadataBatch(id, kvs)
}

// Close delegates to the leaf transaction.
func (t *strictTx) Close(id string) error {
	return t.tx.Close(id)
}

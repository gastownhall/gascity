// Package splittest provides strict, prefix-disjoint bead-store test doubles
// for the work/coordination-class store split.
//
// # Why this package exists
//
// The split-store bug class — code resolving "which store owns this class of
// bead" differently on different paths — is the one this repo keeps paying
// for. beads.MemStore hides it:
//
//   - MemStore.DepAdd appends an edge without resolving either endpoint, so a
//     cross-store dependency succeeds in a test no matter what the backend the
//     test claims to model would have done with it.
//   - MemStore.Create mints over whatever id it was handed, so a graph-prefixed
//     bead can be "created" inside a work store without a peep — and the test
//     never learns that the row it thinks it made does not exist.
//
// StrictStore closes both gaps at the LEAF store, so a policy or class wrapper
// layered on top keeps the checks live on every path (cmd/gc's beadPolicyStore
// does not override DepAdd or Create's id handling — the embedded Store
// interface delegates them straight down).
//
// # Two backends, two answers: pick one per leaf
//
// The two production backends do NOT agree on these operations, so a single
// strictness setting would model one of them and lie about the other. A work
// store runs on bd/Dolt, which hard-fails; a relocated coordination class runs
// on SQLite (internal/storebinding/sqlite/beads_engine.go OpenEngine opens
// beads.OpenSQLiteStore with the class's reserved prefix), which accepts
// silently and corrupts instead. Semantics selects which one a leaf models —
// [BdSemantics] for [NewWorkStore], [SQLiteSemantics] for [NewClassStore] —
// and every rule below names the backend it is modeling.
//
// One rule names none, and that is the point of it: the pinned-id FENCE is a
// property of the class binding, so both backends enforce it identically and
// both semantics do too. A leaf is fenced when it is given namespaces; see
// pinned_id_fence.go.
//
//	rule                               bd/Dolt          SQLite            kit
//	---------------------------------- ---------------- ----------------- -------------------------
//	Create, explicit id outside the    rejects:         accepts verbatim: BdSemantics: reject with
//	store's own prefix, UNFENCED       prefix mismatch, no prefix check   bd's message.
//	                                   --force to       in                SQLiteSemantics: accept,
//	                                   override         normalizeCreate   record a violation.
//
//	Create, explicit id outside the    rejects, wrapping ErrPinnedID-     Reject, wrapping
//	namespaces the binding claims,     OutsideNamespace — the SAME rule   ErrPinnedIDOutsideNamespace
//	FENCED                             in both, because it is the         under BOTH semantics: the
//	                                   invariant, not a backend's         fence is a property of the
//	                                   behavior                           BINDING, not of a backend.
//
//	DepAdd, endpoint missing from      rejects: no      accepts: deps has BdSemantics: reject with
//	this store, SAME prefix            issue found      no foreign key    bd's message.
//	                                   matching <id>    and depAdd is a   SQLiteSemantics: accept,
//	                                                    plain INSERT      record a violation.
//
//	DepAdd, endpoint in ANOTHER        accepts as a     accepts           Neither backend rejects
//	store's prefix                     cross-prefix                       this. BdSemantics rejects
//	                                   external ref                       it anyway, for the DOMAIN
//	                                                                      co-residence invariant
//	                                                                      (convoy.TrackItemIn's
//	                                                                      ErrMemberNotCoResident),
//	                                                                      not for the backend.
//	                                                                      SQLiteSemantics accepts,
//	                                                                      records a violation.
//
//	Leaf hands back an id other than   n/a — both honor a pinned id       Always reject. This is a
//	the one pinned, or mints outside   and mint under their configured    check on the DOUBLE's
//	the declared prefix                prefix                             fidelity, not on a backend.
//
// Sources for the bd column, at the pinned beads version: cmd/bd/create.go
// calls validation.ValidateIDPrefixAllowed(id, dbPrefix, allowed, force), which
// returns "prefix mismatch: ... (use --force to override)"; cmd/bd/dep.go
// resolves both endpoints and returns "resolving issue ID %s: no issue found
// matching %q", EXCEPT that a target whose prefix differs from the source's is
// passed through unresolved as a cross-prefix ref. Sources for the SQLite
// column: beads.SQLiteStore.normalizeCreate keeps an explicit id verbatim (its
// own CreateWithForeignID doc says so), and sqliteSchemaDepsTable declares no
// foreign key on issue_id/depends_on_id.
//
// # Accepted is not unnoticed
//
// A SQLiteSemantics leaf accepting a violation is the point: the row or edge
// lands, so the test sees what production sees — a dangling edge drops its
// dependent out of Ready rather than erroring. But the violation is recorded,
// and the kit's constructors fail the test at cleanup for any violation the
// fixture did not claim with [TakeResidenceViolations]. A fixture asserting the
// production corruption claims them; every other fixture goes red.
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
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// StrictStore wraps a LEAF beads.Store and answers a residence-invariant
// violation the way the backend it models answers it, instead of the way
// beads.MemStore does (silently, always):
//
//   - DepAdd resolves BOTH endpoints in this store first, preserving the
//     parent-child short-circuit exactly as beads.BdStore.DepAdd does.
//   - Create (including Tx creates and CreateWithStorage on storage-capable
//     leaves) checks an explicit id against the store's declared prefix, and
//     always fails loudly when the leaf hands back an id other than the one
//     that was asked for.
//
// A FENCED leaf refuses a pinned id outside the namespaces it claims under both
// semantics, because the fence is the binding's rule rather than a backend's.
// Otherwise a BdSemantics store rejects and a SQLiteSemantics store accepts and
// records (see the package doc's rule table, and ResidenceViolation).
//
// Reads are untouched. Optional leaf capabilities that production code
// discovers by type-assertion are forwarded (see the method set and the
// "deliberately dropped" notes on Strict).
type StrictStore struct {
	beads.Store
	// prefix is the normalized id-prefix segment this store mints under
	// ("gcg" for a graph-class store). It is never empty: the constructors
	// reject a leaf with no declared namespace, because the residence checks
	// have nothing to be about without one.
	prefix string
	// semantics is the production backend this leaf models.
	semantics Semantics
	// namespaces are the id namespaces a FENCED leaf claims — a class
	// binding's reserved prefixes, normalized. Empty means unfenced, which is
	// the shipped default for every store that is not a class binding, and the
	// control that tells a fence from a blanket refusal. See fencedPinnedID.
	namespaces []string
	// residence collects what a SQLiteSemantics leaf accepted. Nil under
	// BdSemantics, which rejects at the call site and has nothing to collect.
	residence *residenceLog
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
	_ assignmentClaimer                      = (*StrictStore)(nil)
	_ beads.GraphApplyHandleProvider         = (*StrictStore)(nil)
	_ beads.AtomicTxStore                    = (*StrictStore)(nil)
	_ beads.ParentProjectionWaiter           = (*StrictStore)(nil)
	_ beads.DepMetadataReader                = (*StrictStore)(nil)
	_ storeref.HasIDPrefix                   = (*StrictStore)(nil)
)

// Strict wraps a leaf store in the strict split-store checks, taking the id
// prefix from the leaf's own storeref.HasIDPrefix accessor and the backend to
// model from semantics.
//
// A leaf that exposes no prefix — beads.MemStore cannot, because its IDPrefix
// is a field and Go forbids a method of the same name; beads.BdStore and
// beads.SQLiteStore report "" when opened without one — fails the test here
// rather than producing a store called strict whose residence checks have no
// namespace to be about. Use StrictWithPrefix to declare the namespace for
// those leaves.
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
// DepMetadata, CloseStore, AtomicTx, Backing, and WaitForParentProjection.
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
//
// namespaces, when non-empty, FENCES the leaf to them: a pinned id outside the
// set is refused with beads.ErrPinnedIDOutsideNamespace, exactly as a store
// serving a class binding refuses it. Pass the same prefixes production passes
// (storebinding.EngineReservedPrefixes for the classes the binding serves);
// passing none models an unfenced store, which is every store that is not a
// class binding. See pinned_id_fence.go.
func Strict(t *testing.T, s beads.Store, semantics Semantics, namespaces ...string) beads.Store {
	t.Helper()
	prefix, err := inferredPrefix(s)
	if err != nil {
		t.Fatalf("splittest.Strict: %v", err)
	}
	return StrictWithPrefix(t, s, prefix, semantics, namespaces...)
}

// inferredPrefix reads a leaf's declared id prefix, rejecting a leaf that has
// none. Split out of Strict so the rejection rule is testable without a failing
// *testing.T.
func inferredPrefix(s beads.Store) (string, error) {
	accessor, ok := s.(storeref.HasIDPrefix)
	if !ok {
		return "", fmt.Errorf("leaf store %T does not expose storeref.HasIDPrefix, so it declares no id namespace; pass the prefix to StrictWithPrefix instead", s)
	}
	prefix := normalizePrefix(accessor.IDPrefix())
	if prefix == "" {
		return "", fmt.Errorf("leaf store %T reports an empty id prefix, so it declares no id namespace; pass the prefix to StrictWithPrefix instead", s)
	}
	return prefix, nil
}

// StrictWithPrefix wraps a leaf store like Strict, declaring the id-prefix
// segment the store mints under (e.g. "gcg" for a graph-class store) rather than
// reading it off the leaf. The declared prefix is what the residence checks are
// about and what IDPrefix reports for storeref prefix routing, so an empty one
// fails the test: a store whose namespace is undeclared cannot tell a foreign id
// from its own.
//
// namespaces fences the leaf exactly as in Strict.
func StrictWithPrefix(t *testing.T, s beads.Store, prefix string, semantics Semantics, namespaces ...string) beads.Store {
	t.Helper()
	strict, err := newStrict(s, prefix, semantics, namespaces)
	if err != nil {
		t.Fatalf("splittest.StrictWithPrefix: %v", err)
	}
	failOnUnclaimedResidenceViolations(t, strict)
	return strict
}

// newStrict builds the wrapper, choosing the StorageCreateStore-preserving
// variant when (and only when) the leaf implements CreateWithStorage. Split out
// of the constructors so the rejection rules are testable without a failing
// *testing.T.
func newStrict(s beads.Store, prefix string, semantics Semantics, namespaces []string) (beads.Store, error) {
	if s == nil {
		return nil, errors.New("leaf store is nil")
	}
	if !semantics.valid() {
		return nil, fmt.Errorf("no production backend declared (%s); pass BdSemantics for a work store or SQLiteSemantics for a coordination-class store", semantics)
	}
	normalized := normalizePrefix(prefix)
	if normalized == "" {
		return nil, fmt.Errorf("empty id prefix %q; the residence checks need a declared namespace to be about", prefix)
	}
	strict := &StrictStore{Store: s, prefix: normalized, semantics: semantics, namespaces: normalizeNamespaces(namespaces)}
	if semantics == SQLiteSemantics {
		strict.residence = &residenceLog{}
	}
	if storage, ok := s.(beads.StorageCreateStore); ok {
		return &strictStorageStore{StrictStore: strict, storage: storage}, nil
	}
	return strict, nil
}

// takeResidenceViolations implements residenceRecorder. A BdSemantics store has
// no log and reports none — it rejected at the call site instead.
func (s *StrictStore) takeResidenceViolations() []ResidenceViolation {
	if s.residence == nil {
		return nil
	}
	return s.residence.take()
}

// acceptedResidenceViolation records a write this store let through because the
// SQLite backend it models lets it through. It must only be called on a
// SQLiteSemantics store.
func (s *StrictStore) acceptedResidenceViolation(op, detail string) {
	s.residence.record(op, detail)
}

// normalizePrefix mirrors the beads package's internal id-prefix normalization
// (CachingStore): lowercase, trimmed, no trailing dashes, so "GCG-" and "gcg"
// declare the same namespace.
func normalizePrefix(prefix string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(prefix)), "-")
}

// Create checks an explicit id against the store's declared namespace before
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
// namespace. It DELIBERATELY bypasses the fence and the foreign-prefix guard
// alike — the conformance suite pins that it stays open on a fenced store
// (TheForeignIDCreateStaysOpenForTheMigrationCopy): this capability
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

// DepAdd resolves both endpoints in THIS store before delegating, which
// beads.MemStore.DepAdd never does — it appends the edge whatever the ids are,
// so a cross-store dependency is invisible in a test regardless of which backend
// the test claims to model.
//
// What an unresolvable endpoint means depends on the store's semantics and on
// which endpoint it is; see the package doc's rule table. A BdSemantics store
// rejects, in bd's own wording for the two cases bd rejects and in the domain
// layer's wording for the cross-prefix case only the domain layer rejects. A
// SQLiteSemantics store accepts and records, because SQLite's deps table has no
// foreign key and its DepAdd is a plain INSERT.
//
// The rejection intentionally does NOT wrap beads.ErrNotFound: bd's real failure
// is a subprocess stderr string that callers can only classify textually, so a
// typed error here would let in-process tests pass on errors.Is checks that
// production could never satisfy.
//
// The parent-child short-circuit is preserved exactly as beads.BdStore.DepAdd
// has it: a parent-child dep that merely restates the bead's own ParentID
// returns nil BEFORE endpoint resolution — on a split store the parent may
// legitimately live elsewhere, and bd never sees the call.
func (s *StrictStore) DepAdd(issueID, dependsOnID, depType string) error {
	if depType == "parent-child" {
		bead, err := s.Get(issueID)
		if err == nil && bead.ParentID == dependsOnID {
			return nil
		}
	}
	for _, endpoint := range []struct {
		id       string
		isSource bool
	}{{issueID, true}, {dependsOnID, false}} {
		_, err := s.Get(endpoint.id)
		if err == nil {
			continue
		}
		if !errors.Is(err, beads.ErrNotFound) {
			return fmt.Errorf("adding dep %s→%s: resolving issue ID %s: %w", issueID, dependsOnID, endpoint.id, err)
		}
		if err := s.endpointNotResident(issueID, dependsOnID, endpoint.id, endpoint.isSource); err != nil {
			return err
		}
	}
	return s.Store.DepAdd(issueID, dependsOnID, depType)
}

// endpointNotResident answers a DepAdd endpoint this store cannot resolve, the
// way the backend this leaf models answers it.
//
// bd resolves the source with resolveIDForMutation and the target with
// resolveIDWithRouting (cmd/bd/dep.go), reporting `resolving issue ID <id>: no
// issue found matching "<id>"` — except for a target whose prefix differs from
// the source's, which bd passes through unresolved as a cross-prefix external
// ref. So the cross-prefix target is the one case NO backend rejects; a
// BdSemantics store rejects it for the domain co-residence invariant instead
// (convoy.TrackItemIn returns ErrMemberNotCoResident "because a dep row cannot
// reference an id its own store cannot resolve"), and says so, rather than
// dressing a domain rule in bd's clothes.
func (s *StrictStore) endpointNotResident(issueID, dependsOnID, missing string, isSource bool) error {
	crossStore := !isSource && !s.ownsID(missing)
	if s.semantics == SQLiteSemantics {
		s.acceptedResidenceViolation("dep-add", fmt.Sprintf(
			"edge %s→%s was recorded although %s is not in this %q store: SQLite's deps table has no foreign key and DepAdd is a plain INSERT, so production keeps the dangling edge and silently drops %s out of Ready instead of erroring",
			issueID, dependsOnID, missing, s.prefix, issueID))
		return nil
	}
	if crossStore {
		return fmt.Errorf("adding dep %s→%s: %s belongs to another store's id namespace, not %q: a dep row cannot reference an id its own store cannot resolve (convoy.TrackItemIn rejects the same shape with ErrMemberNotCoResident). Neither backend rejects this write; both record a dangling edge instead", issueID, dependsOnID, missing, s.prefix)
	}
	return fmt.Errorf("adding dep %s→%s: resolving issue ID %s: no issue found matching %q", issueID, dependsOnID, missing, missing)
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
// segment so storeref.PrefixOwner can route an id to this store. It is never
// empty: the constructors reject a leaf with no declared namespace, because a
// store that reported "" would be silently unroutable while type-asserting as
// routable.
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
//
// FIDELITY GAP, read this before using a kit leaf to model a count. Declaring
// this method makes every kit store type-assert as beads.Counter, and the
// production relocated-class binding does NOT: OpenEngine hands callers a raw
// *beads.SQLiteStore, which has no Count at all (only CachingStore,
// NativeDoltStore and DoltliteReadStore implement it). The two shapes are
// indistinguishable to a caller that reaches Count — both end at
// ErrCountUnsupported — but not to one that branches on the type assertion
// alone. A fixture whose subject is that assertion (internal/api's bead-list
// page bounding is the live example) must use a leaf that implements no Count,
// or it silently models a capability the class binding does not have.
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

// assignmentClaimer is the acquire half of the claim pair, discovered on the
// leaf the same way beads.ConditionalAssignmentReleaser is. It is declared here
// rather than in internal/beads because the canonical Store surface deliberately
// has no claim method — internal/storebinding declares the identical shape for
// the identical reason (beadsAssignmentClaimer), and only the relocated
// coordination-class front door reaches it.
type assignmentClaimer interface {
	Claim(id, assignee string) (beads.Bead, bool, error)
}

// Claim forwards the leaf's two-argument assignment claim — the acquire dual of
// ReleaseIfCurrent, and the CAS the class front door
// (storebinding.NewBeadsGraphStore) routes a claim through. A leaf without one
// errors rather than emulating the compare-and-swap with a read-then-write,
// which would lose the single-winner guarantee the contract promises.
//
// FIDELITY GAP, the mirror of Count's. Declaring this method makes every kit
// store satisfy the claimer assertion, and only the CLASS side of a split
// matches production there: the relocated binding is a *beads.SQLiteStore, which
// claims, while the work side is bd, whose BdStore.Claim takes the assignee
// implicitly (one argument) and does not satisfy this shape at all. So a leaf
// backed by beads.MemStore satisfies the assertion and then fails the call. A
// fixture whose subject is a routed CLAIM must therefore use a real SQLite leaf
// for the class store, not a MemStore one, or it pins the kit's limitation
// instead of the program's behavior.
func (s *StrictStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	claimer, ok := s.Store.(assignmentClaimer)
	if !ok {
		return beads.Bead{}, false, fmt.Errorf("strict store: leaf store %T does not support assignment claim", s.Store)
	}
	return claimer.Claim(id, assignee)
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

// DepMetadata forwards the leaf's edge-payload read. Wrapping changes nothing
// about what an edge carries, so the leaf's answer passes through untouched.
//
// Forwarded rather than dropped because a caller that refuses on uncertainty —
// the infra-class migration is one — reads a stripped capability as UNABLE TO
// ANSWER and refuses a leaf that answers fine. A leaf without the read gets an
// error rather than ("", false, nil): "cannot be asked" and "carries nothing"
// are different answers, and every kit leaf that can hold a payload answers.
func (s *StrictStore) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	reader, ok := s.Store.(beads.DepMetadataReader)
	if !ok {
		return "", false, fmt.Errorf("reading dependency metadata %s -> %s: strict-wrapped leaf %T exposes no edge-payload read", issueID, dependsOnID, s.Store)
	}
	return reader.DepMetadata(issueID, dependsOnID)
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

// guardExplicitID answers a caller-supplied id the store would not have minted
// itself. An empty id is a MINT request, not a pin, so it always passes.
//
// A FENCED leaf answers with the fence and nothing else, because that is all a
// real class binding does: the namespace set is the whole rule, and the store's
// own mint prefix carries no separate authority — a binding legitimately holds
// namespaces it never mints (the nudges store holds the nudge queue's records
// that way). See fencedPinnedID.
//
// An UNFENCED leaf falls through to what the backend it models does with an id
// outside its own mint prefix. bd rejects the mismatch in
// validation.ValidateIDPrefixAllowed, called from cmd/bd/create.go with the
// command's --force flag; the message below is bd's, plus the pointer at the
// forced path. An unfenced SQLite store accepts it verbatim —
// SQLiteStore.normalizeCreate has no prefix check at all, as its own
// CreateWithForeignID doc says — so a SQLiteSemantics store lands the row and
// records the violation.
func (s *StrictStore) guardExplicitID(id string) error {
	if len(s.namespaces) > 0 {
		return fencedPinnedID(id, s.namespaces)
	}
	id = strings.TrimSpace(id)
	if id == "" || s.ownsID(id) {
		return nil
	}
	if s.semantics == SQLiteSemantics {
		s.acceptedResidenceViolation("create", fmt.Sprintf(
			"bead %q was created inside the %q store: SQLiteStore.normalizeCreate keeps a pinned id verbatim with no prefix check, so production lands this foreign-prefix row in the class database and no prefix route will ever look for it there",
			id, s.prefix))
		return nil
	}
	return fmt.Errorf("creating bead %q: prefix mismatch: database uses %q but ID %q doesn't match (use --force to override) — bd's own rejection of a mismatched --id; use CreateWithForeignID for the forced foreign-prefix create", id, s.prefix+"-", id)
}

// checkCreatedID fails loudly when the leaf did not produce the row the caller
// asked for: a pinned id silently replaced by the leaf's own sequence (a leaf
// that does not honor explicit ids, so every wisp-shaped fixture id is a lie),
// or a store-MINTED id outside the declared namespace (the leaf mints under a
// different prefix than the wrapper was declared with). Both are checks on the
// double's fidelity — no backend does either — so they hold under both
// semantics.
//
// The namespace half deliberately covers minted ids only, and it is about the
// MINT prefix rather than the fence's namespace set: a store mints under one
// namespace even when it holds several. A pinned id has already been answered
// by guardExplicitID — accepted inside a fenced leaf's namespaces, or accepted
// and recorded by an unfenced SQLiteSemantics leaf exactly as SQLite accepts it
// — and re-rejecting it here would undo that on the way back out.
func (s *StrictStore) checkCreatedID(requestedID string, created beads.Bead) error {
	if requested := strings.TrimSpace(requestedID); requested != "" {
		if created.ID != requested {
			return fmt.Errorf("store returned bead %q for an explicit create of %q: the leaf store %T clobbers pinned ids, so it cannot model a store that round-trips them (production wisps carry pinned <prefix>-wisp-<suffix> ids)", created.ID, requested, s.Store)
		}
		return nil
	}
	if s.ownsID(created.ID) {
		return nil
	}
	return fmt.Errorf("store minted bead %q outside its declared id namespace %q: the leaf is minting under a different prefix than the one this store was declared with", created.ID, s.prefix)
}

// ownsID reports whether id sits in the declared prefix namespace, using the
// same case-insensitive segment match as storeref.PrefixOwner and CachingStore.
// The prefix is never empty — the constructors reject a leaf without one.
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

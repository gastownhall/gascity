package beads

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func guardedTestPtr[T any](v T) *T { return &v }

// A *BatchItemError must stay transparent to the checks callers already make
// on single-row conditional writes.
func TestBatchItemErrorUnwrapsToTheItemRefusal(t *testing.T) {
	pre := guardedPreconditionError(3, "gc-1", 4, 5, "")
	if !IsPreconditionFailed(pre) {
		t.Fatalf("IsPreconditionFailed(%v) = false", pre)
	}
	var pfe *PreconditionFailedError
	if !errors.As(pre, &pfe) || pfe.ID != "gc-1" || pfe.Expected != 4 || pfe.Current != 5 {
		t.Fatalf("errors.As(*PreconditionFailedError) = %+v", pfe)
	}
	if err := guardedNotFoundError(0, "gc-2", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not-found item error %v does not match ErrNotFound", err)
	}
	if err := guardedExistsError(1, "gc-3", issueops.ErrAlreadyExists); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("exists item error %v does not match ErrAlreadyExists", err)
	}
}

// Validation reports update options the guarded write cannot carry with the
// same typed errors UpdateIfMatch raises, as well as the batch sentinel.
func TestValidateGuardedBatchKeepsTheConditionalUpdateErrors(t *testing.T) {
	parent := "gc-9"
	err := validateGuardedBatch(GuardedBatch{Items: []GuardedItem{{Update: &GuardedUpdate{ID: "gc-1", Opts: UpdateOpts{ParentID: &parent}}}}})
	var unsupported *ConditionalUpdateFieldUnsupportedError
	if !errors.Is(err, ErrInvalidGuardedBatch) || !errors.As(err, &unsupported) || unsupported.Field != "parent_id" {
		t.Fatalf("parent update = %v, want ErrInvalidGuardedBatch wrapping the parent_id refusal", err)
	}
	err = validateGuardedBatch(GuardedBatch{Items: []GuardedItem{{Update: &GuardedUpdate{ID: "gc-1"}}}})
	if !errors.Is(err, ErrInvalidGuardedBatch) || !errors.Is(err, ErrEmptyConditionalUpdate) {
		t.Fatalf("empty update = %v, want ErrInvalidGuardedBatch wrapping ErrEmptyConditionalUpdate", err)
	}
	err = validateGuardedBatch(GuardedBatch{Items: []GuardedItem{{Update: &GuardedUpdate{ID: "gc-1", Opts: UpdateOpts{Labels: []string{"l"}}}}}})
	if err != nil {
		t.Fatalf("label update = %v; every guarded batcher applies labels inside the batch", err)
	}
	if err := validateGuardedBatch(GuardedBatch{Items: []GuardedItem{{Create: &Bead{ID: " gc-1"}}}}); !errors.Is(err, ErrInvalidGuardedBatch) {
		t.Fatalf("create with a padded id = %v, want ErrInvalidGuardedBatch", err)
	}
}

// A guarded batch must fit in one native batch-apply request, or a batch
// validation accepts would be refused by the backend.
func TestMaxGuardedItemsFitsOneNativeApplyRequest(t *testing.T) {
	if MaxGuardedItems > issueops.MaxApplyBatchItems {
		t.Fatalf("MaxGuardedItems = %d exceeds issueops.MaxApplyBatchItems = %d", MaxGuardedItems, issueops.MaxApplyBatchItems)
	}
}

// A create cannot be both ephemeral and no-history; validation refuses it the
// same way on every backend instead of leaving it to whichever store rejects it.
func TestValidateGuardedBatchRefusesBothStorageTiers(t *testing.T) {
	err := validateGuardedBatch(GuardedBatch{Items: []GuardedItem{{Create: &Bead{ID: "gc-1", Ephemeral: true, NoHistory: true}}}})
	if !errors.Is(err, ErrInvalidGuardedBatch) {
		t.Fatalf("ephemeral no-history create = %v, want ErrInvalidGuardedBatch", err)
	}
}

// A pinned create keeps the status it names, so one batch can write a row
// that is already closed.
func TestMemStoreGuardedCreateKeepsItsStatus(t *testing.T) {
	m := NewMemStore()
	_, err := m.ApplyGuarded(context.Background(), GuardedBatch{Items: []GuardedItem{
		{Create: &Bead{ID: "gc-fact", Title: "fact", Status: "closed", Metadata: map[string]string{"close_reason": "recorded"}}},
	}})
	if err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	got, err := m.Get("gc-fact")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" || got.CloseReason != "recorded" {
		t.Fatalf("created row status=%q close_reason=%q, want closed/recorded", got.Status, got.CloseReason)
	}
	// A pinned id in the store's own format consumes its suffix, as Create's
	// does, so the next mint cannot re-issue it.
	if _, err := m.ApplyGuarded(context.Background(), GuardedBatch{Items: []GuardedItem{{Create: &Bead{ID: "gc-7", Title: "pinned"}}}}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	minted, err := m.Create(Bead{Title: "minted"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if minted.ID != "gc-8" {
		t.Fatalf("next minted id = %s, want gc-8 after pinning gc-7", minted.ID)
	}
}

func TestMemStoreApplyGuardedHonorsACanceledContext(t *testing.T) {
	m := NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := m.ApplyGuarded(ctx, GuardedBatch{Items: []GuardedItem{{Create: &Bead{ID: "gc-x", Title: "x"}}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ApplyGuarded on a canceled context = %v, want context.Canceled", err)
	}
	if _, err := m.Get("gc-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after a canceled batch = %v, want ErrNotFound", err)
	}
}

// A legacy SQLite layout cannot guard an update, and a read-only open cannot
// write: discovery answers no for both, and a direct call refuses writing
// nothing.
func TestSQLiteStoreGuardedBatchRefusesWithoutRevisionsOrWrites(t *testing.T) {
	for _, tc := range []struct {
		name               string
		revision, readOnly bool
	}{
		{"legacy layout", false, false},
		{"read-only open", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openSQLiteLayoutForTest(t, tc.revision, tc.readOnly)
			if b, ok := GuardedBatcherFor(s); ok {
				t.Fatalf("GuardedBatcherFor = (%T, true), want no capability", b)
			}
			id := sqliteGraphPrefix + "-gbrefused"
			_, err := s.ApplyGuarded(context.Background(), GuardedBatch{Items: []GuardedItem{{Create: &Bead{ID: id, Title: "refused"}}}})
			if !errors.Is(err, ErrGuardedBatchUnsupported) {
				t.Fatalf("ApplyGuarded = %v, want ErrGuardedBatchUnsupported", err)
			}
			if _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get after a refused batch = %v, want ErrNotFound", err)
			}
		})
	}
	if b, ok := GuardedBatcherFor(openSQLiteLayoutForTest(t, true, false)); !ok || b == nil {
		t.Fatal("a writable layout with the revision column must be discovered as a guarded batcher")
	}
}

// The native store composes ONE apply request whose items map one to one onto
// the batch: the pinned create, the guard as ExpectedVersion, a status write
// waiving the close policy, an assignee write waiving the transfer guard, the
// caller's reason as provenance, and a label edit carrying the label revision.
func TestNativeDoltStoreGuardedBatchComposesOneApplyRequest(t *testing.T) {
	spy := newBatchTxSpy(nativeIssueWithMetadata(t, "gc-labeled", map[string]string{beadmeta.LabelRevisionMetadataKey: "4"}))
	spy.applier.result = issueops.ApplyBatchResult{Items: []issueops.ItemResult{
		{Kind: issueops.ItemCreate, IssueID: "gc-new", RowVersion: 11},
		{Kind: issueops.ItemUpdate, IssueID: "gc-closing", RowVersion: 12},
		{Kind: issueops.ItemUpdate, IssueID: "gc-labeled", RowVersion: 13},
	}}
	store := newNativeDoltStoreForTest(spy)
	guard := int64(7)
	res, err := store.ApplyGuarded(context.Background(), GuardedBatch{
		Reason: "gc: test batch",
		Items: []GuardedItem{
			{Create: &Bead{ID: "gc-new", Title: "new", NoHistory: true}},
			{Update: &GuardedUpdate{ID: "gc-closing", ExpectedRevision: &guard, Opts: UpdateOpts{Status: guardedTestPtr("closed"), Assignee: guardedTestPtr("")}}},
			{Update: &GuardedUpdate{ID: "gc-labeled", Opts: UpdateOpts{Labels: []string{"added"}}}},
		},
	})
	if err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	if want := map[string]int64{"gc-new": 11, "gc-closing": 12, "gc-labeled": 13}; len(res.Revisions) != 3 ||
		res.Revisions["gc-new"] != want["gc-new"] || res.Revisions["gc-closing"] != want["gc-closing"] || res.Revisions["gc-labeled"] != want["gc-labeled"] {
		t.Fatalf("Revisions = %v, want %v", res.Revisions, want)
	}
	req := soleBatchRequest(t, spy.applier)
	if req.Provenance != "gc: test batch" || !req.ForceIDPrefix || len(req.Items) != 3 {
		t.Fatalf("request provenance=%q forceIDPrefix=%v items=%d", req.Provenance, req.ForceIDPrefix, len(req.Items))
	}
	create := req.Items[0]
	if create.Kind != issueops.ItemCreate || create.Create.Issue.ID != "gc-new" || !create.Create.Issue.NoHistory {
		t.Fatalf("item 0 = %+v, want the pinned no-history create", create)
	}
	closing := req.Items[1].Update
	if closing == nil || closing.ExpectedVersion == nil || *closing.ExpectedVersion != 7 ||
		!closing.ForceClosePolicy || !closing.ForceAssigneeTransfer || closing.Patch.Status.Value != beadslib.StatusClosed {
		t.Fatalf("item 1 = %+v, want a guarded close waiving the close policy and the transfer guard", closing)
	}
	labeled := req.Items[2].Update
	if labeled == nil || labeled.ExpectedVersion != nil || string(labeled.Patch.Metadata.Set[beadmeta.LabelRevisionMetadataKey]) != `"5"` {
		t.Fatalf("item 2 = %+v, want an unguarded label edit advancing the label revision to 5", labeled)
	}
}

// The role's typed item refusals become the batch's typed refusals, naming
// the batch item.
func TestNativeDoltStoreGuardedBatchMapsItemRefusals(t *testing.T) {
	guard := int64(3)
	batch := GuardedBatch{Items: []GuardedItem{
		{Create: &Bead{ID: "gc-new", Title: "new"}},
		{Update: &GuardedUpdate{ID: "gc-row", ExpectedRevision: &guard, Opts: UpdateOpts{Title: guardedTestPtr("t")}}},
	}}
	for _, tc := range []struct {
		name  string
		err   error
		index int
		check func(error) bool
	}{
		{"already exists", &issueops.ItemError{Index: 0, Kind: issueops.ItemCreate, Err: issueops.ErrAlreadyExists}, 0, func(err error) bool { return errors.Is(err, ErrAlreadyExists) }},
		{"version mismatch", &issueops.ItemError{Index: 1, Kind: issueops.ItemUpdate, Err: issueops.ErrVersionMismatch}, 1, IsPreconditionFailed},
		{"not found", &issueops.ItemError{Index: 1, Kind: issueops.ItemUpdate, Err: issueops.ErrNotFound}, 1, func(err error) bool { return errors.Is(err, ErrNotFound) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := newBatchTxSpy(&beadslib.Issue{ID: "gc-row", RowVersion: 9})
			spy.applier.err = tc.err
			_, err := newNativeDoltStoreForTest(spy).ApplyGuarded(context.Background(), batch)
			var itemErr *BatchItemError
			if !tc.check(err) || !errors.As(err, &itemErr) || itemErr.Index != tc.index || itemErr.ID != batch.Items[tc.index].id() {
				t.Fatalf("ApplyGuarded = %v, want a typed refusal naming item %d", err, tc.index)
			}
			var pfe *PreconditionFailedError
			if errors.As(err, &pfe) && (pfe.Expected != 3 || pfe.Current != 9) {
				t.Fatalf("precondition expected=%d current=%d, want 3/9", pfe.Expected, pfe.Current)
			}
		})
	}
}

// The revisions are the role's item results, one per item in request order,
// each naming its item's row. A result set that does not line up fails the call
// rather than attributing a revision to the wrong row, and the failure is none
// of the typed refusals: the batch applied, so the caller re-reads.
func TestNativeDoltStoreGuardedBatchRefusesResultsThatDoNotLineUp(t *testing.T) {
	batch := GuardedBatch{Items: []GuardedItem{
		{Create: &Bead{ID: "gc-new", Title: "new"}},
		{Update: &GuardedUpdate{ID: "gc-row", Opts: UpdateOpts{Title: guardedTestPtr("t")}}},
	}}
	for _, tc := range []struct {
		name  string
		items []issueops.ItemResult
	}{
		{"out of order", []issueops.ItemResult{
			{Kind: issueops.ItemUpdate, IssueID: "gc-row", RowVersion: 12},
			{Kind: issueops.ItemCreate, IssueID: "gc-new", RowVersion: 11},
		}},
		{"short", []issueops.ItemResult{
			{Kind: issueops.ItemCreate, IssueID: "gc-new", RowVersion: 11},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := newBatchTxSpy(&beadslib.Issue{ID: "gc-row", RowVersion: 9})
			spy.applier.result = issueops.ApplyBatchResult{Items: tc.items}
			res, err := newNativeDoltStoreForTest(spy).ApplyGuarded(context.Background(), batch)
			var itemErr *BatchItemError
			if err == nil || errors.As(err, &itemErr) || errors.Is(err, ErrInvalidGuardedBatch) || errors.Is(err, ErrGuardedBatchUnsupported) {
				t.Fatalf("ApplyGuarded = %v, want a failure that is none of the typed refusals", err)
			}
			if len(res.Revisions) != 0 {
				t.Fatalf("Revisions = %v, want none", res.Revisions)
			}
			soleBatchRequest(t, spy.applier)
		})
	}
}

// A backend with no batch applier answers unsupported, and a read-only handle
// refuses before it reaches one.
func TestNativeDoltStoreGuardedBatchRefusals(t *testing.T) {
	batch := GuardedBatch{Items: []GuardedItem{{Create: &Bead{ID: "gc-new", Title: "new"}}}}
	batchless := &batchlessStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	if _, err := newNativeDoltStoreForTest(batchless).ApplyGuarded(context.Background(), batch); !errors.Is(err, ErrGuardedBatchUnsupported) {
		t.Fatalf("ApplyGuarded without a batch applier = %v, want ErrGuardedBatchUnsupported", err)
	}
	spy := newBatchTxSpy()
	readOnly := newNativeDoltStoreForTest(spy, WithProxiedReadOnly())
	if _, err := readOnly.ApplyGuarded(context.Background(), batch); !errors.Is(err, ErrProxiedNativeReadOnly) {
		t.Fatalf("ApplyGuarded on a read-only handle = %v, want ErrProxiedNativeReadOnly", err)
	}
	if len(spy.applier.requests) != 0 {
		t.Fatalf("a read-only handle dialed %d batch requests", len(spy.applier.requests))
	}
	fenced := newNativeDoltStoreForTest(newBatchTxSpy(), WithNativeDoltStoreReservedIDPrefixes("kitn"))
	if _, err := fenced.ApplyGuarded(context.Background(), batch); !errors.Is(err, ErrPinnedIDOutsideNamespace) {
		t.Fatalf("ApplyGuarded pinning outside the fence = %v, want ErrPinnedIDOutsideNamespace", err)
	}
}

// A ProxiedStore called directly brackets the write leaf's batcher, and once
// demoted it answers unsupported without writing.
func TestProxiedStoreApplyGuardedRoutesToTheWriteLeaf(t *testing.T) {
	store := NewProxiedStoreForGuardedBatchConformance("prx").(*ProxiedStore)
	batch := GuardedBatch{Items: []GuardedItem{{Create: &Bead{ID: "prx-gbdirect", Title: "direct"}}}}
	if _, err := store.ApplyGuarded(context.Background(), batch); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	if _, err := store.Get("prx-gbdirect"); err != nil {
		t.Fatalf("Get after the batch: %v", err)
	}
	store.standDown(NewSchemaSkewVerdictError(ProxiedSkewLaneMain, ProxiedSkewDirAhead, "demoted"))
	demoted := GuardedBatch{Items: []GuardedItem{{Create: &Bead{ID: "prx-gbdemoted", Title: "demoted"}}}}
	if _, err := store.ApplyGuarded(context.Background(), demoted); !errors.Is(err, ErrGuardedBatchUnsupported) {
		t.Fatalf("ApplyGuarded on a demoted store = %v, want ErrGuardedBatchUnsupported", err)
	}
	if _, err := store.Get("prx-gbdemoted"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after a refused batch = %v, want ErrNotFound", err)
	}
}

// Every typed class wrapper opts in to guarded-batch discovery, so a guarded
// batcher is discovered through each of them.
func TestGuardedBatcherForFollowsTheClassWrappers(t *testing.T) {
	m := NewMemStore()
	for _, wrapped := range []Store{
		WorkStore{Store: m},
		SessionStore{Store: m},
		GraphStore{Store: m},
		MailStore{Store: m},
		OrdersStore{Store: m},
		NudgesStore{Store: m},
	} {
		b, ok := GuardedBatcherFor(wrapped)
		if !ok {
			t.Fatalf("GuardedBatcherFor(%T) = false", wrapped)
		}
		if b != GuardedBatcher(m) {
			t.Fatalf("GuardedBatcherFor(%T) = %T, want the wrapped MemStore", wrapped, b)
		}
	}
	if _, ok := GuardedBatcherFor(WorkStore{Store: readOnlyStoreForGuardedBatchTest{Store: m}}); ok {
		t.Fatal("GuardedBatcherFor(WorkStore over an incapable store) = true")
	}
	if _, ok := GuardedBatcherFor(nil); ok {
		t.Fatal("GuardedBatcherFor(nil) = true")
	}
}

// A conditional-writes resolve target hands out fenced capabilities only; a
// guarded batch can also carry unguarded updates and pinned creates, so a
// wrapper that declares a resolve target but does not opt in to guarded-batch
// discovery (a blind-write-refusing wrapper, say) hides its inner batcher,
// directly and from a cache layered over it.
func TestGuardedBatcherForDoesNotFollowTheConditionalWritesResolveTarget(t *testing.T) {
	m := NewMemStore()
	wrapper := &resolveTargetWrapper{Store: readOnlyStoreForGuardedBatchTest{Store: m}, target: m}
	if got := followConditionalWritesResolveTarget(wrapper); got != Store(m) {
		t.Fatalf("resolve target = %T, want the MemStore: the fixture must declare it", got)
	}
	if b, ok := GuardedBatcherFor(wrapper); ok {
		t.Fatalf("GuardedBatcherFor(resolve-target-only wrapper) = (%T, true), want no capability", b)
	}
	if b, ok := GuardedBatcherFor(NewCachingStoreForTest(wrapper, nil)); ok {
		t.Fatalf("GuardedBatcherFor(cache over a resolve-target-only wrapper) = (%T, true), want no capability", b)
	}
	if b, ok := GuardedBatcherFor(WorkStore{Store: wrapper}); ok {
		t.Fatalf("GuardedBatcherFor(class wrapper over a resolve-target-only wrapper) = (%T, true), want no capability", b)
	}
}

// readOnlyStoreForGuardedBatchTest hides every optional capability of the
// store it embeds.
type readOnlyStoreForGuardedBatchTest struct{ Store }

// A batch through the cache announces each changed row once, and a create is
// visible through the cache without a backing read.
func TestCachingStoreGuardedBatchNotifiesEachRowOnce(t *testing.T) {
	m := NewMemStore()
	row, err := m.Create(Bead{Title: "row"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var events []string
	c := NewCachingStoreForTest(m, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+" "+beadID)
	})
	if err := c.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	b, ok := GuardedBatcherFor(c)
	if !ok {
		t.Fatal("GuardedBatcherFor(CachingStore over MemStore) = false")
	}
	if _, err := b.ApplyGuarded(context.Background(), GuardedBatch{Items: []GuardedItem{
		{Create: &Bead{ID: "gc-gbnew", Title: "new"}},
		{Update: &GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: UpdateOpts{Status: guardedTestPtr("closed")}}},
		{Create: &Bead{ID: "gc-gbfact", Title: "fact", Status: "closed"}},
	}}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	// The cache types a create as it does a row a Tx creates
	// (refreshTxTouchedBeads): an open one is bead.updated, and one created
	// closed is bead.closed with no bead.created before it. The CLI emitter
	// announces both as bead.created. The two channels knowingly diverge
	// (CachingStore.GuardedBatcherHandle), and this pins the cache's side.
	want := []string{"bead.updated gc-gbnew", "bead.closed " + row.ID, "bead.closed gc-gbfact"}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// A native store reports guarded batches only when a call could apply one: a
// read-only handle and a backend without a batch applier are not discovered,
// directly or through a cache, rather than handing out a batcher that always
// refuses.
func TestNativeDoltStoreGuardedBatchDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store *NativeDoltStore
		want  bool
	}{
		{"capable", newNativeDoltStoreForTest(newBatchTxSpy()), true},
		{"read-only handle", newNativeDoltStoreForTest(newBatchTxSpy(), WithProxiedReadOnly()), false},
		{"no batch applier", newNativeDoltStoreForTest(&batchlessStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := GuardedBatcherFor(tc.store); ok != tc.want {
				t.Fatalf("GuardedBatcherFor = %v, want %v", ok, tc.want)
			}
			if _, ok := GuardedBatcherFor(NewCachingStoreForTest(tc.store, nil)); ok != tc.want {
				t.Fatalf("GuardedBatcherFor(cache over it) = %v, want %v", ok, tc.want)
			}
		})
	}
}

// committedThenFailingBatcher applies a batch to its store and then reports a
// failure that does not say whether it committed, as a dropped connection
// after the commit would.
type committedThenFailingBatcher struct{ store *MemStore }

func (b committedThenFailingBatcher) ApplyGuarded(ctx context.Context, batch GuardedBatch) (GuardedResult, error) {
	if _, err := b.store.ApplyGuarded(ctx, batch); err != nil {
		return GuardedResult{}, err
	}
	return GuardedResult{}, errors.New("connection reset after commit")
}

// ambiguousBatchStore is a store whose guarded batches commit and then fail.
type ambiguousBatchStore struct{ *MemStore }

func (s ambiguousBatchStore) GuardedBatcherHandle() (GuardedBatcher, bool) {
	return committedThenFailingBatcher{store: s.MemStore}, true
}

// primedCacheForGuardedBatch returns a primed cache over backing.
func primedCacheForGuardedBatch(t *testing.T, backing Store) *CachingStore {
	t.Helper()
	c := NewCachingStoreForTest(backing, nil)
	if err := c.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return c
}

// A guard the backing refuses means the cache's row is stale: the refused
// batch evicts it, so the caller's re-read through the cache sees the
// backing's revision and the next guard can land.
func TestCachingStoreGuardedBatchMissEvictsTheStaleRow(t *testing.T) {
	m := NewMemStore()
	row, err := m.Create(Bead{Title: "row"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c := primedCacheForGuardedBatch(t, m)
	if err := m.SetMetadata(row.ID, "moved", "behind the cache"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	moved, err := m.Get(row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cached, _ := c.Get(row.ID); cached.Revision != row.Revision {
		t.Fatalf("fixture: cache reads revision %d, want the stale %d", cached.Revision, row.Revision)
	}
	b, ok := GuardedBatcherFor(c)
	if !ok {
		t.Fatal("GuardedBatcherFor(cache) = false")
	}
	_, err = b.ApplyGuarded(context.Background(), GuardedBatch{Items: []GuardedItem{
		{Update: &GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: UpdateOpts{Title: guardedTestPtr("stale")}}},
	}})
	if !IsPreconditionFailed(err) {
		t.Fatalf("ApplyGuarded on the stale revision = %v, want a precondition failure", err)
	}
	got, err := c.Get(row.ID)
	if err != nil {
		t.Fatalf("Get through the cache: %v", err)
	}
	if got.Revision != moved.Revision {
		t.Fatalf("cache reads revision %d after the refused batch, want the backing's %d", got.Revision, moved.Revision)
	}
}

// A failure that may have committed leaves every row the batch named dirty, so
// the cache reads each back from the backing rather than serving the
// pre-batch row.
func TestCachingStoreGuardedBatchAmbiguousFailureDirtiesEveryRow(t *testing.T) {
	m := NewMemStore()
	first, err := m.Create(Bead{Title: "first"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := m.Create(Bead{Title: "second"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c := primedCacheForGuardedBatch(t, ambiguousBatchStore{MemStore: m})
	b, ok := GuardedBatcherFor(c)
	if !ok {
		t.Fatal("GuardedBatcherFor(cache) = false")
	}
	_, err = b.ApplyGuarded(context.Background(), GuardedBatch{Items: []GuardedItem{
		{Update: &GuardedUpdate{ID: first.ID, ExpectedRevision: &first.Revision, Opts: UpdateOpts{Title: guardedTestPtr("first after")}}},
		{Update: &GuardedUpdate{ID: second.ID, ExpectedRevision: &second.Revision, Opts: UpdateOpts{Title: guardedTestPtr("second after")}}},
	}})
	if err == nil {
		t.Fatal("ApplyGuarded = nil, want the backing's failure")
	}
	for _, id := range []string{first.ID, second.ID} {
		want, err := m.Get(id)
		if err != nil {
			t.Fatalf("Get(%s) from the backing: %v", id, err)
		}
		got, err := c.Get(id)
		if err != nil {
			t.Fatalf("Get(%s) through the cache: %v", id, err)
		}
		if got.Revision != want.Revision || got.Title != want.Title {
			t.Errorf("cache reads %s at revision %d (%q), want the backing's %d (%q)", id, got.Revision, got.Title, want.Revision, want.Title)
		}
	}
}

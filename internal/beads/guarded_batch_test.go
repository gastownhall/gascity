package beads

import (
	"context"
	"encoding/json"
	"errors"
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

// The typed class wrappers declare their store as the resolve target, so a
// guarded batcher is discovered through them.
func TestGuardedBatcherForFollowsTheClassWrappers(t *testing.T) {
	m := NewMemStore()
	for _, wrapped := range []Store{WorkStore{Store: m}, SessionStore{Store: m}, GraphStore{Store: m}} {
		b, ok := GuardedBatcherFor(wrapped)
		if !ok {
			t.Fatalf("GuardedBatcherFor(%T) = false", wrapped)
		}
		if b != GuardedBatcher(m) {
			t.Fatalf("GuardedBatcherFor(%T) = %T, want the wrapped MemStore", wrapped, b)
		}
	}
	if _, ok := GuardedBatcherFor(nil); ok {
		t.Fatal("GuardedBatcherFor(nil) = true")
	}
}

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
	}}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	want := []string{"bead.updated gc-gbnew", "bead.closed " + row.ID}
	if len(events) != len(want) || events[0] != want[0] || events[1] != want[1] {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

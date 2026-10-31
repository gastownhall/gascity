package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// What CloseAll puts on the wire.
//
// The per-bead route it replaces spent three round trips per bead in BOTH arms
// — measured at 24 requests for 8 beads against a real bd serve — and the
// session wait store closes a whole batch inside one reconcile, so that count
// is the workflow's batch size rather than a constant. The doubles here record
// the REQUESTS the store composed, because the number of them and their
// contents are the whole property: a route that reached the same end state
// through eight requests would satisfy any assertion about the rows.

// closeBatchRecorder records every ApplyBatch request and answers it the way
// the role's contract says a batch applier must: one ItemResult per item, in
// request order, with Changed false for an idempotent re-close.
type closeBatchRecorder struct {
	requests []issueops.ApplyBatchRequest
	// closed names rows that are already closed, so their close items report
	// the no-op the role describes.
	closed map[string]bool
	// err, when set, refuses every request.
	err error
	// afterFirst, when set, refuses every request but the first — the shape a
	// refusal that is NOT a fact about the backing arrives in.
	afterFirst error
}

var _ issueops.BatchApplier = (*closeBatchRecorder)(nil)

func (r *closeBatchRecorder) ApplyBatch(_ context.Context, req issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return issueops.ApplyBatchResult{}, r.err
	}
	if r.afterFirst != nil && len(r.requests) > 1 {
		return issueops.ApplyBatchResult{}, r.afterFirst
	}
	result := issueops.ApplyBatchResult{Items: make([]issueops.ItemResult, 0, len(req.Items))}
	for _, item := range req.Items {
		switch item.Kind {
		case issueops.ItemUpdate:
			result.Items = append(result.Items, issueops.ItemResult{
				Kind: item.Kind, IssueID: item.Update.Target.ID, Changed: true,
			})
		case issueops.ItemClose:
			id := item.Close.Target.ID
			result.Items = append(result.Items, issueops.ItemResult{
				Kind: item.Kind, IssueID: id, Changed: !r.closed[id],
			})
			r.closed[id] = true
		default:
			return issueops.ApplyBatchResult{}, fmt.Errorf("closeBatchRecorder: unexpected item kind %q", item.Kind)
		}
	}
	return result, nil
}

// closeBatchSpy is a backing whose batch applier is the recorder and whose
// every READ fails the test. A read is the thing this route exists not to make,
// so the double refuses to serve one rather than letting a re-introduced
// per-bead Get pass unnoticed.
type closeBatchSpy struct {
	*nativeDoltStorageSpy
	recorder *closeBatchRecorder
	reads    int
}

func newCloseBatchSpy() *closeBatchSpy {
	spy := &closeBatchSpy{recorder: &closeBatchRecorder{closed: map[string]bool{}}}
	spy.nativeDoltStorageSpy = &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			spy.reads++
			return &beadslib.Issue{ID: id, Title: id, Status: beadslib.StatusOpen}, nil
		},
	}
	return spy
}

func (s *closeBatchSpy) BatchApplier() (issueops.BatchApplier, error) {
	return s.recorder, nil
}

// soleCloseBatchRequest is the one request a CloseAll under the item cap must
// compose, checked rather than indexed so a route that composed none fails
// where it is measured instead of panicking somewhere else.
func soleCloseBatchRequest(t *testing.T, recorder *closeBatchRecorder) issueops.ApplyBatchRequest {
	t.Helper()
	if len(recorder.requests) != 1 {
		t.Fatalf("ApplyBatch called %d times, want exactly 1", len(recorder.requests))
	}
	return recorder.requests[0]
}

// closeBatchIDs mints n ids for a batch.
func closeBatchIDs(n int) []string {
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, fmt.Sprintf("gc-%03d", i))
	}
	return ids
}

// TestCloseAllIsOneRequestPerChunkInBothArms is the round-trip count this slice
// exists to change, asserted at the only place a count is a property rather
// than a coincidence: the requests the store composed.
func TestCloseAllIsOneRequestPerChunkInBothArms(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
	}{
		{"with metadata", map[string]string{"state": "gc_swept", "close_reason": "  the sweep's reason  "}},
		{"without metadata", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := newCloseBatchSpy()
			store := newNativeDoltStoreForTest(spy)
			ids := closeBatchIDs(8)

			closed, err := store.CloseAll(ids, tc.metadata)
			if err != nil {
				t.Fatalf("CloseAll: %v", err)
			}
			if closed != len(ids) {
				t.Fatalf("closed = %d, want %d", closed, len(ids))
			}
			if len(spy.recorder.requests) != 1 {
				t.Fatalf("ApplyBatch called %d times for %d beads, want 1: a whole CloseAll under the item cap is ONE request", len(spy.recorder.requests), len(ids))
			}
			if spy.reads != 0 {
				t.Errorf("the batch route dialed %d per-bead read(s); it composes its request before it dials and has nothing to read back", spy.reads)
			}
		})
	}
}

// TestCloseAllCarriesTheStampAndTheReasonItStamped pins the item shape: the
// metadata patch and the close ride the SAME request in that order, and the
// close carries the reason this call is stamping rather than one read off the
// row it is about to overwrite.
func TestCloseAllCarriesTheStampAndTheReasonItStamped(t *testing.T) {
	spy := newCloseBatchSpy()
	store := newNativeDoltStoreForTest(spy)

	if _, err := store.CloseAll([]string{"gc-1", "gc-2"}, map[string]string{
		"state":        "gc_swept",
		"close_reason": "  session terminated: swept by gc  ",
	}); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}

	req := soleCloseBatchRequest(t, spy.recorder)
	if len(req.Items) != 4 {
		t.Fatalf("the request carries %d items for 2 beads, want 4 (a stamp and a close each)", len(req.Items))
	}
	if req.Actor == "" {
		t.Error("the batch names no actor; it is one act by one caller and the role requires attribution")
	}
	if req.Provenance == "" {
		t.Error("the batch carries no provenance; the per-bead route's writes were labeled and the history entry still needs a name")
	}
	wantOrder := []struct {
		kind issueops.ItemKind
		id   string
	}{
		{issueops.ItemUpdate, "gc-1"},
		{issueops.ItemClose, "gc-1"},
		{issueops.ItemUpdate, "gc-2"},
		{issueops.ItemClose, "gc-2"},
	}
	for i, want := range wantOrder {
		item := req.Items[i]
		if item.Kind != want.kind {
			t.Fatalf("items[%d].Kind = %q, want %q: the stamp must precede the close it feeds", i, item.Kind, want.kind)
		}
		var target string
		switch item.Kind {
		case issueops.ItemUpdate:
			target = item.Update.Target.ID
			if _, stamped := item.Update.Patch.Metadata.Set["close_reason"]; !stamped {
				t.Errorf("items[%d] stamps no close_reason; the stamp is what the caller asked for", i)
			}
		case issueops.ItemClose:
			target = item.Close.Target.ID
			if got := item.Close.Reason; got != "session terminated: swept by gc" {
				t.Errorf("items[%d].Close.Reason = %q, want the trimmed reason THIS call stamped", i, got)
			}
			if !item.Close.Force {
				t.Errorf("items[%d] is not forced; a molecule root routinely closes over open children and the route this replaced applied no close policy", i)
			}
		}
		if target != want.id {
			t.Errorf("items[%d] targets %q, want %q", i, target, want.id)
		}
	}
}

// A CloseAll with no metadata stamps nothing and closes with no reason, which
// is what BdStore.CloseAll — the same interface method on the other production
// store — has always done on its batch path. gc reads a bead's close reason off
// its METADATA, so nothing downstream loses an answer; what it loses is a read
// per bead.
func TestCloseAllWithoutMetadataCarriesOnlyCloses(t *testing.T) {
	spy := newCloseBatchSpy()
	store := newNativeDoltStoreForTest(spy)

	if _, err := store.CloseAll([]string{"gc-1", "gc-2"}, nil); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}

	req := soleCloseBatchRequest(t, spy.recorder)
	if len(req.Items) != 2 {
		t.Fatalf("the request carries %d items for 2 beads with nothing to stamp, want 2", len(req.Items))
	}
	for i, item := range req.Items {
		if item.Kind != issueops.ItemClose {
			t.Fatalf("items[%d].Kind = %q, want a close: an empty metadata map is nothing to patch", i, item.Kind)
		}
		if item.Close.Reason != "" {
			t.Errorf("items[%d].Close.Reason = %q, want empty: this call stamped no reason and cannot invent one", i, item.Close.Reason)
		}
	}
}

// The count is what the batch REPORTS changed, not what the caller hoped. A
// re-close is a no-op the role describes with Changed false, and reading the
// count off the response is a better answer than the loop's pre-read: the loop
// asked, then closed, and counted a row that closed in the gap anyway.
func TestCloseAllCountsWhatTheBatchActuallyClosed(t *testing.T) {
	spy := newCloseBatchSpy()
	spy.recorder.closed["gc-2"] = true
	store := newNativeDoltStoreForTest(spy)

	closed, err := store.CloseAll([]string{"gc-1", "gc-2", "gc-3"}, map[string]string{"state": "done"})
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2: the already-closed row is a no-op and must not be counted", closed)
	}
}

// TestCloseAllOverAStoreCountsOnlyRowsItMoved closes the OVER-REPORT direction
// on the shared applier double, over a real store rather than a recorder.
//
// The under-report direction was closed the moment rawBatchApplier started
// answering per-item outcomes at all: a double returning none reports every
// batch as having changed nothing. The other direction is the one still open —
// a double that answered Changed:true unconditionally would let CloseAll count
// rows it did not move, and no existing case would notice, because every other
// one closes rows that really were open. This one hands it a row that is
// already closed.
func TestCloseAllOverAStoreCountsOnlyRowsItMoved(t *testing.T) {
	storage := newNativeDoltMemStorage()
	store := newNativeDoltStoreForTest(storage)
	open, err := store.Create(Bead{Type: "task", Status: "open", Title: "open"})
	if err != nil {
		t.Fatalf("Create open: %v", err)
	}
	already, err := store.Create(Bead{Type: "task", Status: "open", Title: "already closed"})
	if err != nil {
		t.Fatalf("Create already: %v", err)
	}
	if err := store.Close(already.ID); err != nil {
		t.Fatalf("Close(already): %v", err)
	}

	closed, err := store.CloseAll([]string{open.ID, already.ID}, map[string]string{"close_reason": "the sweep's reason"})
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1: only the open row moved, and the count is what the batch reported changing", closed)
	}
	// And the stamp reached BOTH, which is the semantic the batch route moved
	// and the one every caller that lists with IncludeClosed depends on.
	for _, id := range []string{open.ID, already.ID} {
		got, gErr := store.Get(id)
		if gErr != nil {
			t.Fatalf("Get(%s): %v", id, gErr)
		}
		if got.Metadata["close_reason"] != "the sweep's reason" {
			t.Errorf("bead %s close_reason = %q, want the stamp to reach every id the caller named", id, got.Metadata["close_reason"])
		}
	}
}

// TestCloseAllChunksAtTheApplyItemCap pins the split, and pins that a bead's
// stamp and close are never separated by it. A batch is atomic per request, so
// a chunk boundary between the pair would leave a row stamped and open.
func TestCloseAllChunksAtTheApplyItemCap(t *testing.T) {
	for _, tc := range []struct {
		name      string
		metadata  map[string]string
		beads     int
		wantCalls int
	}{
		{"stamped beads cost two items each", map[string]string{"state": "done"}, issueops.MaxApplyBatchItems/2 + 1, 2},
		{"plain beads cost one", nil, issueops.MaxApplyBatchItems + 1, 2},
		{"a batch at the cap is one request", nil, issueops.MaxApplyBatchItems, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := newCloseBatchSpy()
			store := newNativeDoltStoreForTest(spy)
			ids := closeBatchIDs(tc.beads)

			closed, err := store.CloseAll(ids, tc.metadata)
			if err != nil {
				t.Fatalf("CloseAll: %v", err)
			}
			if closed != tc.beads {
				t.Fatalf("closed = %d, want %d", closed, tc.beads)
			}
			if len(spy.recorder.requests) != tc.wantCalls {
				t.Fatalf("ApplyBatch called %d times for %d beads, want %d", len(spy.recorder.requests), tc.beads, tc.wantCalls)
			}
			seen := 0
			for i, req := range spy.recorder.requests {
				if len(req.Items) > issueops.MaxApplyBatchItems {
					t.Errorf("request %d carries %d items, past the role's cap of %d", i, len(req.Items), issueops.MaxApplyBatchItems)
				}
				byID := map[string]int{}
				for _, item := range req.Items {
					switch item.Kind {
					case issueops.ItemUpdate:
						byID[item.Update.Target.ID]++
					case issueops.ItemClose:
						byID[item.Close.Target.ID]++
					}
				}
				want := 1
				if len(tc.metadata) > 0 {
					want = 2
				}
				for id, count := range byID {
					if count != want {
						t.Errorf("request %d carries %d items for %s, want %d: a bead's stamp and close must land in the same atomic chunk", i, count, id, want)
					}
				}
				seen += len(byID)
			}
			if seen != tc.beads {
				t.Errorf("the chunks covered %d beads, want %d", seen, tc.beads)
			}
		})
	}
}

// A row the batch cannot find refuses the whole request, and the refusal is
// restated as this package's ErrNotFound so a caller's errors.Is arm holds
// across the route change. The applier names the offending id, which is more
// than the per-bead route's own read could say.
func TestCloseAllReportsANotFoundItemAsErrNotFound(t *testing.T) {
	spy := newCloseBatchSpy()
	spy.recorder.err = &issueops.ItemError{
		Index:   1,
		Kind:    issueops.ItemClose,
		IssueID: "gc-2",
		Err:     issueops.ErrNotFound,
	}
	store := newNativeDoltStoreForTest(spy)

	closed, err := store.CloseAll([]string{"gc-1", "gc-2"}, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("CloseAll over a missing bead = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "gc-2") {
		t.Errorf("error = %v, want it to name the bead the batch refused on", err)
	}
	if closed != 0 {
		t.Errorf("closed = %d, want 0: the chunk is atomic, so a refused item closed nothing", closed)
	}
}

// TestCloseAllFallsBackToThePerBeadRouteWhenBatchesAreUnsupported is the same
// ask-do-not-handshake decision Store.Tx makes: a backend with no batch applier
// says so with a typed refusal, and CloseAll must keep working over it rather
// than requiring a capability every class's boot gate would then have to
// demand.
func TestCloseAllFallsBackToThePerBeadRouteWhenBatchesAreUnsupported(t *testing.T) {
	storage := newNativeDoltMemStorage()
	native := newNativeDoltStoreForTest(storage)
	first, err := native.Create(Bead{Type: "task", Status: "open", Title: "first"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := native.Create(Bead{Type: "task", Status: "open", Title: "second"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	refusing := &batchlessStorage{nativeDoltMemStorage: storage}
	store := newNativeDoltStoreForTest(refusing)
	closed, err := store.CloseAll([]string{first.ID, second.ID}, map[string]string{"close_reason": "closed by the fallback route"})
	if err != nil {
		t.Fatalf("CloseAll over a batchless backing: %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2", closed)
	}
	for _, id := range []string{first.ID, second.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != "closed" {
			t.Errorf("bead %s status = %q, want closed", id, got.Status)
		}
		if got.Metadata["close_reason"] != "closed by the fallback route" {
			t.Errorf("bead %s close_reason = %q, want the fallback to have stamped it", id, got.Metadata["close_reason"])
		}
	}
}

// TestCloseAllPerBeadRouteStampsAnAlreadyClosedRowToo is the fallback's twin of
// TestCloseAllOverAStoreCountsOnlyRowsItMoved: the same CloseAll must not
// rewrite an already-closed bead's metadata only when the backing can batch.
// Both routes stamp every id the caller named and count only the rows they
// moved.
func TestCloseAllPerBeadRouteStampsAnAlreadyClosedRowToo(t *testing.T) {
	storage := newNativeDoltMemStorage()
	native := newNativeDoltStoreForTest(storage)
	open, err := native.Create(Bead{Type: "task", Status: "open", Title: "open"})
	if err != nil {
		t.Fatalf("Create open: %v", err)
	}
	already, err := native.Create(Bead{Type: "task", Status: "open", Title: "already closed"})
	if err != nil {
		t.Fatalf("Create already: %v", err)
	}
	if err := native.Close(already.ID); err != nil {
		t.Fatalf("Close(already): %v", err)
	}

	store := newNativeDoltStoreForTest(&batchlessStorage{nativeDoltMemStorage: storage})
	closed, err := store.CloseAll([]string{open.ID, already.ID}, map[string]string{"close_reason": "the sweep's reason"})
	if err != nil {
		t.Fatalf("CloseAll over a batchless backing: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1: only the open row moved", closed)
	}
	for _, id := range []string{open.ID, already.ID} {
		got, gErr := store.Get(id)
		if gErr != nil {
			t.Fatalf("Get(%s): %v", id, gErr)
		}
		if got.Status != "closed" {
			t.Errorf("bead %s status = %q, want closed", id, got.Status)
		}
		if got.Metadata["close_reason"] != "the sweep's reason" {
			t.Errorf("bead %s close_reason = %q, want the stamp to reach every id the caller named, as on the batch route", id, got.Metadata["close_reason"])
		}
	}
}

// batchlessStorage is a backing that refuses the batch applier the way a
// backend without one does.
type batchlessStorage struct{ *nativeDoltMemStorage }

func (s *batchlessStorage) BatchApplier() (issueops.BatchApplier, error) {
	return nil, &beadslib.ErrUnsupported{Op: "BatchApplier", Backend: "batchless-double"}
}

// capabilityRefusal is the shape the HTTP wire refuses in, and it is not the
// portable one. The client consults the server's advertised capability list
// before it dials, so an operation the server does not publish comes back as a
// typed capability error that unwraps to a SENTINEL OF ITS OWN — a plain
// errors.New — and matches no *beadslib.ErrUnsupported arm. Nothing is written
// and no request is made, which is what makes it a legitimate route decision.
type capabilityRefusal struct{ sentinel error }

func (e *capabilityRefusal) Error() string {
	return "bd serve does not advertise capability \"issues.batchApply\", which ApplyBatch requires"
}
func (e *capabilityRefusal) Unwrap() error { return e.sentinel }

// wireRefusingStorage always hands back an applier — as a served backend's
// accessor must, since the refusal is the WIRE's and not the accessor's — and
// that applier refuses every request in the capability shape.
type wireRefusingStorage struct {
	*nativeDoltMemStorage
	sentinel error
}

func (s *wireRefusingStorage) BatchApplier() (issueops.BatchApplier, error) {
	return &closeBatchRecorder{closed: map[string]bool{}, err: &capabilityRefusal{sentinel: s.sentinel}}, nil
}

// TestCloseAllHardFailsOnAnUnregisteredRefusalShape is the DEFAULT half of the
// route decision, and it must stay this way: a refusal nothing has classified
// is not evidence the backing has no batch applier, and falling back on it
// would re-run the whole input against a store that may have written some of
// it. The classification is opt-in per distribution for exactly that reason.
func TestCloseAllHardFailsOnAnUnregisteredRefusalShape(t *testing.T) {
	sentinel := errors.New("an unregistered backend refusal")
	store := newNativeDoltStoreForTest(&wireRefusingStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		sentinel:             sentinel,
	})

	closed, err := store.CloseAll([]string{"gc-1"}, nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("CloseAll = %v, want the unclassified refusal reported rather than swallowed", err)
	}
	if closed != 0 {
		t.Errorf("closed = %d, want 0", closed)
	}
}

// A refusal raised AFTER a chunk has landed is not "this backing has no batch
// applier" — that answer arrives on the first chunk or not at all — so the
// fallback must not fire and silently re-walk work already done. This is the
// `entered` fence Store.Tx keeps for the same reason.
func TestCloseAllDoesNotFallBackOnceAChunkHasLanded(t *testing.T) {
	spy := newCloseBatchSpy()
	store := newNativeDoltStoreForTest(spy)
	ids := closeBatchIDs(issueops.MaxApplyBatchItems + 1)

	// The first chunk applies; the second refuses as unsupported.
	spy.recorder.afterFirst = &beadslib.ErrUnsupported{Op: "ApplyBatch", Backend: "half-way-double"}

	closed, err := store.CloseAll(ids, nil)
	var unsupported *beadslib.ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("CloseAll = %v, want the refusal reported rather than swallowed by a fallback", err)
	}
	if closed != issueops.MaxApplyBatchItems {
		t.Errorf("closed = %d, want the %d the landed chunk closed", closed, issueops.MaxApplyBatchItems)
	}
	if spy.reads != 0 {
		t.Errorf("the per-bead fallback ran anyway (%d reads); it would report only what the remaining chunks closed", spy.reads)
	}
}

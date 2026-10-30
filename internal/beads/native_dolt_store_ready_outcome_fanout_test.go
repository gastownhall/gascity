package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// readyOutcomeFanoutStorage is a beadslib.Storage shaped like an http-native
// backend: its IssueReader.List(IDFilter) refuses, exactly as the http client's
// encoder does unconditionally (E-ListRequest.IDFilter), so
// filterReadyByWorkOutcome must fall back to a per-id IssueReader.Get fan-out
// rather than ever trusting the (refused) List answer. Ready is wired to fail
// the test outright if ever invoked — nothing under test calls it.
//
// ReadEdges enforces the same anchor cap the real bd-serve does
// (readyOutcomeFanoutMaxReadEdgesAnchors), so a test against this fake proves
// the store's own chunking rather than merely agreeing with itself.
type readyOutcomeFanoutStorage struct {
	beadslib.Storage
	edges  map[string][]*beadslib.Dependency
	issues map[string]*beadslib.Issue

	// getErr, when set, is returned instead of a normal answer for exactly
	// this blocker id — the mutation-check lever for "a real blocker-read
	// failure must fail the whole filter."
	getErr   map[string]error
	getDelay chan struct{}
	// atGate, when set, receives one signal from each Get call the moment it
	// reaches getDelay, so a test can wait for calls to park on the gate
	// instead of polling inFlight. It must be buffered for every call that
	// can reach the gate, so a send never blocks.
	atGate chan struct{}

	mu            sync.Mutex
	edgeReadCalls int
	edgeReadSizes []int
	listCalls     int
	getCalls      int
	getCallIDs    []string

	inFlight    int32
	maxInFlight int32
}

// readyOutcomeFanoutMaxReadEdgesAnchors mirrors the real server's own anchor
// cap (beads internal/httpapi/edges.go: maxDependencyAnchors = 100, confirmed
// against a real bd-serve, which rejects the WHOLE call — not just the ids past
// the limit — with a 400 invalid_argument once a ReadEdges request names more
// anchors than this). It is hardcoded here, independently of
// nativeReadEdgesChunkSize, so a test against this fake actually proves the
// store's chunking keeps every call under the server's real limit rather than
// merely agreeing with itself.
const readyOutcomeFanoutMaxReadEdgesAnchors = 100

func (f *readyOutcomeFanoutStorage) EdgeReader() (issueops.EdgeReader, error) {
	return &readyOutcomeFanoutEdgeReader{parent: f}, nil
}

func (f *readyOutcomeFanoutStorage) IssueReader() (issueops.Reader, error) {
	return &readyOutcomeFanoutIssueReader{parent: f}, nil
}

type readyOutcomeFanoutEdgeReader struct {
	parent *readyOutcomeFanoutStorage
}

func (r *readyOutcomeFanoutEdgeReader) ReadEdges(_ context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
	r.parent.mu.Lock()
	r.parent.edgeReadCalls++
	r.parent.edgeReadSizes = append(r.parent.edgeReadSizes, len(req.IDs))
	r.parent.mu.Unlock()
	if len(req.IDs) > readyOutcomeFanoutMaxReadEdgesAnchors {
		return issueops.EdgeReadResult{}, fmt.Errorf("invalid_argument: at most %d issue_id values per request, got %d", readyOutcomeFanoutMaxReadEdgesAnchors, len(req.IDs))
	}
	result := issueops.EdgeReadResult{Anchors: make([]issueops.AnchorEdges, 0, len(req.IDs))}
	for _, id := range req.IDs {
		edges, ok := r.parent.edges[id]
		result.Anchors = append(result.Anchors, issueops.AnchorEdges{ID: id, Edges: edges, Missing: !ok})
	}
	return result, nil
}

type readyOutcomeFanoutIssueReader struct {
	parent *readyOutcomeFanoutStorage
}

func (r *readyOutcomeFanoutIssueReader) Ready(context.Context, issueops.ReadyRequest) (issueops.IssuePage, error) {
	return issueops.IssuePage{}, errors.New("readyOutcomeFanoutIssueReader: Ready not implemented; filterReadyByWorkOutcome must never call it")
}

// List refuses exactly as the http client's encoder does for
// ListRequest.IDFilter: a typed *beadslib.ErrUnsupported, client-side, before
// any round trip. filterReadyByWorkOutcome must try this door first (and this
// fake must see exactly one such attempt) and fall back to the Get fan-out on
// this refusal, never treat the refusal as "no blockers found."
func (r *readyOutcomeFanoutIssueReader) List(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
	r.parent.mu.Lock()
	r.parent.listCalls++
	r.parent.mu.Unlock()
	return issueops.IssuePage{}, &beadslib.ErrUnsupported{Op: "List", Backend: "http"}
}

func (r *readyOutcomeFanoutIssueReader) Get(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
	p := r.parent
	p.mu.Lock()
	err := p.getErr[req.ID]
	p.mu.Unlock()
	if err != nil {
		p.mu.Lock()
		p.getCalls++
		p.getCallIDs = append(p.getCallIDs, req.ID)
		p.mu.Unlock()
		return nil, err
	}
	in := atomic.AddInt32(&p.inFlight, 1)
	defer atomic.AddInt32(&p.inFlight, -1)
	for {
		prev := atomic.LoadInt32(&p.maxInFlight)
		if in <= prev || atomic.CompareAndSwapInt32(&p.maxInFlight, prev, in) {
			break
		}
	}
	if p.getDelay != nil {
		if p.atGate != nil {
			p.atGate <- struct{}{}
		}
		<-p.getDelay
	}
	p.mu.Lock()
	p.getCalls++
	p.getCallIDs = append(p.getCallIDs, req.ID)
	p.mu.Unlock()
	issue, ok := p.issues[req.ID]
	if !ok || issue == nil {
		return nil, beadslib.ErrNotFound
	}
	details := issueops.IssueDetails{Issue: *issue}
	return &details, nil
}

func fanoutIssue(id string, status beadslib.Status, metadataJSON string) *beadslib.Issue {
	issue := &beadslib.Issue{ID: id, Status: status}
	if metadataJSON != "" {
		issue.Metadata = []byte(metadataJSON)
	}
	return issue
}

// fanoutEdge builds a blocks edge. Every call site in this file names a
// blocking dependency specifically — the filter under test only vetoes
// through blocks edges — so the type is fixed rather than threaded through as
// a parameter every caller would pass "blocks" to anyway.
func fanoutEdge(from, to string) *beadslib.Dependency {
	return &beadslib.Dependency{IssueID: from, DependsOnID: to, Type: beadslib.DepBlocks}
}

const fanoutBlockedMeta = `{"gc.work_outcome":"blocked"}`

// TestNativeDoltStoreReadyWorkOutcomeFilterChunksOverAnchorCap pins that a
// ready frontier wider than the server's 100-anchor cap never sends one failing
// ReadEdges call. The fake enforces the same cap the real server does
// (readyOutcomeFanoutMaxReadEdgesAnchors), so this test fails the way the real
// server's 400 invalid_argument would if nativeReadEdgesChunkSize's chunking is
// ever removed or widened past the server's real limit.
func TestNativeDoltStoreReadyWorkOutcomeFilterChunksOverAnchorCap(t *testing.T) {
	const total = 250
	candidates := make([]Bead, 0, total)
	edges := make(map[string][]*beadslib.Dependency, total)
	issues := map[string]*beadslib.Issue{
		"gc-shared-blocker": fanoutIssue("gc-shared-blocker", beadslib.StatusOpen, ""),
	}
	for i := range total {
		id := fmt.Sprintf("gc-wide-%03d", i)
		candidates = append(candidates, Bead{ID: id})
		edges[id] = []*beadslib.Dependency{fanoutEdge(id, "gc-shared-blocker")}
	}
	storage := &readyOutcomeFanoutStorage{edges: edges, issues: issues}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.filterReadyByWorkOutcome(context.Background(), storage, candidates)
	if err != nil {
		t.Fatalf("filterReadyByWorkOutcome: %v", err)
	}
	if len(got) != total {
		t.Errorf("filtered = %d candidates, want all %d kept (the shared blocker is open, so nothing is vetoed)", len(got), total)
	}
	const wantEdgeReadCalls = 3 // ceil(250/100): chunks of 100, 100, 50
	if storage.edgeReadCalls != wantEdgeReadCalls {
		t.Errorf("ReadEdges called %d times, want %d (chunked at the server's 100-anchor cap instead of one call for all %d candidates)", storage.edgeReadCalls, wantEdgeReadCalls, total)
	}
	for _, size := range storage.edgeReadSizes {
		if size > readyOutcomeFanoutMaxReadEdgesAnchors {
			t.Errorf("one ReadEdges call carried %d anchors, want <= %d", size, readyOutcomeFanoutMaxReadEdgesAnchors)
		}
	}
}

// TestNativeDoltStoreReadyWorkOutcomeFilterFallsBackToGetWhenListRefuses pins
// the List-then-Get fallback: filterReadyByWorkOutcome tries
// IssueReader.List(IDFilter) first (exactly once), and on an http-native
// refusal (the http client's encoder refuses E-ListRequest.IDFilter
// unconditionally) falls back to IssueReader.Get, one call per DISTINCT
// ready-blocking target.
func TestNativeDoltStoreReadyWorkOutcomeFilterFallsBackToGetWhenListRefuses(t *testing.T) {
	storage := &readyOutcomeFanoutStorage{
		edges: map[string][]*beadslib.Dependency{
			"gc-a": {fanoutEdge("gc-a", "gc-blocker-open")},
			"gc-b": {fanoutEdge("gc-b", "gc-blocker-blocked")},
		},
		issues: map[string]*beadslib.Issue{
			"gc-blocker-open":    fanoutIssue("gc-blocker-open", beadslib.StatusOpen, ""),
			"gc-blocker-blocked": fanoutIssue("gc-blocker-blocked", beadslib.StatusClosed, fanoutBlockedMeta),
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.filterReadyByWorkOutcome(context.Background(), storage, []Bead{{ID: "gc-a"}, {ID: "gc-b"}})
	if err != nil {
		t.Fatalf("filterReadyByWorkOutcome: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-a" {
		t.Errorf("filtered = %v, want only gc-a (gc-b's blocker is closed+blocked)", readyOutcomeFanoutIDs(got))
	}
	if storage.listCalls != 1 {
		t.Errorf("List called %d times, want 1 (the cheap-first attempt, which this fake refuses)", storage.listCalls)
	}
	if storage.getCalls != 2 {
		t.Errorf("Get called %d times, want 2 (one per distinct blocker, via the fallback)", storage.getCalls)
	}
}

// TestNativeDoltStoreReadyWorkOutcomeFetchBlockersJudgesByReadinessWorkOutcome
// pins #7264's readiness rule on the Get fan-out leg, which the shared
// conformance suite cannot reach (its store answers List+IDFilter): a closed
// formula step that passed satisfies its dependent whatever its
// gc.work_outcome, while a plain work bead closed blocked keeps vetoing even
// with gc.outcome=pass.
func TestNativeDoltStoreReadyWorkOutcomeFetchBlockersJudgesByReadinessWorkOutcome(t *testing.T) {
	storage := &readyOutcomeFanoutStorage{
		edges: map[string][]*beadslib.Dependency{
			"gc-a": {fanoutEdge("gc-a", "gc-step-passed")},
			"gc-b": {fanoutEdge("gc-b", "gc-work-passed")},
		},
		issues: map[string]*beadslib.Issue{
			"gc-step-passed": fanoutIssue("gc-step-passed", beadslib.StatusClosed, `{"gc.work_outcome":"blocked","gc.step_ref":"mol.step","gc.outcome":"pass"}`),
			"gc-work-passed": fanoutIssue("gc-work-passed", beadslib.StatusClosed, `{"gc.work_outcome":"blocked","gc.outcome":"pass"}`),
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.filterReadyByWorkOutcome(context.Background(), storage, []Bead{{ID: "gc-a"}, {ID: "gc-b"}})
	if err != nil {
		t.Fatalf("filterReadyByWorkOutcome: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-a" {
		t.Errorf("filtered = %v, want only gc-a (gc-a's blocker is a passed step; gc-b's is a work bead closed blocked)", readyOutcomeFanoutIDs(got))
	}
	if storage.getCalls != 2 {
		t.Errorf("Get called %d times, want 2 (both blockers judged through the fallback fan-out)", storage.getCalls)
	}
}

// TestNativeDoltStoreReadyWorkOutcomeFilterFansOutConcurrently pins that the
// per-blocker Get calls run concurrently, bounded at
// nativeReadyEdgeFanoutLimit, rather than serially one at a time.
func TestNativeDoltStoreReadyWorkOutcomeFilterFansOutConcurrently(t *testing.T) {
	candidates := make([]Bead, 0, 20)
	edges := make(map[string][]*beadslib.Dependency, 20)
	issues := make(map[string]*beadslib.Issue, 20)
	for i := range 20 {
		id := fmt.Sprintf("gc-fanout-%02d", i)
		blocker := fmt.Sprintf("gc-fanout-blocker-%02d", i)
		candidates = append(candidates, Bead{ID: id})
		edges[id] = []*beadslib.Dependency{fanoutEdge(id, blocker)}
		issues[blocker] = fanoutIssue(blocker, beadslib.StatusOpen, "")
	}
	gate := make(chan struct{})
	atGate := make(chan struct{}, len(candidates))
	storage := &readyOutcomeFanoutStorage{edges: edges, issues: issues, getDelay: gate, atGate: atGate}
	store := newNativeDoltStoreForTest(storage)

	// A LITERAL bound, independent of nativeReadyEdgeFanoutLimit (mirrors
	// TestDepListBatchNeverExceedsTheServerAnchorCap's literal
	// realBdServeReadEdgesAnchorCap): comparing against the production constant
	// itself would move both sides of the inequality together under a mutation
	// that widens the constant, so the bound could never fail. It also doubles
	// as the settle target below: with 20 candidates and
	// nativeReadyEdgeFanoutLimit well under that, the correct implementation
	// reaches exactly this many in flight almost immediately, so waiting for it
	// costs the passing case nothing.
	const wantMaxFanout = 8

	done := make(chan struct{})
	go func() {
		_, err := store.filterReadyByWorkOutcome(context.Background(), storage, candidates)
		if err != nil {
			t.Errorf("filterReadyByWorkOutcome: %v", err)
		}
		close(done)
	}()

	// Wait for wantMaxFanout Get calls to park on the gate before draining
	// any of them, so the in-flight peak is observed rather than raced: each
	// Get signals atGate the moment it reaches getDelay. The correct, bounded
	// implementation parks exactly nativeReadyEdgeFanoutLimit calls almost
	// immediately; an implementation that never reaches wantMaxFanout
	// concurrent calls fails here at the deadline instead of passing.
	deadline := time.After(10 * time.Second)
	for parked := 0; parked < wantMaxFanout; parked++ {
		select {
		case <-atGate:
		case <-deadline:
			t.Fatalf("timed out waiting for %d Get calls to park on the gate (saw %d)", wantMaxFanout, parked)
		}
	}
	for range candidates {
		gate <- struct{}{}
	}
	<-done

	if storage.maxInFlight <= 1 {
		t.Errorf("max concurrent Get calls = %d, want > 1 (the fan-out must run concurrently, not serially)", storage.maxInFlight)
	}
	if storage.maxInFlight > wantMaxFanout {
		t.Errorf("max concurrent Get calls = %d, want <= %d (nativeReadyEdgeFanoutLimit should still be %d)", storage.maxInFlight, wantMaxFanout, wantMaxFanout)
	}
}

// TestNativeDoltStoreReadyWorkOutcomeFilterPropagatesBlockerFetchFailure is
// the mutation-check: a blocker whose Get call fails with a real
// (non-ErrNotFound) error must fail the whole filter rather than silently
// passing the candidate it would have vetoed.
func TestNativeDoltStoreReadyWorkOutcomeFilterPropagatesBlockerFetchFailure(t *testing.T) {
	wantErr := errors.New("blocker read blew up")
	storage := &readyOutcomeFanoutStorage{
		edges: map[string][]*beadslib.Dependency{
			"gc-vetoed": {fanoutEdge("gc-vetoed", "gc-gaveup")},
		},
		issues: map[string]*beadslib.Issue{
			"gc-gaveup": fanoutIssue("gc-gaveup", beadslib.StatusClosed, fanoutBlockedMeta),
		},
		getErr: map[string]error{"gc-gaveup": wantErr},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.filterReadyByWorkOutcome(context.Background(), storage, []Bead{{ID: "gc-vetoed"}})
	if err == nil {
		t.Fatalf("filterReadyByWorkOutcome: want an error when a blocker read fails, got filtered=%v", readyOutcomeFanoutIDs(got))
	}
	if !errors.Is(err, wantErr) && !strings.Contains(err.Error(), wantErr.Error()) {
		t.Errorf("error %q does not wrap/name %q", err, wantErr)
	}
	if len(got) != 0 {
		t.Errorf("filtered = %v, want empty: a failed blocker read must not silently pass the candidate it would have vetoed", readyOutcomeFanoutIDs(got))
	}
}

// TestNativeDoltStoreReadyWorkOutcomeFilterReportsMalformedBlockerMetadata
// pins the malformed-metadata failure contract: a target whose metadata does
// not parse is a named error, never a silently-kept or silently-dropped
// candidate.
func TestNativeDoltStoreReadyWorkOutcomeFilterReportsMalformedBlockerMetadata(t *testing.T) {
	storage := &readyOutcomeFanoutStorage{
		edges: map[string][]*beadslib.Dependency{
			"gc-dep": {fanoutEdge("gc-dep", "gc-garbled")},
		},
		issues: map[string]*beadslib.Issue{
			"gc-garbled": fanoutIssue("gc-garbled", beadslib.StatusClosed, `{not json`),
		},
	}
	store := newNativeDoltStoreForTest(storage)
	_, err := store.filterReadyByWorkOutcome(context.Background(), storage, []Bead{{ID: "gc-dep"}})
	if err == nil {
		t.Fatalf("filterReadyByWorkOutcome: want an error for malformed blocker metadata, got nil")
	}
}

// TestNativeDoltStoreReadyWorkOutcomeFilterNoBlockingEdgesSkipsIssueReader
// pins the short-circuit: a candidate set with no ready-blocking edges at all
// must never dial IssueReader.Get (or List).
func TestNativeDoltStoreReadyWorkOutcomeFilterNoBlockingEdgesSkipsIssueReader(t *testing.T) {
	storage := &readyOutcomeFanoutStorage{edges: map[string][]*beadslib.Dependency{}, issues: map[string]*beadslib.Issue{}}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.filterReadyByWorkOutcome(context.Background(), storage, []Bead{{ID: "gc-free"}})
	if err != nil {
		t.Fatalf("filterReadyByWorkOutcome: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-free" {
		t.Errorf("filtered = %v, want [gc-free]", readyOutcomeFanoutIDs(got))
	}
	if storage.getCalls != 0 {
		t.Errorf("Get called %d times, want 0 (no ready-blocking target to look up)", storage.getCalls)
	}
	if storage.listCalls != 0 {
		t.Errorf("List called %d times, want 0 (no ready-blocking target to look up)", storage.listCalls)
	}
}

// readyOutcomeFanoutListStorage is a beadslib.Storage shaped like a NATIVE
// (dolt or Postgres) backend: its IssueReader.List(IDFilter) answers directly,
// unlike http-native's refusal. filterReadyByWorkOutcome must try this door
// FIRST and never fall back to a per-id Get when it succeeds: a per-id
// fan-out against a backend that answers in one call is the #6491 cost.
type readyOutcomeFanoutListStorage struct {
	beadslib.Storage
	edges  map[string][]*beadslib.Dependency
	issues map[string]*beadslib.Issue

	mu         sync.Mutex
	listCalls  int
	listIDs    []string
	listLimits []*int
	getCalls   int
}

func (f *readyOutcomeFanoutListStorage) EdgeReader() (issueops.EdgeReader, error) {
	return &readyOutcomeFanoutListEdgeReader{parent: f}, nil
}

func (f *readyOutcomeFanoutListStorage) IssueReader() (issueops.Reader, error) {
	return &readyOutcomeFanoutListIssueReader{parent: f}, nil
}

type readyOutcomeFanoutListEdgeReader struct {
	parent *readyOutcomeFanoutListStorage
}

func (r *readyOutcomeFanoutListEdgeReader) ReadEdges(_ context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
	result := issueops.EdgeReadResult{Anchors: make([]issueops.AnchorEdges, 0, len(req.IDs))}
	for _, id := range req.IDs {
		edges, ok := r.parent.edges[id]
		result.Anchors = append(result.Anchors, issueops.AnchorEdges{ID: id, Edges: edges, Missing: !ok})
	}
	return result, nil
}

type readyOutcomeFanoutListIssueReader struct {
	parent *readyOutcomeFanoutListStorage
}

func (r *readyOutcomeFanoutListIssueReader) Ready(context.Context, issueops.ReadyRequest) (issueops.IssuePage, error) {
	return issueops.IssuePage{}, errors.New("readyOutcomeFanoutListIssueReader: Ready not implemented; filterReadyByWorkOutcome must never call it")
}

// readyOutcomeFanoutListDefaultPage stands in for beads 1.3.1's real
// workapi.DefaultListLimit (read_roles.go's own nativeListReadRequest never
// sets ListRequest.Limit, and a nil Limit there means "the shared list
// default," not unlimited): this fake enforces the identical rule — a nil Limit
// truncates to this many rows, an explicit *Limit of 0 is unlimited — so a unit
// test against it can reproduce ready-veto truncation in milliseconds, without
// standing up a real upstream store to rediscover beads' own default.
const readyOutcomeFanoutListDefaultPage = 3

// List answers the IDFilter directly from the graph, exactly as a native
// IssueReader does — proving filterReadyByWorkOutcome's List-first attempt is
// actually wired to this door, not merely assumed to always refuse. It
// enforces readyOutcomeFanoutListDefaultPage on a nil Limit, mirroring
// beads' own nil-means-default-fifty rule, so a caller that forgets to set an
// explicit Limit sees exactly the truncated page a real served backend would
// hand it.
func (r *readyOutcomeFanoutListIssueReader) List(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
	p := r.parent
	p.mu.Lock()
	p.listCalls++
	p.listIDs = append(p.listIDs, req.IDFilter)
	p.listLimits = append(p.listLimits, req.Limit)
	p.mu.Unlock()
	var page issueops.IssuePage
	if req.IDFilter == "" {
		return page, nil
	}
	limit := readyOutcomeFanoutListDefaultPage
	if req.Limit != nil {
		limit = *req.Limit // 0 means unlimited, exactly as issueops.ListRequest.Limit documents.
	}
	for _, id := range strings.Split(req.IDFilter, ",") {
		if limit > 0 && len(page.Items) >= limit {
			page.HasMore = true
			break
		}
		issue, ok := p.issues[id]
		if !ok || issue == nil {
			continue
		}
		cp := *issue
		page.Items = append(page.Items, &issueops.IssueWithCounts{Issue: &cp})
	}
	return page, nil
}

// Get fails the test outright rather than merely returning an error: a
// filterReadyByWorkOutcome that regressed to always fanning out through Get
// would otherwise pass silently against this fake.
func (r *readyOutcomeFanoutListIssueReader) Get(context.Context, issueops.GetRequest) (*issueops.IssueDetails, error) {
	r.parent.mu.Lock()
	r.parent.getCalls++
	r.parent.mu.Unlock()
	return nil, errors.New("readyOutcomeFanoutListIssueReader: Get called; filterReadyByWorkOutcome must prefer List+IDFilter when the reader serves it")
}

// TestNativeDoltStoreReadyWorkOutcomeFilterPrefersListOverGetFanout pins the
// List-first read: against a native-shaped IssueReader that answers
// List+IDFilter, filterReadyByWorkOutcome must use that single call and never
// fall back to a per-id Get fan-out (the #6491 cost: several round trips per
// blocker, every controller tick, against a backend that could answer in one).
func TestNativeDoltStoreReadyWorkOutcomeFilterPrefersListOverGetFanout(t *testing.T) {
	storage := &readyOutcomeFanoutListStorage{
		edges: map[string][]*beadslib.Dependency{
			"gc-a": {fanoutEdge("gc-a", "gc-blocker-open")},
			"gc-b": {fanoutEdge("gc-b", "gc-blocker-blocked")},
		},
		issues: map[string]*beadslib.Issue{
			"gc-blocker-open":    fanoutIssue("gc-blocker-open", beadslib.StatusOpen, ""),
			"gc-blocker-blocked": fanoutIssue("gc-blocker-blocked", beadslib.StatusClosed, fanoutBlockedMeta),
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.filterReadyByWorkOutcome(context.Background(), storage, []Bead{{ID: "gc-a"}, {ID: "gc-b"}})
	if err != nil {
		t.Fatalf("filterReadyByWorkOutcome: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-a" {
		t.Errorf("filtered = %v, want only gc-a (gc-b's blocker is closed+blocked)", readyOutcomeFanoutIDs(got))
	}
	if storage.listCalls != 1 {
		t.Errorf("List called %d times, want 1", storage.listCalls)
	}
	if storage.getCalls != 0 {
		t.Errorf("Get called %d times, want 0: a native reader answers List+IDFilter directly, so the fan-out must never run", storage.getCalls)
	}
}

// TestNativeDoltStoreReadyWorkOutcomeListBlockersSetsAnExplicitUnlimitedLimit
// is the fast unit-level pin on the blocker List's explicit unlimited Limit:
// filterReadyByWorkOutcomeListBlockers builds its List+IDFilter request from
// nativeListReadRequest(), which never touches Limit, and a nil
// issueops.ListRequest.Limit means "the shared list default" — beads 1.3.1's
// workapi.DefaultListLimit, fifty rows — not unlimited. Left at nil, a ready
// frontier with more than that many DISTINCT blockers would see only the first
// page's worth of outcomes, so a blocker past the page could never veto
// readiness.
//
// readyOutcomeFanoutListDefaultPage (3, not 50) stands in for the real default
// so this reproduces in milliseconds: four blockers, closed with
// work_outcome=blocked, each gating a distinct candidate. Only the first
// readyOutcomeFanoutListDefaultPage of them would veto their candidate under a
// nil-Limit request; the store must veto all four.
func TestNativeDoltStoreReadyWorkOutcomeListBlockersSetsAnExplicitUnlimitedLimit(t *testing.T) {
	const blockerCount = readyOutcomeFanoutListDefaultPage + 1
	edges := make(map[string][]*beadslib.Dependency, blockerCount)
	issues := make(map[string]*beadslib.Issue, blockerCount)
	candidates := make([]Bead, 0, blockerCount)
	for i := 0; i < blockerCount; i++ {
		candID := fmt.Sprintf("gc-cand-%02d", i)
		blockerID := fmt.Sprintf("gc-blocker-%02d", i)
		candidates = append(candidates, Bead{ID: candID})
		edges[candID] = []*beadslib.Dependency{fanoutEdge(candID, blockerID)}
		issues[blockerID] = fanoutIssue(blockerID, beadslib.StatusClosed, fanoutBlockedMeta)
	}
	storage := &readyOutcomeFanoutListStorage{edges: edges, issues: issues}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.filterReadyByWorkOutcome(context.Background(), storage, candidates)
	if err != nil {
		t.Fatalf("filterReadyByWorkOutcome: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("filtered = %v, want none: every one of the %d candidates' sole blocker is closed+blocked, including the ones past a %d-row default page", readyOutcomeFanoutIDs(got), blockerCount, readyOutcomeFanoutListDefaultPage)
	}
	for i, limit := range storage.listLimits {
		if limit == nil {
			t.Errorf("List call %d: Limit = nil, want an explicit *int (nil reads as the backend's default page, not unlimited)", i)
			continue
		}
		if *limit != 0 {
			t.Errorf("List call %d: Limit = %d, want 0 (explicit unlimited, per issueops.ListRequest.Limit's own doc)", i, *limit)
		}
	}
}

// TestNativeDoltStoreReadyWorkOutcomeListBlockersChunksTheIDFilter pins that
// filterReadyByWorkOutcomeListBlockers bounds every List+IDFilter call's id
// count at nativeReadyVetoListChunkSize, reusing the one server-enforced
// id-count cap this package already knows a served backend accepts
// (nativeServerEdgeAnchorCap, shared with ReadEdges and DepListBatch) rather
// than sending an unbounded id set on one call's IDFilter.
func TestNativeDoltStoreReadyWorkOutcomeListBlockersChunksTheIDFilter(t *testing.T) {
	// Each candidate names its OWN distinct blocker (unlike
	// TestNativeDoltStoreReadyWorkOutcomeFilterChunksOverAnchorCap's shared
	// one), so the DISTINCT blocker-id set this test chunks over actually
	// grows past nativeReadyVetoListChunkSize instead of deduplicating to one.
	const total = nativeReadyVetoListChunkSize + 50
	edges := make(map[string][]*beadslib.Dependency, total)
	issues := make(map[string]*beadslib.Issue, total)
	candidates := make([]Bead, 0, total)
	for i := 0; i < total; i++ {
		candID := fmt.Sprintf("gc-wide-%03d", i)
		blockerID := fmt.Sprintf("gc-wide-blocker-%03d", i)
		candidates = append(candidates, Bead{ID: candID})
		edges[candID] = []*beadslib.Dependency{fanoutEdge(candID, blockerID)}
		issues[blockerID] = fanoutIssue(blockerID, beadslib.StatusOpen, "")
	}
	storage := &readyOutcomeFanoutListStorage{edges: edges, issues: issues}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.filterReadyByWorkOutcome(context.Background(), storage, candidates)
	if err != nil {
		t.Fatalf("filterReadyByWorkOutcome: %v", err)
	}
	if len(got) != total {
		t.Errorf("filtered = %d candidates, want all %d kept (the shared blocker is open, so nothing is vetoed)", len(got), total)
	}
	const wantListCalls = 2 // ceil(150/100): chunks of 100, 50
	if storage.listCalls != wantListCalls {
		t.Errorf("List called %d times, want %d (chunked at nativeReadyVetoListChunkSize instead of one call naming all %d ids)", storage.listCalls, wantListCalls, total)
	}
	for _, ids := range storage.listIDs {
		if n := strings.Count(ids, ",") + 1; n > nativeReadyVetoListChunkSize {
			t.Errorf("one List call carried %d ids, want <= %d", n, nativeReadyVetoListChunkSize)
		}
	}
}

// TestNativeDoltStoreReadyWorkOutcomeFetchBlockersTreatsNotFoundAsNoEvidence
// pins the fallback fan-out's not-found tolerance: a blocker Get reports
// ErrNotFound for must be dropped as no evidence of blocking, never fail the
// whole call.
func TestNativeDoltStoreReadyWorkOutcomeFetchBlockersTreatsNotFoundAsNoEvidence(t *testing.T) {
	reader := fanoutGetOnlyReader{get: func(context.Context, issueops.GetRequest) (*issueops.IssueDetails, error) {
		return nil, beadslib.ErrNotFound
	}}
	vetoes, malformed, err := filterReadyByWorkOutcomeFetchBlockers(context.Background(), reader, []string{"gc-missing"})
	if err != nil {
		t.Fatalf("filterReadyByWorkOutcomeFetchBlockers: %v (an ErrNotFound blocker must be tolerated, not fail the call)", err)
	}
	if vetoes["gc-missing"] {
		t.Errorf("vetoes[gc-missing] = true, want false/absent: a missing blocker is no evidence of blocking")
	}
	if malformed["gc-missing"] != nil {
		t.Errorf("malformed[gc-missing] = %v, want nil", malformed["gc-missing"])
	}
}

// TestNativeDoltStoreReadyWorkOutcomeFetchBlockersRejectsNilDetailsWithNilError
// pins the nil-details guard: an IssueReader.Get that returns (nil, nil) — a
// contract violation issueops.Reader never promises — must fail the call
// rather than being silently treated as "no evidence of blocking".
func TestNativeDoltStoreReadyWorkOutcomeFetchBlockersRejectsNilDetailsWithNilError(t *testing.T) {
	reader := fanoutGetOnlyReader{get: func(context.Context, issueops.GetRequest) (*issueops.IssueDetails, error) {
		return nil, nil
	}}
	_, _, err := filterReadyByWorkOutcomeFetchBlockers(context.Background(), reader, []string{"gc-broken"})
	if err == nil {
		t.Fatalf("filterReadyByWorkOutcomeFetchBlockers: want an error when IssueReader.Get returns (nil, nil), got nil")
	}
}

// TestNativeDoltStoreReadyWorkOutcomeFetchBlockersSkipsQueuedCallsAfterCancel
// pins the skip-after-cancel contract: once one blocker's Get fails for real,
// every call still queued behind the semaphore must never dial IssueReader at
// all.
//
// gc-fail occupies one of the nativeReadyEdgeFanoutLimit concurrent slots and
// fails immediately, canceling the shared fetch context; cancelFetch() runs,
// in program order, strictly before gc-fail's goroutine releases its
// semaphore slot (the release is a deferred statement registered after the
// cancel), so every later dispatch — including every "queued" id below, which
// can only start once a slot frees — observes the cancellation before it ever
// calls Get. The other nativeReadyEdgeFanoutLimit-1 slots are held open by
// "gc-hold-*" ids that block on a channel the test only closes once the
// result is already being awaited, so they never race the cancellation by
// finishing first.
func TestNativeDoltStoreReadyWorkOutcomeFetchBlockersSkipsQueuedCallsAfterCancel(t *testing.T) {
	holders := make([]string, 0, nativeReadyEdgeFanoutLimit-1)
	for i := 0; i < nativeReadyEdgeFanoutLimit-1; i++ {
		holders = append(holders, fmt.Sprintf("gc-hold-%02d", i))
	}
	queued := []string{"gc-queued-00", "gc-queued-01", "gc-queued-02", "gc-queued-03", "gc-queued-04"}
	blockerIDs := append([]string{"gc-fail"}, holders...)
	blockerIDs = append(blockerIDs, queued...)

	// fetchBlockersSkipHookForTest fires exactly when a dispatch observes the
	// shared fetch context already canceled and skips without dialing
	// IssueReader — the event this test exists to pin for every "queued" id.
	// Waiting on skipped is the deterministic replacement for a settle sleep:
	// it reports the moment each skip actually happened, rather than a
	// duration hoped to be long enough for the cascade to finish first. Every
	// holder blocks on the UNCLOSED release channel below, so the only
	// semaphore slot that can free before release closes is gc-fail's own:
	// holders structurally cannot race ahead of it, and per the
	// happens-before chain through gc-fail's own semaphore release
	// (cancelFetch() runs, then gc-fail's goroutine returns, then its
	// deferred <-sem fires, all in that goroutine's program order), every
	// dispatch that follows is guaranteed — not merely likely — to observe
	// the cancellation and skip.
	skipped := make(chan string, len(blockerIDs))
	t.Cleanup(func() { fetchBlockersSkipHookForTest = nil })
	fetchBlockersSkipHookForTest = func(id string) { skipped <- id }

	var mu sync.Mutex
	var dialed []string
	release := make(chan struct{})
	reader := fanoutGetOnlyReader{get: func(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
		mu.Lock()
		dialed = append(dialed, req.ID)
		mu.Unlock()
		if req.ID == "gc-fail" {
			return nil, errors.New("boom")
		}
		<-release
		return &issueops.IssueDetails{Issue: beadslib.Issue{ID: req.ID, Status: beadslib.StatusOpen}}, nil
	}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, err := filterReadyByWorkOutcomeFetchBlockers(context.Background(), reader, blockerIDs); err == nil {
			t.Error("filterReadyByWorkOutcomeFetchBlockers: want an error from gc-fail's real failure, got nil")
		}
	}()

	// Wait for a distinct skip signal for every "queued" id rather than a
	// fixed sleep. If the skip ever regressed and a queued id was dialed
	// instead of skipped, it would never signal here (it would instead block
	// on the unclosed release channel inside the fake reader), so this times
	// out with a clear message rather than hanging forever.
	seen := make(map[string]bool, len(queued))
	for len(seen) < len(queued) {
		select {
		case id := <-skipped:
			seen[id] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for all %d queued ids to be skipped; seen so far: %v", len(queued), seen)
		}
	}
	close(release)
	<-done

	mu.Lock()
	got := append([]string(nil), dialed...)
	mu.Unlock()
	dialedSet := make(map[string]bool, len(got))
	for _, id := range got {
		dialedSet[id] = true
	}
	for _, id := range queued {
		if dialedSet[id] {
			t.Errorf("Get dialed %q, want it skipped: it was still queued behind the semaphore when gc-fail canceled the fetch", id)
		}
	}
	if len(got) > nativeReadyEdgeFanoutLimit {
		t.Errorf("Get dialed %d times (%v), want <= %d (nativeReadyEdgeFanoutLimit): the rest should have been skipped after cancel", len(got), got, nativeReadyEdgeFanoutLimit)
	}
}

// fanoutGetOnlyReader is a minimal issueops.Reader whose Get is a caller-
// supplied closure; Ready and List fail the test outright if ever invoked,
// since the helpers under test here (filterReadyByWorkOutcomeFetchBlockers)
// never call them.
type fanoutGetOnlyReader struct {
	get func(context.Context, issueops.GetRequest) (*issueops.IssueDetails, error)
}

func (r fanoutGetOnlyReader) Ready(context.Context, issueops.ReadyRequest) (issueops.IssuePage, error) {
	return issueops.IssuePage{}, errors.New("fanoutGetOnlyReader: Ready not implemented")
}

func (r fanoutGetOnlyReader) List(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
	return issueops.IssuePage{}, errors.New("fanoutGetOnlyReader: List not implemented")
}

func (r fanoutGetOnlyReader) Get(ctx context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
	return r.get(ctx, req)
}

func readyOutcomeFanoutIDs(beads []Bead) []string {
	ids := make([]string, 0, len(beads))
	for _, b := range beads {
		ids = append(ids, b.ID)
	}
	return ids
}

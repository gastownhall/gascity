package beads

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The read re-point's own tests: what the store must ASK the reader role for,
// and what it must do with the answer. The behavioral tests that ride the raw
// searchIssues/getReadyWork fixtures live where they always did — the reader
// double forwards onto those hooks, so they keep testing what they tested.

func TestNativeDoltStoreListLiftsEveryDefaultSuppression(t *testing.T) {
	var captured issueops.ListRequest
	storage := &nativeDoltReaderSpy{
		list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			captured = req
			return issueops.IssuePage{}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.List(ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("List: %v", err)
	}

	// Each of these lifts a suppression the ROLE applies by default and the raw
	// search did not. A missing one silently narrows every gc listing.
	if !captured.AllFlag {
		t.Error("AllFlag = false; the default status exclusions AND the pinned-flag default stay in place")
	}
	if !captured.IncludeTemplates {
		t.Error("IncludeTemplates = false; template rows drop out of every listing")
	}
	if !captured.IncludeGates {
		t.Error("IncludeGates = false; gate beads drop out of every listing")
	}
	if !captured.IncludeInfra {
		t.Error("IncludeInfra = false; message/agent/role beads drop out of every listing")
	}
	if !captured.IncludeEphemeral {
		t.Error("IncludeEphemeral = false; the wisp plane is not read at all")
	}
}

// The four quadrants, THROUGH the adapter: {durable, wisp} x {ordinary type,
// infra type}. A TierBoth listing must return all four, and the two flags that
// keep them are not interchangeable — the infra one is a TYPE knob that also
// admits the plane, the ephemeral one is a PLANE knob only, so dropping either
// loses a different quadrant. Mail (type=message) is the infra quadrant gc
// cannot afford to lose: it is how every mailbox read answers.
func TestNativeDoltStoreListTierBothReturnsAllFourQuadrants(t *testing.T) {
	issues := []*beadslib.Issue{
		{ID: "gc-task", Title: "durable task", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-task-wisp", Title: "task wisp", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2, Ephemeral: true},
		{ID: "gc-mail", Title: "durable mail", Status: beadslib.StatusOpen, IssueType: beadslib.IssueType("message"), Priority: 2},
		{ID: "gc-mail-wisp", Title: "mail wisp", Status: beadslib.StatusOpen, IssueType: beadslib.IssueType("message"), Priority: 2, Ephemeral: true},
	}
	storage := &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			out := make([]*beadslib.Issue, 0, len(issues))
			for _, issue := range issues {
				out = append(out, cloneNativeIssueForTest(issue))
			}
			return out, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := map[string]bool{}
	for _, bead := range got {
		seen[bead.ID] = true
	}
	for _, want := range []string{"gc-task", "gc-task-wisp", "gc-mail", "gc-mail-wisp"} {
		if !seen[want] {
			t.Errorf("List(TierBoth) dropped %q; got %v", want, seen)
		}
	}
}

// The same fixture read at the WISP tier: gc's own tier filter keeps the two
// ephemeral rows and drops the durable pair, which is only possible if both
// quadrants of the plane reached it in the first place.
func TestNativeDoltStoreListTierWispsKeepsBothWispQuadrants(t *testing.T) {
	issues := []*beadslib.Issue{
		{ID: "gc-task", Title: "durable task", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-task-wisp", Title: "task wisp", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2, Ephemeral: true},
		{ID: "gc-mail", Title: "durable mail", Status: beadslib.StatusOpen, IssueType: beadslib.IssueType("message"), Priority: 2},
		{ID: "gc-mail-wisp", Title: "mail wisp", Status: beadslib.StatusOpen, IssueType: beadslib.IssueType("message"), Priority: 2, Ephemeral: true},
	}
	storage := &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			out := make([]*beadslib.Issue, 0, len(issues))
			for _, issue := range issues {
				out = append(out, cloneNativeIssueForTest(issue))
			}
			return out, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierWisps})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := map[string]bool{}
	for _, bead := range got {
		seen[bead.ID] = true
	}
	if len(seen) != 2 || !seen["gc-task-wisp"] || !seen["gc-mail-wisp"] {
		t.Fatalf("List(TierWisps) = %v, want exactly the two ephemeral rows", seen)
	}
}

// A nil ListRequest.Limit is not "unlimited", it is workapi.DefaultListLimit —
// fifty rows. Every unbounded gc listing would silently truncate.
func TestNativeDoltStoreListAlwaysStatesTheLimitExplicitly(t *testing.T) {
	var captured issueops.ListRequest
	storage := &nativeDoltReaderSpy{
		list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			captured = req
			return issueops.IssuePage{}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.List(ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if captured.Limit == nil {
		t.Fatal("Limit = nil on an unbounded listing; the role reads that as the 50-row default")
	}
	if *captured.Limit != 0 {
		t.Fatalf("Limit = %d on an unbounded listing, want an explicit 0", *captured.Limit)
	}
}

// The status axis is gc's, applied by ApplyListQuery over the three-value
// projection mapBdStatus produces. A backing limit pushed under a narrower
// query would cut the page before that filter runs.
func TestNativeDoltStoreListPushesTheLimitOnlyWhenTheBackingSetIsTheQuerySet(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query ListQuery
		want  int
	}{
		{
			name:  "every status, both tiers, no type: the request IS the query",
			query: ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth, Sort: SortCreatedAsc, Limit: 7},
			want:  7,
		},
		{
			name:  "not-closed is a gc-side narrowing of an all-status request",
			query: ListQuery{AllowScan: true, TierMode: TierBoth, Sort: SortCreatedAsc, Limit: 7},
			want:  0,
		},
		{
			name:  "a type predicate is gc-side: the role validates types and routes infra ones to the wisp plane alone",
			query: ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth, Type: "task", Sort: SortCreatedAsc, Limit: 7},
			want:  0,
		},
		{
			name:  "the durable tier drops the wisp-plane rows the request admits",
			query: ListQuery{AllowScan: true, IncludeClosed: true, Sort: SortCreatedAsc, Limit: 7},
			want:  0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured issueops.ListRequest
			storage := &nativeDoltReaderSpy{
				list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
					captured = req
					return issueops.IssuePage{}, nil
				},
			}
			store := newNativeDoltStoreForTest(storage)
			if _, err := store.List(tc.query); err != nil {
				t.Fatalf("List: %v", err)
			}
			if captured.Limit == nil {
				t.Fatal("Limit = nil, want an explicit value")
			}
			if *captured.Limit != tc.want {
				t.Fatalf("pushed limit = %d, want %d", *captured.Limit, tc.want)
			}
		})
	}
}

// The detail read is the ONE door that publishes the concurrency token: a list
// row carries none, and an absent token must never be read as the real value 0.
func TestNativeDoltStoreGetCarriesTheDetailViewRevision(t *testing.T) {
	storage := &nativeDoltReaderSpy{
		get: func(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
			return &issueops.IssueDetails{
				Issue: beadslib.Issue{
					ID: req.ID, Title: "detail", Status: beadslib.StatusOpen,
					IssueType: beadslib.TypeTask, Priority: 2,
				},
				Labels:   []string{"native"},
				Revision: "42",
			}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Get("gc-detail")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Revision != 42 {
		t.Fatalf("Revision = %d, want 42 off the detail view", got.Revision)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "native" {
		t.Fatalf("Labels = %v, want the detail view's own label list", got.Labels)
	}
}

// The wire publishes the token as `revision` and leaves the embedded row's
// RowVersion at its zero value, so a store that read RowVersion would hand every
// guarded write a fabricated 0 that matches an un-mutated legacy row.
func TestNativeDoltStoreGetReadsRevisionNotTheEmbeddedRowVersion(t *testing.T) {
	storage := &nativeDoltReaderSpy{
		get: func(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
			return &issueops.IssueDetails{
				Issue: beadslib.Issue{
					ID: req.ID, Title: "wire row", Status: beadslib.StatusOpen,
					IssueType: beadslib.TypeTask, Priority: 2,
					RowVersion: 0, // json:"-": never crosses
				},
				Revision: "9",
			}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Get("gc-wire")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Revision != 9 {
		t.Fatalf("Revision = %d, want 9 — the wire's token, not the row's dropped RowVersion", got.Revision)
	}
}

// A list row publishes no token at all. ABSENT is not 0: a caller that acts on a
// row it listed must re-read the detail view for the token first.
func TestNativeDoltStoreListRowsCarryNoRevision(t *testing.T) {
	storage := &nativeDoltReaderSpy{
		list: func(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
			return issueops.IssuePage{Items: []*issueops.IssueWithCounts{{
				Issue: &beadslib.Issue{
					ID: "gc-listed", Title: "listed", Status: beadslib.StatusOpen,
					IssueType: beadslib.TypeTask, Priority: 2, RowVersion: 77,
				},
			}}}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List len = %d, want 1", len(got))
	}
	if got[0].Revision != 0 {
		t.Fatalf("Revision = %d off a list row, want 0 — the page publishes no token and a stamped one would be a fabricated fence", got[0].Revision)
	}
}

func TestNativeDoltStoreGetMapsTheRoleMissOntoErrNotFound(t *testing.T) {
	storage := &nativeDoltReaderSpy{
		get: func(context.Context, issueops.GetRequest) (*issueops.IssueDetails, error) {
			return nil, issueops.ErrNotFound
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Get("gc-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

// A backend failure must never decay into not-found: the role promises that,
// and a caller that reads a transport error as "the bead is gone" deletes work.
func TestNativeDoltStoreGetKeepsABackendFailureDistinctFromAMiss(t *testing.T) {
	wantErr := errors.New("dial tcp: connection refused")
	storage := &nativeDoltReaderSpy{
		get: func(context.Context, issueops.GetRequest) (*issueops.IssueDetails, error) {
			return nil, wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	_, err := store.Get("gc-unreachable")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Get error = %v, want the transport failure", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want it NOT classified as a miss", err)
	}
}

func TestNativeDoltStoreGetAsksForTheDependencyEdgesTheBeadModelNeeds(t *testing.T) {
	var captured issueops.GetRequest
	var edgeReads []issueops.EdgeReadRequest
	storage := &nativeDoltReaderSpy{
		get: func(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
			captured = req
			return &issueops.IssueDetails{Issue: beadslib.Issue{
				ID: req.ID, Title: "t", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
			}}, nil
		},
		edges: func(ctx context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
			edgeReads = append(edgeReads, req)
			return edgelessReader{}.ReadEdges(ctx, req)
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Get("gc-1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The two expensive lists stay off: Bead carries neither dependents nor
	// comments.
	if captured.IncludeDependents {
		t.Error("IncludeDependents = true; Bead has no dependents field and the row list is the expensive one")
	}
	if captured.IncludeComments {
		t.Error("IncludeComments = true; Bead has no comments field")
	}
	// The edges come from one edge read about this bead alone, of every type: a
	// type filter would hand a cache a partial edge set as the whole topology.
	if len(edgeReads) != 1 {
		t.Fatalf("edge reads = %+v, want exactly one", edgeReads)
	}
	if got := edgeReads[0]; len(got.IDs) != 1 || got.IDs[0] != "gc-1" || len(got.Types) != 0 {
		t.Fatalf("edge read = %+v, want IDs [gc-1] with no type filter", got)
	}
}

// The parent comes from the stored edges, not from the detail view's resolved
// dependency list: a molecule parent in another ledger has no entry there.
func TestNativeDoltStoreGetDerivesTheParentFromTheStoredEdges(t *testing.T) {
	storage := &nativeDoltReaderSpy{
		get: func(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
			return &issueops.IssueDetails{
				Issue: beadslib.Issue{ID: req.ID, Title: "child", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
				Dependencies: []*beadslib.IssueWithDependencyMetadata{
					{Issue: beadslib.Issue{ID: "gc-blocker"}, DependencyType: beadslib.DepBlocks},
				},
			}, nil
		},
		edges: func(_ context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
			return issueops.EdgeReadResult{Anchors: []issueops.AnchorEdges{{ID: req.IDs[0], Edges: []*beadslib.Dependency{
				{IssueID: req.IDs[0], DependsOnID: "gc-blocker", Type: beadslib.DepBlocks},
				{IssueID: req.IDs[0], DependsOnID: "gcg-mol", Type: beadslib.DepParentChild},
			}}}}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Get("gc-child")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ParentID != "gcg-mol" {
		t.Fatalf("ParentID = %q, want gcg-mol", got.ParentID)
	}
	if len(got.Dependencies) != 2 {
		t.Fatalf("Dependencies = %+v, want both edges", got.Dependencies)
	}
	for _, dep := range got.Dependencies {
		if dep.IssueID != "gc-child" {
			t.Fatalf("edge %+v: IssueID = %q, want the anchor", dep, dep.IssueID)
		}
	}
}

// A store that declares its rows complete owes every stored edge on a Get row,
// because a cache installs that set as the bead's whole topology. The detail
// view resolves far ends, so it drops an "external:" target and an id in
// another ledger; the edge read keeps both, and the row carries exactly what a
// DOWN walk reads.
func TestNativeDoltStoreGetCarriesTheEdgesTheDetailViewCannotResolve(t *testing.T) {
	stored := []*beadslib.Dependency{
		{IssueID: "gc-1", DependsOnID: "external:github/1", Type: beadslib.DepBlocks},
		{IssueID: "gc-1", DependsOnID: "gc-2", Type: beadslib.DepBlocks},
		{IssueID: "gc-1", DependsOnID: "gcg-9999", Type: beadslib.DepBlocks},
	}
	storage := &nativeDoltReaderSpy{
		get: func(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
			return &issueops.IssueDetails{
				Issue: beadslib.Issue{ID: req.ID, Title: "t", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
				Dependencies: []*beadslib.IssueWithDependencyMetadata{
					{Issue: beadslib.Issue{ID: "gc-2"}, DependencyType: beadslib.DepBlocks},
				},
			}, nil
		},
		edges: func(_ context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
			return issueops.EdgeReadResult{Anchors: []issueops.AnchorEdges{{ID: req.IDs[0], Edges: stored}}}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Get("gc-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []Dep{
		{IssueID: "gc-1", DependsOnID: "external:github/1", Type: "blocks"},
		{IssueID: "gc-1", DependsOnID: "gc-2", Type: "blocks"},
		{IssueID: "gc-1", DependsOnID: "gcg-9999", Type: "blocks"},
	}
	if !slices.Equal(got.Dependencies, want) {
		t.Fatalf("Get Dependencies = %+v, want every stored edge %+v", got.Dependencies, want)
	}
	down, err := store.DepList("gc-1", "down")
	if err != nil {
		t.Fatalf("DepList down: %v", err)
	}
	if !slices.Equal(got.Dependencies, down) {
		t.Fatalf("Get Dependencies = %+v, DepList down = %+v; a row must carry the edges a walk reads", got.Dependencies, down)
	}
}

// An edge-read failure is a failed Get, never a bead without edges: a cache
// would install the empty set as the bead's topology.
func TestNativeDoltStoreGetFailsWhenTheEdgeReadFails(t *testing.T) {
	wantErr := errors.New("dial tcp: connection refused")
	storage := &nativeDoltReaderSpy{
		get: func(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
			return &issueops.IssueDetails{Issue: beadslib.Issue{ID: req.ID, Title: "t", Status: beadslib.StatusOpen}}, nil
		},
		edges: func(context.Context, issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
			return issueops.EdgeReadResult{}, wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	_, err := store.Get("gc-1")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Get error = %v, want the edge-read failure", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want it NOT classified as a miss", err)
	}
}

// The detail view and the edge read are two transactions. A bead deleted
// between them comes back Missing from the edge read, and the later read is
// the answer: ErrNotFound, not a row with no edges.
func TestNativeDoltStoreGetReportsABeadDeletedBetweenItsReadsAsAMiss(t *testing.T) {
	storage := &nativeDoltReaderSpy{
		get: func(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
			return &issueops.IssueDetails{Issue: beadslib.Issue{ID: req.ID, Title: "t", Status: beadslib.StatusOpen}}, nil
		},
		edges: func(_ context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
			return issueops.EdgeReadResult{Anchors: []issueops.AnchorEdges{{ID: req.IDs[0], Edges: []*beadslib.Dependency{}, Missing: true}}}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Get("gc-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

// The role reports every anchor it was asked about, so an answer without this
// bead is a broken read rather than a bead with no edges.
func TestNativeDoltStoreGetRefusesAnEdgeAnswerThatOmitsTheBead(t *testing.T) {
	storage := &nativeDoltReaderSpy{
		get: func(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
			return &issueops.IssueDetails{Issue: beadslib.Issue{ID: req.ID, Title: "t", Status: beadslib.StatusOpen}}, nil
		},
		edges: func(context.Context, issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
			return issueops.EdgeReadResult{Anchors: []issueops.AnchorEdges{{ID: "gc-other", Edges: []*beadslib.Dependency{}}}}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	_, err := store.Get("gc-1")
	if err == nil {
		t.Fatal("Get = nil error, want the omitted anchor reported")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want it NOT classified as a miss", err)
	}
}

// G10. The ready wire publishes NO status: workapi.BuildReadyFilter pins
// StatusOpen for every caller, so the two-status loop this replaced cannot be
// asked for and must not be re-invented as a second call.
func TestNativeDoltStoreReadyAsksTheRoleExactlyOnce(t *testing.T) {
	calls := 0
	storage := &nativeDoltReaderSpy{
		ready: func(context.Context, issueops.ReadyRequest) (issueops.IssuePage, error) {
			calls++
			return issueops.IssuePage{}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Reader.Ready calls = %d, want exactly 1", calls)
	}
}

// IncludeDeferred is NOT set, and that is a decision rather than an omission:
// setting it drops the deferred-CHILD exclusion too (sqlbuild.BuildReadyWorkWhere
// consults ReadyWorkWhereInputs.DeferredChildIDs only when the flag is off), and
// gc has no Go-side filter that can put a deferred parent's children back.
func TestNativeDoltStoreReadyDoesNotAdmitFutureDeferrals(t *testing.T) {
	var captured issueops.ReadyRequest
	storage := &nativeDoltReaderSpy{
		ready: func(_ context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
			captured = req
			return issueops.IssuePage{}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if captured.IncludeDeferred {
		t.Fatal("IncludeDeferred = true; that admits future-deferred rows AND the children of deferred parents, which no gc-side filter removes")
	}
}

// The harm IncludeDeferred would do, behaviorally: the flag also drops the
// deferred-CHILD exclusion, and a child of a future-deferred parent carries no
// defer_until of its own, so gc's own IsDeferred check cannot put it back. The
// dispatcher would be offered a step whose parent is explicitly on ice.
func TestNativeDoltStoreReadyDoesNotOfferTheChildOfADeferredParent(t *testing.T) {
	future := time.Now().UTC().Add(24 * time.Hour)
	issues := []*beadslib.Issue{
		{ID: "gc-open", Title: "open", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-parent", Title: "deferred parent", Status: beadslib.StatusDeferred, IssueType: beadslib.TypeTask, Priority: 2, DeferUntil: &future},
		{ID: "gc-child", Title: "child of a deferred parent", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
	}
	storage := &nativeDoltStorageSpy{
		getReadyWork: func(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
			return readyWorkFixtureWithDeferredChildrenForTest(issues, []string{"gc-child"}, filter), nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-open" {
		t.Fatalf("Ready = %+v, want only gc-open — a deferred parent's child must stay hidden", got)
	}
}

func TestNativeDoltStoreReadyAlwaysStatesTheLimitExplicitly(t *testing.T) {
	var captured issueops.ReadyRequest
	storage := &nativeDoltReaderSpy{
		ready: func(_ context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
			captured = req
			return issueops.IssuePage{}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if captured.Limit == nil {
		t.Fatal("Limit = nil; the role reads that as the 100-row ready default")
	}
	if *captured.Limit != 0 {
		t.Fatalf("Limit = %d, want an explicit 0 — gc filters its own excluded types before limiting", *captured.Limit)
	}
}

// The assignee is one of the few predicates that pushes DOWN, so it has to
// reach the role rather than being re-filtered Go-side over a full ready page.
// The empty arm is the control: gc spells "anyone" as the zero value, and a
// blank assignee sent as a real predicate would select the rows nobody owns
// instead of all of them.
func TestNativeDoltStoreReadyPushesTheAssigneeDown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query []ReadyQuery
		want  string
	}{
		{name: "named assignee travels", query: []ReadyQuery{{Assignee: "probe-me"}}, want: "probe-me"},
		{name: "no assignee stays unset", query: nil, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured issueops.ReadyRequest
			storage := &nativeDoltReaderSpy{
				ready: func(_ context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
					captured = req
					return issueops.IssuePage{}, nil
				},
			}
			store := newNativeDoltStoreForTest(storage)

			if _, err := store.Ready(tc.query...); err != nil {
				t.Fatalf("Ready: %v", err)
			}
			if captured.Assignee != tc.want {
				t.Fatalf("Assignee = %q, want %q", captured.Assignee, tc.want)
			}
		})
	}
}

// The sort POLICY decides which rows a truncation keeps, so it is sent as the
// concrete value the raw filter resolved to rather than left for whichever
// default the route on the other end happens to apply.
func TestNativeDoltStoreReadyStatesTheSortPolicyItUsedToResolveTo(t *testing.T) {
	var captured issueops.ReadyRequest
	storage := &nativeDoltReaderSpy{
		ready: func(_ context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
			captured = req
			return issueops.IssuePage{}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if captured.Sort != "hybrid" {
		t.Fatalf("Sort = %q, want hybrid — the empty WorkFilter.SortPolicy this replaced resolved to it, and an absent policy is `priority` to the wire's handler", captured.Sort)
	}
}

// The tier is the plane, and the ready query's plane bit is IncludeEphemeral.
func TestNativeDoltStoreReadyCarriesTheTierAsThePlaneBit(t *testing.T) {
	for _, tc := range []struct {
		tier TierMode
		want bool
	}{
		{TierIssues, false},
		{TierWisps, true},
		{TierBoth, true},
	} {
		var captured issueops.ReadyRequest
		storage := &nativeDoltReaderSpy{
			ready: func(_ context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
				captured = req
				return issueops.IssuePage{}, nil
			},
		}
		store := newNativeDoltStoreForTest(storage)
		if _, err := store.Ready(ReadyQuery{TierMode: tc.tier}); err != nil {
			t.Fatalf("Ready: %v", err)
		}
		if captured.IncludeEphemeral != tc.want {
			t.Errorf("tier %v: IncludeEphemeral = %v, want %v", tc.tier, captured.IncludeEphemeral, tc.want)
		}
	}
}

// The plane bit, behaviorally: an ephemeral row is ready work at the wisp and
// both tiers and is absent from the durable one.
func TestNativeDoltStoreReadyWispTierOffersEphemeralWork(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	durable, err := store.Create(Bead{Title: "durable"})
	if err != nil {
		t.Fatalf("Create(durable): %v", err)
	}
	wisp, err := store.Create(Bead{Title: "wisp", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create(wisp): %v", err)
	}

	for _, tc := range []struct {
		tier TierMode
		want []string
	}{
		{TierIssues, []string{durable.ID}},
		{TierWisps, []string{wisp.ID}},
		{TierBoth, []string{durable.ID, wisp.ID}},
	} {
		got, err := store.Ready(ReadyQuery{TierMode: tc.tier})
		if err != nil {
			t.Fatalf("Ready(tier %v): %v", tc.tier, err)
		}
		seen := map[string]bool{}
		for _, bead := range got {
			seen[bead.ID] = true
		}
		if len(seen) != len(tc.want) {
			t.Fatalf("Ready(tier %v) = %v, want %v", tc.tier, seen, tc.want)
		}
		for _, want := range tc.want {
			if !seen[want] {
				t.Fatalf("Ready(tier %v) = %v, want %v", tc.tier, seen, tc.want)
			}
		}
	}
}

// The deferred-visibility semantics, stated as one differential across both
// doors.
//
// `bd defer <id>` without --until is a first-class, documented "status-based"
// INDEFINITE deferral (upstream cmd/bd/defer.go): status=deferred with
// defer_until left NULL. `bd defer <id> --until=<t>` is the time-bound snooze,
// and an EXPIRED one has to come back. The old door asked for status=deferred
// on a second pass and dropped the nil-DeferUntil rows itself; the role
// publishes no status and cannot be asked, and the backends' own lazy
// defer-wake sweep (issueops.WakeExpiredDefersInTx, run ahead of every ready
// read) flips exactly the expired ones to open first. THE MECHANISM MOVED AND
// THE ANSWER DID NOT, which is what this asserts — including the future-deferred
// row, which neither of the two tests this replaced covered.
func TestNativeDoltStoreReadyDeferredVisibilityIsUnchangedThroughTheRole(t *testing.T) {
	past := time.Now().UTC().Add(-24 * time.Hour)
	future := time.Now().UTC().Add(24 * time.Hour)
	issues := []*beadslib.Issue{
		{ID: "gc-open", Title: "open", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-indefinite", Title: "indefinite", Status: beadslib.StatusDeferred, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-expired", Title: "expired", Status: beadslib.StatusDeferred, IssueType: beadslib.TypeTask, Priority: 2, DeferUntil: &past},
		{ID: "gc-future", Title: "future", Status: beadslib.StatusDeferred, IssueType: beadslib.TypeTask, Priority: 2, DeferUntil: &future},
	}
	storage := &nativeDoltStorageSpy{
		getReadyWork: func(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
			return readyWorkFixtureForTest(issues, filter), nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	want := map[string]bool{"gc-open": true, "gc-expired": true}
	if len(got) != len(want) {
		t.Fatalf("Ready = %+v, want exactly %v", got, want)
	}
	for _, bead := range got {
		if !want[bead.ID] {
			t.Fatalf("Ready returned %q; an indefinite or future deferral must stay hidden", bead.ID)
		}
	}
}

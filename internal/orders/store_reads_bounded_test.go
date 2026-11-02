package orders

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestLastRunIsBoundedToNewestRun is the regression guard for the read the
// order-firing doctor check depends on (ga-klv). The check answers "did this
// order fire recently", which needs exactly one row; if this query ever loses
// its bound it pulls the order's whole retained run history on a city with tens
// of thousands of order-run beads and blows the check's 15s budget.
func TestLastRunIsBoundedToNewestRun(t *testing.T) {
	spy := &listSpyStore{Store: beads.NewMemStore()}
	store := NewStore(beads.OrdersStore{Store: spy})

	if _, err := store.LastRun("digest"); err != nil {
		t.Fatalf("LastRun(): %v", err)
	}
	if len(spy.queries) == 0 {
		t.Fatal("LastRun issued no list query")
	}
	for i, q := range spy.queries {
		if q.Limit != 1 {
			t.Fatalf("query %d: Limit = %d, want 1; an unbounded last-run read is the ga-klv doctor timeout", i, q.Limit)
		}
		if !q.AllowBackingCreatedLimit {
			t.Fatalf("query %d: AllowBackingCreatedLimit = false, want true so the bound reaches the backing instead of being cut client-side", i)
		}
		if q.Sort != beads.SortCreatedDesc {
			t.Fatalf("query %d: Sort = %v, want SortCreatedDesc so row one is the newest run", i, q.Sort)
		}
	}
}

// TestRecentRunsPushesLimitToBacking guards the `gc order history` read. A
// positive limit must reach the backing: cutting client-side still fetches and
// serializes the full retained corpus, which is what made the command take 22s
// interactively on a city with 11k+ order-run rows (ga-klv).
func TestRecentRunsPushesLimitToBacking(t *testing.T) {
	spy := &listSpyStore{Store: beads.NewMemStore()}
	store := NewStore(beads.OrdersStore{Store: spy})

	if _, err := store.RecentRuns("digest", 20); err != nil {
		t.Fatalf("RecentRuns(): %v", err)
	}
	if len(spy.queries) == 0 {
		t.Fatal("RecentRuns issued no list query")
	}
	for i, q := range spy.queries {
		if q.Limit != 20 {
			t.Fatalf("query %d: Limit = %d, want 20 (the caller's bound must be threaded through)", i, q.Limit)
		}
		if !q.AllowBackingCreatedLimit {
			t.Fatalf("query %d: AllowBackingCreatedLimit = false, want true; the bound must reach the backing", i)
		}
	}
}

// TestRecentRunsUnlimitedStaysUnlimited keeps the explicit opt-out working: a
// non-positive limit still means "every run", so `--limit 0` remains an honest
// escape hatch for operators who really do want the full history.
func TestRecentRunsUnlimitedStaysUnlimited(t *testing.T) {
	spy := &listSpyStore{Store: beads.NewMemStore()}
	store := NewStore(beads.OrdersStore{Store: spy})

	if _, err := store.RecentRuns("digest", 0); err != nil {
		t.Fatalf("RecentRuns(): %v", err)
	}
	if len(spy.queries) == 0 {
		t.Fatal("RecentRuns issued no list query")
	}
	for i, q := range spy.queries {
		if q.Limit != 0 {
			t.Fatalf("query %d: Limit = %d, want 0 (unlimited opt-out)", i, q.Limit)
		}
	}
}

// TestListTrackingPushesLimitToBacking guards the same class of read cost as
// TestRecentRunsPushesLimitToBacking: the /v0/orders/feed read must bound the
// backing scan instead of fetching every retained tracking bead and slicing
// client-side.
func TestListTrackingPushesLimitToBacking(t *testing.T) {
	spy := &listSpyStore{Store: beads.NewMemStore()}
	store := NewStore(beads.OrdersStore{Store: spy})

	if _, err := store.ListTracking(20); err != nil {
		t.Fatalf("ListTracking(): %v", err)
	}
	if len(spy.queries) == 0 {
		t.Fatal("ListTracking issued no list query")
	}
	for i, q := range spy.queries {
		if q.Limit != 20 {
			t.Fatalf("query %d: Limit = %d, want 20 (the caller's bound must be threaded through)", i, q.Limit)
		}
		if !q.IncludeClosed {
			t.Fatalf("query %d: IncludeClosed = false, want true; the feed lists completed runs too", i)
		}
		if !q.AllowBackingCreatedLimit {
			t.Fatalf("query %d: AllowBackingCreatedLimit = false, want true; the bound must reach the backing", i)
		}
	}
}

// TestListTrackingUnlimitedStaysUnlimited keeps the explicit opt-out working,
// matching RecentRuns's limit<=0-means-unlimited convention.
func TestListTrackingUnlimitedStaysUnlimited(t *testing.T) {
	spy := &listSpyStore{Store: beads.NewMemStore()}
	store := NewStore(beads.OrdersStore{Store: spy})

	if _, err := store.ListTracking(0); err != nil {
		t.Fatalf("ListTracking(): %v", err)
	}
	if len(spy.queries) == 0 {
		t.Fatal("ListTracking issued no list query")
	}
	for i, q := range spy.queries {
		if q.Limit != 0 {
			t.Fatalf("query %d: Limit = %d, want 0 (unlimited opt-out)", i, q.Limit)
		}
	}
}

// nativeTrackingLedger is a native Dolt storage whose reader answers a listing
// as bd's does for the requests the sweep readers send: a named status selects
// only the rows recorded with it, while no status (the native store always sends
// AllFlag) selects every status, closed included. It keeps the id of every row
// it serves, which is every row the native store decodes.
type nativeTrackingLedger struct {
	beadslib.Storage
	rows   []*beadslib.Issue
	served []string
}

func (l *nativeTrackingLedger) IssueReader() (issueops.Reader, error) { return l, nil }

func (l *nativeTrackingLedger) List(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
	page := issueops.IssuePage{Items: []*issueops.IssueWithCounts{}}
	for _, row := range l.rows {
		if req.Status != "" && string(row.Status) != req.Status {
			continue
		}
		if len(req.Labels) > 0 && !slices.Contains(row.Labels, req.Labels[0]) {
			continue
		}
		issue := *row
		page.Items = append(page.Items, &issueops.IssueWithCounts{Issue: &issue})
		l.served = append(l.served, row.ID)
	}
	return page, nil
}

func (l *nativeTrackingLedger) Ready(context.Context, issueops.ReadyRequest) (issueops.IssuePage, error) {
	return issueops.IssuePage{}, errors.New("nativeTrackingLedger serves listings only")
}

func (l *nativeTrackingLedger) Get(context.Context, issueops.GetRequest) (*issueops.IssueDetails, error) {
	return nil, errors.New("nativeTrackingLedger serves listings only")
}

// servedStatus reports whether the ledger served a row recorded with status.
func (l *nativeTrackingLedger) servedStatus(status beadslib.Status) bool {
	for _, row := range l.rows {
		if row.Status == status && slices.Contains(l.served, row.ID) {
			return true
		}
	}
	return false
}

func nativeTrackingRow(id string, status beadslib.Status, created time.Time) *beadslib.Issue {
	return &beadslib.Issue{
		ID:        id,
		Title:     "order:digest",
		Status:    status,
		IssueType: "task",
		Labels:    []string{"order-run:digest", "order-tracking"},
		CreatedAt: created,
		NoHistory: true,
	}
}

// TestStaleOpenRunsDecodesNoClosedRowOnNativeDolt guards the stale watchdog's
// read, which runs every 30 s against every swept store. A native Dolt listing
// cannot exclude a status, so a query that does not name one hands the store
// every retained closed tracking bead (a week of runs by default) to decode and
// then drop in Go: the per-tick full-history hydrate sr-dp9o removed from the
// dispatch index. Each of the sweep's queries names its status, so the reader
// serves no closed row.
func TestStaleOpenRunsDecodesNoClosedRowOnNativeDolt(t *testing.T) {
	created := time.Now().Add(-time.Hour)
	ledger := &nativeTrackingLedger{rows: []*beadslib.Issue{
		nativeTrackingRow("gc-done-1", beadslib.StatusClosed, created),
		nativeTrackingRow("gc-claimed", beadslib.StatusInProgress, created.Add(2*time.Minute)),
		nativeTrackingRow("gc-done-2", beadslib.StatusClosed, created.Add(time.Minute)),
		nativeTrackingRow("gc-open", beadslib.StatusOpen, created.Add(time.Minute)),
	}}
	store := beads.NewNativeDoltStoreOverStorageForTest(ledger)

	runs, err := ordersStoreOver(store).StaleOpenRuns(time.Now())
	if err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	if got, want := runIDs(runs), []string{"gc-open", "gc-claimed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("StaleOpenRuns() IDs = %v, want the open and in_progress beads oldest-first %v", got, want)
	}
	if ledger.servedStatus(beadslib.StatusClosed) {
		t.Fatalf("StaleOpenRuns() had the native store decode closed rows (served %v); every query must name the status it reads", ledger.served)
	}

	// The control: a query that names no status is served the closed rows, so
	// the ledger above would have caught one.
	ledger.served = nil
	if _, err := store.List(beads.ListQuery{Label: "order-tracking", Live: true, TierMode: beads.TierBoth}); err != nil {
		t.Fatalf("List(not closed): %v", err)
	}
	if !ledger.servedStatus(beadslib.StatusClosed) {
		t.Fatalf("a not-closed listing was served %v, want the closed rows too; the ledger no longer models the native request", ledger.served)
	}
}

// TestNotClosedSweepReadsListAHandParkedTrackingBeadOnNativeDolt pins which
// reads see a tracking bead parked by hand (blocked, deferred, ...). No Gas City
// writer parks one, but it still holds the strict open-work gate, which counts
// every not-closed tracking bead, and native and bd rows read it as "open". A
// parked status is in neither of the watchdog read's exact queries, so the
// reads that ask for every not-closed status must list it: the on-demand
// sweep's, which releases the order once the bead is stale, and the boot orphan
// sweep's.
func TestNotClosedSweepReadsListAHandParkedTrackingBeadOnNativeDolt(t *testing.T) {
	created := time.Now().Add(-time.Hour)
	ledger := &nativeTrackingLedger{rows: []*beadslib.Issue{
		nativeTrackingRow("gc-open", beadslib.StatusOpen, created),
		nativeTrackingRow("gc-parked", beadslib.StatusBlocked, created.Add(time.Minute)),
	}}
	st := ordersStoreOver(beads.NewNativeDoltStoreOverStorageForTest(ledger))

	stale, err := st.StaleOpenRuns(time.Now())
	if err != nil {
		t.Fatalf("StaleOpenRuns(): %v", err)
	}
	if got, want := runIDs(stale), []string{"gc-open"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("StaleOpenRuns() IDs = %v, want %v (a parked status is in neither exact query)", got, want)
	}
	notClosed, err := st.StaleNotClosedRuns(time.Now())
	if err != nil {
		t.Fatalf("StaleNotClosedRuns(): %v", err)
	}
	if got, want := runIDs(notClosed), []string{"gc-open", "gc-parked"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("StaleNotClosedRuns() IDs = %v, want %v: the on-demand sweep's read must list every not-closed status the gate counts, a parked one included", got, want)
	}
	orphaned, err := st.OrphanedOpenRuns()
	if err != nil {
		t.Fatalf("OrphanedOpenRuns(): %v", err)
	}
	if got, want := runIDs(orphaned), []string{"gc-open", "gc-parked"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OrphanedOpenRuns() IDs = %v, want %v: the boot sweep's read must list every not-closed status, a parked one included", got, want)
	}
}

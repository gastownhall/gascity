package beads

import (
	"context"
	"errors"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// countRefusalFixture returns a small mixed dataset (closed/open, two types,
// two assignees, overlapping labels) used to drive both CountIssues (via the
// spy's countIssues hook) and List (via searchIssues, which the spy's
// IssueReader adapter uses to answer List). The same data backs every case
// below so Count's classification and List's cardinality are checked against
// one real fixture, not two mocks tuned to agree.
func countRefusalFixture() []*beadslib.Issue {
	raw := []struct {
		id       string
		status   beadslib.Status
		typ      string
		assignee string
		labels   []string
	}{
		{id: "gc-1", status: beadslib.StatusClosed, typ: "task", assignee: "gascity/builder", labels: []string{"sweep"}},
		{id: "gc-2", status: beadslib.StatusClosed, typ: "task", assignee: "gascity/builder", labels: []string{"sweep", "extra"}},
		{id: "gc-3", status: beadslib.StatusClosed, typ: "task", assignee: "gascity/other", labels: []string{"sweep"}},
		{id: "gc-4", status: beadslib.StatusClosed, typ: "bug", assignee: "gascity/builder", labels: []string{"sweep"}},
		{id: "gc-5", status: beadslib.StatusOpen, typ: "task", assignee: "gascity/builder", labels: []string{"sweep"}},
	}
	issues := make([]*beadslib.Issue, 0, len(raw))
	for _, r := range raw {
		issues = append(issues, &beadslib.Issue{
			ID: r.id, Title: r.id, Status: r.status, IssueType: beadslib.IssueType(r.typ),
			Assignee: r.assignee, Labels: append([]string(nil), r.labels...), Priority: 2,
		})
	}
	return issues
}

func countRefusalStorage(fixture []*beadslib.Issue) *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			out := make([]*beadslib.Issue, 0, len(fixture))
			for _, issue := range fixture {
				out = append(out, cloneNativeIssueForTest(issue))
			}
			return out, nil
		},
		countIssues: func(context.Context, string, beadslib.IssueFilter) (int64, error) {
			return 0, &beadslib.ErrUnsupported{Op: "CountIssues", Backend: "http"}
		},
	}
}

// TestNativeDoltStoreCountReportsErrCountUnsupportedOnRefusal is a
// real-serve-shaped regression test for the review finding against the
// now-removed Counter-role fast path: on a storage whose raw CountIssues
// refuses with *beadslib.ErrUnsupported (as the http client's does, confirmed
// against a real bd-serve), Count must ALWAYS classify the refusal as
// ErrCountUnsupported — never a raw, unclassified *beadslib.ErrUnsupported, and
// never a wrong number from a server-side substitute — for every shape
// nativeDoltCountSupported approves. This includes the two shapes the real
// server proved the removed Counter-role mapping got wrong: a plain TierIssues
// count (which silently undercounted no-history rows through
// CountRequest.IncludeInfra=false) and Status:"all" (which overcounted). Every
// existing hydrating-List fallback (store_health's countBeadStoreRows, the
// status handler) keys off errors.Is(err, ErrCountUnsupported) and treats any
// other error as a hard failure, so this is the mutation check too: a
// regression that leaks the raw *beadslib.ErrUnsupported, or that resurrects a
// server-side substitute returning a wrong count instead of erroring, fails
// this test.
func TestNativeDoltStoreCountReportsErrCountUnsupportedOnRefusal(t *testing.T) {
	fixture := countRefusalFixture()
	tests := []struct {
		name  string
		query ListQuery
	}{
		{name: "plain TierIssues closed count", query: ListQuery{AllowScan: true, IncludeClosed: true, Status: "closed"}},
		{name: "TierBoth (the policy-wrapped-city shape)", query: ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth}},
		{name: `status "all"`, query: ListQuery{AllowScan: true, IncludeClosed: true, Status: "all"}},
		{name: "type+assignee+label filter", query: ListQuery{AllowScan: true, IncludeClosed: true, Status: "closed", Type: "task", Assignee: "gascity/builder", Label: "sweep"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newNativeDoltStoreForTest(countRefusalStorage(fixture))
			_, err := store.Count(context.Background(), tt.query)
			if err == nil {
				t.Fatal("Count: want an error when the raw backend refuses, got nil")
			}
			if !errors.Is(err, ErrCountUnsupported) {
				t.Fatalf("Count err = %v, want it to wrap ErrCountUnsupported so callers' hydrating List fallback engages", err)
			}
		})
	}
}

// TestNativeDoltStoreCountRefusalFallsBackToListWithEqualCardinality proves
// the end-to-end contract the review asked for: once Count reports
// ErrCountUnsupported, a caller that falls back to List (exactly what
// countBeadStoreRows and the status handler do) gets the List-cardinality
// answer with no undercounting or overcounting — the property the removed
// Counter-role fast path violated against a real server.
func TestNativeDoltStoreCountRefusalFallsBackToListWithEqualCardinality(t *testing.T) {
	fixture := countRefusalFixture()
	tests := []struct {
		name  string
		query ListQuery
		want  int
	}{
		{name: "closed+task+builder+sweep", query: ListQuery{AllowScan: true, IncludeClosed: true, Status: "closed", Type: "task", Assignee: "gascity/builder", Label: "sweep"}, want: 2},
		{name: "all closed", query: ListQuery{AllowScan: true, IncludeClosed: true, Status: "closed"}, want: 4},
		// ListQuery.Matches treats Status:"all" as a literal exact-status
		// match — no bead's Status is literally "all" — so List correctly
		// answers 0 here. This is the exact shape the real-serve review
		// found the removed Counter-role mapping got wrong in the other
		// direction: it special-cased "all" as "no status filter" and
		// returned a nonzero count (4) the real server's List semantics
		// never agreed with (List gave 0). Reporting ErrCountUnsupported
		// unconditionally means the caller now gets List's own answer,
		// whatever it is, instead of a role's disagreeing guess.
		{name: `TierBoth, status "all" is a literal (no) match`, query: ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth, Status: "all"}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newNativeDoltStoreForTest(countRefusalStorage(fixture))
			_, err := store.Count(context.Background(), tt.query)
			if !errors.Is(err, ErrCountUnsupported) {
				t.Fatalf("Count err = %v, want ErrCountUnsupported so the caller falls back to List", err)
			}
			list, err := store.List(tt.query)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != tt.want {
				t.Fatalf("List fallback returned %d rows, want %d", len(list), tt.want)
			}
		})
	}
}

// TestNativeDoltStoreCountPropagatesNonRefusalErrorUnchanged pins that a
// REAL backend failure (not a refusal) is unaffected by the refusal
// classification: it must reach the caller as-is, not decay into
// ErrCountUnsupported (which would incorrectly tell the caller List can
// answer instead, when in fact the backend is simply failing).
func TestNativeDoltStoreCountPropagatesNonRefusalErrorUnchanged(t *testing.T) {
	wantErr := errors.New("backend is on fire")
	storage := &nativeDoltStorageSpy{
		countIssues: func(context.Context, string, beadslib.IssueFilter) (int64, error) {
			return 0, wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	_, err := store.Count(context.Background(), ListQuery{AllowScan: true, IncludeClosed: true, Status: "closed"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Count err = %v, want it to wrap %v unchanged", err, wantErr)
	}
	if errors.Is(err, ErrCountUnsupported) {
		t.Fatalf("Count err = %v, must NOT classify a real backend error as ErrCountUnsupported", err)
	}
}

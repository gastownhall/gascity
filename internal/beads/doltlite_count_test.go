//go:build gascity_native_beads

package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newDoltliteStoreWithIssues builds a DoltLite read store seeded with the
// given issues, reusing the schema and insert helpers from the read-store
// suite.
func newDoltliteStoreWithIssues(t testing.TB, issues []testDoltliteIssue) *DoltliteReadStore {
	t.Helper()
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir beads dir: %v", err)
	}
	meta := []byte(`{"backend":"doltlite","database":"doltlite","dolt_database":"hq"}`)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), meta, 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	dbDir := filepath.Join(beadsDir, "doltlite")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir doltlite dir: %v", err)
	}
	dbPath := filepath.Join(dbDir, "hq.db")
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=10000")
	if err != nil {
		t.Fatalf("open doltlite fixture db: %v", err)
	}
	defer db.Close() //nolint:errcheck // test cleanup
	createTestDoltliteSchema(t, db)
	for _, issue := range issues {
		insertTestDoltliteIssue(t, db, "issues", "labels", "dependencies", issue)
	}
	backing := NewBdStore(dir, func(string, string, ...string) ([]byte, error) {
		t.Fatal("backing bd runner should not be called by doltlite count tests")
		return nil, nil
	})
	store, err := NewDoltliteReadStore(dir, backing)
	if err != nil {
		t.Fatalf("NewDoltliteReadStore: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

// TestDoltliteCountMatchesList asserts the hydration-free Count returns exactly
// what len(List) would for every supported query shape — the Counter contract.
func TestDoltliteCountMatchesList(t *testing.T) {
	store, cleanup := newTestDoltliteReadStore(t)
	defer cleanup()

	cases := []struct {
		name    string
		query   ListQuery
		exclude []string
	}{
		{name: "open scan", query: ListQuery{AllowScan: true}},
		{name: "all include closed", query: ListQuery{AllowScan: true, IncludeClosed: true}},
		{name: "by type task", query: ListQuery{Type: "task"}},
		{name: "by type task include closed", query: ListQuery{Type: "task", IncludeClosed: true}},
		{name: "status open", query: ListQuery{Status: "open"}},
		{name: "status closed", query: ListQuery{Status: "closed"}},
		{name: "status in_progress", query: ListQuery{Status: "in_progress"}},
		{name: "by assignee", query: ListQuery{Assignee: "rig/worker"}},
		{name: "by label", query: ListQuery{Label: "tier-test"}},
		{name: "exclude session", query: ListQuery{AllowScan: true, IncludeClosed: true}, exclude: []string{"session"}},
		{name: "exclude session+task", query: ListQuery{AllowScan: true, IncludeClosed: true}, exclude: []string{"session", "task"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			list, err := store.List(tc.query)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			want := 0
			for _, b := range list {
				if !containsTestString(tc.exclude, b.Type) {
					want++
				}
			}
			got, err := store.Count(context.Background(), tc.query, tc.exclude...)
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if got != want {
				t.Fatalf("Count = %d, want len(List filtered) = %d", got, want)
			}
		})
	}
}

// TestDoltliteCountUnsupportedShapes asserts Count signals ErrCountUnsupported
// (so callers fall back to List) for query shapes it cannot answer exactly.
func TestDoltliteCountUnsupportedShapes(t *testing.T) {
	store, cleanup := newTestDoltliteReadStore(t)
	defer cleanup()

	cases := []struct {
		name  string
		query ListQuery
	}{
		{name: "metadata filter", query: ListQuery{Metadata: map[string]string{"gc.routed_to": "rig/polecat"}}},
		{name: "parent filter", query: ListQuery{ParentID: "gc-parent"}},
		{name: "created before", query: ListQuery{AllowScan: true, CreatedBefore: time.Now().Add(time.Hour)}},
		{name: "tier both", query: ListQuery{Label: "tier-test", TierMode: TierBoth}},
		{name: "tier wisps", query: ListQuery{Label: "tier-test", TierMode: TierWisps}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Count(context.Background(), tc.query)
			if !errors.Is(err, ErrCountUnsupported) {
				t.Fatalf("Count err = %v, want ErrCountUnsupported", err)
			}
		})
	}
}

// TestDoltliteBoundedListIsCreatedDescPrefix proves the bounded molecule read
// returns exactly the created_at-desc prefix the full scan would — the
// correctness guard for gascity#3253 (no dropped/duplicated roots).
func TestDoltliteBoundedListIsCreatedDescPrefix(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	var issues []testDoltliteIssue
	for i := 0; i < 40; i++ {
		status := "closed"
		if i%5 == 0 {
			status = "open" // mix open and closed roots
		}
		issues = append(issues, testDoltliteIssue{
			ID:        fmt.Sprintf("gc-mol-%03d", i),
			Title:     fmt.Sprintf("molecule %d", i),
			Status:    status,
			IssueType: "molecule",
			// Distinct, increasing created times so the desc order is total.
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	store := newDoltliteStoreWithIssues(t, issues)

	fullQuery := ListQuery{Type: "molecule", IncludeClosed: true, Sort: SortCreatedDesc}
	full, err := store.List(fullQuery)
	if err != nil {
		t.Fatalf("full List: %v", err)
	}
	if len(full) != len(issues) {
		t.Fatalf("full List returned %d, want %d", len(full), len(issues))
	}

	for _, k := range []int{1, 5, 10, 39, 40, 100} {
		bounded := fullQuery
		bounded.Limit = k
		got, err := store.List(bounded)
		if err != nil {
			t.Fatalf("bounded List(limit=%d): %v", k, err)
		}
		want := full
		if k < len(full) {
			want = full[:k]
		}
		if len(got) != len(want) {
			t.Fatalf("bounded List(limit=%d) len = %d, want %d", k, len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i].ID {
				t.Fatalf("bounded List(limit=%d)[%d] = %s, want %s (prefix mismatch)", k, i, got[i].ID, want[i].ID)
			}
		}
	}

	// Count over the same filter equals the full length: an accurate Total
	// without materializing the rows.
	n, err := store.Count(context.Background(), ListQuery{Type: "molecule", IncludeClosed: true})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != len(issues) {
		t.Fatalf("Count = %d, want %d", n, len(issues))
	}
}

func containsTestString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// BenchmarkDoltliteMoleculeRead compares the full-history molecule read against
// the bounded read + hydration-free Count that backs the gascity#3253 fix. Run:
//
//	go test -tags gascity_native_beads -run '^$' \
//	  -bench BenchmarkDoltliteMoleculeRead -benchmem ./internal/beads
func BenchmarkDoltliteMoleculeRead(b *testing.B) {
	const total = 5000
	const pageLimit = 500
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	issues := make([]testDoltliteIssue, 0, total)
	for i := 0; i < total; i++ {
		status := "closed"
		if i >= total-50 {
			status = "open"
		}
		issues = append(issues, testDoltliteIssue{
			ID:        fmt.Sprintf("gc-mol-%05d", i),
			Title:     fmt.Sprintf("molecule run %d", i),
			Status:    status,
			IssueType: "molecule",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
			Labels:    []string{"order-tracking", fmt.Sprintf("run:%d", i)},
			Metadata:  map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
		})
	}
	store := newDoltliteStoreWithIssues(b, issues)

	ctx := context.Background()

	b.Run("full_history_list", func(b *testing.B) {
		q := ListQuery{Type: "molecule", IncludeClosed: true, Sort: SortCreatedDesc}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rows, err := store.List(q)
			if err != nil {
				b.Fatalf("List: %v", err)
			}
			if len(rows) != total {
				b.Fatalf("List len = %d, want %d", len(rows), total)
			}
		}
	})

	b.Run("bounded_list_plus_count", func(b *testing.B) {
		q := ListQuery{Type: "molecule", IncludeClosed: true, Sort: SortCreatedDesc, Limit: pageLimit}
		cq := ListQuery{Type: "molecule", IncludeClosed: true}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rows, err := store.List(q)
			if err != nil {
				b.Fatalf("List: %v", err)
			}
			if len(rows) != pageLimit {
				b.Fatalf("bounded List len = %d, want %d", len(rows), pageLimit)
			}
			n, err := store.Count(ctx, cq)
			if err != nil {
				b.Fatalf("Count: %v", err)
			}
			if n != total {
				b.Fatalf("Count = %d, want %d", n, total)
			}
		}
	})
}

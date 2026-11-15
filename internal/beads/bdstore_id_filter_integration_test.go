//go:build integration

package beads_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// BdStore.List pushes ListQuery.IDs to bd as --id, and ListQuery.Matches stays
// the exact filter. This runs the real bd: for each query shape, the rows List
// returns with IDs must be the rows of the same query read without IDs and cut
// by Matches, so --id only ever narrows what bd sends and never changes the
// answer. The reference read carries no --id, so it is the Go-side filter the
// pushdown replaced.
func TestBdStoreListIDsMatchesGoSideFilterAgainstRealBd(t *testing.T) {
	seed, dir := newConditionalIntegrationBdStore(t)
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("tst-id-%d", i)
		if _, err := seed.Create(beads.Bead{ID: id, Title: id, Type: "task"}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	if err := seed.Close("tst-id-6"); err != nil {
		t.Fatalf("Close tst-id-6: %v", err)
	}

	realBd := newConditionalIntegrationRunner(t, dir)
	var listCalls []string
	store := beads.NewBdStore(dir, func(d, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list" {
			listCalls = append(listCalls, strings.Join(args, " "))
		}
		return realBd(d, name, args...)
	})

	// 700 ids cross the chunk size: two live ids sit in different chunks among
	// ids no bead owns.
	overChunk := make([]string, 700)
	for i := range overChunk {
		overChunk[i] = fmt.Sprintf("tst-id-none-%d", i)
	}
	overChunk[3], overChunk[650] = "tst-id-2", "tst-id-5"

	cases := []struct {
		name      string
		query     beads.ListQuery
		wantCalls int
	}{
		{name: "hit", query: beads.ListQuery{IDs: []string{"tst-id-1"}}, wantCalls: 1},
		{name: "miss", query: beads.ListQuery{IDs: []string{"tst-id-404"}}, wantCalls: 1},
		{name: "mixed hit and miss", query: beads.ListQuery{IDs: []string{"tst-id-404", "tst-id-3", "tst-id-1"}}, wantCalls: 1},
		{name: "closed id dropped by default", query: beads.ListQuery{IDs: []string{"tst-id-1", "tst-id-6"}}, wantCalls: 1},
		{name: "closed id kept by IncludeClosed", query: beads.ListQuery{IDs: []string{"tst-id-1", "tst-id-6"}, IncludeClosed: true}, wantCalls: 1},
		{name: "closed id with Status open", query: beads.ListQuery{IDs: []string{"tst-id-1", "tst-id-6"}, IncludeClosed: true, Status: "open"}, wantCalls: 1},
		{name: "closed id with Status closed", query: beads.ListQuery{IDs: []string{"tst-id-1", "tst-id-6"}, Status: "closed"}, wantCalls: 1},
		{name: "duplicates and blanks", query: beads.ListQuery{IDs: []string{"tst-id-2", "", "tst-id-2", "  ", "tst-id-4"}}, wantCalls: 1},
		{name: "blank only", query: beads.ListQuery{IDs: []string{"", "  "}}, wantCalls: 0},
		{name: "over chunk size", query: beads.ListQuery{IDs: overChunk}, wantCalls: 2},
		{name: "over chunk size with Limit", query: beads.ListQuery{IDs: overChunk, Limit: 1}, wantCalls: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The reference is the same query with no IDs, cut by Matches.
			noIDs := tc.query
			noIDs.IDs, noIDs.Limit, noIDs.AllowScan = nil, 0, true
			all, err := store.List(noIDs)
			if err != nil {
				t.Fatalf("reference List: %v", err)
			}
			var want []string
			for _, b := range all {
				if tc.query.Matches(b) {
					want = append(want, b.ID)
				}
			}
			if tc.query.Limit > 0 && len(want) > tc.query.Limit {
				want = want[:tc.query.Limit]
			}

			listCalls = nil
			got, err := store.List(tc.query)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if n := countIDListCalls(listCalls); n != tc.wantCalls {
				t.Fatalf("bd list --id calls = %d, want %d: %q", n, tc.wantCalls, listCalls)
			}
			ids := sortedIDs(got)
			slices.Sort(want)
			if !slices.Equal(ids, want) {
				t.Fatalf("List(IDs) = %v, Go-side Matches over the same query = %v", ids, want)
			}
		})
	}
}

// countIDListCalls counts the bd list reads that carried --id.
func countIDListCalls(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, " --id ") {
			n++
		}
	}
	return n
}

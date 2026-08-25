//go:build gascity_native_beads

package beads

// The DoltLite half of the namespace census, held to the same bar the
// hydration-free Count is: whatever the predicate says, listing the store and
// applying the rule by hand must say the same thing.

import (
	"strings"
	"testing"
)

func doltliteResidentOutsideByScan(t *testing.T, store *DoltliteReadStore, prefixes []string) bool {
	t.Helper()
	rows, err := store.List(ListQuery{TierMode: FederatedReadTier, AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, row := range rows {
		inside := false
		for _, prefix := range prefixes {
			prefix = strings.TrimSpace(prefix)
			if prefix == "" {
				continue
			}
			if row.ID == prefix || strings.HasPrefix(row.ID, prefix+"-") {
				inside = true
				break
			}
		}
		if !inside {
			return true
		}
	}
	return false
}

func TestDoltliteHasResidentOutside(t *testing.T) {
	classPrefixes := []string{"gcg", "gcnq"}

	for _, tc := range []struct {
		name     string
		issues   []testDoltliteIssue
		wisps    []testDoltliteIssue
		prefixes []string
		want     bool
		why      string
	}{
		{
			name:     "a work-shaped id the migration carried across is a resident outside",
			issues:   []testDoltliteIssue{{ID: "gcg-1", Title: "own"}, {ID: "ga-relic", Title: "carried across"}},
			prefixes: classPrefixes,
			want:     true,
			why:      "ga-relic carries no namespace this binding declares",
		},
		{
			name:     "every declared namespace is inside",
			issues:   []testDoltliteIssue{{ID: "gcg-1", Title: "own"}, {ID: "gcnq-2", Title: "own"}},
			prefixes: classPrefixes,
			want:     false,
			why:      "a binding holding only its own ids is the converged city the probe retires for",
		},
		{
			// The load-bearing row (ga-qdt5y.19): a closed relic is still read,
			// reopened and claimed by id.
			name:     "a closed relic is still a resident",
			issues:   []testDoltliteIssue{{ID: "gcg-1", Title: "own"}, {ID: "ga-done", Title: "drained", Status: "closed"}},
			prefixes: classPrefixes,
			want:     true,
			why:      "closing a relic drains it; it does not make it findable by prefix",
		},
		{
			name:     "a closed bead inside a declared namespace is ordinary retired infrastructure",
			issues:   []testDoltliteIssue{{ID: "gcg-1", Title: "own", Status: "closed"}},
			prefixes: classPrefixes,
			want:     false,
			why:      "it is findable by prefix whether it is open or closed",
		},
		{
			name:     "an empty binding holds nothing outside",
			prefixes: classPrefixes,
			want:     false,
			why:      "nothing is what a fresh born-split city holds",
		},
		{
			// The wisps table is a second place a carried-across row can sit, and
			// a predicate that read one table would miss it.
			name:     "a relic in the wisps table counts",
			issues:   []testDoltliteIssue{{ID: "gcg-1", Title: "own"}},
			wisps:    []testDoltliteIssue{{ID: "ga-wisp", Title: "carried across", Ephemeral: true}},
			prefixes: classPrefixes,
			want:     true,
			why:      "a wisp carried across is as unfindable as an issue",
		},
		{
			name:     "a durable no-history wisp relic counts",
			wisps:    []testDoltliteIssue{{ID: "ga-durable", Title: "carried across", NoHistory: true}},
			prefixes: classPrefixes,
			want:     true,
			why:      "the storage flag decides the tier, not whether the row exists",
		},
		{
			name:     "a lookalike prefix is not the namespace",
			issues:   []testDoltliteIssue{{ID: "gcgx-1", Title: "neighbour"}},
			prefixes: classPrefixes,
			want:     true,
			why:      "gcgx is its own namespace; only gcg and gcg-... are inside gcg",
		},
		{
			name:     "the bare prefix is inside its own namespace",
			issues:   []testDoltliteIssue{{ID: "gcg", Title: "own"}},
			prefixes: classPrefixes,
			want:     false,
			why:      "IDInNamespace admits the bare prefix, and the predicate must agree",
		},
		{
			name:     "a binding claiming no namespace counts every resident",
			issues:   []testDoltliteIssue{{ID: "gcg-1", Title: "own"}},
			prefixes: nil,
			want:     true,
			why:      "nothing is declared, so nothing is inside",
		},
		{
			name:     "a blank prefix claims nothing",
			issues:   []testDoltliteIssue{{ID: "gcg-1", Title: "own"}},
			prefixes: []string{"  "},
			want:     true,
			why:      "an empty namespace is not a wildcard",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newDoltliteStoreWithRows(t, tc.issues, tc.wisps)

			got, err := store.HasResidentOutside(tc.prefixes)
			if err != nil {
				t.Fatalf("HasResidentOutside: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasResidentOutside(%v) = %v, want %v: %s", tc.prefixes, got, tc.want, tc.why)
			}
			if scanned := doltliteResidentOutsideByScan(t, store, tc.prefixes); scanned != got {
				t.Fatalf("the predicate answered %v and listing the store answered %v; one of the two is narrowing the census", got, scanned)
			}
		})
	}
}

// The capability is what a caller type-asserts for, so the assertion has to land.
func TestDoltliteReadStoreIsANamespaceCensus(t *testing.T) {
	var store Store = newDoltliteStoreWithRows(t, nil, nil)
	if _, ok := store.(NamespaceCensus); !ok {
		t.Fatalf("%T does not satisfy beads.NamespaceCensus", store)
	}
}

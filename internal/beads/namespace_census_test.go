package beads

// The namespace census predicate, and the one row that decides whether it is
// sound.
//
// HasResidentOutside answers a verdict the caller uses to retire a by-id probe,
// so an answer of FALSE strands every bead it failed to see. The rows below are
// mostly about what "see" has to mean: every tier, every status, and a namespace
// rule that matches storeref.IDInNamespace byte for byte rather than
// approximately.

import (
	"testing"
)

func seedCensusRow(t *testing.T, store *SQLiteStore, b Bead) {
	t.Helper()
	if b.Title == "" {
		b.Title = b.ID
	}
	if b.Type == "" {
		b.Type = "task"
	}
	if _, err := store.Create(b); err != nil {
		t.Fatalf("seeding %q: %v", b.ID, err)
	}
}

func openCensusStore(t *testing.T) *SQLiteStore {
	t.Helper()
	opened, err := OpenSQLiteStore(t.TempDir(), WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store, ok := opened.(*SQLiteStore)
	if !ok {
		t.Fatalf("OpenSQLiteStore returned %T, want *SQLiteStore", opened)
	}
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

func TestSQLiteHasResidentOutside(t *testing.T) {
	classPrefixes := []string{"gcg", "gcnq"}

	for _, tc := range []struct {
		name     string
		seed     []Bead
		close    []string
		prefixes []string
		want     bool
		why      string
	}{
		{
			name:     "a work-shaped id the migration carried across is a resident outside",
			seed:     []Bead{{ID: "gcg-1"}, {ID: "ga-relic"}},
			prefixes: classPrefixes,
			want:     true,
			why:      "ga-relic carries no namespace this binding declares, so only a probe can find it",
		},
		{
			name:     "every declared namespace is inside",
			seed:     []Bead{{ID: "gcg-1"}, {ID: "gcnq-2"}},
			prefixes: classPrefixes,
			want:     false,
			why:      "a binding holding only its own ids is exactly the converged city the probe retires for",
		},
		{
			// The load-bearing row (ga-qdt5y.19). A closed relic is still read,
			// reopened and claimed by id, and `gc storage migrate` never deletes
			// the work store's pre-migration copy — so a predicate that skipped
			// closed rows would certify this store clean and send that id back to
			// a frozen copy that reads OPEN forever.
			name:     "a closed relic is still a resident",
			seed:     []Bead{{ID: "gcg-1"}, {ID: "ga-done"}},
			close:    []string{"ga-done"},
			prefixes: classPrefixes,
			want:     true,
			why:      "closing a relic drains it; it does not make it findable by prefix",
		},
		{
			// The must-be-silent counterpart: widening past closed rows must widen
			// only the CLOSED half. The namespace still decides what counts, or
			// every city that ever closed an infrastructure bead keeps its probe.
			name:     "a closed bead inside a declared namespace is ordinary retired infrastructure",
			seed:     []Bead{{ID: "gcg-1"}},
			close:    []string{"gcg-1"},
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
			// A wisp carried across is as unfindable as an issue, and it lives in
			// the wisp tier. A predicate that read one tier would miss it.
			name:     "a wisp-tier relic counts",
			seed:     []Bead{{ID: "gcg-1"}, {ID: "ga-wisp", Ephemeral: true}},
			prefixes: classPrefixes,
			want:     true,
			why:      "the census reads both tiers or it is not a census",
		},
		{
			// The rule is `id == prefix || strings.HasPrefix(id, prefix+"-")`. A
			// predicate written as LIKE 'gcg%' would swallow gcgx-1 — a different
			// namespace entirely — and retire the probe over it.
			name:     "a lookalike prefix is not the namespace",
			seed:     []Bead{{ID: "gcgx-1"}},
			prefixes: classPrefixes,
			want:     true,
			why:      "gcgx is its own namespace; only gcg and gcg-... are inside gcg",
		},
		{
			name:     "the bare prefix is inside its own namespace",
			seed:     []Bead{{ID: "gcg"}},
			prefixes: classPrefixes,
			want:     false,
			why:      "IDInNamespace admits the bare prefix, and the predicate must agree",
		},
		{
			// A binding that claims no namespace recognizes nothing, so every
			// resident is a relic. That is the honest answer, and it pairs with
			// the mint bit: such a binding never mints truthfully either.
			name:     "a binding claiming no namespace counts every resident",
			seed:     []Bead{{ID: "gcg-1"}},
			prefixes: nil,
			want:     true,
			why:      "nothing is declared, so nothing is inside",
		},
		{
			name:     "a binding claiming no namespace over an empty store is still clean",
			prefixes: nil,
			want:     false,
			why:      "there is no row to be outside anything",
		},
		{
			// Blank prefixes are dropped rather than treated as a namespace that
			// claims everything; IDInNamespace says an empty prefix claims nothing.
			name:     "a blank prefix claims nothing",
			seed:     []Bead{{ID: "gcg-1"}},
			prefixes: []string{"  "},
			want:     true,
			why:      "an empty namespace is not a wildcard",
		},
		{
			name:     "surrounding whitespace does not change a namespace",
			seed:     []Bead{{ID: "gcg-1"}},
			prefixes: []string{" gcg "},
			want:     false,
			why:      "IDInNamespace trims the prefix before it compares, and so must the predicate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openCensusStore(t)
			for _, b := range tc.seed {
				seedCensusRow(t, store, b)
			}
			for _, id := range tc.close {
				if err := store.Close(id); err != nil {
					t.Fatalf("closing %q: %v", id, err)
				}
			}

			got, err := store.HasResidentOutside(tc.prefixes)
			if err != nil {
				t.Fatalf("HasResidentOutside: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasResidentOutside(%v) = %v, want %v: %s", tc.prefixes, got, tc.want, tc.why)
			}
		})
	}
}

// The capability is what a caller type-asserts for, so the assertion has to
// land. A store that answers the question but does not satisfy the interface is
// invisible to every caller, and nothing else in the tree would fail.
func TestSQLiteStoreIsANamespaceCensus(t *testing.T) {
	var store Store = openCensusStore(t)
	if _, ok := store.(NamespaceCensus); !ok {
		t.Fatalf("%T does not satisfy beads.NamespaceCensus; the census would silently fall back to the full scan on every city", store)
	}
}

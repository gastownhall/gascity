package beads

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// censusPool is a library pool holding the two tier tables the native engine
// keeps its beads in, seeded with ids only: the census reads nothing else.
func censusPool(t *testing.T, issues, wisps []string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "census.db"))
	if err != nil {
		t.Fatalf("opening the census pool: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for table, ids := range map[string][]string{"issues": issues, "wisps": wisps} {
		if _, err := db.Exec("CREATE TABLE " + table + " (id TEXT PRIMARY KEY)"); err != nil {
			t.Fatalf("creating %s: %v", table, err)
		}
		for _, id := range ids {
			if _, err := db.Exec("INSERT INTO "+table+" (id) VALUES (?)", id); err != nil {
				t.Fatalf("seeding %s with %q: %v", table, id, err)
			}
		}
	}
	return db
}

// namespaceCensusCases are populations with the verdict the namespace rule
// (storeref.IDInNamespace: id == prefix, or id begins with prefix + "-") gives
// each, written out by hand.
var namespaceCensusCases = []struct {
	name     string
	prefixes []string
	issues   []string
	wisps    []string
	want     bool
}{
	{name: "empty binding", prefixes: []string{"gcg"}},
	{name: "clean in both tiers", prefixes: []string{"gcg"}, issues: []string{"gcg-1", "gcg-op-2"}, wisps: []string{"gcg-wisp-3"}},
	{name: "relic among issues", prefixes: []string{"gcg"}, issues: []string{"gcg-1", "zc-7"}, wisps: []string{"gcg-wisp-3"}, want: true},
	{name: "relic among wisps only", prefixes: []string{"gcg"}, issues: []string{"gcg-1"}, wisps: []string{"gcg-wisp-3", "zc-wisp-9"}, want: true},
	{name: "the bare prefix is claimed", prefixes: []string{"gcg"}, issues: []string{"gcg"}},
	{name: "the neighboring namespace is not", prefixes: []string{"gcg"}, issues: []string{"gcg-1", "gcgx-1"}, want: true},
	{name: "a shorter id is not", prefixes: []string{"gcg"}, issues: []string{"gc-1"}, want: true},
	{name: "case differs", prefixes: []string{"gcg"}, wisps: []string{"GCG-1"}, want: true},
	{name: "sorts between the bare prefix and its dash", prefixes: []string{"gcg"}, issues: []string{"gcg", "gcg!1", "gcg-1"}, want: true},
	{name: "sorts just past the namespace", prefixes: []string{"gcg"}, issues: []string{"gcg-1", "gcg.1"}, want: true},
	{name: "the dash alone is inside", prefixes: []string{"gcg"}, issues: []string{"gcg-"}},
	{name: "wildcard characters are literal", prefixes: []string{"g_g", "g%"}, issues: []string{"gcg-1"}, want: true},
	{name: "a literal underscore prefix claims its own", prefixes: []string{"g_g"}, issues: []string{"g_g-1"}},
	{name: "several namespaces, all claimed", prefixes: []string{"gcg", "zc"}, issues: []string{"gcg-1", "zc-2"}, wisps: []string{"zc"}},
	{name: "several namespaces, one stranger", prefixes: []string{"gcg", "zc"}, issues: []string{"gcg-1", "zc-2"}, wisps: []string{"ab-1"}, want: true},
	{name: "a stranger between two namespaces", prefixes: []string{"ab", "zc"}, issues: []string{"ab-1", "mm-1", "zc-1"}, want: true},
	{name: "nested namespaces", prefixes: []string{"gc", "gc-x"}, issues: []string{"gc-1", "gc-x-1", "gc-x"}},
	{name: "nested namespaces, a stranger beside them", prefixes: []string{"gc", "gc-x"}, issues: []string{"gc-1", "gcx-1"}, want: true},
	{name: "surrounding whitespace is trimmed", prefixes: []string{"  gcg\t"}, issues: []string{"gcg-1"}},
	{name: "an empty prefix claims nothing", prefixes: []string{"", "  "}, issues: []string{"gcg-1"}, want: true},
	{name: "no prefix and no rows", prefixes: nil},
	{name: "non-ASCII ids", prefixes: []string{"gé"}, issues: []string{"gé-1", "gé"}, wisps: []string{"ge-1"}, want: true},
	{name: "non-ASCII ids, all claimed", prefixes: []string{"gé"}, issues: []string{"gé-1", "gé"}},
}

// TestNativeDoltStoreAnswersTheNamespaceCensusItself is the native engine's
// half of beads.NamespaceCensus: the verdict comes from statements over the
// library's own pool, closed rows and both tiers included, and no bead is
// listed to reach it. The listing form reads the binding's whole history once
// per process, which on a city holding a week of closed order runs is most of
// every hook.
func TestNativeDoltStoreAnswersTheNamespaceCensusItself(t *testing.T) {
	for _, tc := range namespaceCensusCases {
		t.Run(tc.name, func(t *testing.T) {
			store := newNativeDoltStoreForTest(&poolStorage{db: censusPool(t, tc.issues, tc.wisps)})
			census, ok := NamespaceCensusFor(store)
			if !ok {
				t.Fatal("a native store over a library pool offers no namespace census; its binding is censused by listing its whole history")
			}
			got, err := census.HasResidentOutside(tc.prefixes)
			if err != nil {
				t.Fatalf("HasResidentOutside(%q): %v", tc.prefixes, err)
			}
			if got != tc.want {
				t.Fatalf("HasResidentOutside(%q) over issues %q and wisps %q = %v, want %v", tc.prefixes, tc.issues, tc.wisps, got, tc.want)
			}
			if stats, _ := ListRequestStatsOf(store); stats.Requests != 0 || stats.Rows != 0 {
				t.Fatalf("the census listed the binding (%+v); it must be answered without hydrating a row", stats)
			}
		})
	}
}

// TestNativeDoltStoreWithoutAPoolOffersNoCensus keeps the capability a hard
// gate: a library handle that exports no pool cannot answer exactly, so
// discovery says no and the caller lists the binding, as before.
func TestNativeDoltStoreWithoutAPoolOffersNoCensus(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{})
	if _, ok := NamespaceCensusFor(store); ok {
		t.Fatal("a native store whose library handle exports no pool offered a namespace census")
	}
	if has, err := store.HasResidentOutside([]string{"gcg"}); !errors.Is(err, ErrNamespaceCensusUnsupported) || has {
		t.Fatalf("HasResidentOutside over a handle with no pool = (%v, %v), want (false, ErrNamespaceCensusUnsupported)", has, err)
	}
}

// TestNativeDoltNamespaceCensusFailureIsNotAVerdict pins the direction the
// census may be wrong in: a read that failed is an error, never a clean
// binding.
func TestNativeDoltNamespaceCensusFailureIsNotAVerdict(t *testing.T) {
	db := censusPool(t, []string{"gcg-1"}, nil)
	if _, err := db.Exec("DROP TABLE wisps"); err != nil {
		t.Fatalf("dropping wisps: %v", err)
	}
	store := newNativeDoltStoreForTest(&poolStorage{db: db})
	if has, err := store.HasResidentOutside([]string{"gcg"}); err == nil {
		t.Fatalf("a census that could not read a tier answered %v with no error", has)
	}
}

// reportIDCollation makes every tier table report collation for its id column,
// as the Dolt server does and the test pool's SQLite cannot.
func reportIDCollation(t *testing.T, collation string) {
	t.Helper()
	prev := nativeCensusIDCollation
	nativeCensusIDCollation = func(ctx context.Context, tx *sql.Tx, table string) (string, bool, error) {
		var n int
		switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM "+table+" LIMIT 1").Scan(&n); {
		case errors.Is(err, sql.ErrNoRows):
			return "", true, nil
		case err != nil:
			return "", false, err
		}
		return collation, false, nil
	}
	t.Cleanup(func() { nativeCensusIDCollation = prev })
}

// TestNativeDoltNamespaceCensusSeeksAnExactBinaryIDColumn covers the form the
// census takes on the schema the library creates: an id column that compares
// bytes (utf8mb4_0900_bin) is asked for the gaps between the namespaces, which
// the primary key answers by seeking. The verdicts are the byte rule's, case
// for case.
func TestNativeDoltNamespaceCensusSeeksAnExactBinaryIDColumn(t *testing.T) {
	reportIDCollation(t, "utf8mb4_0900_bin")
	for _, tc := range namespaceCensusCases {
		t.Run(tc.name, func(t *testing.T) {
			store := newNativeDoltStoreForTest(&poolStorage{db: censusPool(t, tc.issues, tc.wisps)})
			got, err := store.HasResidentOutside(tc.prefixes)
			if err != nil {
				t.Fatalf("HasResidentOutside(%q): %v", tc.prefixes, err)
			}
			if got != tc.want {
				t.Fatalf("HasResidentOutside(%q) over issues %q and wisps %q = %v, want %v", tc.prefixes, tc.issues, tc.wisps, got, tc.want)
			}
		})
	}
}

// TestNativeDoltNamespaceCensusGapsSeekThePrimaryKey is why the gap form
// exists: a clean binding's census must not read every id. The query plan of
// the statement, over a primary key, names no table scan.
func TestNativeDoltNamespaceCensusGapsSeekThePrimaryKey(t *testing.T) {
	db := censusPool(t, []string{"gcg-1"}, nil)
	for _, prefixes := range [][]string{{"gcg"}, {"gcg", "zc"}, {"gc", "gc-x"}} {
		where, args := namespaceGapSQL("id", prefixes)
		if where == "" {
			t.Fatalf("namespaceGapSQL(%q) built no predicate", prefixes)
		}
		rows, err := db.Query("EXPLAIN QUERY PLAN SELECT 1 FROM issues WHERE "+where+" LIMIT 1", args...)
		if err != nil {
			t.Fatalf("explaining the census over %q: %v", prefixes, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatalf("reading the plan: %v", err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		searched := false
		for _, step := range plan {
			if strings.HasPrefix(step, "SCAN") {
				t.Fatalf("the census over %q scans the table: %q", prefixes, plan)
			}
			searched = searched || strings.HasPrefix(step, "SEARCH")
		}
		if !searched {
			t.Fatalf("the census over %q does not seek the primary key: %q", prefixes, plan)
		}
	}
}

// TestNativeDoltNamespaceCensusComparesBytesUnderAFoldingCollation is the
// reason the gap form is not used everywhere: on an id column that folds case
// or pads spaces, a range over the column would claim GCG-1 for gcg. Such a
// column is asked byte for byte.
func TestNativeDoltNamespaceCensusComparesBytesUnderAFoldingCollation(t *testing.T) {
	for _, collation := range []string{"utf8mb4_0900_ai_ci", "utf8mb4_bin", "utf8mb4_general_ci", ""} {
		t.Run(collation, func(t *testing.T) {
			reportIDCollation(t, collation)
			db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "census.db"))
			if err != nil {
				t.Fatalf("opening the census pool: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			for _, stmt := range []string{
				"CREATE TABLE issues (id TEXT COLLATE NOCASE PRIMARY KEY)",
				"CREATE TABLE wisps (id TEXT COLLATE NOCASE PRIMARY KEY)",
				"INSERT INTO issues (id) VALUES ('gcg-1')",
				"INSERT INTO wisps (id) VALUES ('GCG-1')",
			} {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			store := newNativeDoltStoreForTest(&poolStorage{db: db})
			has, err := store.HasResidentOutside([]string{"gcg"})
			if err != nil {
				t.Fatalf("HasResidentOutside: %v", err)
			}
			if !has {
				t.Fatalf("an id column reporting collation %q claimed GCG-1 for the gcg namespace", collation)
			}
		})
	}
}

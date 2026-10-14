package beads_test

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// pushdownBD is a fake bd that honors --id/--parent on `bd list` and the
// id=/parent= clauses of `bd query`, and records every argv it is run with.
type pushdownBD struct {
	mu      sync.Mutex
	calls   []string
	regular []map[string]any // bd list rows
	wisps   []map[string]any // bd query ephemeral rows
}

var queryTermRE = regexp.MustCompile(`(id|parent)=([A-Za-z0-9_.:-]+)`)

func (f *pushdownBD) run(_, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command: %s %v", name, args)
	}
	switch args[0] {
	case "list":
		var ids, parents map[string]bool
		for i, a := range args {
			if a == "--id" {
				ids = setOf(strings.Split(args[i+1], ","))
			}
			if a == "--parent" {
				parents = setOf([]string{args[i+1]})
			}
		}
		return []byte(mustMarshal(filterRows(f.regular, ids, parents))), nil
	case "query":
		clause := args[2]
		var ids, parents map[string]bool
		for _, m := range queryTermRE.FindAllStringSubmatch(clause, -1) {
			switch m[1] {
			case "id":
				if ids == nil {
					ids = map[string]bool{}
				}
				ids[m[2]] = true
			case "parent":
				if parents == nil {
					parents = map[string]bool{}
				}
				parents[m[2]] = true
			}
		}
		return []byte(mustMarshal(filterRows(f.wisps, ids, parents))), nil
	}
	return nil, fmt.Errorf("unexpected bd subcommand: %v", args)
}

func setOf(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

func filterRows(rows []map[string]any, ids, parents map[string]bool) []map[string]any {
	out := []map[string]any{}
	for _, r := range rows {
		if ids != nil && !ids[r["id"].(string)] {
			continue
		}
		if parents != nil {
			p, _ := r["parent"].(string)
			if !parents[p] {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

func mustMarshal(rows []map[string]any) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = fmt.Sprintf(`{"id":%q,"title":"t","status":"open","issue_type":"task","created_at":"2026-10-%02dT00:00:00Z","parent":%q}`,
			r["id"], 1+i%28, r["parent"])
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func (f *pushdownBD) argvs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// assertNoFullDump fails on any bd call that would read the whole store: a
// list with --all and no --id/--parent, or a query selecting every wisp.
func assertNoFullDump(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c, "bd list") && !strings.Contains(c, "--id ") && !strings.Contains(c, "--parent "):
			t.Errorf("unfiltered bd list: %s", c)
		case strings.HasPrefix(c, "bd query ") && !strings.Contains(c, "id=") && !strings.Contains(c, "parent="):
			t.Errorf("unfiltered bd query: %s", c)
		}
	}
}

func newPushdownStore(f *pushdownBD) *beads.BdStore {
	return beads.NewBdStore(t0Dir(), f.run)
}

func t0Dir() string { return "/city" }

func rowIDs(bs []beads.Bead) []string {
	ids := make([]string, len(bs))
	for i, b := range bs {
		ids[i] = b.ID
	}
	return ids
}

func TestBdStoreListIDsPushesIDFilterToBothTiers(t *testing.T) {
	f := &pushdownBD{
		regular: []map[string]any{{"id": "gc-1", "parent": ""}, {"id": "gc-2", "parent": ""}, {"id": "gc-3", "parent": ""}},
		wisps:   []map[string]any{{"id": "gc-w1", "parent": ""}, {"id": "gc-w2", "parent": ""}},
	}
	got, err := newPushdownStore(f).List(beads.ListQuery{
		IDs: []string{"gc-1", "gc-w1"}, IncludeClosed: true, TierMode: beads.TierBoth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := rowIDs(got); len(ids) != 2 || ids[0] != "gc-1" || ids[1] != "gc-w1" {
		t.Fatalf("ids = %v, want [gc-1 gc-w1]", ids)
	}
	calls := f.argvs()
	assertNoFullDump(t, calls)
	var sawList, sawQuery bool
	for _, c := range calls {
		sawList = sawList || (strings.HasPrefix(c, "bd list") && strings.Contains(c, "--id gc-1,gc-w1"))
		sawQuery = sawQuery || (strings.HasPrefix(c, "bd query") && strings.Contains(c, "ephemeral=true AND (id=gc-1 OR id=gc-w1)"))
	}
	if !sawList || !sawQuery {
		t.Fatalf("want --id list and id OR query, got %v", calls)
	}
}

func TestBdStoreListParentIDsPushesParentFilterToBothTiers(t *testing.T) {
	f := &pushdownBD{
		regular: []map[string]any{{"id": "gc-1", "parent": "gc-p1"}, {"id": "gc-2", "parent": "gc-p2"}, {"id": "gc-3", "parent": "gc-p3"}},
		wisps:   []map[string]any{{"id": "gc-w1", "parent": "gc-p1"}, {"id": "gc-w2", "parent": "gc-p3"}},
	}
	got, err := newPushdownStore(f).List(beads.ListQuery{
		ParentIDs: []string{"gc-p1", "gc-p2"}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := rowIDs(got); strings.Join(ids, ",") != "gc-1,gc-2,gc-w1" {
		t.Fatalf("ids = %v, want [gc-1 gc-2 gc-w1]", ids)
	}
	calls := f.argvs()
	assertNoFullDump(t, calls)
	var parents, wispQueries int
	for _, c := range calls {
		if strings.HasPrefix(c, "bd list") && (strings.Contains(c, "--parent gc-p1") || strings.Contains(c, "--parent gc-p2")) {
			parents++
		}
		if strings.HasPrefix(c, "bd query") && strings.Contains(c, "(parent=gc-p1 OR parent=gc-p2)") {
			wispQueries++
		}
	}
	if parents != 2 || wispQueries != 1 {
		t.Fatalf("want 2 --parent lists and 1 parent OR query, got %v", calls)
	}
}

func TestBdStoreListIDsChunksMergesAndDeduplicates(t *testing.T) {
	const n = 450 // 3 chunks of <=200
	f := &pushdownBD{}
	var want []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("gc-%d", i)
		f.regular = append(f.regular, map[string]any{"id": id, "parent": ""})
		want = append(want, id)
	}
	// Duplicate ids in the query must not duplicate rows or inflate --id.
	query := append(append([]string(nil), want...), "gc-0", "gc-449")
	got, err := newPushdownStore(f).List(beads.ListQuery{IDs: query, TierMode: beads.TierIssues})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rowIDs(got), ",") != strings.Join(want, ",") {
		t.Fatalf("got %d rows, want %d in query order", len(got), n)
	}
	calls := f.argvs()
	assertNoFullDump(t, calls)
	if len(calls) != 3 {
		t.Fatalf("bd calls = %d, want 3 chunks: %v", len(calls), calls)
	}
	for _, c := range calls {
		idArg := c[strings.Index(c, "--id ")+len("--id "):]
		if k := len(strings.Split(strings.Fields(idArg)[0], ",")); k > 200 {
			t.Fatalf("chunk carries %d ids, want <=200", k)
		}
	}
}

func TestBdStoreListIDsLimitAppliesAfterMerge(t *testing.T) {
	f := &pushdownBD{}
	var ids []string
	for i := 0; i < 250; i++ {
		id := fmt.Sprintf("gc-%d", i)
		f.regular = append(f.regular, map[string]any{"id": id, "parent": ""})
		ids = append(ids, id)
	}
	got, err := newPushdownStore(f).List(beads.ListQuery{IDs: ids, Limit: 5, TierMode: beads.TierIssues})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("len = %d, want limit 5 over the merged chunks", len(got))
	}
}

func TestBdStoreListParentIDsDisjointFromParentIDReturnsNothingWithoutBD(t *testing.T) {
	f := &pushdownBD{regular: []map[string]any{{"id": "gc-1", "parent": "gc-p1"}}}
	got, err := newPushdownStore(f).List(beads.ListQuery{
		ParentID: "gc-p9", ParentIDs: []string{"gc-p1"}, TierMode: beads.TierBoth, AllowScan: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || len(f.argvs()) != 0 {
		t.Fatalf("got %v with calls %v, want empty and no bd call", got, f.argvs())
	}
}

func TestBdStoreListIDsWithUnquotableIDFallsBackToGoFilter(t *testing.T) {
	f := &pushdownBD{regular: []map[string]any{{"id": "gc-1", "parent": ""}}}
	got, err := newPushdownStore(f).List(beads.ListQuery{
		IDs: []string{"gc-1", "bad id,with comma"}, TierMode: beads.TierIssues,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ids := rowIDs(got); len(ids) != 1 || ids[0] != "gc-1" {
		t.Fatalf("ids = %v, want [gc-1]", ids)
	}
	for _, c := range f.argvs() {
		if strings.Contains(c, "--id") {
			t.Fatalf("unsafe id must not be forwarded: %s", c)
		}
	}
}

package beads_test

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// bdListArgs returns the argv of the first `bd list` call.
func bdListArgs(t *testing.T, calls [][]string) []string {
	t.Helper()
	for _, args := range calls {
		if len(args) > 0 && args[0] == "list" {
			return args
		}
	}
	t.Fatalf("no bd list call in %q", calls)
	return nil
}

// idFlag returns the value following --id in args, and whether it was
// there at all.
func idFlag(args []string) (string, bool) {
	for i, a := range args {
		if a == "--id" && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// An IDs read names its ids to bd on both legs, so bd reads those rows rather
// than every row the other filters match. The blocker lookup behind Ready's
// work-outcome veto is such a read: without the pushdown it listed a store's
// whole closed history, issues and wisps, to find a few blockers.
func TestBdStoreListPushesIDsIntoBothLegs(t *testing.T) {
	var calls [][]string
	s := beads.NewBdStore("/city", recordingEmptyRunner(&calls))
	if _, err := s.List(beads.ListQuery{
		IDs:      []string{"gc-b", "gc-a.1", "gc-c"},
		Status:   "closed",
		TierMode: beads.TierBoth,
	}); err != nil {
		t.Fatal(err)
	}
	if got, ok := idFlag(bdListArgs(t, calls)); !ok || got != "gc-a.1,gc-b,gc-c" {
		t.Fatalf("bd list --id = %q (present %v), want the ids sorted, comma-separated: gc-a.1,gc-b,gc-c", got, ok)
	}
	want := `ephemeral=true AND status=closed AND (id="gc-a.1" OR id="gc-b" OR id="gc-c")`
	if got := wispQueryArg(t, calls); got != want {
		t.Fatalf("bd query expression = %q, want %q", got, want)
	}
}

// One id needs no OR: bd query answers a lone id clause as an indexed lookup.
func TestBdStoreListPushesOneIDWithoutAnOr(t *testing.T) {
	var calls [][]string
	s := beads.NewBdStore("/city", recordingEmptyRunner(&calls))
	if _, err := s.List(beads.ListQuery{IDs: []string{"gc-a"}, TierMode: beads.TierWisps}); err != nil {
		t.Fatal(err)
	}
	if got, _ := idFlag(bdListArgs(t, calls)); got != "gc-a" {
		t.Fatalf("bd list --id = %q, want gc-a", got)
	}
	if got, want := wispQueryArg(t, calls), `ephemeral=true AND id="gc-a"`; got != want {
		t.Fatalf("bd query expression = %q, want %q", got, want)
	}
}

// The same set of ids makes the same argv whatever order the caller built
// it in: the blocker lookup builds its ids from a map.
func TestBdStoreListIDPushdownIsOrderIndependent(t *testing.T) {
	argvFor := func(ids []string) [][]string {
		var calls [][]string
		s := beads.NewBdStore("/city", recordingEmptyRunner(&calls))
		if _, err := s.List(beads.ListQuery{IDs: ids, TierMode: beads.TierBoth}); err != nil {
			t.Fatal(err)
		}
		sort.Slice(calls, func(i, j int) bool { return calls[i][0] < calls[j][0] })
		return calls
	}
	a := argvFor([]string{"gc-1", "gc-2", "gc-3"})
	b := argvFor([]string{"gc-3", "gc-1", "gc-2"})
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("argv differs by input order:\n%q\n%q", a, b)
	}
}

// What bd answers is cut to the asked ids in Go all the same, so a bd that
// ignores the predicate, or matches more than asked (an id ending in '*' is
// a prefix to bd query), still gives the exact set.
func TestBdStoreListIDsStayExactWhenBdReturnsMore(t *testing.T) {
	rows := func(ids ...string) []byte {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = fmt.Sprintf(`{"id":%q,"title":"t","status":"closed","issue_type":"task","created_at":"2025-01-15T10:00:00Z"}`, id)
		}
		return []byte("[" + strings.Join(parts, ",") + "]")
	}
	var mu sync.Mutex
	runner := func(_, _ string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if args[0] == "query" {
			return rows("gc-a", "gc-wisp-x"), nil
		}
		return rows("gc-a", "gc-b", "gc-other"), nil
	}
	s := beads.NewBdStore("/city", runner)
	got, err := s.List(beads.ListQuery{IDs: []string{"gc-a", "gc-b"}, Status: "closed", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(got))
	for i, b := range got {
		ids[i] = b.ID
	}
	sort.Strings(ids)
	if want := []string{"gc-a", "gc-b"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("List ids = %v, want %v", ids, want)
	}
}

// Ids bd would read back as other ids, and lists too long for one argument,
// are not pushed down: that leg reads unpredicated by id, as before, and Go
// cuts the exact set.
func TestBdStoreListLeavesUncarriableIDsClientSide(t *testing.T) {
	long := make([]string, 0, 7000)
	for i := 0; i < 7000; i++ {
		long = append(long, fmt.Sprintf("gc-%08d", i))
	}
	cases := []struct {
		name      string
		ids       []string
		wantList  string // "" means no --id
		wantQuery string // the whole bd query expression
	}{
		{"comma", []string{"gc-a", "gc-b,c"}, "", `ephemeral=true AND (id="gc-a" OR id="gc-b,c")`},
		{"surrounding space", []string{"gc-a", " gc-b"}, "", `ephemeral=true AND (id=" gc-b" OR id="gc-a")`},
		{"quote", []string{"gc-a", `gc-"b"`}, `gc-"b",gc-a`, `ephemeral=true`},
		{"backslash", []string{"gc-a", `gc-\b`}, `gc-\b,gc-a`, `ephemeral=true`},
		{"non-ASCII", []string{"gc-a", "gc-é"}, "gc-a,gc-é", `ephemeral=true`},
		{"empty ids dropped", []string{"", "gc-a", ""}, "gc-a", `ephemeral=true AND id="gc-a"`},
		{"only empty ids", []string{"", ""}, "", `ephemeral=true`},
		{"past one argument", long, "", `ephemeral=true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			s := beads.NewBdStore("/city", recordingEmptyRunner(&calls))
			if _, err := s.List(beads.ListQuery{IDs: tc.ids, TierMode: beads.TierBoth}); err != nil {
				t.Fatal(err)
			}
			got, ok := idFlag(bdListArgs(t, calls))
			if tc.wantList == "" && ok {
				t.Fatalf("bd list --id = %q, want no --id", got)
			}
			if tc.wantList != "" && got != tc.wantList {
				t.Fatalf("bd list --id = %q, want %q", got, tc.wantList)
			}
			if got := wispQueryArg(t, calls); got != tc.wantQuery {
				t.Fatalf("bd query expression = %q, want %q", got, tc.wantQuery)
			}
		})
	}
}

// A read with no IDs keeps the argv it had.
func TestBdStoreListWithoutIDsPushesNoID(t *testing.T) {
	var calls [][]string
	s := beads.NewBdStore("/city", recordingEmptyRunner(&calls))
	if _, err := s.List(beads.ListQuery{Status: "closed", TierMode: beads.TierBoth}); err != nil {
		t.Fatal(err)
	}
	if got, ok := idFlag(bdListArgs(t, calls)); ok {
		t.Fatalf("bd list --id = %q, want none", got)
	}
	if got, want := wispQueryArg(t, calls), `ephemeral=true AND status=closed`; got != want {
		t.Fatalf("bd query expression = %q, want %q", got, want)
	}
}

// Ready's work-outcome veto looks its blockers up by id, and names them to
// bd: the lookup reads the blockers, not the store's closed history, and the
// veto still fires.
func TestBdStoreReadyBlockerLookupNamesItsBlockers(t *testing.T) {
	runner, listArgs := bdStoreWorkOutcomeReadyRunner(`,"metadata":{"gc.work_outcome":"blocked"}`)
	s := beads.NewBdStore("/city", runner)
	got, err := s.Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Ready() = %+v, want empty: the blocker closed with gc.work_outcome=blocked", got)
	}
	if len(*listArgs) == 0 {
		t.Fatal("Ready() never issued the blocker lookup")
	}
	for _, args := range *listArgs {
		if !strings.HasSuffix(args, " --id bd-blocker") && !strings.Contains(args, " --id bd-blocker ") {
			t.Fatalf("blocker lookup %q does not name its blocker: want --id bd-blocker", args)
		}
	}
}

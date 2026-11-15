package beads_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// idRow is one bead the fake bd below can return.
type idRow struct {
	ID        string
	Status    string
	Ephemeral bool
}

// idFilterBd is a fake bd that honors `bd list --id a,b`, `--status=` and
// `--all` the way bd does: unknown ids are omitted, closed rows need --all or
// an explicit --status, and a list with no --id returns every persistent row.
// `bd query` (the ephemeral tier's leg) returns every ephemeral row; it has no
// id clause, so the store's Go-side filter is what narrows it.
type idFilterBd struct {
	rows []idRow
	// alsoReturn, when set, is a row id the fake adds to EVERY bd list answer
	// whether or not --id named it: the overlap a union across chunks must dedupe.
	alsoReturn string
	// failOnListCall, when > 0, fails the nth (1-based) bd list call.
	failOnListCall int

	mu        sync.Mutex // a two-tier List runs its two bd reads at once
	calls     []string
	listCalls int
}

func (f *idFilterBd) runner(_, name string, args ...string) ([]byte, error) {
	full := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, full)
	isList := len(args) > 0 && args[0] == "list"
	if isList {
		f.listCalls++
	}
	n := f.listCalls
	f.mu.Unlock()

	switch {
	case isList:
		if f.failOnListCall > 0 && n == f.failOnListCall {
			return nil, fmt.Errorf("fake bd: list call %d failed", n)
		}
		return f.list(args)
	case len(args) > 0 && args[0] == "query":
		var out []map[string]any
		for _, row := range f.rows {
			if row.Ephemeral {
				out = append(out, idRowJSON(row))
			}
		}
		return json.Marshal(out)
	}
	return nil, fmt.Errorf("unexpected command: %s", full)
}

func (f *idFilterBd) list(args []string) ([]byte, error) {
	var (
		ids    map[string]bool
		status string
		all    bool
	)
	for i, a := range args {
		switch {
		case a == "--id" && i+1 < len(args):
			ids = map[string]bool{}
			for _, id := range strings.Split(args[i+1], ",") {
				ids[id] = true
			}
		case strings.HasPrefix(a, "--status="):
			status = strings.TrimPrefix(a, "--status=")
		case a == "--all":
			all = true
		}
	}
	out := []map[string]any{}
	for _, row := range f.rows {
		if row.Ephemeral {
			continue
		}
		if ids != nil && !ids[row.ID] && row.ID != f.alsoReturn {
			continue
		}
		if status != "" && row.Status != status {
			continue
		}
		if status == "" && !all && row.Status == "closed" {
			continue
		}
		out = append(out, idRowJSON(row))
	}
	return json.Marshal(out)
}

func idRowJSON(row idRow) map[string]any {
	return map[string]any{
		"id": row.ID, "title": row.ID, "status": row.Status, "issue_type": "task",
		"priority": 2, "created_at": "2026-09-25T08:00:00Z", "ephemeral": row.Ephemeral,
	}
}

func (f *idFilterBd) listCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "bd list ") {
			out = append(out, c)
		}
	}
	return out
}

// idArgOf returns the value after --id in one recorded command, or "" when the
// command has no --id flag.
func idArgOf(call string) string {
	fields := strings.Fields(call)
	for i, field := range fields {
		if field == "--id" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func sortedIDs(got []beads.Bead) []string {
	ids := fanoutIDs(got)
	slices.Sort(ids)
	return ids
}

func openRows(ids ...string) []idRow {
	rows := make([]idRow, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, idRow{ID: id, Status: "open"})
	}
	return rows
}

// manyIDs returns n distinct ids, zero-padded so lexical order is numeric.
func manyIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%04d", i)
	}
	return ids
}

// FR-1: ListQuery.IDs reaches bd as --id, so bd answers with only those rows
// instead of the whole status/tier corpus.
func TestBdStoreListPushesIDsToBdList(t *testing.T) {
	fake := &idFilterBd{rows: openRows("a", "b", "c", "d")}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(beads.ListQuery{IDs: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	calls := fake.listCommands()
	if len(calls) != 1 || idArgOf(calls[0]) != "a,b" {
		t.Fatalf("bd list calls = %q, want exactly one carrying --id a,b", calls)
	}
	if want := []string{"a", "b"}; !slices.Equal(sortedIDs(got), want) {
		t.Fatalf("List ids = %v, want %v", sortedIDs(got), want)
	}
}

// --id composes with the other server-side flags: IncludeClosed still yields
// --all, and an explicit Status still narrows.
func TestBdStoreListIDsComposeWithClosedAndStatus(t *testing.T) {
	rows := append(openRows("a"), idRow{ID: "z", Status: "closed"})

	t.Run("IncludeClosed keeps --all", func(t *testing.T) {
		fake := &idFilterBd{rows: rows}
		s := beads.NewBdStore("/city", fake.runner)
		got, err := s.List(beads.ListQuery{IDs: []string{"a", "z"}, IncludeClosed: true})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		calls := fake.listCommands()
		if len(calls) != 1 || idArgOf(calls[0]) != "a,z" || !strings.Contains(calls[0], " --all") {
			t.Fatalf("bd list calls = %q, want one with --id a,z and --all", calls)
		}
		if want := []string{"a", "z"}; !slices.Equal(sortedIDs(got), want) {
			t.Fatalf("List ids = %v, want %v", sortedIDs(got), want)
		}
	})

	t.Run("Status open drops the closed id", func(t *testing.T) {
		fake := &idFilterBd{rows: rows}
		s := beads.NewBdStore("/city", fake.runner)
		got, err := s.List(beads.ListQuery{IDs: []string{"a", "z"}, Status: "open"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		calls := fake.listCommands()
		if len(calls) != 1 || idArgOf(calls[0]) != "a,z" || !strings.Contains(calls[0], "--status=open") {
			t.Fatalf("bd list calls = %q, want one with --id a,z and --status=open", calls)
		}
		if want := []string{"a"}; !slices.Equal(sortedIDs(got), want) {
			t.Fatalf("List ids = %v, want %v", sortedIDs(got), want)
		}
	})
}

// FR-2 / G1: a blank --id means "no filter" to bd (measured: the whole open
// ledger), so ids that are all blank spawn no bd and return empty.
func TestBdStoreListIDsBlankOnlyRunsNoBd(t *testing.T) {
	fake := &idFilterBd{rows: openRows("a", "b")}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(beads.ListQuery{IDs: []string{"", "  "}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List = %v, want empty", fanoutIDs(got))
	}
	if len(fake.calls) != 0 {
		t.Fatalf("bd was spawned for blank-only ids: %q", fake.calls)
	}
}

// FR-2: ids are trimmed of blanks, deduped, and keep first-seen order.
func TestBdStoreListIDsSentOnceInOrderWithoutBlanks(t *testing.T) {
	fake := &idFilterBd{rows: openRows("a", "b")}
	s := beads.NewBdStore("/city", fake.runner)

	if _, err := s.List(beads.ListQuery{IDs: []string{"b", " ", "a", "b", "", "a"}}); err != nil {
		t.Fatalf("List: %v", err)
	}
	calls := fake.listCommands()
	if len(calls) != 1 || idArgOf(calls[0]) != "b,a" {
		t.Fatalf("bd list calls = %q, want exactly one carrying --id b,a", calls)
	}
}

// FR-3 / G2: more than one chunk of ids is split at 500, every chunk reads
// unbounded, the union is deduped by id, and Limit is applied once afterwards.
func TestBdStoreListIDsChunksAndUnionsOnce(t *testing.T) {
	ids := manyIDs(1201)

	t.Run("no limit returns every row once", func(t *testing.T) {
		// alsoReturn puts id-0000 in all three answers: the union must dedupe it.
		fake := &idFilterBd{rows: openRows(ids...), alsoReturn: ids[0]}
		s := beads.NewBdStore("/city", fake.runner)
		got, err := s.List(beads.ListQuery{IDs: ids})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		assertChunkSizes(t, fake.listCommands(), []int{500, 500, 201})
		if len(got) != len(ids) || !slices.Equal(sortedIDs(got), ids) {
			t.Fatalf("List returned %d rows (want %d unique ids)", len(got), len(ids))
		}
	})

	t.Run("Limit applies once after the union", func(t *testing.T) {
		fake := &idFilterBd{rows: openRows(ids...)}
		s := beads.NewBdStore("/city", fake.runner)
		got, err := s.List(beads.ListQuery{IDs: ids, Limit: 700})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		calls := fake.listCommands()
		assertChunkSizes(t, calls, []int{500, 500, 201})
		for _, call := range calls {
			if !strings.Contains(call, "--limit 0") {
				t.Fatalf("chunk read %q is bounded: a per-chunk limit truncates the union", call)
			}
		}
		if len(got) != 700 {
			t.Fatalf("List returned %d rows, want 700 (Limit applied once to the union)", len(got))
		}
	})
}

func assertChunkSizes(t *testing.T, calls []string, want []int) {
	t.Helper()
	var sizes []int
	for _, call := range calls {
		arg := idArgOf(call)
		if arg == "" {
			t.Fatalf("bd list call without --id: %q", call)
		}
		sizes = append(sizes, len(strings.Split(arg, ",")))
	}
	if !slices.Equal(sizes, want) {
		t.Fatalf("--id chunk sizes = %v, want %v", sizes, want)
	}
}

// FR-4 / G4: one failing chunk fails the whole List; a partial union is never
// returned as if it were complete.
func TestBdStoreListIDsFailingChunkFailsList(t *testing.T) {
	ids := manyIDs(1201)
	fake := &idFilterBd{rows: openRows(ids...), failOnListCall: 2}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(beads.ListQuery{IDs: ids})
	if err == nil {
		t.Fatalf("List succeeded with %d rows though chunk 2 failed", len(got))
	}
	if len(got) != 0 {
		t.Fatalf("List returned %d rows alongside the error", len(got))
	}
}

// FR-5 / G5: the bd list leg narrows, the ephemeral bd query leg is untouched,
// and a TierBoth read stays exact across the two.
func TestBdStoreListIDsTierBothStaysExact(t *testing.T) {
	rows := append(openRows("a", "b"), idRow{ID: "w1", Status: "open", Ephemeral: true}, idRow{ID: "w2", Status: "open", Ephemeral: true})
	fake := &idFilterBd{rows: rows}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(beads.ListQuery{IDs: []string{"a", "w1"}, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"a", "w1"}; !slices.Equal(sortedIDs(got), want) {
		t.Fatalf("List ids = %v, want %v", sortedIDs(got), want)
	}
	lists := fake.listCommands()
	if len(lists) != 1 || idArgOf(lists[0]) != "a,w1" {
		t.Fatalf("bd list calls = %q, want one carrying --id a,w1", lists)
	}
	if q := firstCommandWithPrefix(fake.calls, "bd query "); q == "" || strings.Contains(q, "--id") {
		t.Fatalf("bd query call = %q, want one without --id", q)
	}
}

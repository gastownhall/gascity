package beads_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// fanoutRow is one ephemeral bead the fake bd below can return.
type fanoutRow struct {
	ID       string
	Assignee string
	Priority int
	Created  string
}

// predicatedBd is a fake bd whose rows are all ephemeral. `bd list` (the
// persistent tier) returns none. A `bd query` with an `assignee=X` clause
// (bare or quoted) keeps only X's rows, in bd's default order (priority ASC,
// created_at DESC, id ASC) cut to --limit N (0 = unlimited); a query with no
// assignee clause returns every row.
type predicatedBd struct {
	t     *testing.T
	rows  []fanoutRow
	mu    sync.Mutex // guards calls: a two-tier List runs its two bd reads at once
	calls []string
}

func (f *predicatedBd) runner(_, name string, args ...string) ([]byte, error) {
	full := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, full)
	f.mu.Unlock()
	if len(args) > 0 && args[0] == "list" {
		return []byte(`[]`), nil
	}
	if len(args) < 3 || args[0] != "query" {
		return nil, fmt.Errorf("unexpected command: %s", full)
	}
	var (
		assignee  string
		predicate bool
		limit     int
	)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--limit" {
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return nil, fmt.Errorf("bad --limit %q: %w", args[i+1], err)
			}
			limit = n
		}
	}
	for _, clause := range strings.Split(args[2], " AND ") {
		if v, ok := strings.CutPrefix(clause, "assignee="); ok {
			assignee, predicate = unquoteBdQueryValue(f.t, v), true
		}
	}

	var matched []fanoutRow
	for _, row := range f.rows {
		if !predicate || row.Assignee == assignee {
			matched = append(matched, row)
		}
	}
	slices.SortStableFunc(matched, func(a, b fanoutRow) int {
		if a.Priority != b.Priority {
			return a.Priority - b.Priority
		}
		if a.Created != b.Created {
			return strings.Compare(b.Created, a.Created)
		}
		return strings.Compare(a.ID, b.ID)
	})
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	out := make([]map[string]any, 0, len(matched))
	for _, row := range matched {
		out = append(out, map[string]any{
			"id": row.ID, "title": "message", "status": "open", "issue_type": "message",
			"assignee": row.Assignee, "priority": row.Priority, "created_at": row.Created, "ephemeral": true,
		})
	}
	return json.Marshal(out)
}

// unquoteBdQueryValue decodes a bd query DSL value the way the bd lexer does:
// a bare token as-is, or a double-quoted string with \" and \\ escapes.
func unquoteBdQueryValue(t *testing.T, v string) string {
	t.Helper()
	if !strings.HasPrefix(v, `"`) {
		return v
	}
	if len(v) < 2 || !strings.HasSuffix(v, `"`) {
		t.Fatalf("unterminated quoted bd query value %q", v)
	}
	var sb strings.Builder
	body := v[1 : len(v)-1]
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' && i+1 < len(body) {
			i++
		}
		sb.WriteByte(body[i])
	}
	return sb.String()
}

func fanoutIDs(got []beads.Bead) []string {
	ids := make([]string, 0, len(got))
	for _, b := range got {
		ids = append(ids, b.ID)
	}
	return ids
}

// A quote or backslash in a value is escaped the way bd's lexer reads it back.
func TestBdStoreListWispsEscapesQuotedQueryValue(t *testing.T) {
	const odd = `ops/"night" \shift`
	fake := &predicatedBd{t: t, rows: []fanoutRow{
		{ID: "w1", Assignee: odd, Priority: 2, Created: "2026-09-25T08:00:00Z"},
		{ID: "w2", Assignee: "someone-else", Priority: 2, Created: "2026-09-25T09:00:00Z"},
	}}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(beads.ListQuery{Assignee: odd, TierMode: beads.TierWisps})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	query := firstCommandWithPrefix(fake.calls, "bd query ")
	if want := `assignee="ops/\"night\" \\shift"`; !strings.Contains(query, want) {
		t.Fatalf("query cmd = %q, want escaped predicate %s", query, want)
	}
	if want := []string{"w1"}; !slices.Equal(fanoutIDs(got), want) {
		t.Fatalf("List ids = %v, want %v", fanoutIDs(got), want)
	}
}

// bd reads the value none/null as "no assignee", so such a value cannot be a
// predicate for a route literally named that; it stays a client-side filter.
func TestBdStoreListWispsKeepsNoneValueClientSide(t *testing.T) {
	fake := &predicatedBd{t: t}
	s := beads.NewBdStore("/city", fake.runner)

	if _, err := s.List(beads.ListQuery{Assignee: "None", TierMode: beads.TierWisps}); err != nil {
		t.Fatalf("List: %v", err)
	}
	query := firstCommandWithPrefix(fake.calls, "bd query ")
	if strings.Contains(strings.ToLower(query), "assignee=") {
		t.Fatalf("query cmd = %q, a none/null value must not become a bd predicate", query)
	}
}

// The default sort is bd's own order, so bd may still cut the limit there.
func TestBdStoreListWispsDefaultSortKeepsServerLimit(t *testing.T) {
	fake := &predicatedBd{t: t, rows: []fanoutRow{
		{ID: "w-old", Assignee: "rig/a", Priority: 0, Created: "2026-09-25T08:00:00Z"},
		{ID: "w-new", Assignee: "rig/a", Priority: 2, Created: "2026-09-25T10:00:00Z"},
	}}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(beads.ListQuery{Assignee: "rig/a", TierMode: beads.TierWisps, Limit: 1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"w-old"}; !slices.Equal(fanoutIDs(got), want) {
		t.Fatalf("List ids = %v, want %v", fanoutIDs(got), want)
	}
	query := firstCommandWithPrefix(fake.calls, "bd query ")
	if !strings.Contains(query, "--limit 1") {
		t.Fatalf("query cmd = %q, want the default-sort limit pushed to bd", query)
	}
}

// bd's own order is no prefix of created-desc: it puts a higher priority
// first, and breaks a created_at tie by id ASC where created-desc takes the
// larger id. A created-desc read therefore reaches bd unlimited and Go cuts
// the limit, for a quoted value as for a bare one.
func TestBdStoreListWispsCreatedDescReadsUnlimited(t *testing.T) {
	cases := []struct {
		name string
		rows []fanoutRow
		want string
	}{
		{"older row of higher priority", []fanoutRow{
			{ID: "w-old", Priority: 0, Created: "2026-09-25T08:00:00Z"},
			{ID: "w-new", Priority: 2, Created: "2026-09-25T10:00:00Z"},
		}, "w-new"},
		{"created_at tie", []fanoutRow{
			{ID: "w-1", Priority: 2, Created: "2026-09-25T08:00:00Z"},
			{ID: "w-2", Priority: 2, Created: "2026-09-25T08:00:00Z"},
		}, "w-2"},
	}
	for _, assignee := range []string{"rig/a", "rig-a"} { // quoted, bare
		for _, tc := range cases {
			t.Run(assignee+"/"+tc.name, func(t *testing.T) {
				rows := slices.Clone(tc.rows)
				for i := range rows {
					rows[i].Assignee = assignee
				}
				fake := &predicatedBd{t: t, rows: rows}
				s := beads.NewBdStore("/city", fake.runner)

				got, err := s.List(beads.ListQuery{
					Assignee: assignee, TierMode: beads.TierBoth, Sort: beads.SortCreatedDesc, Limit: 1,
				})
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				if want := []string{tc.want}; !slices.Equal(fanoutIDs(got), want) {
					t.Fatalf("List ids = %v, want %v (calls %v)", fanoutIDs(got), want, fake.calls)
				}
				query := firstCommandWithPrefix(fake.calls, "bd query ")
				if !strings.Contains(query, "--limit 0") {
					t.Fatalf("query cmd = %q, want --limit 0 for a created-desc read", query)
				}
			})
		}
	}
}

// IDs is matched only Go-side, so bd must not cut the limit before that
// filter runs, or the wanted row is never read. The assignee is bare, so the
// limit gate is what is under test.
func TestBdStoreListWispsIDsReadsUnlimitedBeforeIDsFilter(t *testing.T) {
	fake := &predicatedBd{t: t, rows: []fanoutRow{
		{ID: "w-other", Assignee: "rig-a", Priority: 0, Created: "2026-09-25T08:00:00Z"},
		{ID: "w-wanted", Assignee: "rig-a", Priority: 2, Created: "2026-09-25T08:00:00Z"},
	}}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(beads.ListQuery{Assignee: "rig-a", IDs: []string{"w-wanted"}, TierMode: beads.TierWisps, Limit: 1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"w-wanted"}; !slices.Equal(fanoutIDs(got), want) {
		t.Fatalf("List ids = %v, want %v (calls %v)", fanoutIDs(got), want, fake.calls)
	}
}

// bd's lexer reads a byte at a time and writes each byte back as a rune, so a
// value with a byte outside printable ASCII would reach bd as a different
// string, quoted or bare. Such a value stays a client-side filter.
func TestBdStoreListWispsKeepsNonASCIIValueClientSide(t *testing.T) {
	fake := &predicatedBd{t: t, rows: []fanoutRow{
		{ID: "w1", Assignee: "größe", Priority: 2, Created: "2026-09-25T08:00:00Z"},
		{ID: "w2", Assignee: "grosse", Priority: 2, Created: "2026-09-25T09:00:00Z"},
	}}
	s := beads.NewBdStore("/city", fake.runner)

	got, err := s.List(beads.ListQuery{Assignee: "größe", TierMode: beads.TierWisps})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	query := firstCommandWithPrefix(fake.calls, "bd query ")
	if strings.Contains(query, "assignee=") {
		t.Fatalf("query cmd = %q, a non-ASCII value must not become a bd predicate", query)
	}
	if want := []string{"w1"}; !slices.Equal(fanoutIDs(got), want) {
		t.Fatalf("List ids = %v, want %v", fanoutIDs(got), want)
	}
}

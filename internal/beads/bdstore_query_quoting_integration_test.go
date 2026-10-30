//go:build integration

package beads_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The wisp-leg bd query carries a value outside the bare-token set as a quoted
// string. This runs the real bd on PATH: for a slash assignee, a slash label
// and an assignee holding a quote and a backslash, the rows bd's raw output
// holds for the bd query call must equal the rows List keeps. A value bd
// dropped would come back unpredicated (more rows than kept), and one it
// misread would match nothing or fail the query.
func TestBdStoreListWispsQuotedPredicatesAgainstRealBd(t *testing.T) {
	const odd = `ops/"night" \shift`
	seed, dir := newConditionalIntegrationBdStore(t)
	for _, b := range []beads.Bead{
		{Title: "a labeled 1", Type: "task", Ephemeral: true, Assignee: "rig/a", Labels: []string{"team/ops"}},
		{Title: "a labeled 2", Type: "task", Ephemeral: true, Assignee: "rig/a", Labels: []string{"team/ops"}},
		{Title: "a unlabeled", Type: "task", Ephemeral: true, Assignee: "rig/a"},
		{Title: "b labeled", Type: "task", Ephemeral: true, Assignee: "rig/b", Labels: []string{"team/ops"}},
		{Title: "odd", Type: "task", Ephemeral: true, Assignee: odd},
	} {
		if _, err := seed.Create(b); err != nil {
			t.Fatalf("Create %q: %v", b.Title, err)
		}
	}

	realBd := newConditionalIntegrationRunner(t, dir)
	var (
		queryCmd string
		queryOut []byte
	)
	store := beads.NewBdStore(dir, func(d, name string, args ...string) ([]byte, error) {
		out, err := realBd(d, name, args...)
		if len(args) > 0 && args[0] == "query" {
			queryCmd, queryOut = strings.Join(args, " "), out
		}
		return out, err
	})

	cases := []struct {
		name   string
		query  beads.ListQuery
		clause string
		want   int
	}{
		{"slash assignee", beads.ListQuery{Assignee: "rig/a", TierMode: beads.TierWisps}, `assignee="rig/a"`, 3},
		{"slash label", beads.ListQuery{Label: "team/ops", TierMode: beads.TierWisps}, `label="team/ops"`, 3},
		{"escaped assignee", beads.ListQuery{Assignee: odd, TierMode: beads.TierWisps}, `assignee="ops/\"night\" \\shift"`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			queryCmd, queryOut = "", nil // each case reads its own bd query
			got, err := store.List(tc.query)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if !strings.Contains(queryCmd, tc.clause) {
				t.Fatalf("bd query args = %q, want predicate %s", queryCmd, tc.clause)
			}
			var rows []json.RawMessage
			if err := json.Unmarshal(queryOut, &rows); err != nil {
				t.Fatalf("decode bd query output: %v\n%s", err, queryOut)
			}
			if len(rows) != len(got) || len(got) != tc.want {
				t.Fatalf("bd query returned %d rows, List kept %d, want %d: bd did not apply %s",
					len(rows), len(got), tc.want, tc.clause)
			}
		})
	}
}

// A created-desc read with a limit must return the newest rows whatever their
// priority. bd's own page puts priority first, so the wisp query reaches bd
// unlimited and Go cuts the limit. The rows are created in ID order, so a
// created_at tie (bd stamps whole seconds) breaks by id DESC to the same
// answer. order-run:<name> read with IncludeClosed is the shape of an order's
// recent runs.
func TestBdStoreListWispsCreatedDescLimitAgainstRealBd(t *testing.T) {
	seed, dir := newConditionalIntegrationBdStore(t)
	for _, b := range []beads.Bead{
		{ID: "tst-wq-1", Title: "oldest", Priority: ptr(0)},
		{ID: "tst-wq-2", Title: "middle", Priority: ptr(2)},
		{ID: "tst-wq-3", Title: "newest", Priority: ptr(2)},
	} {
		b.Type, b.Ephemeral, b.Assignee, b.Labels = "task", true, "rig/a", []string{"order-run:rig/agent"}
		if _, err := seed.Create(b); err != nil {
			t.Fatalf("Create %s: %v", b.ID, err)
		}
	}

	realBd := newConditionalIntegrationRunner(t, dir)
	var queryCmd string
	store := beads.NewBdStore(dir, func(d, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "query" {
			queryCmd = strings.Join(args, " ")
		}
		return realBd(d, name, args...)
	})

	cases := []struct {
		name   string
		query  beads.ListQuery
		clause string
	}{
		{"slash assignee", beads.ListQuery{Assignee: "rig/a"}, `assignee="rig/a"`},
		{"order-run label", beads.ListQuery{Label: "order-run:rig/agent", IncludeClosed: true}, `label="order-run:rig/agent"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			queryCmd = ""
			q := tc.query
			q.TierMode, q.Sort, q.Limit = beads.TierBoth, beads.SortCreatedDesc, 2
			got, err := store.List(q)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if !strings.Contains(queryCmd, tc.clause) || !strings.Contains(queryCmd, "--limit 0") {
				t.Fatalf("bd query args = %q, want predicate %s read with --limit 0", queryCmd, tc.clause)
			}
			if want := []string{"tst-wq-3", "tst-wq-2"}; !slices.Equal(fanoutIDs(got), want) {
				t.Fatalf("List ids = %v, want the two newest %v", fanoutIDs(got), want)
			}
		})
	}
}

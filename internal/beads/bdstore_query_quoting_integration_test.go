//go:build integration

package beads_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The wisp-leg bd query carries a value outside the bare-token set as a quoted
// string. This runs the real bd on PATH: for a slash assignee and for a slash
// label, the rows bd's raw output holds for the bd query call must equal the
// rows List keeps. A value bd dropped or misread would come back
// unpredicated (more rows than kept) or fail the query.
func TestBdStoreListWispsQuotedPredicatesAgainstRealBd(t *testing.T) {
	seed, dir := newConditionalIntegrationBdStore(t)
	for _, b := range []beads.Bead{
		{Title: "a labeled 1", Type: "task", Ephemeral: true, Assignee: "rig/a", Labels: []string{"team/ops"}},
		{Title: "a labeled 2", Type: "task", Ephemeral: true, Assignee: "rig/a", Labels: []string{"team/ops"}},
		{Title: "a unlabeled", Type: "task", Ephemeral: true, Assignee: "rig/a"},
		{Title: "b labeled", Type: "task", Ephemeral: true, Assignee: "rig/b", Labels: []string{"team/ops"}},
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

package beads

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestSQLiteListPushesPluralAssigneesIntoSQL pins that a plural-assignee list
// filters in SQL rather than in Go. Before, sqliteListSQL left Assignees to
// ListQuery.Matches, so a mailbox read (open messages, Assignees = the
// session's routes) decoded every open message of the binding: on
// maintainer-city's 2.15 GB infra store that is 9,118 rows decoded to keep one,
// about 0.2-0.26 s of every `gc prime --hook` and `gc mail check --inject`.
func TestSQLiteListPushesPluralAssigneesIntoSQL(t *testing.T) {
	cases := map[string]ListQuery{
		"mailbox":         {Type: "message", Status: "open", TierMode: TierBoth, Live: true, Assignees: []string{"sess-1", "alias-1"}},
		"active molecule": {Status: "in_progress", Type: "molecule", Assignees: []string{"sess-1", "name-1"}, TierMode: TierBoth, Limit: 5, Sort: SortCreatedDesc},
		"claimed work":    {Status: "in_progress", Assignees: []string{"sess-1"}, TierMode: TierBoth, Sort: SortCreatedDesc},
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			sqlText, args := sqliteListSQL(q, "b.id")
			if !strings.Contains(sqlText, "b.assignee IN (") {
				t.Fatalf("plural assignees not pushed into SQL:\n  sql: %s", sqlText)
			}
			for _, assignee := range q.Assignees {
				if !slices.Contains(args, any(assignee)) {
					t.Fatalf("assignee %q not bound as an argument: %v", assignee, args)
				}
			}
		})
	}
}

// TestSQLiteListPluralAssigneesKeepMetadataDriving pins that a metadata list
// with plural assignees still drives off the metadata index: the assignee
// predicate is a residual check there, as the other column filters are
// (TestSQLiteListMetadataQueriesDriveOffMetadataIndex).
func TestSQLiteListPluralAssigneesKeepMetadataDriving(t *testing.T) {
	s := newSQLiteGraphApplyStore(t, t.TempDir(), WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	q := ListQuery{Metadata: map[string]string{"gc.source_bead_id": "mc-5495l"}, Assignees: []string{"a", "b"}, Status: "open"}
	sqlText, args := sqliteListSQL(q, "b.id")
	if !strings.Contains(sqlText, "+b.assignee IN (") {
		t.Fatalf("plural assignees beside metadata must be residual:\n  sql: %s", sqlText)
	}
	plan := sqliteExplainPlan(t, s, sqlText, args)
	if !strings.Contains(plan, "idx_metadata_key_value") || strings.Contains(plan, "idx_beads_assignee") {
		t.Fatalf("metadata list with assignees does not drive off the metadata index:\n  sql: %s\n  plan: %s", sqlText, plan)
	}
}

// TestSQLiteListPluralAssigneesAnswerUnchanged pins that pushing the
// assignees down changes no answer: every shape returns exactly the rows a
// full scan filtered by ListQuery.Matches returns, in the same order. The
// fixture covers what exact matching must keep apart: an empty assignee
// (matched only by an explicit ""), padded and case-variant assignees, other
// statuses, types and tiers.
func TestSQLiteListPluralAssigneesAnswerUnchanged(t *testing.T) {
	s := newSQLiteGraphApplyStore(t, t.TempDir())
	base := time.Unix(1_700_000_000, 0).UTC()
	fixture := []Bead{
		{Title: "mail to session", Type: "message", Assignee: "sess-1"},
		{Title: "mail to alias", Type: "message", Assignee: "alias-1"},
		{Title: "mail to other", Type: "message", Assignee: "other"},
		{Title: "mail padded", Type: "message", Assignee: " sess-1"},
		{Title: "mail case", Type: "message", Assignee: "SESS-1"},
		{Title: "mail unassigned", Type: "message"},
		{Title: "closed mail", Type: "message", Assignee: "sess-1", Status: "closed"},
		{Title: "wisp mail", Type: "message", Assignee: "sess-1", Ephemeral: true},
		{Title: "molecule", Type: "molecule", Assignee: "sess-1", Status: "in_progress"},
		{Title: "older molecule", Type: "molecule", Assignee: "name-1", Status: "in_progress"},
		{Title: "task", Type: "task", Assignee: "sess-1", Status: "in_progress"},
		{Title: "unassigned task", Type: "task", Status: "in_progress"},
	}
	for i, b := range fixture {
		b.CreatedAt = base.Add(time.Duration(i) * time.Second)
		status := b.Status
		b.Status = ""
		created, err := s.Create(b)
		if err != nil {
			t.Fatalf("Create %q: %v", b.Title, err)
		}
		switch status {
		case "closed":
			if err := s.Close(created.ID); err != nil {
				t.Fatalf("Close %q: %v", b.Title, err)
			}
		case "in_progress":
			st := "in_progress"
			if err := s.Update(created.ID, UpdateOpts{Status: &st}); err != nil {
				t.Fatalf("Update %q: %v", b.Title, err)
			}
		}
	}
	shapes := map[string]ListQuery{
		"mailbox":              {Type: "message", Status: "open", TierMode: TierBoth, Live: true, Assignees: []string{"sess-1", "alias-1"}},
		"mailbox main tier":    {Type: "message", Status: "open", Assignees: []string{"sess-1", "alias-1"}},
		"mailbox wisp tier":    {Type: "message", Status: "open", TierMode: TierWisps, Assignees: []string{"sess-1"}},
		"explicit empty":       {Type: "message", Status: "open", TierMode: TierBoth, Assignees: []string{""}},
		"include closed":       {Type: "message", IncludeClosed: true, TierMode: TierBoth, Assignees: []string{"sess-1"}},
		"active molecule":      {Status: "in_progress", Type: "molecule", Assignees: []string{"sess-1", "name-1"}, TierMode: TierBoth, Limit: 5, Sort: SortCreatedDesc},
		"one molecule":         {Status: "in_progress", Type: "molecule", Assignees: []string{"sess-1", "name-1"}, TierMode: TierBoth, Limit: 1, Sort: SortCreatedDesc},
		"claimed work":         {Status: "in_progress", Assignees: []string{"sess-1", "name-1"}, TierMode: TierBoth, Sort: SortCreatedDesc},
		"claimed oldest first": {Status: "in_progress", Assignees: []string{"name-1", "sess-1"}, TierMode: TierBoth, Sort: SortCreatedAsc},
		"no match":             {Status: "open", Assignees: []string{"nobody"}, TierMode: TierBoth},
	}
	all, err := s.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth, Sort: SortCreatedAsc})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	for name, q := range shapes {
		t.Run(name, func(t *testing.T) {
			got, err := s.List(q)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			var want []Bead
			for _, b := range all {
				if q.Matches(b) {
					want = append(want, b)
				}
			}
			sortBeadsForQuery(want, q.Sort)
			if q.Limit > 0 && len(want) > q.Limit {
				want = want[:q.Limit]
			}
			if gotIDs, wantIDs := beadIDsOf(got), beadIDsOf(want); !slices.Equal(gotIDs, wantIDs) {
				t.Fatalf("List(%+v) = %v, want %v", q, gotIDs, wantIDs)
			}
		})
	}
}

func sqliteExplainPlan(t *testing.T, s *SQLiteStore, sqlText string, args []any) string {
	t.Helper()
	rows, err := s.readDB.Query("EXPLAIN QUERY PLAN "+sqlText, args...)
	if err != nil {
		t.Fatalf("explain %s: %v", sqlText, err)
	}
	defer rows.Close() //nolint:errcheck
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, detail)
	}
	return strings.Join(plan, " | ")
}

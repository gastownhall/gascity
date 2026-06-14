//go:build gascity_native_beads

package beads

import (
	"context"
	"fmt"
	"strings"
)

// Count implements the optional Counter capability for the DoltLite read
// store. It returns the exact number of beads List would return for query
// (minus beads whose Type is in excludeTypes) using a hydration-free
// SELECT COUNT(*): no rows are scanned into Bead values, no metadata JSON is
// parsed, and no per-row label subquery runs. This backs bounded reads that
// need an accurate total without materializing full closed history
// (gascity#3253), mirroring the hydration-free status counter from #3211.
//
// Count answers only the query shapes it can satisfy exactly with column and
// EXISTS predicates. The read path narrows metadata queries with approximate
// LIKE matching and applies the exact match in Go, so a metadata query cannot
// be counted exactly in SQL and returns ErrCountUnsupported. The wisp and
// both tiers also return ErrCountUnsupported because a union count would
// double-count ids that List dedupes, and CreatedBefore/ParentID filters
// return ErrCountUnsupported because List applies them with Go-side semantics a
// single COUNT cannot reproduce. Limited queries are excluded because the
// Counter contract is List cardinality parity, not full-result total
// cardinality. UpdatedBefore is also excluded, but as an over-conservative
// exclusion pending cleanup of the duplicate SQL/Go filter: queryIssueTable
// already emits an exact COALESCE(updated_at, created_at) predicate for it, so
// a COUNT could reproduce it — the redundant Go-side re-filter is what
// currently keeps it out. Callers fall back to List for those shapes, exactly
// as the Counter contract specifies.
func (s *DoltliteReadStore) Count(ctx context.Context, query ListQuery, excludeTypes ...string) (int, error) {
	if err := query.Validate(); err != nil {
		return 0, err
	}
	if !query.HasFilter() && !query.AllowScan {
		return 0, fmt.Errorf("bd count: %w", ErrQueryRequiresScan)
	}
	if !doltliteCountSupported(query) {
		return 0, fmt.Errorf("bd count: %w", ErrCountUnsupported)
	}
	tables := doltliteIssueTables
	where, args := doltliteCountWhere(query, tables, excludeTypes)
	sqlText := "SELECT COUNT(*) FROM " + tables.issues + " i"
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, sqlText, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("bd count: %w", err)
	}
	return n, nil
}

// doltliteCountSupported reports whether Count can answer query exactly.
func doltliteCountSupported(query ListQuery) bool {
	if len(query.Metadata) > 0 {
		return false
	}
	if query.TierMode != TierIssues {
		return false
	}
	if query.ParentID != "" {
		return false
	}
	if !query.CreatedBefore.IsZero() || !query.UpdatedBefore.IsZero() {
		return false
	}
	if query.Limit > 0 {
		return false
	}
	return true
}

// doltliteCountWhere builds the SELECT COUNT(*) predicates for the supported
// query shapes. It mirrors queryIssueTable's column predicates exactly for the
// fields it covers; doltliteCountSupported gates out everything else, and
// TestDoltliteCountMatchesList asserts the two paths agree across shapes.
func doltliteCountWhere(query ListQuery, tables doltliteTableSet, excludeTypes []string) ([]string, []any) {
	where := make([]string, 0, 6)
	args := make([]any, 0, 6)
	if !query.IncludeClosed && query.Status != "closed" {
		where = append(where, "i.status != 'closed'")
	}
	if query.Status != "" {
		where = append(where, "i.status = ?")
		args = append(args, query.Status)
	}
	if query.Type != "" {
		where = append(where, "i.issue_type = ?")
		args = append(args, query.Type)
	}
	if len(excludeTypes) > 0 {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(excludeTypes)), ",")
		where = append(where, "COALESCE(i.issue_type, '') NOT IN ("+placeholders+")")
		for _, t := range excludeTypes {
			args = append(args, t)
		}
	}
	if query.Assignee != "" {
		where = append(where, "i.assignee = ?")
		args = append(args, query.Assignee)
	}
	if len(query.Assignees) > 0 {
		assignees := compactStrings(query.Assignees)
		if len(assignees) == 0 {
			// queryIssueTable returns no rows for an all-empty assignee set;
			// match it with a predicate that selects nothing.
			where = append(where, "1 = 0")
		} else {
			placeholders := strings.TrimRight(strings.Repeat("?,", len(assignees)), ",")
			where = append(where, "i.assignee IN ("+placeholders+")")
			for _, assignee := range assignees {
				args = append(args, assignee)
			}
		}
	}
	if query.Label != "" {
		where = append(where, "EXISTS (SELECT 1 FROM "+tables.labels+" l WHERE l.issue_id = i.id AND l.label = ?)")
		args = append(args, query.Label)
	}
	return where, args
}

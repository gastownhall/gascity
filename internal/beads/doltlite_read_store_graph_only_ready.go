//go:build gascity_native_beads

package beads

import (
	"sort"
	"strings"
)

// ReadyGraphOnly implements GraphOnlyReadyStore. It unconditionally queries the
// wisp table set with TierWisps, ignoring any TierMode the caller supplies.
// Molecule steps and wisps are the only bead types a controller dispatch loop
// executes, so the normal readyExcludeTypes filter is not applied here.
// Blocking dependencies in wisp_dependencies gate results: a wisp whose
// depends_on_wisp_id target is still open is excluded.
func (s *DoltliteReadStore) ReadyGraphOnly(query ...ReadyQuery) ([]Bead, error) {
	rq := readyQueryFromArgs(query)
	rq.TierMode = TierWisps

	q := ListQuery{
		Status:        "open",
		AllowScan:     true,
		IncludeClosed: false,
		SkipLabels:    true,
		TierMode:      TierWisps,
	}
	if rq.Assignee != "" {
		q.Assignee = rq.Assignee
	}
	if rq.Limit > 0 {
		q.Limit = rq.Limit
	}
	depWhere, depArgs := doltliteWispDepGate()
	return s.queryIssuesOrderedInTables(q, []doltliteTableSet{doltliteWispTables}, depWhere, depArgs, q.Limit, "ORDER BY COALESCE(i.priority, 2) ASC, i.created_at ASC, i.id ASC")
}

// doltliteWispDepGate returns the blocking-dependency predicate for the wisp
// table set. Unlike doltliteReadyIssueWhere it omits the type-exclusion filter,
// because ReadyGraphOnly returns all wisp types including molecule and step beads
// that the human-backlog Ready() excludes.
//
// A wisp_dependencies row can point at either another wisp (depends_on_wisp_id)
// or a durable issue (depends_on_issue_id / depends_on_id / depends_on_external).
// Both arms must resolve — mirroring doltliteReadyIssueWhere's two-arm CASE —
// or a durable-issue-shaped dependency never matches its blocker join and the
// wisp is excluded permanently, even after the blocker closes (ga-rh2mgk).
func doltliteWispDepGate() (string, []any) {
	blockingTypes := make([]string, 0, len(readyBlockingDependencyTypes))
	for typ := range readyBlockingDependencyTypes {
		blockingTypes = append(blockingTypes, typ)
	}
	sort.Strings(blockingTypes)
	blockingPlaceholders := strings.TrimRight(strings.Repeat("?,", len(blockingTypes)), ",")
	args := make([]any, 0, len(blockingTypes))
	for _, typ := range blockingTypes {
		args = append(args, typ)
	}
	issueTarget := "COALESCE(NULLIF(d.depends_on_issue_id, ''), NULLIF(d.depends_on_id, ''), NULLIF(d.depends_on_external, ''), '')"
	wispTarget := "NULLIF(d.depends_on_wisp_id, '')"
	depType := "COALESCE(NULLIF(d.type, ''), 'blocks')"
	blockerJoins := "LEFT JOIN " + doltliteIssueTables.issues + " blocker_issue ON blocker_issue.id = " + issueTarget +
		"\n\t\tLEFT JOIN " + doltliteWispTables.issues + " blocker_wisp ON blocker_wisp.id = " + wispTarget
	blockerStatus := "CASE WHEN " + wispTarget + " IS NOT NULL THEN COALESCE(blocker_wisp.status, '') ELSE COALESCE(blocker_issue.status, '') END"
	return `NOT EXISTS (
		SELECT 1 FROM ` + doltliteWispTables.deps + ` d
		` + blockerJoins + `
		WHERE d.issue_id = i.id AND ` + depType + ` IN (` + blockingPlaceholders + `) AND ` + blockerStatus + ` != 'closed'
	)`, args
}

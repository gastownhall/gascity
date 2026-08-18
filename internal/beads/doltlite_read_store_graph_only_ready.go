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
	wispTarget := "NULLIF(d.depends_on_wisp_id, '')"
	depType := "COALESCE(NULLIF(d.type, ''), 'blocks')"
	blockerJoin := "LEFT JOIN " + doltliteWispTables.issues + " blocker ON blocker.id = " + wispTarget
	return `NOT EXISTS (
		SELECT 1 FROM ` + doltliteWispTables.deps + ` d
		` + blockerJoin + `
		WHERE d.issue_id = i.id AND ` + depType + ` IN (` + blockingPlaceholders + `) AND COALESCE(blocker.status, '') != 'closed'
	)`, args
}

package runproj

import "github.com/gastownhall/gascity/internal/beads"

// BuildRunLane builds the run lane for the single run rooted at rootID, off the
// same fold BuildRunSummary consumes. It exists so a caller can resolve ONE run
// (e.g. GET /runs/{id}) without the historical-lane cap BuildRunSummary applies
// to its list output: a completed run beyond the newest-50 is still resolvable
// here. ok is false when rootID is empty, absent, or not a run group.
//
// The lane is identical to the one BuildRunSummary would place in its buckets for
// this root: the same grouping (by runRootID), the same run-group and
// dangling-root filtering, and the same runLane projection. Feed-scope fallback
// is not applied (the list path's feedScopes are a summary-level input); a run
// whose scope resolves only via a feed scope reports scope "unavailable" here.
func BuildRunLane(beadList []beads.Bead, rootID string) (RunLane, bool) {
	if rootID == "" {
		return RunLane{}, false
	}
	var group []runIssue
	for i := range beadList {
		issue := fromBead(beadList[i])
		if runRootID(issue) == rootID {
			group = append(group, issue)
		}
	}
	if len(group) == 0 || isDanglingRootGroup(rootID, group) || !isRunGroup(rootID, group) {
		return RunLane{}, false
	}
	return runLane(rootID, group, map[string]RunFeedScope{}), true
}

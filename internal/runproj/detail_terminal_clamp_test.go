package runproj

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestTerminalRootClampMappings pins the presentation-clamp mapping for every
// terminal root class against every non-terminal step status, plus the
// already-terminal preservation rule. This is the direct-mapping oracle the
// end-to-end tests below exercise through the full pipeline.
func TestTerminalRootClampMappings(t *testing.T) {
	cases := []struct {
		rootStatus string
		step       string
		want       string
	}{
		// Completed root: unfinished steps complete, never-started steps skip.
		{"completed", "active", "completed"},
		{"completed", "running", "completed"},
		{"completed", "ready", "completed"},
		{"completed", "blocked", "completed"},
		{"completed", "pending", "skipped"},
		// Failed root: unfinished steps cancel, never-started steps skip.
		{"failed", "active", "canceled"},
		{"failed", "running", "canceled"},
		{"failed", "ready", "canceled"},
		{"failed", "blocked", "canceled"},
		{"failed", "pending", "skipped"},
		// Canceled root behaves like failed.
		{"canceled", "active", "canceled"},
		{"canceled", "ready", "canceled"},
		{"canceled", "pending", "skipped"},
		// Already-terminal step statuses are never altered under any terminal root.
		{"completed", "completed", "completed"},
		{"completed", "failed", "failed"},
		{"completed", "skipped", "skipped"},
		{"completed", "canceled", "canceled"},
		{"completed", "done", "done"},
		{"failed", "completed", "completed"},
		{"failed", "failed", "failed"},
		{"canceled", "skipped", "skipped"},
		// Non-terminal roots never clamp anything.
		{"active", "active", "active"},
		{"ready", "pending", "pending"},
		{"blocked", "active", "active"},
		{"pending", "ready", "ready"},
	}
	for _, tc := range cases {
		t.Run(tc.rootStatus+"/"+tc.step, func(t *testing.T) {
			got := clampForRootStatus(tc.rootStatus).apply(tc.step)
			if got != tc.want {
				t.Errorf("clampForRootStatus(%q).apply(%q) = %q, want %q", tc.rootStatus, tc.step, got, tc.want)
			}
		})
	}
}

// TestTerminalRootClampMappingsCoverTaxonomy proves the mapping is total: every
// non-terminal step status maps to a terminal status under a completed root, and
// every terminal step status is preserved — so the clamp can never leave a node in
// a non-terminal state (and never invents a status outside the taxonomy).
func TestTerminalRootClampMappingsCoverTaxonomy(t *testing.T) {
	clamp := clampForRootStatus("completed")
	for _, status := range allRunNodeStatuses {
		got := clamp.apply(status)
		if !isTerminalRunNodeStatus(got) {
			t.Errorf("apply(%q) = %q, which is not terminal — a terminal root must leave no non-terminal step", status, got)
		}
		if isTerminalRunNodeStatus(status) && got != status {
			t.Errorf("apply(%q) = %q, but an already-terminal status must be preserved", status, got)
		}
	}
}

// TestRootClampForResolvesBeadStatus proves rootClampFor keys off the root bead's
// presentation status (not its raw status): a closed root activates a completed
// clamp, a closed+gc.outcome=fail root activates a canceled clamp, an open root
// stays inactive, and an absent (nil) root stays inactive.
func TestRootClampForResolvesBeadStatus(t *testing.T) {
	completed := runSnapshotBead{status: "closed"}
	if c := rootClampFor(&completed); !c.active || c.nonPendingTarget != "completed" {
		t.Errorf("closed root: clamp = %+v, want active completed", c)
	}

	failed := runSnapshotBead{status: "closed", metadata: map[string]string{beadmeta.OutcomeMetadataKey: "fail"}}
	if c := rootClampFor(&failed); !c.active || c.nonPendingTarget != "canceled" {
		t.Errorf("closed+outcome=fail root: clamp = %+v, want active canceled", c)
	}

	open := runSnapshotBead{status: "open"}
	if c := rootClampFor(&open); c.active {
		t.Errorf("open root: clamp = %+v, want inactive", c)
	}

	if c := rootClampFor(nil); c.active {
		t.Errorf("absent root: clamp = %+v, want inactive (no change)", c)
	}
}

// TestBuildRunDetailTerminalClampProductionShape reproduces the reported incident:
// a completed merge-queue run whose 7 in-progress steps and 1 open step lost their
// close events. Before the clamp the detail rendered 7 "active" + 1 "pending" step
// under a completed root (statusCounts {completed:1, active:7, pending:1}); after
// it, every step reads completed/skipped and the run reports terminal.
func TestBuildRunDetailTerminalClampProductionShape(t *testing.T) {
	beadList := []beads.Bead{clampRootBead("runq", "closed", nil)}
	for i := 1; i <= 7; i++ {
		beadList = append(beadList, clampStepBead("runq", fmt.Sprintf("runq.%d", i), fmt.Sprintf("s%d", i), "in_progress", nil))
	}
	beadList = append(beadList, clampStepBead("runq", "runq.8", "s8", "open", nil))

	detail, err := BuildRunDetail(beadList, "runq", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}

	statuses := nodeStatusByID(detail)
	if statuses["runq"] != "completed" {
		t.Errorf("root node status = %q, want completed", statuses["runq"])
	}
	for i := 1; i <= 7; i++ {
		id := fmt.Sprintf("s%d", i)
		if statuses[id] != "completed" {
			t.Errorf("step %q status = %q, want completed (was in_progress under a completed root)", id, statuses[id])
		}
	}
	if statuses["s8"] != "skipped" {
		t.Errorf("open step s8 status = %q, want skipped (never started under a completed root)", statuses["s8"])
	}

	// statusCounts is computed post-clamp: root + 7 steps completed, 1 skipped.
	assertNoLiveStatuses(t, detail)
	sc := detail.Progress.StatusCounts.counts
	if sc["completed"] != 8 || sc["skipped"] != 1 || len(sc) != 2 {
		t.Errorf("progress.statusCounts = %v, want {completed:8, skipped:1}", sc)
	}
	if !detail.Progress.Terminal {
		t.Error("progress.terminal = false, want true — every visible node is terminal after the clamp")
	}
}

// TestBuildRunDetailTerminalClampPreservesRecordedOutcomes proves the clamp never
// overwrites a real recorded step outcome under a completed root: a
// gc.outcome=fail step stays failed, a skipped step stays skipped, a canceled step
// stays canceled, while a still-running step collapses to completed.
func TestBuildRunDetailTerminalClampPreservesRecordedOutcomes(t *testing.T) {
	beadList := []beads.Bead{
		clampRootBead("runp", "closed", nil),
		clampStepBead("runp", "runp.1", "won", "in_progress", nil),
		clampStepBead("runp", "runp.2", "lost", "closed", map[string]string{beadmeta.OutcomeMetadataKey: "fail"}),
		clampStepBead("runp", "runp.3", "dropped", "closed", map[string]string{beadmeta.OutcomeMetadataKey: "skipped"}),
		clampStepBead("runp", "runp.4", "aborted", "closed", map[string]string{beadmeta.OutcomeMetadataKey: "canceled"}),
	}

	detail, err := BuildRunDetail(beadList, "runp", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}

	statuses := nodeStatusByID(detail)
	want := map[string]string{"won": "completed", "lost": "failed", "dropped": "skipped", "aborted": "canceled"}
	for id, wantStatus := range want {
		if statuses[id] != wantStatus {
			t.Errorf("step %q status = %q, want %q", id, statuses[id], wantStatus)
		}
	}
}

// TestBuildRunDetailTerminalClampCannotResurrectActive proves the instance-level
// clamp runs before aggregateStatus: a semantic node with several physical
// instances (one still in_progress) folds to completed, never back to "active".
func TestBuildRunDetailTerminalClampCannotResurrectActive(t *testing.T) {
	beadList := []beads.Bead{
		clampRootBead("runr", "closed", nil),
		// Three physical beads share one logical step (a retried loop node).
		clampStepBead("runr", "runr.1", "loop", "closed", map[string]string{beadmeta.AttemptMetadataKey: "1"}),
		clampStepBead("runr", "runr.2", "loop", "closed", map[string]string{beadmeta.AttemptMetadataKey: "2"}),
		clampStepBead("runr", "runr.3", "loop", "in_progress", map[string]string{beadmeta.AttemptMetadataKey: "3"}),
	}

	detail, err := BuildRunDetail(beadList, "runr", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}

	node, ok := nodeByID(detail, "loop")
	if !ok {
		t.Fatal("loop node not found")
	}
	if node.Status != "completed" {
		t.Errorf("aggregated loop node status = %q, want completed", node.Status)
	}
	if len(node.ExecutionInstances) != 3 {
		t.Fatalf("loop node has %d instances, want 3", len(node.ExecutionInstances))
	}
	assertNoLiveStatuses(t, detail)
	// A terminal run has no running attempt.
	if node.AttemptSummary.Kind == "tracked" && node.AttemptSummary.Active.Kind == "running" {
		t.Errorf("attemptSummary.active = running, want idle under a completed root")
	}
}

// TestBuildRunDetailFailedRootClampsToCanceled proves a failed root cancels its
// unfinished steps (rather than completing them) and skips its never-started ones.
func TestBuildRunDetailFailedRootClampsToCanceled(t *testing.T) {
	beadList := []beads.Bead{
		clampRootBead("runf", "closed", map[string]string{beadmeta.OutcomeMetadataKey: "fail"}),
		clampStepBead("runf", "runf.1", "midflight", "in_progress", nil),
		clampStepBead("runf", "runf.2", "never", "open", nil),
		clampStepBead("runf", "runf.3", "finished", "closed", nil),
	}

	detail, err := BuildRunDetail(beadList, "runf", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}

	statuses := nodeStatusByID(detail)
	want := map[string]string{"runf": "failed", "midflight": "canceled", "never": "skipped", "finished": "completed"}
	for id, wantStatus := range want {
		if statuses[id] != wantStatus {
			t.Errorf("node %q status = %q, want %q", id, statuses[id], wantStatus)
		}
	}
	assertNoLiveStatuses(t, detail)
}

// TestBuildRunDetailOpenRootDoesNotClamp proves the clamp is inert for a
// non-terminal root: an open-root run keeps its steps' live statuses, so healthy
// in-flight runs are unchanged.
func TestBuildRunDetailOpenRootDoesNotClamp(t *testing.T) {
	beadList := []beads.Bead{
		clampRootBead("runo", "open", nil),
		clampStepBead("runo", "runo.1", "working", "in_progress", nil),
		clampStepBead("runo", "runo.2", "waiting", "open", nil),
	}

	detail, err := BuildRunDetail(beadList, "runo", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}

	statuses := nodeStatusByID(detail)
	if statuses["working"] != "active" {
		t.Errorf("in-progress step under an open root = %q, want active (no clamp)", statuses["working"])
	}
	if detail.Progress.Terminal {
		t.Error("progress.terminal = true, want false — an open root is not terminal")
	}
}

// TestBuildRunDetailTerminalClampKeepsSessionLink proves the session interaction:
// a clamped-completed step keeps the session link it earned while running (the
// link is resolved from the step's real recorded status), but is not streamable
// (nothing streams under a terminal root).
func TestBuildRunDetailTerminalClampKeepsSessionLink(t *testing.T) {
	beadList := []beads.Bead{
		clampRootBead("runs", "closed", nil),
		clampStepBead("runs", "runs.1", "review", "in_progress", map[string]string{
			beadmeta.SessionIDMetadataKey:   "gc-sess01",
			beadmeta.SessionNameMetadataKey: "worker-1",
		}),
	}

	detail, err := BuildRunDetail(beadList, "runs", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}

	node, ok := nodeByID(detail, "review")
	if !ok {
		t.Fatal("review node not found")
	}
	if node.Status != "completed" {
		t.Errorf("review node status = %q, want completed", node.Status)
	}
	if len(node.ExecutionInstances) != 1 {
		t.Fatalf("review node has %d instances, want 1", len(node.ExecutionInstances))
	}
	inst := node.ExecutionInstances[0]
	if inst.Session.Kind != "attached" {
		t.Fatalf("clamped-completed step session kind = %q, want attached (link preserved)", inst.Session.Kind)
	}
	if inst.Session.Link.SessionID != "gc-sess01" {
		t.Errorf("session link id = %q, want gc-sess01", inst.Session.Link.SessionID)
	}
	if inst.Session.Streamable {
		t.Error("clamped-completed step is streamable, want false — a terminal run streams nothing")
	}
}

// clampRootBead builds a graph.v2 run root bead for the clamp tests.
func clampRootBead(id, status string, extra map[string]string) beads.Bead {
	md := map[string]string{
		beadmeta.FormulaContractMetadataKey: "graph.v2",
		beadmeta.KindMetadataKey:            "run",
		beadmeta.FormulaMetadataKey:         "mol-adopt-pr-v2",
		beadmeta.RunTargetMetadataKey:       "rig:demo",
		beadmeta.RootStoreRefMetadataKey:    "rig:demo",
		beadmeta.ScopeKindMetadataKey:       "rig",
		beadmeta.ScopeRefMetadataKey:        "demo",
	}
	for k, v := range extra {
		md[k] = v
	}
	return beads.Bead{ID: id, Type: "molecule", Status: status, Metadata: md}
}

// clampStepBead builds a graph.v2 run step bead rooted at rootID.
func clampStepBead(rootID, id, stepID, status string, extra map[string]string) beads.Bead {
	md := map[string]string{
		beadmeta.KindMetadataKey:       "step",
		beadmeta.RootBeadIDMetadataKey: rootID,
		beadmeta.StepIDMetadataKey:     stepID,
		beadmeta.StepRefMetadataKey:    "mol-adopt-pr-v2." + stepID,
	}
	for k, v := range extra {
		md[k] = v
	}
	return beads.Bead{ID: id, Type: "task", Status: status, Metadata: md}
}

// nodeByID finds a display node by its semantic id.
func nodeByID(detail FormulaRunDetail, id string) (RunDisplayNode, bool) {
	for _, node := range detail.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return RunDisplayNode{}, false
}

// nodeStatusByID maps each display node's semantic id to its status.
func nodeStatusByID(detail FormulaRunDetail) map[string]string {
	out := make(map[string]string, len(detail.Nodes))
	for _, node := range detail.Nodes {
		out[node.ID] = node.Status
	}
	return out
}

// assertNoLiveStatuses fails if any node, execution instance, or control badge
// still presents as running after a clamp — the invariant a terminal root must
// guarantee across the whole DAG.
func assertNoLiveStatuses(t *testing.T, detail FormulaRunDetail) {
	t.Helper()
	for _, node := range detail.Nodes {
		if isRunningStatus(node.Status) {
			t.Errorf("node %q is %q after clamp, want a terminal status", node.ID, node.Status)
		}
		for _, inst := range node.ExecutionInstances {
			if isRunningStatus(inst.Status) {
				t.Errorf("instance %q of node %q is %q after clamp, want a terminal status", inst.ID, node.ID, inst.Status)
			}
		}
		for _, badge := range node.ControlBadges {
			if isRunningStatus(badge.Status) {
				t.Errorf("control badge %q of node %q is %q after clamp, want a terminal status", badge.ID, node.ID, badge.Status)
			}
		}
	}
}

// TestMapRunPhaseClosedRootWinsOverLingeringMembers proves root-terminality reaches
// the phase classifier: a closed root whose members never recorded their closes
// (one in_progress, one raw-blocked) still maps to phase "complete".
func TestMapRunPhaseClosedRootWinsOverLingeringMembers(t *testing.T) {
	issues := []runIssue{
		{id: "root", title: "mol-adopt-pr-v2", status: "closed", metadata: map[string]string{beadmeta.KindMetadataKey: "run"}},
		{id: "root.1", status: "in_progress", parent: "root", metadata: map[string]string{beadmeta.StepIDMetadataKey: "review-loop"}},
		{id: "root.2", status: "blocked", parent: "root", metadata: map[string]string{beadmeta.StepIDMetadataKey: "human-approval"}},
	}
	got := mapRunPhase("root", issues)
	if got.phase != "complete" {
		t.Errorf("phase = %q, want complete (closed root beats lingering members)", got.phase)
	}
	if got.label != "complete" {
		t.Errorf("label = %q, want complete (no fail outcome)", got.label)
	}
}

// TestMapRunPhaseClosedRootFailOutcomeLabelsFailed proves the honest failure label
// rides a closed root with gc.outcome=fail even while a member lingers open.
func TestMapRunPhaseClosedRootFailOutcomeLabelsFailed(t *testing.T) {
	issues := []runIssue{
		{id: "root", status: "closed", metadata: map[string]string{beadmeta.OutcomeMetadataKey: "fail"}},
		{id: "root.1", status: "in_progress", parent: "root"},
	}
	if got := mapRunPhase("root", issues); got.phase != "complete" || got.label != "failed" {
		t.Errorf("mapRunPhase = %+v, want phase complete label failed", got)
	}
}

// TestMapRunPhaseRootlessGroupUnchanged proves the root-closed branch does not fire
// when no issue matches rootID: the all-closed fallback still resolves a rootless
// terminal group, and a rootless in-progress group is not forced terminal.
func TestMapRunPhaseRootlessGroupUnchanged(t *testing.T) {
	closedRootless := []runIssue{
		{id: "orphan.1", status: "closed"},
		{id: "orphan.2", status: "closed"},
	}
	if got := mapRunPhase("missing-root", closedRootless); got.phase != "complete" {
		t.Errorf("rootless all-closed phase = %q, want complete (allClosed fallback)", got.phase)
	}
	openRootless := []runIssue{
		{id: "orphan.1", status: "in_progress", metadata: map[string]string{beadmeta.StepIDMetadataKey: "preflight"}},
	}
	if got := mapRunPhase("missing-root", openRootless); got.phase == "complete" {
		t.Errorf("rootless in-progress phase = %q, want non-complete (unchanged)", got.phase)
	}
}

// TestBuildRunDetailTerminalRootPhaseAndStagesComplete proves the detail payload is
// internally consistent under the incident shape: a closed root reports phase
// "complete" with NO stage reading active or blocked, so Phase/Stages no longer
// contradict the clamped DAG.
func TestBuildRunDetailTerminalRootPhaseAndStagesComplete(t *testing.T) {
	beadList := []beads.Bead{
		clampRootBead("runq", "closed", nil),
		clampStepBead("runq", "runq.1", "preflight", "closed", nil),
		clampStepBead("runq", "runq.2", "review-loop", "in_progress", nil),
		clampStepBead("runq", "runq.3", "human-approval", "blocked", nil),
		clampStepBead("runq", "runq.4", "finalize", "open", nil),
	}
	detail, err := BuildRunDetail(beadList, "runq", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	if detail.Phase != "complete" {
		t.Errorf("detail.Phase = %q, want complete (closed root)", detail.Phase)
	}
	if len(detail.Stages) == 0 {
		t.Fatal("detail.Stages is empty; expected the adopt-pr ladder")
	}
	for _, st := range detail.Stages {
		if st.Status == "active" || st.Status == "blocked" {
			t.Errorf("stage %q status = %q, want no active/blocked stage under a closed root", st.Key, st.Status)
		}
	}
	if !detail.Progress.Terminal {
		t.Error("progress.terminal = false, want true")
	}
}

// TestBuildRunSummaryClosedRootBucketsHistorical proves the runs LIST no longer
// strands a lost-close terminal run in the active/blocked buckets, and its lane
// progress no longer reports active_step.
func TestBuildRunSummaryClosedRootBucketsHistorical(t *testing.T) {
	beadList := []beads.Bead{
		clampRootBead("runh", "closed", nil),
		clampStepBead("runh", "runh.1", "preflight", "closed", nil),
		clampStepBead("runh", "runh.2", "review-loop", "in_progress", nil), // lost close
	}
	summary := BuildRunSummary(beadList)

	if laneInGroup(summary.Lanes, "runh") || laneInGroup(summary.BlockedLanes, "runh") {
		t.Error("closed-root run appears in the active/blocked buckets, want historical")
	}
	lane, ok := findLaneInGroup(summary.HistoricalLanes, "runh")
	if !ok {
		t.Fatal("closed-root run not found in historical lanes")
	}
	if lane.Phase != "complete" {
		t.Errorf("historical lane phase = %q, want complete", lane.Phase)
	}
	if lane.Progress.Status == "active_step" {
		t.Errorf("historical lane progress = active_step, want a non-active status under a closed root")
	}
}

// TestBuildRunDetailClampsControlBadges proves R2: a run-finalize hidden construct
// still reading in_progress attaches a badge to the root node, and under a closed
// root that badge clamps to completed instead of rendering "running".
func TestBuildRunDetailClampsControlBadges(t *testing.T) {
	beadList := []beads.Bead{
		clampRootBead("runb", "closed", nil),
		clampStepBead("runb", "runb.1", "preflight", "closed", nil),
		clampStepBead("runb", "runb.fin", "finalize", "in_progress", map[string]string{beadmeta.KindMetadataKey: "run-finalize"}),
	}
	detail, err := BuildRunDetail(beadList, "runb", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	root, ok := nodeByID(detail, "runb")
	if !ok {
		t.Fatal("root node runb not found")
	}
	badge, ok := badgeByLabel(root.ControlBadges, "finalize")
	if !ok {
		t.Fatalf("finalize badge not attached to root; badges=%+v", root.ControlBadges)
	}
	if badge.Status != "completed" {
		t.Errorf("finalize badge status = %q, want completed (clamped under a closed root)", badge.Status)
	}
	assertNoLiveStatuses(t, detail)
}

// TestBuildRunDetailControlBadgeClampInactiveNoOp proves the badge clamp is a strict
// no-op under an open (non-terminal) root: the finalize badge keeps its live status.
func TestBuildRunDetailControlBadgeClampInactiveNoOp(t *testing.T) {
	beadList := []beads.Bead{
		clampRootBead("runc", "open", nil),
		clampStepBead("runc", "runc.1", "preflight", "in_progress", nil),
		clampStepBead("runc", "runc.fin", "finalize", "in_progress", map[string]string{beadmeta.KindMetadataKey: "run-finalize"}),
	}
	detail, err := BuildRunDetail(beadList, "runc", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	root, ok := nodeByID(detail, "runc")
	if !ok {
		t.Fatal("root node runc not found")
	}
	badge, ok := badgeByLabel(root.ControlBadges, "finalize")
	if !ok {
		t.Fatalf("finalize badge not attached to root; badges=%+v", root.ControlBadges)
	}
	if badge.Status != "active" {
		t.Errorf("finalize badge status = %q, want active (inactive clamp is a strict no-op)", badge.Status)
	}
}

// TestTerminalStageLadderKeepsUnmaterializedStagesPending proves D1: a terminal
// run's stage ladder marks only the stages that RAN complete. An all-closed run
// that exited early keeps its never-materialized trailing stages pending (no false
// "Human approval / Merge-ready complete"), while a started-but-lost-close stage
// still reads complete.
func TestTerminalStageLadderKeepsUnmaterializedStagesPending(t *testing.T) {
	// mol-bug-report-flow-v2 closed at classify: intake/repro/audit/classify ran to
	// close; approval/publish/dispatch never materialized.
	early := []beads.Bead{
		clampRootBead("runm", "closed", map[string]string{beadmeta.FormulaMetadataKey: "mol-bug-report-flow-v2"}),
		clampStepBead("runm", "runm.1", "bootstrap-run", "closed", nil),     // intake
		clampStepBead("runm", "runm.2", "main-repro", "closed", nil),        // repro
		clampStepBead("runm", "runm.3", "code-path-audit", "closed", nil),   // audit
		clampStepBead("runm", "runm.4", "normalize-outcome", "closed", nil), // classify
	}
	detail, err := BuildRunDetail(early, "runm", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	byKey := stageStatusByKey(detail.Stages)
	for _, key := range []string{"intake", "repro", "audit", "classify"} {
		if byKey[key] != "complete" {
			t.Errorf("stage %q = %q, want complete (it ran)", key, byKey[key])
		}
	}
	for _, key := range []string{"approval", "publish", "dispatch"} {
		if byKey[key] != "pending" {
			t.Errorf("stage %q = %q, want pending (never materialized in an early-exit run)", key, byKey[key])
		}
	}

	// Counterpart: a started-but-lost-close stage stays complete under a terminal
	// root (review-loop went in_progress then lost its close event).
	incident := []beads.Bead{
		clampRootBead("runi", "closed", nil), // mol-adopt-pr-v2
		clampStepBead("runi", "runi.1", "preflight", "closed", nil),
		clampStepBead("runi", "runi.2", "review-loop", "in_progress", nil),
	}
	incidentDetail, err := BuildRunDetail(incident, "runi", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail incident: %v", err)
	}
	incByKey := stageStatusByKey(incidentDetail.Stages)
	if incByKey["review"] != "complete" {
		t.Errorf("review stage (started, lost close) = %q, want complete", incByKey["review"])
	}
	if incByKey["cleanup"] != "pending" {
		t.Errorf("cleanup stage (never ran) = %q, want pending", incByKey["cleanup"])
	}
}

// TestBuildRunSummaryClosedRootHidesLiveFields proves D2: a closed-root lane must
// not leak lost-close liveness — StatusCounts presents every member closed and
// ActiveAssignees is empty, so a historical LaneCard never reads "on <assignee> ·
// N in progress" for a finished run.
func TestBuildRunSummaryClosedRootHidesLiveFields(t *testing.T) {
	step2 := clampStepBead("runl", "runl.2", "review-loop", "in_progress", nil)
	step2.Assignee = "worker-7"
	beadList := []beads.Bead{
		clampRootBead("runl", "closed", nil),
		clampStepBead("runl", "runl.1", "preflight", "closed", nil),
		step2,
	}
	summary := BuildRunSummary(beadList)
	lane, ok := findLaneInGroup(summary.HistoricalLanes, "runl")
	if !ok {
		t.Fatal("closed-root run not in historical lanes")
	}
	sc := lane.StatusCounts.counts
	if len(sc) != 1 || sc["closed"] != 3 {
		t.Errorf("StatusCounts = %v, want {closed:3} (every member presented closed)", sc)
	}
	if len(lane.ActiveAssignees) != 0 {
		t.Errorf("ActiveAssignees = %v, want empty (no one is assigned to a finished run)", lane.ActiveAssignees)
	}
}

// stageStatusByKey maps each stage key to its status.
func stageStatusByKey(stages []RunStage) map[string]string {
	out := make(map[string]string, len(stages))
	for _, s := range stages {
		out[s.Key] = s.Status
	}
	return out
}

// findLaneInGroup locates a lane by id within one summary bucket.
func findLaneInGroup(lanes []RunLane, id string) (RunLane, bool) {
	for _, l := range lanes {
		if l.ID == id {
			return l, true
		}
	}
	return RunLane{}, false
}

func laneInGroup(lanes []RunLane, id string) bool {
	_, ok := findLaneInGroup(lanes, id)
	return ok
}

// badgeByLabel finds a control badge by its display label.
func badgeByLabel(badges []RunControlBadge, label string) (RunControlBadge, bool) {
	for _, b := range badges {
		if b.Label == label {
			return b, true
		}
	}
	return RunControlBadge{}, false
}

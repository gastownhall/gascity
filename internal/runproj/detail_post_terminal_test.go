package runproj

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestBuildRunDetailDoesNotSkipStepsThatRunAfterTheRootSettles is the detail
// DAG's half of the post-terminal rule the typed /steps endpoint pins: the
// workflow finalizer closes the root before it closes itself, and the teardown
// tail runs because the root settled, so while they are open under a failed
// root they read their own status, not "skipped". An ordinary open step under
// the same root is still skipped.
func TestBuildRunDetailDoesNotSkipStepsThatRunAfterTheRootSettles(t *testing.T) {
	beadList := []beads.Bead{
		clampRootBead("runf", "closed", map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail}),
		clampStepBead("runf", "runf.validate", "validate", "closed", map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail}),
		clampStepBead("runf", "runf.fin", "workflow-finalize", "open", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize}),
		clampStepBead("runf", "runf.cleanup", "cleanup", "in_progress", map[string]string{beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown}),
		clampStepBead("runf", "runf.drain", "drain", "open", nil),
	}
	detail, err := BuildRunDetail(beadList, "runf", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	// The open finalizer has no open blockers, so the DAG presents it ready.
	statuses := nodeStatusByID(detail)
	for id, want := range map[string]string{
		"validate":          "failed",
		"workflow-finalize": "ready",
		"cleanup":           "active",
		"drain":             "skipped",
	} {
		if got := statuses[id]; got != want {
			t.Errorf("node %q status = %q, want %q; statuses=%v", id, got, want, statuses)
		}
	}
}

// TestRunStepClampExemptsOnlyPostTerminalMembers pins which members execute
// after the root settles: the workflow finalizer, a teardown-role member, and
// any member generated for a teardown step (same gc.step_id), mirroring the
// dispatcher's teardown-tail exclusion.
func TestRunStepClampExemptsOnlyPostTerminalMembers(t *testing.T) {
	members := []beads.Bead{
		clampStepBead("r", "r.fin", "workflow-finalize", "open", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize}),
		clampStepBead("r", "r.td", "cleanup", "open", map[string]string{beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown}),
		clampStepBead("r", "r.td-attempt", "cleanup", "open", nil),
		clampStepBead("r", "r.step", "work", "open", nil),
		clampStepBead("r", "r.active", "work2", "in_progress", nil),
	}
	clamp := NewRunStepClamp(CanonicalRunStatusFailed, members)
	for _, tc := range []struct {
		id, status, want string
	}{
		{"r.fin", "pending", "pending"},
		{"r.td", "pending", "pending"},
		{"r.td", "active", "active"},
		{"r.td-attempt", "pending", "pending"},
		{"r.step", "pending", "skipped"},
		{"r.active", "active", "skipped"},
		{"r.fin", "completed", "completed"},
	} {
		if got := clamp.Apply(tc.id, tc.status); got != tc.want {
			t.Errorf("Apply(%s, %s) = %q, want %q", tc.id, tc.status, got, tc.want)
		}
	}
	if got := NewRunStepClamp(CanonicalRunStatusActive, members).Apply("r.step", "pending"); got != "pending" {
		t.Errorf("non-terminal run: Apply(r.step, pending) = %q, want pending", got)
	}
}

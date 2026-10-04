package beads

import (
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// updateForceSpy records which door an Update went through: the plain
// lifecycle, or the single-item batch.
type updateForceSpy struct {
	beadslib.Storage
	lifecycle *nativeDoltRecordingLifecycle
	applier   *recordingBatchApplier
}

func newUpdateForceSpy() *updateForceSpy {
	return &updateForceSpy{lifecycle: &nativeDoltRecordingLifecycle{}, applier: &recordingBatchApplier{}}
}

func (s *updateForceSpy) IssueLifecycle() (issueops.Lifecycle, error) { return s.lifecycle, nil }
func (s *updateForceSpy) BatchApplier() (issueops.BatchApplier, error) {
	return s.applier, nil
}

// A patch that arms neither guard must reach the plain lifecycle door carrying
// NO force member. The http client refuses force_assignee_transfer and
// force_close_policy on updateIssue outright (W-UpdateRequest.*), so a store
// that sets them unconditionally cannot write a title over the wire.
func TestUpdateWithoutAGuardedFieldSendsNoForce(t *testing.T) {
	spy := newUpdateForceSpy()
	store := newNativeDoltStoreForTest(spy)

	title := "renamed"
	if err := store.Update("gc-1", UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(spy.lifecycle.updates) != 1 {
		t.Fatalf("the plain update door was used %d times, want 1", len(spy.lifecycle.updates))
	}
	req := spy.lifecycle.updates[0]
	if req.ForceAssigneeTransfer || req.ForceClosePolicy {
		t.Errorf("update sent a force member the wire refuses: %+v", req)
	}
	if len(spy.applier.requests) != 0 {
		t.Errorf("an unguarded patch took the batch door: %+v", spy.applier.requests)
	}
}

// An assignee edit is the anti-steal fence's trigger, and Store.Update is the
// low-level projection verb that has always bypassed it. The bypass is
// expressible on the apply item and NOT on updateIssue, so this one takes the
// batch door.
func TestUpdateWithAnAssigneeEditTakesTheBatchDoorWithItsForce(t *testing.T) {
	spy := newUpdateForceSpy()
	store := newNativeDoltStoreForTest(spy)

	assignee := "worker-2"
	if err := store.Update("gc-1", UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(spy.lifecycle.updates) != 0 {
		t.Fatalf("a force-requiring patch took the plain door: %+v", spy.lifecycle.updates)
	}
	req := soleBatchRequest(t, spy.applier)
	if len(req.Items) != 1 || req.Items[0].Update == nil {
		t.Fatalf("batch = %+v, want one update item", req.Items)
	}
	item := req.Items[0].Update
	if item.Target.ID != "gc-1" {
		t.Errorf("target = %+v, want gc-1 by id", item.Target)
	}
	if !item.ForceAssigneeTransfer {
		t.Error("the assignee transfer is not forced; Store.Update applied no claim fence before and its callers enforce ownership a layer up")
	}
	if item.ForceClosePolicy {
		t.Error("close policy is forced without a status change; the flag is conditioned on the field that arms its guard")
	}
}

func TestUpdateWithAStatusEditForcesOnlyTheClosePolicy(t *testing.T) {
	spy := newUpdateForceSpy()
	store := newNativeDoltStoreForTest(spy)

	status := "closed"
	if err := store.Update("gc-1", UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	req := soleBatchRequest(t, spy.applier)
	item := req.Items[0].Update
	if !item.ForceClosePolicy {
		t.Error("a done-crossing status write is not force-waived; a molecule root routinely closes over open children")
	}
	if item.ForceAssigneeTransfer {
		t.Error("the assignee fence is waived without an assignee edit, which is a hard validation error on the role")
	}
}

// A reparent alone stays on the plain door, because updateIssue publishes
// parent_id and the apply patch does NOT (W-ApplyPatch.ParentID). Sending it
// through the batch would refuse the one write the wire can serve.
func TestUpdateWithOnlyAReparentStaysOnThePlainDoor(t *testing.T) {
	spy := newUpdateForceSpy()
	store := newNativeDoltStoreForTest(spy)

	parent := "gc-parent"
	if err := store.Update("gc-1", UpdateOpts{ParentID: &parent}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(spy.applier.requests) != 0 {
		t.Fatalf("a reparent took the batch door, where parent_id is refused: %+v", spy.applier.requests)
	}
	if len(spy.lifecycle.updates) != 1 {
		t.Fatalf("the plain update door was used %d times, want 1", len(spy.lifecycle.updates))
	}
	if !spy.lifecycle.updates[0].Patch.ParentID.Set {
		t.Error("the parent edit did not reach the patch")
	}
}

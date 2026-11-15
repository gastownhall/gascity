package splittest

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// batchedChildren is a front with a batched children read.
type batchedChildren struct {
	*beads.MemStore
	calls int
}

func (b *batchedChildren) ChildrenOfAny(parentIDs []string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	b.calls++
	return beads.ChildrenOfAnyPerParent(b.MemStore, parentIDs, opts...)
}

// guardedUpdater is a front with an assignment-guarded update.
type guardedUpdater struct {
	*beads.MemStore
	calls int
}

func (g *guardedUpdater) UpdateIfAssignment(id, _, _ string, opts beads.UpdateOpts) (bool, error) {
	g.calls++
	return true, g.Update(id, opts)
}

// guardFixture is a projection over a MemStore holding one task and one
// session, with its own sink.
func guardFixture(t *testing.T) (*Projection, *beads.MemStore, *violationSink, beads.Bead, beads.Bead) {
	t.Helper()
	p, leaf, sink := newTestProjection(t)
	task := seed(t, leaf, beads.Bead{Title: "task"})
	session := seed(t, leaf, sessionBead("s"))
	return p, leaf, sink, task, session
}

func wantNotHere(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("%s: error = %v, want ErrNotFound (a split city's store does not hold the row)", what, err)
	}
}

func TestProjectionMetadataCASIsChecked(t *testing.T) {
	p, leaf, sink, task, session := guardFixture(t)
	cas, ok := p.WorkView().(beads.MetadataCASWriterHandleProvider).MetadataCASWriterHandle()
	if !ok {
		t.Fatal("no metadata CAS handle over a MemStore")
	}
	_, err := cas.CompareAndSetMetadataKey(session.ID, "k", "", "v")
	wantNotHere(t, "CAS of a session through the work view", err)
	if got, _ := leaf.Get(session.ID); got.Metadata["k"] != "" {
		t.Fatal("the refused CAS wrote the row")
	}
	if ok, err := cas.CompareAndSetMetadataKey(task.ID, "k", "", "v"); err != nil || !ok {
		t.Fatalf("CAS of the view's own row = %v, %v", ok, err)
	}
	if sink.testOnlyCount() != 1 {
		t.Fatalf("flags = %d, want 1", sink.testOnlyCount())
	}
}

func TestProjectionGuardedBatchIsChecked(t *testing.T) {
	p, leaf, sink, task, session := guardFixture(t)
	batcher, ok := p.WorkView().(beads.GuardedBatcherHandleProvider).GuardedBatcherHandle()
	if !ok {
		t.Fatal("no guarded batcher over a MemStore")
	}
	title := "renamed"
	_, err := batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Update: &beads.GuardedUpdate{ID: session.ID, Opts: beads.UpdateOpts{Title: &title}}},
	}})
	wantNotHere(t, "guarded update of a session through the work view", err)
	if got, _ := leaf.Get(session.ID); got.Title != "s" {
		t.Fatal("the refused batch wrote the row")
	}
	created := sessionBead("batched")
	created.ID = "gcg-batch-1"
	if _, err := batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &created},
		{Update: &beads.GuardedUpdate{ID: task.ID, Opts: beads.UpdateOpts{Title: &title}}},
	}}); err != nil {
		t.Fatalf("a batch creating a session through the work view: %v", err)
	}
	if _, err := p.ClassView().Get("gcg-batch-1"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("a session the work view's batch created is visible to the class view: %v", err)
	}
	if sink.testOnlyCount() < 2 {
		t.Fatalf("flags = %d, want the refused update and the misplaced create", sink.testOnlyCount())
	}
}

func TestProjectionBatchedChildrenAreFilteredAndFlagged(t *testing.T) {
	leaf := beads.NewMemStore()
	front := &batchedChildren{MemStore: leaf}
	sink := newViolationSink()
	p := projectionOf(front, sink)
	parent := seed(t, leaf, beads.Bead{Title: "parent"})
	seed(t, leaf, beads.Bead{Title: "work child", ParentID: parent.ID})
	child := sessionBead("session child")
	child.ParentID = parent.ID
	seed(t, leaf, child)
	kids, err := p.WorkViewOf(front).(beads.ChildrenBatchLister).ChildrenOfAny([]string{parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if front.calls != 1 {
		t.Fatalf("the front's batched read ran %d times, want 1", front.calls)
	}
	if len(kids) != 1 || kids[0].Title != "work child" {
		t.Fatalf("ChildrenOfAny through the work view = %+v, want the work child only", kids)
	}
	if sink.testOnlyCount() != 1 {
		t.Fatalf("flags = %d, want 1: asking for a parent's children selects them", sink.testOnlyCount())
	}
}

func TestProjectionTxUpdateIsChecked(t *testing.T) {
	p, leaf, _, task, session := guardFixture(t)
	title := "renamed"
	err := p.WorkView().Tx("t", func(tx beads.Tx) error {
		return tx.Update(session.ID, beads.UpdateOpts{Title: &title})
	})
	wantNotHere(t, "Tx update of a session through the work view", err)
	if got, _ := leaf.Get(session.ID); got.Title != "s" {
		t.Fatal("the refused Tx update wrote the row")
	}
	parent := session.ID
	err = p.WorkView().Tx("t", func(tx beads.Tx) error {
		return tx.Update(task.ID, beads.UpdateOpts{ParentID: &parent})
	})
	wantNotHere(t, "Tx re-parenting a task under a session", err)
}

func TestProjectionGuardedUpdateThroughTheResolveTargetIsChecked(t *testing.T) {
	leaf := beads.NewMemStore()
	front := &guardedUpdater{MemStore: leaf}
	p := projectionOf(front, newViolationSink())
	task := seed(t, leaf, beads.Bead{Title: "task"})
	session := seed(t, leaf, sessionBead("s"))
	updater, ok := beads.AssignmentGuardedUpdaterFor(p.WorkViewOf(front))
	if !ok {
		t.Fatal("the guarded update was stripped")
	}
	title := "renamed"
	_, err := updater.UpdateIfAssignment(session.ID, "open", "", beads.UpdateOpts{Title: &title})
	wantNotHere(t, "guarded update of a session through the work view", err)
	if front.calls != 0 {
		t.Fatal("the refused guarded update reached the front")
	}
	if ok, err := updater.UpdateIfAssignment(task.ID, "open", "", beads.UpdateOpts{Title: &title}); err != nil || !ok || front.calls != 1 {
		t.Fatalf("guarded update of the view's own row = %v, %v (front calls %d)", ok, err, front.calls)
	}
}

func TestProjectionReleaseIfCurrentIsChecked(t *testing.T) {
	p, _, _, task, session := guardFixture(t)
	releaser := p.WorkView().(beads.ConditionalAssignmentReleaser)
	_, err := releaser.ReleaseIfCurrent(session.ID, "someone")
	wantNotHere(t, "release of a session through the work view", err)
	if _, err := releaser.ReleaseIfCurrent(task.ID, "someone"); err != nil {
		t.Fatalf("release of the view's own row: %v", err)
	}
}

func TestProjectionDepRemoveChecksBothEnds(t *testing.T) {
	p, leaf, _, task, session := guardFixture(t)
	other := seed(t, leaf, beads.Bead{Title: "other"})
	if err := leaf.DepAdd(task.ID, session.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	wantNotHere(t, "removing an edge to a session through the work view", p.WorkView().DepRemove(task.ID, session.ID))
	if deps, _ := leaf.DepList(task.ID, "down"); len(deps) != 1 {
		t.Fatal("the refused DepRemove removed the edge")
	}
	wantNotHere(t, "removing an edge from a session through the work view", p.WorkView().DepRemove(session.ID, other.ID))
}

func TestProjectionChecksCrossStoreEdgesOnCreateAndUpdate(t *testing.T) {
	p, leaf, sink, task, session := guardFixture(t)
	work := p.WorkView()
	_, err := work.Create(beads.Bead{Title: "child of a session", ParentID: session.ID})
	wantNotHere(t, "creating a task parented to a session", err)
	_, err = work.Create(beads.Bead{Title: "needs a session", Needs: []string{session.ID}})
	wantNotHere(t, "creating a task that needs a session", err)
	_, err = work.Create(beads.Bead{Title: "typed need", Needs: []string{"blocks:" + session.ID}})
	wantNotHere(t, "creating a task with a typed need on a session", err)
	_, err = work.Create(beads.Bead{Title: "dep on a session", Dependencies: []beads.Dep{{DependsOnID: session.ID, Type: "blocks"}}})
	wantNotHere(t, "creating a task with a dependency on a session", err)
	if _, err := work.Create(beads.Bead{Title: "step ref need", Needs: []string{"review"}}); err != nil {
		t.Fatalf("a Needs entry that names no row (a formula step ref) was refused: %v", err)
	}
	parent := session.ID
	wantNotHere(t, "re-parenting a task under a session", work.Update(task.ID, beads.UpdateOpts{ParentID: &parent}))
	if got, _ := leaf.Get(task.ID); got.ParentID != "" {
		t.Fatal("the refused re-parent wrote the row")
	}
	ownParent := seed(t, leaf, beads.Bead{Title: "work parent"})
	if _, err := work.Create(beads.Bead{Title: "child", ParentID: ownParent.ID}); err != nil {
		t.Fatalf("a task parented to a work row was refused: %v", err)
	}
	if sink.testOnlyCount() != 5 {
		t.Fatalf("flags = %d, want 5 (four refused creates, one refused re-parent)", sink.testOnlyCount())
	}
}

// TestProjectionAllowsTheAttachmentParentOnly: a graph root created through
// the class view under a work bead is the designed attachment link; other
// cross-store parents are refused.
func TestProjectionAllowsTheAttachmentParentOnly(t *testing.T) {
	p, _, sink, task, session := guardFixture(t)
	class := p.ClassView()
	root := beads.Bead{Title: "wisp", Metadata: map[string]string{"gc.kind": "wisp"}, ParentID: task.ID}
	if _, err := class.Create(root); err != nil {
		t.Fatalf("attaching a graph root to its work source bead was refused: %v", err)
	}
	mail := beads.Bead{Title: "msg", Type: "message", ParentID: task.ID}
	_, err := class.Create(mail)
	wantNotHere(t, "a mail row parented to a work bead", err)
	_, err = p.WorkView().Create(beads.Bead{Title: "work under a session", ParentID: session.ID})
	wantNotHere(t, "a work row parented to a session", err)
	if sink.testOnlyCount() != 2 {
		t.Fatalf("flags = %d, want 2 (only the refused parents)", sink.testOnlyCount())
	}
}

// TestProjectionParentChildEdgeStillChecksItsIssue: the parent-child edge
// that mirrors a row's own ParentID skips the parent's check, never the
// row's: a session's parent edge through the work view is refused.
func TestProjectionParentChildEdgeStillChecksItsIssue(t *testing.T) {
	p, leaf, sink, task, _ := guardFixture(t)
	child := sessionBead("child session")
	child.ParentID = task.ID
	child = seed(t, leaf, child)
	wantNotHere(t, "a session's parent-child edge through the work view", p.WorkView().DepAdd(child.ID, task.ID, "parent-child"))
	if sink.testOnlyCount() != 1 {
		t.Fatalf("flags = %d, want 1", sink.testOnlyCount())
	}
}

// writeFront is a MemStore front with the optional writes a MemStore lacks
// (batch delete, edge payloads, the atomic terminal write, graph apply), so
// the view's guards on them can run.
type writeFront struct{ *beads.MemStore }

func (f *writeFront) DeleteBatch(ids []string) error {
	for _, id := range ids {
		if err := f.Delete(id); err != nil {
			return err
		}
	}
	return nil
}

func (f *writeFront) DepAddWithMetadata(issueID, dependsOnID, depType, _ string) error {
	return f.DepAdd(issueID, dependsOnID, depType)
}

func (f *writeFront) CloseWithMetadataIfMatch(id string, rev int64, metadata map[string]string) (beads.Bead, error) {
	if err := f.SetMetadataBatch(id, metadata); err != nil {
		return beads.Bead{}, err
	}
	if err := f.CloseIfMatch(id, rev); err != nil {
		return beads.Bead{}, err
	}
	return f.Get(id)
}

func (f *writeFront) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	ids := make(map[string]string, len(plan.Nodes))
	for _, node := range plan.Nodes {
		created, err := f.Create(beads.Bead{Title: node.Title, Type: node.Type, Labels: node.Labels, Metadata: node.Metadata})
		if err != nil {
			return nil, err
		}
		ids[node.Key] = created.ID
	}
	return &beads.GraphApplyResult{IDs: ids}, nil
}

// TestProjectionGuardsTheRemainingWrites: Reopen, CloseAll, DeleteBatch,
// DepAddWithMetadata, SetMetadataBatch, DeleteIfMatch and
// CloseWithMetadataIfMatch refuse a row of the other side and leave it
// unwritten, and graph apply flags a plan of the other side's class and pins
// the rows it creates to the view that wrote them.
func TestProjectionGuardsTheRemainingWrites(t *testing.T) {
	leaf := beads.NewMemStore()
	front := &writeFront{MemStore: leaf}
	sink := newViolationSink()
	p := projectionOf(front, sink)
	task := seed(t, leaf, beads.Bead{Title: "task"})
	open := seed(t, leaf, sessionBead("open"))
	closed := seed(t, leaf, sessionBead("closed"))
	if err := leaf.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	work := p.WorkViewOf(front)

	wantNotHere(t, "Reopen of a session", work.Reopen(closed.ID))
	if n, err := work.CloseAll([]string{open.ID, task.ID}, nil); err != nil || n != 1 {
		t.Fatalf("CloseAll over a session and a task = %d, %v; want the task closed alone", n, err)
	}
	wantNotHere(t, "DeleteBatch of a session", work.(beads.BatchDeleter).DeleteBatch([]string{open.ID}))
	wantNotHere(t, "DepAddWithMetadata to a session", work.(beads.DepMetadataWriter).DepAddWithMetadata(task.ID, open.ID, "blocks", "{}"))
	wantNotHere(t, "SetMetadataBatch of a session", work.SetMetadataBatch(open.ID, map[string]string{"k": "v"}))
	writer, ok := beads.ConditionalWriterFor(work)
	if !ok {
		t.Fatal("conditional writes were stripped")
	}
	wantNotHere(t, "DeleteIfMatch of a session", writer.DeleteIfMatch(open.ID, 1))
	closer, ok := beads.AtomicConditionalCloserFor(work)
	if !ok {
		t.Fatal("the atomic terminal write was stripped")
	}
	_, err := closer.CloseWithMetadataIfMatch(open.ID, 1, map[string]string{"k": "v"})
	wantNotHere(t, "CloseWithMetadataIfMatch of a session", err)
	if got, err := leaf.Get(open.ID); err != nil || got.Status != "open" || got.Metadata["k"] != "" {
		t.Fatalf("the refused writes changed the open session: %+v, %v", got, err)
	}
	if got, err := leaf.Get(closed.ID); err != nil || got.Status != "closed" {
		t.Fatalf("the refused Reopen changed the closed session: %+v, %v", got, err)
	}

	applier, ok := beads.GraphApplyFor(work)
	if !ok {
		t.Fatal("graph apply was stripped")
	}
	result, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
		{Key: "root", Title: "wisp", Metadata: map[string]string{"gc.kind": "wisp"}},
	}})
	if err != nil {
		t.Fatalf("graph apply of a wisp through the work view: %v", err)
	}
	root := result.IDs["root"]
	if _, err := work.Get(root); err != nil {
		t.Fatalf("the work view lost the wisp its graph apply wrote: %v", err)
	}
	if _, err := p.ClassViewOf(front).Get(root); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("the class view sees a wisp the work view's graph apply wrote: %v", err)
	}
	// One flag per refused write (seven, CloseAll's session included), the
	// misrouted graph apply, and the class view's miss of the pinned wisp.
	if got := sink.testOnlyCount(); got != 9 {
		t.Fatalf("flags = %d, want 9", got)
	}
}

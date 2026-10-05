package beads

import (
	"context"
	"errors"
	"fmt"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// What ApplyGraphPlan puts on the wire.
//
// The RunInTransaction-based implementation this route replaced spent one
// transaction with N+E+P+A individual storage calls inside it (N creates, E
// explicit edges, P parent links, A deferred assignee updates) and hand-rolled
// its own parent/edge conflict pre-check. issueops.BatchApplier is exactly
// what a graph plan needs instead: ONE request, atomic, with the hierarchy and
// cycle checks owned by the role rather than re-implemented here. The
// recorder below exists to check the ONE PROPERTY that matters for this
// route — what request it composed — the same way closeBatchRecorder does
// for CloseAll.

// graphApplyBatchRecorder records every ApplyBatch request and mints a
// sequential id for each create item, so a caller's ValidateGraphApplyResult
// check (every node key maps to a nonempty id) passes without a real backend.
type graphApplyBatchRecorder struct {
	requests []issueops.ApplyBatchRequest
	// err, when set, refuses every request.
	err    error
	nextID int
}

var _ issueops.BatchApplier = (*graphApplyBatchRecorder)(nil)

func (r *graphApplyBatchRecorder) ApplyBatch(_ context.Context, req issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return issueops.ApplyBatchResult{}, r.err
	}
	result := issueops.ApplyBatchResult{Keys: map[string]string{}, Items: make([]issueops.ItemResult, 0, len(req.Items))}
	for _, item := range req.Items {
		switch item.Kind {
		case issueops.ItemCreate:
			r.nextID++
			id := fmt.Sprintf("gc-apply-%d", r.nextID)
			if item.Create.Key != "" {
				result.Keys[item.Create.Key] = id
			}
			result.Items = append(result.Items, issueops.ItemResult{Kind: item.Kind, IssueID: id, Changed: true})
		default:
			result.Items = append(result.Items, issueops.ItemResult{Kind: item.Kind, Changed: true})
		}
	}
	return result, nil
}

// graphApplyBatchSpy is a backing whose batch applier is the recorder and
// whose every read fails the test: the route composes its whole request
// before it dials and has nothing to read back.
type graphApplyBatchSpy struct {
	*nativeDoltStorageSpy
	recorder *graphApplyBatchRecorder
	reads    int
}

func newGraphApplyBatchSpy() *graphApplyBatchSpy {
	spy := &graphApplyBatchSpy{recorder: &graphApplyBatchRecorder{}}
	spy.nativeDoltStorageSpy = &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			spy.reads++
			return nil, fmt.Errorf("graphApplyBatchSpy: unexpected read for %q", id)
		},
	}
	return spy
}

// BatchApplier self-references the spy rather than forwarding to the embedded
// nativeDoltStorageSpy's own role accessor, for the same reason every other
// double in this package that needs its own applier does: the embedding
// promotion would otherwise dispatch ApplyBatch against the EMBEDDED value's
// method set, bypassing this recorder entirely.
func (s *graphApplyBatchSpy) BatchApplier() (issueops.BatchApplier, error) {
	return s.recorder, nil
}

// TestNativeApplyGraphPlanComposesOneBatchRequest pins the item shape: one
// create per node (carrying MetadataRefs for the role's own splice), the
// explicit edges, the parent links (PARENT-ON-CREATE's one spelling), and the
// deferred post-create assignments, all inside ONE never-chunked request, with
// zero per-bead reads.
func TestNativeApplyGraphPlanComposesOneBatchRequest(t *testing.T) {
	spy := newGraphApplyBatchSpy()
	store := newNativeDoltStoreForTest(spy)

	plan := &GraphApplyPlan{
		CommitMessage: "gc: test graph",
		Nodes: []GraphApplyNode{
			{Key: "root", Title: "Root"},
			{Key: "blocker", Title: "Blocker"},
			{
				Key:               "child",
				Title:             "Child",
				ParentKey:         "root",
				Assignee:          "gascity/worker",
				AssignAfterCreate: true,
				MetadataRefs:      map[string]string{"gc.root_bead_id": "root"},
			},
		},
		Edges: []GraphApplyEdge{{FromKey: "child", ToKey: "blocker"}},
	}

	result, err := store.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)
	if err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage: %v", err)
	}

	if len(spy.recorder.requests) != 1 {
		t.Fatalf("ApplyBatch called %d times, want exactly 1: a graph plan is ONE request, never chunked", len(spy.recorder.requests))
	}
	if spy.reads != 0 {
		t.Errorf("the batch route dialed %d per-bead read(s); it composes its request before it dials", spy.reads)
	}

	req := spy.recorder.requests[0]
	if req.Actor == "" {
		t.Error("the batch names no actor; it is one act by one caller and the role requires attribution")
	}
	if req.Provenance != "gc: test graph" {
		t.Errorf("Provenance = %q, want the plan's own commit message", req.Provenance)
	}

	// 3 creates, 1 explicit edge, 1 parent-child dep_add, 1 deferred assignee update.
	if len(req.Items) != 6 {
		t.Fatalf("len(req.Items) = %d, want 6", len(req.Items))
	}
	wantKinds := []issueops.ItemKind{
		issueops.ItemCreate, issueops.ItemCreate, issueops.ItemCreate,
		issueops.ItemDepAdd, issueops.ItemDepAdd, issueops.ItemUpdate,
	}
	for i, want := range wantKinds {
		if req.Items[i].Kind != want {
			t.Fatalf("items[%d].Kind = %q, want %q", i, req.Items[i].Kind, want)
		}
	}

	childCreate := req.Items[2].Create
	if childCreate == nil || childCreate.Key != "child" {
		t.Fatalf("items[2] is not child's create: %#v", req.Items[2])
	}
	if ref, ok := childCreate.MetadataRefs["gc.root_bead_id"]; !ok || ref.Key != "root" {
		t.Errorf("child.MetadataRefs[gc.root_bead_id] = %+v, ok=%v, want Ref{Key: \"root\"}", ref, ok)
	}
	if childCreate.Issue.Assignee != "" {
		t.Errorf("child create carries Assignee %q, want empty: AssignAfterCreate defers the assignment to the update item", childCreate.Issue.Assignee)
	}

	edge := req.Items[3].DepAdd
	if edge == nil || edge.Source.Key != "child" || edge.Target.Key != "blocker" || edge.Type != beadslib.DepBlocks {
		t.Errorf("items[3] = %+v, want child->blocker blocks", edge)
	}

	parent := req.Items[4].DepAdd
	if parent == nil || parent.Source.Key != "child" || parent.Target.Key != "root" || parent.Type != beadslib.DepParentChild {
		t.Errorf("items[4] = %+v, want child->root parent-child", parent)
	}

	assign := req.Items[5].Update
	if assign == nil || assign.Target.Key != "child" || !assign.Patch.Assignee.Set || assign.Patch.Assignee.Value != "gascity/worker" {
		t.Errorf("items[5] = %+v, want child's deferred assignee patch", assign)
	}

	if result.IDs["root"] == "" || result.IDs["blocker"] == "" || result.IDs["child"] == "" {
		t.Fatalf("result.IDs = %+v, missing a key", result.IDs)
	}
}

// TestNativeApplyGraphPlanRefusesOverCapWhenNoLocalTransactionExists pins the
// typed refusal for a backend with NO transaction route at all (a served/v0
// backend's RunInTransaction refuses with a typed *beadslib.ErrUnsupported
// before its callback ever runs): a plan whose batch representation exceeds
// issueops.MaxApplyBatchItems is refused with *GraphApplyTooLargeError, and
// no ApplyBatch request is ever dialed either — the cap is never satisfied by
// chunking into several requests.
func TestNativeApplyGraphPlanRefusesOverCapWhenNoLocalTransactionExists(t *testing.T) {
	spy := newGraphApplyBatchSpy()
	spy.runInTransaction = func(context.Context, string, func(beadslib.Transaction) error) error {
		return &beadslib.ErrUnsupported{Op: "RunInTransaction", Backend: "dolt-server"}
	}
	store := newNativeDoltStoreForTest(spy)

	nodes := make([]GraphApplyNode, issueops.MaxApplyBatchItems+1)
	for i := range nodes {
		nodes[i] = GraphApplyNode{Key: fmt.Sprintf("n%d", i), Title: fmt.Sprintf("Node %d", i)}
	}
	plan := &GraphApplyPlan{Nodes: nodes}

	_, err := store.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)
	if err == nil {
		t.Fatal("over-cap plan was accepted")
	}
	var tooLarge *GraphApplyTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %v, want *GraphApplyTooLargeError", err)
	}
	if tooLarge.Items != len(nodes) {
		t.Errorf("GraphApplyTooLargeError.Items = %d, want %d", tooLarge.Items, len(nodes))
	}
	if tooLarge.Max != issueops.MaxApplyBatchItems {
		t.Errorf("GraphApplyTooLargeError.Max = %d, want %d", tooLarge.Max, issueops.MaxApplyBatchItems)
	}
	if len(spy.recorder.requests) != 0 {
		t.Errorf("ApplyBatch was called %d times; an over-cap plan must never be chunked into several requests", len(spy.recorder.requests))
	}
}

// TestNativeApplyGraphPlanOverCapUsesLocalTransactionWhenSupported pins the
// G3 HIGH-2 fix's other half: a LOCAL backend's RunInTransaction is not
// bound by issueops.MaxApplyBatchItems at all, since the cap belongs to
// BatchApplier, not to the backend. An over-cap plan on a backend whose
// RunInTransaction actually runs the callback must succeed through it,
// dialing zero ApplyBatch requests.
func TestNativeApplyGraphPlanOverCapUsesLocalTransactionWhenSupported(t *testing.T) {
	spy := newGraphApplyBatchSpy()
	nextID := 0
	spy.createIssue = func(_ context.Context, issue *beadslib.Issue, _ string) error {
		nextID++
		issue.ID = fmt.Sprintf("gc-local-%d", nextID)
		return nil
	}
	store := newNativeDoltStoreForTest(spy)

	nodes := make([]GraphApplyNode, issueops.MaxApplyBatchItems+1)
	for i := range nodes {
		nodes[i] = GraphApplyNode{Key: fmt.Sprintf("n%d", i), Title: fmt.Sprintf("Node %d", i)}
	}
	plan := &GraphApplyPlan{Nodes: nodes}

	result, err := store.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)
	if err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage: %v, want success through the local-transaction fallback", err)
	}
	if len(result.IDs) != len(nodes) {
		t.Fatalf("len(result.IDs) = %d, want %d", len(result.IDs), len(nodes))
	}
	if len(spy.recorder.requests) != 0 {
		t.Errorf("ApplyBatch was called %d times, want 0: a local backend never needs BatchApplier for this plan", len(spy.recorder.requests))
	}
}

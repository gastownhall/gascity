package sqlite

// Closed-contract conformance for the Graph class front door, run against the
// REAL SQLite graph engine rather than an in-memory stand-in. Everything here goes
// through storebinding.GraphStore: if a behavior cannot be reached from the
// closed contract, a consumer cannot reach it either, and pinning it from
// inside the package would prove nothing about the front door.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func graphSpec(t *testing.T, root string) storebinding.BindingSpec {
	t.Helper()
	return storebinding.BindingSpec{Name: "infra", Provider: ProviderID, Path: root}
}

// openGraphComponent opens the deployed Graph component below root and closes
// it when the test ends.
func openGraphComponent(t *testing.T, root string) *GraphComponent {
	t.Helper()
	component, err := OpenGraph(graphSpec(t, root))
	if err != nil {
		t.Fatalf("OpenGraph: %v", err)
	}
	t.Cleanup(func() {
		if err := component.Close(); err != nil {
			t.Errorf("closing Graph component: %v", err)
		}
	})
	return component
}

// graphFrontDoor opens a fresh Graph component and returns only its closed
// contract, which is all a consumer ever holds.
func openGraphFrontDoor(t *testing.T) storebinding.GraphStore {
	t.Helper()
	return openGraphComponent(t, t.TempDir()).Graph()
}

func mustCreateGraphBead(t *testing.T, front storebinding.GraphStore, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := front.Create(b)
	if err != nil {
		t.Fatalf("Create(%+v): %v", b, err)
	}
	return created
}

func TestOpenGraphAdoptsTheDeployedPathAndReservedNamespace(t *testing.T) {
	root := t.TempDir()
	component := openGraphComponent(t, root)

	wantPath, err := GraphPath(root)
	if err != nil {
		t.Fatalf("GraphPath: %v", err)
	}
	if component.Path() != wantPath {
		t.Fatalf("component path = %q, want the deployed %q", component.Path(), wantPath)
	}
	created := mustCreateGraphBead(t, component.Graph(), beads.Bead{Title: "root"})
	if created.ID != "gcg-1" {
		t.Fatalf("first minted id = %q, want gcg-1 — only Graph mints the reserved namespace", created.ID)
	}
}

// TestOpenGraphAdoptsExistingRowsInPlace pins that a second open of the same
// deployed file serves the same rows: adoption, not replacement.
func TestOpenGraphAdoptsExistingRowsInPlace(t *testing.T) {
	root := t.TempDir()
	first, err := OpenGraph(graphSpec(t, root))
	if err != nil {
		t.Fatalf("OpenGraph (first): %v", err)
	}
	seeded := mustCreateGraphBead(t, first.Graph(), beads.Bead{Title: "kept", Labels: []string{"keep"}})
	if err := first.Close(); err != nil {
		t.Fatalf("closing first Graph component: %v", err)
	}

	second := openGraphComponent(t, root)
	got, err := second.Graph().Get(seeded.ID)
	if err != nil {
		t.Fatalf("adopted binding lost %s: %v", seeded.ID, err)
	}
	if got.Title != "kept" || len(got.Labels) != 1 || got.Labels[0] != "keep" {
		t.Fatalf("adopted row changed: %+v", got)
	}
	next := mustCreateGraphBead(t, second.Graph(), beads.Bead{Title: "after reopen"})
	if next.ID != "gcg-2" {
		t.Fatalf("id after reopen = %q, want gcg-2 — the allocator must not restart", next.ID)
	}
}

func TestOpenGraphRejectsAForeignProvider(t *testing.T) {
	spec := graphSpec(t, t.TempDir())
	spec.Provider = storebinding.ProviderID("postgres")
	component, err := OpenGraph(spec)
	if !errors.Is(err, ErrInvalidGraphComponent) {
		if component != nil {
			_ = component.Close()
		}
		t.Fatalf("OpenGraph(foreign provider) error = %v, want ErrInvalidGraphComponent", err)
	}
}

// TestNewGraphComponentRefusesAnIncompleteEngine is the fail-closed half of
// "no capability degrades at call time": a store that cannot serve the whole
// class is refused when the component opens, not when a caller reaches the one
// method it is missing.
func TestNewGraphComponentRefusesAnIncompleteEngine(t *testing.T) {
	component, err := newGraphComponent(beads.NewMemStore())
	if component != nil {
		t.Fatal("newGraphComponent accepted a store that is not the deployed Graph engine")
	}
	if !errors.Is(err, ErrInvalidGraphComponent) {
		t.Fatalf("newGraphComponent(memstore) error = %v, want ErrInvalidGraphComponent", err)
	}
}

// TestGraphFrontDoorHidesTheRawStore pins AC 2: the graph front door exposes
// the closed contract and nothing else. It matters more here than for the
// other classes because the engine behind it IS a beads.Store, so an embedded
// field would have handed callers the whole generic surface.
func TestGraphFrontDoorHidesTheRawStore(t *testing.T) {
	front := openGraphFrontDoor(t)

	// Positive control: the engine behind the front door genuinely IS a
	// beads.Store, so a silent assertion below would be a real result rather
	// than a probe that can never fire.
	if _, isStore := any(beads.NewMemStore()).(beads.Store); !isStore {
		t.Fatal("the beads.Store probe cannot fire; the rest of this test proves nothing")
	}
	if _, leaked := any(front).(beads.Store); leaked {
		t.Fatal("the Graph front door satisfies beads.Store — the generic store escapes the adapter edge")
	}
	for name, probe := range map[string]any{
		"Store() beads.Store":  (*interface{ Store() beads.Store })(nil),
		"Unwrap() beads.Store": (*interface{ Unwrap() beads.Store })(nil),
		"CloseStore() error":   (*interface{ CloseStore() error })(nil),
		"IDPrefix() string":    (*interface{ IDPrefix() string })(nil),
		"SetSequenceFloor(int64)": (*interface {
			SetSequenceFloor(int64) error
		})(nil),
	} {
		if reflect.TypeOf(front).Implements(reflect.TypeOf(probe).Elem()) {
			t.Fatalf("the Graph front door exposes %s — the engine escapes the adapter edge", name)
		}
	}

	// Signature scan: no method of the contract, the concrete front door, or
	// the transaction may accept or return a beads.Store in a parameter, a
	// result, or a callback. The detector proves itself against known-leaky
	// shapes first: a scan that has gone blind reports no leaks either.
	storeType := reflect.TypeOf((*beads.Store)(nil)).Elem()
	for name, leaky := range map[string]any{
		"direct parameter": func(string, beads.Store) error { return nil },
		"returned store":   func() (beads.Store, error) { return nil, nil },
		"callback":         func(string, func(beads.Store, beads.Bead) (bool, error)) (bool, error) { return false, nil },
	} {
		if path := findBeadsStore(reflect.TypeOf(leaky), storeType, map[reflect.Type]bool{}); path == "" {
			t.Fatalf("leak detector missed the %s shape", name)
		}
	}
	for _, subject := range []reflect.Type{
		reflect.TypeOf((*graphFrontDoor)(nil)),
		reflect.TypeOf((*storebinding.GraphStore)(nil)).Elem(),
		reflect.TypeOf((*storebinding.GraphTx)(nil)).Elem(),
		reflect.TypeOf(graphFrontDoorTx{}),
		reflect.TypeOf((*GraphComponent)(nil)),
	} {
		for i := 0; i < subject.NumMethod(); i++ {
			method := subject.Method(i)
			if path := findBeadsStore(method.Type, storeType, map[reflect.Type]bool{}); path != "" {
				t.Fatalf("%s.%s exposes beads.Store via %s", subject, method.Name, path)
			}
		}
	}
}

// TestGraphFrontDoorReadyContextAnswersInsteadOfVetoing is the direct
// counter-test to the degradation this slice exists to remove: through the
// generic beads adapter, ReadyContext is a capability error. Over the deployed
// engine it must be an answer.
func TestGraphFrontDoorReadyContextAnswersInsteadOfVetoing(t *testing.T) {
	front := openGraphFrontDoor(t)
	ready := mustCreateGraphBead(t, front, beads.Bead{Title: "actionable", Type: "task"})
	blocker := mustCreateGraphBead(t, front, beads.Bead{Title: "blocker", Type: "task"})
	blocked := mustCreateGraphBead(t, front, beads.Bead{Title: "blocked", Type: "task"})
	if err := front.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	got, err := front.ReadyContext(context.Background(), beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("ReadyContext: %v", err)
	}
	if errors.Is(err, storebinding.ErrBeadsAdapterCapability) {
		t.Fatal("ReadyContext vetoed the capability instead of answering")
	}
	ids := map[string]bool{}
	for _, item := range got {
		ids[item.ID] = true
	}
	if !ids[ready.ID] || !ids[blocker.ID] {
		t.Fatalf("ReadyContext = %v, want the two unblocked beads", ids)
	}
	if ids[blocked.ID] {
		t.Fatal("ReadyContext returned a bead with an open blocker")
	}

	plain, err := front.Ready(beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(plain) != len(got) {
		t.Fatalf("Ready returned %d beads and ReadyContext %d; they must agree", len(plain), len(got))
	}
}

func TestGraphFrontDoorReadyContextHonorsACancelledContext(t *testing.T) {
	front := openGraphFrontDoor(t)
	mustCreateGraphBead(t, front, beads.Bead{Title: "actionable", Type: "task"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := front.ReadyContext(ctx, beads.ReadyQuery{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadyContext(canceled) error = %v, want context.Canceled", err)
	}
	if errors.Is(err, storebinding.ErrBeadsAdapterCapability) {
		t.Fatal("a canceled ReadyContext reported a missing capability")
	}
	if got != nil {
		t.Fatalf("ReadyContext(canceled) returned %d beads, want none", len(got))
	}
}

func TestGraphFrontDoorCountCountsAndExcludesTypes(t *testing.T) {
	front := openGraphFrontDoor(t)
	mustCreateGraphBead(t, front, beads.Bead{Title: "task", Type: "task"})
	mustCreateGraphBead(t, front, beads.Bead{Title: "another task", Type: "task"})
	mustCreateGraphBead(t, front, beads.Bead{Title: "step", Type: "step"})

	query := beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth}
	total, err := front.Count(context.Background(), query)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 3 {
		t.Fatalf("Count = %d, want 3", total)
	}
	tasks, err := front.Count(context.Background(), query, "step")
	if err != nil {
		t.Fatalf("Count(exclude step): %v", err)
	}
	if tasks != 2 {
		t.Fatalf("Count(exclude step) = %d, want 2", tasks)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := front.Count(ctx, query); !errors.Is(err, context.Canceled) {
		t.Fatalf("Count(canceled) error = %v, want context.Canceled", err)
	}
}

func TestGraphFrontDoorWaitForParentProjectionObservesTheReparent(t *testing.T) {
	front := openGraphFrontDoor(t)
	oldParent := mustCreateGraphBead(t, front, beads.Bead{Title: "old parent", Type: "molecule"})
	newParent := mustCreateGraphBead(t, front, beads.Bead{Title: "new parent", Type: "molecule"})
	child := mustCreateGraphBead(t, front, beads.Bead{Title: "child", Type: "step", ParentID: oldParent.ID})

	if err := front.Update(child.ID, beads.UpdateOpts{ParentID: &newParent.ID}); err != nil {
		t.Fatalf("Update(reparent): %v", err)
	}
	if err := front.WaitForParentProjection(context.Background(), child.ID, oldParent.ID, newParent.ID); err != nil {
		t.Fatalf("WaitForParentProjection after a committed reparent: %v", err)
	}
	if errors.Is(front.WaitForParentProjection(context.Background(), child.ID, oldParent.ID, newParent.ID), storebinding.ErrBeadsAdapterCapability) {
		t.Fatal("WaitForParentProjection vetoed the capability instead of checking the projection")
	}
}

// TestGraphFrontDoorWaitForParentProjectionReportsDivergence is the half that
// makes the method non-vacuous: a projection that does not reflect the move
// must be an error, not a silent success.
func TestGraphFrontDoorWaitForParentProjectionReportsDivergence(t *testing.T) {
	front := openGraphFrontDoor(t)
	oldParent := mustCreateGraphBead(t, front, beads.Bead{Title: "old parent", Type: "molecule"})
	newParent := mustCreateGraphBead(t, front, beads.Bead{Title: "new parent", Type: "molecule"})
	child := mustCreateGraphBead(t, front, beads.Bead{Title: "child", Type: "step", ParentID: oldParent.ID})

	err := front.WaitForParentProjection(context.Background(), child.ID, oldParent.ID, newParent.ID)
	if !errors.Is(err, ErrGraphParentProjectionDiverged) {
		t.Fatalf("WaitForParentProjection over an unmoved child = %v, want ErrGraphParentProjectionDiverged", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := front.WaitForParentProjection(ctx, child.ID, oldParent.ID, newParent.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForParentProjection(canceled) = %v, want context.Canceled", err)
	}
}

// TestGraphFrontDoorClaimPreservesDeployedSemantics pins the exact deployed
// claim contract the CLI's hook --claim path depends on. Every clause here is
// behavior that would otherwise have to be re-implemented in a projection.
func TestGraphFrontDoorClaimPreservesDeployedSemantics(t *testing.T) {
	front := openGraphFrontDoor(t)
	target := mustCreateGraphBead(t, front, beads.Bead{Title: "claimable", Type: "task"})

	claimed, ok, err := front.Claim(target.ID, "worker")
	if err != nil || !ok {
		t.Fatalf("first Claim = (%+v, %v, %v), want success", claimed, ok, err)
	}
	if claimed.Status != "in_progress" || claimed.Assignee != "worker" {
		t.Fatalf("first Claim returned %+v, want in_progress held by worker", claimed)
	}
	if claimed.ClaimFence == 0 {
		t.Fatal("first Claim did not bump the ownership fence")
	}

	reclaimed, ok, err := front.Claim(target.ID, "worker")
	if err != nil || !ok {
		t.Fatalf("same-owner Claim = (%+v, %v, %v), want an idempotent success", reclaimed, ok, err)
	}
	if reclaimed.Revision != claimed.Revision || reclaimed.ClaimFence != claimed.ClaimFence {
		t.Fatalf("same-owner Claim consumed a revision or fence: %+v then %+v", claimed, reclaimed)
	}

	stolen, ok, err := front.Claim(target.ID, "other")
	if err != nil {
		t.Fatalf("contended Claim error = %v, want a conflict reported as ok=false", err)
	}
	if ok {
		t.Fatalf("contended Claim = %+v, want ok=false", stolen)
	}
	held, err := front.Get(target.ID)
	if err != nil {
		t.Fatalf("Get after contended Claim: %v", err)
	}
	if held.Assignee != "worker" {
		t.Fatalf("contended Claim changed the holder to %q", held.Assignee)
	}

	if _, ok, err := front.Claim("gcg-nope", "worker"); !errors.Is(err, beads.ErrNotFound) || ok {
		t.Fatalf("Claim(absent) = (%v, %v), want ErrNotFound", ok, err)
	}
	if _, ok, err := front.Claim(target.ID, "  "); err == nil || ok {
		t.Fatalf("Claim(empty assignee) = (%v, %v), want rejection", ok, err)
	}

	closed := mustCreateGraphBead(t, front, beads.Bead{Title: "done", Type: "task"})
	if err := front.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok, err := front.Claim(closed.ID, "worker"); err != nil || ok {
		t.Fatalf("Claim(closed) = (%v, %v), want a conflict, never a resurrection", ok, err)
	}
}

// TestGraphFrontDoorClaimAndReleaseArePaired pins the acquire/release duality:
// the contract exposed the release half long before it had an acquire half.
func TestGraphFrontDoorClaimAndReleaseArePaired(t *testing.T) {
	front := openGraphFrontDoor(t)
	target := mustCreateGraphBead(t, front, beads.Bead{Title: "claimable", Type: "task"})

	if _, ok, err := front.Claim(target.ID, "worker"); err != nil || !ok {
		t.Fatalf("Claim = (%v, %v), want success", ok, err)
	}
	if released, err := front.ReleaseIfCurrent(target.ID, "other"); err != nil || released {
		t.Fatalf("ReleaseIfCurrent(wrong owner) = (%v, %v), want a refusal", released, err)
	}
	released, err := front.ReleaseIfCurrent(target.ID, "worker")
	if err != nil || !released {
		t.Fatalf("ReleaseIfCurrent(holder) = (%v, %v), want success", released, err)
	}
	if _, ok, err := front.Claim(target.ID, "second"); err != nil || !ok {
		t.Fatalf("Claim after release = (%v, %v), want the bead to be claimable again", ok, err)
	}
}

// TestGraphFrontDoorClaimHasExactlyOneWinner exercises the CAS under real
// concurrency: the deployed engine serializes claims through one write
// connection, and the front door must not widen that.
func TestGraphFrontDoorClaimHasExactlyOneWinner(t *testing.T) {
	front := openGraphFrontDoor(t)
	target := mustCreateGraphBead(t, front, beads.Bead{Title: "contended", Type: "task"})

	const claimants = 8
	var wg sync.WaitGroup
	results := make([]bool, claimants)
	failures := make([]error, claimants)
	start := make(chan struct{})
	for i := 0; i < claimants; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			_, ok, err := front.Claim(target.ID, "worker-"+string(rune('a'+index)))
			results[index] = ok
			failures[index] = err
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for index, ok := range results {
		if failures[index] != nil {
			t.Fatalf("claimant %d errored: %v", index, failures[index])
		}
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d claimants won the same bead, want exactly 1", winners)
	}
}

func TestGraphFrontDoorTxCommitsOrRollsBackAtomically(t *testing.T) {
	front := openGraphFrontDoor(t)
	existing := mustCreateGraphBead(t, front, beads.Bead{Title: "existing", Type: "task"})

	failure := errors.New("caller aborted")
	var doomed string
	err := front.Tx("rollback", func(tx storebinding.GraphTx) error {
		created, err := tx.Create(beads.Bead{Title: "doomed", Type: "task"})
		if err != nil {
			return err
		}
		doomed = created.ID
		if err := tx.Close(existing.ID); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("Tx error = %v, want the caller's error", err)
	}
	if _, err := front.Get(doomed); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(%s) after rollback = %v, want ErrNotFound", doomed, err)
	}
	survivor, err := front.Get(existing.ID)
	if err != nil {
		t.Fatalf("Get after rollback: %v", err)
	}
	if survivor.Status == "closed" {
		t.Fatal("a rolled-back transaction still closed the bead")
	}

	var committed string
	if err := front.Tx("commit", func(tx storebinding.GraphTx) error {
		created, err := tx.Create(beads.Bead{Title: "kept", Type: "task"})
		if err != nil {
			return err
		}
		committed = created.ID
		return tx.SetMetadataBatch(existing.ID, map[string]string{"gc.phase": "done"})
	}); err != nil {
		t.Fatalf("Tx(commit): %v", err)
	}
	if _, err := front.Get(committed); err != nil {
		t.Fatalf("committed bead is missing: %v", err)
	}
	stamped, err := front.Get(existing.ID)
	if err != nil {
		t.Fatalf("Get after commit: %v", err)
	}
	if stamped.Metadata["gc.phase"] != "done" {
		t.Fatalf("committed metadata missing: %+v", stamped.Metadata)
	}
}

func TestGraphFrontDoorApplyGraphPlanIsAtomicAndTyped(t *testing.T) {
	front := openGraphFrontDoor(t)

	result, err := front.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{
		CommitMessage: "materialize",
		Nodes: []beads.GraphApplyNode{
			{Key: "root", Title: "root", Type: "molecule"},
			{Key: "step", Title: "step", Type: "step", ParentKey: "root"},
		},
		Edges: []beads.GraphApplyEdge{{FromKey: "step", ToKey: "root", Type: "parent-child"}},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if len(result.IDs) != 2 {
		t.Fatalf("ApplyGraphPlan returned %d ids, want 2", len(result.IDs))
	}
	deps, err := front.DepList(result.IDs["step"], "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != result.IDs["root"] || deps[0].Type != "parent-child" {
		t.Fatalf("applied edges = %+v, want one typed parent-child edge", deps)
	}

	before, err := front.Count(context.Background(), beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("Count before invalid plan: %v", err)
	}
	partial, err := front.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{{Key: "orphan", Title: "orphan"}},
		Edges: []beads.GraphApplyEdge{{FromKey: "orphan", ToKey: "missing", Type: "blocks"}},
	})
	if err == nil {
		t.Fatal("ApplyGraphPlan accepted an edge to an unknown key")
	}
	// Partial-result semantics: a failed apply publishes no IDs at all. A
	// caller that recorded a half-populated map would hold references to
	// beads that were never committed.
	if partial != nil {
		t.Fatalf("a failed ApplyGraphPlan returned ids %+v, want none", partial.IDs)
	}
	after, err := front.Count(context.Background(), beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("Count after invalid plan: %v", err)
	}
	if after != before {
		t.Fatalf("a rejected plan left %d beads behind", after-before)
	}
}

// TestGraphFrontDoorRoundTripsOpaqueEdgeMetadata pins the payload a
// graph-apply edge carries. The deployed deps table has no metadata column, so
// this rides a sidecar; without a read on the closed contract the payload
// would be writable through the front door and readable only by reaching past
// it for a raw store.
func TestGraphFrontDoorRoundTripsOpaqueEdgeMetadata(t *testing.T) {
	front := openGraphFrontDoor(t)

	const payload = `{"recipe":"build","wait":"gate"}`
	result, err := front.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{
			{Key: "root", Title: "root", Type: "molecule"},
			{Key: "step", Title: "step", Type: "step"},
		},
		Edges: []beads.GraphApplyEdge{{FromKey: "step", ToKey: "root", Type: "blocks", Metadata: payload}},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	metadata, ok, err := front.DepMetadata(result.IDs["step"], result.IDs["root"])
	if err != nil {
		t.Fatalf("DepMetadata: %v", err)
	}
	if !ok || metadata != payload {
		t.Fatalf("DepMetadata = (%q, %v), want the applied payload", metadata, ok)
	}

	// An edge that carried no payload reports absence, not an empty payload:
	// the two are different states for a migration to preserve.
	plain := mustCreateGraphBead(t, front, beads.Bead{Title: "plain", Type: "task"})
	if err := front.DepAdd(plain.ID, result.IDs["root"], "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if metadata, ok, err := front.DepMetadata(plain.ID, result.IDs["root"]); err != nil || ok || metadata != "" {
		t.Fatalf("DepMetadata over a plain edge = (%q, %v, %v), want absence", metadata, ok, err)
	}
}

func TestGraphFrontDoorAppliesSelectedStorageTiers(t *testing.T) {
	front := openGraphFrontDoor(t)

	wisp, err := front.CreateWithStorage(beads.Bead{Title: "wisp", Type: "task"}, beads.StorageEphemeral)
	if err != nil {
		t.Fatalf("CreateWithStorage(ephemeral): %v", err)
	}
	durable, err := front.CreateWithStorage(beads.Bead{Title: "durable", Type: "task"}, beads.StorageDefault)
	if err != nil {
		t.Fatalf("CreateWithStorage(default): %v", err)
	}

	mainTier, err := front.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List(main tier): %v", err)
	}
	for _, item := range mainTier {
		if item.ID == wisp.ID {
			t.Fatal("an ephemeral bead is visible in a main-tier read")
		}
	}
	bothTiers, err := front.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List(both tiers): %v", err)
	}
	seen := map[string]bool{}
	for _, item := range bothTiers {
		seen[item.ID] = true
	}
	if !seen[wisp.ID] || !seen[durable.ID] {
		t.Fatalf("both-tier read = %v, want both beads", seen)
	}

	result, err := front.ApplyGraphPlanWithStorage(context.Background(), &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{{Key: "root", Title: "wisp root", Type: "molecule"}},
	}, beads.StorageEphemeral)
	if err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage(ephemeral): %v", err)
	}
	applied, err := front.Get(result.IDs["root"])
	if err != nil {
		t.Fatalf("Get applied wisp root: %v", err)
	}
	if !applied.Ephemeral {
		t.Fatalf("ApplyGraphPlanWithStorage(ephemeral) produced a durable bead: %+v", applied)
	}
}

func TestGraphFrontDoorConditionalWritesGateOnRevision(t *testing.T) {
	front := openGraphFrontDoor(t)
	target := mustCreateGraphBead(t, front, beads.Bead{Title: "conditional", Type: "task"})

	stale := target.Revision
	title := "renamed"
	if err := front.UpdateIfMatch(target.ID, stale, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("UpdateIfMatch at the current revision: %v", err)
	}
	var precondition *beads.PreconditionFailedError
	if err := front.UpdateIfMatch(target.ID, stale, beads.UpdateOpts{Title: &title}); !errors.As(err, &precondition) {
		t.Fatalf("UpdateIfMatch at a stale revision = %v, want *PreconditionFailedError", err)
	}

	current, err := front.Get(target.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := front.SetMetadata(current.ID, "gc.phase", "start"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	swapped, err := front.CompareAndSetMetadataKey(current.ID, "gc.phase", "start", "done")
	if err != nil || !swapped {
		t.Fatalf("CompareAndSetMetadataKey(matching) = (%v, %v), want a swap", swapped, err)
	}
	swapped, err = front.CompareAndSetMetadataKey(current.ID, "gc.phase", "start", "again")
	if err != nil {
		t.Fatalf("CompareAndSetMetadataKey(stale) error = %v, want a refused swap", err)
	}
	if swapped {
		t.Fatal("CompareAndSetMetadataKey swapped on a stale expectation")
	}

	current, err = front.Get(target.ID)
	if err != nil {
		t.Fatalf("Get before CloseIfMatch: %v", err)
	}
	if err := front.CloseIfMatch(current.ID, current.Revision-1); !errors.As(err, &precondition) {
		t.Fatalf("CloseIfMatch at a stale revision = %v, want *PreconditionFailedError", err)
	}
	if err := front.CloseIfMatch(current.ID, current.Revision); err != nil {
		t.Fatalf("CloseIfMatch at the current revision: %v", err)
	}
}

func TestGraphFrontDoorReopenAndCloseAllRoundTrip(t *testing.T) {
	front := openGraphFrontDoor(t)
	first := mustCreateGraphBead(t, front, beads.Bead{Title: "one", Type: "task"})
	second := mustCreateGraphBead(t, front, beads.Bead{Title: "two", Type: "task"})

	closed, err := front.CloseAll([]string{first.ID, second.ID}, map[string]string{"gc.reason": "swept"})
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 2 {
		t.Fatalf("CloseAll closed %d beads, want 2", closed)
	}
	stamped, err := front.Get(first.ID)
	if err != nil {
		t.Fatalf("Get after CloseAll: %v", err)
	}
	if stamped.Status != "closed" || stamped.Metadata["gc.reason"] != "swept" {
		t.Fatalf("CloseAll left %+v, want closed and stamped", stamped)
	}
	if err := front.Reopen(first.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	reopened, err := front.Get(first.ID)
	if err != nil {
		t.Fatalf("Get after Reopen: %v", err)
	}
	if reopened.Status == "closed" {
		t.Fatal("Reopen left the bead closed")
	}
	if err := front.Delete(second.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := front.Get(second.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func TestGraphFrontDoorDependenciesSurviveRestart(t *testing.T) {
	root := t.TempDir()
	first, err := OpenGraph(graphSpec(t, root))
	if err != nil {
		t.Fatalf("OpenGraph: %v", err)
	}
	blocker := mustCreateGraphBead(t, first.Graph(), beads.Bead{Title: "blocker", Type: "task"})
	blocked := mustCreateGraphBead(t, first.Graph(), beads.Bead{Title: "blocked", Type: "task"})
	if err := first.Graph().DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if _, ok, err := first.Graph().Claim(blocker.ID, "worker"); err != nil || !ok {
		t.Fatalf("Claim = (%v, %v), want success", ok, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing first component: %v", err)
	}

	second := openGraphComponent(t, root)
	deps, err := second.Graph().DepList(blocked.ID, "down")
	if err != nil {
		t.Fatalf("DepList after restart: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != blocker.ID || deps[0].Type != "blocks" {
		t.Fatalf("dependencies after restart = %+v, want the blocks edge", deps)
	}
	held, err := second.Graph().Get(blocker.ID)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if held.Assignee != "worker" || held.Status != "in_progress" || held.ClaimFence == 0 {
		t.Fatalf("claim did not survive restart: %+v", held)
	}
	if err := second.Graph().DepRemove(blocked.ID, blocker.ID); err != nil {
		t.Fatalf("DepRemove: %v", err)
	}
	remaining, err := second.Graph().DepList(blocked.ID, "down")
	if err != nil {
		t.Fatalf("DepList after DepRemove: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("DepRemove left %+v", remaining)
	}
}

// TestGraphFrontDoorKeepsClosedMembersOfLiveRoots pins the retention hold: the
// deployed generic sweeper is root-blind, so a closed step of a still-running
// workflow is exactly what it would delete. OpenGraph therefore leaves generic
// retention at its default-disabled setting until root-aware retention lands,
// and a closed member must survive a restart.
func TestGraphFrontDoorKeepsClosedMembersOfLiveRoots(t *testing.T) {
	root := t.TempDir()
	first, err := OpenGraph(graphSpec(t, root))
	if err != nil {
		t.Fatalf("OpenGraph: %v", err)
	}
	liveRoot := mustCreateGraphBead(t, first.Graph(), beads.Bead{Title: "live workflow", Type: "molecule"})
	member := mustCreateGraphBead(t, first.Graph(), beads.Bead{Title: "finished step", Type: "step", ParentID: liveRoot.ID})
	if err := first.Graph().Close(member.ID); err != nil {
		t.Fatalf("Close(member): %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing the first component: %v", err)
	}

	second := openGraphComponent(t, root)
	survivor, err := second.Graph().Get(member.ID)
	if err != nil {
		t.Fatalf("a closed member of a live root did not survive: %v", err)
	}
	if survivor.Status != "closed" || survivor.ParentID != liveRoot.ID {
		t.Fatalf("closed member came back as %+v", survivor)
	}
	children, err := second.Graph().Children(liveRoot.ID, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	if len(children) != 1 || children[0].ID != member.ID {
		t.Fatalf("live root lost its closed member: %+v", children)
	}
}

func TestGraphFrontDoorPingsTheOpenComponent(t *testing.T) {
	if err := openGraphFrontDoor(t).Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestGraphComponentClosesOnce pins that closing twice is not an error: the
// provider unwinds components it may already have closed.
func TestGraphComponentClosesOnce(t *testing.T) {
	component, err := OpenGraph(graphSpec(t, t.TempDir()))
	if err != nil {
		t.Fatalf("OpenGraph: %v", err)
	}
	if err := component.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := component.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestGraphFrontDoorCannotBeMistakenForAWorkStore pins the class boundary from
// the other side: the Graph component's path is the deployed graph directory,
// never a Work residence, and the front door mints only the reserved prefix.
func TestGraphFrontDoorCannotBeMistakenForAWorkStore(t *testing.T) {
	root := t.TempDir()
	component := openGraphComponent(t, root)
	if filepath.Base(filepath.Dir(component.Path())) != "graph" {
		t.Fatalf("component path %q is not below the deployed graph directory", component.Path())
	}
	created := mustCreateGraphBead(t, component.Graph(), beads.Bead{Title: "graph bead", Type: "task"})
	if got := created.ID[:4]; got != "gcg-" {
		t.Fatalf("minted id %q does not carry the reserved graph prefix", created.ID)
	}
}

// The allocator-floor properties below outlived the cross-class genesis census
// that used to carry them. That census read all five files of the the legacy combined layout
// split; under one Beads scope there is one file and one allocator, so it is
// gone. What is NOT gone is the
// floor the front door writes: the Beads provider's component is this same
// graph-prefixed engine, and a floor it persists has to survive a reopen by a
// binary that knows only the sidecar.

// TestApplyGenesisSequenceFloorRejectsANegativeFloor keeps a nonsense value
// from reaching the engine, where it would be indistinguishable from genesis.
func TestApplyGenesisSequenceFloorRejectsANegativeFloor(t *testing.T) {
	component := openGraphComponent(t, t.TempDir())
	if err := component.ApplyGenesisSequenceFloor(-1); err == nil {
		t.Fatal("ApplyGenesisSequenceFloor accepted a negative floor")
	}
}

// TestApplyGenesisSequenceFloorNeverLowers pins idempotence: replaying an
// older value must not walk the allocator backwards into a minted suffix.
func TestApplyGenesisSequenceFloorNeverLowers(t *testing.T) {
	component := openGraphComponent(t, t.TempDir())
	if err := component.ApplyGenesisSequenceFloor(500); err != nil {
		t.Fatalf("ApplyGenesisSequenceFloor(500): %v", err)
	}
	if err := component.ApplyGenesisSequenceFloor(10); err != nil {
		t.Fatalf("ApplyGenesisSequenceFloor(10): %v", err)
	}
	floor, err := component.SequenceFloor()
	if err != nil {
		t.Fatalf("SequenceFloor: %v", err)
	}
	if floor != 500 {
		t.Fatalf("floor = %d after replaying a stale value, want 500", floor)
	}
}

// TestApplyGenesisSequenceFloorSurvivesRollbackToTheDeployedBinary is the
// rollback leg. The floor lives in graph.seqfloor, which the deployed engine
// reads on its own, so reopening the same directory with beads.OpenSQLiteStore
// — no front door, no knowledge that any floor was ever applied — must still
// mint above it.
func TestApplyGenesisSequenceFloorSurvivesRollbackToTheDeployedBinary(t *testing.T) {
	root := t.TempDir()
	component := openGraphComponent(t, root)
	for i := 0; i < 3; i++ {
		mustCreateGraphBead(t, component.Graph(), beads.Bead{Title: "graph row", Type: "task"})
	}
	if err := component.ApplyGenesisSequenceFloor(750); err != nil {
		t.Fatalf("ApplyGenesisSequenceFloor(750): %v", err)
	}
	dir := filepath.Dir(component.Path())
	if err := component.Close(); err != nil {
		t.Fatalf("closing the component: %v", err)
	}

	floorBytes, err := os.ReadFile(filepath.Join(dir, graphSequenceFloorFilename))
	if err != nil {
		t.Fatalf("reading %s: %v", graphSequenceFloorFilename, err)
	}
	if string(floorBytes) != "750\n" {
		t.Fatalf("%s = %q, want the applied 750\\n", graphSequenceFloorFilename, floorBytes)
	}

	rolledBack, err := beads.OpenSQLiteStore(dir, beads.WithSQLiteStoreIDPrefix(graphIDPrefix))
	if err != nil {
		t.Fatalf("reopening with the deployed engine: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := rolledBack.(interface{ CloseStore() error }); ok {
			if err := closer.CloseStore(); err != nil {
				t.Errorf("closing the rolled-back store: %v", err)
			}
		}
	})
	minted, err := rolledBack.Create(beads.Bead{Title: "after rollback", Type: "task"})
	if err != nil {
		t.Fatalf("Create after rollback: %v", err)
	}
	if minted.ID != "gcg-751" {
		t.Fatalf("post-rollback mint = %q, want gcg-751", minted.ID)
	}
}

// findBeadsStore walks a function signature, descending through nested
// function types (callbacks), slices, maps, and pointers, and reports where a
// beads.Store appears. A callback taking a store is the loophole the beads
// backend's HasOpenWork predicate used, which is why the walk descends rather
// than looking only at top-level parameter types.
func findBeadsStore(fn reflect.Type, storeType reflect.Type, seen map[reflect.Type]bool) string {
	for i := 0; i < fn.NumIn(); i++ {
		if path := findIn(fn.In(i), storeType, seen); path != "" {
			return "parameter " + path
		}
	}
	for i := 0; i < fn.NumOut(); i++ {
		if path := findIn(fn.Out(i), storeType, seen); path != "" {
			return "result " + path
		}
	}
	return ""
}

func findIn(t reflect.Type, storeType reflect.Type, seen map[reflect.Type]bool) string {
	if t == storeType {
		return t.String()
	}
	if seen[t] {
		return ""
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Func:
		if path := findBeadsStore(t, storeType, seen); path != "" {
			return t.String() + " -> " + path
		}
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		return findIn(t.Elem(), storeType, seen)
	case reflect.Map:
		if path := findIn(t.Key(), storeType, seen); path != "" {
			return path
		}
		return findIn(t.Elem(), storeType, seen)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if path := findIn(t.Field(i).Type, storeType, seen); path != "" {
				return t.String() + "." + t.Field(i).Name + " -> " + path
			}
		}
	case reflect.Interface:
		if t.Implements(storeType) && t.NumMethod() > 0 {
			return t.String()
		}
	}
	return ""
}

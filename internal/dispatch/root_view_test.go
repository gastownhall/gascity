package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Interleaving tests for the request-scoped root view (ga-vnycm2.18 Phase 2).
// Workers write with raw bd and no event, so a worker's close or metadata
// write can land between the moment a control reads the root's members and
// the control's first write. These pin what that interleaving may do: delay a
// scope close to a later control, never close a scope early, and never change
// what the control writes.

// firstWriteHookStore runs before once, right before the first write made
// through it, then behaves as the wrapped store. It offers BdStore's
// update-that-reports-its-row so control processing takes its production path.
type firstWriteHookStore struct {
	beads.Store
	before func()
	fired  bool
}

func (s *firstWriteHookStore) hook() {
	if !s.fired && s.before != nil {
		s.fired = true
		s.before()
	}
}

func (s *firstWriteHookStore) UpdateReporting(id string, opts beads.UpdateOpts) (beads.UpdatedRow, error) {
	s.hook()
	return beads.UpdateAndReadBack(s.Store, id, opts)
}

func (s *firstWriteHookStore) Update(id string, opts beads.UpdateOpts) error {
	s.hook()
	return s.Store.Update(id, opts)
}

func (s *firstWriteHookStore) Close(id string) error {
	s.hook()
	return s.Store.Close(id)
}

func (s *firstWriteHookStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.hook()
	return s.Store.CloseAll(ids, metadata)
}

func (s *firstWriteHookStore) SetMetadata(id, key, value string) error {
	s.hook()
	return s.Store.SetMetadata(id, key, value)
}

func (s *firstWriteHookStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.hook()
	return s.Store.SetMetadataBatch(id, kvs)
}

// scopedRetryFixture is a workflow whose scope holds a retry control R whose
// attempt passed, a plain member, and optionally scope-checks for both (the
// compiled shape: every scoped retry or plain step gets one).
type scopedRetryFixture struct {
	store   *beads.MemStore
	root    beads.Bead
	body    beads.Bead
	retry   beads.Bead
	attempt beads.Bead
	member  beads.Bead
	checks  []beads.Bead
}

func newScopedRetryFixture(t *testing.T, withScopeChecks bool) scopedRetryFixture {
	t.Helper()
	store := beads.NewMemStore()
	f := scopedRetryFixture{store: store}
	f.root = mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	member := func(metadata map[string]string) map[string]string {
		md := map[string]string{
			beadmeta.RootBeadIDMetadataKey:   f.root.ID,
			beadmeta.RootStoreRefMetadataKey: "city:test-city",
		}
		for k, v := range metadata {
			md[k] = v
		}
		return md
	}
	f.body = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body", Type: "task",
		Metadata: member(map[string]string{"gc.kind": "scope", "gc.scope_role": "body", "gc.step_ref": "body"}),
	})
	f.retry = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "implement", Type: "task",
		Metadata: member(map[string]string{
			"gc.kind": "retry", "gc.max_attempts": "3", "gc.scope_ref": "body", "gc.scope_role": "member",
			"gc.step_ref": "demo.implement", "gc.step_id": "implement",
		}),
	})
	f.attempt = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "implement attempt 1", Type: "task", Status: "closed",
		Metadata: member(map[string]string{
			"gc.control_for": f.retry.ID, "gc.retry_attempt": "1", "gc.attempt": "1",
			"gc.step_ref": "demo.implement.attempt.1", "gc.outcome": "pass",
			"gc.output_json": `{"from":"attempt"}`, "review.verdict": "approved",
		}),
	})
	mustDepAdd(t, store, f.retry.ID, f.attempt.ID, "blocks")
	f.member = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "document", Type: "task",
		Metadata: member(map[string]string{"gc.scope_ref": "body", "gc.scope_role": "member", "gc.step_ref": "demo.document"}),
	})
	if withScopeChecks {
		for _, subject := range []beads.Bead{f.retry, f.member} {
			check := mustCreateWorkflowBead(t, store, beads.Bead{
				Title: "Finalize scope for " + subject.Title, Type: "task",
				Metadata: member(map[string]string{
					"gc.kind": "scope-check", "gc.scope_ref": "body", "gc.scope_role": "control", "gc.control_for": subject.ID,
				}),
			})
			mustDepAdd(t, store, check.ID, subject.ID, "blocks")
			mustDepAdd(t, store, f.body.ID, check.ID, "blocks")
			f.checks = append(f.checks, check)
		}
	}
	return f
}

func (f scopedRetryFixture) processRetry(t *testing.T, before func()) {
	t.Helper()
	hooked := &firstWriteHookStore{Store: f.store, before: before}
	if _, err := ProcessControl(hooked, mustGetBead(t, f.store, f.retry.ID), ProcessOptions{}); err != nil {
		t.Fatalf("ProcessControl(retry): %v", err)
	}
	if !hooked.fired && before != nil {
		t.Fatal("premise: the retry made no write, so the interleaving never ran")
	}
}

func (f scopedRetryFixture) workerClose(t *testing.T, id string) {
	t.Helper()
	if err := f.store.SetMetadata(id, beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass); err != nil {
		t.Fatalf("worker outcome %s: %v", id, err)
	}
	if err := f.store.Close(id); err != nil {
		t.Fatalf("worker close %s: %v", id, err)
	}
}

// TestRootViewSiblingClosingAfterTheViewOnlyDelaysTheScopeClose: the plain
// member closes after the retry read the root but before its first write. The
// retry's view still shows the member open, so the retry does not close the
// scope; the member's own reconcile then does, to the same end state an
// uninterrupted run reaches.
func TestRootViewSiblingClosingAfterTheViewOnlyDelaysTheScopeClose(t *testing.T) {
	t.Parallel()

	interleaved := newScopedRetryFixture(t, false)
	interleaved.processRetry(t, func() { interleaved.workerClose(t, interleaved.member.ID) })
	if got := mustGetBead(t, interleaved.store, interleaved.body.ID).Status; got != "open" {
		t.Fatalf("body status after the retry = %q, want open: the retry's view showed the member open, so it must not close the scope", got)
	}
	if _, err := ReconcileClosedScopeMember(interleaved.store, interleaved.member.ID); err != nil {
		t.Fatalf("member reconcile: %v", err)
	}

	uninterrupted := newScopedRetryFixture(t, false)
	uninterrupted.workerClose(t, uninterrupted.member.ID)
	uninterrupted.processRetry(t, nil)

	for _, f := range []scopedRetryFixture{interleaved, uninterrupted} {
		body := mustGetBead(t, f.store, f.body.ID)
		if body.Status != "closed" || body.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass || body.Metadata["review.verdict"] != "approved" {
			t.Fatalf("body = status %q metadata %v, want closed pass with the members' metadata", body.Status, body.Metadata)
		}
	}
}

// TestRootViewSiblingClosingAfterTheViewWithScopeChecksMatchesUninterrupted is
// the same interleaving on the compiled shape, where every member has a
// scope-check: the scope-checks close the scope, so the interleaved and the
// uninterrupted runs end identical.
func TestRootViewSiblingClosingAfterTheViewWithScopeChecksMatchesUninterrupted(t *testing.T) {
	t.Parallel()

	run := func(interleave bool) scopedRetryFixture {
		f := newScopedRetryFixture(t, true)
		if interleave {
			f.processRetry(t, func() { f.workerClose(t, f.member.ID) })
		} else {
			f.workerClose(t, f.member.ID)
			f.processRetry(t, nil)
		}
		for _, check := range f.checks {
			if _, err := ProcessControl(f.store, mustGetBead(t, f.store, check.ID), ProcessOptions{}); err != nil {
				t.Fatalf("ProcessControl(scope-check %s): %v", check.ID, err)
			}
		}
		return f
	}
	want, got := run(false), run(true)
	for _, pick := range []func(scopedRetryFixture) string{
		func(f scopedRetryFixture) string { return f.body.ID },
		func(f scopedRetryFixture) string { return f.retry.ID },
		func(f scopedRetryFixture) string { return f.member.ID },
	} {
		w, g := mustGetBead(t, want.store, pick(want)), mustGetBead(t, got.store, pick(got))
		if w.Status != g.Status || w.Metadata[beadmeta.OutcomeMetadataKey] != g.Metadata[beadmeta.OutcomeMetadataKey] {
			t.Fatalf("%s: interleaved status %q outcome %q, uninterrupted status %q outcome %q",
				w.Title, g.Status, g.Metadata[beadmeta.OutcomeMetadataKey], w.Status, w.Metadata[beadmeta.OutcomeMetadataKey])
		}
	}
	if body := mustGetBead(t, got.store, got.body.ID); body.Status != "closed" {
		t.Fatalf("body status = %q, want closed by the scope-checks", body.Status)
	}
}

// TestRootViewOperatorReopenAfterTheViewMatchesTheLiveRead: an operator reopens
// a closed member after the retry read the root. On the compiled shape the
// retry never closes the scope itself (its own scope-check is still open), so
// the reopen is seen by the scope-check that follows, exactly as with live
// reads: the scope stays open while the member is open.
func TestRootViewOperatorReopenAfterTheViewMatchesTheLiveRead(t *testing.T) {
	t.Parallel()

	f := newScopedRetryFixture(t, true)
	f.workerClose(t, f.member.ID)
	f.processRetry(t, func() {
		if err := f.store.Reopen(f.member.ID); err != nil {
			t.Fatalf("operator reopen: %v", err)
		}
	})
	if got := mustGetBead(t, f.store, f.body.ID).Status; got != "open" {
		t.Fatalf("body status after the retry = %q, want open", got)
	}
	// The retry's scope-check runs next and sees the reopened member.
	result, err := ProcessControl(f.store, mustGetBead(t, f.store, f.checks[0].ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}
	if result.Action != "continue" {
		t.Fatalf("scope-check action = %q, want continue: the reopened member is open", result.Action)
	}
	if got := mustGetBead(t, f.store, f.body.ID).Status; got != "open" {
		t.Fatalf("body status = %q, want open while a member is reopened", got)
	}
}

// TestRootViewWorkerMetadataAfterTheViewWritesWhatTheReadSaw: a worker writes
// the attempt's output after the retry read the root. The retry copies the
// attempt's output as that one read saw it -- the same single read the live
// path made -- and the attempt keeps the worker's new value.
func TestRootViewWorkerMetadataAfterTheViewWritesWhatTheReadSaw(t *testing.T) {
	t.Parallel()

	f := newScopedRetryFixture(t, true)
	f.processRetry(t, func() {
		if err := f.store.SetMetadata(f.attempt.ID, beadmeta.OutputJSONMetadataKey, `{"from":"late"}`); err != nil {
			t.Fatalf("worker metadata: %v", err)
		}
	})
	retry := mustGetBead(t, f.store, f.retry.ID)
	if retry.Status != "closed" || retry.Metadata[beadmeta.OutputJSONMetadataKey] != `{"from":"attempt"}` {
		t.Fatalf("retry = status %q output %q, want closed with the output its read saw", retry.Status, retry.Metadata[beadmeta.OutputJSONMetadataKey])
	}
	if got := mustGetBead(t, f.store, f.attempt.ID).Metadata[beadmeta.OutputJSONMetadataKey]; got != `{"from":"late"}` {
		t.Fatalf("attempt output = %q, want the worker's write kept", got)
	}
}

// TestRootViewIsReadOncePerInvocationAndNeverAcrossInvocations pins the
// request scope: a retry pass reads the root's members once and answers its
// scope reconcile from that read, and the next invocation reads afresh.
func TestRootViewIsReadOncePerInvocationAndNeverAcrossInvocations(t *testing.T) {
	t.Parallel()

	f := newScopedRetryFixture(t, true)
	counting := &callCountingStore{Store: f.store}
	if _, err := ProcessControl(counting, mustGetBead(t, f.store, f.retry.ID), ProcessOptions{}); err != nil {
		t.Fatalf("ProcessControl(retry): %v", err)
	}
	rootLists := 0
	for _, op := range counting.ops {
		if op == "R List map[gc.root_bead_id:"+f.root.ID+"]" {
			rootLists++
		}
	}
	if rootLists != 1 || counting.reads != 2 {
		t.Fatalf("retry pass made %d root-member lists and %d reads, want 1 and 2 (gate Get, one member list):\n%v", rootLists, counting.reads, counting.ops)
	}

	// A member closed after that invocation is seen by the next one.
	f.workerClose(t, f.member.ID)
	counting.reset()
	if _, err := ProcessControl(counting, mustGetBead(t, f.store, f.checks[1].ID), ProcessOptions{}); err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}
	if got := mustGetBead(t, f.store, f.checks[1].ID).Status; got != "closed" {
		t.Fatalf("member scope-check status = %q, want closed", got)
	}
}

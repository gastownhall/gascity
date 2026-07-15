package molecule

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// newStampedAttachStore opens a MemStore through the beads factory so it
// carries a real conditional-writes stamp.
func newStampedAttachStore(t *testing.T, mode gate.Mode) *beads.MemStore {
	t.Helper()
	mem := beads.NewMemStore()
	_, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: mode,
		OpenFileStore:     func() (beads.Store, error) { return mem, nil },
	})
	if err != nil {
		t.Fatalf("factory open: %v", err)
	}
	return mem
}

// TestAttachEpochFenceConcurrentAttachesConvergeOnOneSubDAG is the §9.2
// stage-3 merge gate: two concurrent Attach calls sharing an idempotency key
// and ExpectedEpoch must leave exactly one live sub-DAG. The loser either
// converges via findExistingAttach (Duplicate) or loses the CAS-last epoch
// fence, in which case its sub-DAG is neutralized (molecule_failed, blocking
// edge detached) and a re-entrant call returns the winner.
func TestAttachEpochFenceConcurrentAttachesConvergeOnOneSubDAG(t *testing.T) {
	store := newStampedAttachStore(t, gate.Auto)
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	_ = store.SetMetadata(control.ID, "gc.control_epoch", "1")

	results := make([]*AttachResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recipe := makeWorkflowRecipe("attempt", "run")
			results[i], errs[i] = Attach(context.Background(), store, recipe, control.ID, AttachOptions{
				IdempotencyKey: control.ID + ":attempt:2",
				ExpectedEpoch:  1,
			})
		}()
	}
	wg.Wait()

	updated, _ := store.Get(control.ID)
	if got := updated.Metadata["gc.control_epoch"]; got != "2" {
		t.Fatalf("epoch = %q, want exactly one advance to 2", got)
	}

	var liveRoots []string
	all, err := store.List(beads.ListQuery{Metadata: map[string]string{
		beadmeta.IdempotencyKeyMetadataKey: control.ID + ":attempt:2",
	}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, b := range all {
		if b.Metadata["molecule_failed"] != "true" {
			liveRoots = append(liveRoots, b.ID)
		}
	}
	if len(liveRoots) != 1 {
		t.Fatalf("live idempotency-keyed roots = %v, want exactly one surviving sub-DAG", liveRoots)
	}
	winnerRoot := liveRoots[0]

	for i := range 2 {
		switch {
		case errs[i] == nil && results[i] != nil && results[i].RootID == winnerRoot:
			// Winner, or duplicate-convergence onto the winner.
		case errs[i] != nil && errors.Is(errs[i], ErrEpochConflict):
			// Fence loser: its sub-DAG must be neutralized.
		default:
			t.Fatalf("attach %d = (%+v, %v), want winner/duplicate or ErrEpochConflict", i, results[i], errs[i])
		}
	}

	// Any failed loser root must not keep a blocking edge from the control.
	controlDeps, err := store.DepList(control.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	for _, dep := range controlDeps {
		for _, b := range all {
			if b.ID == dep.DependsOnID && b.Metadata["molecule_failed"] == "true" {
				t.Fatalf("control still blocks on the LOSER's root %s", b.ID)
			}
		}
	}

	// A third re-entrant call converges on the winner.
	recipe := makeWorkflowRecipe("attempt", "run")
	third, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{
		IdempotencyKey: control.ID + ":attempt:2",
		ExpectedEpoch:  2,
	})
	if err != nil {
		t.Fatalf("re-entrant attach: %v", err)
	}
	if !third.Duplicate || third.RootID != winnerRoot {
		t.Fatalf("re-entrant attach = %+v, want Duplicate of the winner %s", third, winnerRoot)
	}
}

// TestAttachRequireIncapableRefusesBeforeSideEffects pins fail-closed
// ordering: under require on an incapable store, Attach must refuse before
// Instantiate — no orphan sub-DAG, no burned epoch.
func TestAttachRequireIncapableRefusesBeforeSideEffects(t *testing.T) {
	store := newStampedAttachStore(t, gate.Require)
	store.DisableConditionalWrites = true
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	_ = store.SetMetadata(control.ID, "gc.control_epoch", "1")

	before, _ := store.ListOpen()
	recipe := makeWorkflowRecipe("attempt", "run")
	_, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{ExpectedEpoch: 1})
	if !beads.IsConditionalWritesRequired(err) {
		t.Fatalf("err = %v, want the typed require refusal", err)
	}
	after, _ := store.ListOpen()
	if len(after) != len(before) {
		t.Fatalf("bead count %d -> %d: refusal must precede side effects", len(before), len(after))
	}
	updated, _ := store.Get(control.ID)
	if got := updated.Metadata["gc.control_epoch"]; got != "1" {
		t.Fatalf("epoch = %q, want untouched 1", got)
	}
}

// TestAdvanceAttachEpochFenceLoserPath drives the fence helper directly with
// a writer that always reports a conflict: the just-created sub-DAG must be
// neutralized (molecule_failed + blocking edge detached) and the epoch
// conflict surfaced for the dispatch layer's partial-attach classification.
func TestAdvanceAttachEpochFenceLoserPath(t *testing.T) {
	store := newStampedAttachStore(t, gate.Auto)
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	_ = store.SetMetadata(control.ID, "gc.control_epoch", "2")

	sub, err := store.Create(beads.Bead{Title: "loser sub-DAG root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(control.ID, sub.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	result := &Result{RootID: sub.ID, IDMapping: map[string]string{"root": sub.ID}}

	err = advanceAttachEpochFence(store, conflictOnlyWriter{}, control.ID, 1, result)
	if !errors.Is(err, ErrEpochConflict) {
		t.Fatalf("fence loser err = %v, want ErrEpochConflict", err)
	}
	subAfter, _ := store.Get(sub.ID)
	if subAfter.Metadata["molecule_failed"] != "true" {
		t.Fatal("loser sub-DAG root not marked molecule_failed")
	}
	deps, err := store.DepList(control.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	for _, dep := range deps {
		if dep.DependsOnID == sub.ID {
			t.Fatal("blocking edge to the loser's root was not detached")
		}
	}
}

// TestAdvanceAttachEpochFenceAmbiguousErrorLeavesSubDAGLive pins §9.3's C4
// tolerance: on an ambiguous transport error the fence write may have
// committed, so the sub-DAG must NOT be neutralized — the retry converges
// through findExistingAttach (which runs before the fence by documented
// contract).
func TestAdvanceAttachEpochFenceAmbiguousErrorLeavesSubDAGLive(t *testing.T) {
	store := newStampedAttachStore(t, gate.Auto)
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	_ = store.SetMetadata(control.ID, "gc.control_epoch", "1")

	sub, err := store.Create(beads.Bead{Title: "ambiguous sub-DAG root"})
	if err != nil {
		t.Fatal(err)
	}
	result := &Result{RootID: sub.ID, IDMapping: map[string]string{"root": sub.ID}}

	inner, _ := beads.ConditionalWriterFor(store)
	fenceErr := advanceAttachEpochFence(store, commitThenErrWriter{inner: inner, err: errors.New("i/o timeout")}, control.ID, 1, result)
	if fenceErr == nil || errors.Is(fenceErr, ErrEpochConflict) {
		t.Fatalf("ambiguous fence err = %v, want a transient (non-conflict) error", fenceErr)
	}
	if !strings.Contains(fenceErr.Error(), "i/o timeout") {
		t.Fatalf("fence err = %v, want the transport cause surfaced", fenceErr)
	}
	subAfter, _ := store.Get(sub.ID)
	if subAfter.Metadata["molecule_failed"] == "true" {
		t.Fatal("ambiguous fence neutralized the sub-DAG — the write may have committed and we may be the winner")
	}
}

// TestAdvanceAttachEpochIfNeededCASNeverDoubleAdvances races the duplicate-
// recovery epoch advance: with the value-CAS port, N concurrent advances from
// the same expected epoch land on exactly one increment.
func TestAdvanceAttachEpochIfNeededCASNeverDoubleAdvances(t *testing.T) {
	store := newStampedAttachStore(t, gate.Auto)
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	_ = store.SetMetadata(control.ID, "gc.control_epoch", "1")

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = advanceAttachEpochIfNeeded(store, control.ID, 1)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("advance %d: %v (a lost advance race is benign, never an error)", i, err)
		}
	}
	updated, _ := store.Get(control.ID)
	if got := updated.Metadata["gc.control_epoch"]; got != "2" {
		t.Fatalf("epoch = %q, want exactly 2 (no double advance)", got)
	}
}

type conflictOnlyWriter struct{}

func (conflictOnlyWriter) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	return beads.ErrConditionalWriteUnsupported
}

func (conflictOnlyWriter) CloseIfMatch(string, int64) error {
	return beads.ErrConditionalWriteUnsupported
}

func (conflictOnlyWriter) DeleteIfMatch(string, int64) error {
	return beads.ErrConditionalWriteUnsupported
}

func (conflictOnlyWriter) CompareAndSetMetadataKey(id, _, _, _ string) (bool, error) {
	return false, &beads.PreconditionFailedError{ID: id, Expected: 1, Current: 2}
}

type commitThenErrWriter struct {
	inner beads.ConditionalWriter
	err   error
}

func (w commitThenErrWriter) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	return w.inner.UpdateIfMatch(id, rev, opts)
}

func (w commitThenErrWriter) CloseIfMatch(id string, rev int64) error {
	return w.inner.CloseIfMatch(id, rev)
}

func (w commitThenErrWriter) DeleteIfMatch(id string, rev int64) error {
	return w.inner.DeleteIfMatch(id, rev)
}

func (w commitThenErrWriter) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if _, err := w.inner.CompareAndSetMetadataKey(id, key, expected, next); err != nil {
		return false, err
	}
	return false, w.err
}

// TestAttachFencedWinnerActivatedLoserNeverRunnable pins the speculative-
// creation contract: under an active fence writer, candidates are created
// deferred (non-runnable), only the fence winner activates, and a fence
// loser is neutralized WITHOUT ever having been runnable.
func TestAttachFencedWinnerActivatedLoserNeverRunnable(t *testing.T) {
	store := newStampedAttachStore(t, gate.Auto)
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	_ = store.SetMetadata(control.ID, "gc.control_epoch", "1")

	results := make([]*AttachResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recipe := makeWorkflowRecipe("attempt", "run")
			results[i], errs[i] = Attach(context.Background(), store, recipe, control.ID, AttachOptions{
				IdempotencyKey: control.ID + ":attempt:2",
				ExpectedEpoch:  1,
			})
		}()
	}
	wg.Wait()

	all, err := store.List(beads.ListQuery{Metadata: map[string]string{
		beadmeta.IdempotencyKeyMetadataKey: control.ID + ":attempt:2",
	}})
	if err != nil {
		t.Fatal(err)
	}
	var activated, neutralized, pendingLeft int
	for _, b := range all {
		switch {
		case b.Metadata[beadmeta.MoleculeFailedMetadataKey] == "true":
			neutralized++
			if b.Metadata[beadmeta.DeferredTypeMetadataKey] == "" {
				t.Fatalf("loser root %s lost its deferred marker: it was ACTIVATED before neutralization (was runnable)", b.ID)
			}
		case b.Metadata[beadmeta.AttachFencePendingMetadataKey] == "true":
			pendingLeft++
		default:
			activated++
			if b.Metadata[beadmeta.DeferredTypeMetadataKey] != "" || b.Type == "gate" {
				t.Fatalf("winner root %s not fully activated: type=%q deferred=%q", b.ID, b.Type, b.Metadata[beadmeta.DeferredTypeMetadataKey])
			}
		}
	}
	if activated != 1 {
		t.Fatalf("activated roots = %d (neutralized=%d pending=%d), want exactly 1 runnable root", activated, neutralized, pendingLeft)
	}
	for i := range 2 {
		if errs[i] != nil && !errors.Is(errs[i], ErrEpochConflict) {
			t.Fatalf("attach %d error = %v, want nil or the convergent epoch conflict", i, errs[i])
		}
	}
}

// TestAttachAmbiguousFenceConvergesDeterministically drives the §9.3 hole the
// review flagged: an ambiguous fence error leaves a NON-RUNNABLE pending
// candidate, and the retry converges through deterministic pending recovery —
// activating exactly one candidate and advancing the epoch — rather than
// accepting whichever same-idempotency root it happens to find.
func TestAttachAmbiguousFenceConvergesDeterministically(t *testing.T) {
	store := newStampedAttachStore(t, gate.Auto)
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	_ = store.SetMetadata(control.ID, "gc.control_epoch", "1")

	// First attempt: the fence write COMMITS but reports an ambiguous
	// transport error, so Attach surfaces transient and leaves the candidate
	// pending and deferred.
	inner, _ := beads.ConditionalWriterFor(store)
	sub, err := store.Create(beads.Bead{
		Title: "ambiguous candidate",
		Type:  "gate",
		Metadata: map[string]string{
			beadmeta.IdempotencyKeyMetadataKey:     control.ID + ":attempt:2",
			beadmeta.RootBeadIDMetadataKey:         root.ID,
			beadmeta.AttachFencePendingMetadataKey: "true",
			beadmeta.DeferredTypeMetadataKey:       "task",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := &Result{RootID: sub.ID, IDMapping: map[string]string{"root": sub.ID}}
	fenceErr := advanceAttachEpochFence(store, commitThenErrWriter{inner: inner, err: errors.New("i/o timeout")}, control.ID, 1, result)
	if fenceErr == nil || errors.Is(fenceErr, ErrEpochConflict) {
		t.Fatalf("ambiguous fence err = %v, want transient", fenceErr)
	}
	if got, _ := store.Get(sub.ID); got.Metadata[beadmeta.AttachFencePendingMetadataKey] != "true" {
		t.Fatal("candidate lost its pending marker on ambiguity")
	}

	// Retry: findExistingAttach's recovery must adopt the pending candidate,
	// activate it, and return it as the duplicate with the epoch advanced.
	recipe := makeWorkflowRecipe("attempt", "run")
	retry, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{
		IdempotencyKey: control.ID + ":attempt:2",
		ExpectedEpoch:  2, // the ambiguous write committed: epoch is 2
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !retry.Duplicate || retry.RootID != sub.ID {
		t.Fatalf("retry = %+v, want Duplicate of the pending candidate %s", retry, sub.ID)
	}
	got, _ := store.Get(sub.ID)
	if got.Metadata[beadmeta.AttachFencePendingMetadataKey] == "true" {
		t.Fatal("recovered candidate still pending: never activated")
	}
	if got.Type != "task" {
		t.Fatalf("recovered candidate type = %q, want the deferred type restored", got.Type)
	}
}

// TestFindExistingAttachResolvesDualPendingDeterministically seeds the
// worst-case ambiguity — BOTH racers ambiguous, both candidates alive and
// pending — and asserts recovery picks the lexicographically smallest,
// activates only it, and neutralizes the other.
func TestFindExistingAttachResolvesDualPendingDeterministically(t *testing.T) {
	store := newStampedAttachStore(t, gate.Auto)
	root := setupWorkflow(t, store)
	control := setupWorkflowChild(t, store, root.ID, "Control")
	_ = store.SetMetadata(control.ID, "gc.control_epoch", "2") // fence committed by someone

	mk := func(title string) beads.Bead {
		b, err := store.Create(beads.Bead{
			Title: title,
			Type:  "gate",
			Metadata: map[string]string{
				beadmeta.IdempotencyKeyMetadataKey:     control.ID + ":attempt:2",
				beadmeta.RootBeadIDMetadataKey:         root.ID,
				beadmeta.AttachFencePendingMetadataKey: "true",
				beadmeta.DeferredTypeMetadataKey:       "task",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	c1, c2 := mk("candidate one"), mk("candidate two")
	winnerID, loserID := c1.ID, c2.ID
	if c2.ID < c1.ID {
		winnerID, loserID = c2.ID, c1.ID
	}

	recipe := makeWorkflowRecipe("attempt", "run")
	got, err := Attach(context.Background(), store, recipe, control.ID, AttachOptions{
		IdempotencyKey: control.ID + ":attempt:2",
		ExpectedEpoch:  2,
	})
	if err != nil {
		t.Fatalf("recovery attach: %v", err)
	}
	if !got.Duplicate || got.RootID != winnerID {
		t.Fatalf("recovery = %+v, want deterministic Duplicate of %s", got, winnerID)
	}
	w, _ := store.Get(winnerID)
	if w.Metadata[beadmeta.AttachFencePendingMetadataKey] == "true" || w.Type != "task" {
		t.Fatalf("deterministic winner %s not activated: %+v", winnerID, w.Metadata)
	}
	l, _ := store.Get(loserID)
	if l.Metadata[beadmeta.MoleculeFailedMetadataKey] != "true" {
		t.Fatalf("stale candidate %s not neutralized", loserID)
	}
	if l.Metadata[beadmeta.DeferredTypeMetadataKey] == "" {
		t.Fatalf("stale candidate %s was activated before neutralization", loserID)
	}
}

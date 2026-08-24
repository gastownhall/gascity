//go:build integration

package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"
	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// TestNativeDoltStoreMetadataCASPreservesMixedJSONSiblingTypesAgainstRealDolt
// retains one real-storage proof for the raw JSON metadata boundary. The fast
// in-memory test owns the branch detail; this test proves the CAS preserves the
// exact durable sibling representation exposed by upstream Dolt.
func TestNativeDoltStoreMetadataCASPreservesMixedJSONSiblingTypesAgainstRealDolt(t *testing.T) {
	ctx := context.Background()
	store := openRealNativeDoltStoreForCAS(t, "cas-mixed-metadata")
	created, err := store.Create(Bead{Title: "real Dolt mixed metadata CAS"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	seed := json.RawMessage(`{
		"lease":"old",
		"bool_sibling":true,
		"number_sibling":42,
		"large_number_sibling":9007199254740993123456789,
		"null_sibling":null,
		"object_sibling":{"nested":"value"},
		"array_sibling":[1,"two",false],
		"string_sibling":"preserved"
	}`)
	storage, release, err := store.acquireStorage()
	if err != nil {
		t.Fatalf("acquire storage for fixture: %v", err)
	}
	if err := storage.UpdateIssue(
		ctx,
		created.ID,
		map[string]interface{}{"metadata": seed},
		"mixed-metadata-fixture",
	); err != nil {
		release()
		t.Fatalf("seed mixed metadata: %v", err)
	}
	release()
	storage, release, err = store.acquireStorage()
	if err != nil {
		t.Fatalf("reacquire storage for pre-CAS read: %v", err)
	}
	preCAS, err := storage.GetIssue(ctx, created.ID)
	release()
	if err != nil {
		t.Fatalf("GetIssue before CAS: %v", err)
	}
	var preRaw map[string]json.RawMessage
	if err := json.Unmarshal(preCAS.Metadata, &preRaw); err != nil {
		t.Fatalf("decode pre-CAS metadata: %v", err)
	}
	largeNumberBefore := string(preRaw["large_number_sibling"])
	if largeNumberBefore == "" {
		t.Fatal("pre-CAS metadata lacks the large numeric sibling")
	}

	swapped, err := store.CompareAndSetMetadataKey(created.ID, "lease", "old", "1")
	if err != nil || !swapped {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want (true, nil)", swapped, err)
	}

	storage, release, err = store.acquireStorage()
	if err != nil {
		t.Fatalf("reacquire storage for readback: %v", err)
	}
	issue, err := storage.GetIssue(ctx, created.ID)
	release()
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	assertMixedMetadataCASResult(t, issue.Metadata, largeNumberBefore)
}

// openRealNativeDoltStoreForCAS opens a NativeDoltStore over REAL upstream
// native storage. The narrow CAS contract is a claim about the backend's
// transaction semantics, and the in-memory fixture used by the unit-level
// conformance cannot answer it: nativeDoltMemStorage.RunInTransaction
// snapshots for rollback and then runs the callback UNLOCKED, so it models
// atomicity but provides no isolation whatsoever.
func openRealNativeDoltStoreForCAS(t *testing.T, actor string) *NativeDoltStore {
	t.Helper()
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Fatalf("open upstream native beads storage: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Errorf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	return newNativeDoltStoreWithStorageAndPrefix(storage, actor, "gc")
}

// TestNativeDoltStoreConditionalWriterRequireAgainstRealOpenBestAvailable
// proves that the exact upstream production constructor resolves the required
// conditional-write capability and executes all three revision-fenced verbs.
func TestNativeDoltStoreConditionalWriterRequireAgainstRealOpenBestAvailable(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "conditional-writer-require")
	store.stampConditionalWritesMode(gate.Require, false)

	writer, diagnostic, err := ResolveConditionalWriter(store)
	if err != nil || diagnostic != nil || writer == nil {
		t.Fatalf("ResolveConditionalWriter = (%T, %+v, %v), want writer, nil, nil", writer, diagnostic, err)
	}

	parent, err := store.Create(Bead{Title: "conditional-writer-parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	created, err := store.Create(Bead{Title: "conditional-writer-real", Labels: []string{"remove-me"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created, err = store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if created.Revision == 0 {
		t.Fatal("revision after create = 0, want a live token")
	}
	title := "conditional-writer-updated"
	if err := writer.UpdateIfMatch(created.ID, created.Revision, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("UpdateIfMatch: %v", err)
	}
	updated, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if updated.Title != title {
		t.Fatalf("title after update = %q, want %q", updated.Title, title)
	}
	if updated.Revision == created.Revision {
		t.Fatalf("revision after update = %d, want a fresh token", updated.Revision)
	}

	parentID := parent.ID
	compositeTitle := "conditional-writer-composite"
	if err := writer.UpdateIfMatch(updated.ID, updated.Revision, UpdateOpts{
		Title:        &compositeTitle,
		ParentID:     &parentID,
		Labels:       []string{"atomic"},
		RemoveLabels: []string{"remove-me"},
	}); err != nil {
		t.Fatalf("composite UpdateIfMatch: %v", err)
	}
	composite, err := store.Get(updated.ID)
	if err != nil {
		t.Fatalf("Get after composite update: %v", err)
	}
	if composite.Revision == updated.Revision || composite.Title != compositeTitle || composite.ParentID != parent.ID ||
		!slices.Contains(composite.Labels, "atomic") || slices.Contains(composite.Labels, "remove-me") {
		t.Fatalf("composite after-image = %+v", composite)
	}

	if err := writer.CloseIfMatch(composite.ID, composite.Revision); err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}
	closed, err := store.Get(composite.ID)
	if err != nil {
		t.Fatalf("Get after close: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("status after CloseIfMatch = %q, want closed", closed.Status)
	}
	if closed.Revision == composite.Revision {
		t.Fatalf("revision after close = %d, want a fresh token", closed.Revision)
	}
	if err := writer.DeleteIfMatch(closed.ID, closed.Revision); err != nil {
		t.Fatalf("DeleteIfMatch: %v", err)
	}
	if _, err := store.Get(closed.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
}

func TestNativeDoltStoreUpdateIfMatchClosePolicyAgainstRealDolt(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "conditional-close-policy")
	closed := "closed"

	t.Run("live blocker", func(t *testing.T) {
		blocker, err := store.Create(Bead{Title: "live blocker"})
		if err != nil {
			t.Fatalf("Create blocker: %v", err)
		}
		target, err := store.Create(Bead{Title: "blocked target"})
		if err != nil {
			t.Fatalf("Create target: %v", err)
		}
		if err := store.DepAdd(target.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("DepAdd blocker: %v", err)
		}
		before, err := store.Get(target.ID)
		if err != nil {
			t.Fatalf("Get before: %v", err)
		}
		err = store.UpdateIfMatch(target.ID, before.Revision, UpdateOpts{Status: &closed})
		if !errors.Is(err, beadslib.ErrCloseBlocked) {
			t.Fatalf("UpdateIfMatch = %v, want ErrCloseBlocked", err)
		}
		after, err := store.Get(target.ID)
		if err != nil || after.Status != "open" || after.Revision != before.Revision {
			t.Fatalf("refused blocker close = %+v, err=%v; before=%+v", after, err, before)
		}
	})

	t.Run("open child", func(t *testing.T) {
		parent, err := store.Create(Bead{Title: "parent"})
		if err != nil {
			t.Fatalf("Create parent: %v", err)
		}
		child, err := store.Create(Bead{Title: "open child"})
		if err != nil {
			t.Fatalf("Create child: %v", err)
		}
		if err := store.DepAdd(child.ID, parent.ID, "parent-child"); err != nil {
			t.Fatalf("DepAdd child: %v", err)
		}
		before, err := store.Get(parent.ID)
		if err != nil {
			t.Fatalf("Get before: %v", err)
		}
		err = store.UpdateIfMatch(parent.ID, before.Revision, UpdateOpts{Status: &closed})
		if !errors.Is(err, issueops.ErrCloseOpenChildren) {
			t.Fatalf("UpdateIfMatch = %v, want ErrCloseOpenChildren", err)
		}
		after, err := store.Get(parent.ID)
		if err != nil || after.Status != "open" || after.Revision != before.Revision {
			t.Fatalf("refused open-child close = %+v, err=%v; before=%+v", after, err, before)
		}
	})
}

func TestNativeDoltStoreUpdateIfMatchMixedClosePolicyAgainstRealDolt(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "conditional-mixed-close-policy")
	requireNativeDoltRelatedUpdatePrerequisite(t, store)
	closed := "closed"

	blocker, err := store.Create(Bead{Title: "live blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	newParent, err := store.Create(Bead{Title: "new parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	target, err := store.Create(Bead{Title: "before", Labels: []string{"keep"}})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	if err := store.DepAdd(target.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd blocker: %v", err)
	}
	before, err := store.Get(target.ID)
	if err != nil {
		t.Fatalf("Get before refused mixed close: %v", err)
	}
	title := "must roll back"
	err = store.UpdateIfMatch(target.ID, before.Revision, UpdateOpts{
		Status:   &closed,
		Title:    &title,
		ParentID: &newParent.ID,
		Labels:   []string{"must-roll-back"},
	})
	if !errors.Is(err, beadslib.ErrCloseBlocked) {
		t.Fatalf("mixed blocked UpdateIfMatch = %v, want ErrCloseBlocked", err)
	}
	afterRefusal, err := store.Get(target.ID)
	if err != nil {
		t.Fatalf("Get after refused mixed close: %v", err)
	}
	if afterRefusal.Status != before.Status || afterRefusal.Title != before.Title ||
		afterRefusal.ParentID != before.ParentID || afterRefusal.Revision != before.Revision ||
		forceCloseContainsString(afterRefusal.Labels, "must-roll-back") {
		t.Fatalf("refused mixed close left partial mutation: before=%+v after=%+v", before, afterRefusal)
	}

	oldParent, err := store.Create(Bead{Title: "old parent"})
	if err != nil {
		t.Fatalf("Create old parent: %v", err)
	}
	success, err := store.Create(Bead{Title: "before", ParentID: oldParent.ID, Labels: []string{"keep", "remove"}})
	if err != nil {
		t.Fatalf("Create success target: %v", err)
	}
	success, err = store.Get(success.ID)
	if err != nil {
		t.Fatalf("Get success target: %v", err)
	}
	title = "after"
	err = store.UpdateIfMatch(success.ID, success.Revision, UpdateOpts{
		Status:       &closed,
		Title:        &title,
		ParentID:     &newParent.ID,
		Labels:       []string{"added"},
		RemoveLabels: []string{"remove"},
	})
	if err != nil {
		t.Fatalf("successful mixed UpdateIfMatch: %v", err)
	}
	after, err := store.Get(success.ID)
	if err != nil {
		t.Fatalf("Get successful mixed close: %v", err)
	}
	if after.Status != "closed" || after.Title != title || after.ParentID != newParent.ID ||
		!forceCloseContainsString(after.Labels, "added") || !forceCloseContainsString(after.Labels, "keep") ||
		forceCloseContainsString(after.Labels, "remove") || after.Revision == success.Revision {
		t.Fatalf("successful mixed close = %+v; before=%+v", after, success)
	}

	priorRevision := after.Revision
	err = store.UpdateIfMatch(after.ID, priorRevision, UpdateOpts{Status: &closed, Labels: []string{"after-close"}})
	if err != nil {
		t.Fatalf("already-closed label UpdateIfMatch: %v", err)
	}
	afterTouch, err := store.Get(after.ID)
	if err != nil {
		t.Fatalf("Get after already-closed label update: %v", err)
	}
	if afterTouch.Status != "closed" || !forceCloseContainsString(afterTouch.Labels, "after-close") || afterTouch.Revision == priorRevision {
		t.Fatalf("already-closed label update = %+v, want label and fresh revision from %d", afterTouch, priorRevision)
	}
}

func requireNativeDoltRelatedUpdatePrerequisite(t *testing.T, store *NativeDoltStore) {
	t.Helper()
	storage, release, err := store.acquireStorage()
	if err != nil {
		t.Fatalf("acquire storage for TouchIssue prerequisite probe: %v", err)
	}
	defer release()
	hasTouch := false
	err = storage.RunInTransaction(context.Background(), "gc: probe related update prerequisite", func(tx beadslib.Transaction) error {
		_, hasTouch = any(tx).(nativeDoltTransactionIssueToucher)
		return nil
	})
	if err != nil {
		t.Fatalf("probe TouchIssue prerequisite: %v", err)
	}
	if !hasTouch {
		t.Skip("current beads pin predates transaction TouchIssue; exercised by the pending-pin compatibility gate")
	}
}

func assertRealNativeConditionalCloseCoordinate(t *testing.T, store *NativeDoltStore, id string) {
	t.Helper()
	storage, release, err := store.acquireStorage()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	coordinate, err := readNativeConditionalCloseCoordinate(context.Background(), storage, id)
	if err != nil {
		t.Fatalf("read durable conditional-close coordinate: %v", err)
	}
	if !strings.HasPrefix(coordinate, nativeConditionalCloseCoordinatePrefix) ||
		len(coordinate) != len(nativeConditionalCloseCoordinatePrefix)+32 {
		t.Fatalf("durable conditional-close coordinate = %q, want namespaced 128-bit coordinate", coordinate)
	}
}

func TestNativeDoltStoreRelatedOnlyUpdateBumpsRealRowVersion(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "conditional-related-bump")
	parent, err := store.Create(Bead{Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	target, err = store.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(target.ID, UpdateOpts{Labels: []string{"label-only"}}); err != nil {
		t.Fatalf("label-only Update: %v", err)
	}
	afterLabel, err := store.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterLabel.Revision == target.Revision {
		t.Fatalf("label-only update left RowVersion %d unchanged", target.Revision)
	}
	parentID := parent.ID
	if err := store.Update(target.ID, UpdateOpts{ParentID: &parentID}); err != nil {
		t.Fatalf("parent-only Update: %v", err)
	}
	afterParent, err := store.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterParent.Revision == afterLabel.Revision {
		t.Fatalf("parent-only update left RowVersion %d unchanged", afterLabel.Revision)
	}

	// A nonempty scalar map is not proof the scalar changed: beads discards
	// same-value row patches. The related mutation must still trigger TouchIssue
	// and invalidate the preimage token.
	sameTitle := afterParent.Title
	if err := store.UpdateIfMatch(target.ID, afterParent.Revision, UpdateOpts{
		Title: &sameTitle, Labels: []string{"same-title-related"},
	}); err != nil {
		t.Fatalf("same-title related UpdateIfMatch: %v", err)
	}
	afterSameTitle, err := store.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterSameTitle.Revision == afterParent.Revision || !slices.Contains(afterSameTitle.Labels, "same-title-related") {
		t.Fatalf("same-title related after-image = %+v, preimage revision %d", afterSameTitle, afterParent.Revision)
	}
	if err := store.UpdateIfMatch(target.ID, afterParent.Revision, UpdateOpts{Labels: []string{"stale"}}); !IsPreconditionFailed(err) {
		t.Fatalf("reused preimage revision after related update = %v, want PreconditionFailedError", err)
	}
}

func TestNativeDoltStoreConditionalWriterAgainstRealWisp(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "conditional-writer-wisp")
	parent, err := store.Create(Bead{Title: "regular parent"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Create(Bead{Title: "wisp", Ephemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	target, err = store.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	parentID := parent.ID
	title := "wisp composite"
	if err := store.UpdateIfMatch(target.ID, target.Revision, UpdateOpts{
		Title: &title, ParentID: &parentID, Labels: []string{"wisp-atomic"},
	}); err != nil {
		t.Fatalf("wisp composite UpdateIfMatch: %v", err)
	}
	composite, err := store.Get(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if composite.Revision == target.Revision || composite.ParentID != parent.ID ||
		!slices.Contains(composite.Labels, "wisp-atomic") {
		t.Fatalf("wisp composite after-image = %+v", composite)
	}
	if err := store.CloseIfMatch(composite.ID, composite.Revision); err != nil {
		t.Fatalf("wisp CloseIfMatch: %v", err)
	}
	closed, err := store.Get(composite.ID)
	if err != nil || closed.Status != "closed" {
		t.Fatalf("closed wisp = %+v, err=%v", closed, err)
	}
	if err := store.DeleteIfMatch(closed.ID, closed.Revision); err != nil {
		t.Fatalf("wisp DeleteIfMatch: %v", err)
	}
}

func TestNativeDoltStoreConditionalCloseCoordinateAgainstRealServerDolt(t *testing.T) {
	ctx := context.Background()
	scopeRoot := t.TempDir()
	port := startTestDoltServer(t)
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"backend":"dolt","database":"beads","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":%d}`, port)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	storage, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Fatalf("open server-mode native storage: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Errorf("close server-mode native storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatal(err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "conditional-close-coordinate", "gc")
	created, err := store.Create(Bead{Title: "server conditional close"})
	if err != nil {
		t.Fatal(err)
	}
	created, err = store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CloseIfMatch(created.ID, created.Revision); err != nil {
		t.Fatal(err)
	}
	assertRealNativeConditionalCloseCoordinate(t, store, created.ID)
}

// TestNativeDoltStoreMetadataCASSequentialAgainstRealDolt exercises the
// sequential value-CAS contract — both pinned traps — against real storage.
func TestNativeDoltStoreMetadataCASSequentialAgainstRealDolt(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "cas-sequential")

	b, err := store.Create(Bead{Title: "real-dolt-cas"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := b.ID

	// Trap 1: expected "" claims an ABSENT key.
	if ok, err := store.CompareAndSetMetadataKey(id, "k", "", "one"); err != nil || !ok {
		t.Fatalf("claim absent key: (%v, %v), want (true, nil)", ok, err)
	}
	// ...and also a PRESENT-AND-EMPTY key.
	if err := store.SetMetadata(id, "k", ""); err != nil {
		t.Fatalf("SetMetadata clear: %v", err)
	}
	if ok, err := store.CompareAndSetMetadataKey(id, "k", "", "two"); err != nil || !ok {
		t.Fatalf("claim empty-valued key: (%v, %v), want (true, nil)", ok, err)
	}
	// ...but never a non-empty one.
	if ok, err := store.CompareAndSetMetadataKey(id, "k", "", "three"); err != nil || ok {
		t.Fatalf("claim non-empty key with empty expected: (%v, %v), want (false, nil)", ok, err)
	}

	// Trap 2: a genuine mismatch is (false, nil), never an error.
	ok, err := store.CompareAndSetMetadataKey(id, "k", "WRONG", "four")
	if err != nil {
		t.Fatalf("value-mismatch CAS returned error: %v (want nil)", err)
	}
	if ok {
		t.Fatal("value-mismatch CAS returned true (want false)")
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["k"] != "two" {
		t.Fatalf("value = %q, want %q", got.Metadata["k"], "two")
	}
}

// TestNativeDoltStoreMetadataCASContentionAgainstRealDolt is the load-bearing
// test for the lease lane: under concurrency exactly ONE racer may win a claim
// from a single starting value. This is the property the in-memory fixture
// cannot evaluate, and the property D3/D5 leases and target_scope member
// declaration actually depend on — a CAS that admits two winners hands the
// same lease to two holders.
func TestNativeDoltStoreMetadataCASContentionAgainstRealDolt(t *testing.T) {
	store := openRealNativeDoltStoreForCAS(t, "cas-contention")

	b, err := store.Create(Bead{Title: "real-dolt-cas-contention"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := b.ID
	if err := store.SetMetadata(id, "lease", ""); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
		errs    []error
	)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(racer int) {
			defer wg.Done()
			holder := "holder-" + strconv.Itoa(racer)
			<-start
			ok, err := store.CompareAndSetMetadataKey(id, "lease", "", holder)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if ok {
				winners = append(winners, holder)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("racer returned an error (a lost race must be (false, nil)): %v", err)
	}
	if len(winners) != 1 {
		t.Fatalf("winners = %d %v, want exactly 1 — no mutual exclusion, so this CAS cannot carry a lease",
			len(winners), winners)
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["lease"] != winners[0] {
		t.Fatalf("stored lease = %q, want the sole winner %q", got.Metadata["lease"], winners[0])
	}
}

// TestNativeDoltStoreMetadataCASContentionAcrossIndependentHandles is the
// multi-writer leg, and it is the one that actually decides whether this CAS
// can carry a lease.
//
// The single-handle contention test above cannot distinguish a fence enforced
// by the DATABASE from exclusion accidentally provided by shared in-process
// state (a connection pool, a handle-level lock). The gascity Dolt database is
// multi-writer by design — the bd CLI, other gascity processes and graph-apply
// all write it — so a guard that only holds within one store handle is not a
// fence at all, which is precisely why a store-maintained counter was rejected
// as a revision token.
//
// Racing two INDEPENDENTLY OPENED storage handles over the same database
// directory reproduces that condition inside one test binary: the handles
// share no Go-level state, so any exclusion observed here is enforced below
// them.
func TestNativeDoltStoreMetadataCASContentionAcrossIndependentHandles(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), ".beads")

	openHandle := func(actor string) *NativeDoltStore {
		t.Helper()
		storage, err := beadslib.OpenBestAvailable(ctx, dir)
		if err != nil {
			t.Skipf("upstream native beads storage unavailable: %v", err)
		}
		t.Cleanup(func() {
			if err := storage.Close(); err != nil {
				t.Logf("close upstream storage (%s): %v", actor, err)
			}
		})
		if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
			t.Fatalf("set issue prefix (%s): %v", actor, err)
		}
		return newNativeDoltStoreWithStorageAndPrefix(storage, actor, "gc")
	}

	writerA := openHandle("cas-writer-a")
	writerB := openHandle("cas-writer-b")

	b, err := writerA.Create(Bead{Title: "cross-handle-cas-contention"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := b.ID
	if err := writerA.SetMetadata(id, "lease", ""); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	// The second handle must observe the bead before racing for it, otherwise
	// a miss proves nothing about the fence.
	if got, err := writerB.Get(id); err != nil || got.ID != id {
		t.Fatalf("second handle cannot see bead %q: (%v, %v)", id, got.ID, err)
	}

	type result struct {
		holder string
		won    bool
		err    error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, racer := range []struct {
		store  *NativeDoltStore
		holder string
	}{{writerA, "holder-A"}, {writerB, "holder-B"}} {
		go func(s *NativeDoltStore, holder string) {
			<-start
			won, err := s.CompareAndSetMetadataKey(id, "lease", "", holder)
			results <- result{holder: holder, won: won, err: err}
		}(racer.store, racer.holder)
	}
	close(start)

	var winners []string
	for range 2 {
		r := <-results
		if r.err != nil {
			// A conflict surfaced as an error is NOT contract-conformant: the
			// contract says a lost race is (false, nil). Report it as the
			// contract violation it is rather than tolerating it.
			t.Errorf("racer %s returned an error (a lost race must be (false, nil)): %v", r.holder, r.err)
			continue
		}
		if r.won {
			winners = append(winners, r.holder)
		}
	}
	if t.Failed() {
		return
	}
	if len(winners) != 1 {
		t.Fatalf("winners across independent handles = %d %v, want exactly 1 — the fence does not hold "+
			"between writers, so this CAS cannot carry a lease in the multi-writer Dolt database",
			len(winners), winners)
	}

	// Both handles must agree on who holds the lease.
	for name, s := range map[string]*NativeDoltStore{"writerA": writerA, "writerB": writerB} {
		got, err := s.Get(id)
		if err != nil {
			t.Fatalf("%s Get: %v", name, err)
		}
		if got.Metadata["lease"] != winners[0] {
			t.Fatalf("%s sees lease %q, want the sole winner %q", name, got.Metadata["lease"], winners[0])
		}
	}
}

// TestNativeDoltStoreAtomicConditionalCloseAcrossIndependentHandles proves
// the atomic terminal-write fence against the actual OpenBestAvailable
// backend. The fast native fixture owns retry branch coverage; this test owns
// the database isolation and rollback boundary shared by independent handles.
func TestNativeDoltStoreAtomicConditionalCloseAcrossIndependentHandles(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), ".beads")
	openHandle := func(actor string) *NativeDoltStore {
		t.Helper()
		storage, err := beadslib.OpenBestAvailable(ctx, dir)
		if err != nil {
			t.Fatalf("open upstream native beads storage (%s): %v", actor, err)
		}
		t.Cleanup(func() {
			if err := storage.Close(); err != nil {
				t.Errorf("close upstream storage (%s): %v", actor, err)
			}
		})
		if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
			t.Fatalf("set issue prefix (%s): %v", actor, err)
		}
		return newNativeDoltStoreWithStorageAndPrefix(storage, actor, "gc")
	}

	writerA := openHandle("atomic-close-A")
	writerB := openHandle("atomic-close-B")
	created, err := writerA.Create(Bead{Title: "real atomic conditional close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	snapshot, err := writerA.Get(created.ID)
	if err != nil {
		t.Fatalf("writerA Get: %v", err)
	}
	if peer, err := writerB.Get(created.ID); err != nil || peer.Revision != snapshot.Revision {
		t.Fatalf("writerB snapshot = (%#v, %v), want revision %d", peer, err, snapshot.Revision)
	}

	type result struct {
		bead Bead
		err  error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, writer := range []*NativeDoltStore{writerA, writerB} {
		go func(store *NativeDoltStore) {
			<-start
			bead, err := store.CloseWithMetadataIfMatch(created.ID, snapshot.Revision, map[string]string{"winner": store.actor})
			results <- result{bead: bead, err: err}
		}(writer)
	}
	close(start)

	var winner Bead
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			if winner.ID != "" {
				t.Fatalf("multiple successful terminal writes: %#v and %#v", winner, result.bead)
			}
			winner = result.bead
		case IsPreconditionFailed(result.err):
			if result.bead.ID != "" {
				t.Fatalf("losing close returned %#v, want zero bead", result.bead)
			}
		default:
			t.Fatalf("contending close error = %v, want precondition failure", result.err)
		}
	}
	if winner.ID == "" || winner.Status != "closed" || winner.Metadata["winner"] == "" {
		t.Fatalf("winner = %#v, want exact closed row", winner)
	}

	staleCreated, err := writerA.Create(Bead{Title: "real atomic close stale rollback", Metadata: map[string]string{"before": "keep"}})
	if err != nil {
		t.Fatalf("Create stale bead: %v", err)
	}
	if err := writerB.SetMetadata(staleCreated.ID, "intervening", "write"); err != nil {
		t.Fatalf("intervening SetMetadata: %v", err)
	}
	before, err := writerA.Get(staleCreated.ID)
	if err != nil {
		t.Fatalf("Get before stale close: %v", err)
	}
	closed, err := writerA.CloseWithMetadataIfMatch(staleCreated.ID, staleCreated.Revision, map[string]string{"state": "drained"})
	if !IsPreconditionFailed(err) {
		t.Fatalf("stale CloseWithMetadataIfMatch error = %v, want precondition failure", err)
	}
	if closed.ID != "" {
		t.Fatalf("stale close returned %#v, want zero bead", closed)
	}
	after, err := writerB.Get(staleCreated.ID)
	if err != nil {
		t.Fatalf("Get after stale close: %v", err)
	}
	if after.Status != before.Status || after.Metadata["before"] != "keep" || after.Metadata["intervening"] != "write" || after.Metadata["state"] != "" {
		t.Fatalf("stale close mutated real row: before=%#v after=%#v", before, after)
	}
}

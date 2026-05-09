package sourceworkflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestWithLockHonorsContextWhileWaitingForLocalLock(t *testing.T) {
	cityPath := t.TempDir()
	locked := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)

	go func() {
		holderDone <- WithLock(context.Background(), cityPath, "city:test", "BL-42", func() error {
			close(locked)
			<-release
			return nil
		})
	}()

	<-locked
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := WithLock(ctx, cityPath, "city:test", "BL-42", func() error {
		t.Fatal("WithLock ran callback while lock was already held")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WithLock error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("WithLock waited %s after context deadline, want bounded wait", elapsed)
	}

	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("holder WithLock: %v", err)
	}
}

func TestWithLockReleasesLocalLockEntryAfterUnlock(t *testing.T) {
	cityPath := t.TempDir()
	_, key, err := lockIdentity(cityPath, "city:test", "BL-42")
	if err != nil {
		t.Fatalf("lockIdentity: %v", err)
	}

	if err := WithLock(context.Background(), cityPath, "city:test", "BL-42", func() error {
		localLocksMu.Lock()
		_, ok := localLocks[key]
		localLocksMu.Unlock()
		if !ok {
			t.Fatal("local lock entry missing while lock held")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithLock: %v", err)
	}

	localLocksMu.Lock()
	_, ok := localLocks[key]
	localLocksMu.Unlock()
	if ok {
		t.Fatal("local lock entry still present after unlock")
	}
}

func TestLockIdentityCanonicalizesScopeRefSymlinks(t *testing.T) {
	cityPath := t.TempDir()
	targetDir := filepath.Join(t.TempDir(), "rig")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(targetDir): %v", err)
	}
	linkDir := filepath.Join(t.TempDir(), "rig-link")
	if err := os.Symlink(targetDir, linkDir); err != nil {
		t.Fatalf("Symlink(linkDir): %v", err)
	}

	lockPathA, keyA, err := lockIdentity(cityPath, targetDir, "BL-42")
	if err != nil {
		t.Fatalf("lockIdentity(targetDir): %v", err)
	}
	lockPathB, keyB, err := lockIdentity(cityPath, linkDir, "BL-42")
	if err != nil {
		t.Fatalf("lockIdentity(linkDir): %v", err)
	}
	if lockPathA != lockPathB {
		t.Fatalf("lockPath mismatch = %q vs %q", lockPathA, lockPathB)
	}
	if keyA != keyB {
		t.Fatalf("key mismatch = %q vs %q", keyA, keyB)
	}
}

func TestLockScopeForStoreRefResolvesCityRigAndDefaultScopes(t *testing.T) {
	cityPath := filepath.Clean("/city")
	rigPath := filepath.Join("rigs", "alpha")
	resolveRig := func(name string) (string, bool) {
		if name != "alpha" {
			return "", false
		}
		return rigPath, true
	}

	tests := []struct {
		name             string
		defaultStorePath string
		storeRef         string
		want             string
	}{
		{name: "default store path", defaultStorePath: "/city/rigs/default", want: filepath.Clean("/city/rigs/default")},
		{name: "city store ref", storeRef: "city:test", want: cityPath},
		{name: "rig store ref", storeRef: "rig:alpha", want: filepath.Join(cityPath, rigPath)},
		{name: "unknown store ref", storeRef: "external:one", want: filepath.Clean("external:one")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LockScopeForStoreRef(cityPath, tt.defaultStorePath, tt.storeRef, resolveRig)
			if got != tt.want {
				t.Fatalf("LockScopeForStoreRef() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkflowMatchesSourceUsesSourceStoreRefWhenPresent(t *testing.T) {
	root := beads.Bead{
		ID: "wf-1",
		Metadata: map[string]string{
			"gc.source_bead_id":       "BL-42",
			SourceStoreRefMetadataKey: "rig:alpha",
		},
	}
	if !WorkflowMatchesSource(root, "BL-42", "rig:alpha", "rig:beta") {
		t.Fatal("WorkflowMatchesSource() = false, want true for matching store ref")
	}
	if WorkflowMatchesSource(root, "BL-42", "rig:beta", "rig:alpha") {
		t.Fatal("WorkflowMatchesSource() = true, want false for mismatched store ref")
	}
}

func TestWorkflowMatchesSourceTreatsMissingSourceStoreRefAsLegacyMatchInOwningStore(t *testing.T) {
	root := beads.Bead{
		ID: "wf-legacy",
		Metadata: map[string]string{
			"gc.source_bead_id": "BL-42",
		},
	}
	if !WorkflowMatchesSource(root, "BL-42", "rig:alpha", "rig:alpha") {
		t.Fatal("WorkflowMatchesSource() = false, want true for legacy root in owning store")
	}
	if WorkflowMatchesSource(root, "BL-42", "rig:alpha", "rig:beta") {
		t.Fatal("WorkflowMatchesSource() = true, want false for legacy root in different store")
	}
}

func TestListLiveRootsFiltersBySourceStoreRef(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		ID:     "wf-alpha",
		Title:  "alpha workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":                 "workflow",
			"gc.source_bead_id":       "BL-42",
			SourceStoreRefMetadataKey: "rig:alpha",
		},
	}); err != nil {
		t.Fatalf("Create(alpha): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		ID:     "wf-beta",
		Title:  "beta workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":                 "workflow",
			"gc.source_bead_id":       "BL-42",
			SourceStoreRefMetadataKey: "rig:beta",
		},
	}); err != nil {
		t.Fatalf("Create(beta): %v", err)
	}

	roots, err := ListLiveRoots(store, "BL-42", "rig:alpha", "rig:alpha")
	if err != nil {
		t.Fatalf("ListLiveRoots: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("ListLiveRoots(...) = %#v, want 1 root", roots)
	}
	if got := roots[0].Metadata[SourceStoreRefMetadataKey]; got != "rig:alpha" {
		t.Fatalf("root %s = %q, want rig:alpha", SourceStoreRefMetadataKey, got)
	}
}

func TestListLiveRootsIncludesGraphV2OnlyRoots(t *testing.T) {
	// Regression: sling.IsWorkflowAttachment treats a bead as a workflow
	// root if it carries gc.formula_contract=graph.v2 even without
	// gc.kind=workflow. If ListLiveRoots queries only on gc.kind=workflow,
	// such roots are invisible to the singleton scanner and --force can
	// launch a duplicate root alongside the live one.
	store := beads.NewMemStore()
	graphRoot, err := store.Create(beads.Bead{
		Title:  "graph.v2 root without gc.kind",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.formula_contract":     "graph.v2",
			"gc.source_bead_id":       "BL-42",
			SourceStoreRefMetadataKey: "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create(graph-only): %v", err)
	}

	roots, err := ListLiveRoots(store, "BL-42", "rig:alpha", "rig:alpha")
	if err != nil {
		t.Fatalf("ListLiveRoots: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("ListLiveRoots(...) = %d roots, want 1 (graph.v2-only root must not be hidden)", len(roots))
	}
	if roots[0].ID != graphRoot.ID {
		t.Fatalf("root ID = %q, want %q", roots[0].ID, graphRoot.ID)
	}
	if roots[0].Metadata["gc.formula_contract"] != "graph.v2" {
		t.Fatalf("root gc.formula_contract = %q, want graph.v2", roots[0].Metadata["gc.formula_contract"])
	}
}

func TestListLiveRootsExcludesNonWorkflowBeadsUnderSameSource(t *testing.T) {
	// Beads tagged with gc.source_bead_id but not marked as workflow roots
	// (neither gc.kind=workflow nor gc.formula_contract=graph.v2) must be
	// filtered out — the source_bead_id label alone is not enough to promote
	// a bead to a live root.
	store := beads.NewMemStore()
	realRoot, err := store.Create(beads.Bead{
		Title:  "real workflow root",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":                 "workflow",
			"gc.source_bead_id":       "BL-42",
			SourceStoreRefMetadataKey: "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create(real root): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "free-floating note about BL-42",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.source_bead_id":       "BL-42",
			SourceStoreRefMetadataKey: "rig:alpha",
		},
	}); err != nil {
		t.Fatalf("Create(note): %v", err)
	}

	roots, err := ListLiveRoots(store, "BL-42", "rig:alpha", "rig:alpha")
	if err != nil {
		t.Fatalf("ListLiveRoots: %v", err)
	}
	if len(roots) != 1 || roots[0].ID != realRoot.ID {
		t.Fatalf("ListLiveRoots(...) = %#v, want exactly the real root %q", roots, realRoot.ID)
	}
}

func TestListLiveRootsTreatsLegacyRootAsStoreScoped(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		ID:     "wf-legacy",
		Title:  "legacy workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": "BL-42",
		},
	}); err != nil {
		t.Fatalf("Create(legacy): %v", err)
	}

	alphaRoots, err := ListLiveRoots(store, "BL-42", "rig:alpha", "rig:alpha")
	if err != nil {
		t.Fatalf("ListLiveRoots(alpha): %v", err)
	}
	if len(alphaRoots) != 1 {
		t.Fatalf("ListLiveRoots(alpha) = %#v, want 1 root", alphaRoots)
	}

	betaRoots, err := ListLiveRoots(store, "BL-42", "rig:alpha", "rig:beta")
	if err != nil {
		t.Fatalf("ListLiveRoots(beta): %v", err)
	}
	if len(betaRoots) != 0 {
		t.Fatalf("ListLiveRoots(beta) = %#v, want 0 roots", betaRoots)
	}
}

type parentLastCloseStore struct {
	*beads.MemStore
}

func (s *parentLastCloseStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	positions := make(map[string]int, len(ids))
	for i, id := range ids {
		positions[id] = i
	}
	all, err := s.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		return 0, err
	}
	for _, bead := range all {
		if bead.ID == "" || bead.ParentID == "" {
			continue
		}
		parentPos, parentOK := positions[bead.ParentID]
		childPos, childOK := positions[bead.ID]
		if parentOK && childOK && parentPos < childPos {
			return 0, fmt.Errorf("parent %s closed before child %s", bead.ParentID, bead.ID)
		}
	}
	return s.MemStore.CloseAll(ids, metadata)
}

type blockValidatingWorkflowStore struct {
	*beads.MemStore
}

func (s *blockValidatingWorkflowStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		bead, err := s.Get(id)
		if err != nil {
			return closed, err
		}
		if bead.Status == "closed" {
			continue
		}
		if err := s.assertNoOpenBlockers(id); err != nil {
			return closed, err
		}
		n, err := s.MemStore.CloseAll([]string{id}, metadata)
		closed += n
		if err != nil {
			return closed, err
		}
	}
	return closed, nil
}

func (s *blockValidatingWorkflowStore) assertNoOpenBlockers(id string) error {
	deps, err := s.DepList(id, "down")
	if err != nil {
		return err
	}
	for _, d := range deps {
		if d.IssueID != id || d.Type != "blocks" {
			continue
		}
		blocker, err := s.Get(d.DependsOnID)
		if err != nil {
			continue
		}
		if blocker.Status != "closed" {
			return fmt.Errorf("cannot close %s: blocked by open %s", id, d.DependsOnID)
		}
	}
	return nil
}

func TestCloseWorkflowSubtreeClosesDeepestChildrenFirst(t *testing.T) {
	store := &parentLastCloseStore{MemStore: beads.NewMemStore()}

	root, err := store.Create(beads.Bead{Title: "root", Type: "task"})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "child",
		Type:     "task",
		ParentID: root.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	if err := store.DepAdd(child.ID, root.ID, "parent-child"); err != nil {
		t.Fatalf("DepAdd(child): %v", err)
	}
	grandchild, err := store.Create(beads.Bead{
		Title:    "grandchild",
		Type:     "task",
		ParentID: child.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	if err != nil {
		t.Fatalf("Create(grandchild): %v", err)
	}
	if err := store.DepAdd(grandchild.ID, child.ID, "parent-child"); err != nil {
		t.Fatalf("DepAdd(grandchild): %v", err)
	}

	closed, err := CloseWorkflowSubtree(store, root.ID)
	if err != nil {
		t.Fatalf("CloseWorkflowSubtree: %v", err)
	}
	if closed != 3 {
		t.Fatalf("CloseWorkflowSubtree closed %d beads, want 3", closed)
	}
	for _, id := range []string{root.ID, child.ID, grandchild.ID} {
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if bead.Status != "closed" {
			t.Fatalf("bead %s status = %q, want closed", id, bead.Status)
		}
	}
}

func TestCloseWorkflowSubtreeOrdersBlockersBeforeBlocked(t *testing.T) {
	store := &blockValidatingWorkflowStore{MemStore: beads.NewMemStore()}

	root, err := store.Create(beads.Bead{
		Title: "root",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	stepNames := []string{
		"submit-and-exit",
		"self-review",
		"implement",
		"preflight-tests",
		"workspace-setup",
		"load-context",
	}
	steps := make([]beads.Bead, 0, len(stepNames))
	for _, name := range stepNames {
		step, err := store.Create(beads.Bead{
			Title:    name,
			Type:     "task",
			ParentID: root.ID,
			Metadata: map[string]string{
				"gc.root_bead_id": root.ID,
			},
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		steps = append(steps, step)
	}
	for i := 0; i < len(steps)-1; i++ {
		blocked := steps[i]
		blocker := steps[i+1]
		if err := store.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("DepAdd(%s blocks-on %s): %v", blocked.ID, blocker.ID, err)
		}
	}

	closed, err := CloseWorkflowSubtree(store, root.ID)
	if err != nil {
		t.Fatalf("CloseWorkflowSubtree: %v", err)
	}
	wantClosed := 1 + len(steps)
	if closed != wantClosed {
		t.Fatalf("CloseWorkflowSubtree closed %d beads, want %d", closed, wantClosed)
	}
	for _, id := range append([]string{root.ID}, workflowIDsOf(steps)...) {
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if bead.Status != "closed" {
			t.Fatalf("bead %s status = %q, want closed", id, bead.Status)
		}
		if got := bead.Metadata["gc.outcome"]; got != "skipped" {
			t.Fatalf("bead %s gc.outcome = %q, want skipped", id, got)
		}
	}
}

func workflowIDsOf(bs []beads.Bead) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.ID
	}
	return out
}

func TestCloseWorkflowSubtreeHandlesParentCycles(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		ID:     "wf-root",
		Title:  "root",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:    "child",
		Type:     "task",
		ParentID: root.ID,
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	if err := store.Update(root.ID, beads.UpdateOpts{ParentID: &child.ID}); err != nil {
		t.Fatalf("Update(root.ParentID): %v", err)
	}
	if err := store.DepAdd(child.ID, root.ID, "parent-child"); err != nil {
		t.Fatalf("DepAdd(child): %v", err)
	}

	closed, err := CloseWorkflowSubtree(store, root.ID)
	if err != nil {
		t.Fatalf("CloseWorkflowSubtree: %v", err)
	}
	if closed != 2 {
		t.Fatalf("CloseWorkflowSubtree closed %d beads, want 2", closed)
	}
	for _, id := range []string{root.ID, child.ID} {
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if bead.Status != "closed" {
			t.Fatalf("bead %s status = %q, want closed", id, bead.Status)
		}
	}
}

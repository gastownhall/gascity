package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
)

// attachGuardCity returns a one-shot cook city plus the work store holding the
// attach targets the tests below create.
func attachGuardCity(t *testing.T) (string, beads.Store) {
	t.Helper()
	cityDir := oneShotCookCity(t)
	seedCLIStorageRoutes(t, cityDir, nil)
	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	return cityDir, work
}

func mustCreateBead(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("create %q: %v", b.Title, err)
	}
	return created
}

// attachGuardCook runs `gc formula cook legacy-work --attach target` through
// the real cobra command and returns its outcome without failing the test, so
// a caller can assert a refusal as well as a success. Safe to call from a
// goroutine.
func attachGuardCook(target string) (res formulaCookJSONResult, stderr string, err error) {
	var out, errBuf bytes.Buffer
	cmd := newFormulaCookCmd(&out, &errBuf)
	cmd.SetArgs([]string{"legacy-work", "--json", "--attach", target})
	err = cmd.Execute()
	if err == nil {
		if jerr := json.Unmarshal(out.Bytes(), &res); jerr != nil {
			err = jerr
		}
	}
	return res, errBuf.String(), err
}

// attachBlockers lists the beads target is wired to block on.
func attachBlockers(t *testing.T, store beads.Store, target string) []string {
	t.Helper()
	deps, err := store.DepList(target, "down")
	if err != nil {
		t.Fatalf("dep list %s: %v", target, err)
	}
	var ids []string
	for _, d := range deps {
		if d.Type == "blocks" {
			ids = append(ids, d.DependsOnID)
		}
	}
	return ids
}

// TestFormulaCookAttachLiveWorkflowIDRootsAtTheWorkflow pins the v1 --attach
// arm against a target whose workflow_id names a LIVE workflow root. That
// stamp points upward, at the container the target belongs to; it is not a
// downward attachment, so the guard must neither refuse the cook with a
// ConflictError nor stop the new sub-DAG rooting at the workflow's real root.
func TestFormulaCookAttachLiveWorkflowIDRootsAtTheWorkflow(t *testing.T) {
	_, work := attachGuardCity(t)
	workflowRoot := mustCreateBead(t, work, beads.Bead{
		Title:    "live workflow root",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	target := mustCreateBead(t, work, beads.Bead{
		Title:    "attach target inside a live workflow",
		Type:     "task",
		Metadata: map[string]string{"workflow_id": workflowRoot.ID},
	})

	res, stderr, err := attachGuardCook(target.ID)
	if err != nil {
		t.Fatalf("cook --attach onto a target with a live workflow_id: %v\nstderr=%s", err, stderr)
	}
	if res.WorkflowRootID != workflowRoot.ID {
		t.Fatalf("workflow_root_id = %q, want the live workflow's real root %q (not the target %q)", res.WorkflowRootID, workflowRoot.ID, target.ID)
	}
	root, err := work.Get(res.RootID)
	if err != nil {
		t.Fatalf("attached sub-DAG root %s: %v", res.RootID, err)
	}
	if got := root.Metadata[beadmeta.RootBeadIDMetadataKey]; got != workflowRoot.ID {
		t.Errorf("sub-DAG root %s gc.root_bead_id = %q, want %q", res.RootID, got, workflowRoot.ID)
	}
}

// TestFormulaCookAttachStaleMoleculeIDSelfRoots is the regression for the
// ga-yov1rr shape on the CLI wiring: a target whose only run-chain stamp is a
// stale DOWNWARD molecule_id (its molecule long closed) roots the new sub-DAG
// at the target's own id. molecule_id is deliberately not an upward pointer.
func TestFormulaCookAttachStaleMoleculeIDSelfRoots(t *testing.T) {
	_, work := attachGuardCity(t)
	prior := mustCreateBead(t, work, beads.Bead{Title: "prior molecule", Type: "molecule"})
	if err := work.Close(prior.ID); err != nil {
		t.Fatalf("close prior molecule: %v", err)
	}
	target := mustCreateBead(t, work, beads.Bead{
		Title:    "re-attached content bead",
		Type:     "task",
		Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: prior.ID},
	})

	res, stderr, err := attachGuardCook(target.ID)
	if err != nil {
		t.Fatalf("cook --attach onto a target with a stale molecule_id: %v\nstderr=%s", err, stderr)
	}
	if res.WorkflowRootID != target.ID {
		t.Fatalf("workflow_root_id = %q, want the target's own id %q", res.WorkflowRootID, target.ID)
	}
}

// TestFormulaCookAttachRefusesLiveAttachedMolecule is the duplicate-attach
// guard: a second cook --attach onto a target that already carries a live
// molecule must not create a second one. An assigned target is refused with
// the sling path's MoleculeAttachedError wording and nothing is created; an
// unassigned target whose molecule has no active work is auto-burned, so the
// old molecule is closed and its molecule_id cleared before the new sub-DAG
// attaches.
func TestFormulaCookAttachRefusesLiveAttachedMolecule(t *testing.T) {
	t.Run("assigned target is refused", func(t *testing.T) {
		_, work := attachGuardCity(t)
		wrapper := mustCreateBead(t, work, beads.Bead{Title: "existing molecule", Type: "molecule"})
		target := mustCreateBead(t, work, beads.Bead{
			Title:    "review target",
			Type:     "task",
			Assignee: "reviewer",
			Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: wrapper.ID},
		})

		_, stderr, err := attachGuardCook(target.ID)
		if err == nil {
			t.Fatalf("cook --attach = nil error, want a refusal: %s already has attached molecule %s\nstderr=%s", target.ID, wrapper.ID, stderr)
		}
		if got := attachBlockers(t, work, target.ID); len(got) != 0 {
			t.Errorf("refused cook wired %s to block on %v, want nothing created", target.ID, got)
		}
		rooted, lerr := work.List(beads.ListQuery{
			Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: target.ID},
			IncludeClosed: true,
			TierMode:      beads.TierBoth,
			AllowScan:     true,
		})
		if lerr != nil {
			t.Fatalf("list beads rooted at target: %v", lerr)
		}
		if len(rooted) != 0 {
			t.Errorf("refused cook created beads rooted at the target: %+v", rooted)
		}
	})

	t.Run("unassigned target auto-burns the idle molecule", func(t *testing.T) {
		_, work := attachGuardCity(t)
		wrapper := mustCreateBead(t, work, beads.Bead{Title: "idle molecule", Type: "molecule"})
		target := mustCreateBead(t, work, beads.Bead{
			Title:    "unassigned target",
			Type:     "task",
			Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: wrapper.ID},
		})

		res, stderr, err := attachGuardCook(target.ID)
		if err != nil {
			t.Fatalf("cook --attach onto an unassigned target with an idle molecule: %v\nstderr=%s", err, stderr)
		}
		burned, err := work.Get(wrapper.ID)
		if err != nil {
			t.Fatalf("get old molecule: %v", err)
		}
		if burned.Status != "closed" {
			t.Errorf("old molecule %s status = %q, want closed (auto-burned) so the target never carries two live molecules", wrapper.ID, burned.Status)
		}
		got, err := work.Get(target.ID)
		if err != nil {
			t.Fatalf("get target: %v", err)
		}
		if id := got.Metadata[beadmeta.MoleculeIDMetadataKey]; id != "" {
			t.Errorf("target molecule_id = %q, want it cleared by the auto-burn", id)
		}
		if blockers := attachBlockers(t, work, target.ID); len(blockers) != 1 || blockers[0] != res.RootID {
			t.Errorf("target blocks on %v, want exactly the new sub-DAG root %s", blockers, res.RootID)
		}
	})
}

// sourceWorkflowLockProbe reports whether the one source-workflow lock under
// cityPath is held right now. Unlike sourceWorkflowLockHeld it never fails
// the test, so a store hook can call it from a cook running in a goroutine.
func sourceWorkflowLockProbe(cityPath string) (held bool, err error) {
	dir := filepath.Join(citylayout.RuntimeDataDir(cityPath), "sling-source-locks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	if len(entries) != 1 {
		return false, errors.New("want exactly one source-workflow lock file")
	}
	f, err := os.OpenFile(filepath.Join(dir, entries[0].Name()), os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer f.Close() //nolint:errcheck // probe cleanup
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// attachGuardSpyStore records, for every store the cook opens, whether the
// source-workflow lock was held when the attach wired its blocking dep, and
// how many times each bead was closed.
type attachGuardSpyStore struct {
	beads.Store
	cityDir string
	spy     *attachGuardSpy
}

type attachGuardSpy struct {
	mu          sync.Mutex
	depAdds     int
	unlocked    int
	probeErrors []error
	closes      map[string]int
}

func (s *attachGuardSpyStore) DepAdd(issueID, dependsOnID, depType string) error {
	held, err := sourceWorkflowLockProbe(s.cityDir)
	s.spy.mu.Lock()
	s.spy.depAdds++
	switch {
	case err != nil:
		s.spy.probeErrors = append(s.spy.probeErrors, err)
	case !held:
		s.spy.unlocked++
	}
	s.spy.mu.Unlock()
	return s.Store.DepAdd(issueID, dependsOnID, depType)
}

func (s *attachGuardSpyStore) Close(id string) error {
	s.spy.mu.Lock()
	s.spy.closes[id]++
	s.spy.mu.Unlock()
	return s.Store.Close(id)
}

func (s *attachGuardSpyStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.spy.mu.Lock()
	for _, id := range ids {
		s.spy.closes[id]++
	}
	s.spy.mu.Unlock()
	return s.Store.CloseAll(ids, metadata)
}

func spyOnAttachGuardStores(t *testing.T, cityDir string) *attachGuardSpy {
	t.Helper()
	spy := &attachGuardSpy{closes: map[string]int{}}
	prev := observeOpenedStore
	observeOpenedStore = func(_ string, store beads.Store) beads.Store {
		return &attachGuardSpyStore{Store: store, cityDir: cityDir, spy: spy}
	}
	t.Cleanup(func() { observeOpenedStore = prev })
	return spy
}

// TestFormulaCookAttachV1RunsUnderTheSourceWorkflowLock pins that the v1
// --attach arm takes the source-workflow lock, as the graph.v2 arm does: the
// blocking dep that wires the target to its new sub-DAG is written while the
// lock is held.
func TestFormulaCookAttachV1RunsUnderTheSourceWorkflowLock(t *testing.T) {
	cityDir, work := attachGuardCity(t)
	target := mustCreateBead(t, work, beads.Bead{Title: "attach target", Type: "task"})
	spy := spyOnAttachGuardStores(t, cityDir)

	if _, stderr, err := attachGuardCook(target.ID); err != nil {
		t.Fatalf("cook --attach: %v\nstderr=%s", err, stderr)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.depAdds == 0 {
		t.Fatalf("the attach wired no blocking dep; the lock probe never ran")
	}
	if len(spy.probeErrors) > 0 {
		t.Fatalf("lock probe failed: %v (a cook that never takes the lock leaves no lock file)", spy.probeErrors)
	}
	if spy.unlocked != 0 {
		t.Errorf("%d of %d blocking-dep writes ran without the source-workflow lock held", spy.unlocked, spy.depAdds)
	}
}

// TestFormulaCookAttachConcurrentCooksOnlyOneBurns pins the lock's purpose:
// two cooks racing onto one unassigned target that carries an idle molecule
// serialize, so the first burns the molecule and the second finds it already
// cleared. Without the lock both read the stamp and both burn it.
func TestFormulaCookAttachConcurrentCooksOnlyOneBurns(t *testing.T) {
	cityDir, work := attachGuardCity(t)
	wrapper := mustCreateBead(t, work, beads.Bead{Title: "idle molecule", Type: "molecule"})
	target := mustCreateBead(t, work, beads.Bead{
		Title:    "contended target",
		Type:     "task",
		Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: wrapper.ID},
	})
	spy := spyOnAttachGuardStores(t, cityDir)

	const cooks = 2
	var wg sync.WaitGroup
	errs := make([]error, cooks)
	stderrs := make([]string, cooks)
	for i := 0; i < cooks; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, stderrs[i], errs[i] = attachGuardCook(target.ID)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent cook %d: %v\nstderr=%s", i, err, stderrs[i])
		}
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if got := spy.closes[wrapper.ID]; got != 1 {
		t.Errorf("the idle molecule %s was closed %d times, want exactly 1 (one cook burns it, the other finds it gone)", wrapper.ID, got)
	}
	if spy.unlocked != 0 || len(spy.probeErrors) > 0 {
		t.Errorf("blocking-dep writes without the lock: %d, probe errors: %v", spy.unlocked, spy.probeErrors)
	}
}
